package api

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeClock 是可拨测试时钟。
type fakeClock struct{ now time.Time }

func (f *fakeClock) advance(d time.Duration) { f.now = f.now.Add(d) }

func newTestLimiter(rate, maxFailures int, window, lockout time.Duration) (*loginLimiter, *fakeClock) {
	fc := &fakeClock{now: time.Date(2026, 9, 25, 1, 0, 0, 0, time.UTC)}
	return newLoginLimiter(Config{
		LoginRateLimit: rate, LoginRateWindow: window,
		LoginMaxFailures: maxFailures, LoginLockout: lockout,
	}), fc
}

func TestLoginLimiterRateWindow(t *testing.T) {
	l, clk := newTestLimiter(3, 100, time.Minute, time.Minute)
	l.now = func() time.Time { return clk.now }
	for i := 0; i < 3; i++ {
		if _, limited := l.check("1.2.3.4", "alice"); limited {
			t.Fatalf("request %d: limited early", i+1)
		}
	}
	if _, limited := l.check("1.2.3.4", "alice"); !limited {
		t.Fatal("4th request in window should be limited")
	}
	// 第二 IP 不受第一 IP 窗口牵连(限流维度 = IP)。
	if _, limited := l.check("5.6.7.8", "alice"); limited {
		t.Fatal("different IP affected by first IP's window")
	}
	// 窗口滚动:期满后计数重开。
	clk.advance(time.Minute + time.Second)
	l.now = func() time.Time { return clk.now }
	if _, limited := l.check("1.2.3.4", "alice"); limited {
		t.Fatal("window should have rolled over")
	}
}

func TestLoginLimiterLockout(t *testing.T) {
	l, clk := newTestLimiter(100, 3, time.Minute, 10*time.Minute)
	l.now = func() time.Time { return clk.now }
	for i := 0; i < 3; i++ {
		l.record("alice", false)
	}
	if locked, _ := l.check("1.1.1.1", "alice"); !locked {
		t.Fatal("alice should be locked after 3 failures")
	}
	// 正确密码也进不来(锁定期内防御优先——暴力破解减速语义)。
	if locked, limited := l.check("1.1.1.1", "alice"); !locked || limited {
		t.Fatalf("locked = %v limited = %v, want locked only", locked, limited)
	}
	// 锁定期满自动解锁(无显式解锁面)。
	clk.advance(10*time.Minute + time.Second)
	l.now = func() time.Time { return clk.now }
	if locked, _ := l.check("1.1.1.1", "alice"); locked {
		t.Fatal("lock should expire")
	}
	// 锁定窗内的失败不追加:期满后一次失败不立即再锁(计数已清)。
	l.record("alice", false)
	clk.advance(time.Second)
	l.now = func() time.Time { return clk.now }
	if locked, _ := l.check("1.1.1.1", "alice"); locked {
		t.Fatal("single failure after lock expiry must not re-lock")
	}
}

func TestLoginLimiterSuccessClearsFailures(t *testing.T) {
	l, clk := newTestLimiter(100, 3, time.Minute, time.Minute)
	l.now = func() time.Time { return clk.now }
	l.record("bob", false)
	l.record("bob", false)
	l.record("bob", true) // 第 3 次前成功:清零
	l.record("bob", false)
	if locked, _ := l.check("1.1.1.1", "bob"); locked {
		t.Fatal("success must clear failure count")
	}
}

// TestLoginLimiterUnknownUsernameLocksEqually:防枚举口径——域外/未知
// 用户名同样计数同样锁(「连错 N 次 → 429」对存在与不存在主体行为一致,
// 无账号存在性 oracle)。
func TestLoginLimiterUnknownUsernameLocksEqually(t *testing.T) {
	l, clk := newTestLimiter(100, 2, time.Minute, time.Minute)
	l.now = func() time.Time { return clk.now }
	for i := 0; i < 2; i++ {
		l.record("ghost-user-不存在", false)
	}
	lockedKnown, _ := l.check("1.1.1.1", "alice")
	lockedGhost, _ := l.check("1.1.1.1", "ghost-user-不存在")
	if lockedKnown || !lockedGhost {
		t.Fatalf("unknown=%v known=%v: lock must track the tried username itself", lockedGhost, lockedKnown)
	}
}

// TestLoginLimiterSweepBoundsMemory:表满时惰性清扫——过期项(窗口已过
// 的 IP / 无失败计数的主体)被清,新鲜项保留(攻击进行中条目本就该在)。
func TestLoginLimiterSweepBoundsMemory(t *testing.T) {
	l, clk := newTestLimiter(1<<30, 1<<30, time.Minute, time.Minute)
	l.now = func() time.Time { return clk.now }
	for i := 0; i <= maxLoginEntries; i++ {
		l.ips["ip"+strconv.Itoa(i)] = &ipWindow{
			windowStart: clk.now, count: 1, lastSeen: clk.now,
		}
	}
	if got := len(l.ips); got <= maxLoginEntries {
		t.Fatalf("precondition: ips = %d, want > %d", got, maxLoginEntries)
	}
	// 全部条目拨过窗口 → 下一次 check 触发清扫,老条目全部过期可清。
	clk.advance(2 * time.Minute)
	l.now = func() time.Time { return clk.now }
	l.check("fresh-ip", "fresh-user")
	if got := len(l.ips); got != 1 {
		t.Fatalf("sweep left %d entries, want 1 (only fresh-ip)", got)
	}
}

// TestLoginLimiterAcctDecay:失败计数条目按闲置衰减(review HIGH:原先
// 1..N-1 失败条目永生——喷洒唯一用户名即可无界堆积);衰减后同主体的
// 历史失败清零(不跨衰减窗累计)。
func TestLoginLimiterAcctDecay(t *testing.T) {
	l, clk := newTestLimiter(100, 3, time.Minute, 10*time.Minute)
	l.now = func() time.Time { return clk.now }
	l.record("ghost", false) // failures=1,旧实现永生
	clk.advance(4*10*time.Minute + time.Second)
	l.now = func() time.Time { return clk.now }
	// 触发 sweep:表未满不扫 → 直接调 sweepLocked?公开行为面:灌满表。
	for i := 0; i <= maxLoginEntries; i++ {
		l.ips["sweep-ip"+strconv.Itoa(i)] = &ipWindow{windowStart: clk.now, count: 1, lastSeen: clk.now}
	}
	l.check("fresh", "fresh-user")
	if _, ok := l.accts["ghost"]; ok {
		t.Fatal("idle acct entry survived sweep (immortal failure entries = unbounded growth)")
	}
}

// TestLoginLimiterKeyBounded:超长 username 折叠到有界键(键即内存承诺),
// 变体折叠不产生第二键。
func TestLoginLimiterKeyBounded(t *testing.T) {
	long := strings.Repeat("a", 5000)
	l, clk := newTestLimiter(100, 2, time.Minute, time.Minute)
	l.now = func() time.Time { return clk.now }
	l.record(long, false)
	l.record(long[:4000]+"b", false) // 变体(截断后同键;阈值 2 ⇒ 直接锁)
	a := l.accts[limiterKey(long)]
	if a == nil || !a.lockedUntil.After(clk.now) {
		t.Fatalf("variants must fold onto one bounded key (2 hits on cap-2 should lock): %+v", a)
	}
	if len(limiterKey(long)) > maxAuditUsername {
		t.Fatalf("key length %d exceeds bound", len(limiterKey(long)))
	}
}

// TestLoginLimiterSaturatedInsertEvicts:表满(全表新鲜)时插入新键 =
// 驱逐最闲置腾位(rev2 修:替代拒计——饱和期新主体保留锁定保护),
// 表大小恒 ≤ cap,既有新鲜键的锁定语义不受影响。
func TestLoginLimiterSaturatedInsertEvicts(t *testing.T) {
	l, clk := newTestLimiter(1<<30, 2, time.Hour, time.Hour)
	l.now = func() time.Time { return clk.now }
	for i := 0; i < maxLoginEntries; i++ {
		l.accts["fill"+strconv.Itoa(i)] = &acctState{failures: 1, lastSeen: clk.now}
	}
	// 把 fill1 拨旧 → 它是最闲置,应被驱逐。
	l.accts["fill1"].lastSeen = clk.now.Add(-time.Hour)
	l.record("brand-new", false)
	if _, ok := l.accts["brand-new"]; !ok {
		t.Fatal("saturated insert must evict-and-admit (rejecting = losing defense for new principals)")
	}
	if _, ok := l.accts["fill1"]; ok {
		t.Fatal("idlest entry was not evicted")
	}
	if len(l.accts) > maxLoginEntries {
		t.Fatalf("cap violated: %d entries", len(l.accts))
	}
	// 既有键照常累计并锁。
	l.record("fill0", false)
	if !l.accts["fill0"].lockedUntil.After(clk.now) {
		t.Fatal("existing key lost lock semantics under cap")
	}
}
