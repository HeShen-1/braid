package scheduler

// T2.6 验收守护:每次节点状态迁移/调度裁决产生对应审计行。三个用例钉住
// scheduler 侧全部埋点(claim/succeed/fail/redispatch/skip/branch.evaluated)
// 的行序、事件类型、actor 与 attempt 锚;词表闭集与列落位的单元面归
// internal/audit/writer_test.go。

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"braid/internal/audit"
	"braid/internal/engine"
)

// auditEvent 是断言用审计行投影。
type auditEvent struct {
	eventType string
	nodeID    sql.NullString
	attemptID sql.NullInt64
	actorType string
	actorID   string
}

// auditEvents 读该执行的审计行(ORDER BY id:同事务内雪花序 = 落行序)。
// T6.1 起高频四类经 audit_pending 异步——读取口先排空暂存(reaper flush
// 的测试内等价物),断言面对的是全量终行。
func auditEvents(t *testing.T, d *sql.DB, execID uint64) []auditEvent {
	t.Helper()
	if _, err := audit.FlushAll(context.Background(), d, 500, 10000); err != nil {
		t.Fatalf("flush staged audit: %v", err)
	}
	rows, err := d.Query(`
SELECT event_type, node_id, attempt_id, actor_type, actor_id
  FROM audit_log WHERE execution_id=? ORDER BY id`, execID)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditEvent
	for rows.Next() {
		var e auditEvent
		if err := rows.Scan(&e.eventType, &e.nodeID, &e.attemptID, &e.actorType, &e.actorID); err != nil {
			t.Fatalf("scan audit: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("audit rows: %v", err)
	}
	return out
}

// wantEvent 是期望序的简写形:eventType|nodeID|attemptID(-0 = NULL 列)。
func wantEvent(eventType, nodeID string, attemptID uint64) auditEvent {
	e := auditEvent{eventType: eventType, nodeID: sql.NullString{String: nodeID, Valid: true},
		actorType: audit.ActorWorker, actorID: "sched-test"}
	if attemptID != 0 {
		e.attemptID = sql.NullInt64{Int64: int64(attemptID), Valid: true}
	}
	return e
}

func assertAuditSequence(t *testing.T, d *sql.DB, execID uint64, want []auditEvent) {
	t.Helper()
	got := auditEvents(t, d, execID)
	if len(got) != len(want) {
		t.Fatalf("audit rows = %d, want %d\ngot:  %+v\nwant: %+v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("audit[%d] = %+v, want %+v\ngot:  %+v\nwant: %+v", i, got[i], want[i], got, want)
		}
	}
}

// TestAuditTrailChainLifecycle:链式执行全程 —— 每次迁移一行的行序与
// attempt 锚(派发行无 attempt 行可锚 → NULL;观察行锚定收口的 attempt)。
func TestAuditTrailChainLifecycle(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, chainDSL, `{"q":"hi"}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	attA := seedAttempt(t, d, execID, a.id, 1)
	mustComplete(t, s, execID, a.id, attA, engine.AttemptSucceed, json.RawMessage(`{"text":"1"}`))

	b := waitNodeStatus(t, d, execID, "b", engine.NodeExecuting)
	attB := seedAttempt(t, d, execID, b.id, 1)
	mustComplete(t, s, execID, b.id, attB, engine.AttemptSucceed, json.RawMessage(`{"text":"2"}`))

	c := waitNodeStatus(t, d, execID, "c", engine.NodeExecuting)
	attC := seedAttempt(t, d, execID, c.id, 1)
	mustComplete(t, s, execID, c.id, attC, engine.AttemptSucceed, json.RawMessage(`{"text":"3"}`))
	waitExecStatus(t, d, execID, engine.WorkflowSucceeded)

	assertAuditSequence(t, d, execID, []auditEvent{
		wantEvent(audit.EventNodeStateChanged, "a", 0), // Start 派发 claim
		wantEvent(audit.EventNodeStateChanged, "a", attA),
		wantEvent(audit.EventNodeStateChanged, "b", 0), // 同事务推进派发
		wantEvent(audit.EventNodeStateChanged, "b", attB),
		wantEvent(audit.EventNodeStateChanged, "c", 0),
		wantEvent(audit.EventNodeStateChanged, "c", attC),
	})
}

// TestAuditTrailBranchAndSkip:branch 内联(2 迁移 + branch.evaluated)+
// 剪枝传播(d/e/f)+ 取路派发(c)。
func TestAuditTrailBranchAndSkip(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, branchDSL, `{}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	attA := seedAttempt(t, d, execID, a.id, 1)
	mustComplete(t, s, execID, a.id, attA, engine.AttemptSucceed, json.RawMessage(`{"flag":true,"text":"路上"}`))
	c := waitNodeStatus(t, d, execID, "c", engine.NodeExecuting)
	attC := seedAttempt(t, d, execID, c.id, 1)
	mustComplete(t, s, execID, c.id, attC, engine.AttemptSucceed, json.RawMessage(`{"text":"取路值"}`))
	waitExecStatus(t, d, execID, engine.WorkflowSucceeded)

	assertAuditSequence(t, d, execID, []auditEvent{
		wantEvent(audit.EventNodeStateChanged, "a", 0),
		wantEvent(audit.EventNodeStateChanged, "a", attA),
		wantEvent(audit.EventNodeStateChanged, "b", 0), // 内联 claim
		wantEvent(audit.EventNodeStateChanged, "b", 0), // 内联 succeed(无 attempt 行)
		wantEvent(audit.EventBranchEvaluated, "b", 0),  // 路由决定(D-t24 #3)
		wantEvent(audit.EventNodeStateChanged, "d", 0), // 未取臂剪枝
		wantEvent(audit.EventNodeStateChanged, "e", 0), // 传递闭包
		wantEvent(audit.EventNodeStateChanged, "f", 0),
		wantEvent(audit.EventNodeStateChanged, "c", 0), // 取路派发
		wantEvent(audit.EventNodeStateChanged, "c", attC),
	})

	// 路由决定 payload:next 取路目标(review 修:分支语义判别)。
	var raw []byte
	if err := d.QueryRow(`SELECT payload FROM audit_log WHERE execution_id=? AND event_type=?`,
		execID, audit.EventBranchEvaluated).Scan(&raw); err != nil {
		t.Fatalf("read branch.evaluated payload: %v", err)
	}
	var bp audit.BranchEvaluatedPayload
	if err := json.Unmarshal(raw, &bp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if bp.NodeID != "b" || bp.Next != "c" {
		t.Fatalf("branch.evaluated payload = %+v, want b/c", bp)
	}
}

// TestAuditTrailRetryAndExhaust:重试路径 node.redispatched(status-preserving,
// 无 state_changed 行)+ 耗尽 fail(带 code 与 attempt 锚)。
func TestAuditTrailRetryAndExhaust(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, retryDSL, `{}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)

	att1 := seedAttempt(t, d, execID, a.id, 1)
	mustComplete(t, s, execID, a.id, att1, engine.AttemptFailed, nil)
	if a2 := readNode(t, d, execID, "a"); a2.status != engine.NodeExecuting || a2.attempts != 2 {
		t.Fatalf("node a after retry = %+v, want EXECUTING/attempts2", a2)
	}
	att2 := seedAttempt(t, d, execID, a.id, 2)
	mustComplete(t, s, execID, a.id, att2, engine.AttemptFailed, nil)
	waitExecStatus(t, d, execID, engine.WorkflowFailed)

	assertAuditSequence(t, d, execID, []auditEvent{
		wantEvent(audit.EventNodeStateChanged, "a", 0),    // claim
		wantEvent(audit.EventNodeRedispatched, "a", 0),    // 重试再派发(非迁移)
		wantEvent(audit.EventNodeStateChanged, "a", att2), // 耗尽终败
	})

	// 重派行 payload:trigger 与新 attempt_no。
	var p audit.NodeRedispatchedPayload
	var raw []byte
	if err := d.QueryRow(`SELECT payload FROM audit_log WHERE execution_id=? AND event_type=?`,
		execID, audit.EventNodeRedispatched).Scan(&raw); err != nil {
		t.Fatalf("read redispatch payload: %v", err)
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.NodeID != "a" || p.AttemptNo != 2 || p.Trigger != audit.TriggerAttemptFailed {
		t.Fatalf("redispatch payload = %+v", p)
	}

	// 终败行 payload:code 归一化(attempts_exhausted)。
	if err := d.QueryRow(`SELECT payload FROM audit_log WHERE execution_id=? AND event_type=?
	  AND node_id='a' ORDER BY id DESC LIMIT 1`, execID, audit.EventNodeStateChanged).Scan(&raw); err != nil {
		t.Fatalf("read fail payload: %v", err)
	}
	var fp audit.NodeStateChangedPayload
	if err := json.Unmarshal(raw, &fp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if fp.From != "EXECUTING" || fp.To != "FAILED" || fp.Code != "attempts_exhausted" || fp.AttemptNo != 2 {
		t.Fatalf("fail payload = %+v", fp)
	}
}

// TestHotPathRoutesThroughStaging(T6.1 路由锚,对抗审计 test-gaps:stageRow
// 任一回退 auditRow 全套件绿):flush 之前,高频 state_changed 必须在
// audit_pending 且不在 audit_log——「高频四类走暂存」是分阶段口径的承重墙。
func TestHotPathRoutesThroughStaging(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, chainDSL, `{"q":"hi"}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)

	var pending, inLog int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_pending WHERE execution_id=? AND event_type=?`,
		execID, audit.EventNodeStateChanged).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE execution_id=?`,
		execID).Scan(&inLog); err != nil {
		t.Fatalf("count log: %v", err)
	}
	if pending != 1 {
		t.Fatalf("staged state_changed = %d, want 1(高频类必须先落暂存——回退同步直写即路由破坏)", pending)
	}
	if inLog != 0 {
		t.Fatalf("audit_log rows before flush = %d, want 0", inLog)
	}
	// 排空后终行在场(读取口自带 flush);cleanupExec 收暂存面。
	if got := auditEvents(t, d, execID); len(got) != 1 || got[0].eventType != audit.EventNodeStateChanged {
		t.Fatalf("after flush = %+v, want single state_changed", got)
	}
}
