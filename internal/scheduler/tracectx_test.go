package scheduler

// D-t121 #3 发射面半边:traceIDFromCtx / spanIDFromCtx 对同一 span ctx
// 提取出同源的 (trace, span) 对——payload 携真 span id 的发射单点。无
// span ctx(测试路径 / Tracing(nil))双空 = payload 双键缺席。消费半边
// (remote parent 真值)在 worker tracemw_test 与 worker e2e 协议契约测试。

import (
	"context"
	"encoding/json"
	"testing"

	"braid/internal/tracing"
	"braid/internal/tracing/tracingtest"
)

func TestTraceAndSpanIDFromCtx(t *testing.T) {
	// 无 span ctx:双空。
	if got := traceIDFromCtx(context.Background()); got != "" {
		t.Fatalf("traceIDFromCtx(background) = %q, want empty", got)
	}
	if got := spanIDFromCtx(context.Background()); got != "" {
		t.Fatalf("spanIDFromCtx(background) = %q, want empty", got)
	}

	// 有效 span ctx:同源提取,trace 32hex / span 16hex。
	prov, err := tracing.NewProvider(tracing.Config{Exporter: tracing.ExporterOff, ServiceName: "test"})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	t.Cleanup(func() { _ = prov.Shutdown(context.Background()) })
	ctx, span := prov.Tracer("braid/test").Start(context.Background(), "op")
	defer span.End()
	sc := span.SpanContext()

	gotTrace, gotSpan := traceIDFromCtx(ctx), spanIDFromCtx(ctx)
	if gotTrace != sc.TraceID().String() {
		t.Fatalf("traceIDFromCtx = %q, want %q", gotTrace, sc.TraceID().String())
	}
	if gotSpan != sc.SpanID().String() {
		t.Fatalf("spanIDFromCtx = %q, want %q", gotSpan, sc.SpanID().String())
	}
	if len(gotTrace) != 32 || len(gotSpan) != 16 {
		t.Fatalf("id shapes: trace %d chars, span %d chars, want 32/16", len(gotTrace), len(gotSpan))
	}
}

// D-t121 #3 发射面主断言(review HIGH 修):Instantiate(exec:)与
// Start 派发(att:)的 payload 携 span_id == 发射 ctx 当段活跃 span id——
// 删掉任一发射点接线 → 本测试红(消费侧静默退化到派生回退,此前全仓
// 测试无感)。mustPayload/taskEvents/waitNodeStatus 为 sched_test 既有面。
func TestPayloadSpanIDEmitted(t *testing.T) {
	s, d := newSched(t)
	tp, _ := tracingtest.New()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	sctx, span := tp.Tracer("braid/test").Start(context.Background(), "emit")
	defer span.End()
	wantSpan := span.SpanContext().SpanID().String()

	defID := seedDef(t, d, chainDSL)
	execID, err := s.Instantiate(sctx, InstantiateParams{
		DefinitionID: defID, Input: json.RawMessage(`{"q":"hi"}`), TriggeredBy: "test",
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	t.Cleanup(func() { cleanupExec(t, d, execID) })

	var start NodeTaskPayload
	if err := s.Start(sctx, execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	events := taskEvents(t, d, execID)
	if len(events) != 2 {
		t.Fatalf("outbox events = %v, want exec:+att:", events)
	}
	var es ExecStartPayload
	if err := json.Unmarshal(mustPayload(t, d, events[0]), &es); err != nil {
		t.Fatalf("exec payload: %v", err)
	}
	if es.SpanID != wantSpan {
		t.Fatalf("exec: payload span_id = %q, want emitting span %q", es.SpanID, wantSpan)
	}
	if err := json.Unmarshal(mustPayload(t, d, events[1]), &start); err != nil {
		t.Fatalf("att payload: %v", err)
	}
	if start.SpanID != wantSpan {
		t.Fatalf("att: payload span_id = %q, want emitting span %q (same Start ctx)", start.SpanID, wantSpan)
	}
}
