// Package tracing provides braid's process-local OpenTelemetry tracer
// provider. It is explicitly injected (never registered via the otel global),
// mirroring internal/metrics conventions: per-process construction, Deps
// injection, no hidden global state.
//
// The "off" exporter is a discard exporter, not a noop provider: span and
// trace ids are still generated so audit_log.trace_id never degrades with the
// observability switch (the audit chain is a moat, not a dashboard). Exporter
// selection is P0 (D-2026-09-27-t62-otel-trace); the jaeger exporter (OTLP/
// HTTP) is P1 (D-2026-09-29-t121-otel-p1).
package tracing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"unicode/utf8"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
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
	// ExporterJaeger ships spans over OTLP/HTTP to a Jaeger OTLP port
	// (D-2026-09-29-t121-otel-p1 #1: the native jaeger exporter is retired
	// upstream; Jaeger >=1.35 natively ingests OTLP). The implementation is
	// generic OTLP/HTTP — any OTLP collector works.
	ExporterJaeger Exporter = "jaeger"
)

// defaultJaegerEndpoint is the OTLP/HTTP target when BRAID_TRACE_OTLP_ENDPOINT
// is unset: Jaeger all-in-one's default OTLP HTTP port, loopback.
const defaultJaegerEndpoint = "http://localhost:4318"

// Config controls provider construction. ServiceName and Writer are set by
// the caller (not env): the service name is fixed per entrypoint, the writer
// exists for tests.
type Config struct {
	Exporter    Exporter
	ServiceName string
	// Writer sinks stdout exports; nil means os.Stdout. Ignored by off.
	Writer io.Writer
	// Endpoint is the OTLP/HTTP target for the jaeger exporter
	// (BRAID_TRACE_OTLP_ENDPOINT; empty = defaultJaegerEndpoint; scheme
	// http → insecure, https → TLS). Ignored by off/stdout.
	Endpoint string
}

// ConfigFromEnv reads BRAID_TRACE_EXPORTER (unset/empty = off) and
// BRAID_TRACE_OTLP_ENDPOINT. Unknown exporter values pass through and are
// rejected by Validate at startup.
func ConfigFromEnv(getenv func(string) string) Config {
	v := strings.TrimSpace(getenv("BRAID_TRACE_EXPORTER"))
	cfg := Config{Exporter: ExporterOff}
	if v != "" {
		cfg.Exporter = Exporter(v)
	}
	if ep := strings.TrimSpace(getenv("BRAID_TRACE_OTLP_ENDPOINT")); ep != "" {
		cfg.Endpoint = ep
	}
	return cfg
}

// Validate rejects unknown exporter values (fail-fast at startup).
func (c Config) Validate() error {
	switch c.Exporter {
	case ExporterOff, ExporterStdout, "":
		return nil
	case ExporterJaeger:
		return validateJaegerEndpoint(c.Endpoint)
	default:
		return fmt.Errorf("trace exporter %q invalid: want off|stdout|jaeger", c.Exporter)
	}
}

// validateJaegerEndpoint checks the OTLP/HTTP target shape. Empty is valid —
// NewProvider fills the default (env-omitted = loopback Jaeger).
func validateJaegerEndpoint(ep string) error {
	if ep == "" {
		return nil
	}
	u, err := url.Parse(ep)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("trace jaeger endpoint %q invalid: want http(s)://host[:port][/path]", ep)
	}
	return nil
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
	switch cfg.Exporter {
	case ExporterStdout:
		w := cfg.Writer
		if w == nil {
			w = os.Stdout
		}
		e, err := stdouttrace.New(stdouttrace.WithWriter(w))
		if err != nil {
			return nil, fmt.Errorf("tracing: stdout exporter: %w", err)
		}
		exp = e
	case ExporterJaeger:
		ep := cfg.Endpoint
		if ep == "" {
			ep = defaultJaegerEndpoint
		}
		u, err := url.Parse(ep)
		if err != nil {
			return nil, fmt.Errorf("tracing: jaeger endpoint: %w", err)
		}
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(u.Host)}
		if u.Scheme == "http" {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		if u.Path != "" && u.Path != "/" {
			opts = append(opts, otlptracehttp.WithURLPath(u.Path))
		}
		e, err := otlptracehttp.New(context.Background(), opts...)
		if err != nil {
			return nil, fmt.Errorf("tracing: jaeger exporter: %w", err)
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

// PayloadTrace 是任务协议 payload 的自描述链上下文字段(D-t121 #3:四类
// 事件 payload 以 trace_id 键携链、span_id 键可选;worker 段根与 relay
// 投递 span 的共同消费契约——单源防两方漂移)。
type PayloadTrace struct {
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`
}

// ExtractPayloadTrace 只抽字段不做形态判定(空 TraceID = 无链上下文发射,
// 键缺席形态)。载荷非对象/坏 JSON 时 ok=false,字段零值。
func ExtractPayloadTrace(raw []byte) (p PayloadTrace, ok bool) {
	if err := json.Unmarshal(raw, &p); err != nil {
		return PayloadTrace{}, false
	}
	return p, true
}

// PayloadSpanContext 把 payload 字段构造为 remote parent span context:
// ok = trace_id 合法 32hex(SpanID = span_id 真值,或 trace id 前 8 字节
// 确定性派生——D-t62 #2 legacy 回退:SDK remote parent 需非零 SpanID);
// ok=false = 无有效链上下文,调用方零注入短路。badSpanID = span_id 在场
// 但形态坏(已派生回退,调用方 warn 留痕;缺席是合法 legacy 形态不标记)。
func PayloadSpanContext(traceID, spanID string) (sc trace.SpanContext, ok, badSpanID bool) {
	if traceID == "" {
		return trace.SpanContext{}, false, false
	}
	tid, err := trace.TraceIDFromHex(traceID)
	if err != nil || !tid.IsValid() {
		// 全零 trace id hex 解码成功但 IsValid=false(SDK 对无效 remote
		// parent 按无 parent 处理,会新开根 span = 孤儿链形态)——与
		// malformed 同臂短路(review 修:契约「合法 32hex 必续链」闭合)。
		return trace.SpanContext{}, false, false
	}
	sid, bad := payloadSpanID(tid, spanID)
	sc = trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled, Remote: true,
	})
	if !sc.IsValid() {
		// 零前缀 trace id 的派生 span id 恰为全零、或 span_id 字面全零
		// (SpanIDFromHex 接受)——组装出无效 remote parent,SDK 会按无
		// parent 新开根 = 孤儿链形态。与 malformed 同臂短路(review 二轮
		// 修:每个被接受的 id 必真续链)。
		return trace.SpanContext{}, false, false
	}
	return sc, true, bad
}

func payloadSpanID(tid trace.TraceID, raw string) (trace.SpanID, bool) {
	if raw != "" {
		if sid, err := trace.SpanIDFromHex(raw); err == nil {
			return sid, false
		}
		return derivedSpanID(tid), true
	}
	return derivedSpanID(tid), false
}

func derivedSpanID(tid trace.TraceID) trace.SpanID {
	var sid [8]byte
	copy(sid[:], tid[:8])
	return trace.SpanID(sid)
}

// TruncateID 截断坏形态 id 值(64 字节界,日志体积面):payload id 是队列
// 写者可控值,warn 留痕前统一封顶——worker/relay 两消费方共用(worker
// tracemw / relay deliverRow,与 PayloadSpanContext 同文件单源)。回退到
// rune 边界再切,不产出非法 UTF-8 字节段(slog 转义在,此为洁净面)。
func TruncateID(v string) string {
	if len(v) <= 64 {
		return v
	}
	cut := 64
	for cut > 0 && !utf8.RuneStart(v[cut]) {
		cut--
	}
	return v[:cut]
}

// discardExporter accepts every span and writes nothing (ExporterOff).
type discardExporter struct{}

func (discardExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (discardExporter) Shutdown(context.Context) error                             { return nil }
