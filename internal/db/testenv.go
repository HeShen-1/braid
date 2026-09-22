package db

// T2.7 测试隔离 env 化(worktree 并行地基;分配表与公约见 AGENTS.md
// 「远端与并行协作」)。本文件只服务测试基建(sqltest / migrate_test /
// 消费包 TestMain),不进任何运行时路径。单 worktree 下所有默认值 =
// 历史现状,行为零变化。
//
//   - MySQL:schema 名 = base + "_" + wtKey,wtKey 由 os.Getwd() 向上找
//     go.mod 推导(kill_test 的 helper 子进程继承父进程 cwd,天然同 key);
//     BRAID_TEST_SCHEMA_SUFFIX 非空则改用固定后缀(CI/调试)。
//   - DSN:BRAID_TEST_MYSQL_DSN 覆盖 server 级默认,sqltest 与
//     migrate_test 共用此口。
//   - Redis:BRAID_TEST_REDIS_DB 语义 = worktree 槽位(人工分配,主仓 0、
//     worktree 1..3),绝对 DB = 槽位×4 + 包基号;组合发生在消费包
//     TestMain(算好写回同名 env),redistest 读到的已是绝对号,零改动。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const defaultTestServerDSN = "root:braid_dev_root@tcp(127.0.0.1:13306)/"

// WorktreeKey 返回当前 worktree 的 8 hex 短键:sha256(go.mod 所在目录
// 绝对路径)[:8]。找不到 go.mod(仅 go test 之外调用,理论不可达)退化
// 为对 cwd 本身哈希——保持确定而非 panic。
func WorktreeKey() string {
	dir, err := os.Getwd()
	if err != nil {
		dir = "."
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // 已到文件系统根:退化用 cwd
		}
		dir = parent
	}
	sum := sha256.Sum256([]byte(dir))
	return hex.EncodeToString(sum[:])[:8]
}

// TestSchema 返回测试 schema 全名(base + "_" + 后缀)。后缀默认 = wtKey;
// BRAID_TEST_SCHEMA_SUFFIX 非空则用其值(CI/调试要固定名时)。
func TestSchema(base string) string {
	if s := os.Getenv("BRAID_TEST_SCHEMA_SUFFIX"); s != "" {
		return base + "_" + s
	}
	return base + "_" + WorktreeKey()
}

// TestServerDSN 返回测试 MySQL 的 server 级 DSN(env BRAID_TEST_MYSQL_DSN
// 覆盖)。值必须尾随 "/" 且不带库名——带库名时 panic:静默容忍会拼出坏
// 地址,表现成「栈不可达」全量 skip,掩盖配置错误。
func TestServerDSN() string {
	if d := os.Getenv("BRAID_TEST_MYSQL_DSN"); d != "" {
		if !strings.HasSuffix(d, "/") {
			panic(fmt.Sprintf("BRAID_TEST_MYSQL_DSN must be server-level (trailing /, no db name), got %q", d))
		}
		return d
	}
	return defaultTestServerDSN
}

// TestRedisDB 把 BRAID_TEST_REDIS_DB(语义 = worktree 槽位)组合为绝对
// DB 号:槽位×4 + base(包基号:relay=0、worker=1、reaper=2)。槽位非法
// 时返回 error——越界绝对号(>15)经 redistest 的 0..15 校验会被静默
// 回退 DB 0,造成并行 worktree 互踩(已知失败模式),必须响亮失败。
func TestRedisDB(base int) (int, error) {
	slot := 0
	if raw := os.Getenv("BRAID_TEST_REDIS_DB"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > 3 {
			return 0, fmt.Errorf("BRAID_TEST_REDIS_DB=%q: want slot 0..3 (main repo 0, worktrees 1..3)", raw)
		}
		slot = n
	}
	return slot*4 + base, nil
}
