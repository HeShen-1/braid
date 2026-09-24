package api

// T4.4 登录面 HTTP 契约测试(t13 真 MySQL,不可达自动 skip):凭据换取
// Bearer token。防枚举语义继承 auth.Authenticate(未知用户/错密码/域外
// 输入统一 401 invalid_credentials,时序均衡);DISABLED 仅持正确凭证可
// 达(403 user_disabled);签出的 token 必须能过既有 Authenticate 中间件
// (读面 200)——登录面与中间件同 secret 同 claims 形状的闭环锚。
//
// 变异自查:错误分支映射互换 → unknown/wrong_password 用例红(码断言);
// SignToken 换错 secret → token_usable 用例红;DISABLED 判定提前到密码
// 验证前(密码错误也 403)→ disabled_wrong_password 用例红。

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"braid/internal/auth"
	"braid/internal/snowflake"
)

// loginTestIDs 登录面测试的雪花源(节点 8;节点号分配见 approval_test.go
// approvalTestIDs 注释;users 表 PK 全库雪花唯一,节点号无跨表约束)。
var loginTestIDs, _ = snowflake.New(8)

// seedUser 建一个可登录用户(bcrypt.MinCost 换速度,生产 cost 见
// auth.Config.DefaultBcryptCost);同名撞 uk → 测试自身 bug,t.Fatal。
func seedUser(t *testing.T, dbh *sql.DB, username string, roles ...auth.Role) {
	t.Helper()
	hash, err := auth.HashPassword("correct-horse-battery", bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := auth.CreateUser(t.Context(), dbh, loginTestIDs,
		auth.CreateUserParams{Username: username, PasswordHash: hash, Roles: roles}); err != nil {
		t.Fatalf("create user %s: %v", username, err)
	}
}

func TestLogin(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	// users/user_role 消费方 = 本文件 + webjourney_test(两处均先清后种,
	// api 包内串行零 t.Parallel;后续新增 users 消费测试同样必须自清)。
	for _, q := range []string{"DELETE FROM user_role", "DELETE FROM users"} {
		if _, err := dbh.Exec(q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}

	seedUser(t, dbh, "carol", auth.RoleApprover, auth.RoleAuthor)
	seedUser(t, dbh, "dave", auth.RoleApprover)
	// eve:DISABLED(密码验证通过后才暴露的态,SQL 直改状态列)。
	seedUser(t, dbh, "eve", auth.RoleApprover)
	if _, err := dbh.Exec("UPDATE users SET status='DISABLED' WHERE username='eve'"); err != nil {
		t.Fatalf("disable eve: %v", err)
	}

	login := func(body string) (int, map[string]any) {
		t.Helper()
		return doJSON(t, r, http.MethodPost, "/api/v1/auth/login", "", []byte(body))
	}

	t.Run("success_token_usable", func(t *testing.T) {
		code, out := login(`{"username":"carol","password":"correct-horse-battery"}`)
		if code != http.StatusOK {
			t.Fatalf("code = %d, body %v", code, out)
		}
		tok, _ := out["token"].(string)
		if tok == "" {
			t.Fatalf("no token in response: %v", out)
		}
		if out["username"] != "carol" {
			t.Fatalf("username = %v", out["username"])
		}
		// 签出的 token 必须过既有 Authenticate:读面 200 即闭环。
		code, _ = doJSON(t, r, http.MethodGet, "/api/v1/executions", tok, nil)
		if code != http.StatusOK {
			t.Fatalf("token not accepted by Authenticate: %d", code)
		}
	})

	t.Run("wrong_password", func(t *testing.T) {
		code, out := login(`{"username":"carol","password":"nope"}`)
		if code != http.StatusUnauthorized || out["error"] != "invalid_credentials" {
			t.Fatalf("code=%d error=%v, want 401 invalid_credentials", code, out["error"])
		}
	})

	t.Run("unknown_user_same_face", func(t *testing.T) {
		code, out := login(`{"username":"ghost","password":"whatever-pass"}`)
		if code != http.StatusUnauthorized || out["error"] != "invalid_credentials" {
			t.Fatalf("code=%d error=%v, want 401 invalid_credentials(与错密码同面)", code, out["error"])
		}
	})

	t.Run("out_of_domain_input", func(t *testing.T) {
		// 域外 username/空口令:不可能命中真实凭证,统一 401 同码。
		for _, body := range []string{
			`{"username":"Alice","password":"correct-horse-battery"}`,
			`{"username":"carol","password":""}`,
		} {
			code, out := login(body)
			if code != http.StatusUnauthorized || out["error"] != "invalid_credentials" {
				t.Fatalf("body %s: code=%d error=%v, want 401 invalid_credentials", body, code, out["error"])
			}
		}
	})

	t.Run("disabled", func(t *testing.T) {
		code, out := login(`{"username":"eve","password":"correct-horse-battery"}`)
		if code != http.StatusForbidden || out["error"] != "user_disabled" {
			t.Fatalf("code=%d error=%v, want 403 user_disabled", code, out["error"])
		}
	})

	t.Run("disabled_wrong_password_stands_unified", func(t *testing.T) {
		// 状态探测防线:DISABLED + 错密码必须仍是 401(判定顺序钉死)。
		code, out := login(`{"username":"eve","password":"nope"}`)
		if code != http.StatusUnauthorized || out["error"] != "invalid_credentials" {
			t.Fatalf("code=%d error=%v, want 401 invalid_credentials", code, out["error"])
		}
	})

	t.Run("bad_json", func(t *testing.T) {
		code, out := login(`{not json`)
		if code != http.StatusBadRequest || out["error"] != "bad_request" {
			t.Fatalf("code=%d error=%v, want 400 bad_request", code, out["error"])
		}
	})
}

// TestLogUsernameBounded:登录失败日志的主体名有界化(对抗审计 HIGH:
// invalid_input 路径不付 bcrypt 成本,未截断的 1MiB 原文逐请求进日志 =
// 无上界日志放大)。变异自查:截断分支删除 → 超限用例红(返回原文);
// rune 边界回退删除 → 多字节用例红(截出非法 UTF-8 尾)。
func TestLogUsernameBounded(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"short_passthrough", "carol", "carol"},
		{"boundary_exact", strings.Repeat("a", maxLogUsername), strings.Repeat("a", maxLogUsername)},
		{
			"ascii_over_limit",
			strings.Repeat("a", maxLogUsername+100),
			strings.Repeat("a", maxLogUsername) + fmt.Sprintf("...(len=%d)", maxLogUsername+100),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := logUsername(tt.in); got != tt.want {
				t.Fatalf("logUsername = %q(len %d), want %q", got, len(got), tt.want)
			}
		})
	}
	// 多字节安全:截断点落在 rune 中段时回退到 rune 边界,产物是合法 UTF-8。
	t.Run("multibyte_rune_boundary", func(t *testing.T) {
		in := strings.Repeat("张", 40) // 120 字节,截断点 64 落在第 21 个「张」中段
		got := logUsername(in)
		if !utf8.ValidString(got) {
			t.Fatalf("截断产物非合法 UTF-8: %q", got)
		}
		if !strings.HasPrefix(got, strings.Repeat("张", 21)) {
			t.Fatalf("截断前缀丢字: %q", got)
		}
		if !strings.HasSuffix(got, fmt.Sprintf("(len=%d)", len(in))) {
			t.Fatalf("缺长度事实: %q", got)
		}
	})
}
