package reaper

// D-t121 #4 span 面:RunOnce 在 Tracer 非 nil 时导出 tick 根 span + 五作业
// 子 span(名 = reaper.<job>),nil tracer 零导出。「span 恒不进发射 ctx」
// 的解耦由 nodes faultinject e2e 全链面断言端到端钉死(spans 在场时非空
// trace 仍恒 = X;若解耦破坏,reaper 自生行携 tick trace ⇒ e2e 红)。

import (
	"context"
	"slices"
	"testing"

	"go.opentelemetry.io/otel/codes"

	"braid/internal/db/sqltest"
	"braid/internal/scheduler"
	"braid/internal/tracing/tracingtest"
)

func TestRunOnceSpans(t *testing.T) {
	d := sqltest.MustOpenAt(t, reaperSchema)
	s, err := scheduler.New(scheduler.Deps{DB: d, IDs: mustTestIDs(t), Queue: "reap-test"})
	if err != nil {
		t.Fatalf("scheduler New: %v", err)
	}
	tp, rec := tracingtest.New()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	r, err := New(Deps{
		DB: d, Sched: s, IDs: mustTestIDs(t), Logger: testLogger(t),
		Tracer: tp.Tracer("braid/reaper-test"),
	}, DefaultConfig())
	if err != nil {
		t.Fatalf("reaper New: %v", err)
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce on empty schema: %v", err)
	}
	want := []string{
		"reaper.tick",
		"reaper.scan_attempts",
		"reaper.scan_nodes",
		"reaper.scan_execs",
		"reaper.scan_approvals",
		"reaper.collect_metrics",
		"reaper.verify_audit_chain", // T9.3 第七作业(t12.1 后合者 rebase 补登记,issue #86 注记 3 仲裁)
	}
	got := rec.Names()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("span names = %v, want %v", got, want)
	}
	// tick 是作业 span 的 parent(层级经独立 span ctx 链维系)。
	var tickID string
	for _, sp := range rec.Snapshot() {
		if sp.Name() == "reaper.tick" {
			tickID = sp.SpanContext().SpanID().String()
		}
	}
	for _, sp := range rec.Snapshot() {
		if sp.Name() == "reaper.tick" {
			continue
		}
		if p := sp.Parent().SpanID().String(); p != tickID {
			t.Fatalf("job span %s parent = %s, want tick %s", sp.Name(), p, tickID)
		}
	}
}

// nil tracer:零导出、行为不变(既有全套测试即此形态,这里显式锚定零 span)。
func TestRunOnceNilTracerNoSpans(t *testing.T) {
	r, _ := newReaper(t, DefaultConfig())
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
}

// flush/janitor span 面(review MEDIUM 修):删除两处包装 → 红。tick 错误
// 面臂:作业全败(坏 DB)时 tick span status = Error(RecordError 在 End
// 之前的顺序锚)。
func TestFlushAndJanitorSpans(t *testing.T) {
	r, _ := newReaper(t, DefaultConfig())
	tp, rec := tracingtest.New()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	r.deps.Tracer = tp.Tracer("braid/reaper-test")
	if err := r.flushOnce(context.Background()); err != nil {
		t.Fatalf("flushOnce on empty: %v", err)
	}
	if err := r.janitorOnce(context.Background()); err != nil {
		t.Fatalf("janitorOnce: %v", err)
	}
	names := rec.Names()
	if !slices.Contains(names, "reaper.audit_flush") {
		t.Fatalf("spans = %v, want reaper.audit_flush", names)
	}
	if !slices.Contains(names, "reaper.janitor") {
		t.Fatalf("spans = %v, want reaper.janitor", names)
	}
}

func TestTickSpanCarriesErrorStatus(t *testing.T) {
	d := sqltest.MustOpenAt(t, reaperSchema)
	s, err := scheduler.New(scheduler.Deps{DB: d, IDs: mustTestIDs(t), Queue: "reap-test"})
	if err != nil {
		t.Fatalf("scheduler New: %v", err)
	}
	tp, rec := tracingtest.New()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	r, err := New(Deps{
		DB: d, Sched: s, IDs: mustTestIDs(t), Logger: testLogger(t),
		Tracer: tp.Tracer("braid/reaper-test"),
	}, DefaultConfig())
	if err != nil {
		t.Fatalf("reaper New: %v", err)
	}
	// 坏 DB:全部作业失败 ⇒ firstErr 非_nil ⇒ tick span 必须 Error。
	if err := d.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce on closed DB = nil, want error")
	}
	for _, sp := range rec.Snapshot() {
		if sp.Name() != "reaper.tick" {
			continue
		}
		if sp.Status().Code != codes.Error {
			t.Fatalf("tick span status = %v, want Error (RecordError must precede End)", sp.Status().Code)
		}
		return
	}
	t.Fatal("reaper.tick span missing")
}

// flush/janitor 错误面(review 二轮 advisory 修):RecordError/SetStatus
// 删除变异或 End 序错在此红(成功面由 TestFlushAndJanitorSpans 钉)。
func TestFlushJanitorSpanErrorStatus(t *testing.T) {
	d := sqltest.MustOpenAt(t, reaperSchema)
	s, err := scheduler.New(scheduler.Deps{DB: d, IDs: mustTestIDs(t), Queue: "reap-test"})
	if err != nil {
		t.Fatalf("scheduler New: %v", err)
	}
	tp, rec := tracingtest.New()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	r, err := New(Deps{
		DB: d, Sched: s, IDs: mustTestIDs(t), Logger: testLogger(t),
		Tracer: tp.Tracer("braid/reaper-test"),
	}, DefaultConfig())
	if err != nil {
		t.Fatalf("reaper New: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if err := r.flushOnce(context.Background()); err == nil {
		t.Fatal("flushOnce on closed DB = nil, want error")
	}
	if err := r.janitorOnce(context.Background()); err == nil {
		t.Fatal("janitorOnce on closed DB = nil, want error")
	}
	seen := map[string]bool{}
	for _, sp := range rec.Snapshot() {
		if sp.Name() != "reaper.audit_flush" && sp.Name() != "reaper.janitor" {
			continue
		}
		seen[sp.Name()] = true
		if sp.Status().Code != codes.Error {
			t.Fatalf("%s status = %v, want Error", sp.Name(), sp.Status().Code)
		}
	}
	if !seen["reaper.audit_flush"] || !seen["reaper.janitor"] {
		t.Fatalf("error spans missing: %v", seen)
	}
}
