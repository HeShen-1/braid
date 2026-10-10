// lease.go 承载 D-2026-10-01-t151 决策 2 的节点号租约仲裁(MySQL 租约表
// migration 0012):acquire(显式直达 / auto 池线性抢注)→ 续期(TTL/3)
// → lost(停止签发 + 响亮)→ 释放(SIGTERM DELETE)。过期判定全 DB 端
// NOW(3)(会话 time_zone='+00:00' 连接契约,结构性排除应用时钟漂移)。
// 租约 fencing(代际 + 等值加冕)随 T20.1 落地:D-2026-10-06-t201(0015
// 加 fencing_epoch 列;MySQL 异步 failover 尾吞 + 心跳停顿的 zombie 双签发/
// 双执行窗由此收口,残余边界见该条决策 8)。
package snowflake

import (
	"context"
	"database/sql"
	"encoding/json"
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

// CapApprovalModeAll 是 v1 能力词表唯一条目(D-t151 决策 6):本二进制
// 正确实现 T14.2 会签 mode=all 三方语义(api 定局守卫 / worker 续跑复核 /
// reaper 已定局判定)。旧二进制不宣告(NULL = fail-closed),api 硬闸在
// 发布/触发含 mode=all 的工作流前查在场实例全部宣告。
const CapApprovalModeAll = "approval_mode_all"

// DeclaredCapabilities 返回本二进制的 v1 能力宣告集(cmd 三入口装配
// 单源;词表演进在此追加)。
func DeclaredCapabilities() []string {
	return []string{CapApprovalModeAll}
}

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

	// ErrFenced:写事务被租约 fence 拒(D-2026-10-06-t201:行上
	// (holder_id, fencing_epoch) 已与本持有代际不符——被接管/复占/释放)。
	// 这是 zombie 停写的结构性信号,不是可重试错误:调用方整事务回滚,
	// 证据走事务外(slog warn + FenceRejected 计数器)。
	ErrFenced = errors.New("snowflake: lease fence rejected write (holder superseded)")
)

// LeaseConfig 是租约装配输入:Kind 三类之一;Holder 是实例身份(裸值,
// 落库前拼 kind 前缀成 holder_id——跨类前缀防同宿主 api/worker 互认复占;
// cmd 装配缺省 os.Hostname(),k8s pod 名/compose 容器名天然唯一且
// restart 稳定);Spec 是三层解析产物;TTL 缺省 LeaseTTLDef;Capabilities
// 是能力宣告(D-t151 决策 6:宣告时点 = acquire 启动期,无条件无旋钮;
// 空切片 = 写 NULL 未宣告)。行内容始终反映本次 acquire 的二进制:复占/
// 接管同样回写,不残留前身宣告(回滚到旧二进制的残窗误放由 D-t142②
// 回滚前置 SELECT 承载,UPGRADE 登记)。
type LeaseConfig struct {
	Kind         LeaseKind
	Spec         NodeSpec
	Holder       string
	TTL          time.Duration
	Capabilities []string
}

// Lease 是一个进程的节点号租约:acquire 成功即起续期 goroutine;lost 只发
// 一次(缓冲 1);Release 幂等安全。
type Lease struct {
	db     *sql.DB
	kind   LeaseKind
	holder string // 完整 holder_id = kind + ":" + 实例身份
	spec   NodeSpec
	node   uint16
	epoch  uint64 // 本持代际(D-t201:acquire 时行上起算;fence 等值基准)
	ttl    time.Duration
	caps   any // 宣告 JSON 字符串(空宣告 = nil → SQL NULL);三条写路径共用

	lost        chan error
	stopRenew   chan struct{}
	renewDone   chan struct{}
	guard       atomic.Pointer[Generator] // 停发守护(原子:renewLoop goroutine 跨 goroutine 读写)
	releaseOnce sync.Once
}

// cfgNode 是显式号(spec 已在 StartLease 校验 ≤ MaxNodeID)。
func (l *Lease) cfgNode() uint16 { return l.spec.Node }

// StartLease 按三层解析仲裁节点号并起续期:显式(env 数字/内建默认 1/2/3)
// 按号直达——同 holder 复占即时、被他持拒启、过期行 CAS 接管;auto 线性
// 取空闲号(缺行或过期行),INSERT 抢注、竞态 ≤3 次退避重扫后拒启。
// capabilities 随本次 acquire 宣告(T15.6 消费:NULL = 未宣告 fail-closed)。
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
	var caps any
	if len(cfg.Capabilities) > 0 {
		b, err := json.Marshal(cfg.Capabilities)
		if err != nil {
			return nil, fmt.Errorf("snowflake: lease capabilities marshal: %w", err)
		}
		caps = string(b)
	}
	l := &Lease{
		db:        dbh,
		kind:      cfg.Kind,
		holder:    string(cfg.Kind) + ":" + cfg.Holder,
		spec:      cfg.Spec,
		ttl:       ttl,
		caps:      caps,
		lost:      make(chan error, 1),
		stopRenew: make(chan struct{}),
		renewDone: make(chan struct{}),
	}
	var (
		node  uint16
		epoch uint64
		err   error
	)
	switch cfg.Spec.Mode {
	case NodeExplicit:
		node, epoch, err = l.acquireExplicit(ctx)
	case NodeAuto:
		node, epoch, err = l.acquireAuto(ctx)
	default:
		return nil, fmt.Errorf("snowflake: unknown node mode %d", cfg.Spec.Mode)
	}
	if err != nil {
		return nil, err
	}
	l.node = node
	l.epoch = epoch
	go l.renewLoop()
	return l, nil
}

// claimAndRead 在单事务内执行 claim 语句并锁定回读本持代际(D-t201
// 对抗审计 HIGH① 修复):行锁自 claim 的 UPDATE/INSERT 语句持至回读
// SELECT FOR UPDATE,并发同 holder 的 bump 被阻塞到本事务提交——回读
// 恒为本持自己的 bump 值,两进程不可能共持同 (holder, fencing_epoch)
// (复占双活语义 = 行加冕唯一持有者,代际在加冕点即分流)。claim 回
// ErrLeaseRace ⇒ 事务回滚、由调用方重扫;其余错误原样上抛。
func (l *Lease) claimAndRead(ctx context.Context, n uint16, claim func(context.Context, *sql.Tx) error) (uint64, error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("snowflake: lease claim tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // Commit 后幂等无害
	if err := claim(ctx, tx); err != nil {
		return 0, err
	}
	var epoch uint64
	if err := tx.QueryRowContext(ctx,
		`SELECT fencing_epoch FROM snowflake_node_lease WHERE node_id = ? FOR UPDATE`, n,
	).Scan(&epoch); err != nil {
		return 0, fmt.Errorf("snowflake: lease epoch readback (node %d): %w", n, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("snowflake: lease claim commit: %w", err)
	}
	return epoch, nil
}

// Node 返回仲裁成功的节点号(0..MaxNodeID)。
func (l *Lease) Node() uint16 { return l.node }

// Epoch 返回本持代际(D-t201:fence 等值基准;持有变化 +1,续期不变)。
func (l *Lease) Epoch() uint64 { return l.epoch }

// HolderID 返回完整持有者身份(kind:identity;fence 判等与对账面的
// 随身身份,拼装单源在本包)。
func (l *Lease) HolderID() string { return l.holder }

// Fence 是写事务内的加冕校验(D-t201 决策 3):同源锁定读,行上当前
// (holder_id, fencing_epoch) 与本持精确等值才放行;0 行 = ErrFenced、
// 整事务回滚。谓词不含 expires_at(加冕语义:夺写只因易主,不因墙钟——
// 懒回收下过期未被接管即无第二持有者,撞号与双执行的加害者都必经「成为
// 第二持有者」,而第二持有者必经 bump)。holder 比较显式 utf8mb4_bin
// (D-t204 :778 交接缝:表缺省 ai_ci 会把 case 变体互认成同持有者)。
// FOR UPDATE 持行锁至事务结束:并发接管与本校验在行锁上串行;锁序契约
// = 租约行恒为 fence 事务首锁(与 reaper 的 node→attempt→wf、claimant
// 启动的租约行单语句无环)。q 接受 *sql.DB / *sql.Tx / engine.DBTX
// (各包自定义 tx 面不互相 import 的仓内纪律,FleetScanner 同款最小面)。
// nil 接收者 = legacy 无 fence 装配(滚动窗/测试),恒放行。
func (l *Lease) Fence(ctx context.Context, q FenceScanner) error {
	if l == nil {
		return nil
	}
	var one int
	err := q.QueryRowContext(ctx,
		`SELECT 1 FROM snowflake_node_lease
		 WHERE node_id = ? AND holder_id COLLATE utf8mb4_bin = ? AND fencing_epoch = ?
		 FOR UPDATE`,
		l.node, l.holder, l.epoch).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: node %d epoch %d superseded (holder %q)", ErrFenced, l.node, l.epoch, l.holder)
	}
	if err != nil {
		return fmt.Errorf("snowflake: lease fence check (node %d): %w", l.node, err)
	}
	return nil
}

// FenceScanner 是 Fence 的最小查询面(*sql.DB、*sql.Tx 与各包自定义事务
// 句柄[如 engine.DBTX]均满足)。
type FenceScanner interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

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
		// 判等 bin + 代际限定(D-t201):前身进程不得误释放复占后继者的行
		//(lost 后调用本就是 0 行幂等,失败语义不变)。
		_, err = l.db.ExecContext(dctx,
			`DELETE FROM snowflake_node_lease
			 WHERE node_id = ? AND holder_id COLLATE utf8mb4_bin = ? AND fencing_epoch = ?`,
			l.node, l.holder, l.epoch)
	})
	return err
}

// --- acquire -----------------------------------------------------------------

// acquireExplicit 显式号直达:单事务内(claimAndRead)INSERT 抢注,在场
// 则同事务 CAS 复占(同 holder 或已过期)+ 锁定回读代际。CAS 落空后用
// 独立 SELECT 仲裁他持详情。SET 恒含 fencing_epoch+1(D-t201 决策 5)
// ⇒ 命中即 changed ≥ 1(go-sql-driver 缺省 clientFoundRows=false,
// 0 = changed 非 matched——engine CAS 语义依赖此口径,不动 DSN 契约),
// T15.2 的 matched-unchanged(同毫秒零改值)歧路对复占结构性不可达,
// 仲裁段 holder==mine 分支保留为 belt、按重扫处理(决策 5 登记);否则
// 被他持拒启。行消失(释放竞态)视作重扫信号,共用 ≤3 预算;1213/1205
// (并发同 holder 多启的 INSERT 意向锁 × 行锁环)按 engine.Transient 同
// 口径折叠为退避重扫。
func (l *Lease) acquireExplicit(ctx context.Context) (uint16, uint64, error) {
	for attempt := 0; attempt < autoClaimAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return 0, 0, ctx.Err()
			case <-time.After(autoBackoffBase << uint(attempt-1)):
			}
		}
		epoch, err := l.claimAndRead(ctx, l.cfgNode(), func(ctx context.Context, tx *sql.Tx) error {
			if err := l.tryInsert(ctx, tx, l.cfgNode()); err == nil {
				return nil
			} else if !errors.Is(err, ErrLeaseRace) {
				return err
			}
			// INSERT 撞在场行(缺行竞态败者):同事务内 CAS——同 holder
			// 复占或过期接管(SET 恒含 epoch+1 ⇒ 命中即 changed ≥ 1,
			// RowsAffected=changed 口径下无 matched-unchanged 歧路)。
			res, err := tx.ExecContext(ctx,
				`UPDATE snowflake_node_lease
				 SET kind = ?, holder_id = ?, expires_at = DATE_ADD(NOW(3), INTERVAL ? MICROSECOND), capabilities = ?,
				     fencing_epoch = fencing_epoch + 1
				 WHERE node_id = ? AND (holder_id COLLATE utf8mb4_bin = ? OR expires_at < NOW(3))`,
				string(l.kind), l.holder, l.ttl.Microseconds(), l.caps, l.cfgNode(), l.holder)
			if err != nil {
				return fmt.Errorf("snowflake: lease explicit reclaim: %w", err)
			}
			if n, err := res.RowsAffected(); err == nil && n == 1 {
				return nil // 同 holder 复占 / 过期接管
			}
			return ErrLeaseRace // 落空 ⇒ 仲裁段重判(独立连接,见下)
		})
		if err == nil {
			return l.cfgNode(), epoch, nil
		}
		if isTransientDB(err) {
			continue // 并发同 holder 抢注的 InnoDB 死锁(INSERT 意向锁 × 行锁环):退避重扫
		}
		if !errors.Is(err, ErrLeaseRace) {
			return 0, 0, err
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
			return 0, 0, fmt.Errorf("snowflake: lease explicit held-detail: %w", qerr)
		}
		if holder == l.holder {
			// 结构性不可达的 belt(D-t201 决策 5:SET 恒含 epoch+1,命中即
			// changed ≥ 1,不可能既匹配本持又零改值)——保守按重扫处理,
			// 杜绝「不 bump 直接加冕」路径存在。
			continue
		}
		return 0, 0, fmt.Errorf("%w: node %d by %s until %v (kill -9 residual window ≤ TTL; runbook: deploy/UPGRADE.md §7 清行应急)",
			ErrLeaseHeld, l.cfgNode(), holder, expires.UTC())
	}
	return 0, 0, fmt.Errorf("snowflake: explicit claim of node %d: %w", l.cfgNode(), ErrPoolContention)
}

// acquireAuto 装配真实 scan/claim 并跑重扫环。
func (l *Lease) acquireAuto(ctx context.Context) (uint16, uint64, error) {
	node, epoch, err := l.acquireAutoLoop(ctx, l.scanPool, l.claimFree, sleepCtx(ctx))
	return node, epoch, err
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

// claimFree 对候选号抢注并同事务回读代际:缺行 → INSERT;在场(扫描时
// 过期)→ CAS 接管。两语句条件互斥,任一落空 = ErrLeaseRace(状态已被
// 并发改写,事务回滚重扫)。
func (l *Lease) claimFree(ctx context.Context, n uint16) (uint64, error) {
	return l.claimAndRead(ctx, n, func(ctx context.Context, tx *sql.Tx) error {
		if err := l.tryInsert(ctx, tx, n); err == nil {
			return nil
		} else if !errors.Is(err, ErrLeaseRace) {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE snowflake_node_lease
			 SET kind = ?, holder_id = ?, expires_at = DATE_ADD(NOW(3), INTERVAL ? MICROSECOND), capabilities = ?,
			     fencing_epoch = fencing_epoch + 1
			 WHERE node_id = ? AND expires_at < NOW(3)`,
			string(l.kind), l.holder, l.ttl.Microseconds(), l.caps, n)
		if err != nil {
			return fmt.Errorf("snowflake: lease expired takeover: %w", err)
		}
		if k, err := res.RowsAffected(); err == nil && k == 1 {
			return nil
		}
		return ErrLeaseRace
	})
}

// tryInsert 抢注缺行:PK 冲突(1062)= 竞态信号。新行起代 1(D-t201;
// 缺行无前身,1 = 首个非零代际,legacy DEFAULT 0 只属于 0015 前存量行)。
// 语句在 claimAndRead 的事务连接上执行(1062 语句级失败不毁事务,调用方
// 同事务续走 CAS)。
func (l *Lease) tryInsert(ctx context.Context, tx *sql.Tx, n uint16) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO snowflake_node_lease (node_id, kind, holder_id, expires_at, capabilities, fencing_epoch)
		 VALUES (?, ?, ?, DATE_ADD(NOW(3), INTERVAL ? MICROSECOND), ?, 1)`,
		n, string(l.kind), l.holder, l.ttl.Microseconds(), l.caps)
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

// isTransientDB 折叠 1213/1205(InnoDB 死锁/锁等待超时)为可重试竞态信号
// (engine.Transient 同口径;本包不 import engine——叶工具包保持零内部
// 依赖,并发同 holder 多启的 INSERT 意向锁 × 行锁环由退避重扫收敛)。
func isTransientDB(err error) bool {
	var me *gomysql.MySQLError
	return errors.As(err, &me) && (me.Number == 1213 || me.Number == 1205)
}

// --- 重扫环(scan/claim/sleep 注入,纯单元可测) ----------------------------

// acquireAutoLoop:auto 池线性取空闲号(缺行或过期行),claim 竞态败者
// 退避重扫 ≤3 次后拒启;池满(1024 行全 live)立即拒,不烧预算。
func (l *Lease) acquireAutoLoop(
	ctx context.Context,
	scan func(context.Context) (map[uint16]bool, map[uint16]bool, error),
	claim func(context.Context, uint16) (uint64, error),
	sleep func(time.Duration),
) (uint16, uint64, error) {
	for attempt := 0; attempt < autoClaimAttempts; attempt++ {
		if attempt > 0 {
			if err := ctx.Err(); err != nil {
				return 0, 0, err
			}
			sleep(autoBackoffBase << uint(attempt-1))
		}
		live, _, err := scan(ctx)
		if err != nil {
			return 0, 0, err
		}
		if len(live) > int(MaxNodeID) {
			return 0, 0, ErrPoolExhausted
		}
		for n := uint16(0); ; n++ {
			if live[n] {
				if n == MaxNodeID {
					break
				}
				continue
			}
			epoch, cerr := claim(ctx, n)
			if cerr == nil {
				return n, epoch, nil
			}
			if !errors.Is(cerr, ErrLeaseRace) && !isTransientDB(cerr) {
				return 0, 0, cerr
			}
			break // 状态已被并发改写:重扫新鲜状态
		}
	}
	return 0, 0, ErrPoolContention
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

// --- 在场查询(T15.6 mode=all 硬闸读面) ------------------------------------

// FleetInstance 是一条在场租约行的读投影(供 api 硬闸判定与诊断详情)。
type FleetInstance struct {
	Node         uint16
	Kind         LeaseKind
	HolderID     string
	ExpiresAt    time.Time
	Capabilities []string // NULL = 未宣告(nil 切片)
}

// FleetScanner 是 InFleet 的最小查询面(*sql.DB 与事务句柄均满足;各包
// 自定义 tx 面不互相 import 的仓内纪律)。发布面闸在 Publish 事务内经
// 事务连接扫描——持事务期间经池另取连接 = 并发发布自饿死面(T20.3
// 对抗审计收口)。
type FleetScanner interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// InFleet 返回在场实例(expires_at >= NOW(3),kind 三类全扫——relay 无
// 生成器不在 ENUM),按 node_id 升序。过期判定与 acquire 一致全 DB 端
// NOW(3);查询错误原样上抛(硬闸侧 fail-closed:误拒方向安全,D-t151
// 决策 6)。dbh 可传池或事务句柄(发布面闸传 Publish 事务连接)。
func InFleet(ctx context.Context, dbh FleetScanner) ([]FleetInstance, error) {
	rows, err := dbh.QueryContext(ctx,
		`SELECT node_id, kind, holder_id, expires_at, capabilities
		 FROM snowflake_node_lease
		 WHERE expires_at >= NOW(3) AND kind IN ('api', 'worker', 'reaper')
		 ORDER BY node_id`)
	if err != nil {
		return nil, fmt.Errorf("snowflake: fleet scan: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []FleetInstance
	for rows.Next() {
		var inst FleetInstance
		var caps []byte
		if err := rows.Scan(&inst.Node, &inst.Kind, &inst.HolderID, &inst.ExpiresAt, &caps); err != nil {
			return nil, fmt.Errorf("snowflake: fleet scan row: %w", err)
		}
		if caps != nil {
			if err := json.Unmarshal(caps, &inst.Capabilities); err != nil {
				return nil, fmt.Errorf("snowflake: fleet capabilities parse (holder %s): %w", inst.HolderID, err)
			}
		}
		out = append(out, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snowflake: fleet scan rows: %w", err)
	}
	return out, nil
}

// HolderLive 返回同 kind+identity 的在场租约行(t20.4 沙箱清扫前活性复核
// 读面,D-t151 决策 8-③ 转正):无行或已过期 ⇒ (nil, nil)——「死 owner」
// 判定,清扫照走;在场 ⇒ 该行(多行同持的违例形态取最晚到期,供日志)。
// holder_id 拼装(kind:identity)单源在本包,同宿主跨类不互认;判等显式
// utf8mb4_bin 字节精确(与沙箱 owner label 的 docker 过滤同语义——表缺省
// ai_ci 大小写不敏感,异 case 的两个合法 owner 会被互判活体,恒误跳过
// 自家残容器清扫;本判等口径原为 T20.1 交接缝[D-t204 决策 1],已随
// D-2026-10-06-t201 决策 5 统一:acquire CAS / 续期 / Release / Fence 的
// holder 比较现全部显式 bin,与本函数同口径)。判活与 acquire/InFleet 同口径:全 DB 端
// NOW(3),应用时钟漂移不入库。查询错误原样上抛——调用侧 fail-safe:
// 复核不可达 ⇒ 跳过清扫(误跳过 < 误杀,同 D-t151 决策 6 取舍)。预期
// 调用时点 = 调用方 StartLease 之前(自身行尚不存在,任何同 holder 在场
// 行都是 peer;复占后同行即自身持有,后查会把活体 peer 漏成自身)。
func HolderLive(ctx context.Context, dbh *sql.DB, kind LeaseKind, identity string) (*FleetInstance, error) {
	var inst FleetInstance
	var caps []byte
	err := dbh.QueryRowContext(ctx,
		`SELECT node_id, kind, holder_id, expires_at, capabilities
		 FROM snowflake_node_lease
		 WHERE kind = ? AND holder_id COLLATE utf8mb4_bin = ? AND expires_at >= NOW(3)
		 ORDER BY expires_at DESC LIMIT 1`,
		kind, string(kind)+":"+identity).
		Scan(&inst.Node, &inst.Kind, &inst.HolderID, &inst.ExpiresAt, &caps)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("snowflake: holder liveness scan (%s:%s): %w", kind, identity, err)
	}
	if caps != nil {
		if err := json.Unmarshal(caps, &inst.Capabilities); err != nil {
			return nil, fmt.Errorf("snowflake: fleet capabilities parse (holder %s): %w", inst.HolderID, err)
		}
	}
	return &inst, nil
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
		// 续期判等 bin + 代际限定(D-t201):case 变体抢注者不被旧持有者
		// 续命;同 holder 复占(bump)后前身续期落空 → lost(复占双活的
		// 停摆收口——行加冕唯一持有者,前身必须停签发并下线)。
		res, err := l.db.ExecContext(rctx,
			`UPDATE snowflake_node_lease
			 SET expires_at = DATE_ADD(NOW(3), INTERVAL ? MICROSECOND)
			 WHERE node_id = ? AND holder_id COLLATE utf8mb4_bin = ? AND fencing_epoch = ?`,
			l.ttl.Microseconds(), l.node, l.holder, l.epoch)
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
