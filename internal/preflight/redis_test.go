package preflight

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"braid/internal/redistest"
)

func TestRedisVerdict(t *testing.T) {
	green := map[string]string{
		"maxmemory-policy": "noeviction",
		"appendonly":       "yes",
		"appendfsync":      "everysec",
	}
	mut := func(kv ...[2]string) map[string]string {
		m := make(map[string]string, len(green))
		for k, v := range green {
			m[k] = v
		}
		for _, p := range kv {
			if p[1] == "" {
				delete(m, p[0])
			} else {
				m[p[0]] = p[1]
			}
		}
		return m
	}
	tests := []struct {
		name    string
		got     map[string]string
		wantSub []string
	}{
		{name: "contract met", got: green},
		{name: "eviction policy", got: mut([2]string{"maxmemory-policy", "allkeys-lru"}), wantSub: []string{"maxmemory-policy=\"allkeys-lru\"", "--maxmemory-policy noeviction"}},
		{name: "aof off", got: mut([2]string{"appendonly", "no"}), wantSub: []string{"appendonly=\"no\"", "--appendonly yes"}},
		{name: "fsync always", got: mut([2]string{"appendfsync", "always"}), wantSub: []string{"appendfsync=\"always\""}},
		{name: "missing key reads as mismatch", got: mut([2]string{"appendonly", ""}), wantSub: []string{"appendonly=\"\""}},
		{name: "all three broken joined", got: mut(
			[2]string{"maxmemory-policy", "volatile-lru"},
			[2]string{"appendonly", "no"},
			[2]string{"appendfsync", "no"},
		), wantSub: []string{"maxmemory-policy", "appendonly", "appendfsync"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := redisVerdict("127.0.0.1:16379", tt.got)
			assertSubstrings(t, err, tt.wantSub)
		})
	}
}

// TestCheckRedisQueueT13RedPath:t13 队列实例 = noeviction + everysec + AOF
// 刻意关闭(栈契约「down 即清」)——恰是 appendonly 契约的现成负例:真值
// 探测必须拒绝,且只拒绝 appendonly(policy/fsync 两项在 t13 上是绿的)。
// 绿路径不靠改栈(CONFIG SET 是实例级全局状态,并行 worktree 共享 t13,
// 运行时翻配置会互踩),由 TestRedisVerdict 的注入锚覆盖。
func TestCheckRedisQueueT13RedPath(t *testing.T) {
	addr := redistest.Addr()
	c := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		// REQUIRE_STACK=1(CI)时栈不可达 = Fatalf 而非 Skipf(redistest.
		// MustOpt 同口径,T8.1 审计 MEDIUM 修)。
		if os.Getenv("BRAID_TEST_REQUIRE_STACK") == "1" {
			t.Fatalf("braid-t13-redis-queue 不可达(REQUIRE_STACK=1,栈必达断言): %v", err)
		}
		t.Skipf("braid-t13-redis-queue 不可达,先起验证容器: %v", err)
	}
	_ = c.Close()
	err := CheckRedisQueue(context.Background(), addr)
	if err == nil {
		t.Fatal("t13 queue redis runs appendonly=no; CheckRedisQueue must reject the persistence contract")
	}
	if !strings.Contains(err.Error(), "appendonly") {
		t.Fatalf("err should name appendonly: %v", err)
	}
	if strings.Contains(err.Error(), "maxmemory-policy") {
		t.Fatalf("t13 is noeviction; policy must not be flagged: %v", err)
	}
	if strings.Contains(err.Error(), "appendfsync") {
		t.Fatalf("t13 is everysec; appendfsync must not be flagged: %v", err)
	}
}
