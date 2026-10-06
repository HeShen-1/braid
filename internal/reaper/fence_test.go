package reaper

// fence_test.go = T20.1 fence 面(reaper 裁决权威)集成测试(t13 栈 MySQL;
// 栈不可达自动 skip)。口径真源 = D-2026-10-06-t201 决策 4:reaper 的合法
// 性 = 自身租约行加冕——失冠 reaper 的处置事务结构性回滚(fail-closed:
// 本轮 skip,下一 tick 重试),心跳陈旧 attempt 不被 zombie reaper 收口。

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
)

const reapFenceNode = 313

func TestReapFenceRejectsSupersededLease(t *testing.T) {
	d := sqltest.MustOpenAt(t, reaperSchema)
	if _, err := d.Exec(`DELETE FROM snowflake_node_lease`); err != nil {
		t.Fatalf("clear leases: %v", err)
	}
	ctx := context.Background()
	lease, err := snowflake.StartLease(ctx, d, snowflake.LeaseConfig{
		Kind:   snowflake.KindReaper,
		Spec:   snowflake.Explicit(reapFenceNode),
		Holder: "reap-fence",
	})
	if err != nil {
		t.Fatalf("StartLease: %v", err)
	}
	t.Cleanup(func() { _ = lease.Release(context.Background()) })
	s, err := scheduler.New(scheduler.Deps{DB: d, IDs: mustTestIDs(t), Queue: "reap-test"})
	if err != nil {
		t.Fatalf("scheduler New: %v", err)
	}
	r, err := New(Deps{DB: d, Sched: s, IDs: mustTestIDs(t), Lease: lease, Logger: testLogger(t)}, DefaultConfig())
	if err != nil {
		t.Fatalf("reaper New: %v", err)
	}

	seed := func(tag string) (uint64, uint64) {
		t.Helper()
		ids := mustTestIDs(t)
		defID, _ := ids.Next()
		execID, _ := ids.Next()
		nodeExecID, _ := ids.Next()
		attemptID, _ := ids.Next()
		name := fmt.Sprintf("fence-wf-%d", defID)
		ins := func(query string, args ...any) {
			t.Helper()
			if _, err := d.Exec(query, args...); err != nil {
				t.Fatalf("seed %s: %v", tag, err)
			}
		}
		dsl := fmt.Sprintf(`{"dsl":"1.0","name":%q,"inputs":[],"edges":[],"nodes":[{"id":"n","type":"llm","prompt":{"template":"t"},"outputs":{}}],"outputs":{}}`, name)
		ins(`INSERT INTO workflow_definition
		  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
		  VALUES (?, ?, 1, 'PUBLISHED', '1.0', ?, ?, 'seed', 'seed', 'seed', NOW(3))`,
			defID, name, dsl, string(make([]byte, 64)))
		ins(`INSERT INTO workflow_execution
		  (id, definition_id, definition_name, definition_version, definition_hash, triggered_by, input, status)
		  VALUES (?, ?, ?, 1, ?, 'seed', '{}', 'RUNNING')`,
			execID, defID, name, string(make([]byte, 64)))
		ins(`INSERT INTO node_execution
		  (id, execution_id, node_id, type, status, attempts, worker_id, version, deadline_at)
		  VALUES (?, ?, 'n', 'llm', 'EXECUTING', 1, 'dead-worker', 1, DATE_ADD(NOW(3), INTERVAL 300 SECOND))`,
			nodeExecID, execID)
		ins(`INSERT INTO node_attempt
		  (id, node_execution_id, execution_id, attempt_no, worker_id, status, idempotency_key, asynq_task_id, started_at, heartbeat_at)
		  VALUES (?,?,?,?,?,'RUNNING',?,?,NOW(3),DATE_SUB(NOW(3), INTERVAL 999 SECOND))`,
			attemptID, nodeExecID, execID, 1, "dead-worker",
			fmt.Sprintf("att:%d:1", nodeExecID), fmt.Sprintf("att:%d:1", nodeExecID))
		return nodeExecID, attemptID
	}
	attemptStatus := func(attemptID uint64) engine.AttemptState {
		t.Helper()
		var st engine.AttemptState
		if err := d.QueryRow(`SELECT status FROM node_attempt WHERE id=?`, attemptID).Scan(&st); err != nil {
			t.Fatalf("read attempt: %v", err)
		}
		return st
	}

	// seedApproval 落一套审批升级候选(WAITING_APPROVAL + deadline 远过 +
	// escalated_at NULL + on_timeout=escalate 快照)。
	seedApproval := func(tag string) (uint64, uint64) {
		t.Helper()
		ids := mustTestIDs(t)
		defID, _ := ids.Next()
		execID, _ := ids.Next()
		nodeExecID, _ := ids.Next()
		name := fmt.Sprintf("fence-appr-%d", defID)
		dsl := fmt.Sprintf(`{"dsl":"1.0","name":%q,"inputs":[],"edges":[],"nodes":[{"id":"n","type":"approval","approvers":["x"]}],"outputs":{}}`, name)
		if _, err := d.Exec(`INSERT INTO workflow_definition
		  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
		  VALUES (?, ?, 1, 'PUBLISHED', '1.0', ?, ?, 'seed', 'seed', 'seed', NOW(3))`,
			defID, name, dsl, string(make([]byte, 64))); err != nil {
			t.Fatalf("seed %s def: %v", tag, err)
		}
		if _, err := d.Exec(`INSERT INTO workflow_execution
		  (id, definition_id, definition_name, definition_version, definition_hash, status, triggered_by, input)
		  VALUES (?, ?, ?, 1, ?, 'RUNNING', 'seed', '{}')`,
			execID, defID, name, string(make([]byte, 64))); err != nil {
			t.Fatalf("seed %s exec: %v", tag, err)
		}
		if _, err := d.Exec(`INSERT INTO node_execution
		  (id, execution_id, node_id, type, status, attempts, worker_id, version, deadline_at, input_snapshot)
		  VALUES (?, ?, 'n', 'approval', 'WAITING_APPROVAL', 1, 'dead-worker', 2,
		          NOW(3) - INTERVAL 24 HOUR, ?)`,
			nodeExecID, execID,
			`{"approvers":["x"],"mode":"any","timeout_hours":24,"on_timeout":"escalate","instructions":""}`); err != nil {
			t.Fatalf("seed %s node: %v", tag, err)
		}
		return execID, nodeExecID
	}
	// seedPending 落一套 PENDING 执行候选(last_activity_at 远过)。
	seedPending := func(tag string) uint64 {
		t.Helper()
		ids := mustTestIDs(t)
		defID, _ := ids.Next()
		execID, _ := ids.Next()
		name := fmt.Sprintf("fence-pend-%d", defID)
		dsl := fmt.Sprintf(`{"dsl":"1.0","name":%q,"inputs":[],"edges":[],"nodes":[{"id":"n","type":"llm","prompt":{"template":"t"},"outputs":{}}],"outputs":{}}`, name)
		if _, err := d.Exec(`INSERT INTO workflow_definition
		  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
		  VALUES (?, ?, 1, 'PUBLISHED', '1.0', ?, ?, 'seed', 'seed', 'seed', NOW(3))`,
			defID, name, dsl, string(make([]byte, 64))); err != nil {
			t.Fatalf("seed %s def: %v", tag, err)
		}
		if _, err := d.Exec(`INSERT INTO workflow_execution
		  (id, definition_id, definition_name, definition_version, definition_hash, status, triggered_by, input, last_activity_at)
		  VALUES (?, ?, ?, 1, ?, 'PENDING', 'seed', '{}', NOW(3) - INTERVAL 48 HOUR)`,
			execID, defID, name, string(make([]byte, 64))); err != nil {
			t.Fatalf("seed %s exec: %v", tag, err)
		}
		return execID
	}

	// 冠内:三处置面全部收敛——心跳判死(DEAD)+ 审批升级(escalated_at
	// 落值)+ PENDING 终局化(FAILED)。
	node1, att1 := seed("crowned")
	apprExec1, apprNode1 := seedApproval("crowned")
	pendExec1 := seedPending("crowned")
	if err := r.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce (crowned): %v", err)
	}
	if st := attemptStatus(att1); st != engine.AttemptDead {
		t.Fatalf("crowned reap: attempt = %s, want DEAD", st)
	}
	_ = node1
	var esc sql.NullTime
	if err := d.QueryRow(`SELECT escalated_at FROM node_execution WHERE id=?`, apprNode1).Scan(&esc); err != nil || !esc.Valid {
		t.Fatalf("crowned approval escalate: escalated_at = (%v, %v), want set", esc, err)
	}
	var st1 engine.WorkflowState
	if err := d.QueryRow(`SELECT status FROM workflow_execution WHERE id=?`, pendExec1).Scan(&st1); err != nil || st1 != engine.WorkflowFailed {
		t.Fatalf("crowned pending-exec verdict: (%v, %v), want FAILED", st1, err)
	}
	_ = apprExec1

	// 易主(zombie reaper 形态:代际失配)。
	if _, err := d.Exec(
		`UPDATE snowflake_node_lease SET fencing_epoch = fencing_epoch + 1 WHERE node_id = ?`,
		reapFenceNode); err != nil {
		t.Fatalf("supersede lease: %v", err)
	}
	_, att2 := seed("superseded")
	apprExec2, apprNode2 := seedApproval("superseded")
	pendExec2 := seedPending("superseded")
	if err := r.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce (superseded): %v", err)
	}
	if st := attemptStatus(att2); st != engine.AttemptRunning {
		t.Fatalf("superseded reap: attempt = %s, want RUNNING(zombie reaper 不得收口)", st)
	}
	// 审批处置面:zombie reaper 不得升级(escalated_at 保持 NULL)。
	var esc2 sql.NullTime
	if err := d.QueryRow(`SELECT escalated_at FROM node_execution WHERE id=?`, apprNode2).Scan(&esc2); err != nil || esc2.Valid {
		t.Fatalf("superseded approval verdict: escalated_at = (%v, %v), want NULL(zombie reaper 不得 escalate)", esc2, err)
	}
	// PENDING 终局化面:zombie reaper 不得判 FAILED。
	var st2 engine.WorkflowState
	if err := d.QueryRow(`SELECT status FROM workflow_execution WHERE id=?`, pendExec2).Scan(&st2); err != nil || st2 != engine.WorkflowPending {
		t.Fatalf("superseded pending-exec verdict: (%v, %v), want PENDING(zombie reaper 不得终局化)", st2, err)
	}
	_ = apprExec2
}
