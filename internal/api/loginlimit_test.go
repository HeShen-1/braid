package api

// 登录防御 DB 锚定测试(T6.4 语义,T11.3 换装为 login_throttle 表后的
// 全量移植 + 新锚):t13 真 MySQL,不可达自动 skip。原进程内单测的
// 语义锚(窗口/锁定/成功清零/同锁未知主体/闲置衰减/键有界)逐条移植;
// 内存有界性锚(8192 封顶/驱逐/sweep)随实现退役——行空间归 janitor
// (reaper 侧 purgeLoginThrottle 测试锚定)。新增跨副本精确性锚:两实例
// 共享同一 DB 交替打点,阈值恰在第 N 次触发(D-t113 #2 行锁单事务的
// 验收面)。
//
// 变异自查:锁内复核删除 → decay/lock 用例红;成功重置删除 →
// success_clears 用例红;闲置衰减删除 → acct_decay 用例红;ip 窗口
// 过期重开删除 → rate_window 的跨窗用例红。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"braid/internal/db/sqltest"
)

// newTestThrottle 构造短旋钮测试防御层(时钟可注入)+ 独立清理。
func newTestThrottle(t *testing.T) *loginThrottle {
	t.Helper()
	dbh := sqltest.MustOpenAt(t, "braid_api_test")
	t.Cleanup(func() { _ = dbh.Close() })
	if _, err := dbh.Exec("DELETE FROM login_throttle"); err != nil {
		t.Fatalf("cleanup login_throttle: %v", err)
	}
	return newLoginThrottle(Config{
		LoginRateLimit:   3,
		LoginRateWindow:  time.Minute,
		LoginMaxFailures: 3,
		LoginLockout:     15 * time.Minute,
	}, dbh)
}

func TestLoginThrottleRateWindow(t *testing.T) {
	l := newTestThrottle(t)
	ctx := context.Background()
	base := time.Now().UTC()
	l.now = func() time.Time { return base }

	for i := 1; i <= 3; i++ {
		if limited, err := l.checkIP(ctx, "10.0.0.1"); err != nil || limited {
			t.Fatalf("第 %d 次(≤限): limited=%v err=%v", i, limited, err)
		}
	}
	if limited, err := l.checkIP(ctx, "10.0.0.1"); err != nil || !limited {
		t.Fatalf("第 4 次(>限): limited=%v err=%v, want limited", limited, err)
	}
	// 其它 IP 独立窗口(键隔离)。
	if limited, err := l.checkIP(ctx, "10.0.0.2"); err != nil || limited {
		t.Fatalf("异 IP 被连坐: limited=%v err=%v", limited, err)
	}
	// 窗口过期重开:时钟拨过窗口宽,同一 IP 计数归零。
	l.now = func() time.Time { return base.Add(2 * time.Minute) }
	if limited, err := l.checkIP(ctx, "10.0.0.1"); err != nil || limited {
		t.Fatalf("跨窗首试被拒: limited=%v err=%v", limited, err)
	}
}

func TestLoginThrottleLockout(t *testing.T) {
	l := newTestThrottle(t)
	ctx := context.Background()
	base := time.Now().UTC()
	l.now = func() time.Time { return base }

	fail := func() {
		t.Helper()
		l.record(ctx, "carol", false) // best-effort:无错误面(见实现注释)
	}
	for range 3 {
		fail() // MaxFailures=3:第 3 次落锁
	}
	locked, limited, err := l.check(ctx, "10.0.0.9", "carol")
	if err != nil || limited || !locked {
		t.Fatalf("落锁后 check: locked=%v limited=%v err=%v, want locked", locked, limited, err)
	}
	// 锁定期内的失败不追加、不解锁(锁定期满自愈)。
	l.record(ctx, "carol", false)
	locked, _, err = l.check(ctx, "10.0.0.9", "carol")
	if err != nil || !locked {
		t.Fatalf("锁内失败后: locked=%v err=%v, want 仍锁", locked, err)
	}
	// 期满自愈:拨过 lockout,重开计数(下次失败从 1 起)。
	l.now = func() time.Time { return base.Add(16 * time.Minute) }
	locked, _, err = l.check(ctx, "10.0.0.9", "carol")
	if err != nil || locked {
		t.Fatalf("期满后: locked=%v err=%v, want 自愈", locked, err)
	}
	// 锁内 no-op 计数锚(审计 MEDIUM):锁定期内的失败不追加——期满后
	// 再 2 败不锁;变异「锁内也计数」下期满时已带 1 计数,+2 = 3 → 锁红。
	l.record(ctx, "carol", false)
	l.record(ctx, "carol", false)
	locked, _, err = l.check(ctx, "10.0.0.9", "carol")
	if err != nil || locked {
		t.Fatalf("锁内 no-op 后期满重开 2 败不应锁: locked=%v err=%v", locked, err)
	}
}

func TestLoginThrottleSuccessClearsFailures(t *testing.T) {
	l := newTestThrottle(t)
	ctx := context.Background()
	base := time.Now().UTC()
	l.now = func() time.Time { return base }

	for range 2 {
		l.record(ctx, "carol", false)
	}
	l.record(ctx, "carol", true)
	// 成功清零后再败 2 次:仍未达阈值 3。
	for range 2 {
		l.record(ctx, "carol", false)
	}
	locked, _, err := l.check(ctx, "10.0.0.9", "carol")
	if err != nil || locked {
		t.Fatalf("成功清零后: locked=%v err=%v, want 未锁", locked, err)
	}
}

func TestLoginThrottleUnknownUsernameLocksEqually(t *testing.T) {
	// 防枚举:未知/域外主体同样计数同样锁(无存在性 oracle,D-t64 口径)。
	l := newTestThrottle(t)
	ctx := context.Background()
	base := time.Now().UTC()
	l.now = func() time.Time { return base }

	for range 3 {
		l.record(ctx, "ghost-user", false)
	}
	locked, _, err := l.check(ctx, "10.0.0.9", "ghost-user")
	if err != nil || !locked {
		t.Fatalf("未知主体未同锁: locked=%v err=%v", locked, err)
	}
	// 对照半边(审计 LOW):锁只跟踪被试主体,零失败主体不被连坐。
	if locked, _, err := l.check(ctx, "10.0.0.9", "alice"); err != nil || locked {
		t.Fatalf("零失败主体被连坐: locked=%v err=%v", locked, err)
	}
}

func TestLoginThrottleAcctDecay(t *testing.T) {
	// 闲置衰减(生产形态:经 check→record 序列)。衰减纪元 = 计数写入
	// 时刻(window_start,record 独占);check 的 updated_at 活性触达不得
	// 摧毁它——orch-review HIGH 的回归锚:旧实现(衰减读 updated_at +
	// check 每请求触达)在生产路径衰减永不可达,直调 record 的测试绿而
	// 掩盖。
	l := newTestThrottle(t)
	ctx := context.Background()
	base := time.Now().UTC()
	now := base
	l.now = func() time.Time { return now }
	attempt := func() {
		t.Helper()
		locked, _, err := l.check(ctx, "10.0.0.9", "carol")
		if err != nil {
			t.Fatalf("check: %v", err)
		}
		if locked {
			t.Fatal("衰减路径不应落锁")
		}
		l.record(ctx, "carol", false)
	}
	for range 2 {
		attempt() // 纪元内 count=2
	}
	// 闲置超过 4×lockout(衰减纪元过期),期间同类请求的 check 触达不
	// 刷新纪元:下次失败计数重开为 1。
	now = base.Add(61 * time.Minute)
	attempt()
	attempt() // 纪元内累计 2
	locked, _, err := l.check(ctx, "10.0.0.9", "carol")
	if err != nil || locked {
		t.Fatalf("衰减重开后 2 次不应锁(MaxFailures=3): locked=%v err=%v", locked, err)
	}
	// 控制半边:衰减没有破坏锁定本身——纪元内第 3 次仍恰时落锁。
	attempt()
	locked, _, err = l.check(ctx, "10.0.0.9", "carol")
	if err != nil || !locked {
		t.Fatalf("纪元内累计 3 次应恰时落锁: locked=%v err=%v", locked, err)
	}
}

func TestLoginThrottleKeyBounded(t *testing.T) {
	// 键截断与审计列同限:超长变体折叠同键反而收紧防御(rune 边界安全
	// 由 auditUsername 单测锚,此处锚 DB 键域行为)。
	l := newTestThrottle(t)
	ctx := context.Background()
	base := time.Now().UTC()
	l.now = func() time.Time { return base }
	long := strings.Repeat("a", 300)
	for i := range 3 {
		l.record(ctx, long[:200+i], false) // 截断后同键
	}
	locked, _, err := l.check(ctx, "10.0.0.9", long)
	if err != nil || !locked {
		t.Fatalf("超长变体折叠键未锁: locked=%v err=%v", locked, err)
	}
}

func TestLoginThrottleCrossReplicaExact(t *testing.T) {
	// 跨副本阈值精确(D-t113 #2 验收面):两「副本」共享同 DB,交替打点,
	// 恰在第 MaxFailures 次落锁——行锁单事务使计数不丢不重。
	dbh := sqltest.MustOpenAt(t, "braid_api_test")
	t.Cleanup(func() { _ = dbh.Close() })
	if _, err := dbh.Exec("DELETE FROM login_throttle"); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	cfg := Config{LoginRateLimit: 100, LoginRateWindow: time.Minute,
		LoginMaxFailures: 5, LoginLockout: 15 * time.Minute}
	replicaA := newLoginThrottle(cfg, dbh)
	replicaB := newLoginThrottle(cfg, dbh)
	ctx := context.Background()

	locked := false
	for i := 1; i <= 5; i++ {
		var l *loginThrottle
		if i%2 == 1 {
			l = replicaA
		} else {
			l = replicaB
		}
		var err error
		locked, _, err = l.check(ctx, "10.0.0.9", "carol")
		if err != nil {
			t.Fatalf("check #%d: %v", i, err)
		}
		if locked {
			t.Fatalf("第 %d 次提前落锁(< 阈值 5)", i)
		}
		l.record(ctx, "carol", false)
	}
	locked, _, err := replicaA.check(ctx, "10.0.0.9", "carol")
	if err != nil || !locked {
		t.Fatalf("第 5 败后: locked=%v err=%v, want 恰此时锁定", locked, err)
	}
}

func TestLoginThrottleNilDBSkipsDefense(t *testing.T) {
	// 零栈桩面:nil DB → 防御跳过且不报错(生产恒有 DB;oidc 桩测试面)。
	l := newLoginThrottle(Config{}, nil)
	locked, limited, err := l.check(context.Background(), "10.0.0.1", "carol")
	if err != nil || locked || limited {
		t.Fatalf("nil DB: locked=%v limited=%v err=%v, want 全零", locked, limited, err)
	}
	l.record(context.Background(), "carol", false)
}

// TestLoginThrottleSprayCreatesNoUserRows:审计 HIGH 回归锚——u 行创建
// 收敛到 record,被 IP 窗口 429 的喷洒请求不建任何 u 行(认证前锚插的
// 变异 = 未认证 username 喷洒绕过 IP 限流无界灌表)。
func TestLoginThrottleSprayCreatesNoUserRows(t *testing.T) {
	l := newTestThrottle(t)
	ctx := context.Background()
	base := time.Now().UTC()
	l.now = func() time.Time { return base }

	for i := 0; i < 6; i++ { // 限 3/min:第 4 起被拒
		_, _, _ = l.check(ctx, "10.7.7.7", fmt.Sprintf("spray-%d", i))
	}
	var uRows int
	if err := l.dbh.QueryRow(
		`SELECT COUNT(*) FROM login_throttle WHERE kind = 'username'`).Scan(&uRows); err != nil {
		t.Fatalf("count u rows: %v", err)
	}
	if uRows != 0 {
		t.Fatalf("喷洒建立 %d 张 u 行(want 0——IP 窗口拒绝的请求不得建行)", uRows)
	}
	// 判定后的 record 才建行(行创建速率被 IP 窗口钳制)。
	l.record(ctx, "real-user", false)
	if err := l.dbh.QueryRow(
		`SELECT COUNT(*) FROM login_throttle WHERE kind = 'username'`).Scan(&uRows); err != nil || uRows != 1 {
		t.Fatalf("record 后 u 行 = %d err=%v, want 1", uRows, err)
	}
}

// TestLoginThrottleConcurrentRecordExact:跨副本并发锚(审计 MEDIUM)——
// FOR UPDATE 防丢失更新:N 并发 record(false) 同主体恰一次落锁;丢失
// 更新的实现计数封顶 <N 恒不锁。真时钟(并发窗内衰减纪元恒新鲜)。
func TestLoginThrottleConcurrentRecordExact(t *testing.T) {
	l := newTestThrottle(t)
	ctx := context.Background()
	const n = 3 // = MaxFailures
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.record(ctx, "carol", false)
		}()
	}
	wg.Wait()
	locked, _, err := l.check(ctx, "10.0.0.9", "carol")
	if err != nil || !locked {
		t.Fatalf("并发 %d 败后应恰一次落锁: locked=%v err=%v", n, locked, err)
	}
}

// TestLoginThrottleDefenseFailsClosed:fail-closed 锚(审计阵亡补验)——
// 防御面 DB 故障(表不可达,1146 非 Transient)必须 error 上抛(gate 映
// 射 500),绝不静默放行;fail-open 变异(吞错返 false,nil)在此红。
func TestLoginThrottleDefenseFailsClosed(t *testing.T) {
	l := newTestThrottle(t)
	if _, err := l.dbh.Exec(`RENAME TABLE login_throttle TO login_throttle_x`); err != nil {
		t.Fatalf("rename: %v", err)
	}
	t.Cleanup(func() { _, _ = l.dbh.Exec(`RENAME TABLE login_throttle_x TO login_throttle`) })
	if _, err := l.checkIP(context.Background(), "10.0.0.1"); err == nil {
		t.Fatal("checkIP 表缺失静默放行(fail-open 变异存活)")
	}
	if _, _, err := l.check(context.Background(), "10.0.0.1", "carol"); err == nil {
		t.Fatal("check 表缺失静默放行(fail-open 变异存活)")
	}
}

// TestRetryTransient:重试循环契约(审计阵亡补验:内联形态零锚)——
// 上界判定先于睡(总尝试 4 次)、非 Transient 立返、ctx 退出短路。
func TestRetryTransient(t *testing.T) {
	transientErr := &mysql.MySQLError{Number: 1213}
	plainErr := errors.New("boom")

	t.Run("first_try_success", func(t *testing.T) {
		calls := 0
		if err := retryTransient(context.Background(), func() error { calls++; return nil }); err != nil || calls != 1 {
			t.Fatalf("err=%v calls=%d, want nil/1", err, calls)
		}
	})
	t.Run("non_transient_immediate", func(t *testing.T) {
		calls := 0
		if err := retryTransient(context.Background(), func() error { calls++; return plainErr }); !errors.Is(err, plainErr) || calls != 1 {
			t.Fatalf("err=%v calls=%d, want plain/1", err, calls)
		}
	})
	t.Run("transient_then_success", func(t *testing.T) {
		calls := 0
		err := retryTransient(context.Background(), func() error {
			calls++
			if calls == 1 {
				return transientErr
			}
			return nil
		})
		if err != nil || calls != 2 {
			t.Fatalf("err=%v calls=%d, want nil/2", err, calls)
		}
	})
	t.Run("exhausts_at_four_attempts", func(t *testing.T) {
		calls := 0
		err := retryTransient(context.Background(), func() error { calls++; return transientErr })
		if !errors.Is(err, transientErr) || calls != 4 {
			t.Fatalf("err=%v calls=%d, want transient/4(maxTransientRetries=3)", err, calls)
		}
	})
	t.Run("ctx_cancel_short_circuits", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		err := retryTransient(ctx, func() error { calls++; return transientErr })
		if !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatalf("err=%v calls=%d, want ctx.Canceled/1", err, calls)
		}
	})
}
