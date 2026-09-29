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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"braid/internal/auth"
	"braid/internal/db/sqltest"
	"braid/internal/snowflake"
	"braid/internal/tools"
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

// newBoundLoginServer 是登录签发→中间件校验闭环的绑定模式 server
// (T11.3;防御旋钮放宽同 newTestServer)。
func newBoundLoginServer(t *testing.T, mode TokenBindingMode) (*gin.Engine, *sql.DB) {
	t.Helper()
	dbh := sqltest.MustOpenAt(t, "braid_api_test")
	cleanExecutionChain(t, dbh)
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	reg, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	gin.SetMode(gin.TestMode)
	cfg := Config{Addr: DefaultAddr, JWTSecret: testSecret, SnowflakeNode: 1,
		LoginRateLimit: 100000, LoginRateWindow: time.Minute,
		LoginMaxFailures: 100000, LoginLockout: time.Minute,
		TokenBinding: mode}
	return NewServer(cfg, dbh, ids, reg, fakeTrigger{id: 1}, auth.BuiltinProvider{DB: dbh}).Router(), dbh
}

// boundLogin 用给定 UA 登录 carol,返回签出的 token(t13 面)。
func boundLogin(t *testing.T, r *gin.Engine, ua string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"username":"carol","password":"correct-horse-battery"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login code=%d body=%q", w.Code, w.Body.String())
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Token == "" {
		t.Fatalf("no token: %q err=%v", w.Body.String(), err)
	}
	return out.Token
}

// boundAuthedGet 对真实 server router 发带 UA/RemoteAddr/Bearer 的读面
// 请求,返回状态码与错误码(doJSONHeaders 无 RemoteAddr 控制,ip24 模式
// 需要它)。
func boundAuthedGet(r *gin.Engine, path, token, ua, remoteAddr string) (int, string) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("User-Agent", ua)
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body.Error
}

// seedBoundUser 清库 + 种 carol(三种模式的 server 共用)。
func seedBoundUser(t *testing.T, dbh *sql.DB) {
	t.Helper()
	for _, q := range []string{"DELETE FROM user_role", "DELETE FROM users"} {
		if _, err := dbh.Exec(q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
	seedUser(t, dbh, "carol", auth.RoleAuthor)
}

// TestLoginIssuesBoundToken:两种开启模式的签发→校验 HTTP 闭环(D-t113
// #1)。ua_ip24 半边把签发点的 IP 接线钉进闭环——对抗审计 MEDIUM:该模式
// 原先在两签发点零 HTTP 覆盖,签发侧 IP 误传的变异全绿、上线即全断。
func TestLoginIssuesBoundToken(t *testing.T) {
	const ua = "braid-it/1.0"

	t.Run("ua", func(t *testing.T) {
		r, dbh := newBoundLoginServer(t, TokenBindingUA)
		seedBoundUser(t, dbh)
		tok := boundLogin(t, r, ua)
		if code, errc := boundAuthedGet(r, "/api/v1/executions", tok, ua, ""); code != http.StatusOK {
			t.Fatalf("同 UA 读面 = %d %s", code, errc)
		}
		// 异 UA:外泄重放形态。
		code, errc := boundAuthedGet(r, "/api/v1/executions", tok, "curl/8.0", "")
		if code != http.StatusUnauthorized || errc != "token_binding_mismatch" {
			t.Fatalf("异 UA = %d %s, want 401 token_binding_mismatch", code, errc)
		}
	})

	t.Run("ua_ip24", func(t *testing.T) {
		r, dbh := newBoundLoginServer(t, TokenBindingUAIP24)
		seedBoundUser(t, dbh)
		// 登录请求 RemoteAddr = httptest 缺省 192.0.2.1:token 绑
		// 192.0.2.0/24 段(签发点 IP 接线的真锚)。
		tok := boundLogin(t, r, ua)
		if code, errc := boundAuthedGet(r, "/api/v1/executions", tok, ua, "192.0.2.99:777"); code != http.StatusOK {
			t.Fatalf("同 /24 读面 = %d %s", code, errc)
		}
		code, errc := boundAuthedGet(r, "/api/v1/executions", tok, ua, "192.0.3.1:777")
		if code != http.StatusUnauthorized || errc != "token_binding_mismatch" {
			t.Fatalf("跨 /24 = %d %s, want 401 token_binding_mismatch", code, errc)
		}
		// UA 漂移在 ip24 模式同样失配。
		if code, _ := boundAuthedGet(r, "/api/v1/executions", tok, "curl/8.0", "192.0.2.1:777"); code != http.StatusUnauthorized {
			t.Fatalf("ip24 模式 UA 漂移未拒: code=%d", code)
		}
	})
}

// TestLoginOffModeIssuesUnboundToken:off(缺省)签发面在 api 装配层的
// 语义锚——签出 token 不带 bnd claim 且读面可用(对抗审计 LOW:该语义
// 原先只在 auth 单元层被锚)。
func TestLoginOffModeIssuesUnboundToken(t *testing.T) {
	r, dbh := newBoundLoginServer(t, TokenBindingOff)
	seedBoundUser(t, dbh)
	tok := boundLogin(t, r, "braid-it/1.0")
	claims, err := auth.ParseToken(testSecret, tok)
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	if claims.Binding != "" {
		t.Fatalf("off 模式签出 token 携 bnd = %q, want 无 claim", claims.Binding)
	}
	if code, errc := boundAuthedGet(r, "/api/v1/executions", tok, "curl/8.0", ""); code != http.StatusOK {
		t.Fatalf("off 模式读面 = %d %s(零介入,UA 无关)", code, errc)
	}
}
