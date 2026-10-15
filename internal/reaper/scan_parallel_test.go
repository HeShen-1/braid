package reaper

// T9.1(D-2026-09-28-t91)reaper 面回归(对抗审计 HIGH②):parallel 实例行
// 判死耗尽的节点级收口须委托 scheduler.ReapInstanceExhausted——
//   - fail_fast:任一实例判死耗尽 → 控制器 FAILED(parallel_instance_failed)
//     + 执行 FAILED,健康兄弟仍在飞(orphanQuiet 2min 常量保护,不误杀);
//   - wait_all:单实例判死 → 执行保持 RUNNING(收齐再判);末位实例也判死 →
//     收齐后控制器 FAILED + 执行 FAILED。
// 审计维度:实例行 state_changed 暂存行携 instance_no(载荷归因面)。
//
// 变异自查:reaper 侧耗尽分支不委托(直接 FailNode+FinishExecution)→
// wait_all 首段断言红(执行被误终败);委托面漏 FailNode 控制器 →
// fail_fast 控制器断言红。

import (
	"context"
	"database/sql"
	"fmt"

	"testing"
	"time"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/scheduler"
)

// seedParallelReap 种一个 parallel 判死现场:definition(parallel DSL,%
// s 注入 on_failure 槽)→ execution(RUNNING,items 2 元)→ 控制行 EXECUTING
// (deadline NULL)+ 2 实例行 EXECUTING(max 1);deadIdx 实例的 attempt 心跳
// 陈旧(判死),其余新鲜。
func seedParallelReap(t *testing.T, d *sql.DB, onFailureSlot string, deadIdx int) (execID, ctrlID uint64, instIDs []uint64) {
	t.Helper()
	defID := nextID(t)
	execID = nextID(t)
	ctrlID = nextID(t)
	name := fmt.Sprintf("reap-par-%d", defID)
	dsl := fmt.Sprintf(`{"dsl":"1.0","name":%q,"description":"","inputs":[{"name":"items","type":"array"}],`+
		`"edges":[],"nodes":[{"id":"fan","type":"parallel","title":"f","description":"","parallel":{`+
		`"foreach":"{{inputs.items}}",%s"body":{"type":"llm","prompt":{"template":"t"},"outputs":{"verdict":"string"}}}}],`+
		`"outputs":{}}`, name, onFailureSlot)
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
	  (id, definition_id, definition_name, definition_version, definition_hash, status, triggered_by, input)
	  VALUES (?, ?, ?, 1, ?, 'RUNNING', 'reap-seed', '{"items":["x","y"]}')`,
		execID, defID, name, string(make([]byte, 64)))
	// 控制行:EXECUTING、deadline NULL(无驻留)、无 attempt。
	ins(`INSERT INTO node_execution
	  (id, execution_id, node_id, instance_no, type, status, attempts, max_attempts, worker_id, version, deadline_at)
	  VALUES (?, ?, 'fan', 0, 'parallel', 'EXECUTING', 0, 1, 'seed-ctrl', 1, NULL)`, ctrlID, execID)
	instIDs = make([]uint64, 2)
	for i := 0; i < 2; i++ {
		instIDs[i] = nextID(t)
		stale := ""
		if i == deadIdx {
			stale = fmt.Sprintf("NOW(3) - INTERVAL %d MICROSECOND", (5 * time.Minute).Microseconds())
		} else {
			stale = "NOW(3)"
		}
		ins(`INSERT INTO node_execution
		  (id, execution_id, node_id, instance_no, type, status, attempts, max_attempts, worker_id, version, deadline_at)
		  VALUES (?, ?, 'fan', ?, 'llm', 'EXECUTING', 1, 1, 'seed-dispatch', 1, NOW(3) + INTERVAL 30 MINUTE)`,
			instIDs[i], execID, i+1)
		taskID := fmt.Sprintf("att:%d:1", instIDs[i])
		ins(fmt.Sprintf(`INSERT INTO node_attempt
		  (id, node_execution_id, execution_id, attempt_no, worker_id, status,
		   idempotency_key, asynq_task_id, started_at, heartbeat_at)
		  VALUES (?,?,?,?,?,'RUNNING',?,?,NOW(3),%s)`, stale),
			nextID(t), instIDs[i], execID, 1, "w-first", taskID, taskID)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM audit_log WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM outbox WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM audit_pending WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM chain_heads WHERE execution_id=?`, execID) // T9.3 链锚
		_, _ = d.Exec(`DELETE FROM node_attempt WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM node_execution WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM workflow_execution WHERE id=?`, execID)
		_, _ = d.Exec(`DELETE FROM workflow_definition WHERE id=?`, defID)
	})
	return execID, ctrlID, instIDs
}

func parallelRowState(t *testing.T, d *sql.DB, execID, rowID uint64) (engine.NodeState, string) {
	t.Helper()
	var st engine.NodeState
	var code sql.NullString
	if err := d.QueryRow(`SELECT status, error_code FROM node_execution WHERE id=?`, rowID).
		Scan(&st, &code); err != nil {
		t.Fatalf("read row %d: %v", rowID, err)
	}
	return st, code.String
}

func execStateOf(t *testing.T, d *sql.DB, execID uint64) engine.WorkflowState {
	t.Helper()
	var st engine.WorkflowState
	if err := d.QueryRow(`SELECT status FROM workflow_execution WHERE id=?`, execID).Scan(&st); err != nil {
		t.Fatalf("read execution: %v", err)
	}
	return st
}

func TestReapParallelInstanceFailFast(t *testing.T) {
	r, d := newComponentReaper(t)
	execID, ctrlID, instIDs := seedParallelReap(t, d, "", 1) // deadIdx=1(实例 2)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// 实例 2 判死耗尽 → fail_fast:控制器 FAILED(parallel_instance_failed)+
	// 执行 FAILED;健康实例 1 仍在飞(orphanQuiet 2min 保护)。
	if st, code := parallelRowState(t, d, execID, instIDs[1]); st != engine.NodeFailed || code != "attempts_exhausted" {
		t.Fatalf("dead instance = %s code %q, want FAILED/attempts_exhausted", st, code)
	}
	if st, code := parallelRowState(t, d, execID, ctrlID); st != engine.NodeFailed || code != "parallel_instance_failed" {
		t.Fatalf("controller = %s code %q, want FAILED/parallel_instance_failed (fail_fast 收敛控制器)", st, code)
	}
	if st := execStateOf(t, d, execID); st != engine.WorkflowFailed {
		t.Fatalf("execution = %s, want FAILED (fail_fast)", st)
	}
	if st, _ := parallelRowState(t, d, execID, instIDs[0]); st != engine.NodeExecuting {
		t.Fatalf("healthy sibling = %s, want EXECUTING (在飞兄弟自然跑完)", st)
	}
	// 审计维度:判死实例的 state_changed 暂存行携 instance_no=2。
	var ino int
	if err := d.QueryRow(`SELECT JSON_EXTRACT(payload,'$.instance_no') FROM audit_pending
	 WHERE execution_id=? AND event_type='node.state_changed' AND JSON_EXTRACT(payload,'$.instance_no')=2`,
		execID).Scan(&ino); err != nil {
		t.Fatalf("instance_no audit dim: %v", err)
	}
}

func TestReapParallelInstanceWaitAll(t *testing.T) {
	r, d := newComponentReaper(t)
	execID, ctrlID, instIDs := seedParallelReap(t, d, `"on_failure":"wait_all",`, 1)

	// 首轮:实例 2 判死耗尽,实例 1 健康 → 收齐再判:执行必须仍 RUNNING、
	// 控制器仍 EXECUTING(reaper 侧直接镜像 completeOnce default 会误终败)。
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st, code := parallelRowState(t, d, execID, instIDs[1]); st != engine.NodeFailed || code != "attempts_exhausted" {
		t.Fatalf("dead instance = %s code %q, want FAILED/attempts_exhausted", st, code)
	}
	if st := execStateOf(t, d, execID); st != engine.WorkflowRunning {
		t.Fatalf("execution = %s, want RUNNING (wait_all 未收齐)", st)
	}
	if st, _ := parallelRowState(t, d, execID, ctrlID); st != engine.NodeExecuting {
		t.Fatalf("controller = %s, want EXECUTING (wait_all)", st)
	}

	// 末位实例也判死 → 收齐:控制器 FAILED + 执行 FAILED。
	if _, err := d.Exec(`UPDATE node_attempt SET heartbeat_at = NOW(3) - INTERVAL 5 MINUTE
	 WHERE node_execution_id=? AND status='RUNNING'`, instIDs[0]); err != nil {
		t.Fatalf("age sibling heartbeat: %v", err)
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 2: %v", err)
	}
	if st, code := parallelRowState(t, d, execID, ctrlID); st != engine.NodeFailed || code != "parallel_instance_failed" {
		t.Fatalf("controller = %s code %q, want FAILED/parallel_instance_failed (wait_all 收齐)", st, code)
	}
	if st := execStateOf(t, d, execID); st != engine.WorkflowFailed {
		t.Fatalf("execution = %s, want FAILED (wait_all 收齐后)", st)
	}
}

// T23.2:扫描器及其委托 scheduler 共用真实 reaper 账号,admin 仅服务
// seedParallelReap/断言/清理,不能替耗尽收口补写 audit_pending。
func newComponentReaper(t *testing.T) (*Reaper, *sql.DB) {
	t.Helper()
	d := sqltest.MustOpenAt(t, reaperSchema)
	limited := sqltest.MustComponentDB(t, d, "reaper")
	sched, err := scheduler.New(scheduler.Deps{DB: limited, IDs: mustTestIDs(t), Queue: "reap-test"})
	if err != nil {
		t.Fatalf("component scheduler: %v", err)
	}
	r, err := New(Deps{DB: limited, Sched: sched, IDs: mustTestIDs(t), Logger: testLogger(t)}, testCfg())
	if err != nil {
		t.Fatalf("component reaper: %v", err)
	}
	return r, d
}
