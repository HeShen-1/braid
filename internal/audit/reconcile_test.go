package audit

// TestFlushReconcilesByAttempt(T6.1 验收核心:「批量写无丢失」对账单测)。
// 场景 = scheduler 热路径的真实形状:2 节点 × 3 attempt(llm 一次失败重试后
// 成功 + tool 成功 + branch 路由),事件跨多个事务交错暂存,flush 以小于
// 总量的批上限分多轮搬运。对账面:
//   - 按 attempt 核销(DDL idx_audit_attempt 的设计意图):node_attempt
//     真值表逐 attempt 比对 audit_log 的 attempt 级行,期望事件集全到、
//     无丢失、无重复;
//   - 总行数守恒(staged 总量 = audit_log 落行量);
//   - 暂存排空;
//   - event_at 保真(暂存时刻 = 终行事件时刻,非 flush 时刻);
//   - D-t55 纪律:断言全部按事件集/身份,不依赖物理行序或「触发行非首行」。
//
// 变异自查(实现错 → 哪条红):
//   - flush 丢行 → 总量守恒或 attempt 核销红;
//   - flush 重复搬运(重放不幂等)→ 总量守恒红;
//   - event_at 被重算为 flush 时刻 → 保真锚红;
//   - attempt_id 列丢失 → attempt 核销红(空集)。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
)

// seedNodeExec 落最小 node_execution 行(FK 载体)。
func seedNodeExec(t *testing.T, d *sql.DB, execID uint64, nodeID, typ string) uint64 {
	t.Helper()
	id := nextID(t)
	if _, err := d.Exec(`
INSERT INTO node_execution (id, execution_id, node_id, type, status)
VALUES (?, ?, ?, ?, 'SUCCEEDED')`, id, execID, nodeID, typ); err != nil {
		t.Fatalf("seed node_exec: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM node_execution WHERE id=?`, id) })
	return id
}

// seedAttempt 落最小 node_attempt 行(对账真值表)。
func seedAttempt(t *testing.T, d *sql.DB, execID, nodeExecID uint64, attemptNo int) uint64 {
	t.Helper()
	id := nextID(t)
	key := fmt.Sprintf("att:%d:%d", nodeExecID, attemptNo)
	if _, err := d.Exec(`
INSERT INTO node_attempt (id, node_execution_id, execution_id, attempt_no, status,
                          idempotency_key, asynq_task_id, started_at)
VALUES (?, ?, ?, ?, 'SUCCEEDED', ?, ?, NOW(3))`, id, nodeExecID, execID, attemptNo, key, key); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM node_attempt WHERE id=?`, id) })
	return id
}

func TestFlushReconcilesByAttempt(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	ctx := context.Background()
	execID := seedExec(t, d)

	// 拓扑:n1(llm,2 attempts:失败→重试成功)、n2(tool,1 attempt)、
	// n3(branch,无 attempt 行——路由裁决在派发面)。
	n1 := seedNodeExec(t, d, execID, "n1", "llm")
	n2 := seedNodeExec(t, d, execID, "n2", "tool")
	n3 := seedNodeExec(t, d, execID, "n3", "branch")
	att1 := seedAttempt(t, d, execID, n1, 1)
	att2 := seedAttempt(t, d, execID, n1, 2)
	att3 := seedAttempt(t, d, execID, n2, 1)
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM audit_pending WHERE execution_id=?`, execID) })

	statePayload := func(from, to string, no uint16) json.RawMessage {
		raw, err := json.Marshal(NodeStateChangedPayload{From: from, To: to, NodeID: "n1", AttemptNo: no})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return raw
	}
	branchPayload, err := json.Marshal(BranchEvaluatedPayload{NodeID: "n3", Next: "n2"})
	if err != nil {
		t.Fatalf("marshal branch: %v", err)
	}
	llmPayload, err := json.Marshal(LLMCallCompletedPayload{NodeID: "n1", Model: "glm-5.3", TotalTokens: 88})
	if err != nil {
		t.Fatalf("marshal llm: %v", err)
	}
	toolPayload, err := json.Marshal(ToolCallCompletedPayload{NodeID: "n2", Tool: "http", DurationMs: 120})
	if err != nil {
		t.Fatalf("marshal tool: %v", err)
	}

	// 交错暂存(每事件独立事务,模拟 scheduler 各状态迁移事务;顺序刻意
	// 与 attempt 号错开,验证断言不依赖物理序)。
	events := []Event{
		{ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: execID,
			NodeExecID: n1, NodeID: "n1", AttemptID: att1, ActorType: ActorWorker, ActorID: "w1",
			Payload: statePayload("EXECUTING", "FAILED", 1)},
		{ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: execID,
			NodeExecID: n1, NodeID: "n1", AttemptID: att2, ActorType: ActorWorker, ActorID: "w1",
			Payload: statePayload("EXECUTING", "SUCCEEDED", 2)},
		{ID: nextID(t), EventType: EventToolCallCompleted, ExecutionID: execID,
			NodeExecID: n2, NodeID: "n2", AttemptID: att3, ActorType: ActorTool, ActorID: "http",
			Payload: toolPayload},
		{ID: nextID(t), EventType: EventBranchEvaluated, ExecutionID: execID,
			NodeExecID: n3, NodeID: "n3", ActorType: ActorWorker, ActorID: "w1",
			Payload: branchPayload},
		{ID: nextID(t), EventType: EventLLMCallCompleted, ExecutionID: execID,
			NodeExecID: n1, NodeID: "n1", AttemptID: att2, ActorType: ActorLLM, ActorID: "glm-5.3",
			Payload: llmPayload,
			Usage: &engine.AttemptUsage{Model: "glm-5.3", TotalTokens: 88,
				CostAmount: "0.001100", CostCurrency: "CNY", Billed: true}},
		{ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: execID,
			NodeExecID: n2, NodeID: "n2", AttemptID: att3, ActorType: ActorWorker, ActorID: "w1",
			Payload: statePayload("EXECUTING", "SUCCEEDED", 1)},
	}
	stagedAt := map[uint64]string{} // id → 暂存时刻(保真锚的期望侧)
	for i := range events {
		evt := events[i]
		if err := engine.WithTx(ctx, d, func(q engine.DBTX) error {
			return Enqueue(ctx, q, evt)
		}); err != nil {
			t.Fatalf("stage %d: %v", evt.ID, err)
		}
		var at string
		if err := d.QueryRow(`SELECT CAST(event_at AS CHAR) FROM audit_pending WHERE id=?`, evt.ID).
			Scan(&at); err != nil {
			t.Fatalf("read staged event_at: %v", err)
		}
		stagedAt[evt.ID] = at
	}

	// 分多轮搬运(batch 2 < 总量 6):批切分也是对账面(跨轮无丢失)。
	var totalFlushed int
	for {
		res, err := Flush(ctx, d, 0, 2)
		if err != nil {
			t.Fatalf("Flush round: %v", err)
		}
		totalFlushed += res.Flushed
		if res.Staged < 2 {
			break
		}
	}
	if totalFlushed != len(events) {
		t.Fatalf("total flushed = %d, want %d", totalFlushed, len(events))
	}

	// 暂存排空。
	if n := len(readStaged(t, d, execID)); n != 0 {
		t.Fatalf("staged after flush = %d, want 0", n)
	}

	// 总量守恒:audit_log 行数 = 暂存总量(无丢失、无重复)。
	rows := flushAuditRows(t, d, execID)
	if len(rows) != len(events) {
		t.Fatalf("audit rows = %d, want %d(守恒)", len(rows), len(events))
	}
	byID := map[uint64]auditRow{}
	for _, r := range rows {
		byID[r.id] = r
	}
	for _, evt := range events {
		if _, ok := byID[evt.ID]; !ok {
			t.Fatalf("event %d (%s) 丢失", evt.ID, evt.EventType)
		}
	}

	// 按 attempt 核销(idx_audit_attempt 查询形状):node_attempt 真值表
	// 逐 attempt 比对期望事件集——集合相等 = 全到且无重复。
	wantByAttempt := map[uint64]map[string]bool{
		att1: {EventNodeStateChanged: true},
		att2: {EventNodeStateChanged: true, EventLLMCallCompleted: true},
		att3: {EventNodeStateChanged: true, EventToolCallCompleted: true},
	}
	for attID, want := range wantByAttempt {
		got := map[string]bool{}
		arows, err := d.Query(`SELECT event_type FROM audit_log WHERE attempt_id=?`, attID)
		if err != nil {
			t.Fatalf("reconcile attempt %d: %v", attID, err)
		}
		for arows.Next() {
			var et string
			if err := arows.Scan(&et); err != nil {
				t.Fatalf("scan reconcile: %v", err)
			}
			if got[et] {
				t.Fatalf("attempt %d: event %s 重复落行", attID, et)
			}
			got[et] = true
		}
		if err := arows.Err(); err != nil {
			t.Fatalf("reconcile rows: %v", err)
		}
		_ = arows.Close()
		if len(got) != len(want) {
			t.Fatalf("attempt %d 事件集 = %v, want %v(核销缺失或多余)", attID, got, want)
		}
		for et := range want {
			if !got[et] {
				t.Fatalf("attempt %d 缺事件 %s(批量写丢失)", attID, et)
			}
		}
	}

	// event_at 保真:终行事件时刻 = 暂存时刻(非 flush 时刻)。
	for _, evt := range events {
		var at string
		if err := d.QueryRow(`SELECT CAST(event_at AS CHAR) FROM audit_log WHERE execution_id=? AND id=?`,
			execID, evt.ID).Scan(&at); err != nil {
			t.Fatalf("read audit event_at: %v", err)
		}
		if at != stagedAt[evt.ID] {
			t.Fatalf("event %d event_at = %s, want staged %s(事件时刻被重算)", evt.ID, at, stagedAt[evt.ID])
		}
	}

	// 落库滞后半边:created_at ≥ event_at(异步分立的两侧都在场)。
	var lagNegative int
	if err := d.QueryRow(`
SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND created_at < event_at`, execID).
		Scan(&lagNegative); err != nil {
		t.Fatalf("lag check: %v", err)
	}
	if lagNegative != 0 {
		t.Fatalf("created_at < event_at rows = %d(时钟语义倒置)", lagNegative)
	}
}
