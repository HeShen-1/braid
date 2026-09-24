package preflight

import (
	"fmt"
	"os"
	"strconv"
	"testing"

	"braid/internal/db"
)

// TestMain:T2.7 槽位组合(redistest 契约:消费包必须把 BRAID_TEST_REDIS_DB
// 从槽位语义组合为绝对号写回同名 env)。基号 0,与 relay 共租——本包零键
// 空间、仅 CONFIG GET(实例级探测,与 DB 号无关),不占独立基号;分配表
// 登记见 AGENTS.md「远端与并行协作」。非法槽位响亮退出(静默回退 DB 0 =
// 并行互踩,已知失败模式)。
func TestMain(m *testing.M) {
	redisDB, err := db.TestRedisDB(0)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.Setenv("BRAID_TEST_REDIS_DB", strconv.Itoa(redisDB)); err != nil {
		fmt.Fprintf(os.Stderr, "set BRAID_TEST_REDIS_DB: %v\n", err)
		os.Exit(2)
	}
	os.Exit(m.Run())
}
