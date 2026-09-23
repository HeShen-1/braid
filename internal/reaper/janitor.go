package reaper

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"braid/internal/engine"
)

// audit 保留期外的 outbox DISPATCHED 清理窗口(DDL §5 注释钉死 7 天,不暴露
// env:与 outbox 表契约同源)。
const outboxRetention = 7 * 24 * time.Hour

// janitor 单 tick 的行/分区上限(量控;批间 100ms 让出复制/缓冲池水位)。
const (
	outboxRoundCap   = 10000
	janitorBatchGap  = 100 * time.Millisecond
	ddlLockWaitSecs  = 5
	partitionHorizon = 7 // 预建天数:保持 ≥7 天未来分区(验收口径)
)

// janitorLoop 独立 goroutine:audit_log 分区水位 + outbox DISPATCHED 清理。
// 与扫描循环隔离(攻击 8/12 吸收):分区 DDL 的 MDL 等待被 lock_wait_timeout
// 封顶,超时弃轮,永不冻结扫描 tick。
func (r *Reaper) janitorLoop(ctx context.Context) {
	if err := r.janitorOnce(ctx); err != nil {
		r.log.Error("janitor.round_failed", "err", err)
	}
	t := time.NewTicker(r.cfg.JanitorInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.janitorOnce(ctx); err != nil {
				r.log.Error("janitor.round_failed", "err", err)
			}
		}
	}
}

// janitorOnce 一轮维护:专用连接 + 会话级 MDL 等待封顶;分区作业与 outbox
// 清理错误隔离(前者失败只记日志,不阻断后者)。DDL 全程 autocommit
// (SET SESSION 后每条 ALTER 独立提交),不持行锁、不走 Transient 重试。
func (r *Reaper) janitorOnce(ctx context.Context) error {
	conn, err := r.deps.DB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reaper: janitor conn: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx,
		fmt.Sprintf("SET SESSION lock_wait_timeout=%d", ddlLockWaitSecs)); err != nil {
		return fmt.Errorf("reaper: janitor lock_wait_timeout: %w", err)
	}
	if err := r.ensurePartitions(ctx, conn); err != nil {
		r.log.Error("janitor.partitions_failed", "err", err)
	}
	return r.purgeOutbox(ctx, conn)
}

// partitionRow 是 INFORMATION_SCHEMA 水位行。
type partitionRow struct {
	name  string
	bound string // PARTITION_DESCRIPTION:RANGE 上界(带引号,normalize 剥离)
}

// normalizeBound 剥离 PARTITION_DESCRIPTION 的类型字面量引号
// (RANGE COLUMNS 分区返回 '2026-10-05' 含单引号)。
func normalizeBound(raw string) string {
	return strings.Trim(raw, "' \t")
}

// ensurePartitions 维持 audit_log 日分区水位:预建至 today+7(不设 MAXVALUE
// 是锁定决策——越界 1526 响亮失败 = 受监控条件);回收超保留期分区(先过
// 活执行守卫,保留期语义 bends 为 ≥ retention,攻击 13 吸收)。
func (r *Reaper) ensurePartitions(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `
SELECT PARTITION_NAME, PARTITION_DESCRIPTION
  FROM INFORMATION_SCHEMA.PARTITIONS
 WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='audit_log'`)
	if err != nil {
		return fmt.Errorf("reaper: read partitions: %w", err)
	}
	var parts []partitionRow
	err = func() error {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var p partitionRow
			if err := rows.Scan(&p.name, &p.bound); err != nil {
				return fmt.Errorf("reaper: scan partition row: %w", err)
			}
			p.bound = normalizeBound(p.bound)
			parts = append(parts, p)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("reaper: read partitions rows: %w", err)
		}
		return nil
	}()
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		return errors.New("reaper: audit_log has no partitions (non-partitioned table?)")
	}

	var today, maxBound time.Time
	if err := conn.QueryRowContext(ctx, `SELECT CAST(NOW(3) AS DATE)`).Scan(&today); err != nil {
		return fmt.Errorf("reaper: read db date: %w", err)
	}
	for _, p := range parts {
		if d, err := time.ParseInLocation("2006-01-02", p.bound, time.UTC); err == nil && d.After(maxBound) {
			maxBound = d
		}
	}

	// 预建:日期升序逐条追加(RANGE 上界必须严格递增,中途失败即停链——
	// 后续日因上界不递增必同败)。容忍 {1517 duplicate, 1493 non-increasing}
	// (攻击 15 吸收:并发/时序交错下的良性收敛),复读确认后继续。
	horizon := today.AddDate(0, 0, partitionHorizon)
	for d := maxBound; !d.After(horizon); d = d.AddDate(0, 0, 1) {
		next := d.AddDate(0, 0, 1)
		_, err := conn.ExecContext(ctx, fmt.Sprintf(
			"ALTER TABLE audit_log ADD PARTITION (PARTITION p%s VALUES LESS THAN ('%s'))",
			d.Format("20060102"), next.Format("2006-01-02")))
		if err == nil {
			r.log.Info("audit.partition_added", "partition", "p"+d.Format("20060102"))
			continue
		}
		if me, ok := mysqlErr(err); ok && (me.Number == 1517 || me.Number == 1493) {
			// 复读确认:目标边界已存在/覆盖已满足 → 良性收敛。
			var cnt int
			if qerr := conn.QueryRowContext(ctx, `
SELECT COUNT(*) FROM INFORMATION_SCHEMA.PARTITIONS
 WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='audit_log'
   AND PARTITION_DESCRIPTION=?`, "'"+next.Format("2006-01-02")+"'").Scan(&cnt); qerr == nil && cnt > 0 {
				r.log.Info("audit.partition_add_converged", "boundary", next.Format("2006-01-02"))
				continue
			}
			return fmt.Errorf("reaper: add partition %s: %w", next.Format("2006-01-02"), err)
		}
		return fmt.Errorf("reaper: add partition (bound %s): %w", next.Format("2006-01-02"), err)
	}

	// 回收:边界 ≤ today−retention 且非 p_min(时钟漂移兜底,永不 DROP)。
	dropBefore := today.Add(-r.cfg.AuditRetention)
	for _, p := range parts {
		if p.name == "p_min" {
			continue
		}
		bound, err := time.ParseInLocation("2006-01-02", p.bound, time.UTC)
		if err != nil {
			continue // 非日期边界(未来形态),不动
		}
		if bound.After(dropBefore) {
			continue
		}
		// 活执行守卫:非终态执行可能仍写该分区(exec_date = DATE(created_at)),
		// 命中则本轮跳过 + warn——长寿命执行钉住保留期是合法语义。
		var live int
		if err := conn.QueryRowContext(ctx, `
SELECT 1 FROM workflow_execution
 WHERE status IN ('PENDING','RUNNING','WAITING_APPROVAL') AND created_at < ? LIMIT 1`,
			bound.Format("2006-01-02")).Scan(&live); err == nil {
			r.log.Warn("audit.partition_drop_deferred_live_exec",
				"partition", p.name, "boundary", p.bound,
				"hint", "non-terminal execution predates boundary; retention bends")
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("reaper: live exec guard: %w", err)
		}
		if _, err := conn.ExecContext(ctx,
			fmt.Sprintf("ALTER TABLE audit_log DROP PARTITION %s", p.name)); err != nil {
			if me, ok := mysqlErr(err); ok && me.Number == 1507 {
				continue // 已被他方 DROP:良性收敛
			}
			return fmt.Errorf("reaper: drop partition %s: %w", p.name, err)
		}
		r.log.Info("audit.partition_dropped", "partition", p.name)
	}
	return nil
}

// purgeOutbox 批删超保留期的 DISPATCHED 行。双列谓词(available_at 保守
// 超集 + dispatched_at 精滤,防「长退避后新近投出」行被早删);无 ORDER BY
// (攻击 16 吸收:大批量首跑免 filesort,批沿索引序自然推进;ROW binlog 下
// 复制安全)。DEAD/PENDING 永不清(T6.4 告警 + T7.4 重放依赖)。
func (r *Reaper) purgeOutbox(ctx context.Context, conn *sql.Conn) error {
	total := 0
	for {
		res, err := conn.ExecContext(ctx, `
DELETE FROM outbox
 WHERE status='DISPATCHED'
   AND available_at  < NOW(3) - INTERVAL ? MICROSECOND
   AND dispatched_at < NOW(3) - INTERVAL ? MICROSECOND
 LIMIT ?`,
			outboxRetention.Microseconds(), outboxRetention.Microseconds(), r.cfg.OutboxBatch)
		if err != nil {
			if engine.Transient(err) {
				return nil // 下轮再清:清理是收敛性作业,不重试单轮
			}
			return fmt.Errorf("reaper: purge outbox: %w", err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
		if n < int64(r.cfg.OutboxBatch) || total >= outboxRoundCap {
			break
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(janitorBatchGap):
		}
	}
	if total > 0 {
		r.log.Info("outbox.purged", "rows", total)
	}
	return nil
}

// mysqlErr 解包驱动错误码(容忍集判定用)。
func mysqlErr(err error) (*mysql.MySQLError, bool) {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me, true
	}
	return nil, false
}
