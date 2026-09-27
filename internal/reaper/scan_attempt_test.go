package reaper

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"braid/internal/scheduler"
)

// TestReapRedispatchCarriesOriginalTrace:心跳判死重派拷原行 trace(T8.1
// trace 连续性维度:重放 = 原事件同一真相,不换段)——原派发行
// att:<id>:1 带主链 X,重派行 att:<id>:2 的 payload.trace_id 与 trace 列
// 都 == X(与 redispatchResume 死信网同式;列/payload 同值双写)。
func TestReapRedispatchCarriesOriginalTrace(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedReap(t, d, 5*time.Minute, 0, 3) // 心跳陈旧判死;deadline 未到;预算内重派
	const x = "4bf92f3577b34da6a3ce929d0e0e4736"
	payload, err := json.Marshal(scheduler.NodeTaskPayload{
		ExecutionID: s.execID, NodeExecutionID: s.nodeExecID, AttemptNo: 1, TraceID: x,
	})
	if err != nil {
		t.Fatalf("marshal node task payload: %v", err)
	}
	origTask := fmt.Sprintf("att:%d:1", s.nodeExecID)
	if _, err := d.Exec(`INSERT INTO outbox
	  (id, execution_id, event_type, task_id, queue, payload, status, trace_id, dispatched_at, available_at)
	  VALUES (?, ?, 'node.task_enqueue', ?, 'reap-test', ?, 'DISPATCHED', ?, NOW(3), NOW(3))`,
		nextID(t), s.execID, origTask, payload, x); err != nil {
		t.Fatalf("seed original dispatch event: %v", err)
	}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	rows := fetchOutbox(t, d, s.execID, "node.task_enqueue")
	if len(rows) != 2 {
		t.Fatalf("node.task_enqueue 行数 = %d, want 2(原行 + 重派)", len(rows))
	}
	want := fmt.Sprintf("att:%d:2", s.nodeExecID)
	if rows[1].taskID != want {
		t.Fatalf("重派 task_id = %q, want %q", rows[1].taskID, want)
	}
	var np scheduler.NodeTaskPayload
	if err := json.Unmarshal(rows[1].payload, &np); err != nil {
		t.Fatalf("redispatch payload: %v", err)
	}
	if np.TraceID != x {
		t.Fatalf("重派 payload trace_id = %q, want 原行 %q(重放不换段)", np.TraceID, x)
	}
	var colTrace sql.NullString
	if err := d.QueryRow(
		`SELECT trace_id FROM outbox WHERE task_id=?`, want).Scan(&colTrace); err != nil {
		t.Fatalf("read redispatched row trace column: %v", err)
	}
	if colTrace.String != x {
		t.Fatalf("重派行 trace 列 = %v, want %q(列/payload 同值双写)", colTrace, x)
	}
}

// TestReapRedispatchNoTraceStaysZeroInjected:原行无 trace(内部测试路径
// 派发)→ 重派零注入:payload 键缺席、列 NULL——不伪造段链。
func TestReapRedispatchNoTraceStaysZeroInjected(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedReap(t, d, 5*time.Minute, 0, 3)
	origTask := fmt.Sprintf("att:%d:1", s.nodeExecID)
	payload, err := json.Marshal(scheduler.NodeTaskPayload{
		ExecutionID: s.execID, NodeExecutionID: s.nodeExecID, AttemptNo: 1,
	})
	if err != nil {
		t.Fatalf("marshal node task payload: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO outbox
	  (id, execution_id, event_type, task_id, queue, payload, status, dispatched_at, available_at)
	  VALUES (?, ?, 'node.task_enqueue', ?, 'reap-test', ?, 'DISPATCHED', NOW(3), NOW(3))`,
		nextID(t), s.execID, origTask, payload); err != nil {
		t.Fatalf("seed original dispatch event: %v", err)
	}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	want := fmt.Sprintf("att:%d:2", s.nodeExecID)
	var colTrace sql.NullString
	if err := d.QueryRow(
		`SELECT trace_id, payload FROM outbox WHERE task_id=?`, want).
		Scan(&colTrace, &payload); err != nil {
		t.Fatalf("read redispatched row: %v", err)
	}
	if colTrace.Valid {
		t.Fatalf("无 trace 原行 → 重派 trace 列 = %q, want NULL(不伪造段链)", colTrace.String)
	}
	var np scheduler.NodeTaskPayload
	if err := json.Unmarshal(payload, &np); err != nil {
		t.Fatalf("redispatch payload: %v", err)
	}
	if np.TraceID != "" {
		t.Fatalf("无 trace 原行 → 重派 payload trace_id = %q, want 空(键缺席零注入)", np.TraceID)
	}
}
