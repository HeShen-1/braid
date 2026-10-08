// Package redistest 提供 relay 层测试的 Redis 基建:验证 Redis 由 CI services
// 提供(契约端口 127.0.0.1:16379;本地复跑经 BRAID_TEST_REDIS_ADDR 指自备栈,
// 或自 git 历史取回已移除的 deploy/migrate-test.docker-compose.yml 单文件还原,
// D-2026-10-08-prod-oneclick)。与 sqltest 同模式(仅回环、占位凭证)。
//
// 隔离策略(两层):
//  1. DB 号:RedisClientOpt 的 DB 取 env BRAID_TEST_REDIS_DB(默认 0)。该 env
//     语义 = worktree 槽位(T2.7;基号与槽位分配表见 AGENTS.md「远端与并行
//     协作」)。**每个使用本包的测试包必须在 TestMain 里调 db.TestRedisDB(<包
//     基号>) 把槽位组合为绝对 DB 号并写回同名 env**(范例 internal/relay/
//     kill_test.go)。本包零改动、只读绝对号——漏写组合器的包会把槽位号当
//     绝对号解析,并行 worktree 下静默互踩(T2.3 worker / 后续 reaper 接入
//     前必读);
//  2. 键名:asynq 键内嵌队列名,每测试用唯一队列名即键级隔离(并行安全、
//     -count=1 重跑安全);task_id 由 per-process 唯一 ID 派生,跨运行不撞。
//
// 首次探活成功时顺带 FlushDB 一次,清上一轮测试进程在**本 DB**的残留队列。
package redistest

import (
	"context"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

const defaultAddr = "127.0.0.1:16379"

var flushOnce sync.Once

// Addr 返回测试 Redis 地址(env BRAID_TEST_REDIS_ADDR 可覆盖)。
func Addr() string {
	if a := os.Getenv("BRAID_TEST_REDIS_ADDR"); a != "" {
		return a
	}
	return defaultAddr
}

// db 返回所选 DB 号(env BRAID_TEST_REDIS_DB,默认 0)。
func db() int {
	if raw := os.Getenv("BRAID_TEST_REDIS_DB"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 && n <= 15 {
			return n
		}
	}
	return 0
}

// Opt 返回 asynq 连接选项(kill -9 子进程等无 *testing.T 的场景用)。
func Opt() asynq.RedisClientOpt {
	return asynq.RedisClientOpt{Addr: Addr(), DB: db()}
}

// MustOpt 探活测试 Redis 并返回连接选项;不可达 → t.Skipf(附起栈命令)。
// BRAID_TEST_REQUIRE_STACK=1(CI)时栈不可达 = Fatalf 而非 Skipf:静默
// skip 会把「没跑」伪装成「绿」(T8.1)。
func MustOpt(t TB) asynq.RedisClientOpt {
	t.Helper()
	insp := asynq.NewInspector(Opt())
	if _, err := insp.Queues(); err != nil { // Queues 即 ping(一次真实往返)
		_ = insp.Close()
		if os.Getenv("BRAID_TEST_REQUIRE_STACK") == "1" {
			t.Fatalf("验证 Redis 不可达(REQUIRE_STACK=1,栈必达断言): %v", err)
		}
		t.Skipf("验证 Redis 不可达(自备栈或 BRAID_TEST_REDIS_ADDR): %v", err)
	}
	_ = insp.Close()
	flushOnce.Do(func() { FlushDB(t) })
	return Opt()
}

// FlushDB 清空所选 DB(实例独占测试用;go-redis 直连,asynq Inspector 无此操作)。
func FlushDB(t TB) {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: Addr(), DB: db()})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.FlushDB(ctx).Err(); err != nil {
		_ = c.Close()
		t.Fatalf("flush test redis %s db %d: %v", Addr(), db(), err)
	}
	_ = c.Close()
}

// TB 收窄 testing.TB(与 sqltest 同款,避免测试文件外泄 testing 依赖)。
type TB interface {
	Helper()
	Skipf(format string, args ...any)
	Fatalf(format string, args ...any)
}
