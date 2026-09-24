package engine

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// T4.3 审批超时 CAS 原语:FailSuspendedNode(WAITING_APPROVAL→FAILED,fail
// 策略终败,DDL error_code 词表预留 approval_timeout)与 MarkNodeEscalated
// (escalated_at 一次性落定,Q9「escalate 仅一次」)。

// suspendOK 把节点推进到 WAITING_APPROVAL 并返回当前 version(挂起截止由
// 调用方给,测试用远期值保证升级/终败判定不被时间谓词污染)。
func suspendOK(t *testing.T, f *fixture, id uint64, worker string, deadline time.Time) uint32 {
	t.Helper()
	if _, err := ClaimNode(f.ctx(), f.db, id, 1, ClaimInput{WorkerID: worker, InputSnapshot: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("claim %d: %v", id, err)
	}
	v, err := SuspendNode(f.ctx(), f.db, id, 2, worker, deadline)
	if err != nil {
		t.Fatalf("suspend %d: %v", id, err)
	}
	return v
}

func TestFailSuspendedEdge(t *testing.T) {
	f := newFixture(t)
	id := f.node()
	v := suspendOK(t, f, id, "w1", time.Now().UTC().Add(72*time.Hour))

	v2, err := FailSuspendedNode(f.ctx(), f.db, id, v, "approval_timeout",
		"deadline exceeded, policy=fail")
	if err != nil {
		t.Fatalf("fail suspended: %v", err)
	}
	if v2 != v+1 {
		t.Fatalf("newVer=%d, want %d", v2, v+1)
	}
	r := scanNode(t, f.db, id)
	if r.Status != NodeFailed || r.ErrorCode.String != "approval_timeout" ||
		!r.FinishedAt.Valid || r.Deadline.Valid {
		t.Fatalf("行=%+v, want FAILED/approval_timeout/finished/deadline NULL", r)
	}
	if r.WorkerID.String != "w1" {
		t.Fatalf("worker_id 应保留挂起归因, got %q", r.WorkerID.String)
	}
}

func TestFailSuspendedTaxonomy(t *testing.T) {
	t.Run("EXECUTING 行拒绝(那是 FailNode 的边)", func(t *testing.T) {
		f := newFixture(t)
		id := f.node()
		claimOK(t, f, id, "w1")
		if _, err := FailSuspendedNode(f.ctx(), f.db, id, 2, "x", "y"); !errors.Is(err, ErrConflict) {
			t.Fatalf("got %v, want ErrConflict", err)
		}
		if r := scanNode(t, f.db, id); r.Status != NodeExecuting || r.Version != 2 {
			t.Fatalf("跨边 fail 不得推进行: %+v", r)
		}
	})
	t.Run("过期 version 必败", func(t *testing.T) {
		f := newFixture(t)
		id := f.node()
		v := suspendOK(t, f, id, "w1", time.Now().Add(time.Hour))
		if _, err := FailSuspendedNode(f.ctx(), f.db, id, v-1, "x", "y"); !errors.Is(err, ErrConflict) {
			t.Fatalf("got %v, want ErrConflict", err)
		}
		if r := scanNode(t, f.db, id); r.Status != NodeWaitingApproval || r.Version != v {
			t.Fatalf("过期 fail 不得推进行: %+v", r)
		}
	})
}

func TestMarkEscalated(t *testing.T) {
	f := newFixture(t)
	id := f.node()
	v := suspendOK(t, f, id, "w1", time.Now().UTC().Add(72*time.Hour))

	v2, err := MarkNodeEscalated(f.ctx(), f.db, id, v)
	if err != nil {
		t.Fatalf("mark escalated: %v", err)
	}
	if v2 != v+1 {
		t.Fatalf("newVer=%d, want %d", v2, v+1)
	}
	r := scanNode(t, f.db, id)
	if !r.EscalatedAt.Valid {
		t.Fatalf("escalated_at 应落定: %+v", r)
	}
	if r.Status != NodeWaitingApproval {
		t.Fatalf("升级是 status-preserving: %+v", r)
	}
	if !r.Deadline.Valid {
		t.Fatalf("升级不得清 deadline(仍需超时判定): %+v", r)
	}

	// 恰好一次:再调(带最新 version)0 行——escalated_at IS NULL 谓词是并发
	// 双写的 DB 兜底,Expect==Observed 的 Conflict 形态归 checkNodeCAS。
	if _, err := MarkNodeEscalated(f.ctx(), f.db, id, v2); !errors.Is(err, ErrConflict) {
		t.Fatalf("二次升级 got %v, want ErrConflict", err)
	}
	if r2 := scanNode(t, f.db, id); r2.Version != v2 || !r2.EscalatedAt.Time.Equal(r.EscalatedAt.Time) {
		t.Fatalf("二次升级不得改行: %+v → %+v", r, r2)
	}
}

func TestMarkEscalatedTaxonomy(t *testing.T) {
	t.Run("EXECUTING 行拒绝(升级只在挂起行上)", func(t *testing.T) {
		f := newFixture(t)
		id := f.node()
		claimOK(t, f, id, "w1")
		if _, err := MarkNodeEscalated(f.ctx(), f.db, id, 2); !errors.Is(err, ErrConflict) {
			t.Fatalf("got %v, want ErrConflict", err)
		}
		if r := scanNode(t, f.db, id); r.EscalatedAt.Valid {
			t.Fatalf("EXECUTING 行不得落升级时刻: %+v", r)
		}
	})
	t.Run("终态行拒绝", func(t *testing.T) {
		f := newFixture(t)
		id := f.node()
		v := suspendOK(t, f, id, "w1", time.Now().Add(time.Hour))
		if _, err := FailSuspendedNode(f.ctx(), f.db, id, v, "approval_timeout", "x"); err != nil {
			t.Fatalf("fail suspended: %v", err)
		}
		if _, err := MarkNodeEscalated(f.ctx(), f.db, id, v+1); !errors.Is(err, ErrConflict) {
			t.Fatalf("终态行升级 got %v, want ErrConflict", err)
		}
	})
}
