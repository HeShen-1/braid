package preflight

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
)

// 队列 Redis 的三项硬契约(deploy/dev.docker-compose.yml redis-queue 同口径,
// issue #45):noeviction(逐出 = 静默丢任务)、AOF on(重启不烧队列)、
// appendfsync everysec(官方承认 ~1s 丢失窗;任务状态真相源在 MySQL,此处
// 只保队列本身)。
var (
	redisWants = map[string]string{
		"maxmemory-policy": "noeviction",
		"appendonly":       "yes",
		"appendfsync":      "everysec",
	}
	// redisOrder 固定输出键序(提示文本稳定,测试可锚)。
	redisOrder = []string{"maxmemory-policy", "appendonly", "appendfsync"}
)

// CheckRedisQueue 经 CONFIG GET 探测队列实例的逐出/持久化契约(本版
// go-redis ConfigGet 单参数,三次往返)。全部参数查完、失败 errors.Join
// 一次报全——运维一次重启看到全部错,不挤牙膏。
func CheckRedisQueue(ctx context.Context, addr string) error {
	c := redis.NewClient(&redis.Options{Addr: addr})
	defer func() { _ = c.Close() }()
	got := make(map[string]string, len(redisOrder))
	for _, key := range redisOrder {
		m, err := c.ConfigGet(ctx, key).Result()
		if err != nil {
			return fmt.Errorf("preflight: redis %s: CONFIG GET %s: %w (unreachable, or CONFIG renamed/disabled via rename-command)", addr, key, err)
		}
		if v, ok := m[key]; ok {
			got[key] = strings.TrimSpace(v)
		}
	}
	return redisVerdict(addr, got)
}

// redisVerdict 是契约比对的纯判定(表驱动单测锚;绿路径在此锚,红路径由
// t13 集测用真值覆盖)。
func redisVerdict(addr string, got map[string]string) error {
	var errs []error
	for _, key := range redisOrder {
		if got[key] != redisWants[key] {
			errs = append(errs, fmt.Errorf("redis %s: %s=%q, want %q; fix: restart redis-server with --%s %s (contract: deploy/dev.docker-compose.yml redis-queue)",
				addr, key, got[key], redisWants[key], key, redisWants[key]))
		}
	}
	return errors.Join(errs...)
}
