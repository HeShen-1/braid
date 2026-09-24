package scheduler

// T4.1 审批挂起收口集成测试(t13 真 MySQL,不可达自动 skip)。验收锚:
// 挂起 = 单事务(attempt SUCCEEDED 收口 + 节点 EXECUTING→WAITING_APPROVAL +
// 双审计行 + 静默检查→执行级镜像/活跃刷新),worker 不阻塞语义的调度半边。
//
// 变异自查(实现错 → 哪条红):
//   - attempt 收口与挂起拆成两事务 → 回滚类子测试(非 approval 拒绝/快照
//     缺失守卫)中 attempt 仍应 RUNNING,若已收口即红;
//   - 静默检查漏数在飞兄弟 → TestSuspendFanOutSiblingKeepsRunning 红
//     (执行被误转 WAITING_APPROVAL,制造 reaper 违约态);
//   - 静默检查永不转态 → TestSuspendApprovalNode 的执行断言红;
//   - quiesce 把 WAITING_APPROVAL 兄弟也计入活跃 → TestSuspendDualApproval
//     MirrorConverges 红(全挂起执行永卡 RUNNING);
//   - fan-out 分支忘刷 TouchExecution → TestSuspendFanOutSiblingKeepsRunning
//     的 last_activity_at 断言红(对抗审计修:变异此前存活);
//   - schema default 生效值未物化进快照 → TestSuspendDefaultsFallback 的
//     快照断言红(三处口径分叉);
//   - deadline 不用 DSL timeout_hours → deadline 断言红(24h ≠ 72h 回退,
//     双向钉死);
//   - schema default 未在挂起面补齐 → TestSuspendDefaultsFallback 红;
//   - 终态守卫仍挂起节点 → TestSuspendTerminalGuardClosesAttemptOnly 红;
//   - CloseAttempt Conflict 不走幂等哨兵 → TestSuspendIdempotentReplay 红;
//   - 非 approval 节点放行挂起 → TestSuspendRejectsNonApprovalNode 红;
//   - input_snapshot NULL 放行挂起 → TestSuspendMissingSnapshotGuarded 红
//     (破坏 engine 快照不变量 WAITING_APPROVAL ⇒ NOT NULL)。

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

// ---- 测试用 DSL 文档(形状同 testdata/valid/approval_full)----

// approvalDSL:显式全参(timeout_hours=24 → deadline 断言与 72h 回退双向可判)。
const approvalDSL = `{"dsl":"1.0","name":"approval-t",
 "edges":[{"from":"a","to":"b"},{"from":"b","to":"c"}],
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"gen"},"outputs":{"text":"string"}},
  {"id":"b","type":"approval","approvers":["carol"],"mode":"any","timeout_hours":24,
   "on_timeout":"fail","instructions":"请审批",
   "outputs":{"comment":"string"}},
  {"id":"c","type":"llm","prompt":{"template":"end","user_data":[{"name":"v","ref":"{{nodes.b.output.comment}}"}]},"outputs":{"text":"string"}}],
 "outputs":{"doc":"{{nodes.c.output.text}}"}}`

// approvalDefaultsDSL:缺省 mode/timeout_hours/on_timeout(schema default 不物化
// 进 struct,挂起面按生效值补齐的判别锚)。
const approvalDefaultsDSL = `{"dsl":"1.0","name":"approval-def-t",
 "edges":[{"from":"a","to":"b"},{"from":"b","to":"c"}],
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"gen"},"outputs":{"text":"string"}},
  {"id":"b","type":"approval","approvers":["dave"],"instructions":"请审批",
   "outputs":{"comment":"string"}},
  {"id":"c","type":"llm","prompt":{"template":"end","user_data":[{"name":"v","ref":"{{nodes.b.output.comment}}"}]},"outputs":{"text":"string"}}],
 "outputs":{"doc":"{{nodes.c.output.text}}"}}`

// approvalFanoutDSL:a 双后继(b approval + c llm 并发在飞)→ 静默检查的
// 「有 EXECUTING 兄弟 ⇒ 执行留 RUNNING」分支。
const approvalFanoutDSL = `{"dsl":"1.0","name":"approval-fanout-t",
 "edges":[{"from":"a","to":"b"},{"from":"a","to":"c"}],
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"gen"},"outputs":{"text":"string"}},
  {"id":"b","type":"approval","approvers":["erin"],"timeout_hours":24,"instructions":"请审批","outputs":{"comment":"string"}},
  {"id":"c","type":"llm","prompt":{"template":"c"},"outputs":{"text":"string"}}]}`

// approvalFailFastDSL:a 双后继(b llm 终败触发 fail-fast,c approval 迟到挂起)
// → 终态守卫分支。
const approvalFailFastDSL = `{"dsl":"1.0","name":"approval-failfast-t",
 "edges":[{"from":"a","to":"b"},{"from":"a","to":"c"}],
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"gen"},"outputs":{"text":"string"}},
  {"id":"b","type":"llm","prompt":{"template":"boom"},"outputs":{"text":"string"}},
  {"id":"c","type":"approval","approvers":["frank"],"timeout_hours":24,"instructions":"请审批","outputs":{"comment":"string"}}]}`

// approvalDualFanoutDSL:a 双后继(b/c 均 approval)→ 静默检查的镜像收敛分支:
// 第二个挂起时兄弟已 WAITING_APPROVAL(非 EXECUTING)⇒ 执行必须转
// WAITING_APPROVAL(「全部节点挂起 ⇒ 执行级镜像」不变量的另一半)。
const approvalDualFanoutDSL = `{"dsl":"1.0","name":"approval-dual-t",
 "edges":[{"from":"a","to":"b"},{"from":"a","to":"c"}],
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"gen"},"outputs":{"text":"string"}},
  {"id":"b","type":"approval","approvers":["gina"],"timeout_hours":24,"instructions":"请审批","outputs":{"comment":"string"}},
  {"id":"c","type":"approval","approvers":["hank"],"timeout_hours":24,"instructions":"请审批","outputs":{"comment":"string"}}]}`

// ---- 断言助手 ----

func readAttempt(t *testing.T, d *sql.DB, attemptID uint64) engine.AttemptState {
	t.Helper()
	var st engine.AttemptState
	if err := d.QueryRow(`SELECT status FROM node_attempt WHERE id=?`, attemptID).Scan(&st); err != nil {
		t.Fatalf("read attempt %d: %v", attemptID, err)
	}
	return st
}

func readDeadline(t *testing.T, d *sql.DB, execID uint64, nodeID string) time.Time {
	t.Helper()
	var dl sql.NullTime
	if err := d.QueryRow(`SELECT deadline_at FROM node_execution WHERE execution_id=? AND node_id=?`,
		execID, nodeID).Scan(&dl); err != nil {
		t.Fatalf("read deadline %s: %v", nodeID, err)
	}
	if !dl.Valid {
		t.Fatalf("deadline of %s = NULL, want set", nodeID)
	}
	return dl.Time
}

// assertDeadlineWindow 断言 deadline ∈ (want-5m, want+5m)(应用侧时钟与
// DB NOW(3) 的毫秒差由窗口吸收;72h↔24h 的实现错误远超窗口,判别力足够)。
func assertDeadlineWindow(t *testing.T, got, want time.Time) {
	t.Helper()
	if diff := got.Sub(want); diff < -5*time.Minute || diff > 5*time.Minute {
		t.Fatalf("deadline = %v, want ≈%v (diff %v)", got, want, diff)
	}
}

func suspend(t *testing.T, s *Scheduler, execID, nodeExecID, attemptID uint64) error {
	t.Helper()
	return s.Suspend(context.Background(), SuspendRequest{
		ExecID: execID, NodeExecID: nodeExecID, AttemptID: attemptID, WorkerID: "sched-test",
	})
}

func mustSuspend(t *testing.T, s *Scheduler, execID, nodeExecID, attemptID uint64) {
	t.Helper()
	if err := suspend(t, s, execID, nodeExecID, attemptID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
}

// dispatchApproval 前置链:Start → a 成功 → b(approval)派发为 EXECUTING,
// 返回 (execID, b 的 RUNNING attempt id, b 行快照)。
func dispatchApproval(t *testing.T, s *Scheduler, d *sql.DB, doc string) (execID, attemptID uint64, b nodeSnapshotRow) {
	t.Helper()
	_, execID = inst(t, s, d, doc, `{}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	attemptA := seedAttempt(t, d, execID, a.id, 1)
	mustComplete(t, s, execID, a.id, attemptA, engine.AttemptSucceed, json.RawMessage(`{"text":"v"}`))
	b = waitNodeStatus(t, d, execID, "b", engine.NodeExecuting)
	return execID, seedAttempt(t, d, execID, b.id, 1), b
}

// approvalRequestedPayload 读出 approval.requested 的 payload(域事件断言面)。
func approvalRequestedPayload(t *testing.T, d *sql.DB, execID uint64) audit.ApprovalRequestedPayload {
	t.Helper()
	var raw []byte
	if err := d.QueryRow(`SELECT payload FROM audit_log WHERE execution_id=? AND event_type=?`,
		execID, audit.EventApprovalRequested).Scan(&raw); err != nil {
		t.Fatalf("read approval.requested payload: %v", err)
	}
	var p audit.ApprovalRequestedPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal approval.requested: %v", err)
	}
	return p
}

// ---- 集成测试 ----

// TestSuspendApprovalNode:验收主路径——单事务挂起 + attempt 收口 + 执行级镜像 +
// 下游不推进 + 输入快照审批人可读 + 双审计行。
func TestSuspendApprovalNode(t *testing.T) {
	s, d := newSched(t)
	execID, attemptB, b := dispatchApproval(t, s, d, approvalDSL)
	suspendStart := time.Now()

	mustSuspend(t, s, execID, b.id, attemptB)

	// 节点:WAITING_APPROVAL、worker 归因保留、deadline=now+24h、快照原样。
	got := readNode(t, d, execID, "b")
	if got.status != engine.NodeWaitingApproval {
		t.Fatalf("node b = %s, want WAITING_APPROVAL", got.status)
	}
	if got.workerID.String != "sched-test" {
		t.Fatalf("node b worker = %q, want retained", got.workerID.String)
	}
	if got.version != b.version+1 {
		t.Fatalf("node b version = %d, want %d", got.version, b.version+1)
	}
	assertDeadlineWindow(t, readDeadline(t, d, execID, "b"), suspendStart.Add(24*time.Hour))
	if got.snapshot.String != b.snapshot.String {
		t.Fatalf("snapshot rewritten by suspend:\n got %s\nwant %s", got.snapshot.String, b.snapshot.String)
	}
	// 快照形状 = 审批人可读面(approvers/mode/timeout/on_timeout/instructions)。
	var snap map[string]any
	if err := json.Unmarshal([]byte(got.snapshot.String), &snap); err != nil {
		t.Fatalf("snapshot json: %v", err)
	}
	for _, k := range []string{"approvers", "mode", "timeout_hours", "on_timeout", "instructions"} {
		if snap[k] == nil {
			t.Fatalf("approval snapshot missing approver-facing field %q: %v", k, snap)
		}
	}

	// attempt:SUCCEEDED 收口(挂起 = worker 侧任务成功,无阻塞残留)。
	if st := readAttempt(t, d, attemptB); st != engine.AttemptSucceed {
		t.Fatalf("attempt = %s, want SUCCEEDED", st)
	}

	// 执行级镜像:静默(无其他 EXECUTING)→ RUNNING→WAITING_APPROVAL。
	if e := readExec(t, d, execID); e.status != engine.WorkflowWaitingApproval {
		t.Fatalf("execution = %s, want WAITING_APPROVAL(静默挂起镜像)", e.status)
	}

	// 下游不推进:c 留 PENDING,无 c 的 att 事件。
	if got := readNode(t, d, execID, "c").status; got != engine.NodePending {
		t.Fatalf("node c = %s, want PENDING(挂起不推进下游)", got)
	}
	cRow := readNode(t, d, execID, "c")
	for _, e := range taskEvents(t, d, execID) {
		if e == fmt.Sprintf("att:%d:1", cRow.id) {
			t.Fatalf("outbox 含 c 的派发事件(挂起后下游被错误推进): %v", taskEvents(t, d, execID))
		}
	}

	// 审计:双行同事务(迁移行 + 审批域事件行),attempt 锚定。
	// 先锚同步半边(T6.1 分阶段口径,对抗审计 test-gaps:低频/回读事件回退
	// 暂存全绿):approval.requested 不经 flush 直接在场。
	var syncRows int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND event_type=?`,
		execID, audit.EventApprovalRequested).Scan(&syncRows); err != nil {
		t.Fatalf("count sync rows: %v", err)
	}
	if syncRows != 1 {
		t.Fatalf("approval.requested pre-flush rows = %d, want 1(同步直写半边)", syncRows)
	}
	assertAuditSequence(t, d, execID, []auditEvent{
		wantEvent(audit.EventNodeStateChanged, "a", 0),
		wantEvent(audit.EventNodeStateChanged, "a", attemptAOf(t, d, execID)),
		wantEvent(audit.EventNodeStateChanged, "b", 0),
		wantEvent(audit.EventNodeStateChanged, "b", attemptB),
		wantEvent(audit.EventApprovalRequested, "b", attemptB),
	})
	p := approvalRequestedPayload(t, d, execID)
	if len(p.Approvers) != 1 || p.Approvers[0] != "carol" || p.Mode != "any" ||
		p.TimeoutHours != 24 || p.OnTimeout != "fail" {
		t.Fatalf("approval.requested payload = %+v", p)
	}
	assertDeadlineWindow(t, p.DeadlineAt, suspendStart.Add(24*time.Hour))

	// 迁移行 payload 断言(审计行 = 状态机证据链,From/To/Version 与状态表
	// 口径必须一致;变异锚:copy-paste 镜像 completeOnce 时 To 写成
	// SUCCEEDED 的残留,仅靠 eventType/actor 投影断言抓不住——对抗审计
	// test-gaps 维度变异实证存活,review 修)。
	var scpRaw []byte
	if err := d.QueryRow(`
SELECT payload FROM audit_log
 WHERE execution_id=? AND event_type=? AND node_id=? AND attempt_id=?
 ORDER BY id DESC LIMIT 1`,
		execID, audit.EventNodeStateChanged, "b", attemptB).Scan(&scpRaw); err != nil {
		t.Fatalf("read suspend state_changed payload: %v", err)
	}
	var scp audit.NodeStateChangedPayload
	if err := json.Unmarshal(scpRaw, &scp); err != nil {
		t.Fatalf("unmarshal state_changed: %v", err)
	}
	if scp.From != string(engine.NodeExecuting) || scp.To != string(engine.NodeWaitingApproval) ||
		scp.Version != b.version+1 || scp.AttemptNo != b.attempts || scp.WorkerID != "sched-test" {
		t.Fatalf("suspend state_changed payload = %+v, want EXECUTING→WAITING_APPROVAL"+
			"/v%d/attempt%d/sched-test", scp, b.version+1, b.attempts)
	}
}

// attemptAOf 读回 a 的收口 attempt id(审计行序断言需要;经 idempotency_key 反查)。
func attemptAOf(t *testing.T, d *sql.DB, execID uint64) uint64 {
	t.Helper()
	aRow := readNode(t, d, execID, "a")
	var id uint64
	if err := d.QueryRow(`SELECT id FROM node_attempt WHERE node_execution_id=? AND attempt_no=1`, aRow.id).
		Scan(&id); err != nil {
		t.Fatalf("read a attempt: %v", err)
	}
	return id
}

// TestSuspendDefaultsFallback:DSL 缺省字段(schema default 不物化)→ 生效值
// 补齐三面同源:快照(审批人可读面)、timeout=72h、mode=any、on_timeout=
// escalate(对抗审计 MEDIUM 修:快照零值与 deadline/payload 分叉已收口)。
func TestSuspendDefaultsFallback(t *testing.T) {
	s, d := newSched(t)
	execID, attemptB, b := dispatchApproval(t, s, d, approvalDefaultsDSL)
	suspendStart := time.Now()

	mustSuspend(t, s, execID, b.id, attemptB)

	assertDeadlineWindow(t, readDeadline(t, d, execID, "b"), suspendStart.Add(72*time.Hour))
	p := approvalRequestedPayload(t, d, execID)
	if p.Mode != "any" || p.OnTimeout != "escalate" || p.TimeoutHours != 72 {
		t.Fatalf("defaults payload = %+v, want any/escalate/72", p)
	}
	// 快照面同样携带生效值(claim 时由 buildSnapshot 物化,Suspend 不改写)。
	var snap map[string]any
	if err := json.Unmarshal([]byte(b.snapshot.String), &snap); err != nil {
		t.Fatalf("defaults snapshot json: %v", err)
	}
	if snap["mode"] != "any" || snap["on_timeout"] != "escalate" || snap["timeout_hours"] != float64(72) {
		t.Fatalf("defaults snapshot = %v, want any/escalate/72(三处口径一致)", snap)
	}
	if e := readExec(t, d, execID); e.status != engine.WorkflowWaitingApproval {
		t.Fatalf("execution = %s, want WAITING_APPROVAL", e.status)
	}
}

// TestSuspendFanOutSiblingKeepsRunning:合法 fan-out DSL 下,b 挂起时 c 仍在飞
// → 执行留 RUNNING(静默检查);c 随后终态,执行仍不误转/不误判终态。
func TestSuspendFanOutSiblingKeepsRunning(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, approvalFanoutDSL, `{}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	attemptA := seedAttempt(t, d, execID, a.id, 1)
	mustComplete(t, s, execID, a.id, attemptA, engine.AttemptSucceed, json.RawMessage(`{"text":"v"}`))
	b := waitNodeStatus(t, d, execID, "b", engine.NodeExecuting)
	waitNodeStatus(t, d, execID, "c", engine.NodeExecuting) // 兄弟在飞

	// fan-out 分支的活跃刷新契约(对抗审计修:变异「TouchExecution → nil」
	// 此前全绿):挂起推进了节点/attempt,同事务必须刷 last_activity_at。
	var activityBefore time.Time
	if err := d.QueryRow(`SELECT last_activity_at FROM workflow_execution WHERE id=?`, execID).
		Scan(&activityBefore); err != nil {
		t.Fatalf("read last_activity_at: %v", err)
	}
	time.Sleep(5 * time.Millisecond) // DATETIME(3) 毫秒精度,防同毫秒假平

	attemptB := seedAttempt(t, d, execID, b.id, 1)
	mustSuspend(t, s, execID, b.id, attemptB)

	if got := readNode(t, d, execID, "b").status; got != engine.NodeWaitingApproval {
		t.Fatalf("node b = %s, want WAITING_APPROVAL", got)
	}
	if e := readExec(t, d, execID); e.status != engine.WorkflowRunning {
		t.Fatalf("execution = %s, want RUNNING(兄弟在飞不得转 WAITING_APPROVAL,"+
			"否则制造 reaper 孤儿违约态)", e.status)
	}
	var activityAfter time.Time
	if err := d.QueryRow(`SELECT last_activity_at FROM workflow_execution WHERE id=?`, execID).
		Scan(&activityAfter); err != nil {
		t.Fatalf("read last_activity_at after: %v", err)
	}
	if !activityAfter.After(activityBefore) {
		t.Fatalf("last_activity_at = %v, want advanced past %v(fan-out 分支仍刷活跃,"+
			"否则 reaper 对健康执行误报)", activityAfter, activityBefore)
	}

	// c 终态后:执行仍 RUNNING(b WAITING_APPROVAL 非终态;P0 降级表示,
	// reaper 终态补判对它恒 no-op,代价仅索引扫描噪声)。
	c := readNode(t, d, execID, "c")
	attemptC := seedAttempt(t, d, execID, c.id, 1)
	mustComplete(t, s, execID, c.id, attemptC, engine.AttemptSucceed, json.RawMessage(`{"text":"v"}`))
	if e := readExec(t, d, execID); e.status != engine.WorkflowRunning {
		t.Fatalf("execution = %s after sibling done, want RUNNING(降级表示不误终态)", e.status)
	}
}

// TestSuspendDualApprovalMirrorConverges:双 approval fan-out——b 挂起(c 在飞,
// wf 留 RUNNING)后 c 挂起:兄弟仅剩 WAITING_APPROVAL(非 EXECUTING)⇒ 执行
// 镜像收敛转 WAITING_APPROVAL。变异锚:quiesce 把 WAITING_APPROVAL 也计入活跃
// ⇒ 全挂起执行永卡 RUNNING(对抗审计 MEDIUM 修:该变异此前全套件存活)。
func TestSuspendDualApprovalMirrorConverges(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, approvalDualFanoutDSL, `{}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	attemptA := seedAttempt(t, d, execID, a.id, 1)
	mustComplete(t, s, execID, a.id, attemptA, engine.AttemptSucceed, json.RawMessage(`{"text":"v"}`))
	b := waitNodeStatus(t, d, execID, "b", engine.NodeExecuting)
	waitNodeStatus(t, d, execID, "c", engine.NodeExecuting)

	// 第一挂起:c 仍 EXECUTING ⇒ wf 留 RUNNING。
	attemptB := seedAttempt(t, d, execID, b.id, 1)
	mustSuspend(t, s, execID, b.id, attemptB)
	if e := readExec(t, d, execID); e.status != engine.WorkflowRunning {
		t.Fatalf("execution = %s after first suspend, want RUNNING(c 在飞)", e.status)
	}

	// 第二挂起:兄弟仅剩 WAITING_APPROVAL ⇒ 镜像收敛,转 WAITING_APPROVAL。
	c := readNode(t, d, execID, "c")
	attemptC := seedAttempt(t, d, execID, c.id, 1)
	mustSuspend(t, s, execID, c.id, attemptC)
	if got := readNode(t, d, execID, "c").status; got != engine.NodeWaitingApproval {
		t.Fatalf("node c = %s, want WAITING_APPROVAL", got)
	}
	if e := readExec(t, d, execID); e.status != engine.WorkflowWaitingApproval {
		t.Fatalf("execution = %s after both suspended, want WAITING_APPROVAL"+
			"(全部非终态节点已挂起 ⇒ 镜像不变量)", e.status)
	}
}

// TestSuspendTerminalGuardClosesAttemptOnly:fail-fast 后的迟到挂起——attempt
// 收口 SUCCEEDED,节点留 EXECUTING(归 T2.5 扫描),不落挂起审计/不转执行态。
func TestSuspendTerminalGuardClosesAttemptOnly(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, approvalFailFastDSL, `{}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	attemptA := seedAttempt(t, d, execID, a.id, 1)
	mustComplete(t, s, execID, a.id, attemptA, engine.AttemptSucceed, json.RawMessage(`{"text":"v"}`))
	b := waitNodeStatus(t, d, execID, "b", engine.NodeExecuting)
	waitNodeStatus(t, d, execID, "c", engine.NodeExecuting)

	// b 终败(max_attempts=1)→ fail-fast:执行 FAILED。
	attemptB := seedAttempt(t, d, execID, b.id, 1)
	mustComplete(t, s, execID, b.id, attemptB, engine.AttemptFailed, nil)
	waitExecStatus(t, d, execID, engine.WorkflowFailed)

	c := readNode(t, d, execID, "c")
	attemptC := seedAttempt(t, d, execID, c.id, 1)
	mustSuspend(t, s, execID, c.id, attemptC)

	if st := readAttempt(t, d, attemptC); st != engine.AttemptSucceed {
		t.Fatalf("attempt = %s, want SUCCEEDED(迟到收口仍执行)", st)
	}
	if got := readNode(t, d, execID, "c").status; got != engine.NodeExecuting {
		t.Fatalf("node c = %s, want EXECUTING(终态守卫:不再挂起,归 T2.5)", got)
	}
	if e := readExec(t, d, execID); e.status != engine.WorkflowFailed {
		t.Fatalf("execution = %s, want FAILED(迟到挂起不改执行态)", e.status)
	}
	for _, e := range auditEvents(t, d, execID) {
		if e.eventType == audit.EventApprovalRequested {
			t.Fatalf("终态执行内落 approval.requested(守卫失效): %+v", e)
		}
	}
}

// TestSuspendIdempotentReplay:同请求重放 → CloseAttempt 撞已收口 → 幂等 nil,
// 无二次迁移/审计增长。
func TestSuspendIdempotentReplay(t *testing.T) {
	s, d := newSched(t)
	execID, attemptB, b := dispatchApproval(t, s, d, approvalDSL)
	mustSuspend(t, s, execID, b.id, attemptB)
	before := readNode(t, d, execID, "b")
	auditBefore := len(auditEvents(t, d, execID))

	if err := suspend(t, s, execID, b.id, attemptB); err != nil {
		t.Fatalf("replay Suspend 应幂等 nil,got %v", err)
	}
	after := readNode(t, d, execID, "b")
	if after.version != before.version || after.status != before.status {
		t.Fatalf("重放引发二次迁移: %+v → %+v", before, after)
	}
	if got := len(auditEvents(t, d, execID)); got != auditBefore {
		t.Fatalf("audit rows = %d, want %d(重放不得落新行)", got, auditBefore)
	}
}

// TestSuspendRejectsNonApprovalNode:非 approval 节点/参数缺陷响亮拒绝,
// 全事务回滚(attempt 仍 RUNNING、节点仍 EXECUTING)。
func TestSuspendRejectsNonApprovalNode(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, approvalDSL, `{}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting) // llm 节点
	attemptA := seedAttempt(t, d, execID, a.id, 1)

	if err := suspend(t, s, execID, a.id, attemptA); err == nil {
		t.Fatal("Suspend(llm 节点) 应拒绝")
	}
	if st := readAttempt(t, d, attemptA); st != engine.AttemptRunning {
		t.Fatalf("attempt = %s, want RUNNING(拒绝路径全回滚)", st)
	}
	if got := readNode(t, d, execID, "a").status; got != engine.NodeExecuting {
		t.Fatalf("node a = %s, want EXECUTING(拒绝路径全回滚)", got)
	}

	// 参数校验(零 id/空 worker;进程内拒绝,不触 DB)。
	zeroCases := []struct {
		name string
		req  SuspendRequest
	}{
		{"zero exec id", SuspendRequest{NodeExecID: 1, AttemptID: 1, WorkerID: "w"}},
		{"zero node exec id", SuspendRequest{ExecID: 1, AttemptID: 1, WorkerID: "w"}},
		{"zero attempt id", SuspendRequest{ExecID: 1, NodeExecID: 1, WorkerID: "w"}},
		{"empty worker", SuspendRequest{ExecID: 1, NodeExecID: 1, AttemptID: 1}},
	}
	for _, tc := range zeroCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.Suspend(context.Background(), tc.req); err == nil {
				t.Fatal("应拒绝(参数校验)")
			}
		})
	}
}

// TestSuspendMissingSnapshotGuarded:EXECUTING approval 行快照缺失(数据缺陷,
// 生产路径不可达——ClaimNode 必写)→ 响亮拒绝,不制造
// WAITING_APPROVAL ⇒ NULL 快照的不变量破坏。
func TestSuspendMissingSnapshotGuarded(t *testing.T) {
	s, d := newSched(t)
	execID, attemptB, b := dispatchApproval(t, s, d, approvalDSL)
	if _, err := d.Exec(`UPDATE node_execution SET input_snapshot=NULL WHERE id=?`, b.id); err != nil {
		t.Fatalf("seed null snapshot: %v", err)
	}

	if err := suspend(t, s, execID, b.id, attemptB); err == nil {
		t.Fatal("Suspend(快照缺失) 应拒绝(engine 快照不变量守卫)")
	}
	if got := readNode(t, d, execID, "b").status; got != engine.NodeExecuting {
		t.Fatalf("node b = %s, want EXECUTING(守卫拒绝全回滚)", got)
	}
	if st := readAttempt(t, d, attemptB); st != engine.AttemptRunning {
		t.Fatalf("attempt = %s, want RUNNING(守卫拒绝全回滚)", st)
	}
}
