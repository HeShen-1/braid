package reaper

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"braid/internal/metrics"
)

// notifyReconcileGrace 是 escalated-vs-notified 对账的宽限窗:覆盖 asynq
// MaxRetry=1 重投 + relay 退避穷尽(~11.8min)+ SMTP 单次 Timeout。窗内
// 缺 notified 行是正常在途,窗外才算丢失(D-t45 #1)。
const notifyReconcileGrace = 15 * time.Minute

// notifyReconcileCap 是对账候选的硬上限(防御性:gauge 采集是观测路径,
// 不为病态行数付无界循环;触顶即少计,下轮照常)。
const notifyReconcileCap = 500

// collectMetrics 是每 tick 的 gauge 快照采集(T6.4): backlog/aging/
// dead-letter/对账面全部 DB 真源快照。设计口径:
//   - 只读普通 SELECT,无锁无事务(与扫描候选集同判——D-t25 #1);
//   - 单 gauge 失败记日志继续,不拖垮其余(观测面错误隔离);
//   - resume-stuck 谓词只取 scan ⑤a 的「已决票」半边(已升级未决票的
//     deadline 是 Q9 刻意保留的,计入 = 把正常审批时延当滞留),外加
//     活执行过滤(孤儿由 ⑤b cancel,不构成滞留信号);
//   - escalated-vs-notified 从 escalated_at 驱动、NOT EXISTS 反查按
//     exec_date 常量点查(audit_log 按执行日分区,行值日期无法整表裁剪,
//     镜像 sender.go 去重查形态)。
func (r *Reaper) collectMetrics(ctx context.Context) error {
	if r.m == nil {
		return nil
	}
	r.collectAuditPending(ctx)
	r.collectOutbox(ctx)
	r.collectApprovalBacklog(ctx)
	r.collectStuckCandidates(ctx)
	r.collectResumeStuck(ctx)
	r.collectNotifyMissing(ctx)
	r.collectIMNotifyMissing(ctx)
	return nil
}

func (r *Reaper) collectAuditPending(ctx context.Context) {
	var rows int
	var oldest float64
	if err := r.deps.DB.QueryRowContext(ctx, `
SELECT COUNT(*), IFNULL(TIMESTAMPDIFF(SECOND, MIN(created_at), NOW(3)), 0)
  FROM audit_pending`).Scan(&rows, &oldest); err != nil && !errors.Is(err, sql.ErrNoRows) {
		r.log.Warn("metrics.audit_pending", "err", err)
		return
	}
	r.m.AuditPendingRows.Set(float64(rows))
	r.m.AuditPendingOldest.Set(oldest)
}

func (r *Reaper) collectOutbox(ctx context.Context) {
	rs, err := r.deps.DB.QueryContext(ctx, `
SELECT status, COUNT(*), IFNULL(TIMESTAMPDIFF(SECOND, MIN(created_at), NOW(3)), 0)
  FROM outbox WHERE status IN ('PENDING','DEAD') GROUP BY status`)
	if err != nil {
		r.log.Warn("metrics.outbox", "err", err)
		return
	}
	defer func() { _ = rs.Close() }() //nolint:errcheck // 只读探测,关闭错误无处置义
	counts := map[string][2]float64{}
	for rs.Next() {
		var status string
		var rows int
		var oldest float64
		if err := rs.Scan(&status, &rows, &oldest); err != nil {
			r.log.Warn("metrics.outbox scan", "err", err)
			return
		}
		counts[status] = [2]float64{float64(rows), oldest}
	}
	if err := rs.Err(); err != nil {
		r.log.Warn("metrics.outbox rows", "err", err)
		return
	}
	for _, status := range []string{"PENDING", "DEAD"} {
		label := metrics.OutboxPending
		if status == "DEAD" {
			label = metrics.OutboxDead
		}
		v := counts[status]
		r.m.OutboxRows.WithLabelValues(label).Set(v[0])
		r.m.OutboxOldest.WithLabelValues(label).Set(v[1])
	}
}

func (r *Reaper) collectApprovalBacklog(ctx context.Context) {
	var rows int
	var oldest float64
	if err := r.deps.DB.QueryRowContext(ctx, `
SELECT COUNT(*), IFNULL(TIMESTAMPDIFF(SECOND, MIN(ne.status_changed_at), NOW(3)), 0)
  FROM node_execution ne
  JOIN workflow_execution we ON we.id = ne.execution_id
 WHERE ne.status='WAITING_APPROVAL' AND we.status IN ('RUNNING','WAITING_APPROVAL')`).
		Scan(&rows, &oldest); err != nil {
		r.log.Warn("metrics.approval_backlog", "err", err)
		return
	}
	r.m.ApprovalBacklog.Set(float64(rows))
	r.m.ApprovalOldest.Set(oldest)
}

// collectStuckCandidates 报各扫描的候选存量(gauge),与处置计数(counter)
// 互补:gauge 是水位,counter 是流量;「卡死节点」告警消费水位。
func (r *Reaper) collectStuckCandidates(ctx context.Context) {
	probe := func(scan, query string, grace time.Duration) {
		var n int
		if err := r.deps.DB.QueryRowContext(ctx, query, grace.Microseconds()).
			Scan(&n); err != nil {
			r.log.Warn("metrics.stuck_candidates", "scan", scan, "err", err)
			return
		}
		r.m.StuckCandidates.WithLabelValues(scan).Set(float64(n))
	}
	probe(metrics.ScanAttempt, `
SELECT COUNT(*) FROM node_attempt
 WHERE status='RUNNING' AND heartbeat_at < NOW(3) - INTERVAL ? MICROSECOND`, r.cfg.DeadGrace)
	probe(metrics.ScanNodeDeadline, `
SELECT COUNT(*) FROM node_execution
 WHERE status='EXECUTING' AND deadline_at < NOW(3) - INTERVAL ? MICROSECOND`, r.cfg.ScanInterval)
	probe(metrics.ScanExecStale, `
SELECT COUNT(*) FROM workflow_execution
 WHERE status='RUNNING' AND last_activity_at < NOW(3) - INTERVAL ? MICROSECOND`, r.cfg.ExecStale)
}

func (r *Reaper) collectResumeStuck(ctx context.Context) {
	var n int
	// 谓词只取 ⑤a 的「已决票」半边(对抗审计 3/3):已升级未决票的
	// deadline 是 Q9 刻意保留的(等人决定,永不自动通过),计入即把
	// 正常审批时延当滞留——CRITICAL 告警常燃会淹没真滞留。已决票 +
	// 超阈 = 续跑事件死的信号,与扫描 ⑤a 的修复路径同一人群。
	if err := r.deps.DB.QueryRowContext(ctx, `
SELECT COUNT(*)
  FROM node_execution ne
  JOIN workflow_execution we ON we.id = ne.execution_id
 WHERE ne.status='WAITING_APPROVAL'
   AND ne.deadline_at < NOW(3) - INTERVAL ? MICROSECOND
   AND EXISTS(SELECT 1 FROM approval_decision d
               WHERE d.node_execution_id = ne.id
                 AND d.round = ne.attempts)
   AND we.status IN ('RUNNING','WAITING_APPROVAL')`,
		r.cfg.ScanInterval.Microseconds()).Scan(&n); err != nil {
		r.log.Warn("metrics.resume_stuck", "err", err)
		return
	}
	r.m.ResumeStuck.Set(float64(n))
}

// collectNotifyMissing 是 escalated-vs-notified 对账(D-t45 #1):升级意图
// (escalated_at / approval.escalated 行)有、送达(approval.notified 行)
// 缺且过宽限 = 通知丢失——relay 耗尽转 DEAD / worker archived 等成因的
// 统一观测信号。SMTP 未配置时 notified 设计性缺席,由消费侧
// braid_smtp_configured==1 门闸告警(D-t45 #3 假阳性防御)。
func (r *Reaper) collectNotifyMissing(ctx context.Context) {
	r.collectNotifyReconcile(ctx, "approval.notified", r.m.ApprovalNotifyMissing)
}

// collectIMNotifyMissing 是 IM 通道的同构对账(T17.4):escalated 有、
// approval.im_notified 审计行缺且过宽限 = 卡片送达缺口。IM 未配置时
// im_notified 设计性缺席,消费侧 braid_im_configured==1 门闸告警
// (metrics.go IMConfigured 注释预留的本任务面)。宽限窗复用
// notifyReconcileGrace(同一事件管道:asynq MaxRetry=1 + relay 退避 +
// webhook 单次 Timeout)。
func (r *Reaper) collectIMNotifyMissing(ctx context.Context) {
	r.collectNotifyReconcile(ctx, "approval.im_notified", r.m.IMNotifyMissing)
}

// collectNotifyReconcile 是双通道对账实体:notified/im_notified 的候选集
// (escalated_at 驱动)与逐候选分区点查(镜像 sender.go 去重查形态,
// idx_audit_exec 最左前缀入口)完全同构,仅 event_type 与落点 gauge 分立。
// T18.7 分轮收口(D-t174 已知边界:按 (exec,node) 不分轮会被早轮行遮蔽):
// sender 去重锚按轮(payload.$.round),对账点查必须同粒度——两模式:
//   - WAITING_APPROVAL 候选:不变量「escalated_at ∧ WAITING ⇒ 升级属当前
//     轮 = attempts」(MarkNodeEscalated 的 IS NULL 锚 + 轮推进必然离开
//     WAITING),点查按轮匹配——本轮卡片丢失不被早轮行遮蔽;
//   - 非 waiting 候选:ResumeNode 不清 escalated_at(engine 现实),决策
//     续跑后 attempts 已进、升级轮次不可恢复——保持任意轮语义(任意轮
//     行满足即不计;零行仍计,候选不窄化)。
func (r *Reaper) collectNotifyReconcile(ctx context.Context, eventType string, gauge prometheus.Gauge) {
	rs, err := r.deps.DB.QueryContext(ctx, `
SELECT ne.id, we.id, DATE_FORMAT(we.created_at, '%Y-%m-%d'), ne.attempts,
       ne.status='WAITING_APPROVAL'
  FROM node_execution ne
  JOIN workflow_execution we ON we.id = ne.execution_id
 WHERE ne.escalated_at IS NOT NULL
   AND ne.escalated_at < NOW(3) - INTERVAL ? MICROSECOND
   AND we.status IN ('RUNNING','WAITING_APPROVAL')
 LIMIT ?`, notifyReconcileGrace.Microseconds(), notifyReconcileCap)
	if err != nil {
		r.log.Warn("metrics.notify_missing candidates", "event_type", eventType, "err", err)
		return
	}
	type cand struct {
		id        uint64
		execID    uint64
		execDate  string
		attemptNo int
		waiting   bool
	}
	var cands []cand
	for rs.Next() {
		var c cand
		var d sql.NullString // DATE 直出字符串,不经 driver loc 解析(D-t26-F1 同判)
		if err := rs.Scan(&c.id, &c.execID, &d, &c.attemptNo, &c.waiting); err != nil {
			_ = rs.Close() //nolint:errcheck // 探测失败路径,关闭错误无处置义
			r.log.Warn("metrics.notify_missing scan", "err", err)
			return
		}
		if !d.Valid {
			continue
		}
		c.execDate = d.String
		cands = append(cands, c)
	}
	_ = rs.Close() //nolint:errcheck // 同上

	missing := 0
	for _, c := range cands {
		var one int
		// exec_date 常量点查 = 分区裁剪锚(镜像 sender.go 去重查)。
		// execution_id 谓词逐字镜像 sender 去重查:idx_audit_exec 最左前缀
		// 的入口(rev3 HIGH:漏掉它 = exec_date 只裁分区,node_execution_id
		// 无索引,每候选每 tick 全分区扫描——观测面在病理态自我放大)。
		query := `SELECT 1 FROM audit_log
 WHERE exec_date=? AND execution_id=? AND node_execution_id=? AND event_type=?`
		args := []any{c.execDate, c.execID, c.id, eventType}
		if c.waiting {
			// 轮谓词逐字镜像 im/sender 去重锚(JSON_EXTRACT '$.round' = ?)。
			query += ` AND JSON_EXTRACT(payload, '$.round') = ?`
			args = append(args, c.attemptNo)
		}
		query += ` LIMIT 1`
		err := r.deps.DB.QueryRowContext(ctx, query, args...).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			missing++
			continue
		}
		if err != nil {
			r.log.Warn("metrics.notify_missing probe", "event_type", eventType,
				"node_execution_id", c.id, "err", err)
			return
		}
	}
	gauge.Set(float64(missing))
}
