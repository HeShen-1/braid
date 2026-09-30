// 登录限流 + 账号锁定(T6.4 落地,T11.3 DB 锚定换装——D-2026-09-29-t113-
// auth-depth #2):username 连败锁定(u: 行)+ IP 固定窗口(ip: 行),状态
// 迁入 login_throttle 表(0010,防御状态簿记面,镜像 chain_heads「可
// UPDATE 簿记 ≠ 审计事实」哲学)。进程内实现(D-t64 已知边界「重启清零、
// 多副本各自计数」)整体退役;阈值跨副本精确 = 行锁单事务(锚 upsert →
// SELECT..FOR UPDATE → 判定 → 写回,D-t93 chain_heads 锚定形态)。
//
// 防枚举口径(逐字继承):
//   - 锁定计数键 = username 原样(域外/未知用户名同样计数同样锁)——
//     「连错 N 次后 429」对存在与不存在的用户名行为一致,无账号存在性
//     oracle;
//   - 锁定与限流共用同一响应形态(429 too_many_attempts),外部无法区分
//     「IP 窗口超限」与「账号锁定」——后者本身是用户名维度的事实泄露面。
//
// 失败语义:DB 故障 → error → 500(防御不可用 = 登录不可用,fail-closed;
// 认证读同一 DB,「DB 故障下登录本就不可用」无新 fail-open 面,D-t113
// #2)。nil DB(零栈桩构造)→ 防御跳过,仅测试面;生产 api 恒有 DB。
package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"braid/internal/engine"
)

// limiterKey 把 username 钳到有界键长('u:' 前缀 + 128 = VARCHAR(130)
// 键域;review HIGH 先例:自由输入做键 = 键本身就是放大面)。截断形与
// 审计列同限(128),域外超长变体折叠到同键反而收紧防御。
func limiterKey(username string) string { return auditUsername(username) }

// loginThrottle 是 DB 锚定的登录防御层(0010)。零值不可用,经
// newLoginThrottle 构造(Config 四旋钮驱动);now 注入测试时钟(全时间列
// app 侧写入,UTC 规整——与 NOW(3) 比较的 janitor 谓词同钟系)。
type loginThrottle struct {
	dbh *sql.DB
	cfg Config
	now func() time.Time
}

// 防御行键形(kind 与 0010 DDL ENUM 同步)。
const (
	throttleKindUsername = "username"
	throttleKindIP       = "ip"
)

func newLoginThrottle(cfg Config, dbh *sql.DB) *loginThrottle {
	return &loginThrottle{dbh: dbh, cfg: cfg, now: time.Now}
}

// throttleRow 是锁内读回的防御行快照。
type throttleRow struct {
	windowStart time.Time
	count       int
	lockedUntil sql.NullTime
	updatedAt   time.Time
}

// lockRow 锚定并锁行(D-t93 chain_heads 形态):INSERT..ON DUPLICATE
// no-op 自赋值保证行在场(双首写者竞态压成串行),随后 SELECT..FOR
// UPDATE 取行锁。返回快照供锁内判定。**只用于行创建被防御语义担保的
// 路径**:ip 行(每 IP 至多一行,窗口判定本身需要)与 record 的 u 行
// (登录判定已发生,行创建速率被前置 IP 窗口钳制)。
func (t *loginThrottle) lockRow(ctx context.Context, q DBTX, key, kind string, now time.Time) (throttleRow, error) {
	if _, err := q.ExecContext(ctx, `
INSERT INTO login_throttle (throttle_key, kind, window_start, count, locked_until, updated_at)
VALUES (?, ?, ?, 0, NULL, ?)
ON DUPLICATE KEY UPDATE count = count`,
		key, kind, now, now); err != nil {
		return throttleRow{}, fmt.Errorf("anchor %s: %w", key, err)
	}
	return t.lockExistingRow(ctx, q, key)
}

// lockExistingRow 只锁已在场的行(不锚插);无行 = sql.ErrNoRows,调用方
// 按防御语义解读(如 u 行不存在 = 未锁)。
func (t *loginThrottle) lockExistingRow(ctx context.Context, q DBTX, key string) (throttleRow, error) {
	var r throttleRow
	if err := q.QueryRowContext(ctx, `
SELECT window_start, count, locked_until, updated_at
  FROM login_throttle WHERE throttle_key = ? FOR UPDATE`,
		key).Scan(&r.windowStart, &r.count, &r.lockedUntil, &r.updatedAt); err != nil {
		return throttleRow{}, fmt.Errorf("lock %s: %w", key, err)
	}
	return r, nil
}

// checkIP 是 body 不可解析路径的 IP-only 判定(不触锁定表):坏 body 请求
// 同样消费 IP 窗口配额,防「malformed 绕过限流」(review HIGH 先例)。
func (t *loginThrottle) checkIP(ctx context.Context, ip string) (limited bool, err error) {
	if t == nil || t.dbh == nil {
		return false, nil // 零栈桩面:防御跳过(生产恒有 DB)
	}
	now := t.now().UTC()
	err = t.withTxRetry(ctx, func(q DBTX) error {
		var lerr error
		limited, lerr = t.bumpIPWindow(ctx, q, ip, now)
		return lerr
	})
	return limited, err
}

// bumpIPWindow 是 IP 固定窗口的单源推进(锁内调用):窗口过期重开 →
// 计数 +1 → 超限判定 → 写回。checkIP 与 check 共用,防防御逻辑两份
// 漂移(orch-review rev5 LOW)。
func (t *loginThrottle) bumpIPWindow(ctx context.Context, q DBTX, ip string, now time.Time) (bool, error) {
	r, err := t.lockRow(ctx, q, "ip:"+ip, throttleKindIP, now)
	if err != nil {
		return false, err
	}
	if now.Sub(r.windowStart) >= t.cfg.LoginRateWindow {
		r.windowStart, r.count = now, 0
	}
	r.count++
	limited := r.count > t.cfg.LoginRateLimit
	if _, err := q.ExecContext(ctx, `
UPDATE login_throttle SET window_start = ?, count = ?, updated_at = ? WHERE throttle_key = ?`,
		r.windowStart, r.count, now, "ip:"+ip); err != nil {
		return false, err
	}
	return limited, nil
}

// check 在认证之前判定(不付 bcrypt):本次请求计入 IP 固定窗口 + 读
// username 锁定态;返回 (locked, limited)。两者皆映射 429,语义区分只在
// 指标 label。**u 行只锁读不锚插**:无行 = 从未计数 = 未锁——u 行创建
// 收敛到 record(登录判定已发生),IP 窗口因此成为 u 行创建速率的真实
// 制动(审计 HIGH:认证前锚插则未认证 username 喷洒可绕过 IP 限流无界
// 灌表,每请求一张持久行,回收容量结构性追不上)。锁序恒 u:(若在
// 场)→ ip:(固定序 ⇒ api 事务两两无环;与 janitor DELETE 的交错死锁
// 由双端 Transient(1213) 重试兜底——锁序不覆盖 janitor 的 idx 扫描序)。
func (t *loginThrottle) check(ctx context.Context, ip, username string) (locked, limited bool, err error) {
	if t == nil || t.dbh == nil {
		return false, false, nil // 零栈桩面:防御跳过(生产恒有 DB)
	}
	now := t.now().UTC()
	ukey := "u:" + limiterKey(username)
	err = t.withTxRetry(ctx, func(q DBTX) error {
		u, err := t.lockExistingRow(ctx, q, ukey)
		switch {
		case err == nil:
			locked = u.lockedUntil.Valid && u.lockedUntil.Time.After(now)
			// u 行触达只写 updated_at(janitor 活性纪元),不写 window_start
			// (闲置衰减纪元 = 最近计数写入时刻,record 独占)——两纪元解耦
			// 是衰减在生产路径可达的前提(orch-review HIGH:check 也刷衰减
			// 纪元则同请求内 check→record 差值恒毫秒级,衰减永不可达;进程
			// 内实现 check 不写 lastSeen 同判)。
			if _, err := q.ExecContext(ctx, `
UPDATE login_throttle SET updated_at = ? WHERE throttle_key = ?`, now, ukey); err != nil {
				return err
			}
		case errors.Is(err, sql.ErrNoRows):
			locked = false // 无行 = 从未计数 = 未锁;不锚插(见函数注释)
		default:
			return err
		}

		var lerr error
		limited, lerr = t.bumpIPWindow(ctx, q, ip, now)
		return lerr
	})
	return locked, limited, err
}

// record 登录判定之后回填:成功重置失败计数(UPDATE 置零,非 DELETE——
// api 账号对 login_throttle 无 DELETE 授权,D-t113 #2 锁定);失败递增,
// 达到阈值即锁定 Lockout(锁定期满自动解锁——不设显式解锁面,攻击者
// 停手后账号自愈)。闲置衰减:上次**计数写入**(window_start 纪元)距令
// 超过 4×lockout 的失败计数重开(进程内 sweep 的锁内等价——纪元与
// check 的 janitor 活性触达解耦,见 check 注释)。best-effort:失败只记
// 日志(键面经 limiterKey 截断有界),登录判定不被簿记失败翻转
// (writeLoginAudit 同契约)——正因如此无错误返回,调用面不消费。
func (t *loginThrottle) record(ctx context.Context, username string, success bool) {
	if t == nil || t.dbh == nil {
		return // 零栈桩面
	}
	now := t.now().UTC()
	ukey := "u:" + limiterKey(username)
	err := t.withTxRetry(ctx, func(q DBTX) error {
		u, err := t.lockRow(ctx, q, ukey, throttleKindUsername, now)
		if err != nil {
			return err
		}
		if success {
			_, err := q.ExecContext(ctx, `
UPDATE login_throttle
   SET count = 0, locked_until = NULL, window_start = ?, updated_at = ? WHERE throttle_key = ?`,
				now, now, ukey)
			return err
		}
		if u.lockedUntil.Valid && u.lockedUntil.Time.After(now) {
			// 锁定窗内的失败不追加、不刷衰减纪元(锁定期满重开计数);
			// updated_at 触达仍记账(janitor 活性)。
			_, err := q.ExecContext(ctx, `
UPDATE login_throttle SET updated_at = ? WHERE throttle_key = ?`, now, ukey)
			return err
		}
		count := u.count + 1
		if now.Sub(u.windowStart) >= t.acctIdleHorizon() {
			count = 1 // 闲置衰减:停滞攻击留下的旧计数不累积
		}
		if count >= t.cfg.LoginMaxFailures {
			_, err := q.ExecContext(ctx, `
UPDATE login_throttle
   SET count = 0, locked_until = ?, window_start = ?, updated_at = ? WHERE throttle_key = ?`,
				now.Add(t.cfg.LoginLockout), now, now, ukey)
			return err
		}
		_, err = q.ExecContext(ctx, `
UPDATE login_throttle SET count = ?, window_start = ?, updated_at = ? WHERE throttle_key = ?`, count, now, now, ukey)
		return err
	})
	if err != nil {
		slog.Error("api: login throttle record", "err", err)
	}
}

// acctIdleHorizon 是失败计数的闲置衰减期(4× 锁定窗;进程内先例同值)。
func (t *loginThrottle) acctIdleHorizon() time.Duration {
	h := 4 * t.cfg.LoginLockout
	if h <= 0 {
		return time.Hour
	}
	return h
}

// withTxRetry 包 WithTx 加 Transient 重试(抖动指数退避 + ctx 退出
// ——engine「唯一允许抖动退避重试的一类」纪律的完整形态,scheduler
// sched/resume 先例同款循环:**上界判定先于睡**,末次失败即返不空等;
// maxTransientRetries=3 = 总尝试 4 次,与 scheduler/reaper 同值)。安全
// 网 = 本重试本身:api 事务两两锁序恒定无环,但 janitor DELETE(idx
// updated_at 扫描序)是第三方持锁方,api↔janitor 交错 1213 结构性可达
// ——双端 Transient 重试兜底(审计 LOW:勿把重试当冗余删)。
func (t *loginThrottle) withTxRetry(ctx context.Context, fn func(DBTX) error) error {
	return retryTransient(ctx, func() error { return WithTx(ctx, t.dbh, fn) })
}

// retryTransient 是重试循环的可测提取(审计阵亡补验:内联形态零测试
// 锚——真 DB 上逼 1213 不可确定性)。run 全额包裹一次事务尝试。
func retryTransient(ctx context.Context, run func() error) error {
	const maxTransientRetries = 3
	var err error
	for attempt := 0; ; attempt++ {
		err = run()
		if err == nil || !engine.Transient(err) || attempt >= maxTransientRetries {
			return err
		}
		backoff := time.Duration(float64(time.Millisecond*25<<(attempt+1)) * (1 + rand.Float64()))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
}
