package scheduler

// T4.2 审批续跑收口集成测试(t13 真 MySQL,不可达自动 skip)。验收锚:
// 续跑 = 单事务(ResumeNode 复入新 attempt + 迁移审计行 + 执行级镜像 +
// 决策分岔收口:批准→SUCCEEDED+下游推进 / 驳回→FAILED+fail-fast)。
//
// 变异自查(实现错 → 哪条红):
//   - 执行级镜像漏做/顺序颠倒(WAITING→FAILED 直达) → TestResumeApprove*
//     /TestResumeRejectFailsExecution 的 wf 断言红(finishExecution 撞
//     非法边或 wf 卡 WAITING_APPROVAL);
//   - 复入与收口拆事务 → 快照/中间态可见性类断言红(attempts 与审计行
//     组合不再原子);
//   - 镜像先于收口的顺序被破坏 → TestResumeRejectFailsExecution 红
//     (wfEdges 无 WAITING_APPROVAL→FAILED 边,CAS 必败);
//   - 幂等收敛守卫漏做 → TestResumeDuplicateConverges 红(第二发投递
//     对已 SUCCEEDED 节点重放 CAS,Conflict 上抛);
//   - 终态守卫漏做 → TestResumeTerminalExecutionNoop 红(迟到续跑推进
//     已 FAILED 执行的节点);
//   - 决策复核漏做 → TestResumeDecisionMissingFails 变绿(伪事件静默
//     续跑);
//   - outputs 声明校验漏做 → TestResumeBadOutputDeclarationFails 变绿
//     (不可引用字段静默放行);
//   - 下游推进漏做 → TestResumeApproveAdvancesDownstream 的 c EXECUTING
//     + att: 事件断言红;
//   - 快照被复入改写 → TestResumeApproveAdvancesDownstream 的快照字节
//     断言红(复现真相源被污染)。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"braid/internal/audit"
	"braid/internal/engine"
)

// approvalTailDSL:approval 是尾节点 → 批准后 finishIfDone 收口 SUCCEEDED。
const approvalTailDSL = `{"dsl":"1.0","name":"approval-tail-t",
 "edges":[{"from":"a","to":"b"}],
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"gen"},"outputs":{"text":"string"}},
  {"id":"b","type":"approval","approvers":["carol"],"mode":"any","timeout_hours":24,
   "on_timeout":"fail","instructions":"请审批","outputs":{"decision":"string","comment":"string"}}],
 "outputs":{"verdict":"{{nodes.b.output.decision}}"}}`

// approvalBadOutputDSL:声明不可产出字段(数值类型)→ bad_approval_output 终败。
const approvalBadOutputDSL = `{"dsl":"1.0","name":"approval-badout-t",
 "edges":[{"from":"a","to":"b"}],
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"gen"},"outputs":{"text":"string"}},
  {"id":"b","type":"approval","approvers":["carol"],"timeout_hours":24,
   "outputs":{"score":"number"}}]}`

// seedDecision 直插决定行(resume 的事实前置;生产写入面 = api 决定事务)。
func seedDecision(t *testing.T, d *sql.DB, execID, nodeExecID uint64, nodeID, decidedBy, decision, comment string) uint64 {
	t.Helper()
	id := uint64(time.Now().UnixNano())&0x7FFFFFFFFFFF<<10 | uint64(nodeExecID&0x3FF)
	if _, err := d.Exec(`INSERT INTO approval_decision
	  (id, execution_id, node_execution_id, node_id, decided_by, decision, comment)
	VALUES (?,?,?,?,?,?,?)`,
		id, execID, nodeExecID, nodeID, decidedBy, decision, comment); err != nil {
		t.Fatalf("seed decision: %v", err)
	}
	return id
}

// countAuditEvents 数某执行某类型的审计行数(收敛/原子性断言面)。
func countAuditEvents(t *testing.T, d *sql.DB, execID uint64, eventType string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND event_type=?`,
		execID, eventType).Scan(&n); err != nil {
		t.Fatalf("count audit %s: %v", eventType, err)
	}
	return n
}

func mustResume(t *testing.T, s *Scheduler, execID, nodeExecID, decisionID uint64, attempt uint16) {
	t.Helper()
	if err := s.Resume(context.Background(), ResumeRequest{
		ExecID: execID, NodeExecID: nodeExecID, DecisionID: decisionID, AttemptNo: attempt, WorkerID: "resume-test",
	}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
}

// dispatchSuspended 前置链:dispatchApproval + 挂起,返回 (execID, b 行)。
func dispatchSuspended(t *testing.T, s *Scheduler, d *sql.DB, doc string) (uint64, nodeSnapshotRow) {
	t.Helper()
	execID, attemptB, b := dispatchApproval(t, s, d, doc)
	mustSuspend(t, s, execID, b.id, attemptB)
	return execID, readNode(t, d, execID, "b")
}

// TestResumeApproveAdvancesDownstream:批准 → 节点 SUCCEEDED(output 形状)+
// attempts+1 + 快照原值重写 + 双迁移审计行 + wf 回 RUNNING + 下游 c 派发
// (EXECUTING + att: 事件)。
func TestResumeApproveAdvancesDownstream(t *testing.T) {
	s, d := newSched(t)
	execID, b := dispatchSuspended(t, s, d, approvalDSL)
	snapBefore := b.snapshot.String
	decisionID := seedDecision(t, d, execID, b.id, "b", "carol", DecisionApproved, "可以")

	mustResume(t, s, execID, b.id, decisionID, b.attempts+1)

	got := readNode(t, d, execID, "b")
	if got.status != engine.NodeSucceeded {
		t.Fatalf("node b = %s, want SUCCEEDED", got.status)
	}
	if got.attempts != b.attempts+1 {
		t.Fatalf("attempts = %d, want %d(复入新 attempt)", got.attempts, b.attempts+1)
	}
	if !got.snapshot.Valid || got.snapshot.String != snapBefore {
		t.Fatalf("snapshot rewritten to %q, want 原值(前驱输出不可变 ⇒ 求值恒同)", got.snapshot.String)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(got.output.String), &out); err != nil {
		t.Fatalf("output json: %v", err)
	}
	if out["decision"] != "approved" || out["decided_by"] != "carol" || out["comment"] != "可以" {
		t.Fatalf("output = %v, want {decision:approved, decided_by:carol, comment:可以}", out)
	}
	// wf 回 RUNNING(c 在飞);下游 c 已派发 + att: 事件同事务落。
	waitExecStatus(t, d, execID, engine.WorkflowRunning)
	c := waitNodeStatus(t, d, execID, "c", engine.NodeExecuting)
	var attEvents int
	if err := d.QueryRow(`SELECT COUNT(*) FROM outbox WHERE execution_id=? AND task_id=?`,
		execID, fmt.Sprintf("att:%d:%d", c.id, c.attempts)).Scan(&attEvents); err != nil {
		t.Fatalf("count att events: %v", err)
	}
	if attEvents != 1 {
		t.Fatalf("att: events for c = %d, want 1(批准后下游推进)", attEvents)
	}
	// 双迁移审计行(精确边锚,对抗审计 MEDIUM 修:计数差值断言在 dispatch
	// 前置行面前恒真,零杀伤):WAITING_APPROVAL→EXECUTING 与
	// EXECUTING→SUCCEEDED 各恰 1 行。
	edgeCount := func(nodeID, from, to string) int {
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log
		 WHERE execution_id=? AND event_type=? AND JSON_EXTRACT(payload,'$.node_id')=?
		   AND JSON_EXTRACT(payload,'$.from')=? AND JSON_EXTRACT(payload,'$.to')=?`,
			execID, audit.EventNodeStateChanged, nodeID, from, to).Scan(&n); err != nil {
			t.Fatalf("count edge %s %s→%s: %v", nodeID, from, to, err)
		}
		return n
	}
	if n := edgeCount("b", "WAITING_APPROVAL", "EXECUTING"); n != 1 {
		t.Fatalf("b WAITING_APPROVAL→EXECUTING audit rows = %d, want 1", n)
	}
	if n := edgeCount("b", "EXECUTING", "SUCCEEDED"); n != 1 {
		t.Fatalf("b EXECUTING→SUCCEEDED audit rows = %d, want 1(收口审计行缺失=证据链断)", n)
	}
}

// TestResumeApproveLastNodeFinishes:批准尾节点 → finishIfDone 收口
// SUCCEEDED + 工作流 outputs 物化(decision 引用穿透)。
func TestResumeApproveLastNodeFinishes(t *testing.T) {
	s, d := newSched(t)
	execID, b := dispatchSuspended(t, s, d, approvalTailDSL)
	decisionID := seedDecision(t, d, execID, b.id, "b", "carol", DecisionApproved, "")

	mustResume(t, s, execID, b.id, decisionID, b.attempts+1)

	ex := waitExecStatus(t, d, execID, engine.WorkflowSucceeded)
	var outs map[string]any
	if err := json.Unmarshal([]byte(ex.outputs.String), &outs); err != nil {
		t.Fatalf("wf outputs json: %v", err)
	}
	if outs["verdict"] != "approved" {
		t.Fatalf("wf outputs = %v, want verdict=approved(引用穿透)", outs)
	}
}

// TestResumeRejectFailsExecution:驳回 → 节点 FAILED(approval_rejected)+
// wf FAILED(node_failed)+ 下游不推进(c 留 PENDING)+ fail-fast 语义。
func TestResumeRejectFailsExecution(t *testing.T) {
	s, d := newSched(t)
	execID, b := dispatchSuspended(t, s, d, approvalDSL)
	decisionID := seedDecision(t, d, execID, b.id, "b", "carol", DecisionRejected, "不行")

	mustResume(t, s, execID, b.id, decisionID, b.attempts+1)

	got := readNode(t, d, execID, "b")
	if got.status != engine.NodeFailed {
		t.Fatalf("node b = %s, want FAILED", got.status)
	}
	var code sql.NullString
	if err := d.QueryRow(`SELECT error_code FROM node_execution WHERE id=?`, b.id).Scan(&code); err != nil {
		t.Fatalf("read error_code: %v", err)
	}
	if !code.Valid || code.String != "approval_rejected" {
		t.Fatalf("error_code = %v, want approval_rejected", code)
	}
	ex := waitExecStatus(t, d, execID, engine.WorkflowFailed)
	if ex.errorCode.String != "node_failed" {
		t.Fatalf("wf error_code = %q, want node_failed(fail-fast 镜像)", ex.errorCode.String)
	}
	// 驳回方向收口审计行(对抗审计 MEDIUM 修:error_code 列锚之外,审计
	// 证据行的 EXECUTING→FAILED 边此前零锚——删 failApprovalNode 的审计
	// 调用不红)。
	var rejectEdges int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log
	 WHERE execution_id=? AND event_type=? AND JSON_EXTRACT(payload,'$.node_id')='b'
	   AND JSON_EXTRACT(payload,'$.from')='EXECUTING' AND JSON_EXTRACT(payload,'$.to')='FAILED'`,
		execID, audit.EventNodeStateChanged).Scan(&rejectEdges); err != nil {
		t.Fatalf("count reject edge: %v", err)
	}
	if rejectEdges != 1 {
		t.Fatalf("b EXECUTING→FAILED audit rows = %d, want 1(驳回收口审计行)", rejectEdges)
	}
	c := readNode(t, d, execID, "c")
	if c.status != engine.NodePending {
		t.Fatalf("node c = %s, want PENDING(fail-fast:驳回后零推进)", c.status)
	}
}

// TestResumeFanOutKeepsRunning:fan-out 挂起(wf 留 RUNNING 的降级表示)
// → 续跑只刷活跃时刻,wf 保持 RUNNING(c 仍在飞)。
func TestResumeFanOutKeepsRunning(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, approvalFanoutDSL, `{}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	attemptA := seedAttempt(t, d, execID, a.id, 1)
	mustComplete(t, s, execID, a.id, attemptA, engine.AttemptSucceed, json.RawMessage(`{"text":"v"}`))
	b := waitNodeStatus(t, d, execID, "b", engine.NodeExecuting)
	waitNodeStatus(t, d, execID, "c", engine.NodeExecuting)
	attemptB := seedAttempt(t, d, execID, b.id, 1)
	mustSuspend(t, s, execID, b.id, attemptB)

	decisionID := seedDecision(t, d, execID, b.id, "b", "erin", DecisionApproved, "")
	mustResume(t, s, execID, b.id, decisionID, b.attempts+1)

	// wf 全程 RUNNING(挂起时留 RUNNING 的 fan-out 降级表示,续跑不动它)。
	ex := waitExecStatus(t, d, execID, engine.WorkflowRunning)
	_ = ex
	got := readNode(t, d, execID, "b")
	if got.status != engine.NodeSucceeded {
		t.Fatalf("node b = %s, want SUCCEEDED", got.status)
	}
}

// TestResumeDualApprovalBothResume:双 approval 全挂起(wf WAITING_APPROVAL)
// → 两次续跑:第一次 ResumeExecution(WAITING→RUNNING),第二次走 touch
// 分支(wf 已 RUNNING);终态判定收敛 SUCCEEDED。
func TestResumeDualApprovalBothResume(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, approvalDualFanoutDSL, `{}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	attemptA := seedAttempt(t, d, execID, a.id, 1)
	mustComplete(t, s, execID, a.id, attemptA, engine.AttemptSucceed, json.RawMessage(`{"text":"v"}`))
	b := waitNodeStatus(t, d, execID, "b", engine.NodeExecuting)
	c := waitNodeStatus(t, d, execID, "c", engine.NodeExecuting)
	mustSuspend(t, s, execID, b.id, seedAttempt(t, d, execID, b.id, 1))
	mustSuspend(t, s, execID, c.id, seedAttempt(t, d, execID, c.id, 1))
	waitExecStatus(t, d, execID, engine.WorkflowWaitingApproval)

	decB := seedDecision(t, d, execID, b.id, "b", "gina", DecisionApproved, "")
	decC := seedDecision(t, d, execID, c.id, "c", "hank", DecisionApproved, "")
	mustResume(t, s, execID, b.id, decB, b.attempts+1)
	mustResume(t, s, execID, c.id, decC, c.attempts+1)

	waitExecStatus(t, d, execID, engine.WorkflowSucceeded)
}

// TestResumeTerminalExecutionNoop:终态执行(wf FAILED 于 fail-fast)内的
// 迟到续跑 → 零动作收敛(节点留 WAITING_APPROVAL,版本不变,无新审计)。
func TestResumeTerminalExecutionNoop(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, approvalFailFastDSL, `{}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	attemptA := seedAttempt(t, d, execID, a.id, 1)
	mustComplete(t, s, execID, a.id, attemptA, engine.AttemptSucceed, json.RawMessage(`{"text":"v"}`))
	b := waitNodeStatus(t, d, execID, "b", engine.NodeExecuting) // llm,将终败
	c := waitNodeStatus(t, d, execID, "c", engine.NodeExecuting) // approval
	mustSuspend(t, s, execID, c.id, seedAttempt(t, d, execID, c.id, 1))
	// b 终败(max_attempts=1)→ fail-fast,wf FAILED。
	mustComplete(t, s, execID, b.id, seedAttempt(t, d, execID, b.id, 1), engine.AttemptFailed, nil)
	waitExecStatus(t, d, execID, engine.WorkflowFailed)
	before := readNode(t, d, execID, "c")
	auditBefore := countAuditEvents(t, d, execID, audit.EventNodeStateChanged)

	decisionID := seedDecision(t, d, execID, c.id, "c", "frank", DecisionApproved, "")
	mustResume(t, s, execID, c.id, decisionID, c.attempts+1)

	after := readNode(t, d, execID, "c")
	if after.status != engine.NodeWaitingApproval || after.version != before.version {
		t.Fatalf("late resume mutated node: %s v%d, want WAITING_APPROVAL v%d",
			after.status, after.version, before.version)
	}
	if n := countAuditEvents(t, d, execID, audit.EventNodeStateChanged); n != auditBefore {
		t.Fatalf("late resume wrote audit rows: %d → %d", auditBefore, n)
	}
}

// TestResumeDuplicateConverges:at-least-once 第二发(节点已 SUCCEEDED)→
// 零动作收敛(nil 返回,无新写)。
func TestResumeDuplicateConverges(t *testing.T) {
	s, d := newSched(t)
	execID, b := dispatchSuspended(t, s, d, approvalTailDSL)
	decisionID := seedDecision(t, d, execID, b.id, "b", "carol", DecisionApproved, "")
	mustResume(t, s, execID, b.id, decisionID, b.attempts+1)
	waitExecStatus(t, d, execID, engine.WorkflowSucceeded)
	auditBefore := countAuditEvents(t, d, execID, audit.EventNodeStateChanged)

	mustResume(t, s, execID, b.id, decisionID, b.attempts+1) // 第二发:幂等收敛

	if n := countAuditEvents(t, d, execID, audit.EventNodeStateChanged); n != auditBefore {
		t.Fatalf("duplicate resume wrote audit rows: %d → %d", auditBefore, n)
	}
	if ex := readExec(t, d, execID); ex.status != engine.WorkflowSucceeded {
		t.Fatalf("wf = %s, want SUCCEEDED(重复投递不扰终态)", ex.status)
	}
}

// TestResumeRejectsNonApprovalNode:非 approval 节点的续跑请求 = 装配缺陷,
// 响亮拒绝(节点状态零变化)。
func TestResumeRejectsNonApprovalNode(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, approvalDSL, `{}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	attemptA := seedAttempt(t, d, execID, a.id, 1)
	mustComplete(t, s, execID, a.id, attemptA, engine.AttemptSucceed, json.RawMessage(`{"text":"v"}`))
	decisionID := seedDecision(t, d, execID, a.id, "a", "carol", DecisionApproved, "")

	err := s.Resume(context.Background(), ResumeRequest{
		ExecID: execID, NodeExecID: a.id, DecisionID: decisionID, AttemptNo: a.attempts + 1, WorkerID: "resume-test",
	})
	if err == nil {
		t.Fatal("resume on llm node: want error, got nil")
	}
	got := readNode(t, d, execID, "a")
	if got.status != engine.NodeSucceeded {
		t.Fatalf("node a = %s, want SUCCEEDED untouched", got.status)
	}
}

// TestResumeDecisionMissingFails:事件引用的决定行缺失(伪事件/漂移)→
// 响亮失败,节点零变化。
func TestResumeDecisionMissingFails(t *testing.T) {
	s, d := newSched(t)
	execID, b := dispatchSuspended(t, s, d, approvalDSL)
	before := readNode(t, d, execID, "b")

	err := s.Resume(context.Background(), ResumeRequest{
		ExecID: execID, NodeExecID: b.id, DecisionID: 9527, AttemptNo: b.attempts + 1, WorkerID: "resume-test",
	})
	if err == nil {
		t.Fatal("resume with missing decision: want error, got nil")
	}
	after := readNode(t, d, execID, "b")
	if after.status != before.status || after.version != before.version {
		t.Fatalf("fabricated resume mutated node: %s v%d, want %s v%d",
			after.status, after.version, before.status, before.version)
	}
}

// TestResumeStaleAttemptRejected:轮次守卫——事件声称的 attempt 号与锁行
// 读出的 attempts+1 不一致(滞留旧轮/伪造)→ 响亮拒绝,节点零变化
// (对抗审计 LOW 修:payload.AttemptNo 此前写而不校验,P1 回滚重挂后旧轮
// 决定可套用到新轮挂起)。
func TestResumeStaleAttemptRejected(t *testing.T) {
	s, d := newSched(t)
	execID, b := dispatchSuspended(t, s, d, approvalDSL)
	decisionID := seedDecision(t, d, execID, b.id, "b", "carol", DecisionApproved, "")
	before := readNode(t, d, execID, "b")

	err := s.Resume(context.Background(), ResumeRequest{
		ExecID: execID, NodeExecID: b.id, DecisionID: decisionID,
		AttemptNo: b.attempts + 2, WorkerID: "resume-test", // 旧轮/错位号
	})
	if err == nil {
		t.Fatal("stale attempt resume: want error, got nil")
	}
	after := readNode(t, d, execID, "b")
	if after.status != before.status || after.version != before.version {
		t.Fatalf("stale resume mutated node: %s v%d, want %s v%d",
			after.status, after.version, before.status, before.version)
	}
}

// TestResumeDecisionAnchorMismatch:决策锚定复核——决定行存在但锚定到别的
// 节点(事件与决定漂移)→ 响亮拒绝,节点零变化(对抗审计 LOW 修:锚定
// 复核分支此前零变异锚,删除守卫全绿)。
func TestResumeDecisionAnchorMismatch(t *testing.T) {
	s, d := newSched(t)
	execID, attemptB, b := dispatchApproval(t, s, d, approvalDSL)
	mustSuspend(t, s, execID, b.id, attemptB)
	b = readNode(t, d, execID, "b")
	a := readNode(t, d, execID, "a")
	// 决定锚到 a 行(合法行、错误锚)。
	decisionID := seedDecision(t, d, execID, a.id, "b", "carol", DecisionApproved, "")
	before := readNode(t, d, execID, "b")

	err := s.Resume(context.Background(), ResumeRequest{
		ExecID: execID, NodeExecID: b.id, DecisionID: decisionID,
		AttemptNo: b.attempts + 1, WorkerID: "resume-test",
	})
	if err == nil {
		t.Fatal("anchor-mismatched resume: want error, got nil")
	}
	after := readNode(t, d, execID, "b")
	if after.status != before.status || after.version != before.version {
		t.Fatalf("mismatched resume mutated node: %s v%d, want %s v%d",
			after.status, after.version, before.status, before.version)
	}
}

// TestResumeBadOutputDeclarationFails:outputs 声明不可满足(数值类型)→
// 节点 FAILED(bad_approval_output)+ wf FAILED(声明即契约,镜像
// bad_llm_output 语义)。
func TestResumeBadOutputDeclarationFails(t *testing.T) {
	s, d := newSched(t)
	execID, b := dispatchSuspended(t, s, d, approvalBadOutputDSL)
	decisionID := seedDecision(t, d, execID, b.id, "b", "carol", DecisionApproved, "")

	mustResume(t, s, execID, b.id, decisionID, b.attempts+1)

	got := readNode(t, d, execID, "b")
	if got.status != engine.NodeFailed {
		t.Fatalf("node b = %s, want FAILED", got.status)
	}
	var code sql.NullString
	if err := d.QueryRow(`SELECT error_code FROM node_execution WHERE id=?`, b.id).Scan(&code); err != nil {
		t.Fatalf("read error_code: %v", err)
	}
	if !code.Valid || code.String != "bad_approval_output" {
		t.Fatalf("error_code = %v, want bad_approval_output", code)
	}
	waitExecStatus(t, d, execID, engine.WorkflowFailed)
}
