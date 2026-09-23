package reaper

// 用例 14(leader):Lua 三分支(nil/NX、自身/续期、他人/skip)+ 释放。
// t13 真 Redis(不可达自动 skip);键隔离 = 测试内唯一 token,测试后 DEL。

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"braid/internal/redistest"
)

func newTestLocker(t *testing.T) *redis.Client {
	t.Helper()
	opt := redistest.MustOpt(t)
	rdb := redis.NewClient(&redis.Options{Addr: opt.Addr, DB: opt.DB})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("t13 redis unreachable: %v", err)
	}
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer dcancel()
		_ = rdb.Del(dctx, leaderKey).Err()
	})
	return rdb
}

func TestLeaderAcquireRenewSkip(t *testing.T) {
	rdb := newTestLocker(t)
	l := RedisLocker(rdb)
	ctx := context.Background()
	ttl := 5 * time.Second

	held, err := l.AcquireOrRenew(ctx, "tok-a", ttl)
	if err != nil || !held {
		t.Fatalf("first acquire = %v/%v, want true/nil", held, err)
	}
	// 他人 token:skip。
	held, err = l.AcquireOrRenew(ctx, "tok-b", ttl)
	if err != nil || held {
		t.Fatalf("rival acquire = %v/%v, want false/nil", held, err)
	}
	// 自身 token:续期成功。
	held, err = l.AcquireOrRenew(ctx, "tok-a", ttl)
	if err != nil || !held {
		t.Fatalf("renew = %v/%v, want true/nil", held, err)
	}
	// 释放(非持有者不误删)。
	if err := l.Release(ctx, "tok-b"); err != nil {
		t.Fatalf("rival release: %v", err)
	}
	if v, err := rdb.Get(ctx, leaderKey).Result(); err != nil || v != "tok-a" {
		t.Fatalf("leader key = %q/%v, want tok-a 未被误删", v, err)
	}
	// 持有者释放后可再取。
	if err := l.Release(ctx, "tok-a"); err != nil {
		t.Fatalf("owner release: %v", err)
	}
	held, err = l.AcquireOrRenew(ctx, "tok-b", ttl)
	if err != nil || !held {
		t.Fatalf("post-release acquire = %v/%v, want true/nil", held, err)
	}
}

// TestLeaderGateFailOpen:选主不可达 → fail-open(本 tick 照常执行)。
func TestLeaderGateFailOpen(t *testing.T) {
	r, _ := newReaper(t, testCfg())
	r.deps.Locker = brokenLocker{}
	if !r.leaderGate(context.Background(), "tok", time.Second) {
		t.Fatal("选主不可达应 fail-open 放行")
	}
}

type brokenLocker struct{}

func (brokenLocker) AcquireOrRenew(context.Context, string, time.Duration) (bool, error) {
	return false, errUnreachable
}
func (brokenLocker) Release(context.Context, string) error { return errUnreachable }

var errUnreachable = &staticErr{"redis unreachable"}

type staticErr struct{ msg string }

func (e *staticErr) Error() string { return e.msg }
