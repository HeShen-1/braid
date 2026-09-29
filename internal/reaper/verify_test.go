package reaper

// T9.3 链校验第七作业 + 启动自检(t13 真 MySQL,不可达自动 skip):
// 周期作业水位追平 + 指标发射;断链是**结局不是故障**(闩已落定、写路径
// 已拒,job 返回 nil——RunOnce 不因已执法的断链每 tick 红);启动自检
// 全量排空待校验头。

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"braid/internal/audit"
	"braid/internal/engine"
)

func TestVerifyAuditChainJobAdvancesWatermark(t *testing.T) {
	r, m, d := newCollectReaper(t)
	execID, _ := seedExecRow(t, d, "RUNNING", 0)
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM chain_heads WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM audit_log WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM workflow_execution WHERE id=?`, execID)
	})

	for i := 0; i < 3; i++ {
		evt := audit.Event{
			ID: nextID(t), EventType: audit.EventNodeStateChanged, ExecutionID: execID,
			NodeExecID: nextID(t), NodeID: "n1", AttemptID: nextID(t),
			ActorType: audit.ActorWorker, ActorID: "w1",
			Payload: json.RawMessage(`{"from":"PENDING","to":"RUNNING"}`),
		}
		if err := engine.WithTx(context.Background(), d, func(q engine.DBTX) error { return audit.Append(context.Background(), q, evt) }); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	if err := r.verifyAuditChain(context.Background()); err != nil {
		t.Fatalf("verifyAuditChain: %v", err)
	}
	var verified, lastID uint64
	if err := d.QueryRow(
		`SELECT verified_last_id, last_id FROM chain_heads WHERE execution_id=?`, execID).
		Scan(&verified, &lastID); err != nil {
		t.Fatalf("head: %v", err)
	}
	if verified != lastID {
		t.Fatalf("verified_last_id = %d, want last_id %d", verified, lastID)
	}
	if got := testutil.ToFloat64(m.ChainVerifyRows); got != 3 {
		t.Fatalf("chain_verify_rows = %v, want 3", got)
	}
	if got := testutil.ToFloat64(m.ChainBrokenHeads); got != 0 {
		t.Fatalf("chain_broken_heads = %v, want 0", got)
	}

	// 幂等:第二轮无新行,零走链
	if err := r.verifyAuditChain(context.Background()); err != nil {
		t.Fatalf("verifyAuditChain(2nd): %v", err)
	}
	if got := testutil.ToFloat64(m.ChainVerifyRows); got != 3 {
		t.Fatalf("chain_verify_rows(2nd) = %v, want 3(无新行不加)", got)
	}
}

func TestVerifyAuditChainBrokenIsOutcomeNotError(t *testing.T) {
	r, m, d := newCollectReaper(t)
	execID, _ := seedExecRow(t, d, "RUNNING", 0)
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM chain_heads WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM audit_log WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM workflow_execution WHERE id=?`, execID)
	})

	for i := 0; i < 2; i++ {
		evt := audit.Event{
			ID: nextID(t), EventType: audit.EventNodeStateChanged, ExecutionID: execID,
			NodeExecID: nextID(t), NodeID: "n1", AttemptID: nextID(t),
			ActorType: audit.ActorWorker, ActorID: "w1",
			Payload: json.RawMessage(`{"from":"PENDING","to":"RUNNING"}`),
		}
		if err := engine.WithTx(context.Background(), d, func(q engine.DBTX) error { return audit.Append(context.Background(), q, evt) }); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if _, err := d.Exec(
		`UPDATE audit_log SET actor_id='tamperer' WHERE execution_id=? LIMIT 1`, execID); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	// 断链已执法(闩 + 写路径拒绝),job 不报错——RunOnce 语义保持收敛
	if err := r.verifyAuditChain(context.Background()); err != nil {
		t.Fatalf("verifyAuditChain(broken) = %v, want nil(结局不是故障)", err)
	}
	if got := testutil.ToFloat64(m.ChainBrokenTotal); got != 1 {
		t.Fatalf("chain_broken_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.ChainBrokenHeads); got != 1 {
		t.Fatalf("chain_broken_heads = %v, want 1", got)
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce with broken chain = %v, want nil", err)
	}
	// 重复作业:已闩不重复计数
	if err := r.verifyAuditChain(context.Background()); err != nil {
		t.Fatalf("verifyAuditChain(2nd): %v", err)
	}
	if got := testutil.ToFloat64(m.ChainBrokenTotal); got != 1 {
		t.Fatalf("chain_broken_total(2nd) = %v, want 1(首见计数)", got)
	}
}

func TestStartupVerifyDrainsDueHeads(t *testing.T) {
	r, _, d := newCollectReaper(t)
	execID, _ := seedExecRow(t, d, "RUNNING", 0)
	execID2, _ := seedExecRow(t, d, "RUNNING", 0)
	for _, id := range []uint64{execID, execID2} {
		t.Cleanup(func() {
			_, _ = d.Exec(`DELETE FROM chain_heads WHERE execution_id=?`, id)
			_, _ = d.Exec(`DELETE FROM audit_log WHERE execution_id=?`, id)
			_, _ = d.Exec(`DELETE FROM workflow_execution WHERE id=?`, id)
		})
		evt := audit.Event{
			ID: nextID(t), EventType: audit.EventNodeStateChanged, ExecutionID: id,
			NodeExecID: nextID(t), NodeID: "n1", AttemptID: nextID(t),
			ActorType: audit.ActorWorker, ActorID: "w1",
			Payload: json.RawMessage(`{}`),
		}
		if err := engine.WithTx(context.Background(), d, func(q engine.DBTX) error { return audit.Append(context.Background(), q, evt) }); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	r.StartupVerify(context.Background())
	for _, id := range []uint64{execID, execID2} {
		var verified, lastID uint64
		if err := d.QueryRow(
			`SELECT verified_last_id, last_id FROM chain_heads WHERE execution_id=?`, id).
			Scan(&verified, &lastID); err != nil {
			t.Fatalf("head %d: %v", id, err)
		}
		if verified != lastID {
			t.Fatalf("exec %d verified=%d last=%d(启动自检未排空)", id, verified, lastID)
		}
	}
}
