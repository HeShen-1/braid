package db

// T2.7 测试隔离 env 化的单元测试:wtKey 推导、schema 后缀、server DSN、
// Redis 槽位组合。wtKey 期望值经 moduleKey 独立重算(重新向上找 go.mod
// 再哈希),不复制被测实现,防自证空洞。

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// moduleDir 独立上溯 go.mod 所在目录(与 WorktreeKey 实现刻意不同源:若
// 实现忘了向上找 go.mod 而直接哈希 cwd,对 internal/db 包目录两者结果不同)。
func moduleDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod upward")
		}
		dir = parent
	}
}

func moduleKey(t *testing.T) string {
	t.Helper()
	sum := sha256.Sum256([]byte(moduleDir(t)))
	return hex.EncodeToString(sum[:])[:8]
}

func TestWorktreeKeyMatchesModuleRoot(t *testing.T) {
	if got := WorktreeKey(); got != moduleKey(t) {
		t.Fatalf("WorktreeKey = %q, want %q", got, moduleKey(t))
	}
}

// 模块根(经独立上溯得到)与包目录应得同 key:同 worktree 内位置无关。
func TestWorktreeKeyStableFromModuleRoot(t *testing.T) {
	want := WorktreeKey()
	t.Chdir(moduleDir(t))
	if got := WorktreeKey(); got != want {
		t.Fatalf("from module root: %q, want %q", got, want)
	}
}

// 合成模块:深两层子目录向上找 go.mod,命中模块根本身(而非 cwd)。
func TestWorktreeKeySyntheticModule(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test.example/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(root))
	want := hex.EncodeToString(sum[:])[:8]
	t.Chdir(sub)
	if got := WorktreeKey(); got != want {
		t.Fatalf("synthetic module: %q, want %q", got, want)
	}
}

func TestSchemaDefaultSuffix(t *testing.T) {
	if got := TestSchema("braid"); got != "braid_"+moduleKey(t) {
		t.Fatalf("TestSchema = %q, want braid_%s", got, moduleKey(t))
	}
}

func TestSchemaSuffixOverride(t *testing.T) {
	t.Setenv("BRAID_TEST_SCHEMA_SUFFIX", "ci7")
	if got := TestSchema("braid_engine_test"); got != "braid_engine_test_ci7" {
		t.Fatalf("override: %q", got)
	}
}

// suffix 空串等价未设(回落 wtKey)。
func TestSchemaSuffixEmptyIsUnset(t *testing.T) {
	t.Setenv("BRAID_TEST_SCHEMA_SUFFIX", "")
	if got := TestSchema("braid"); got != "braid_"+moduleKey(t) {
		t.Fatalf("empty suffix: %q, want wtKey fallback", got)
	}
}

// 非法 suffix 必须 panic:suffix 拼进 CREATE DATABASE(multiStatements=true
// 连接),`;`/引号等是实证过的多语句注入载体;超长与空白同样拒收。
func TestSchemaSuffixRejects(t *testing.T) {
	for _, s := range []string{
		"t; CREATE DATABASE braid_inj_probe", // 多语句注入 PoC
		"ci?params",                          // DSN 参数分隔符
		"has space",                          // 空白
		"b`raid",                             // 反引号逃逸标识符
		"25-chars-aaaaaaaaaaaaaaaaa",         // 超长(25)
		"中文",
	} {
		t.Run(s, func(t *testing.T) {
			t.Setenv("BRAID_TEST_SCHEMA_SUFFIX", s)
			defer func() {
				if recover() == nil {
					t.Fatalf("suffix %q 未被拒绝", s)
				}
			}()
			_ = TestSchema("braid")
		})
	}
}

func TestServerDSNDefault(t *testing.T) {
	if got := TestServerDSN(); got != "root:braid_dev_root@tcp(127.0.0.1:13306)/" {
		t.Fatalf("default DSN: %q", got)
	}
}

func TestServerDSNOverride(t *testing.T) {
	t.Setenv("BRAID_TEST_MYSQL_DSN", "root:p@tcp(127.0.0.1:23306)/")
	if got := TestServerDSN(); got != "root:p@tcp(127.0.0.1:23306)/" {
		t.Fatalf("override DSN: %q", got)
	}
}

// 带库名的 DSN 必须 panic 而非静默使用:拼出坏地址会表现成「栈不可达」
// 全量 skip,掩盖配置错误。含尾随斜杠绕过形态与语法垃圾。
func TestServerDSNRejectsDBName(t *testing.T) {
	for _, dsn := range []string{
		"root:p@tcp(127.0.0.1:23306)/oops",
		"root:p@tcp(127.0.0.1:23306)/braid/", // 尾随斜杠伪装 server 级
		"total garbage",                      // 语法非法,静默放行会在连接期变绿色 skip
	} {
		t.Run(dsn, func(t *testing.T) {
			t.Setenv("BRAID_TEST_MYSQL_DSN", dsn)
			defer func() {
				if recover() == nil {
					t.Fatalf("DSN %q 未被拒绝", dsn)
				}
			}()
			_ = TestServerDSN()
		})
	}
}

func TestRedisDBSlotCombination(t *testing.T) {
	cases := []struct {
		name string
		slot string // BRAID_TEST_REDIS_DB 值;"" = 未设(空串等价未设)
		base int
		want int
	}{
		{"default slot0 relay", "", 0, 0},
		{"default slot0 worker", "", 1, 1},
		{"explicit slot0 relay", "0", 0, 0},
		{"slot1 worker", "1", 1, 5},
		{"slot2 relay", "2", 0, 8},
		{"slot3 reaper", "3", 2, 14},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("BRAID_TEST_REDIS_DB", c.slot)
			got, err := TestRedisDB(c.base)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != c.want {
				t.Fatalf("DB = %d, want %d", got, c.want)
			}
		})
	}
}

// 越界/非法槽位必须报错:绝对号 >15 经 redistest 的 0..15 校验会被静默
// 回退 DB 0,并行 worktree 互踩(已知失败模式)。
func TestRedisDBRejectsBadSlot(t *testing.T) {
	for _, v := range []string{"4", "-1", "x", "16", "0x2"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("BRAID_TEST_REDIS_DB", v)
			if _, err := TestRedisDB(0); err == nil {
				t.Fatalf("槽位 %q 未被拒绝", v)
			}
		})
	}
}
