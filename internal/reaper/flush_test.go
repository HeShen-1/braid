package reaper

// T6.1 暂存审计搬运的 reaper 驱动半边测试:flushOnce 排空暂存 → audit_log
// 在场;auditFlushLoop 与 janitorLoop 同形(错误只记日志),循环本体不
// 重复测。搬运语义(批量契约/毒行/对账)的完整验收在 internal/audit。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"braid/internal/audit"
	"braid/internal/engine"
)

func TestFlushOnceDrainsStagedAudit(t *testing.T) {
	r, d := newReaper(t, Config{
		ScanInterval:       time.Second,
		AuditFlushInterval: time.Second,
		AuditFlushBatch:    100,
		JanitorInterval:    time.Hour,
	})
	// seedReap 提供最小执行链(判死扫描不在本测试面,只要 execution 行)。
	sd := seedReap(t, d, 0, 0, 1)
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM audit_pending WHERE execution_id=?`, sd.execID)
		_, _ = d.Exec(`DELETE FROM chain_heads WHERE execution_id=?`, sd.execID) // T9.3 链锚(Enqueue 建链)
	})

	id, err := mustTestIDs(t).Next()
	if err != nil {
		t.Fatalf("gen id: %v", err)
	}
	raw, err := json.Marshal(audit.NodeStateChangedPayload{
		From: "EXECUTING", To: "SUCCEEDED", NodeID: "n1", Version: 2, AttemptNo: 1,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ctx := context.Background()
	if err := engine.WithTx(ctx, d, func(q engine.DBTX) error {
		return audit.Enqueue(ctx, q, audit.Event{
			ID: id, EventType: audit.EventNodeStateChanged, ExecutionID: sd.execID,
			NodeExecID: sd.nodeExecID, NodeID: "n1", AttemptID: sd.attID,
			ActorType: audit.ActorWorker, ActorID: "w1", Payload: raw,
		})
	}); err != nil {
		t.Fatalf("stage: %v", err)
	}

	// 暂存在场(排空前)→ flushOnce → 排空 + 终行在场。
	var staged int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_pending WHERE execution_id=?`, sd.execID).
		Scan(&staged); err != nil || staged != 1 {
		t.Fatalf("staged before flush = %d (err %v), want 1", staged, err)
	}
	if err := r.flushOnce(ctx); err != nil {
		t.Fatalf("flushOnce: %v", err)
	}
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_pending WHERE execution_id=?`, sd.execID).
		Scan(&staged); err != nil || staged != 0 {
		t.Fatalf("staged after flush = %d (err %v), want 0", staged, err)
	}
	var final int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND event_type=? AND attempt_id=?`,
		sd.execID, audit.EventNodeStateChanged, sd.attID).Scan(&final); err != nil || final != 1 {
		t.Fatalf("audit_log rows = %d (err %v), want 1", final, err)
	}
}

// TestAuditFlushLoopWiring(T6.1 接线锚,对抗审计 test-gaps:删除 Run 的
// auditFlushLoop 启动块全绿):首轮立即搬运 + ctx 取消干净退出。
// 长 tick(10min)隔离变量:排空只能来自启动首 flushOnce。
func TestAuditFlushLoopWiring(t *testing.T) {
	r, d := newReaper(t, Config{
		ScanInterval:       time.Second,
		AuditFlushInterval: 10 * time.Minute,
		AuditFlushBatch:    100,
		JanitorInterval:    time.Hour,
	})
	sd := seedReap(t, d, 0, 0, 1)
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM audit_pending WHERE execution_id=?`, sd.execID)
		_, _ = d.Exec(`DELETE FROM chain_heads WHERE execution_id=?`, sd.execID) // T9.3 链锚(Enqueue 建链)
	})

	id, err := mustTestIDs(t).Next()
	if err != nil {
		t.Fatalf("gen id: %v", err)
	}
	if err := engine.WithTx(context.Background(), d, func(q engine.DBTX) error {
		return audit.Enqueue(context.Background(), q, audit.Event{
			ID: id, EventType: audit.EventNodeStateChanged, ExecutionID: sd.execID,
			NodeExecID: sd.nodeExecID, NodeID: "n1", ActorType: audit.ActorWorker, ActorID: "w1",
			Payload: json.RawMessage(`{}`),
		})
	}); err != nil {
		t.Fatalf("stage: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.auditFlushLoop(ctx, "gate-test") }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM audit_pending WHERE execution_id=?`, sd.execID).
			Scan(&n); err == nil && n == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	var final int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_pending WHERE execution_id=?`, sd.execID).
		Scan(&final); err != nil || final != 0 {
		t.Fatalf("staged after loop = %d (err %v), want 0(首轮必须已排空)", final, err)
	}
}
