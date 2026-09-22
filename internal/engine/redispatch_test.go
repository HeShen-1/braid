package engine

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// RedispatchNode(T2.4 节点级重试再派发)表驱动:status-preserving CAS 的
// 行效应(attempts+1 / worker_id 重写 / deadline 刷新 / error_code 与快照不动)
// 与谓词仲裁(EXECUTING+version 缺一不可)。
//
// 变异自查(实现错 → 哪条红):
//   - 谓词丢了 version → 冲突子测试红(过期 version 仍命中);
//   - 谓词丢了 status → 非 EXECUTING 子测试红;
//   - 意外写 error_code/input_snapshot → 行效应子测试红。

func TestRedispatchEdge(t *testing.T) {
	f := newFixture(t)
	id := f.node()
	snap := json.RawMessage(`{"prompt":"hello"}`)
	if _, err := ClaimNode(f.ctx(), f.db, id, 1, ClaimInput{WorkerID: "w1", InputSnapshot: snap, DeadlineAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	deadline := time.Now().UTC().Truncate(time.Millisecond).Add(5 * time.Minute)

	v, err := RedispatchNode(f.ctx(), f.db, id, 2, RedispatchInput{WorkerID: "w2", DeadlineAt: deadline})
	if err != nil {
		t.Fatalf("redispatch: %v", err)
	}
	if v != 3 {
		t.Fatalf("newVer=%d, want 3", v)
	}
	r := scanNode(t, f.db, id)
	if r.Status != NodeExecuting || r.Version != 3 || r.Attempts != 2 {
		t.Fatalf("行=%+v, want EXECUTING/v3/attempts2", r)
	}
	if r.WorkerID.String != "w2" {
		t.Fatalf("worker_id=%q, want w2(再派发方重写)", r.WorkerID.String)
	}
	if !r.Deadline.Valid || !r.Deadline.Time.Equal(deadline) {
		t.Fatalf("deadline=%v, want %v(重试窗必须刷新,否则 T2.5 误判超时)", r.Deadline, deadline)
	}
	if !r.Snapshot.Valid {
		t.Fatal("input_snapshot 被清(重试不重写快照——前驱输出终态不可变)")
	}
	jsonEq(t, r.Snapshot, snap)
	if r.ErrorCode.Valid {
		t.Fatalf("error_code=%q 被写入(终局失败专用列)", r.ErrorCode.String)
	}
	if r.FinishedAt.Valid {
		t.Fatalf("finished_at=%v 被写入(非终态)", r.FinishedAt)
	}
}

// redispatchRow 按目标状态造行,返回 fixture、行 id 与该行当前 version。
func redispatchRow(t *testing.T, status NodeState) (*fixture, uint64, uint32) {
	t.Helper()
	f := newFixture(t)
	id := f.node()
	switch status {
	case NodeExecuting:
		if _, err := ClaimNode(f.ctx(), f.db, id, 1, ClaimInput{WorkerID: "w1", InputSnapshot: json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		return f, id, 2
	case NodePending:
		return f, id, 1
	case NodeSucceeded:
		if _, err := ClaimNode(f.ctx(), f.db, id, 1, ClaimInput{WorkerID: "w1", InputSnapshot: json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if _, err := SucceedNode(f.ctx(), f.db, id, 2, "w1", nil); err != nil {
			t.Fatalf("succeed: %v", err)
		}
		return f, id, 3
	}
	t.Fatalf("unreachable status %s", status)
	return nil, 0, 0
}

func TestRedispatchPredicate(t *testing.T) {
	tests := []struct {
		name      string
		status    NodeState
		expectVer uint32
		wantOK    bool // true = 期待 CAS 命中
	}{
		{"executing_current_version", NodeExecuting, 2, true},
		{"executing_stale_version", NodeExecuting, 1, false},
		{"pending", NodePending, 1, false},
		{"succeeded", NodeSucceeded, 3, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, id, curVer := redispatchRow(t, tt.status)
			if tt.expectVer > curVer && tt.wantOK {
				t.Fatalf("用例自相矛盾:expectVer %d > 当前 %d", tt.expectVer, curVer)
			}
			v, err := RedispatchNode(f.ctx(), f.db, id, tt.expectVer, RedispatchInput{WorkerID: "w"})
			if tt.wantOK {
				if err != nil {
					t.Fatalf("redispatch: %v", err)
				}
				if v != tt.expectVer+1 {
					t.Fatalf("newVer=%d, want %d", v, tt.expectVer+1)
				}
				return
			}
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("err=%v, want ErrConflict", err)
			}
			var ce *ConflictError
			if !errors.As(err, &ce) || ce.Observed != tt.status {
				t.Fatalf("conflict 归因=%+v, want Observed=%s", ce, tt.status)
			}
		})
	}
}
