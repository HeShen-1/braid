package reaper

// T4.3 审批超时扫描器(scan_approvals 作业)集成验收:策略两分支(escalate
// 恰好一次 / fail 显式终败)、resume 死信网最小版(已决票 + 超阈幂等重投,
// 活事件闸防抖)、孤儿收口(wf 终态 → CANCELLED,resume.go 终态守卫移交面)。
// 种子只造 WAITING_APPROVAL 现场,不触 scans ①-④ 的谓词面(EXECUTING/attempt
// RUNNING/PENDING 才可见),断言因此可归因到本扫描器。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"braid/internal/audit"
	"braid/internal/engine"
	"braid/internal/scheduler"
)

// approvalSeeded 是挂起现场种子链:definition → execution(状态可配)→
// node WAITING_APPROVAL(deadline 年龄可控,input_snapshot 携策略)。
type approvalSeeded struct {
	execID, nodeExecID uint64
}

func seedApproval(t *testing.T, d *sql.DB, wfStatus string, deadlinePast time.Duration,
	onTimeout string, escalated bool) approvalSeeded {
	t.Helper()
	ids := mustTestIDs(t)
	defID, _ := ids.Next()
	execID, _ := ids.Next()
	nodeRowID, _ := ids.Next()
	name := fmt.Sprintf("appr-wf-%d", defID)
	dsl := fmt.Sprintf(`{"dsl":"1.0","name":%q,"description":"","inputs":[],"edges":[],`+
		`"nodes":[{"id":"boss","type":"approval","title":"b","description":"",`+
		`"approvers":["alice"],"mode":"any"}],"outputs":{}}`, name)
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
	  VALUES (?, ?, ?, 1, ?, ?, 'reap-seed', '{}')`,
		execID, defID, name, string(make([]byte, 64)), wfStatus)
	deadline := "NOW(3) + INTERVAL 72 HOUR"
	if deadlinePast > 0 {
		deadline = fmt.Sprintf("NOW(3) - INTERVAL %d MICROSECOND", deadlinePast.Microseconds())
	}
	escalatedAt := "NULL"
	if escalated {
		escalatedAt = "NOW(3) - INTERVAL 1 MINUTE"
	}
	// on_timeout 空 = schema default 路径(escalate);快照形状 = scheduler.ApprovalInput。
	snap, err := json.Marshal(scheduler.ApprovalInput{
		Approvers: []string{"alice"}, Mode: "any", TimeoutHours: 72, OnTimeout: onTimeout,
	})
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	ins(fmt.Sprintf(`INSERT INTO node_execution
	  (id, execution_id, node_id, type, status, attempts, max_attempts, worker_id,
	   version, started_at, deadline_at, input_snapshot, escalated_at)
	  VALUES (?, ?, 'boss', 'approval', 'WAITING_APPROVAL', 1, 1, 'w-susp',
	   1, NOW(3), %s, ?, %s)`, deadline, escalatedAt),
		nodeRowID, execID, snap)
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM approval_decision WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM audit_log WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM audit_pending WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM chain_heads WHERE execution_id=?`, execID) // T9.3 链锚
		_, _ = d.Exec(`DELETE FROM outbox WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM node_execution WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM workflow_execution WHERE id=?`, execID)
		_, _ = d.Exec(`DELETE FROM workflow_definition WHERE id=?`, defID)
	})
	return approvalSeeded{execID: execID, nodeExecID: nodeRowID}
}

// seedDecision 落一行决定(或签首票形态,唯一行)。round = 行内 attempts
// (T9.2 决策 6:⑤a 死信网按轮锚定,种子行与生产写入面同源)。
func seedDecision(t *testing.T, d *sql.DB, s approvalSeeded, decision string) uint64 {
	t.Helper()
	id := nextID(t)
	var attempts uint16
	if err := d.QueryRow(`SELECT attempts FROM node_execution WHERE id=?`, s.nodeExecID).Scan(&attempts); err != nil {
		t.Fatalf("seed decision read attempts: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO approval_decision
	  (id, execution_id, node_execution_id, node_id, round, decided_by, decision, decided_at)
	  VALUES (?, ?, ?, 'boss', ?, 'alice', ?, NOW(3))`,
		id, s.execID, s.nodeExecID, int(attempts), decision); err != nil {
		t.Fatalf("seed decision: %v", err)
	}
	return id
}

// seedResumeEvent 落一行既有续跑事件(PENDING=活 / DISPATCHED 回拨=死窗)。
func seedResumeEvent(t *testing.T, d *sql.DB, s approvalSeeded, decisionID uint64,
	attemptNo int, status string, staleAgo time.Duration) {
	t.Helper()
	id := nextID(t)
	payload, err := json.Marshal(scheduler.ResumePayload{
		ExecutionID: s.execID, NodeExecutionID: s.nodeExecID,
		DecisionID: decisionID, AttemptNo: uint16(attemptNo),
	})
	if err != nil {
		t.Fatalf("marshal resume payload: %v", err)
	}
	taskID := fmt.Sprintf("resume:%d:boss:%d", s.execID, attemptNo)
	if status == "DISPATCHED" {
		if _, err := d.Exec(fmt.Sprintf(`INSERT INTO outbox
		  (id, execution_id, event_type, task_id, queue, payload, status, dispatched_at, available_at)
		  VALUES (?, ?, 'execution.resume', ?, 'reap-test', ?, 'DISPATCHED',
	          NOW(3) - INTERVAL %d MICROSECOND, NOW(3))`, staleAgo.Microseconds()),
			id, s.execID, taskID, payload); err != nil {
			t.Fatalf("seed resume event: %v", err)
		}
		return
	}
	if _, err := d.Exec(`INSERT INTO outbox
	  (id, execution_id, event_type, task_id, queue, payload, status, available_at)
	  VALUES (?, ?, 'execution.resume', ?, 'reap-test', ?, ?, NOW(3))`,
		id, s.execID, taskID, payload, status); err != nil {
		t.Fatalf("seed resume event: %v", err)
	}
}

// outboxRow 是断言用事件行投影。
type outboxRow struct {
	id      uint64
	taskID  string
	status  string
	payload []byte
}

func fetchOutbox(t *testing.T, d *sql.DB, execID uint64, eventType string) []outboxRow {
	t.Helper()
	rows, err := d.Query(`
SELECT id, task_id, status, payload FROM outbox
 WHERE execution_id=? AND event_type=? ORDER BY id`, execID, eventType)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var got []outboxRow
	for rows.Next() {
		var e outboxRow
		if err := rows.Scan(&e.id, &e.taskID, &e.status, &e.payload); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		got = append(got, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("outbox rows: %v", err)
	}
	return got
}

func readApprovalNode(t *testing.T, d *sql.DB, id uint64) (status engine.NodeState,
	errCode sql.NullString, escalatedAt sql.NullTime) {
	t.Helper()
	if err := d.QueryRow(`SELECT status, error_code, escalated_at FROM node_execution WHERE id=?`, id).
		Scan(&status, &errCode, &escalatedAt); err != nil {
		t.Fatalf("read node %d: %v", id, err)
	}
	return
}

// TestApprovalEscalateNotifiesOnce:escalate(默认)策略 = 升级通知恰好一次 +
// 保持等待(永不自动通过)。on_timeout 留空证明 schema default 生效路径。
func TestApprovalEscalateNotifiesOnce(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedApproval(t, d, "RUNNING", 5*time.Minute, "", false)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	ns, _, escalatedAt := readApprovalNode(t, d, s.nodeExecID)
	if ns != engine.NodeWaitingApproval {
		t.Fatalf("node = %s, want WAITING_APPROVAL(escalate 不动状态)", ns)
	}
	if !escalatedAt.Valid {
		t.Fatalf("escalated_at 应落定")
	}
	rows := fetchOutbox(t, d, s.execID, "approval.notify")
	if len(rows) != 1 {
		t.Fatalf("approval.notify 行数 = %d, want 1", len(rows))
	}
	if want := fmt.Sprintf("notify:%d:1", s.nodeExecID); rows[0].taskID != want {
		t.Fatalf("task_id = %q, want %q", rows[0].taskID, want)
	}
	var np scheduler.ApprovalNotifyPayload
	if err := json.Unmarshal(rows[0].payload, &np); err != nil {
		t.Fatalf("notify payload: %v", err)
	}
	if np.ExecutionID != s.execID || np.NodeExecutionID != s.nodeExecID || np.Reason != "escalated" {
		t.Fatalf("notify payload = %+v", np)
	}
	var ep audit.ApprovalEscalatedPayload
	if err := json.Unmarshal(reapAuditPayload(t, d, s.execID, audit.EventApprovalEscalated), &ep); err != nil {
		t.Fatalf("escalated audit payload: %v", err)
	}
	if ep.NodeID != "boss" {
		t.Fatalf("escalated audit node_id = %q, want boss", ep.NodeID)
	}
	// DeadlineAt 是 D-t43 §5 登记的判据证据,必须锚(对抗审计:断言缺位则
	// 字段被砍/置零照绿)。种子 deadline = NOW-5min,窗内即证。
	if ep.DeadlineAt.IsZero() || !ep.DeadlineAt.Before(time.Now().UTC()) ||
		ep.DeadlineAt.Before(time.Now().UTC().Add(-10*time.Minute)) {
		t.Fatalf("escalated audit deadline_at = %v, want ~5min 前的种子值", ep.DeadlineAt)
	}

	// 恰好一次:二轮零新增。权威锚 = MarkNodeEscalated 的 escalated_at IS NULL
	// 谓词:即使去掉 reaper 层分支,二轮 CAS 0 行 → 整事务回滚(含通知行),
	// 行数断言仍绿——reaper 分支是防已升级行每 tick 空转错误循环的短路,
	// correctness 锚在 DB 谓词(单元级锚 = TestMarkEscalated 二次调用
	// ErrConflict),不宣称本用例杀死该分支变异。
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 2: %v", err)
	}
	if rows = fetchOutbox(t, d, s.execID, "approval.notify"); len(rows) != 1 {
		t.Fatalf("二轮后 approval.notify 行数 = %d, want 1", len(rows))
	}
	if ns2, _, escalatedAt2 := readApprovalNode(t, d, s.nodeExecID); ns2 != engine.NodeWaitingApproval ||
		!escalatedAt2.Time.Equal(escalatedAt.Time) {
		t.Fatalf("二轮改动现场: status=%s escalated=%v→%v", ns2, escalatedAt.Time, escalatedAt2.Time)
	}
	if st := readExecRow(t, d, s.execID); st != engine.WorkflowRunning {
		t.Fatalf("wf = %s, want RUNNING(保持等待)", st)
	}
}

// TestApprovalFailPolicyFailsNode:fail 策略 + wf RUNNING(fan-out 降级表示)
// → 节点显式 FAILED(approval_timeout)+ 执行终局 FAILED。ROADMAP T4.3 验收面。
func TestApprovalFailPolicyFailsNode(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedApproval(t, d, "RUNNING", 5*time.Minute, "fail", false)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	ns, errCode, _ := readApprovalNode(t, d, s.nodeExecID)
	if ns != engine.NodeFailed || errCode.String != "approval_timeout" {
		t.Fatalf("node = %s/%v, want FAILED/approval_timeout", ns, errCode)
	}
	if st := readExecRow(t, d, s.execID); st != engine.WorkflowFailed {
		t.Fatalf("wf = %s, want FAILED", st)
	}
	var p audit.NodeStateChangedPayload
	if err := json.Unmarshal(reapAuditPayload(t, d, s.execID, audit.EventNodeStateChanged), &p); err != nil {
		t.Fatalf("state_changed payload: %v", err)
	}
	if p.From != string(engine.NodeWaitingApproval) || p.To != string(engine.NodeFailed) ||
		p.Code != "approval_timeout" || p.NodeID != "boss" {
		t.Fatalf("state_changed payload = %+v", p)
	}
	assertReapAudit(t, d, s.execID, []reapAuditRow{
		{eventType: audit.EventNodeStateChanged, actorType: audit.ActorSystem, actorID: "reaper:test-reaper"},
	})
}

// TestApprovalFailPolicyWaitingWf:fail 策略 + wf WAITING_APPROVAL(单活动路径)
// → 同样收敛 node FAILED + wf FAILED(wfEdges 无直达边,镜像先行;同事务内
// 中间态不可见,可观测面只有终值)。
func TestApprovalFailPolicyWaitingWf(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedApproval(t, d, "WAITING_APPROVAL", 5*time.Minute, "fail", false)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if ns, errCode, _ := readApprovalNode(t, d, s.nodeExecID); ns != engine.NodeFailed ||
		errCode.String != "approval_timeout" {
		t.Fatalf("node = %s/%v, want FAILED/approval_timeout", ns, errCode)
	}
	if st := readExecRow(t, d, s.execID); st != engine.WorkflowFailed {
		t.Fatalf("wf = %s, want FAILED", st)
	}
}

// TestApprovalDecidedRedispatchesResume:已决票 + 超阈 + 续跑事件死窗 → 幂等重投
// (同 TaskID resume:<exec>:<node>:<attempts+1>,payload 锚 DecisionID/AttemptNo),
// 节点保持 WAITING_APPROVAL(续跑权威在 worker),不发通知(decided 分支零 notify)。
func TestApprovalDecidedRedispatchesResume(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedApproval(t, d, "RUNNING", 5*time.Minute, "escalate", false)
	decID := seedDecision(t, d, s, "APPROVED")
	seedResumeEvent(t, d, s, decID, 2, "DISPATCHED", 5*time.Minute) // > DispatchStale(1m)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	rows := fetchOutbox(t, d, s.execID, "execution.resume")
	if len(rows) != 2 {
		t.Fatalf("resume 行数 = %d, want 2(原行 + 重投)", len(rows))
	}
	want := fmt.Sprintf("resume:%d:boss:2", s.execID)
	if rows[1].taskID != want {
		t.Fatalf("重投 task_id = %q, want %q(同 ID 幂等)", rows[1].taskID, want)
	}
	var rp scheduler.ResumePayload
	if err := json.Unmarshal(rows[1].payload, &rp); err != nil {
		t.Fatalf("resume payload: %v", err)
	}
	if rp.ExecutionID != s.execID || rp.NodeExecutionID != s.nodeExecID ||
		rp.DecisionID != decID || rp.AttemptNo != 2 {
		t.Fatalf("resume payload = %+v", rp)
	}
	var p audit.ApprovalResumeRedispatchedPayload
	if err := json.Unmarshal(reapAuditPayload(t, d, s.execID, audit.EventApprovalResumeRedispatched), &p); err != nil {
		t.Fatalf("redispatch audit payload: %v", err)
	}
	if p.NodeID != "boss" || p.DecisionID != decID || p.AttemptNo != 2 {
		t.Fatalf("redispatch audit payload = %+v", p)
	}
	if ns, _, escalatedAt := readApprovalNode(t, d, s.nodeExecID); ns != engine.NodeWaitingApproval ||
		escalatedAt.Valid {
		t.Fatalf("decided 分支不得升级/动状态: %s %v", ns, escalatedAt)
	}
	if got := fetchOutbox(t, d, s.execID, "approval.notify"); len(got) != 0 {
		t.Fatalf("decided 分支零 notify, got %d", len(got))
	}
}

// TestApprovalDecidedLiveEventDefers:活事件(PENDING)在场 → 本轮跳过,零重投。
func TestApprovalDecidedLiveEventDefers(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedApproval(t, d, "RUNNING", 5*time.Minute, "escalate", false)
	decID := seedDecision(t, d, s, "APPROVED")
	seedResumeEvent(t, d, s, decID, 2, "PENDING", 0)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rows := fetchOutbox(t, d, s.execID, "execution.resume"); len(rows) != 1 {
		t.Fatalf("resume 行数 = %d, want 1(活事件在场不重投)", len(rows))
	}
	if ns, _, escalatedAt := readApprovalNode(t, d, s.nodeExecID); ns != engine.NodeWaitingApproval ||
		escalatedAt.Valid {
		t.Fatalf("decided 分支不得动行: %s %v", ns, escalatedAt)
	}
}

// TestApprovalDecidedDeadEventRedispatchesWithTrace:死信网主场景(review
// HIGH-1 回归锚):原行 DEAD(投递穷尽)≠ 活证据——重投必须发生(计入
// live 则审批已决执行永卡 WAITING_APPROVAL);且 T6.2 trace 随行拷贝:
// 重投行 payload.trace_id 与列值都 == 原行 Z(段链不因死信网换段)。
func TestApprovalDecidedDeadEventRedispatchesWithTrace(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedApproval(t, d, "RUNNING", 5*time.Minute, "escalate", false)
	decID := seedDecision(t, d, s, "APPROVED")
	const z = "4bf92f3577b34da6a3ce929d0e0e4736"
	payload, err := json.Marshal(scheduler.ResumePayload{
		ExecutionID: s.execID, NodeExecutionID: s.nodeExecID,
		DecisionID: decID, AttemptNo: 2, TraceID: z,
	})
	if err != nil {
		t.Fatalf("marshal resume payload: %v", err)
	}
	taskID := fmt.Sprintf("resume:%d:boss:2", s.execID)
	if _, err := d.Exec(`INSERT INTO outbox
	  (id, execution_id, event_type, task_id, queue, payload, status, trace_id, available_at)
	  VALUES (?, ?, 'execution.resume', ?, 'reap-test', ?, 'DEAD', ?, NOW(3))`,
		nextID(t), s.execID, taskID, payload, z); err != nil {
		t.Fatalf("seed dead resume event: %v", err)
	}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	rows := fetchOutbox(t, d, s.execID, "execution.resume")
	if len(rows) != 2 {
		t.Fatalf("resume 行数 = %d, want 2(DEAD 是重投主场景,不是活证据)", len(rows))
	}
	var rp scheduler.ResumePayload
	if err := json.Unmarshal(rows[1].payload, &rp); err != nil {
		t.Fatalf("redispatch payload: %v", err)
	}
	if rp.TraceID != z {
		t.Fatalf("重投 payload trace_id = %q, want 原行 %q(段链不换)", rp.TraceID, z)
	}
	// D-t121 #3 反向臂:重投 payload 恒不携 span_id(键缺席)。
	var rawMap map[string]any
	if err := json.Unmarshal(rows[1].payload, &rawMap); err != nil {
		t.Fatalf("redispatch payload raw: %v", err)
	}
	if _, ok := rawMap["span_id"]; ok {
		t.Fatalf("redispatch payload carries span_id %q, want key absent (D-t121 #3)", rawMap["span_id"])
	}
	var colTrace sql.NullString
	if err := d.QueryRow(
		`SELECT trace_id FROM outbox WHERE task_id=? AND status='PENDING'`, taskID).
		Scan(&colTrace); err != nil {
		t.Fatalf("read redispatched row trace column: %v", err)
	}
	if colTrace.String != z {
		t.Fatalf("重投行 trace 列 = %v, want %q(列/payload 同值双写)", colTrace, z)
	}
}

// TestApprovalOrphanUnderTerminalWfCancelled:wf 已终态(fail-fast 遗留)→
// 平台裁决 CANCELLED(镜像扫描③),即使已决票也不重投(cancel 优先于
// resume 死信网——终态执行的续跑是永久 no-op)。deadline 未来证明孤儿
// 扫描独立于超时谓词。
func TestApprovalOrphanUnderTerminalWfCancelled(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedApproval(t, d, "FAILED", 0, "escalate", false)
	seedDecision(t, d, s, "APPROVED")
	if _, err := d.Exec(`UPDATE workflow_execution
	   SET last_activity_at = NOW(3) - INTERVAL 3 MINUTE WHERE id=?`, s.execID); err != nil {
		t.Fatalf("backdate activity: %v", err)
	}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if ns, _, escalatedAt := readApprovalNode(t, d, s.nodeExecID); ns != engine.NodeCancelled ||
		escalatedAt.Valid {
		t.Fatalf("node = %s %v, want CANCELLED/未升级", ns, escalatedAt)
	}
	if rows := fetchOutbox(t, d, s.execID, "execution.resume"); len(rows) != 0 {
		t.Fatalf("终态执行零重投, got %d", len(rows))
	}
	var p audit.NodeStateChangedPayload
	if err := json.Unmarshal(reapAuditPayload(t, d, s.execID, audit.EventNodeStateChanged), &p); err != nil {
		t.Fatalf("state_changed payload: %v", err)
	}
	if p.From != string(engine.NodeWaitingApproval) || p.To != string(engine.NodeCancelled) {
		t.Fatalf("state_changed payload = %+v", p)
	}
}

// TestApprovalNotOverdueUntouched:deadline 未来 → 全不动(超时谓词零误报)。
func TestApprovalNotOverdueUntouched(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedApproval(t, d, "RUNNING", 0, "fail", false)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if ns, errCode, escalatedAt := readApprovalNode(t, d, s.nodeExecID); ns != engine.NodeWaitingApproval ||
		errCode.Valid || escalatedAt.Valid {
		t.Fatalf("未到期被改动: %s %v %v", ns, errCode, escalatedAt)
	}
	if got := countEvents(t, d, s.execID); got != 0 {
		t.Fatalf("事件行 = %d, want 0", got)
	}
	if st := readExecRow(t, d, s.execID); st != engine.WorkflowRunning {
		t.Fatalf("wf = %s, want RUNNING", st)
	}
}

// TestApprovalAlreadyEscalatedStaysQuiet:已升级(escalated_at 已定)+ 未决票
// + 超阈 → 零通知、行不动(保持等待,永不自动通过)。seedApproval 的
// escalated 形参由此接线(对抗审计:死参数)。
func TestApprovalAlreadyEscalatedStaysQuiet(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedApproval(t, d, "RUNNING", 5*time.Minute, "escalate", true)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if ns, _, escalatedAt := readApprovalNode(t, d, s.nodeExecID); ns != engine.NodeWaitingApproval ||
		!escalatedAt.Valid {
		t.Fatalf("已升级行被改动: %s %v", ns, escalatedAt)
	}
	if rows := fetchOutbox(t, d, s.execID, "approval.notify"); len(rows) != 0 {
		t.Fatalf("已升级零通知, got %d", len(rows))
	}
}

// TestApprovalLiveGateExactTaskID:活闸精确四段匹配锚——异轮滞留事件
// (attemptNo=3 ≠ attempts+1=2,状态 PENDING)不挡本轮重投(变异:task_id
// 匹配放宽为 LIKE 前缀 → 本用例红)。
func TestApprovalLiveGateExactTaskID(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedApproval(t, d, "RUNNING", 5*time.Minute, "escalate", false)
	decID := seedDecision(t, d, s, "APPROVED")
	seedResumeEvent(t, d, s, decID, 3, "PENDING", 0)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	rows := fetchOutbox(t, d, s.execID, "execution.resume")
	if len(rows) != 2 {
		t.Fatalf("resume 行数 = %d, want 2(异轮活事件不挡同意图重投)", len(rows))
	}
	if want := fmt.Sprintf("resume:%d:boss:2", s.execID); rows[1].taskID != want {
		t.Fatalf("重投 task_id = %q, want %q", rows[1].taskID, want)
	}
}

// TestApprovalLiveGateFreshDispatchedDefers:活闸 DISPATCHED 新鲜臂——投递
// 后仍在 DispatchStale 窗内(10s < 1m)的事件算活,本轮零重投(变异:删
// DISPATCHED 臂 → 本用例红)。
func TestApprovalLiveGateFreshDispatchedDefers(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedApproval(t, d, "RUNNING", 5*time.Minute, "escalate", false)
	decID := seedDecision(t, d, s, "APPROVED")
	seedResumeEvent(t, d, s, decID, 2, "DISPATCHED", 10*time.Second)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rows := fetchOutbox(t, d, s.execID, "execution.resume"); len(rows) != 1 {
		t.Fatalf("resume 行数 = %d, want 1(新鲜 DISPATCHED 挡闸)", len(rows))
	}
}

// TestApprovalOrphanQuietWindowDefers:孤儿扫描静默窗负例——wf 已终态但
// last_activity 新鲜(刚终局化的在飞窗口)本轮不收口(变异:删 quiet 谓词
// → 本用例红)。deadline 未来使超时入口同时让位,归因唯一。
// (审计员另提议的 skew-margin 边界用例被拒:deadline 落在 (0, margin) 窗
// 内时,seed 与扫描间的毫秒级时延决定选中与否,必 flaky。)
func TestApprovalOrphanQuietWindowDefers(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedApproval(t, d, "FAILED", 0, "escalate", false) // 不回拨 activity

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if ns, _, escalatedAt := readApprovalNode(t, d, s.nodeExecID); ns != engine.NodeWaitingApproval ||
		escalatedAt.Valid {
		t.Fatalf("静默窗内孤儿被收口: %s %v", ns, escalatedAt)
	}
	if got := countEvents(t, d, s.execID); got != 0 {
		t.Fatalf("事件行 = %d, want 0", got)
	}
}

// TestApprovalSnapshotDriftNeverAutoPass:on_timeout 词表外值(快照漂移/
// 物化缺陷)→ 响亮报错(逐项隔离,RunOnce 整体仍 nil)但零写入、行原样
// 每轮重扫——漂移面的「永不自动通过」锚(变异:default 分支改按 escalate
// 或 fail 处理 → 行被改写 → 红)。
func TestApprovalSnapshotDriftNeverAutoPass(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedApproval(t, d, "RUNNING", 5*time.Minute, "explode", false)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v(逐项错误隔离,漂移行不阻断整轮)", err)
	}
	ns, errCode, escalatedAt := readApprovalNode(t, d, s.nodeExecID)
	if ns != engine.NodeWaitingApproval || errCode.Valid || escalatedAt.Valid {
		t.Fatalf("漂移行被改写: %s %v %v", ns, errCode, escalatedAt)
	}
	if got := countEvents(t, d, s.execID); got != 0 {
		t.Fatalf("事件行 = %d, want 0", got)
	}
	var auditRows int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE execution_id=?`, s.execID).
		Scan(&auditRows); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if auditRows != 0 {
		t.Fatalf("审计行 = %d, want 0", auditRows)
	}
}

// TestApprovalEscalateOnWaitingWf:wf=WAITING_APPROVAL(T4.1 静默规则的
// 典型拓扑)下的 escalate——节点与 wf 均保持 WAITING_APPROVAL,通知恰好
// 一条(coverage gap:escalate/decided 分支此前只测过 wf=RUNNING 降级形态)。
func TestApprovalEscalateOnWaitingWf(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedApproval(t, d, "WAITING_APPROVAL", 5*time.Minute, "escalate", false)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if ns, _, escalatedAt := readApprovalNode(t, d, s.nodeExecID); ns != engine.NodeWaitingApproval ||
		!escalatedAt.Valid {
		t.Fatalf("escalate 不动节点状态: %s %v", ns, escalatedAt)
	}
	if rows := fetchOutbox(t, d, s.execID, "approval.notify"); len(rows) != 1 {
		t.Fatalf("通知行 = %d, want 1", len(rows))
	}
	if st := readExecRow(t, d, s.execID); st != engine.WorkflowWaitingApproval {
		t.Fatalf("wf = %s, want WAITING_APPROVAL(升级不动执行级状态)", st)
	}
}

// TestApprovalDecidedOnWaitingWf:wf=WAITING_APPROVAL + 已决票 + 死窗 →
// 重投照发(同意图修复不依赖执行级形态),wf 保持 WAITING_APPROVAL——
// RUNNING 镜像归 worker 续跑事务,resume 事件是唯一权威通道。
func TestApprovalDecidedOnWaitingWf(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedApproval(t, d, "WAITING_APPROVAL", 5*time.Minute, "escalate", false)
	decID := seedDecision(t, d, s, "APPROVED")
	seedResumeEvent(t, d, s, decID, 2, "DISPATCHED", 5*time.Minute)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	rows := fetchOutbox(t, d, s.execID, "execution.resume")
	if len(rows) != 2 {
		t.Fatalf("resume 行数 = %d, want 2", len(rows))
	}
	if want := fmt.Sprintf("resume:%d:boss:2", s.execID); rows[1].taskID != want {
		t.Fatalf("重投 task_id = %q, want %q", rows[1].taskID, want)
	}
	if st := readExecRow(t, d, s.execID); st != engine.WorkflowWaitingApproval {
		t.Fatalf("wf = %s, want WAITING_APPROVAL(镜像归 worker)", st)
	}
}

// TestApprovalEscalatedDecidedRedispatched:已升级 + 已决票(REJECTED)+ 死窗
// → 死信网照投。双锚:①方向盲(D-t43 §5 决定方向不入判据——REJECTED 的
// 续跑由 worker 终败收口,重投仍是唯一通道;变异:redispatch 加
// APPROVED-only 守卫 → 红);②已升级行必须留在扫描⑤a 候选内(escalate→
// 后决定→事件死序是可达序;变异:候选谓词收紧为 escalated_at IS NULL 的
// 「便宜修法」→ 红)。排序语义:decided 分支先于 escalate 分支,升级与否
// 不影响已决票处置。
func TestApprovalEscalatedDecidedRedispatched(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedApproval(t, d, "RUNNING", 5*time.Minute, "escalate", true)
	decID := seedDecision(t, d, s, "REJECTED")
	seedResumeEvent(t, d, s, decID, 2, "DISPATCHED", 5*time.Minute)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	rows := fetchOutbox(t, d, s.execID, "execution.resume")
	if len(rows) != 2 {
		t.Fatalf("resume 行数 = %d, want 2(已升级+已决票仍在死信网内)", len(rows))
	}
	var rp scheduler.ResumePayload
	if err := json.Unmarshal(rows[1].payload, &rp); err != nil {
		t.Fatalf("resume payload: %v", err)
	}
	if rp.DecisionID != decID || rp.AttemptNo != 2 {
		t.Fatalf("resume payload = %+v, want decision %d attempt 2(REJECTED 方向盲)", rp, decID)
	}
	if ns, _, escalatedAt := readApprovalNode(t, d, s.nodeExecID); ns != engine.NodeWaitingApproval ||
		!escalatedAt.Valid {
		t.Fatalf("decided 分支不动行: %s %v", ns, escalatedAt)
	}
}

// TestApprovalEscalateNotifyRoundSegment:M5 锚(T9.2 决策 7)——升级通知
// TaskID 携挂起轮次:回滚重挂后(attempts=2)第二轮升级是新的通知意图,
// notify:<id>:<round> 与滞留旧副本零冲突。硬编码 round=1 的旧断言测不出
// 动态轮次(变异:轮次段回退单段 → 本测红)。
func TestApprovalEscalateNotifyRoundSegment(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedApproval(t, d, "RUNNING", 5*time.Minute, "", false)
	// 回滚重挂后现场:attempts=2(第二轮挂起)。
	if _, err := d.Exec(`UPDATE node_execution SET attempts=2 WHERE id=?`, s.nodeExecID); err != nil {
		t.Fatalf("seed round-2 suspension: %v", err)
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	rows := fetchOutbox(t, d, s.execID, "approval.notify")
	if len(rows) != 1 {
		t.Fatalf("approval.notify 行数 = %d, want 1", len(rows))
	}
	if want := fmt.Sprintf("notify:%d:2", s.nodeExecID); rows[0].taskID != want {
		t.Fatalf("task_id = %q, want %q(第二轮轮次段)", rows[0].taskID, want)
	}
}

// TestApprovalDeadLetterAnchorRoundScoped:H5 锚(对抗审计 HIGH 修)——⑤a
// 死信网决策锚按轮匹配:回滚重挂后旧轮决定行(round=1)对本轮(attempts=2)
// 不可见,不得触发续跑重投(旧票套新轮的实害路径封闭)。
func TestApprovalDeadLetterAnchorRoundScoped(t *testing.T) {
	r, d := newReaper(t, testCfg())
	// 已升级 + deadline 过期 + 旧轮已决票:候选谓词(本轮有票)不命中,
	// [5] round 锚亦不命中 → 无 redispatchResume。
	s := seedApproval(t, d, "RUNNING", 5*time.Minute, "", true)
	if _, err := d.Exec(`UPDATE node_execution SET attempts=2 WHERE id=?`, s.nodeExecID); err != nil {
		t.Fatalf("seed round-2 suspension: %v", err)
	}
	seedDecision(t, d, s, "APPROVED") // round = 行内 attempts = 2?种子读行内——改旧轮:
	if _, err := d.Exec(`UPDATE approval_decision SET round=1 WHERE node_execution_id=?`, s.nodeExecID); err != nil {
		t.Fatalf("age decision to round 1: %v", err)
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	rows := fetchOutbox(t, d, s.execID, "execution.resume")
	if len(rows) != 0 {
		t.Fatalf("execution.resume 行数 = %d, want 0(旧轮票不得套用新轮挂起)", len(rows))
	}
	ns, _, _ := readApprovalNode(t, d, s.nodeExecID)
	if ns != engine.NodeWaitingApproval {
		t.Fatalf("node = %s, want WAITING_APPROVAL(零动作)", ns)
	}
}
