package reaper

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"go.opentelemetry.io/otel/codes"

	"braid/internal/engine"
)

// audit 保留期外的 outbox DISPATCHED 清理窗口(DDL §5 注释钉死 7 天,不暴露
// env:与 outbox 表契约同源)。
const outboxRetention = 7 * 24 * time.Hour

// janitor 单 tick 的行/分区上限(量控;批间 100ms 让出复制/缓冲池水位)。
const (
	janitorRoundCap  = 10000 // 单清理作业的单轮行上限(outbox/login_audit/login_throttle 各自独立计,node_attempt 走专属 nodeAttemptRoundCap;软上限——判定在整批 DELETE 后,至多超出一批)
	loginAuditBatch  = 500   // login_audit 单批 DELETE 上限(量级被限流钳制,不设旋钮)
	janitorBatchGap  = 100 * time.Millisecond
	ddlLockWaitSecs  = 5
	partitionHorizon = 7 // 预建天数:保持 ≥7 天未来分区(验收口径)

	// loginThrottleHorizon 是 login_throttle 行的 janitor 回收期:常量非
	// 旋钮(闲置衰减是应用层判定、纪元 = window_start 的计数写入时刻,
	// 与 janitor 无关;janitor 只兜「distinct 键长尾」的表体积——超龄
	// 未触达行 = 死重,30d 是体积兜底不是语义前提,D-t113 #2「行空间由
	// distinct 键限界」的执行面)。
	loginThrottleHorizon = 30 * 24 * time.Hour
	// loginThrottleBatch 单批 DELETE 上限(镜像 loginAuditBatch)。
	loginThrottleBatch = 500

	// nodeAttemptBatch 是 node_attempt 留档清理的单批 DELETE 上限(T18.6,
	// 镜像 loginAuditBatch:旋钮只有保留期,批量与轮上限由量控常量兜)。
	nodeAttemptBatch = 500

	// nodeAttemptRoundCap 是 node_attempt 留档清理的专属单轮上限(T18.6,
	// 不共用 janitorRoundCap:该面摄入 = 无界业务吞吐——CAPACITY §7 生产
	// 轮询形态帽 6.4 nodes/s ≈ 55.3 万行/天,共用 1 万 × 24 轮 = 24 万/天
	// 清理帽 < 摄入帽,满帽持续负载下保留期无界漂移(对抗审计 2×HIGH);
	// 2.5 万 × 24 轮 = 60 万/天 > 该形态摄入帽 9% 余量,盈亏平衡 6.94 行/s。
	// §7 压缩轮询形态净帽 29.5 nodes/s(≈ 255 万行/天)单副本即超本帽
	// 4.3×——扩容口径见 CAPACITY §9 唤醒触发。轮活跃代价 ≈ 50 批 ×
	// (DELETE ~10ms + 批隙 100ms) ≈ 5.5s。
	nodeAttemptRoundCap = 25000
)

// janitorLoop 独立 goroutine:audit_log 分区水位 + outbox DISPATCHED 清理 +
// login_audit 保留期清理(T7.1 接线,migration 0005 登记)+ login_throttle
// 死重回收(T11.3)+ node_attempt 留档保留期清理(T18.6)。
// 与扫描循环隔离(攻击 8/12 吸收):分区 DDL 的 MDL 等待被 lock_wait_timeout
// 封顶,超时弃轮,永不冻结扫描 tick。
func (r *Reaper) janitorLoop(ctx context.Context, token string) {
	r.gatedRound(ctx, leaderKeyJanitor, token, 2*r.cfg.JanitorInterval, "janitor",
		r.janitorOnce)
	t := time.NewTicker(r.cfg.JanitorInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.gatedRound(ctx, leaderKeyJanitor, token, 2*r.cfg.JanitorInterval, "janitor",
				r.janitorOnce)
		}
	}
}

// janitorOnce 一轮维护:专用连接 + 会话级 MDL 等待封顶;分区作业与 outbox/
// login_audit/login_throttle/node_attempt 清理错误隔离(前者失败只记日志,
// 不阻断清理)。DDL 全程
// autocommit(SET SESSION 后每条 ALTER 独立提交),不持行锁、不走 Transient
// 重试。span 形态同 RunOnce(D-t121 #4:计时 span 不进发射 ctx)。
func (r *Reaper) janitorOnce(ctx context.Context) error {
	if r.deps.Tracer == nil {
		return r.doJanitor(ctx)
	}
	_, span := r.deps.Tracer.Start(ctx, "reaper.janitor")
	defer span.End()
	err := r.doJanitor(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

func (r *Reaper) doJanitor(ctx context.Context) error {
	conn, err := r.deps.DB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reaper: janitor conn: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx,
		fmt.Sprintf("SET SESSION lock_wait_timeout=%d", ddlLockWaitSecs)); err != nil {
		return fmt.Errorf("reaper: janitor lock_wait_timeout: %w", err)
	}
	// 行锁等待同点位封顶(algo 补齐审查:上一变量只封 MDL;janitor 首个
	// 持锁触碰活热路径的面是 node_attempt 留档清理,其 next-key/S 锁若被
	// 长事务 X 锁阻塞,默认 innodb_lock_wait_timeout=50s 会把等待传导给
	// 同期 claim/心跳/收口——5s 封顶 + Transient 让位,下轮再清)。
	if _, err := conn.ExecContext(ctx,
		fmt.Sprintf("SET SESSION innodb_lock_wait_timeout=%d", ddlLockWaitSecs)); err != nil {
		return fmt.Errorf("reaper: janitor innodb_lock_wait_timeout: %w", err)
	}
	if err := r.ensurePartitions(ctx, conn); err != nil {
		r.log.Error("janitor.partitions_failed", "err", err)
	}
	return errors.Join(r.purgeOutbox(ctx, conn), r.purgeLoginAudit(ctx, conn), r.purgeLoginThrottle(ctx, conn),
		r.purgeNodeAttempt(ctx, conn))
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

	// T9.3 级联解除链义务(每轮独立执行,不挂在本轮 DROP 结果上——对抗
	// 审计 MEDIUM 修:DROP 成功后 DELETE 失败的分区下轮不再出现在扫描集,
	// 挂靠式级联永不重试,滞留头会被校验按「行丢失」误闩):证据窗口随
	// 保留期消亡 ⇒ 对应执行的 chain_heads 头随之消亡。幂等谓词每轮重放,
	// 自然覆盖漏网轮次。终态过滤镜像分区 DROP 的活执行守卫(活执行的头
	// 不能删:其分区被守卫保留,删头会让下一次 Append 以 prev='' 重开链,
	// 旧链行全部沦为未装配孤儿 ⇒ 误闩)。两害同防:水位滞后旧头误闩 +
	// 头表随执行数无限增长(执行行无清理路径,恒在场作 join 锚;exec_date
	// = DATE(created_at) 单源)。
	if _, err := conn.ExecContext(ctx, `
DELETE ch FROM chain_heads ch
  JOIN workflow_execution we ON we.id = ch.execution_id
 WHERE we.created_at < ?
   AND we.status NOT IN ('PENDING','RUNNING','WAITING_APPROVAL')`,
		dropBefore.Format("2006-01-02")); err != nil {
		return fmt.Errorf("reaper: cascade chain_heads: %w", err)
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
		if n < int64(r.cfg.OutboxBatch) || total >= janitorRoundCap {
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

// purgeLoginAudit 批删超保留期的 login_audit 行(migration 0005 登记的
// 保留期接线,T7.1 落地)。谓词沿 idx_login_audit_event_at;单批上限 =
// loginAuditBatch 常量(量级被登录限流钳制,不新增旋钮);Transient 容忍
// 与轮上限语义同 purgeOutbox。授权契约:reaper 账号对 login_audit 持
// DELETE(api 账号仍 INSERT/SELECT only;deploy/README.md 授权矩阵)。
func (r *Reaper) purgeLoginAudit(ctx context.Context, conn *sql.Conn) error {
	total := 0
	for {
		res, err := conn.ExecContext(ctx, `
DELETE FROM login_audit
 WHERE event_at < NOW(3) - INTERVAL ? MICROSECOND
 LIMIT ?`,
			r.cfg.LoginAuditRetention.Microseconds(), loginAuditBatch)
		if err != nil {
			if engine.Transient(err) {
				return nil // 下轮再清:清理是收敛性作业,不重试单轮
			}
			return fmt.Errorf("reaper: purge login_audit: %w", err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
		if n < int64(loginAuditBatch) || total >= janitorRoundCap {
			break
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(janitorBatchGap):
		}
	}
	if total > 0 {
		r.log.Info("login_audit.purged", "rows", total)
	}
	return nil
}

// purgeLoginThrottle 批删超龄的 login_throttle 防御行(0010,T11.3):
// 活性闲置超过 loginThrottleHorizon 即死重,回收兜表体积。**语义守卫:
// 锁定期未满(locked_until ≥ NOW)的行永不回收**——旋钮只有下界校验,
// LoginLockout > horizon 的(合法但荒谬)配置下,无守卫的回收会把仍在
// 锁定期的行删掉、下次尝试重建 count=0 全新行 = 锁定提前解除(审计
// MEDIUM);ip 行的极端宽窗口(LoginRateWindow > horizon,同样荒谬)
// 闲置回收 = 窗口计数清零重启,方向是防御面归零非解锁,登记接受。
// 谓词沿 idx_login_throttle_updated;Transient 容忍与轮上限语义同
// purgeOutbox/purgeLoginAudit。授权契约:
// reaper 账号对 login_throttle 持 SELECT+DELETE(DELETE 谓词列需 SELECT,
// grants.sh 注记同 login_audit 先例;api 账号 SELECT/INSERT/UPDATE 无
// DELETE——成功重置走 UPDATE,D-t113 #2)。
func (r *Reaper) purgeLoginThrottle(ctx context.Context, conn *sql.Conn) error {
	total := 0
	for {
		res, err := conn.ExecContext(ctx, `
DELETE FROM login_throttle
 WHERE updated_at < NOW(3) - INTERVAL ? MICROSECOND
   AND (locked_until IS NULL OR locked_until < NOW(3))
 LIMIT ?`,
			loginThrottleHorizon.Microseconds(), loginThrottleBatch)
		if err != nil {
			if engine.Transient(err) {
				return nil // 下轮再清:清理是收敛性作业,不重试单轮
			}
			return fmt.Errorf("reaper: purge login_throttle: %w", err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
		if n < int64(loginThrottleBatch) || total >= janitorRoundCap {
			break
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(janitorBatchGap):
		}
	}
	if total > 0 {
		r.log.Info("login_throttle.purged", "rows", total)
	}
	return nil
}

// purgeNodeAttempt 批删超保留期的终态 node_attempt 留档行(T18.6,
// D-t186:janitor 第五面)。三重守卫:
//  1. status IN 终态集:RUNNING 永不删——活 attempt 的判死是扫描②的
//     CAS 处置面,清理作业不越权代笔回收;
//  2. heartbeat_at 时钟:全行 NOT NULL、终态即冻结(收口是一次终写,
//     终态后禁改),偏斜 ≤ 心跳间隔+DeadGrace ≈ 分钟级(相对收口钟最多
//     早删一个偏斜窗,30d 量级下无害);谓词骑 idx_attempt_status_heartbeat,
//     零 DDL;
//  3. 活执行守卫(镜像 audit 分区守卫先例):所属 execution 非终态则
//     本轮跳过——长审批执行钉住留档是合法语义,保留期为活执行弯曲。
//
// 锁面(T18.6 是 janitor 首个持锁触碰活热路径表的面):DELETE 段扫描的
// next-key 锁与 NOT EXISTS 对 workflow_execution 的 S 锁均为批内毫秒级
// 持有(单批 autocommit + 批隙 100ms),与 claim/心跳/收口的竞争是瞬态
// 等待非死锁环——锁序单向(本面:attempt 段锁 → we S 锁;worker:
// attempt X 锁 → we X 锁);被长事务 X 锁阻塞时行锁等待由会话级
// innodb_lock_wait_timeout=5 封顶(doJanitor,MDL 闸 lock_wait_timeout
// 分立),1205 走 Transient 让位,下轮再清。
//
// 删除纪律红线(D-t186):本清理是保留期治理非审计删除——audit_log 分区
// 照旧独立(90d,usage/cost 双录兜底合规链);t9.2 双路径追溯的状态路径
// 在保留期外变薄,审计路径不受影响。回滚机制不读终态 attempt 行
// (rollback 只锁 RUNNING 收口),保留期即留档证据支持窗。授权契约:
// reaper 账号对 node_attempt 补 DELETE(grants.sh;谓词列走既有 SELECT)。
// Transient 容忍同 purgeOutbox;轮上限走专属 nodeAttemptRoundCap(摄入 =
// 无界业务吞吐,与限流钳制/事件量有界的既有三面异质,见常量注)。
func (r *Reaper) purgeNodeAttempt(ctx context.Context, conn *sql.Conn) error {
	total := 0
	for {
		res, err := conn.ExecContext(ctx, `
DELETE FROM node_attempt
 WHERE status IN ('SUCCEEDED','FAILED','DEAD')
   AND heartbeat_at < NOW(3) - INTERVAL ? MICROSECOND
   AND NOT EXISTS (SELECT 1 FROM workflow_execution we
                    WHERE we.id = node_attempt.execution_id
                      AND we.status IN ('PENDING','RUNNING','WAITING_APPROVAL'))
 LIMIT ?`,
			r.cfg.NodeAttemptRetention.Microseconds(), nodeAttemptBatch)
		if err != nil {
			if engine.Transient(err) {
				return nil // 下轮再清:清理是收敛性作业,不重试单轮
			}
			return fmt.Errorf("reaper: purge node_attempt: %w", err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
		if n < int64(nodeAttemptBatch) || total >= nodeAttemptRoundCap {
			break
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(janitorBatchGap):
		}
	}
	if total > 0 {
		r.log.Info("node_attempt.purged", "rows", total)
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
