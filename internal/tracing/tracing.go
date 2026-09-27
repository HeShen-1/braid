// Package tracing provides braid's process-local OpenTelemetry tracer
// provider. It is explicitly injected (never registered via the otel global),
// mirroring internal/metrics conventions: per-process construction, Deps
// injection, no hidden global state.
//
// The "off" exporter is a discard exporter, not a noop provider: span and
// trace ids are still generated so audit_log.trace_id never degrades with the
// observability switch (the audit chain is a moat, not a dashboard). Exporter
// selection is the only knob for P0 (D-2026-09-27-t62-otel-trace); Jaeger and
// W3C traceparent injection on outbound HTTP are P1.
package tracing

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// Exporter selects the span sink.
type Exporter string

const (
	ExporterOff    Exporter = "off"    // discard exporter; ids still generated
	ExporterStdout Exporter = "stdout" // one JSON object per span, batched
)

// Config controls provider construction. ServiceName and Writer are set by
// the caller (not env): the service name is fixed per entrypoint, the writer
// exists for tests.
type Config struct {
	Exporter    Exporter
	ServiceName string
	// Writer sinks stdout exports; nil means os.Stdout. Ignored by off.
	Writer io.Writer
}

// ConfigFromEnv reads BRAID_TRACE_EXPORTER (unset/empty = off). Unknown
// values pass through and are rejected by Validate at startup.
func ConfigFromEnv(getenv func(string) string) Config {
	v := strings.TrimSpace(getenv("BRAID_TRACE_EXPORTER"))
	if v == "" {
		return Config{Exporter: ExporterOff}
	}
	return Config{Exporter: Exporter(v)}
}

// Validate rejects unknown exporter values (fail-fast at startup).
func (c Config) Validate() error {
	switch c.Exporter {
	case ExporterOff, ExporterStdout, "":
		return nil
	default:
		return fmt.Errorf("trace exporter %q invalid: want off|stdout", c.Exporter)
	}
}

// Provider owns the process TracerProvider. Construct one per entrypoint,
// inject Tracer via component Deps, Shutdown on exit (flushes the batcher).
type Provider struct {
	tp *sdktrace.TracerProvider
}

// NewProvider builds the SDK provider with the configured exporter. Sampling
// stays at the SDK default (parent-based always-on): P0 has no sampling knob.
func NewProvider(cfg Config) (*Provider, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	var exp sdktrace.SpanExporter = discardExporter{}
	if cfg.Exporter == ExporterStdout {
		w := cfg.Writer
		if w == nil {
			w = os.Stdout
		}
		e, err := stdouttrace.New(stdouttrace.WithWriter(w))
		if err != nil {
			return nil, fmt.Errorf("tracing: stdout exporter: %w", err)
		}
		exp = e
	}
	res, err := resource.Merge(resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName(cfg.ServiceName)))
	if err != nil {
		return nil, fmt.Errorf("tracing: resource: %w", err)
	}
	return &Provider{tp: sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)}, nil
}

// Tracer returns a named tracer; safe to call on any goroutine.
func (p *Provider) Tracer(name string) trace.Tracer { return p.tp.Tracer(name) }

// Shutdown flushes pending spans and releases exporter resources; idempotent.
func (p *Provider) Shutdown(ctx context.Context) error { return p.tp.Shutdown(ctx) }

// discardExporter accepts every span and writes nothing (ExporterOff).
type discardExporter struct{}

func (discardExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (discardExporter) Shutdown(context.Context) error                             { return nil }
