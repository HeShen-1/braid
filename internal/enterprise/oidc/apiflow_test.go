package oidc_test

// 企业 OIDC × 核心 api 路由面的全链集成(T11.1 立面,T16.1 随协议实现迁
// 企业侧,D-2026-10-01-t161-engine-enterprise-split):真 OIDCProvider(本包)
// 经生产 setter 注入核心 Server,假 IdP(oidctest)驱动 login 302 →
// authorize 302 → callback 200 token 往返 + login_audit 行断言(成功/未知
// 账号两臂)。企业→核心单向依赖的活体锚(boundary 门之外的行为面证明)。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"braid/internal/api"
	"braid/internal/auth"
	"braid/internal/db/sqltest"
	"braid/internal/enterprise/oidc"
	"braid/internal/enterprise/oidc/oidctest"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
	"braid/internal/tools"
)

// 测试密钥纪律(gitleaks 实测,api 包同款):低熵词组占位,≥32 字节;
// token 一律测试内签发。
const testSecret = "unit-test-jwt-signing-secret-words-only-placeholder"

// flowSchema:本测试文件自开 base(api 包 braid_api_test 同纪律;独立名
// 避免与 api 包测试并行互踩,schema 名经 TestSchema 自动拼 worktree 后缀)。
const flowSchema = "braid_ent_oidc_api_test"

// fakeTrigger 是 api.ExecutionTrigger 缝桩(api 包测试同形;本包外测试
// 无法触达包内桩,复制两方法最小面)。
type fakeTrigger struct {
	id  uint64
	err error
}

func (f fakeTrigger) Instantiate(context.Context, scheduler.InstantiateParams) (uint64, error) {
	return f.id, f.err
}

func (f fakeTrigger) RollbackTo(_ context.Context, p scheduler.RollbackParams) (*scheduler.RollbackResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &scheduler.RollbackResult{TargetNodeID: p.TargetNodeID}, nil
}

// CancelExecution(T19.3):fake 面与 RollbackTo 同构。
func (f fakeTrigger) CancelExecution(context.Context, scheduler.CancelParams) (*scheduler.CancelResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &scheduler.CancelResult{CancelledCount: 1}, nil
}

func doGet(t *testing.T, r http.Handler, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// faultProvider 是 api.OIDCProvider 缝桩(固定 Complete 错误):把本包
// ErrOIDCIDToken 经生产 setter 送进核心 handler,钉「身份源故障 → 500、
// 不计数不审计」映射(core 侧错误表无法引用企业哨兵,该契约锚在此)。
type faultProvider struct{ err error }

func (f faultProvider) StartAuth() (auth.OIDCStart, error) {
	return auth.OIDCStart{}, f.err
}

func (f faultProvider) Complete(context.Context, string, string, string) (auth.User, string, error) {
	return auth.User{}, "", f.err
}

// TestOIDCIDTokenFaultMaps500:ErrOIDCIDToken(身份源故障面)经核心回调
// handler = 500 仅 error 键、登录指标零递增(T11.1 错误族契约,零栈)。
func TestOIDCIDTokenFaultMaps500(t *testing.T) {
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	reg, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	gin.SetMode(gin.TestMode)
	srv := api.NewServer(api.Config{Addr: api.DefaultAddr, JWTSecret: testSecret, SnowflakeNode: snowflake.Explicit(1),
		LoginRateLimit: 100000, LoginRateWindow: time.Minute,
		LoginMaxFailures: 100000, LoginLockout: time.Minute},
		nil, ids, reg, fakeTrigger{id: 1}, auth.BuiltinProvider{})
	srv.OIDCProvider(faultProvider{err: fmt.Errorf("%w: bad sig", oidc.ErrOIDCIDToken)})
	w := doGet(t, srv.Router(), "/api/v1/auth/oidc/callback?code=c",
		&http.Cookie{Name: auth.OIDCStateCookie, Value: "stub"})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d body %s", w.Code, w.Body)
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"error":"internal"}` {
		t.Fatalf("body = %s, want 仅 error 键", got)
	}
}

// TestOIDCFlowIntegration(t13):假 IdP 全链 HTTP 旅程——login 302 →
// authorize 302 → callback 200 token 往返 + login_audit 行(成功/未知
// 账号两臂)。真 OIDCProvider + 真 DB(本地锚定),装配经生产 setter。
func TestOIDCFlowIntegration(t *testing.T) {
	dbh := sqltest.MustOpenAt(t, flowSchema)
	t.Cleanup(func() {
		_, _ = dbh.Exec(`DELETE FROM user_role WHERE user_id IN (SELECT id FROM users WHERE username LIKE 'oidc-%')`)
		_, _ = dbh.Exec(`DELETE FROM users WHERE username LIKE 'oidc-%'`)
		_, _ = dbh.Exec(`DELETE FROM login_audit WHERE username LIKE 'oidc-%'`)
	})
	hash, err := auth.HashPassword("pw-unused-oidc-1", 4)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	if err := auth.WithTx(context.Background(), dbh, func(q auth.DBTX) error {
		return auth.CreateUser(context.Background(), q, ids, auth.CreateUserParams{
			Username: "oidc-alice", PasswordHash: hash, Roles: []auth.Role{auth.RoleApprover},
		})
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	build := func(claims map[string]any) *gin.Engine {
		t.Helper()
		idp := oidctest.New(t)
		idp.Claims = claims
		prov, err := oidc.NewOIDCProvider(context.Background(), oidc.OIDCConfig{
			Issuer:       idp.Issuer(),
			ClientID:     idp.ClientID,
			ClientSecret: "integration-oidc-client-secret-placeholder",
			RedirectURL:  "http://127.0.0.1:1/api/v1/auth/oidc/callback",
		}, dbh)
		if err != nil {
			t.Fatalf("provider: %v", err)
		}
		reg, err := tools.NewRegistry()
		if err != nil {
			t.Fatalf("registry: %v", err)
		}
		gin.SetMode(gin.TestMode)
		srv := api.NewServer(api.Config{Addr: api.DefaultAddr, JWTSecret: testSecret, SnowflakeNode: snowflake.Explicit(1),
			LoginRateLimit: 100000, LoginRateWindow: time.Minute,
			LoginMaxFailures: 100000, LoginLockout: time.Minute},
			dbh, ids, reg, fakeTrigger{id: 1}, auth.BuiltinProvider{DB: dbh})
		srv.OIDCProvider(prov) // 生产 setter 装配面(核心缝注入企业实现)
		return srv.Router()
	}
	// 全链驱动:login(本地)→ authorize(假 IdP,302)→ callback(本地),
	// 浏览器语义零跟踪。
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	flow := func(t *testing.T, r *gin.Engine) (int, string) {
		t.Helper()
		w1 := doGet(t, r, "/api/v1/auth/oidc/login", nil)
		if w1.Code != http.StatusFound {
			t.Fatalf("login code = %d, body %s", w1.Code, w1.Body)
		}
		var stateCookie *http.Cookie
		for _, c := range w1.Result().Cookies() {
			if c.Name == auth.OIDCStateCookie {
				stateCookie = c
			}
		}
		if stateCookie == nil {
			t.Fatalf("state cookie 缺席")
		}
		authz, err := client.Get(w1.Header().Get("Location"))
		if err != nil {
			t.Fatalf("authorize: %v", err)
		}
		authz.Body.Close() //nolint:errcheck // 诊断面
		if authz.StatusCode != http.StatusFound {
			t.Fatalf("authorize code = %d", authz.StatusCode)
		}
		cbReq, _ := http.NewRequest(http.MethodGet, authz.Header.Get("Location"), nil)
		cbReq.AddCookie(stateCookie)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, cbReq)
		return w.Code, w.Body.String()
	}

	t.Run("success_token_roundtrip_and_audit", func(t *testing.T) {
		r := build(map[string]any{"preferred_username": "oidc-alice"})
		code, body := flow(t, r)
		if code != http.StatusOK {
			t.Fatalf("code = %d, body %s", code, body)
		}
		m := map[string]any{}
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatalf("body: %v", err)
		}
		tok, ok := m["token"].(string)
		if !ok || tok == "" {
			t.Fatalf("no token: %q", body)
		}
		claims, err := auth.ParseToken(testSecret, tok)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if claims.Subject != "oidc-alice" || len(claims.Roles) != 1 || claims.Roles[0] != auth.RoleApprover {
			t.Fatalf("claims = %+v", claims)
		}
	})
	t.Run("unknown_user_401_and_audit", func(t *testing.T) {
		r := build(map[string]any{"preferred_username": "oidc-ghost"})
		code, body := flow(t, r)
		if code != http.StatusUnauthorized || !strings.Contains(body, "invalid_credentials") {
			t.Fatalf("code = %d body %s", code, body)
		}
	})

	rows, err := dbh.Query(`SELECT event_type, IFNULL(method,''), username, reason FROM login_audit WHERE username IN ('oidc-alice','oidc-ghost')`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close() //nolint:errcheck // 测试收尾
	var got []string
	for rows.Next() {
		var ev, method, un string
		var reasonPtr *string
		if err := rows.Scan(&ev, &method, &un, &reasonPtr); err != nil {
			t.Fatalf("scan: %v", err)
		}
		reason := ""
		if reasonPtr != nil {
			reason = *reasonPtr
		}
		got = append(got, fmt.Sprintf("%s/%s/%s/%s", ev, method, un, reason))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := []string{
		"login.succeeded/oidc/oidc-alice/",
		"login.failed/oidc/oidc-ghost/" + auth.ReasonUnknownUser,
	}
	if len(got) != len(want) {
		t.Fatalf("login_audit 行 = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("login_audit[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
