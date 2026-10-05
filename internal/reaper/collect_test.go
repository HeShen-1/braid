package reaper

import (
	"context"
	"database/sql"
	"fmt"
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

	// 补送达行 → missing 归零(T18.7:种子带轮,对账按轮点查)。
	seedNotifyAuditRow(t, d, s.execID, s.nodeExecID, "approval.notified", 1)
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

// TestCollectIMNotifyMissingGauges(T17.4):im_notified 对账——升级超宽限、
// 无 im_notified 行 → missing=1;补行归零(与 SMTP 通道同构收敛锚)。
func TestCollectIMNotifyMissingGauges(t *testing.T) {
	r, m, d := newCollectReaper(t)
	s := seedApproval(t, d, "RUNNING", time.Hour, "escalate", true)
	if _, err := d.Exec(`UPDATE node_execution SET escalated_at = NOW(3) - INTERVAL 20 MINUTE
	  WHERE id=?`, s.nodeExecID); err != nil {
		t.Fatalf("backdate escalated_at: %v", err)
	}
	ctx := context.Background()
	if err := r.collectMetrics(ctx); err != nil {
		t.Fatalf("collectMetrics: %v", err)
	}
	if got := testutil.ToFloat64(m.IMNotifyMissing); got != 1 {
		t.Errorf("approval_im_notified_missing = %v, want 1(升级无卡片送达)", got)
	}

	// 补 im_notified 送达行 → 归零(T18.7:种子带轮,镜像 sender 落行形状)。
	seedNotifyAuditRow(t, d, s.execID, s.nodeExecID, "approval.im_notified", 1)
	if err := r.collectMetrics(ctx); err != nil {
		t.Fatalf("collectMetrics(2): %v", err)
	}
	if got := testutil.ToFloat64(m.IMNotifyMissing); got != 0 {
		t.Errorf("approval_im_notified_missing after delivery = %v, want 0", got)
	}
}

// TestCollectIMNotifyGraceWindow(T17.4 负例):宽限窗内的升级不计缺失
// (在途正常,镜像 TestCollectNotifyMissingGraceWindow)。
func TestCollectIMNotifyGraceWindow(t *testing.T) {
	r, m, d := newCollectReaper(t)
	seedApproval(t, d, "RUNNING", time.Hour, "escalate", true) // escalated 1 分钟前
	if err := r.collectMetrics(context.Background()); err != nil {
		t.Fatalf("collectMetrics: %v", err)
	}
	if got := testutil.ToFloat64(m.IMNotifyMissing); got != 0 {
		t.Errorf("approval_im_notified_missing(窗内) = %v, want 0", got)
	}
}

// seedNotifyAuditRow 直插一条带轮次的送达审计行(T18.7 分轮对账:去重锚
// payload.$.round 的写半边,镜像 im/sender 落行形状)。
func seedNotifyAuditRow(t *testing.T, d *sql.DB, execID, nodeExecID uint64, eventType string, round int) {
	t.Helper()
	if _, err := d.Exec(`INSERT INTO audit_log
	  (id, exec_date, event_at, event_type, execution_id, node_execution_id, node_id,
	   actor_type, actor_id, payload)
	  VALUES (?, DATE((SELECT created_at FROM workflow_execution WHERE id=?)), NOW(3),
	          ?, ?, ?, 'boss', 'worker', 'w1', ?)`,
		nextID(t), execID, eventType, execID, nodeExecID,
		fmt.Sprintf(`{"node_id":"boss","reason":"escalated","round":%d}`, round)); err != nil {
		t.Fatalf("seed %s row: %v", eventType, err)
	}
}

// TestCollectIMNotifyMissingPerRound(T18.7 分轮对账主锚,D-t174 已知边界
// 收口):IM sender 去重按轮(payload.$.round),对账必须同粒度——第二轮
// 升级的卡片丢失不能被第一轮的送达行遮蔽。变异:点查丢 round 谓词 →
// missing 恒 0,本测试红。
func TestCollectIMNotifyMissingPerRound(t *testing.T) {
	r, m, d := newCollectReaper(t)
	s := seedApproval(t, d, "RUNNING", time.Hour, "escalate", true)
	// 推进到第二轮(回滚重挂形态:attempts=2),本轮升级已超宽限。
	if _, err := d.Exec(`UPDATE node_execution SET attempts=2, escalated_at = NOW(3) - INTERVAL 20 MINUTE
	  WHERE id=?`, s.nodeExecID); err != nil {
		t.Fatalf("seed round-2 escalation: %v", err)
	}
	// 第一轮送达行在场(round=1):不分轮的旧对账会被它遮蔽。
	seedNotifyAuditRow(t, d, s.execID, s.nodeExecID, "approval.im_notified", 1)
	if err := r.collectMetrics(context.Background()); err != nil {
		t.Fatalf("collectMetrics: %v", err)
	}
	if got := testutil.ToFloat64(m.IMNotifyMissing); got != 1 {
		t.Errorf("approval_im_notified_missing(round1 行遮蔽 round2) = %v, want 1(分轮对账)", got)
	}
	// 补本轮(round=2)送达行 → 归零。
	seedNotifyAuditRow(t, d, s.execID, s.nodeExecID, "approval.im_notified", 2)
	if err := r.collectMetrics(context.Background()); err != nil {
		t.Fatalf("collectMetrics(2): %v", err)
	}
	if got := testutil.ToFloat64(m.IMNotifyMissing); got != 0 {
		t.Errorf("approval_im_notified_missing(round2 行在场) = %v, want 0", got)
	}
}

// TestCollectNotifyMissingPerRound(T18.7):SMTP 通道同修——sender 去重
// 同为按轮(sender.go $.round),遮蔽病同根同源;恢复「对账逐字镜像
// sender 去重」不变量。
func TestCollectNotifyMissingPerRound(t *testing.T) {
	r, m, d := newCollectReaper(t)
	s := seedApproval(t, d, "RUNNING", time.Hour, "escalate", true)
	if _, err := d.Exec(`UPDATE node_execution SET attempts=2, escalated_at = NOW(3) - INTERVAL 20 MINUTE
	  WHERE id=?`, s.nodeExecID); err != nil {
		t.Fatalf("seed round-2 escalation: %v", err)
	}
	seedNotifyAuditRow(t, d, s.execID, s.nodeExecID, "approval.notified", 1)
	if err := r.collectMetrics(context.Background()); err != nil {
		t.Fatalf("collectMetrics: %v", err)
	}
	if got := testutil.ToFloat64(m.ApprovalNotifyMissing); got != 1 {
		t.Errorf("approval_notify_missing(round1 行遮蔽 round2) = %v, want 1(分轮对账)", got)
	}
	seedNotifyAuditRow(t, d, s.execID, s.nodeExecID, "approval.notified", 2)
	if err := r.collectMetrics(context.Background()); err != nil {
		t.Fatalf("collectMetrics(2): %v", err)
	}
	if got := testutil.ToFloat64(m.ApprovalNotifyMissing); got != 0 {
		t.Errorf("approval_notify_missing(round2 行在场) = %v, want 0", got)
	}
}

// TestCollectIMNotifyResumedNodeLegacyMode(T18.7 假阳性守卫):ResumeNode
// 不清 escalated_at(engine 现实,nodecas.go)——决策续跑后节点已非
// WAITING_APPROVAL,本轮次已不可恢复(attempts 已 +1),对账必须回落
// 任意轮语义:旧轮行满足即不计缺失;且非 waiting 候选不被裁掉(零行
// 仍计丢失 = 未窄化)。
func TestCollectIMNotifyResumedNodeLegacyMode(t *testing.T) {
	r, m, d := newCollectReaper(t)
	s := seedApproval(t, d, "RUNNING", time.Hour, "escalate", true)
	if _, err := d.Exec(`UPDATE node_execution SET escalated_at = NOW(3) - INTERVAL 20 MINUTE
	  WHERE id=?`, s.nodeExecID); err != nil {
		t.Fatalf("backdate escalated_at: %v", err)
	}
	seedNotifyAuditRow(t, d, s.execID, s.nodeExecID, "approval.im_notified", 1)
	// 模拟 ResumeNode 后残留:EXECUTING + attempts 已进,escalated_at 仍在。
	if _, err := d.Exec(`UPDATE node_execution SET status='EXECUTING', attempts=2 WHERE id=?`, s.nodeExecID); err != nil {
		t.Fatalf("simulate resume residue: %v", err)
	}
	if err := r.collectMetrics(context.Background()); err != nil {
		t.Fatalf("collectMetrics: %v", err)
	}
	if got := testutil.ToFloat64(m.IMNotifyMissing); got != 0 {
		t.Errorf("approval_im_notified_missing(续跑残留任意轮语义) = %v, want 0(防假阳性)", got)
	}
	// 零行 ≠ 不对账:非 waiting 候选的绝对丢失仍要计入(未窄化锚)。
	if _, err := d.Exec(`DELETE FROM audit_log WHERE execution_id=? AND event_type='approval.im_notified'`, s.execID); err != nil {
		t.Fatalf("drop notified rows: %v", err)
	}
	if err := r.collectMetrics(context.Background()); err != nil {
		t.Fatalf("collectMetrics(2): %v", err)
	}
	if got := testutil.ToFloat64(m.IMNotifyMissing); got != 1 {
		t.Errorf("approval_im_notified_missing(非 waiting 零行) = %v, want 1(候选未窄化)", got)
	}
}

// TestCollectAuditPartitionGauges(T20.7):sqltest 前滚态下水位余量 ≥ +7
// (janitor 满格形态,终界 today+8,≥7 留给 DB 端午夜翻转一天的容差);
// p_min 兜底分区零行(无时钟漂移写入)。水位 gauge 是 janitor 停摆的唯一
// 运行期显影面,跌破告警线(alerts.yml <3)早于 api/worker preflight
// 拒启(水位 ≤0)——这里是采集正确性锚。
func TestCollectAuditPartitionGauges(t *testing.T) {
	r, m, d := newCollectReaper(t)
	if err := r.collectMetrics(context.Background()); err != nil {
		t.Fatalf("collectMetrics: %v", err)
	}
	if got := testutil.ToFloat64(m.AuditPartitionWatermarkDays); got < 7 {
		t.Errorf("audit_partition_watermark_days = %v, want >= 7 (sqltest roll-forward tops through today+7)", got)
	}
	if got := testutil.ToFloat64(m.AuditPminRows); got != 0 {
		t.Errorf("audit_pmin_rows = %v, want 0 (no pre-2026 rows yet)", got)
	}
	// p_min 正例(对抗审计:0 期望与未 Set 的零值重合,无杀伤力):直插
	// 一条 exec_date=2025-06-01 的审计行(0001:268 p_min 边界 <2026-01-01
	// 必落 p_min),断言计数为 1。root 测试账号 + TestMain 已有 audit_log
	// 清扫先例,不违 append-only 纪律。
	id := nextID(t)
	if _, err := d.Exec(`INSERT INTO audit_log
	  (id, exec_date, event_at, event_type, execution_id, actor_type, actor_id, payload)
	  VALUES (?, '2025-06-01', NOW(3), 'node.state_changed', ?, 'worker', 'w1', '{}')`,
		id, nextID(t)); err != nil {
		t.Fatalf("seed p_min row: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM audit_log WHERE id=?`, id) })
	if err := r.collectMetrics(context.Background()); err != nil {
		t.Fatalf("collectMetrics(2): %v", err)
	}
	if got := testutil.ToFloat64(m.AuditPminRows); got != 1 {
		t.Errorf("audit_pmin_rows = %v, want 1 (p_min row visible)", got)
	}
}
