package api

// OIDC 路由面(T11.1)。零栈面:挂载条件、start 形状(302/cookie 属性)、
// callback 错误映射(400/401/403/500)+ 指标发射口径;配置解析
// (ConfigFromEnv → OIDC 字段)。t13 面:假 IdP 全链 HTTP 旅程 + login_audit
// 行断言(oidctest 基建)。
//
// 变异自查:Router 挂载去 nil 门 → not_mounted 行红;错误映射互换 →
// mapping 表各行红;清 cookie 删除 → clears_cookie 行红;审计发射删除 →
// integration 的 login_audit 行断言红。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"braid/internal/auth"
	"braid/internal/auth/oidctest"
	"braid/internal/db/sqltest"
	"braid/internal/metrics"
	"braid/internal/snowflake"
	"braid/internal/tools"
)

// fakeOIDC 是 oidcAuther 桩(零栈面);记录入参供透传断言。
type fakeOIDC struct {
	start       auth.OIDCStart
	startErr    error
	user        auth.User
	subject     string
	completeErr error

	seenCookie, seenState, seenCode string
}

func (f *fakeOIDC) StartAuth() (auth.OIDCStart, error) { return f.start, f.startErr }

func (f *fakeOIDC) Complete(_ context.Context, cookieValue, state, code string) (auth.User, string, error) {
	f.seenCookie, f.seenState, f.seenCode = cookieValue, state, code
	return f.user, f.subject, f.completeErr
}

// newOIDCTestServer 构造挂(或不挂)OIDC 桩的 server;登录防御旋钮放宽
// (newIDPTestRouter 同判)。不触碰 newTestServer(t9.2 冲突面,issue #82
// 注记 4)。返回 router 与 *Server(指标注入用)。
func newOIDCTestServer(t *testing.T, o oidcAuther) (*gin.Engine, *Server) {
	t.Helper()
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	reg, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	gin.SetMode(gin.TestMode)
	srv := NewServer(Config{Addr: DefaultAddr, JWTSecret: testSecret, SnowflakeNode: 1,
		LoginRateLimit: 100000, LoginRateWindow: time.Minute,
		LoginMaxFailures: 100000, LoginLockout: time.Minute},
		nil, ids, reg, fakeTrigger{id: 1}, auth.BuiltinProvider{})
	srv.oidc = o // 测试桩直赋(生产唯一实现经 OIDCProvider setter)
	return srv.Router(), srv
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

func attempts(t *testing.T, m *metrics.LoginSet, result string) float64 {
	t.Helper()
	return testutil.ToFloat64(m.Attempts.WithLabelValues(result))
}

// TestOIDCRoutesNotMountedWithoutProvider:未注入 = 路由不存在(未配置即
// 不注册,issue #82「未配置零行为」)。
func TestOIDCRoutesNotMountedWithoutProvider(t *testing.T) {
	r, _ := newOIDCTestServer(t, nil)
	for _, path := range []string{"/api/v1/auth/oidc/login", "/api/v1/auth/oidc/callback"} {
		if w := doGet(t, r, path, nil); w.Code != http.StatusNotFound {
			t.Fatalf("%s code = %d, want 404", path, w.Code)
		}
	}
}

// TestOIDCStartRedirectAndCookie:302 目标逐字透传 + 票根 cookie 属性。
func TestOIDCStartRedirectAndCookie(t *testing.T) {
	const target = "https://idp.example.com/authorize?client_id=x&nonce=y"
	r, _ := newOIDCTestServer(t, &fakeOIDC{start: auth.OIDCStart{
		RedirectURL: target, CookieValue: "stub-cookie-value",
	}})
	w := doGet(t, r, "/api/v1/auth/oidc/login", nil)
	if w.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302", w.Code)
	}
	if got := w.Header().Get("Location"); got != target {
		t.Fatalf("location = %q", got)
	}
	ck := w.Header().Get("Set-Cookie")
	for _, want := range []string{
		"braid_oidc_state=stub-cookie-value",
		"Path=/api/v1/auth/oidc",
		"HttpOnly",
		"SameSite=Lax",
		"Max-Age=600",
	} {
		if !strings.Contains(ck, want) {
			t.Fatalf("Set-Cookie %q 缺 %q", ck, want)
		}
	}
}

// TestOIDCCallbackClearsStateCookie:握手态一次性——callback 无论后续
// 成败先清票根。
func TestOIDCCallbackClearsStateCookie(t *testing.T) {
	r, _ := newOIDCTestServer(t, &fakeOIDC{completeErr: errors.New("boom")})
	w := doGet(t, r, "/api/v1/auth/oidc/callback?code=c", &http.Cookie{
		Name: auth.OIDCStateCookie, Value: "stub",
	})
	ck := w.Header().Get("Set-Cookie")
	if !strings.Contains(ck, auth.OIDCStateCookie+"=") || !strings.Contains(ck, "Max-Age=0") {
		t.Fatalf("清 cookie 缺席: %q", ck)
	}
}

// TestOIDCCallbackErrorMapping:错误族分层映射 + 指标口径(传输面只指标,
// 身份面指标,故障面零)。
func TestOIDCCallbackErrorMapping(t *testing.T) {
	okUser := auth.User{Username: "stub-ada", Roles: []auth.Role{auth.RoleApprover}, Status: auth.StatusActive}
	cases := []struct {
		name       string
		query      string
		noCookie   bool // true = 不带 state cookie(state_cookie_missing 面)
		fake       *fakeOIDC
		wantCode   int
		wantErr    string
		wantMetric string // 期望递增的 result label;空 = 断言全部不增
	}{
		{
			name: "state_cookie_missing", query: "code=c", noCookie: true,
			fake: &fakeOIDC{}, wantCode: 400, wantErr: "bad_request", wantMetric: metrics.ResultFailed,
		},
		{
			name: "idp_error_redirect", query: "error=access_denied",
			fake: &fakeOIDC{}, wantCode: 400, wantErr: "bad_request", wantMetric: metrics.ResultFailed,
		},
		{
			name: "code_missing", query: "",
			fake: &fakeOIDC{}, wantCode: 400, wantErr: "bad_request", wantMetric: metrics.ResultFailed,
		},
		{
			name: "state_rejected", query: "code=c",
			fake:     &fakeOIDC{completeErr: fmt.Errorf("%w: tampered", auth.ErrOIDCState)},
			wantCode: 400, wantErr: "bad_request", wantMetric: metrics.ResultFailed,
		},
		{
			name: "code_exchange_failed", query: "code=c",
			fake:     &fakeOIDC{completeErr: fmt.Errorf("%w: invalid_grant", auth.ErrOIDCCode)},
			wantCode: 400, wantErr: "bad_request", wantMetric: metrics.ResultFailed,
		},
		{
			name: "credentials_401", query: "code=c",
			fake:     &fakeOIDC{completeErr: &auth.CredentialsError{Username: "stub-ada", Reason: auth.ReasonUnknownUser}},
			wantCode: 401, wantErr: "invalid_credentials", wantMetric: metrics.ResultFailed,
		},
		{
			name: "disabled_403", query: "code=c",
			fake:     &fakeOIDC{subject: "stub-ada", completeErr: fmt.Errorf("%w: %q", auth.ErrUserDisabled, "stub-ada")},
			wantCode: 403, wantErr: "user_disabled", wantMetric: metrics.ResultFailed,
		},
		{
			name: "idtoken_fault_500", query: "code=c",
			fake:     &fakeOIDC{completeErr: fmt.Errorf("%w: bad sig", auth.ErrOIDCIDToken)},
			wantCode: 500, wantErr: "internal", wantMetric: "",
		},
		{
			name: "opaque_500", query: "code=c",
			fake:     &fakeOIDC{completeErr: errors.New("idp: downstream unavailable")},
			wantCode: 500, wantErr: "internal", wantMetric: "",
		},
		{
			name: "success", query: "code=c&state=echo-xyz",
			fake:     &fakeOIDC{user: okUser, subject: "stub-ada"},
			wantCode: 200, wantErr: "", wantMetric: metrics.ResultSucceeded,
		},
	}
	cookie := &http.Cookie{Name: auth.OIDCStateCookie, Value: "stub"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mset, _ := metrics.NewSet()
			r, srv := newOIDCTestServer(t, tc.fake)
			srv.LoginMetrics(mset.Login)
			var ck *http.Cookie
			if !tc.noCookie {
				ck = cookie
			}
			w := doGet(t, r, "/api/v1/auth/oidc/callback?"+tc.query, ck)
			if w.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d (body %s)", w.Code, tc.wantCode, w.Body)
			}
			body := w.Body.String()
			if tc.wantErr != "" && !strings.Contains(body, tc.wantErr) {
				t.Fatalf("body %s 缺 %q", body, tc.wantErr)
			}
			// 500 响应体只许 error 键(不透明故障无泄漏契约)。
			if tc.wantCode == 500 && !strings.HasPrefix(strings.TrimSpace(body), `{"error":"internal"}`) {
				t.Fatalf("500 body = %s, want 仅 error 键", body)
			}
			for _, label := range []string{metrics.ResultSucceeded, metrics.ResultFailed, metrics.ResultLocked, metrics.ResultRateLimited} {
				want := 0.0
				if tc.wantMetric == label {
					want = 1
				}
				if got := attempts(t, mset.Login, label); got != want {
					t.Fatalf("metric[%s] = %v, want %v", label, got, want)
				}
			}
			if tc.wantCode == http.StatusOK {
				// token 携带面禁缓存(review LOW 收口)。
				if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
					t.Fatalf("Cache-Control = %q, want no-store", cc)
				}
				// 回调 query 逐字到达缝(state 透传供回显比对)。
				if tc.fake.seenState != "echo-xyz" || tc.fake.seenCode != "c" || tc.fake.seenCookie != "stub" {
					t.Fatalf("入参未逐字到达桩: cookie=%q state=%q code=%q",
						tc.fake.seenCookie, tc.fake.seenState, tc.fake.seenCode)
				}
				claims, err := auth.ParseToken(testSecret, extractJSONField(t, body, "token"))
				if err != nil {
					t.Fatalf("parse: %v", err)
				}
				if claims.Subject != "stub-ada" || len(claims.Roles) != 1 || claims.Roles[0] != auth.RoleApprover {
					t.Fatalf("claims = %+v", claims)
				}
			}
		})
	}
}

func extractJSONField(t *testing.T, body, field string) string {
	t.Helper()
	// 轻量提取:登录响应契约由 login_idp_test 钉死,此处只取 token 值。
	m := map[string]any{}
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("body: %v", err)
	}
	v, _ := m[field].(string)
	if v == "" {
		t.Fatalf("body %s 缺 %q", body, field)
	}
	return v
}

// TestConfigFromEnvOIDCGroup:api 配置面承接 BRAID_OIDC_ 组(半开在
// ConfigFromEnv 即败;全组缺省 = 关闭零值)。
func TestConfigFromEnvOIDCGroup(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		c, err := ConfigFromEnv(func(k string) string {
			if k == "BRAID_AUTH_JWT_SECRET" {
				return testSecret
			}
			return ""
		})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if c.OIDC.Configured() {
			t.Fatalf("off 组报告 configured")
		}
	})
	t.Run("half_open_rejected", func(t *testing.T) {
		_, err := ConfigFromEnv(func(k string) string {
			if k == "BRAID_AUTH_JWT_SECRET" {
				return testSecret
			}
			if k == "BRAID_OIDC_ISSUER" {
				return "https://idp.example.com"
			}
			return ""
		})
		if err == nil || !strings.Contains(err.Error(), "half-configured") {
			t.Fatalf("err = %v, want half-configured", err)
		}
	})
	t.Run("full_group", func(t *testing.T) {
		c, err := ConfigFromEnv(func(k string) string {
			switch k {
			case "BRAID_AUTH_JWT_SECRET":
				return testSecret
			case "BRAID_OIDC_ISSUER":
				return "https://idp.example.com"
			case "BRAID_OIDC_CLIENT_ID":
				return "braid"
			case "BRAID_OIDC_CLIENT_SECRET":
				return "client-secret-words-placeholder"
			case "BRAID_OIDC_REDIRECT_URL":
				return "https://braid.example.com/api/v1/auth/oidc/callback"
			default:
				return ""
			}
		})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !c.OIDC.Configured() || c.OIDC.Claim != auth.DefaultOIDCClaim {
			t.Fatalf("oidc = %+v", c.OIDC)
		}
	})
}

// TestOIDCFlowIntegration(t13):假 IdP 全链 HTTP 旅程——login 302 →
// authorize 302 → callback 200 token 往返 + login_audit 行(成功/未知
// 账号两臂)。真 OIDCProvider + 真 DB(本地锚定),装配经生产 setter。
func TestOIDCFlowIntegration(t *testing.T) {
	dbh := sqltest.MustOpenAt(t, "braid_api_test")
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
		prov, err := auth.NewOIDCProvider(context.Background(), auth.OIDCConfig{
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
		srv := NewServer(Config{Addr: DefaultAddr, JWTSecret: testSecret, SnowflakeNode: 1,
			LoginRateLimit: 100000, LoginRateWindow: time.Minute,
			LoginMaxFailures: 100000, LoginLockout: time.Minute},
			dbh, ids, reg, fakeTrigger{id: 1}, auth.BuiltinProvider{DB: dbh})
		srv.OIDCProvider(prov) // 生产 setter 装配面
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
		claims, err := auth.ParseToken(testSecret, extractJSONField(t, body, "token"))
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

	rows, err := dbh.Query(`SELECT event_type, username, reason FROM login_audit WHERE username IN ('oidc-alice','oidc-ghost')`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close() //nolint:errcheck // 测试收尾
	var got []string
	for rows.Next() {
		var ev, un string
		var reasonPtr *string
		if err := rows.Scan(&ev, &un, &reasonPtr); err != nil {
			t.Fatalf("scan: %v", err)
		}
		reason := ""
		if reasonPtr != nil {
			reason = *reasonPtr
		}
		got = append(got, fmt.Sprintf("%s/%s/%s", ev, un, reason))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := []string{
		"login.succeeded/oidc-alice/",
		"login.failed/oidc-ghost/" + auth.ReasonUnknownUser,
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
