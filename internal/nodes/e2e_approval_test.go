package nodes

// T4.1 端到端验收:approval 节点全链 = Instantiate 真调度器 → outbox →
// 真 relay → 真 asynq server → Mux → ApprovalExecutor(纯挂起决策)→
// worker.suspendNode → scheduler.Suspend。验收断言面(issue #27):
//   - 挂起后无阻塞中的 worker 任务:attempt SUCCEEDED 收口 + asynq 任务
//     ack(记录即删,队列无 pending/retry/archived 残留);
//   - 节点停 WAITING_APPROVAL、deadline=now+timeout_hours、下游不推进
//     (单节点链无下游;下游推进面归 scheduler 挂起测试);
//   - 输入快照落库且审批人可读(approvalSnapshotData 形状);
//   - 审计行记录挂起事件:node.state_changed(EXECUTING→WAITING_APPROVAL)+
//     approval.requested,attempt 锚定,同事务;
//   - 执行级镜像:wf RUNNING→WAITING_APPROVAL(单活动路径静默)。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"braid/internal/audit"
	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/redistest"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
	"braid/internal/worker"
)

// seedApprovalE2E 落单 approval 节点定义并物化执行(生产入口)。
func seedApprovalE2E(t *testing.T, d *sql.DB, sched *scheduler.Scheduler) (defID, execID uint64) {
	t.Helper()
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("seed snowflake: %v", err)
	}
	defID, err = ids.Next()
	if err != nil {
		t.Fatalf("gen def id: %v", err)
	}
	name := fmt.Sprintf("e2e-approval-%d", defID)
	dslDoc := fmt.Sprintf(`{"dsl":"1.0","name":%q,
 "nodes":[{"id":"boss","type":"approval","approvers":["carol"],"mode":"any",
  "timeout_hours":24,"on_timeout":"fail","instructions":"请审批","outputs":{}}],
 "edges":[],"outputs":{}}`, name)
	if _, err := d.Exec(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', ?, ?, 'e2e', 'e2e', 'e2e', NOW(3))`,
		defID, name, dslDoc, string(make([]byte, 64))); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	execID, err = sched.Instantiate(context.Background(), scheduler.InstantiateParams{
		DefinitionID: defID, Input: json.RawMessage(`{}`), TriggeredBy: "e2e", TriggerType: "TEST",
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	return defID, execID
}

func waitE2EExecWaitingApproval(t *testing.T, d *sql.DB, execID uint64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var status engine.WorkflowState
		if err := d.QueryRow(`SELECT status FROM workflow_execution WHERE id=?`, execID).
			Scan(&status); err == nil && status == engine.WorkflowWaitingApproval {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("execution %d not WAITING_APPROVAL within 30s", execID)
}

func TestE2EApprovalNodeSuspends(t *testing.T) {
	d := sqltest.MustOpenAt(t, nodesSchema)
	ctx := context.Background()
	queue := fmt.Sprintf("t_n%d", queueSeq+200)
	suspendStart := time.Now()

	// Mux 生产装配(approval 真执行器;llm/tool 用桩——单 approval 链不触)。
	executor := NewMux(mustE2ESched(t, d, queue), &fakeRunner{}, &fakeToolExec{}, ApprovalExecutor{})

	sched := mustE2ESched(t, d, queue)
	defID, execID := seedApprovalE2E(t, d, sched)
	t.Cleanup(func() { cleanupNodesE2E(t, d, execID, defID) })

	startE2ERelay(t, d, ctx)
	workerCfg := worker.DefaultConfig()
	workerCfg.HeartbeatInterval = 40 * time.Millisecond
	startE2EWorker(t, d, executor, workerCfg, queue)

	// 执行级镜像先到(挂起事务提交即见)。
	waitE2EExecWaitingApproval(t, d, execID)

	// 验收①a:节点停 WAITING_APPROVAL,attempt SUCCEEDED 收口。
	assertE2ENodeState(t, d, execID, "boss", engine.NodeWaitingApproval)
	var nodeExecID uint64
	if err := d.QueryRow(`SELECT id FROM node_execution WHERE execution_id=? AND node_id=?`,
		execID, "boss").Scan(&nodeExecID); err != nil {
		t.Fatalf("read node_execution id: %v", err)
	}
	var attemptID uint64
	var attemptStatus engine.AttemptState
	if err := d.QueryRow(`SELECT id, status FROM node_attempt WHERE node_execution_id=? AND attempt_no=1`,
		nodeExecID).Scan(&attemptID, &attemptStatus); err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	if attemptStatus != engine.AttemptSucceed {
		t.Fatalf("attempt = %s, want SUCCEEDED(挂起 = worker 任务成功收口)", attemptStatus)
	}

	// 验收②:输入快照落库且审批人可读(approvalSnapshotData 形状)。
	var snapshot []byte
	var deadline sql.NullTime
	if err := d.QueryRow(`SELECT input_snapshot, deadline_at FROM node_execution WHERE id=?`,
		nodeExecID).Scan(&snapshot, &deadline); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var snap map[string]any
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatalf("snapshot json: %v", err)
	}
	if fmt.Sprint(snap["approvers"]) != "[carol]" || snap["mode"] != "any" ||
		snap["timeout_hours"] != float64(24) || snap["on_timeout"] != "fail" || snap["instructions"] != "请审批" {
		t.Fatalf("approval snapshot = %v, want approver-facing shape", snap)
	}
	if !deadline.Valid {
		t.Fatal("deadline NULL after suspend")
	}
	if diff := deadline.Time.Sub(suspendStart.Add(24 * time.Hour)); diff < -5*time.Minute || diff > 5*time.Minute {
		t.Fatalf("deadline = %v, want ≈now+24h (diff %v)", deadline.Time, diff)
	}

	// 验收①b:无阻塞中的 worker 任务——asynq 记录 ack 即删(Retention=0;
	// 起点 exec: 与挂起 attempt 的 att: 皆然);挂起不是失败,archived 零死信。
	// 轮询窗口:att 任务可能与本断言短暂在飞;只在 ErrTaskNotFound 收敛,
	// 其余错误(在飞/瞬时)继续等——break 条件不得把基础设施错误当「已删」
	// (对抗审计 review 修:否则 Redis 抖动下验收断言空转假绿)。
	insp := asynq.NewInspector(redistest.MustOpt(t))
	t.Cleanup(func() { _ = insp.Close() })
	goneAt := time.Now().Add(15 * time.Second)
	for time.Now().Before(goneAt) {
		_, err := insp.GetTaskInfo(queue, fmt.Sprintf("att:%d:1", nodeExecID))
		if errors.Is(err, asynq.ErrTaskNotFound) {
			break // 处理完毕,记录已删
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := insp.GetTaskInfo(queue, fmt.Sprintf("att:%d:1", nodeExecID)); !errors.Is(err, asynq.ErrTaskNotFound) {
		t.Fatalf("att task record: %v, want ErrTaskNotFound (worker slot released)", err)
	}
	if _, err := insp.GetTaskInfo(queue, fmt.Sprintf("exec:%d", execID)); !errors.Is(err, asynq.ErrTaskNotFound) {
		t.Fatalf("exec:start record: %v, want ErrTaskNotFound", err)
	}
	if list, err := insp.ListArchivedTasks(queue); err != nil || len(list) != 0 {
		t.Fatalf("archived tasks = %d (err %v), want 0(挂起不留死信)", len(list), err)
	}

	flushStagedAudit(t, d) // T6.1 异步暂存排空
	// 验收③:审计行记录挂起事件(双行、attempt 锚、域事件 payload)。
	var gotStateChange, gotRequested int
	rows, err := d.Query(`
SELECT event_type FROM audit_log WHERE execution_id=? AND attempt_id=? ORDER BY id`, execID, attemptID)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var et string
		if err := rows.Scan(&et); err != nil {
			t.Fatalf("scan audit: %v", err)
		}
		switch et {
		case audit.EventNodeStateChanged:
			gotStateChange++
		case audit.EventApprovalRequested:
			gotRequested++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("audit rows: %v", err)
	}
	if gotStateChange != 1 || gotRequested != 1 {
		t.Fatalf("suspend audit rows: state_changed=%d approval.requested=%d, want 1/1", gotStateChange, gotRequested)
	}
}
