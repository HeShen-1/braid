// lease.go 承载 D-2026-10-01-t151 决策 2 的节点号租约仲裁(MySQL 租约表
// migration 0012):acquire(显式直达 / auto 池线性抢注)→ 续期(TTL/3)
// → lost(停止签发 + 响亮)→ 释放(SIGTERM DELETE)。过期判定全 DB 端
// NOW(3)(会话 time_zone='+00:00' 连接契约,结构性排除应用时钟漂移;
// 不声称排除一切双持——MySQL 异步 failover 尾吞 + 心跳停顿的 zombie 窗
// 如实登记于 D-t151 决策 8-①,fencing 不随 T15.2 实施)。
package snowflake

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
)

// DefaultHolder 返回实例身份缺省(hostname):k8s pod 名/compose 容器名
// 天然唯一且 restart 稳定(up -d recreate 换名 = 新实例语义,旧租约走
// ≤TTL 残窗);hostname 不可得时退化 PID 标识。
func DefaultHolder() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return fmt.Sprintf("pid-%d", os.Getpid())
}

// LeaseKind 是持有者组件类(ENUM 'api'/'worker'/'reaper';relay 无生成器
// 不参与——T15.6 mode=all 硬闸扫三类)。
type LeaseKind string

const (
	KindAPI    LeaseKind = "api"
	KindWorker LeaseKind = "worker"
	KindReaper LeaseKind = "reaper"
)

const (
	// LeaseTTLDef 是租约 TTL 缺省(kill -9 残窗上界 = TTL;续期间隔 = TTL/3,
	// 单次失败仍有 2/3 余量即 fail-closed)。
	LeaseTTLDef = 30 * time.Second

	// autoClaimAttempts 是 PK 竞态败者的重扫预算(≤3 次退避后拒启)。
	autoClaimAttempts = 3

	// autoBackoffBase 是重扫退避基(50/100/200ms)。
	autoBackoffBase = 50 * time.Millisecond

	// releaseDeleteTimeout 是 Release 侧单行 PK DELETE 的独立短上界
	//(不跟 caller ctx——ctx 超时走残窗语义,释放仍尽力)。
	releaseDeleteTimeout = 5 * time.Second
)

var (
	// ErrLeaseHeld:显式号被他持(未过期)→ 拒启;错误详情带持有者与到期
	// 时刻 + runbook 清行提示(kill -9 残窗 ≤ TTL 有界)。
	ErrLeaseHeld = errors.New("snowflake: node lease held by another holder")

	// ErrPoolExhausted:auto 池无空闲号(1024 行全 live)→ 拒启,不烧预算。
	ErrPoolExhausted = errors.New("snowflake: node lease pool exhausted (no free node in 0..1023)")

	// ErrPoolContention:竞态败者烧尽重扫预算 → 拒启。
	ErrPoolContention = errors.New("snowflake: node lease claim lost PK race 3x; refusing to start")

	// ErrLeaseRace:claim 竞态落空(内部可重试信号,非终态错误)。
	ErrLeaseRace = errors.New("snowflake: lease claim lost race")
)

// LeaseConfig 是租约装配输入:Kind 三类之一;Holder 是实例身份(裸值,
// 落库前拼 kind 前缀成 holder_id——跨类前缀防同宿主 api/worker 互认复占;
// cmd 装配缺省 os.Hostname(),k8s pod 名/compose 容器名天然唯一且
// restart 稳定);Spec 是三层解析产物;TTL 缺省 LeaseTTLDef。
type LeaseConfig struct {
	Kind   LeaseKind
	Spec   NodeSpec
	Holder string
	TTL    time.Duration
}

// Lease 是一个进程的节点号租约:acquire 成功即起续期 goroutine;lost 只发
// 一次(缓冲 1);Release 幂等安全。
type Lease struct {
	db     *sql.DB
	kind   LeaseKind
	holder string // 完整 holder_id = kind + ":" + 实例身份
	spec   NodeSpec
	node   uint16
	ttl    time.Duration

	lost        chan error
	stopRenew   chan struct{}
	renewDone   chan struct{}
	guard       atomic.Pointer[Generator] // 停发守护(原子:renewLoop goroutine 跨 goroutine 读写)
	releaseOnce sync.Once
}

// cfgNode 是显式号(spec 已在 StartLease 校验 ≤ MaxNodeID)。
func (l *Lease) cfgNode() uint16 { return l.spec.Node }

// StartLease 按三层解析仲裁节点号并起续期:显式(env 数字/内建默认 1/2)
// 按号直达——同 holder 复占即时、被他持拒启、过期行 CAS 接管;auto 线性
// 取空闲号(缺行或过期行),INSERT 抢注、竞态 ≤3 次退避重扫后拒启。
// capabilities 恒写 NULL(未宣告 fail-closed;宣告面 T15.6 消费)。
func StartLease(ctx context.Context, dbh *sql.DB, cfg LeaseConfig) (*Lease, error) {
	switch cfg.Kind {
	case KindAPI, KindWorker, KindReaper:
	default:
		return nil, fmt.Errorf("snowflake: lease kind %q: want api|worker|reaper", cfg.Kind)
	}
	if cfg.Holder == "" {
		return nil, errors.New("snowflake: lease holder is required (hostname by default; k8s pod / compose container name)")
	}
	if cfg.Spec.Mode == NodeExplicit && cfg.Spec.Node > MaxNodeID {
		return nil, fmt.Errorf("snowflake: lease node %d > %d", cfg.Spec.Node, MaxNodeID)
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = LeaseTTLDef
	}
	l := &Lease{
		db:        dbh,
		kind:      cfg.Kind,
		holder:    string(cfg.Kind) + ":" + cfg.Holder,
		spec:      cfg.Spec,
		ttl:       ttl,
		lost:      make(chan error, 1),
		stopRenew: make(chan struct{}),
		renewDone: make(chan struct{}),
	}
	var (
		node uint16
		err  error
	)
	switch cfg.Spec.Mode {
	case NodeExplicit:
		node, err = l.acquireExplicit(ctx)
	case NodeAuto:
		node, err = l.acquireAuto(ctx)
	default:
		return nil, fmt.Errorf("snowflake: unknown node mode %d", cfg.Spec.Mode)
	}
	if err != nil {
		return nil, err
	}
	l.node = node
	go l.renewLoop()
	return l, nil
}

// Node 返回仲裁成功的节点号(0..MaxNodeID)。
func (l *Lease) Node() uint16 { return l.node }

// Lost 在租约丢失时收到唯一一次错误(续期失败:行被他持接管或 DB 错)。
// 复航 = 进程退出后重新 acquire(容器 restart 承载),无进程内复用面。
func (l *Lease) Lost() <-chan error { return l.lost }

// Guard 登记本租约守护的生成器:lost 先 Block 停发、后发信号——Lost 可见
// 时 Next 已恒拒(ErrLeaseLost)。原子注册,可在 StartLease 返回后的任意
// 时点调用(renewLoop goroutine 读侧无窗);仍建议装配后立即调用,把
// 「守护生效前的 lost」窗收窄到装配期。
func (l *Lease) Guard(g *Generator) { l.guard.Store(g) }

// Release 是优雅停机路径(SIGTERM 必须 DELETE 释放,决策 2):停续期、
// 删本行。lost 后调用安全(行非本持,DELETE 0 行幂等)。续期排空等待受
// caller ctx 上界(续期 Exec 自带 ≤TTL 上界,但停机路径不应无限等);
// 等待超时 = 走 kill -9 残窗语义(行 ≤TTL 过期收敛),DELETE 仍以独立
// 短超时尽力执行(与在途续期竞态无害:进程退出,UPDATE 落空即 lost
// 信号无人消费)。
func (l *Lease) Release(ctx context.Context) error {
	var err error
	l.releaseOnce.Do(func() {
		close(l.stopRenew)
		select {
		case <-l.renewDone:
		case <-ctx.Done():
		}
		dctx, cancel := context.WithTimeout(context.Background(), releaseDeleteTimeout)
		defer cancel()
		_, err = l.db.ExecContext(dctx,
			`DELETE FROM snowflake_node_lease WHERE node_id = ? AND holder_id = ?`, l.node, l.holder)
	})
	return err
}

// --- acquire -----------------------------------------------------------------

// acquireExplicit 显式号直达:INSERT 抢注;在场则 CAS 复占(同 holder 或
// 已过期)。RowsAffected=0 后用 SELECT 仲裁:go-sql-driver 缺省
// clientFoundRows=false(0 = changed 非 matched——engine CAS 语义依赖此
// 口径,不动 DSN 契约),复占值与行完全同毫秒相同时 changed=0 但确已
// 持有(CI 实证:首租约续期与重启复占同毫秒碰撞);holder==mine 即成功,
// 否则被他持拒启。行消失(释放竞态)视作重扫信号,共用 ≤3 预算。
func (l *Lease) acquireExplicit(ctx context.Context) (uint16, error) {
	for attempt := 0; attempt < autoClaimAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(autoBackoffBase << uint(attempt-1)):
			}
		}
		if err := l.tryInsert(ctx, l.cfgNode()); err == nil {
			return l.cfgNode(), nil
		} else if !errors.Is(err, ErrLeaseRace) {
			return 0, err
		}
		res, err := l.db.ExecContext(ctx,
			`UPDATE snowflake_node_lease
			 SET kind = ?, holder_id = ?, expires_at = DATE_ADD(NOW(3), INTERVAL ? MICROSECOND), capabilities = NULL
			 WHERE node_id = ? AND (holder_id = ? OR expires_at < NOW(3))`,
			string(l.kind), l.holder, l.ttl.Microseconds(), l.cfgNode(), l.holder)
		if err != nil {
			return 0, fmt.Errorf("snowflake: lease explicit reclaim: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 1 {
			return l.cfgNode(), nil // 同 holder 复占 / 过期接管(changed ≥ 1)
		}
		var holder string
		var expires time.Time
		qerr := l.db.QueryRowContext(ctx,
			`SELECT holder_id, expires_at FROM snowflake_node_lease WHERE node_id = ?`, l.cfgNode(),
		).Scan(&holder, &expires)
		switch {
		case errors.Is(qerr, sql.ErrNoRows):
			continue // 行刚被释放:重扫
		case qerr != nil:
			return 0, fmt.Errorf("snowflake: lease explicit held-detail: %w", qerr)
		}
		if holder == l.holder {
			// matched-unchanged:UPDATE 已匹配本持行但新旧值同毫秒完全一致
			//(changed=0)。复占成立。
			return l.cfgNode(), nil
		}
		return 0, fmt.Errorf("%w: node %d by %s until %v (kill -9 residual window ≤ TTL; runbook: deploy/UPGRADE.md §7 清行应急)",
			ErrLeaseHeld, l.cfgNode(), holder, expires.UTC())
	}
	return 0, fmt.Errorf("snowflake: explicit claim of node %d: %w", l.cfgNode(), ErrPoolContention)
}

// acquireAuto 装配真实 scan/claim 并跑重扫环。
func (l *Lease) acquireAuto(ctx context.Context) (uint16, error) {
	return l.acquireAutoLoop(ctx, l.scanPool, l.claimFree, sleepCtx(ctx))
}

// scanPool 读全池(≤1024 行)分 live/expired——过期判定全 DB 端 NOW(3)。
func (l *Lease) scanPool(ctx context.Context) (map[uint16]bool, map[uint16]bool, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT node_id, expires_at < NOW(3) FROM snowflake_node_lease`)
	if err != nil {
		return nil, nil, fmt.Errorf("snowflake: lease pool scan: %w", err)
	}
	defer func() { _ = rows.Close() }()
	live, expired := map[uint16]bool{}, map[uint16]bool{}
	for rows.Next() {
		var n uint16
		var isExp bool
		if err := rows.Scan(&n, &isExp); err != nil {
			return nil, nil, fmt.Errorf("snowflake: lease pool scan row: %w", err)
		}
		if isExp {
			expired[n] = true
		} else {
			live[n] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("snowflake: lease pool scan rows: %w", err)
	}
	return live, expired, nil
}

// claimFree 对候选号抢注:缺行 → INSERT;在场(扫描时过期)→ CAS 接管。
// 两语句条件互斥,任一落空 = ErrLeaseRace(状态已被并发改写)。
func (l *Lease) claimFree(ctx context.Context, n uint16) error {
	if err := l.tryInsert(ctx, n); err == nil {
		return nil
	} else if !errors.Is(err, ErrLeaseRace) {
		return err
	}
	res, err := l.db.ExecContext(ctx,
		`UPDATE snowflake_node_lease
		 SET kind = ?, holder_id = ?, expires_at = DATE_ADD(NOW(3), INTERVAL ? MICROSECOND), capabilities = NULL
		 WHERE node_id = ? AND expires_at < NOW(3)`,
		string(l.kind), l.holder, l.ttl.Microseconds(), n)
	if err != nil {
		return fmt.Errorf("snowflake: lease expired takeover: %w", err)
	}
	if k, err := res.RowsAffected(); err == nil && k == 1 {
		return nil
	}
	return ErrLeaseRace
}

// tryInsert 抢注缺行:PK 冲突(1062)= 竞态信号。
func (l *Lease) tryInsert(ctx context.Context, n uint16) error {
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO snowflake_node_lease (node_id, kind, holder_id, expires_at, capabilities)
		 VALUES (?, ?, ?, DATE_ADD(NOW(3), INTERVAL ? MICROSECOND), NULL)`,
		n, string(l.kind), l.holder, l.ttl.Microseconds())
	if err == nil {
		return nil
	}
	if isDupKey(err) {
		return ErrLeaseRace
	}
	return fmt.Errorf("snowflake: lease insert node %d: %w", n, err)
}

func isDupKey(err error) bool {
	var me *gomysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

// --- 重扫环(scan/claim/sleep 注入,纯单元可测) ----------------------------

// acquireAutoLoop:auto 池线性取空闲号(缺行或过期行),claim 竞态败者
// 退避重扫 ≤3 次后拒启;池满(1024 行全 live)立即拒,不烧预算。
func (l *Lease) acquireAutoLoop(
	ctx context.Context,
	scan func(context.Context) (map[uint16]bool, map[uint16]bool, error),
	claim func(context.Context, uint16) error,
	sleep func(time.Duration),
) (uint16, error) {
	for attempt := 0; attempt < autoClaimAttempts; attempt++ {
		if attempt > 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			sleep(autoBackoffBase << uint(attempt-1))
		}
		live, _, err := scan(ctx)
		if err != nil {
			return 0, err
		}
		if len(live) > int(MaxNodeID) {
			return 0, ErrPoolExhausted
		}
		for n := uint16(0); ; n++ {
			if live[n] {
				if n == MaxNodeID {
					break
				}
				continue
			}
			cerr := claim(ctx, n)
			if cerr == nil {
				return n, nil
			}
			if !errors.Is(cerr, ErrLeaseRace) {
				return 0, cerr
			}
			break // 状态已被并发改写:重扫新鲜状态
		}
	}
	return 0, ErrPoolContention
}

// sleepCtx 尊重 ctx 的退避睡眠(生产形态;单测注入空睡眠)。
func sleepCtx(ctx context.Context) func(time.Duration) {
	return func(wait time.Duration) {
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
}

// --- 续期 -------------------------------------------------------------------

// renewLoop 每 TTL/3 续期一次:UPDATE 只匹配本持行。任何失败(错误或
// 0 行)即 lost——先 Block 守护生成器(停止签发)再发信号。续期挂
// Background ctx、只受 stopRenew(Release)制动:SIGTERM 后的 asynq 排空
// 窗(≤ ShutdownTimeout)内仍持续续约——排空尾段租约过期 = 节点号被
// 接管者复用而本进程仍在签发,是必须关死的撞号窗。单次失败即判死 =
// fail-closed 严格口径:复航 = 退出重启重新 acquire(不预留宽限窗,防
// zombie 双签发)。
func (l *Lease) renewLoop() {
	defer close(l.renewDone)
	ticker := time.NewTicker(l.ttl / 3)
	defer ticker.Stop()
	for {
		select {
		case <-l.stopRenew:
			return
		case <-ticker.C:
		}
		rctx, cancel := context.WithTimeout(context.Background(), l.ttl)
		res, err := l.db.ExecContext(rctx,
			`UPDATE snowflake_node_lease
			 SET expires_at = DATE_ADD(NOW(3), INTERVAL ? MICROSECOND)
			 WHERE node_id = ? AND holder_id = ?`,
			l.ttl.Microseconds(), l.node, l.holder)
		cancel()
		if err == nil {
			if n, rerr := res.RowsAffected(); rerr == nil && n == 1 {
				continue
			}
		}
		rerr := fmt.Errorf("snowflake: lease renewal lost row for node %d (holder %s): stopping issuance; recover = restart & re-acquire", l.node, l.holder)
		if err != nil {
			rerr = fmt.Errorf("snowflake: lease renewal error for node %d (holder %s): %w; stopping issuance; recover = restart & re-acquire", l.node, l.holder, err)
		}
		if g := l.guard.Load(); g != nil {
			g.Block()
		}
		select {
		case l.lost <- rerr:
		default:
		}
		return
	}
}
