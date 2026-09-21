package engine

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// T1 合法边全表:10 条边逐条走 helper,断言 to-status、newVer 与边专属列。
// 测试自身也只用 helper 造状态(不用裸 UPDATE),与生产同一纪律。

func TestClaimEdge(t *testing.T) {
	f := newFixture(t)
	id := f.node()
	deadline := time.Now().UTC().Truncate(time.Millisecond).Add(time.Minute)
	snap := json.RawMessage(`{"prompt":"hello","limit":8}`)

	v, err := ClaimNode(f.ctx(), f.db, id, 1, ClaimInput{WorkerID: "w1", DeadlineAt: deadline, InputSnapshot: snap})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if v != 2 {
		t.Fatalf("newVer=%d, want 2", v)
	}
	r := scanNode(t, f.db, id)
	if r.Status != NodeExecuting || r.Version != 2 || r.Attempts != 1 {
		t.Fatalf("行=%+v, want EXECUTING/v2/attempts1", r)
	}
	if !r.StartedAt.Valid {
		t.Fatalf("started_at 应落定(首次 EXECUTING)")
	}
	if r.WorkerID.String != "w1" {
		t.Fatalf("worker_id=%q, want w1", r.WorkerID.String)
	}
	jsonEq(t, r.Snapshot, snap)
	if !r.Deadline.Valid || !r.Deadline.Time.Equal(deadline) {
		t.Fatalf("deadline=%v, want %v", r.Deadline, deadline)
	}
}

func TestResumeEdge(t *testing.T) {
	f := newFixture(t)
	id := f.node()
	snapA := json.RawMessage(`{"v":"first"}`)
	snapB := json.RawMessage(`{"v":"second","extra":{"k":[1,2]}}`)
	if _, err := ClaimNode(f.ctx(), f.db, id, 1, ClaimInput{WorkerID: "w1", InputSnapshot: snapA}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	firstStart := scanNode(t, f.db, id).StartedAt
	if _, err := SuspendNode(f.ctx(), f.db, id, 2, "w1", time.Time{}.Add(time.Hour)); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	v, err := ResumeNode(f.ctx(), f.db, id, 3, ClaimInput{WorkerID: "w2", InputSnapshot: snapB})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if v != 4 {
		t.Fatalf("newVer=%d, want 4", v)
	}
	r := scanNode(t, f.db, id)
	if r.Status != NodeExecuting || r.Attempts != 2 {
		t.Fatalf("行=%+v, want EXECUTING/attempts2", r)
	}
	if !r.StartedAt.Time.Equal(firstStart.Time) {
		t.Fatalf("started_at 跨复入变化: %v → %v(DDL:首次 EXECUTING 不变)", firstStart, r.StartedAt)
	}
	jsonEq(t, r.Snapshot, snapB) // 复入快照重写为新解析值
}

func TestSucceedEdge(t *testing.T) {
	f := newFixture(t)
	id := f.node()
	// 带截止认领 → succeed 清 deadline 的断言才是有效证明
	claimWithDeadlineOK(t, f, id, "w1", time.Now().UTC().Add(time.Minute))
	out := json.RawMessage(`{"summary":"done","score":0.5}`)
	v, err := SucceedNode(f.ctx(), f.db, id, 2, "w1", out)
	if err != nil {
		t.Fatalf("succeed: %v", err)
	}
	if v != 3 {
		t.Fatalf("newVer=%d, want 3", v)
	}
	r := scanNode(t, f.db, id)
	if r.Status != NodeSucceeded || !r.FinishedAt.Valid || r.Deadline.Valid {
		t.Fatalf("行=%+v, want SUCCEEDED/finished/deadline NULL", r)
	}
	jsonEq(t, r.Output, out)
	jsonEq(t, r.Snapshot, json.RawMessage(`{}`)) // 终态保留快照(复现真相源)
}

func TestFailEdge(t *testing.T) {
	f := newFixture(t)
	id := f.node()
	claimOK(t, f, id, "w1")
	v, err := FailNode(f.ctx(), f.db, id, 2, "w1", "llm_rate_limited", "429 too many requests")
	if err != nil {
		t.Fatalf("fail: %v", err)
	}
	if v != 3 {
		t.Fatalf("newVer=%d, want 3", v)
	}
	r := scanNode(t, f.db, id)
	if r.Status != NodeFailed || r.ErrorCode.String != "llm_rate_limited" || !r.FinishedAt.Valid || r.Deadline.Valid {
		t.Fatalf("行=%+v", r)
	}
}

func TestSuspendEdge(t *testing.T) {
	f := newFixture(t)
	id := f.node()
	claimOK(t, f, id, "w1")
	waDeadline := time.Now().UTC().Truncate(time.Millisecond).Add(72 * time.Hour)
	if _, err := SuspendNode(f.ctx(), f.db, id, 2, "w1", waDeadline); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	r := scanNode(t, f.db, id)
	if r.Status != NodeWaitingApproval {
		t.Fatalf("status=%s, want WAITING_APPROVAL", r.Status)
	}
	if r.WorkerID.String != "w1" {
		t.Fatalf("worker_id 应保留归因,got %q", r.WorkerID.String)
	}
	if !r.Deadline.Valid || !r.Deadline.Time.Equal(waDeadline) {
		t.Fatalf("deadline=%v, want %v(挂起截止覆盖执行截止)", r.Deadline, waDeadline)
	}
}

func TestRequeueEdge(t *testing.T) {
	f := newFixture(t)
	// requeue 仅自 EXECUTING:FAILED 是终态,必须拒绝(否决 FAILED 复活)
	idFailed := f.node()
	claimOK(t, f, idFailed, "w1")
	if _, err := FailNode(f.ctx(), f.db, idFailed, 2, "w1", "x", "y"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if _, err := RequeueNode(f.ctx(), f.db, idFailed, 3); !errors.Is(err, ErrConflict) {
		t.Fatalf("FAILED→PENDING 应拒绝,got %v", err)
	}

	// 干净路径:claim(带截止) → requeue → 干净 PENDING → 再认领(新快照)
	id := f.node()
	v, err := ClaimNode(f.ctx(), f.db, id, 1, ClaimInput{
		WorkerID: "w1", DeadlineAt: time.Now().UTC().Add(time.Minute), InputSnapshot: json.RawMessage(`{"a":1}`),
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	v2, err := RequeueNode(f.ctx(), f.db, id, v)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if v2 != 3 {
		t.Fatalf("newVer=%d, want 3", v2)
	}
	r := scanNode(t, f.db, id)
	if r.Status != NodePending || r.WorkerID.Valid || r.Snapshot.Valid || r.Deadline.Valid ||
		r.ErrorCode.Valid || r.Attempts != 1 {
		t.Fatalf("requeue 后应干净 PENDING(五列 NULL 化,attempts 保留),行=%+v", r)
	}
	if _, err := ClaimNode(f.ctx(), f.db, id, 3, ClaimInput{WorkerID: "w2", InputSnapshot: json.RawMessage(`{"b":2}`)}); err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	jsonEq(t, scanNode(t, f.db, id).Snapshot, json.RawMessage(`{"b":2}`))
}

func TestSkipEdge(t *testing.T) {
	f := newFixture(t)
	id := f.node()
	v, err := SkipNode(f.ctx(), f.db, id, 1)
	if err != nil {
		t.Fatalf("skip: %v", err)
	}
	if v != 2 {
		t.Fatalf("newVer=%d, want 2", v)
	}
	r := scanNode(t, f.db, id)
	if r.Status != NodeSkipped || !r.FinishedAt.Valid || r.Snapshot.Valid || r.StartedAt.Valid {
		t.Fatalf("行=%+v, want SKIPPED/finished/snapshot NULL/未起跑", r)
	}
}

func TestCancelEdges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		toExec bool
		toWA   bool
	}{
		{"from PENDING", false, false},
		{"from EXECUTING", true, false},
		{"from WAITING_APPROVAL", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			id := f.node()
			var ver uint32 = 1
			if tc.toExec {
				claimOK(t, f, id, "w1")
				ver = 2
			}
			if tc.toWA {
				if _, err := SuspendNode(f.ctx(), f.db, id, ver, "w1", time.Now().Add(time.Hour)); err != nil {
					t.Fatalf("suspend: %v", err)
				}
				ver++
			}
			v, err := CancelNode(f.ctx(), f.db, id, ver)
			if err != nil {
				t.Fatalf("cancel: %v", err)
			}
			if v != ver+1 {
				t.Fatalf("newVer=%d, want %d", v, ver+1)
			}
			r := scanNode(t, f.db, id)
			if r.Status != NodeCancelled || !r.FinishedAt.Valid || r.Deadline.Valid {
				t.Fatalf("行=%+v", r)
			}
		})
	}
}

// T6 taxonomy + 非法/过期 CAS:三因各一条最小复现;非法操作后回读逐列不变。
func TestCASTaxonomy(t *testing.T) {
	t.Run("NotFound", func(t *testing.T) {
		f := newFixture(t)
		if _, err := ClaimNode(f.ctx(), f.db, newID(), 1, ClaimInput{WorkerID: "w", InputSnapshot: json.RawMessage(`{}`)}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})
	t.Run("InvalidSnapshot 不触 DB", func(t *testing.T) {
		f := newFixture(t)
		id := f.node()
		if _, err := ClaimNode(f.ctx(), f.db, id, 1, ClaimInput{WorkerID: "w", InputSnapshot: json.RawMessage(`{"broken"}`)}); !errors.Is(err, ErrInvalidSnapshot) {
			t.Fatalf("got %v, want ErrInvalidSnapshot", err)
		}
		if r := scanNode(t, f.db, id); r.Status != NodePending || r.Version != 1 {
			t.Fatalf("非法快照不得触碰行: %+v", r)
		}
	})
	t.Run("Conflict 带观测归因", func(t *testing.T) {
		f := newFixture(t)
		id := f.node()
		claimOK(t, f, id, "w1")
		_, err := ClaimNode(f.ctx(), f.db, id, 1, ClaimInput{WorkerID: "w2", InputSnapshot: json.RawMessage(`{}`)})
		var ce *ConflictError
		if !errors.As(err, &ce) || !errors.Is(err, ErrConflict) {
			t.Fatalf("got %v, want *ConflictError", err)
		}
		if ce.Observed != NodeExecuting || ce.ObservedVer != 2 || ce.Expect != NodePending || ce.ExpectVer != 1 {
			t.Fatalf("归因=%+v, want expect PENDING/v1 observed EXECUTING/v2", ce)
		}
		if r := scanNode(t, f.db, id); r.Version != 2 || r.WorkerID.String != "w1" {
			t.Fatalf("败方不得推进任何列: %+v", r)
		}
	})
	t.Run("租约防御:旧 worker 收口必败", func(t *testing.T) {
		f := newFixture(t)
		id := f.node()
		claimOK(t, f, id, "w1")
		if _, err := SucceedNode(f.ctx(), f.db, id, 2, "w2", json.RawMessage(`{}`)); !errors.Is(err, ErrConflict) {
			t.Fatalf("非持有者收口 got %v, want ErrConflict", err)
		}
		if r := scanNode(t, f.db, id); r.Status != NodeExecuting {
			t.Fatalf("行被非持有者推进: %+v", r)
		}
	})
	t.Run("过期 version 必败", func(t *testing.T) {
		f := newFixture(t)
		id := f.node()
		claimOK(t, f, id, "w1")
		if _, err := SucceedNode(f.ctx(), f.db, id, 1, "w1", json.RawMessage(`{}`)); !errors.Is(err, ErrConflict) {
			t.Fatalf("过期 version got %v, want ErrConflict", err)
		}
	})
	t.Run("CancelNode 过期 version 必败", func(t *testing.T) {
		// 变异验证:删掉 CancelNode 谓词的 AND version=? 全套测试曾照绿——钉死它
		f := newFixture(t)
		id := f.node()
		claimOK(t, f, id, "w1") // version 1→2
		if _, err := CancelNode(f.ctx(), f.db, id, 1); !errors.Is(err, ErrConflict) {
			t.Fatalf("过期 version cancel got %v, want ErrConflict", err)
		}
		if r := scanNode(t, f.db, id); r.Status != NodeExecuting || r.Version != 2 {
			t.Fatalf("过期 cancel 不得推进行: %+v", r)
		}
		if _, err := CancelNode(f.ctx(), f.db, id, 2); err != nil {
			t.Fatalf("正确 version cancel: %v", err)
		}
	})
	t.Run("终态行拒绝一切迁移", func(t *testing.T) {
		f := newFixture(t)
		id := f.node()
		claimOK(t, f, id, "w1")
		if _, err := SucceedNode(f.ctx(), f.db, id, 2, "w1", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("succeed: %v", err)
		}
		before := scanNode(t, f.db, id)
		for _, act := range []struct {
			name string
			fn   func() error
		}{
			{"cancel", func() error { _, err := CancelNode(f.ctx(), f.db, id, 3); return err }},
			{"requeue", func() error { _, err := RequeueNode(f.ctx(), f.db, id, 3); return err }},
			{"claim", func() error {
				_, err := ClaimNode(f.ctx(), f.db, id, 3, ClaimInput{WorkerID: "w", InputSnapshot: json.RawMessage(`{}`)})
				return err
			}},
		} {
			if err := act.fn(); !errors.Is(err, ErrConflict) {
				t.Fatalf("%s on SUCCEEDED: got %v, want ErrConflict", act.name, err)
			}
		}
		if r := scanNode(t, f.db, id); r.Version != before.Version || r.Status != before.Status {
			t.Fatalf("终态行被改动: %+v → %+v", before, r)
		}
	})
}

func claimOK(t *testing.T, f *fixture, id uint64, worker string) {
	t.Helper()
	// 带截止时间认领:让后续 Succeed/Fail/Requeue 的 deadline_at=NULL 断言非空洞
	// (变异验证:不带截止时删掉这些清列语句测试照绿)。
	claimWithDeadlineOK(t, f, id, worker, time.Now().UTC().Add(time.Minute))
}

func claimWithDeadlineOK(t *testing.T, f *fixture, id uint64, worker string, deadline time.Time) {
	t.Helper()
	if _, err := ClaimNode(f.ctx(), f.db, id, 1, ClaimInput{
		WorkerID: worker, DeadlineAt: deadline, InputSnapshot: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("claim %d: %v", id, err)
	}
}
