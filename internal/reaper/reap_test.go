package reaper

// reapAttempt 分派树表驱动(设计 K 节用例 2–7):判死×重派/耗竭/终态裁决/
// 活性保护/deadline 路径(含 ExtendNodeDeadline)/分支 B/orphan 三形状。
// 时钟控制 = 直写陈旧列(heartbeat_at/deadline_at 回拨),谓词全 DB 端。
//
// 变异自查(实现错 → 哪条红):
//   - 活性保护丢「无条件前置」 → heartbeat_fresh 红;
//   - 分支 B 缺活事件闸 → branch_b_live_event 红(双派发);
//   - 重派不写 outbox/新 attempt 号 → redispatch_within_budget 红;
//   - 耗尽路径不镜像 fail-fast → exhausted_fails_execution 红;
//   - Extend 判据丢 started_at ≥ deadline → late_start_killed 红(误杀)。

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"braid/internal/engine"
)

func readNodeRow(t *testing.T, d *sql.DB, id uint64) (status engine.NodeState, attempts uint16, worker string) {
	t.Helper()
	var w sql.NullString
	if err := d.QueryRow(`SELECT status, attempts, worker_id FROM node_execution WHERE id=?`, id).
		Scan(&status, &attempts, &w); err != nil {
		t.Fatalf("read node %d: %v", id, err)
	}
	return status, attempts, w.String
}

func readAttemptRow(t *testing.T, d *sql.DB, id uint64) (status engine.AttemptState, code sql.NullString, finished sql.NullTime) {
	t.Helper()
	if err := d.QueryRow(`SELECT status, error_code, finished_at FROM node_attempt WHERE id=?`, id).
		Scan(&status, &code, &finished); err != nil {
		t.Fatalf("read attempt %d: %v", id, err)
	}
	return
}

func readExecRow(t *testing.T, d *sql.DB, id uint64) engine.WorkflowState {
	t.Helper()
	var s engine.WorkflowState
	if err := d.QueryRow(`SELECT status FROM workflow_execution WHERE id=?`, id).Scan(&s); err != nil {
		t.Fatalf("read exec %d: %v", id, err)
	}
	return s
}

func countEvents(t *testing.T, d *sql.DB, execID uint64) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM outbox WHERE execution_id=?`, execID).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// TestReapHeartbeatStaleRedispatch:心跳陈旧 + 预算内 → DEAD(heartbeat_lost)
// + Redispatch(attempts+1、worker_id=reaper:*)+ outbox att:N+1(新 TaskID)。
func TestReapHeartbeatStaleRedispatch(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedReap(t, d, time.Minute, 0, 3) // 心跳陈旧 1min,deadline 未过期
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	st, code, finished := readAttemptRow(t, d, s.attID)
	if st != engine.AttemptDead || code.String != "heartbeat_lost" {
		t.Fatalf("attempt = %s/%v, want DEAD/heartbeat_lost", st, code.String)
	}
	if finished.Valid {
		t.Fatalf("finished_at=%v 被写入(DEAD 恒 NULL)", finished)
	}
	ns, na, nw := readNodeRow(t, d, s.nodeExecID)
	if ns != engine.NodeExecuting || na != 2 || nw != "reaper:test-reaper" {
		t.Fatalf("node = %s/attempts=%d/worker=%s, want EXECUTING/2/reaper:test-reaper", ns, na, nw)
	}
	var taskID string
	if err := d.QueryRow(`SELECT task_id FROM outbox WHERE execution_id=? AND status='PENDING'`, s.execID).
		Scan(&taskID); err != nil {
		t.Fatalf("redispatch event missing: %v", err)
	}
	if want := fmt.Sprintf("att:%d:2", s.nodeExecID); taskID != want {
		t.Fatalf("event task_id=%s, want %s(新 attempt_no ⇒ 新 TaskID)", taskID, want)
	}
}

// TestReapExhaustedFailsExecution:attempts=max → FailNode + wf FAILED
// (fail-fast 同事务镜像,零新形状)。
func TestReapExhaustedFailsExecution(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedReap(t, d, time.Minute, 0, 1) // attempts=1 已是 max
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st, _, _ := readAttemptRow(t, d, s.attID); st != engine.AttemptDead {
		t.Fatalf("attempt = %s, want DEAD", st)
	}
	if ns, _, _ := readNodeRow(t, d, s.nodeExecID); ns != engine.NodeFailed {
		t.Fatalf("node = %s, want FAILED", ns)
	}
	if es := readExecRow(t, d, s.execID); es != engine.WorkflowFailed {
		t.Fatalf("exec = %s, want FAILED", es)
	}
	var code sql.NullString
	if err := d.QueryRow(`SELECT error_code FROM workflow_execution WHERE id=?`, s.execID).Scan(&code); err != nil {
		t.Fatalf("read exec code: %v", err)
	}
	if code.String != "node_failed" {
		t.Fatalf("exec error_code=%q, want node_failed", code.String)
	}
}

// TestReapTerminalExecutionCancelsNode:wf 已终态(靶 a 形状2)→ DEAD +
// node CANCELLED(平台裁决,不伪造业务失败)。
func TestReapTerminalExecutionCancelsNode(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedReap(t, d, time.Minute, 0, 3)
	if _, err := d.Exec(`UPDATE workflow_execution SET status='FAILED' WHERE id=?`, s.execID); err != nil {
		t.Fatalf("terminalize exec: %v", err)
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st, _, _ := readAttemptRow(t, d, s.attID); st != engine.AttemptDead {
		t.Fatalf("attempt = %s, want DEAD", st)
	}
	if ns, _, _ := readNodeRow(t, d, s.nodeExecID); ns != engine.NodeCancelled {
		t.Fatalf("node = %s, want CANCELLED", ns)
	}
	if n := countEvents(t, d, s.execID); n != 0 {
		t.Fatalf("outbox events = %d, want 0(终态执行不重派)", n)
	}
}

// TestReapHeartbeatFreshSkipped:心跳新鲜(无条件活性保护,任何入口)→
// 节点/attempt/outbox 零改写。
func TestReapHeartbeatFreshSkipped(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedReap(t, d, 10*time.Millisecond, 0, 3) // 心跳 10ms 前,新鲜
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st, _, _ := readAttemptRow(t, d, s.attID); st != engine.AttemptRunning {
		t.Fatalf("attempt = %s, want RUNNING(活 worker 绝不被抢写)", st)
	}
	if ns, na, _ := readNodeRow(t, d, s.nodeExecID); ns != engine.NodeExecuting || na != 1 {
		t.Fatalf("node = %s/attempts=%d, want EXECUTING/1", ns, na)
	}
	if n := countEvents(t, d, s.execID); n != 0 {
		t.Fatalf("outbox events = %d, want 0", n)
	}
}

// TestReapDeadlineExceeded:心跳陈旧且 deadline 亦过期 → 收口码
// deadline_exceeded(分码可辨)。
func TestReapDeadlineExceeded(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedReap(t, d, time.Minute, time.Minute, 3) // 双过期
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	_, code, _ := readAttemptRow(t, d, s.attID)
	if code.String != "deadline_exceeded" {
		t.Fatalf("code = %q, want deadline_exceeded", code.String)
	}
	if _, na, _ := readNodeRow(t, d, s.nodeExecID); na != 2 {
		t.Fatalf("attempts = %d, want 2(预算内重派)", na)
	}
}

// TestReapLateStartExtended:deadline 过期但心跳新鲜且 started_at ≥
// deadline_at(迟到启动)→ ExtendNodeDeadline 一次性重挂,不烧预算。
func TestReapLateStartExtended(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedReap(t, d, 10*time.Millisecond, 2*time.Minute, 3)
	// 迟到现场:started_at 也要 ≥ deadline_at(deadline 在过去,attempt 在
	// deadline 之后才启动)——seedReap 的 started_at=NOW,天然满足。
	var dl sql.NullTime
	if err := d.QueryRow(`SELECT deadline_at FROM node_execution WHERE id=?`, s.nodeExecID).Scan(&dl); err != nil || !dl.Valid {
		t.Fatalf("read deadline: %v", err)
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st, _, _ := readAttemptRow(t, d, s.attID); st != engine.AttemptRunning {
		t.Fatalf("attempt = %s, want RUNNING(未被误判死)", st)
	}
	if ns, na, _ := readNodeRow(t, d, s.nodeExecID); ns != engine.NodeExecuting || na != 1 {
		t.Fatalf("node = %s/attempts=%d, want EXECUTING/1(重挂不烧预算)", ns, na)
	}
	var dl2 sql.NullTime
	if err := d.QueryRow(`SELECT deadline_at FROM node_execution WHERE id=?`, s.nodeExecID).Scan(&dl2); err != nil || !dl2.Valid {
		t.Fatalf("read deadline2: %v", err)
	}
	if !dl2.Time.After(time.Now()) {
		t.Fatalf("deadline=%v 未被重挂(仍在过去)", dl2.Time)
	}
}

// TestReapEarlyStartKilledOnDeadline:deadline 过期 + 心跳陈旧 + started_at
// < deadline_at(窗口内启动但挂死)→ DEAD(SLA 强制,不重挂)。
func TestReapEarlyStartKilledOnDeadline(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedReap(t, d, time.Minute, time.Minute, 3)
	// started_at 回拨到 deadline 之前(窗口内启动)。
	if _, err := d.Exec(`UPDATE node_attempt SET started_at = NOW(3) - INTERVAL 2 MINUTE WHERE id=?`, s.attID); err != nil {
		t.Fatalf("backdate started_at: %v", err)
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st, code, _ := readAttemptRow(t, d, s.attID); st != engine.AttemptDead || code.String != "deadline_exceeded" {
		t.Fatalf("attempt = %s/%v, want DEAD/deadline_exceeded", st, code.String)
	}
}

// TestBranchBRedispatch:EXECUTING + 无 RUNNING attempt + 无活事件(分支 B,
// 攻击 6/10 的 CRITICAL 收敛)→ 预算内重派(新 attempt_no 新 TaskID)。
func TestBranchBRedispatch(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedReap(t, d, time.Minute, time.Minute, 3)
	// 造分支 B 前置态:attempt 已终态(DEAD)+ 派发事件行 DEAD。
	if _, err := d.Exec(`UPDATE node_attempt SET status='DEAD', error_code='heartbeat_lost' WHERE id=?`, s.attID); err != nil {
		t.Fatalf("dead attempt: %v", err)
	}
	if err := engine.WithTx(context.Background(), d, func(q engine.DBTX) error {
		_, err := q.ExecContext(context.Background(), `
INSERT INTO outbox (id, execution_id, event_type, task_id, queue, payload, status, available_at, deliver_attempts)
VALUES (?, ?, 'node.task_enqueue', ?, 'reap-test', '{}', 'DEAD', NOW(3), 6)`,
			nextID(t), s.execID, fmt.Sprintf("att:%d:1", s.nodeExecID))
		return err
	}); err != nil {
		t.Fatalf("seed dead event: %v", err)
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, na, _ := readNodeRow(t, d, s.nodeExecID); na != 2 {
		t.Fatalf("attempts = %d, want 2(分支 B 重派)", na)
	}
	var taskID string
	if err := d.QueryRow(`SELECT task_id FROM outbox WHERE execution_id=? AND status='PENDING'`, s.execID).
		Scan(&taskID); err != nil {
		t.Fatalf("redispatch event missing: %v", err)
	}
	if want := fmt.Sprintf("att:%d:2", s.nodeExecID); taskID != want {
		t.Fatalf("event task_id=%s, want %s", taskID, want)
	}
}

// TestBranchBLiveEventDefers:同前置态但存在 PENDING 活事件 → skip
// (防双派发;warn 可观测)。
func TestBranchBLiveEventDefers(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedReap(t, d, time.Minute, time.Minute, 3)
	if _, err := d.Exec(`UPDATE node_attempt SET status='DEAD', error_code='heartbeat_lost' WHERE id=?`, s.attID); err != nil {
		t.Fatalf("dead attempt: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO outbox
	  (id, execution_id, event_type, task_id, queue, payload, status, available_at)
	  VALUES (?, ?, 'node.task_enqueue', ?, 'reap-test', '{}', 'PENDING', NOW(3))`,
		nextID(t), s.execID, fmt.Sprintf("att:%d:1", s.nodeExecID)); err != nil {
		t.Fatalf("seed pending event: %v", err)
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, na, _ := readNodeRow(t, d, s.nodeExecID); na != 1 {
		t.Fatalf("attempts = %d, want 1(活事件闸拦下重派)", na)
	}
}

// TestBranchBExhausted:同前置态且 attempts=max → FailNode + wf FAILED。
func TestBranchBExhausted(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedReap(t, d, time.Minute, time.Minute, 1)
	if _, err := d.Exec(`UPDATE node_attempt SET status='DEAD', error_code='heartbeat_lost' WHERE id=?`, s.attID); err != nil {
		t.Fatalf("dead attempt: %v", err)
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if ns, _, _ := readNodeRow(t, d, s.nodeExecID); ns != engine.NodeFailed {
		t.Fatalf("node = %s, want FAILED", ns)
	}
	if es := readExecRow(t, d, s.execID); es != engine.WorkflowFailed {
		t.Fatalf("exec = %s, want FAILED", es)
	}
}

// TestOrphanTerminalAttemptCancelled:形状3(wf 终态 + attempt 已终态,
// termGuard 下迟到失败的遗留)→ 仅 CancelNode,无 attempt 改写。
func TestOrphanTerminalAttemptCancelled(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedReap(t, d, time.Minute, 0, 3)
	if _, err := d.Exec(`UPDATE workflow_execution SET status='FAILED' WHERE id=?`, s.execID); err != nil {
		t.Fatalf("terminalize: %v", err)
	}
	if _, err := d.Exec(`UPDATE node_attempt SET status='SUCCEEDED', finished_at=NOW(3) WHERE id=?`, s.attID); err != nil {
		t.Fatalf("finalize attempt: %v", err)
	}
	// 触发扫描③路径需要 last_activity_at 静默窗;直接调处置事务等价
	// (扫描③的候选谓词在本用例外测)。
	if err := r.reapAttempt(context.Background(), s.nodeExecID, s.execID, entryOrphan); err != nil {
		t.Fatalf("reapAttempt: %v", err)
	}
	if ns, _, _ := readNodeRow(t, d, s.nodeExecID); ns != engine.NodeCancelled {
		t.Fatalf("node = %s, want CANCELLED", ns)
	}
	if st, _, finished := readAttemptRow(t, d, s.attID); st != engine.AttemptSucceed || !finished.Valid {
		t.Fatalf("attempt = %s, want SUCCEEDED 原样(不二次改写)", st)
	}
}

