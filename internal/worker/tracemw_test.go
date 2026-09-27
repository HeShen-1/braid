package worker

// T6.2 消费半边单元面:payload trace_id → ctx 续链(trace id 原样继承、
// 段根 span 有效)、无 trace/坏形态零注入降级、tracer nil 直通。
// 落库与跨进程续链的端到端断言在 nodes e2e 真链层(片 5)。

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel/trace"

	"braid/internal/tracing"
)

func TestTraceMiddlewareExtractsPayloadTrace(t *testing.T) {
	prov, err := tracing.NewProvider(tracing.Config{Exporter: tracing.ExporterOff, ServiceName: "test"})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	t.Cleanup(func() { _ = prov.Shutdown(context.Background()) })
	tracer := prov.Tracer("braid/worker")

	const want = "4bf92f3577b34da6a3ce929d0e0e4736"
	payload, _ := json.Marshal(map[string]any{
		"execution_id": uint64(1), "trace_id": want,
	})

	var got trace.SpanContext
	called := false
	handler := asynq.HandlerFunc(func(ctx context.Context, tk *asynq.Task) error {
		called = true
		got = trace.SpanContextFromContext(ctx)
		return nil
	})
	mw := traceMiddleware(tracer, slog.Default())
	task := asynq.NewTask("node.task_enqueue", payload)
	if err := mw(handler).ProcessTask(context.Background(), task); err != nil {
		t.Fatalf("process: %v", err)
	}
	if !called {
		t.Fatal("handler not invoked")
	}
	if !got.HasTraceID() || got.TraceID().String() != want {
		t.Fatalf("handler ctx trace = %v, want %s (payload trace must continue, not fork)", got, want)
	}
}

func TestTraceMiddlewareDegradation(t *testing.T) {
	prov, err := tracing.NewProvider(tracing.Config{Exporter: tracing.ExporterOff, ServiceName: "test"})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	t.Cleanup(func() { _ = prov.Shutdown(context.Background()) })
	tracer := prov.Tracer("braid/worker")

	cases := []struct {
		name    string
		payload string
		// noInheritPrefix:防「宽容修复后继承」回归(截断/补零)——新链
		// trace 的前缀不得等于坏值(review LOW)。
		noInheritPrefix string
	}{
		{"no trace key", `{"execution_id":1}`, ""},
		{"empty trace", `{"execution_id":1,"trace_id":""}`, ""},
		{"malformed hex", `{"trace_id":"NOTHEXATALL"}`, ""},
		{"wrong length", `{"trace_id":"0123456789abcdef"}`, "0123456789abcdef"},
		{"not an object", `[1,2,3]`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen trace.SpanContext
			handler := asynq.HandlerFunc(func(ctx context.Context, tk *asynq.Task) error {
				seen = trace.SpanContextFromContext(ctx)
				return nil
			})
			// 坏形态不得拒收业务任务:handler 必须被调用且零错误;段根
			// span 照常开(无上游 = 自成新链,「段」的语义),trace 面零降级
			// 只体现在不继承 payload 值——继承面由合法臂钉死。
			if err := traceMiddleware(tracer, slog.Default())(handler).
				ProcessTask(context.Background(), asynq.NewTask("node.task_enqueue", []byte(tc.payload))); err != nil {
				t.Fatalf("process: %v", err)
			}
			if !seen.HasTraceID() {
				t.Fatalf("segment root span missing for payload %q", tc.payload)
			}
			if tc.noInheritPrefix != "" && len(seen.TraceID().String()) >= len(tc.noInheritPrefix) &&
				seen.TraceID().String()[:len(tc.noInheritPrefix)] == tc.noInheritPrefix {
				t.Fatalf("degenerate trace %q partially inherited by segment span %s",
					tc.payload, seen.TraceID())
			}
		})
	}
}

func TestTraceMiddlewareNilTracerPassthrough(t *testing.T) {
	called := false
	handler := asynq.HandlerFunc(func(ctx context.Context, tk *asynq.Task) error {
		called = true
		if trace.SpanContextFromContext(ctx).HasTraceID() {
			t.Fatal("nil tracer must leave ctx free of trace ids")
		}
		return nil
	})
	if err := traceMiddleware(nil, slog.Default())(handler).
		ProcessTask(context.Background(), asynq.NewTask("node.task_enqueue", []byte(`{"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736"}`))); err != nil {
		t.Fatalf("process: %v", err)
	}
	if !called {
		t.Fatal("handler not invoked")
	}
}
