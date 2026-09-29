package worker

// OTel 消费半边单元面(D-t121):payload (trace_id, span_id) → 段根 span 的
// remote parent 用真 span id;span_id 缺席 = legacy 派生回退;无有效链上下文
// → 孤儿链口径短路(零 span、零注入、任务照常执行);tracer nil 直通。
// parent 精确断言用 sdktrace 捕获导出器直读 ReadOnlySpan.Parent();落库与
// 跨进程续链的端到端断言在 nodes e2e 真链层(T8.2 片 5)。

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel/trace"

	"braid/internal/tracing"
	"braid/internal/tracing/tracingtest"
)

// captureTracer 构造直读导出面的 tracer(parent 断言需要 ReadOnlySpan 面,
// 生产导出器不回读;共享工具见 internal/tracing/tracingtest)。
func captureTracer(t *testing.T) (trace.Tracer, *tracingtest.Recorder) {
	t.Helper()
	tp, rec := tracingtest.New()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp.Tracer("braid/worker-test"), rec
}

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

// D-t121 #3 主断言:payload 携真 span id 时,段根 span 的 parent ==
// payload span id(跨进程 span 树闭合,P0 伪 parent 消解)。
func TestTraceMiddlewareRealParentSpanID(t *testing.T) {
	const (
		wantTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
		wantSpan  = "00f067aa0ba902b7"
	)
	tracer, rec := captureTracer(t)
	payload, _ := json.Marshal(map[string]any{
		"execution_id": uint64(1), "trace_id": wantTrace, "span_id": wantSpan,
	})
	noop := asynq.HandlerFunc(func(context.Context, *asynq.Task) error { return nil })
	if err := traceMiddleware(tracer, slog.Default())(noop).
		ProcessTask(context.Background(), asynq.NewTask("node.task_enqueue", payload)); err != nil {
		t.Fatalf("process: %v", err)
	}
	spans := rec.Snapshot()
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	s := spans[0]
	if got := s.SpanContext().TraceID().String(); got != wantTrace {
		t.Fatalf("segment trace = %s, want %s", got, wantTrace)
	}
	if p := s.Parent().SpanID().String(); p != wantSpan {
		t.Fatalf("segment parent span = %s, want payload span %s (real parent)", p, wantSpan)
	}
}

// span_id 缺席(legacy payload / reaper 重派)= trace 前 8 字节派生回退
// (D-t62 #2 形态保留);span_id 坏形态同回退(观察面不拒收)。
func TestTraceMiddlewareDerivedParentFallback(t *testing.T) {
	const wantTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	derived := wantTrace[:16] // D-t62 #2:trace id 前 8 字节(16 hex 字符)派生
	cases := []struct {
		name    string
		payload string
	}{
		{"legacy no span_id", `{"execution_id":1,"trace_id":"` + wantTrace + `"}`},
		{"malformed span_id", `{"execution_id":1,"trace_id":"` + wantTrace + `","span_id":"NOTHEX"}`},
		{"wrong length span_id", `{"execution_id":1,"trace_id":"` + wantTrace + `","span_id":"0123456789abcdef0123"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tracer, rec := captureTracer(t)
			noop := asynq.HandlerFunc(func(context.Context, *asynq.Task) error { return nil })
			if err := traceMiddleware(tracer, slog.Default())(noop).
				ProcessTask(context.Background(), asynq.NewTask("node.task_enqueue", []byte(tc.payload))); err != nil {
				t.Fatalf("process: %v", err)
			}
			spans := rec.Snapshot()
			if len(spans) != 1 {
				t.Fatalf("exported spans = %d, want 1 (valid trace must still link)", len(spans))
			}
			if p := spans[0].Parent().SpanID().String(); p != derived {
				t.Fatalf("derived parent = %s, want %s (trace-id prefix fallback)", p, derived)
			}
		})
	}
}

// D-t121 #5 孤儿链口径短路:无有效链上下文(trace 缺席/坏形态/载荷非对象)
// → handler 必被调用、零错误、ctx 无 span(P0「自起段根」废止)。
func TestTraceMiddlewareOrphanShortCircuit(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"no trace key", `{"execution_id":1}`},
		{"empty trace", `{"execution_id":1,"trace_id":""}`},
		{"malformed hex", `{"trace_id":"NOTHEXATALL"}`},
		{"wrong length", `{"trace_id":"0123456789abcdef"}`},
		{"not an object", `[1,2,3]`},
		// notify 生产形态:发射面钉死不携 trace(approval.notified 行 NULL)。
		{"notify payload without trace", `{"execution_id":1,"node_execution_id":2,"reason":"escalated","round":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tracer, rec := captureTracer(t)
			called := false
			handler := asynq.HandlerFunc(func(ctx context.Context, tk *asynq.Task) error {
				called = true
				if trace.SpanContextFromContext(ctx).HasTraceID() {
					t.Fatal("short-circuit must leave ctx free of trace ids (no orphan root span)")
				}
				return nil
			})
			if err := traceMiddleware(tracer, slog.Default())(handler).
				ProcessTask(context.Background(), asynq.NewTask("node.task_enqueue", []byte(tc.payload))); err != nil {
				t.Fatalf("process: %v", err)
			}
			if !called {
				t.Fatal("handler not invoked (observation face must not reject business tasks)")
			}
			if n := len(rec.Snapshot()); n != 0 {
				t.Fatalf("exported spans = %d, want 0 (orphan short-circuit)", n)
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

// warn 留痕面(review MEDIUM 修):坏形态 trace/span 各出一条 warn——删掉
// warn 回归在此红(行为臂由 OrphanShortCircuit/DerivedParentFallback 钉)。
func TestTraceMiddlewareWarnsOnBadPayload(t *testing.T) {
	var buf lockedBuffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	noop := asynq.HandlerFunc(func(context.Context, *asynq.Task) error { return nil })
	const traceOK = "4bf92f3577b34da6a3ce929d0e0e4736"
	if err := traceMiddleware(slogDefaultTracer(t), log)(noop).
		ProcessTask(context.Background(), asynq.NewTask("node.task_enqueue",
			[]byte(`{"trace_id":"NOTHEXATALL"}`))); err != nil {
		t.Fatalf("process: %v", err)
	}
	if err := traceMiddleware(slogDefaultTracer(t), log)(noop).
		ProcessTask(context.Background(), asynq.NewTask("node.task_enqueue",
			[]byte(`{"trace_id":"`+traceOK+`","span_id":"NOTHEX"}`))); err != nil {
		t.Fatalf("process: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "worker.trace_payload_invalid") {
		t.Fatalf("missing worker.trace_payload_invalid warn:\n%s", out)
	}
	if !strings.Contains(out, "worker.span_payload_invalid") {
		t.Fatalf("missing worker.span_payload_invalid warn:\n%s", out)
	}
}

// slogdefaultTracer:off 导出 tracer(warn 面测试用,不关心 span 导出)。
func slogDefaultTracer(t *testing.T) trace.Tracer {
	t.Helper()
	prov, err := tracing.NewProvider(tracing.Config{Exporter: tracing.ExporterOff, ServiceName: "test"})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	t.Cleanup(func() { _ = prov.Shutdown(context.Background()) })
	return prov.Tracer("braid/worker-test")
}

// lockedBuffer 是并发安全 bytes.Buffer(slog handler 写入)。
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
