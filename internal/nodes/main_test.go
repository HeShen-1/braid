package nodes

// 集成测试基建:T3.6 验收链 = 真 asynq server + 真 relay + 真 MySQL(t13 栈,
// 不可达自动 skip)+ OpenAI 兼容 httptest 桩。TestMain 组合 Redis 槽位
// (AGENTS.md 分配表:nodes 基号 3——第 4 个 Redis 消费包,登记于并行公约;
// 绝对 DB = worktree 槽位×4 + 基号,0..15 恰容纳 4 槽位 × 4 包)。

import (
	"fmt"
	"os"
	"strconv"
	"testing"

	"braid/internal/db"
	"braid/internal/db/sqltest"
)

// nodesRedisBase:nodes 包的 Redis 基号(AGENTS.md:relay=0、worker=1、
// reaper=2、nodes=3)。
const nodesRedisBase = 3

// nodesSchema:nodes 包独占 schema(跨包隔离 go test ./... 并行)。
const nodesSchema = "braid_nodes_test"

func TestMain(m *testing.M) {
	// T2.7:BRAID_TEST_REDIS_DB 语义 = worktree 槽位,组合为绝对 DB 号
	// (槽位×4 + nodes 基号)写回同名 env;非法槽位响亮退出。
	redisDB, err := db.TestRedisDB(nodesRedisBase)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := os.Setenv("BRAID_TEST_REDIS_DB", strconv.Itoa(redisDB)); err != nil {
		fmt.Fprintf(os.Stderr, "set BRAID_TEST_REDIS_DB: %v\n", err)
		os.Exit(2)
	}
	// 父进程清扫(栈不可达则留给各测试自行 skip)。
	if d, err := sqltest.OpenAt(nodesSchema); err == nil {
		_, _ = d.Exec(`DELETE FROM outbox`)
		_, _ = d.Exec(`DELETE FROM node_attempt`)
		_ = d.Close()
	}
	os.Exit(m.Run())
}
