package tracing

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// Off 模式契约:span/trace id 照常生成,导出面零字节——audit trace_id 不随观测开关退化。
func TestOffProviderKeepsTraceIDAndExportsNothing(t *testing.T) {
	var buf bytes.Buffer
	p, err := NewProvider(Config{Exporter: ExporterOff, ServiceName: "test", Writer: &buf})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	ctx, span := p.Tracer("braid/test").Start(context.Background(), "op")
	defer span.End()

	sc := span.SpanContext()
	if !sc.IsValid() {
		t.Fatal("span context must stay valid under off exporter")
	}
	if got := trace.SpanContextFromContext(ctx); got.TraceID() != sc.TraceID() {
		t.Fatal("ctx must carry the started span's trace id")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("off exporter wrote %d bytes, want 0", buf.Len())
	}
}

// stdout 模式契约:导出的 JSON 携带生成侧的 trace id(导出的就是生成的)。
func TestStdoutProviderExportsStartedSpan(t *testing.T) {
	var buf bytes.Buffer
	p, err := NewProvider(Config{Exporter: ExporterStdout, ServiceName: "test", Writer: &buf})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	_, span := p.Tracer("braid/test").Start(context.Background(), "op")
	want := span.SpanContext().TraceID().String()
	span.End()

	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown must flush the batcher: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `"op"`) {
		t.Fatalf("exported output missing span name:\n%s", out)
	}
	if !strings.Contains(out, want) {
		t.Fatalf("exported output missing trace id %s:\n%s", want, out)
	}
}

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"off ok", Config{Exporter: ExporterOff}, false},
		{"stdout ok", Config{Exporter: ExporterStdout}, false},
		{"empty ok (zero value = off)", Config{}, false},
		{"unknown rejected", Config{Exporter: "jaeger"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestConfigFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want Exporter
	}{
		{"unset defaults to off", nil, ExporterOff},
		{"empty defaults to off", map[string]string{"BRAID_TRACE_EXPORTER": ""}, ExporterOff},
		{"spaces trimmed", map[string]string{"BRAID_TRACE_EXPORTER": " stdout "}, ExporterStdout},
		{"stdout", map[string]string{"BRAID_TRACE_EXPORTER": "stdout"}, ExporterStdout},
		{"unknown passes through for Validate to reject", map[string]string{"BRAID_TRACE_EXPORTER": "otlp"}, Exporter("otlp")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ConfigFromEnv(func(k string) string { return tc.env[k] })
			if got.Exporter != tc.want {
				t.Fatalf("Exporter = %q, want %q", got.Exporter, tc.want)
			}
			if _, err := NewProvider(Config{Exporter: got.Exporter}); err == nil && tc.want == Exporter("otlp") {
				t.Fatal("NewProvider must reject unknown exporter")
			}
		})
	}
}

// 默认 writer = os.Stdout 的构造路径必须可用(stdout 生产形态)。
func TestStdoutDefaultsToOSStdout(t *testing.T) {
	p, err := NewProvider(Config{Exporter: ExporterStdout, ServiceName: "test"})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

var _ = os.Stdout // keep os import aligned with production default path
