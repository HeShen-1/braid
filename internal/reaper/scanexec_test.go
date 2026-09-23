package reaper

// 用例 8-9(扫描④):PENDING 判死(事件活性闸三分)+ RUNNING 终态补判
// (FinishIfDone 委托)。
//
// 变异自查:
//   - ④a 丢事件闸 → pending_live_event_red 红(杀掉健康执行);
//   - ④b 委托失败上抛 → stale_running_finish 红。

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"braid/internal/engine"
)

// TestPendingExecFailsOnDeadEvent:PENDING + TTL 过 + 无活事件 →
// FAILED('reconciler_timeout')。
func TestPendingExecFailsOnDeadEvent(t *testing.T) {
	r, d := newReaper(t, testCfg())
	execID, _ := seedExecRow(t, d, "PENDING", time.Hour)
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if es := readExecRow(t, d, execID); es != engine.WorkflowFailed {
		t.Fatalf("exec = %s, want FAILED", es)
	}
	var code sql.NullString
	if err := d.QueryRow(`SELECT error_code FROM workflow_execution WHERE id=?`, execID).
		Scan(&code); err != nil {
		t.Fatalf("read code: %v", err)
	}
	if code.String != "reconciler_timeout" {
		t.Fatalf("code = %v, want reconciler_timeout", code.String)
	}
}

// TestPendingExecLiveEventDefers:事件 PENDING(退避重投中)或 DISPATCHED
// 新鲜 → skip。
func TestPendingExecLiveEventDefers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
	}{
		{"pending_event", "PENDING"},
		{"fresh_dispatched", "DISPATCHED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, d := newReaper(t, testCfg())
			execID, _ := seedExecRow(t, d, "PENDING", time.Hour)
			ins := func(q string, args ...any) {
				t.Helper()
				if _, err := d.Exec(q, args...); err != nil {
					t.Fatalf("seed event: %v", err)
				}
			}
			ins(`INSERT INTO outbox
			  (id, execution_id, event_type, task_id, queue, payload, status, available_at, dispatched_at)
			  VALUES (?, ?, 'execution.start', ?, 'reap-test', '{}', ?, NOW(3), NOW(3))`,
				nextID(t), execID, fmt.Sprintf("exec:%d", execID), tc.status)
			if err := r.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if es := readExecRow(t, d, execID); es != engine.WorkflowPending {
				t.Fatalf("exec = %s, want PENDING(活事件闸拦下判死)", es)
			}
		})
	}
}

// TestStaleRunningFinishIfDone:RUNNING + 静默 + 全节点 SUCCEEDED → 委托
// FinishIfDone 收口 SUCCEEDED。
func TestStaleRunningFinishIfDone(t *testing.T) {
	r, d := newReaper(t, testCfg())
	// 完整链走 scheduler.Instantiate 同构太重;直接种子:RUNNING 执行 +
	// 单 SUCCEEDED 节点 + last_activity 回拨。
	ids := mustTestIDs(t)
	defID, _ := ids.Next()
	execID, _ := ids.Next()
	nodeID, _ := ids.Next()
	name := fmt.Sprintf("fin-wf-%d", defID)
	dsl := fmt.Sprintf(`{"dsl":"1.0","name":%q,"description":"","inputs":[],"edges":[],`+
		`"nodes":[{"id":"n1","type":"llm","title":"n","description":"","prompt":{"template":"t"},"outputs":{}}],"outputs":{}}`, name)
	ins := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	ins(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', ?, ?, 'seed', 'seed', 'seed', NOW(3))`,
		defID, name, dsl, string(make([]byte, 64)))
	ins(`INSERT INTO workflow_execution
	  (id, definition_id, definition_name, definition_version, definition_hash, status, triggered_by, input, last_activity_at)
	  VALUES (?, ?, ?, 1, ?, 'RUNNING', 'reap-seed', '{}', NOW(3) - INTERVAL 1 HOUR)`,
		execID, defID, name, string(make([]byte, 64)))
	ins(`INSERT INTO node_execution
	  (id, execution_id, node_id, type, status, attempts, max_attempts, version, finished_at)
	  VALUES (?, ?, 'n1', 'llm', 'SUCCEEDED', 1, 1, 2, NOW(3))`, nodeID, execID)
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM outbox WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM node_execution WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM workflow_execution WHERE id=?`, execID)
		_, _ = d.Exec(`DELETE FROM workflow_definition WHERE id=?`, defID)
	})
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if es := readExecRow(t, d, execID); es != engine.WorkflowSucceeded {
		t.Fatalf("exec = %s, want SUCCEEDED(终态补判委托)", es)
	}
}
