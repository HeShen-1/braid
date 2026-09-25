package reaper

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"braid/internal/metrics"
)

// newCollectReaper 构造带指标面的被测实例(同包直写 r.m,免扩 newReaper)。
func newCollectReaper(t *testing.T) (*Reaper, *metrics.ReaperSet, *sql.DB) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.ScanInterval = 50 * time.Millisecond
	cfg.DeadGrace = 50 * time.Millisecond
	cfg.ExecStale = 50 * time.Millisecond
	r, d := newReaper(t, cfg)
	set, _ := metrics.NewSet()
	r.m = set.Reaper
	return r, set.Reaper, d
}

// TestCollectAuditPendingAndOutboxGauges(T6.4 验收面:注入积压后指标计数
// 断言):audit_pending 2 行(其一回拨)→ rows/oldest 双 gauge;outbox
// DEAD 1 行 + PENDING 1 行 → 按 status 分面 + 年龄;DEAD 是唯一可靠
// 死信面(D-t42 §8-②/T7.4 重放依赖)。
func TestCollectAuditPendingAndOutboxGauges(t *testing.T) {
	r, m, d := newCollectReaper(t)
	execID := nextID(t)
	if _, err := d.Exec(`INSERT INTO audit_pending
	  (id, event_at, event_type, execution_id, actor_type, actor_id, payload, created_at)
	  VALUES (?, NOW(3), 'node.state_changed', ?, 'worker', 'w1', '{}', NOW(3) - INTERVAL 90 SECOND),
	         (?, NOW(3), 'node.state_changed', ?, 'worker', 'w1', '{}', NOW(3))`,
		nextID(t), execID, nextID(t), execID); err != nil {
		t.Fatalf("seed audit_pending: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM audit_pending WHERE execution_id=?`, execID) })
	if _, err := d.Exec(`INSERT INTO outbox
	  (id, execution_id, event_type, task_id, queue, payload, status, created_at)
	  VALUES (?, ?, 'execution.start', 'x:1', 'default', '{}', 'DEAD', NOW(3) - INTERVAL 120 SECOND),
	         (?, ?, 'execution.start', 'x:2', 'default', '{}', 'PENDING', NOW(3))`,
		nextID(t), execID, nextID(t), execID); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM outbox WHERE execution_id=?`, execID) })

	if err := r.collectMetrics(context.Background()); err != nil {
		t.Fatalf("collectMetrics: %v", err)
	}
	if got := testutil.ToFloat64(m.AuditPendingRows); got != 2 {
		t.Errorf("audit_pending_rows = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.AuditPendingOldest); got < 89 || got > 120 {
		t.Errorf("audit_pending_oldest = %v, want ~90", got)
	}
	if got := testutil.ToFloat64(m.OutboxRows.WithLabelValues(metrics.OutboxDead)); got != 1 {
		t.Errorf("outbox_rows{dead} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.OutboxRows.WithLabelValues(metrics.OutboxPending)); got != 1 {
		t.Errorf("outbox_rows{pending} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.OutboxOldest.WithLabelValues(metrics.OutboxDead)); got < 119 {
		t.Errorf("outbox_oldest{dead} = %v, want >=120s-ish", got)
	}
}

// TestCollectApprovalBacklogGauge:审批挂起积压(T6.4 验收面「注入审批挂起
// 后指标计数断言」)——WAITING_APPROVAL × 活执行 → backlog=1 + 年龄 > 0。
func TestCollectApprovalBacklogGauge(t *testing.T) {
	r, m, d := newCollectReaper(t)
	s := seedApproval(t, d, "RUNNING", time.Hour, "escalate", false)
	// 挂起年龄:快照 status_changed_at 回拨(schema default = 插入时刻,
	// 秒级 TIMESTAMPDIFF 恒 0)。
	if _, err := d.Exec(`UPDATE node_execution SET status_changed_at = NOW(3) - INTERVAL 1 HOUR
	  WHERE id=?`, s.nodeExecID); err != nil {
		t.Fatalf("backdate status_changed_at: %v", err)
	}
	if err := r.collectMetrics(context.Background()); err != nil {
		t.Fatalf("collectMetrics: %v", err)
	}
	if got := testutil.ToFloat64(m.ApprovalBacklog); got != 1 {
		t.Errorf("approval_backlog = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.ApprovalOldest); got <= 0 {
		t.Errorf("approval_backlog_oldest = %v, want > 0", got)
	}
}

// TestCollectResumeStuckAndNotifyMissingGauges:⑤a 滞留(deadline 过期 +
// 已升级)与 escalated-vs-notified 对账(升级超宽限、无 notified 行)。
// 补 notified 行后 missing 归零——对账面收敛性锚。
func TestCollectResumeStuckAndNotifyMissingGauges(t *testing.T) {
	r, m, d := newCollectReaper(t)
	// 已决票形态(对抗审计 HIGH 修复后的语义):decision + deadline 过期
	// → resume_stuck = 1;escalated-only 是 Q9 设计内等待,必须不计
	// (原谓词极性反了会把正常审批时延当滞留,CRITICAL 常燃)。
	s := seedApproval(t, d, "RUNNING", time.Hour, "escalate", true)
	seedDecision(t, d, s, "APPROVED")
	// escalated 回拨跨过 15min 对账宽限(seed 固定 1 分钟在窗内)。
	if _, err := d.Exec(`UPDATE node_execution SET escalated_at = NOW(3) - INTERVAL 20 MINUTE
	  WHERE id=?`, s.nodeExecID); err != nil {
		t.Fatalf("backdate escalated_at: %v", err)
	}
	ctx := context.Background()
	if err := r.collectMetrics(ctx); err != nil {
		t.Fatalf("collectMetrics: %v", err)
	}
	if got := testutil.ToFloat64(m.ResumeStuck); got != 1 {
		t.Errorf("resume_stuck = %v, want 1(decided + past deadline)", got)
	}
	if got := testutil.ToFloat64(m.ApprovalNotifyMissing); got != 1 {
		t.Errorf("approval_notify_missing = %v, want 1(升级无送达 = 丢失信号,D-t45)", got)
	}

	// 补送达行 → missing 归零(audit_log 直插 notified,exec_date = 执行创建日)。
	if _, err := d.Exec(`INSERT INTO audit_log
	  (id, exec_date, event_at, event_type, execution_id, node_execution_id, node_id,
	   actor_type, actor_id, payload)
	  VALUES (?, DATE((SELECT created_at FROM workflow_execution WHERE id=?)), NOW(3),
	          'approval.notified', ?, ?, 'boss', 'worker', 'w1', '{}')`,
		nextID(t), s.execID, s.execID, s.nodeExecID); err != nil {
		t.Fatalf("seed notified row: %v", err)
	}
	if err := r.collectMetrics(ctx); err != nil {
		t.Fatalf("collectMetrics(2): %v", err)
	}
	if got := testutil.ToFloat64(m.ApprovalNotifyMissing); got != 0 {
		t.Errorf("approval_notify_missing after notified = %v, want 0", got)
	}
}

// TestCollectResumeStuckExcludesEscalatedAwaiting:Q9 负例——已升级未决票
// (deadline 刻意保留)不进滞留 gauge。
func TestCollectResumeStuckExcludesEscalatedAwaiting(t *testing.T) {
	r, m, d := newCollectReaper(t)
	seedApproval(t, d, "RUNNING", time.Hour, "escalate", true) // escalated,无 decision
	if err := r.collectMetrics(context.Background()); err != nil {
		t.Fatalf("collectMetrics: %v", err)
	}
	if got := testutil.ToFloat64(m.ResumeStuck); got != 0 {
		t.Errorf("resume_stuck(escalated-only) = %v, want 0(Q9 等人决定不是滞留)", got)
	}
}

// TestCollectNotifyMissingGraceWindow:宽限窗内(升级刚发生)不计丢失——
// 在途通知不是丢失(asynq 重试 + relay 退避窗)。
func TestCollectNotifyMissingGraceWindow(t *testing.T) {
	r, m, d := newCollectReaper(t)
	seedApproval(t, d, "RUNNING", time.Hour, "escalate", true) // escalated 1 分钟前
	if err := r.collectMetrics(context.Background()); err != nil {
		t.Fatalf("collectMetrics: %v", err)
	}
	if got := testutil.ToFloat64(m.ApprovalNotifyMissing); got != 0 {
		t.Errorf("approval_notify_missing(窗内) = %v, want 0", got)
	}
}

// TestCollectNilMetricsIsNoop:Deps.Metrics nil = 观测关闭(既有全部测试
// 的默认形态),collectMetrics 短路零查询。
func TestCollectNilMetricsIsNoop(t *testing.T) {
	r, _ := newReaper(t, DefaultConfig())
	if err := r.collectMetrics(context.Background()); err != nil {
		t.Fatalf("collectMetrics(nil m): %v", err)
	}
}
