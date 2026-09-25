// 登录限流 + 账号锁定(T6.4,D-t44 §7-① 三件之两件):进程内存实现
// (GATE 1 拍板)——固定窗口(IP)+ 失败计数锁定(username),零迁移零
// Redis,不破「api 零 Redis 消费包」契约(D-t55 #1/D-t73 #3)。
//
// 防枚举口径:
//   - 锁定计数键 = username 原样(域外/未知用户名同样计数同样锁)——
//     「连错 N 次后 429」对存在与不存在的用户名行为一致,无账号存在性
//     oracle;
//   - 锁定与限流共用同一响应形态(429 too_many_attempts),外部无法区分
//     「IP 窗口超限」与「账号锁定」——后者本身是用户名维度的事实泄露面。
//
// 已知边界(登记 D-t64):进程内存 ⇒ 重启清零、多副本各自计数(Q14 单
// 二进制单实例部署下接受;P1 多副本时升级为 DB 列)。
package api

import (
	"sync"
	"time"
)

// maxLoginEntries 是限流/锁定表的有界上限(防 username 灌水撑爆内存:
// 域外用户名同样计数 = 同样占条目)。超限时惰性清扫过期项。
const maxLoginEntries = 8192

// loginLimiter 是进程内登录防御层。零值不可用,经 NewLoginLimiter 构造
// (Config 四旋钮驱动);now 注入测试时钟。
type loginLimiter struct {
	mu    sync.Mutex
	ips   map[string]*ipWindow
	accts map[string]*acctState
	cfg   Config
	now   func() time.Time
}

type ipWindow struct {
	windowStart time.Time
	count       int
	lastSeen    time.Time
}

type acctState struct {
	failures    int
	lockedUntil time.Time
	lastSeen    time.Time
}

func newLoginLimiter(cfg Config) *loginLimiter {
	return &loginLimiter{
		ips:   map[string]*ipWindow{},
		accts: map[string]*acctState{},
		cfg:   cfg,
		now:   time.Now,
	}
}

// limiterKey 把 username 钳到有界键长(map 键即内存承诺;review HIGH:
// 1MiB 自由输入做键 = 键本身就是放大面)。截断形与审计列同限(128),
// 域外超长变体折叠到同键反而收紧防御。
func limiterKey(username string) string { return auditUsername(username) }

// evictIdlestLocked 从 count(map)驱逐 lastSeen 最旧的条目腾位(仅持锁
// 调用;饱和插入路径)。表已过 cap 且 sweep 无可收(全表新鲜攻击中)时,
// 驱逐最闲置条目优于拒绝计数——限流/锁定语义对新键继续成立,内存恒有界
// (rev2 advisory:两张表对称封顶,拒绝计数会让饱和期的新主体失去防御)。
func evictIdlestLocked[V interface{ idleAt() time.Time }](m map[string]V) {
	var oldestKey string
	var oldest time.Time
	first := true
	for k, v := range m {
		t := v.idleAt()
		if first || t.Before(oldest) {
			oldestKey, oldest, first = k, t, false
		}
	}
	if !first {
		delete(m, oldestKey)
	}
}

func (w *ipWindow) idleAt() time.Time  { return w.lastSeen }
func (a *acctState) idleAt() time.Time { return a.lastSeen }

// ipWindowAtCap 在持锁下取/建 IP 窗口,饱和时驱逐最闲置腾位。
func (l *loginLimiter) ipWindowAt(now time.Time, ip string) *ipWindow {
	if w := l.ips[ip]; w != nil {
		return w
	}
	if len(l.ips) >= maxLoginEntries {
		evictIdlestLocked(l.ips)
	}
	w := &ipWindow{windowStart: now}
	l.ips[ip] = w
	return w
}

// checkIP 是 body 不可解析路径的 IP-only 判定(不触锁定表):坏 body 请求
// 同样消费 IP 窗口配额,防「malformed 绕过限流」(review HIGH)。
func (l *loginLimiter) checkIP(ip string) (locked, limited bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w := l.ipWindowAt(now, ip)
	if now.Sub(w.windowStart) >= l.cfg.LoginRateWindow {
		w.windowStart, w.count = now, 0
	}
	w.count++
	w.lastSeen = now
	l.sweepLocked(now)
	return false, w.count > l.cfg.LoginRateLimit
}

// check 在认证之前判定(不付 bcrypt):本次请求计入 IP 固定窗口;返回
// (locked, limited)。两者皆映射 429,语义区分只在指标 label。
func (l *loginLimiter) check(ip, username string) (locked, limited bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	username = limiterKey(username)
	w := l.ipWindowAt(now, ip)
	if now.Sub(w.windowStart) >= l.cfg.LoginRateWindow {
		w.windowStart, w.count = now, 0
	}
	w.count++
	w.lastSeen = now
	limited = w.count > l.cfg.LoginRateLimit
	if a := l.accts[username]; a != nil && a.lockedUntil.After(now) {
		locked = true
	}
	l.sweepLocked(now)
	return locked, limited
}

// record 登录判定之后回填:成功清零该主体失败计数;失败递增,达到阈值
// 即锁定 Lockout(锁定期满自动解锁——不设显式解锁面,攻击者停手后账号
// 自愈)。
func (l *loginLimiter) record(username string, success bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	username = limiterKey(username)
	if success {
		delete(l.accts, username)
		return
	}
	a := l.accts[username]
	if a == nil {
		// 饱和封顶(rev2 修:驱逐最闲置腾位,而非拒计——拒计会让饱和期的
		// 新主体失去锁定保护;驱逐的代价是极旧计数蒸发,可接受)。
		if len(l.accts) >= maxLoginEntries {
			evictIdlestLocked(l.accts)
		}
		a = &acctState{}
		l.accts[username] = a
	}
	a.lastSeen = now
	if a.lockedUntil.After(now) {
		return // 锁定窗内的失败不追加(锁定期满重开计数)
	}
	a.failures++
	if a.failures >= l.cfg.LoginMaxFailures {
		a.lockedUntil = now.Add(l.cfg.LoginLockout)
		a.failures = 0
	}
}

// acctIdleHorizon 是失败计数的闲置衰减期(4× 锁定窗):停滞攻击留下的
// 1..N-1 失败条目按 lastSeen 过期回收(review HIGH:它们原先永生——
// sweep 只删 failures==0,喷洒唯一用户名即可无界堆积)。
func (l *loginLimiter) acctIdleHorizon() time.Duration {
	h := 4 * l.cfg.LoginLockout
	if h <= 0 {
		return time.Hour
	}
	return h
}

// sweepLocked 惰性清扫(仅持锁调用):表满时丢弃已过期条目。饱和且全部
// 新鲜时不清(攻击进行中,条目本就该在)——上限防的是无界增长,不是
// 精确配额。
func (l *loginLimiter) sweepLocked(now time.Time) {
	if len(l.ips)+len(l.accts) <= maxLoginEntries {
		return
	}
	ipIdle := l.cfg.LoginRateWindow
	for k, w := range l.ips {
		if now.Sub(w.lastSeen) >= ipIdle {
			delete(l.ips, k)
		}
	}
	acctIdle := l.acctIdleHorizon()
	for k, a := range l.accts {
		if now.Sub(a.lastSeen) >= acctIdle {
			delete(l.accts, k)
		}
	}
}
