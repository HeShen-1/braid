package api

// OIDC 路由面(T11.1;T16.1 收线,协议实现迁企业增值面)。零栈面:挂载
// 条件、start 形状(302/cookie 属性)、callback 错误映射(400/401/403/500)
// + 指标发射口径——全部经 OIDCProvider 缝桩(fakeOIDC),零企业依赖。
// t13 全链旅程(真 provider × handler × login_audit)随实现迁企业侧
// (internal/enterprise/oidc/apiflow_test.go);配置解析随 OIDC 字段
// 消亡移除(企业侧 ParseOIDCConfig 测试承载)。
//
// 变异自查:Router 挂载去 nil 门 → not_mounted 行红;错误映射互换 →
// mapping 表各行红;清 cookie 删除 → clears_cookie 行红;审计发射删除 →
// integration 的 login_audit 行断言红(企业侧)。

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
	"braid/internal/metrics"
	"braid/internal/snowflake"
	"braid/internal/tools"
)

// fakeOIDC 是 OIDCProvider 缝桩(零栈面);记录入参供透传断言。
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
func newOIDCTestServer(t *testing.T, o OIDCProvider) (*gin.Engine, *Server) {
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
	srv := NewServer(Config{Addr: DefaultAddr, JWTSecret: testSecret, SnowflakeNode: snowflake.Explicit(1),
		LoginRateLimit: 100000, LoginRateWindow: time.Minute,
		LoginMaxFailures: 100000, LoginLockout: time.Minute},
		nil, ids, reg, fakeTrigger{id: 1}, auth.BuiltinProvider{})
	srv.oidc = o // 测试桩直赋(生产唯一实现 = 企业树,经 OIDCProvider setter)
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
			// ErrOIDCIDToken 哨兵随实现迁企业侧(T16.1);本臂用普通故障
			// 错误锚 default 500 分支——真哨兵经缝的 500 映射由企业侧
			// apiflow_test 钉(internal/enterprise/oidc)。
			name: "idtoken_fault_500", query: "code=c",
			fake:     &fakeOIDC{completeErr: errors.New("idp: id token bad sig")},
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

// TestOIDCCallbackIssuesBoundToken:模式开启下回调签出的 token 携 bnd
// claim = 回调请求指纹(与口令登录同契约,D-t113 #1 两签发点);两种
// 开启模式都锚(对抗审计:ua_ip24 签发点原先零覆盖)。nil DB = 桩构造,
// writeLoginAudit 容忍(dbh nil 跳审计写)。
func TestOIDCCallbackIssuesBoundToken(t *testing.T) {
	const ua = "braid-oidc-it/1.0"
	for _, mode := range []TokenBindingMode{TokenBindingUA, TokenBindingUAIP24} {
		t.Run(string(mode), func(t *testing.T) {
			ids, err := snowflake.New(1)
			if err != nil {
				t.Fatalf("snowflake: %v", err)
			}
			reg, err := tools.NewRegistry()
			if err != nil {
				t.Fatalf("registry: %v", err)
			}
			gin.SetMode(gin.TestMode)
			srv := NewServer(Config{Addr: DefaultAddr, JWTSecret: testSecret, SnowflakeNode: snowflake.Explicit(1),
				LoginRateLimit: 100000, LoginRateWindow: time.Minute,
				LoginMaxFailures: 100000, LoginLockout: time.Minute,
				TokenBinding: mode},
				nil, ids, reg, fakeTrigger{id: 1}, auth.BuiltinProvider{})
			srv.oidc = &fakeOIDC{user: auth.User{Username: "carol", Roles: []auth.Role{auth.RoleAuthor}}}
			r := srv.Router()

			req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code=c&state=s", nil)
			req.Header.Set("User-Agent", ua)
			req.AddCookie(&http.Cookie{Name: auth.OIDCStateCookie, Value: "state-cookie"})
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("callback code=%d body=%q", w.Code, w.Body.String())
			}
			var out struct {
				Token string `json:"token"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Token == "" {
				t.Fatalf("no token: %q err=%v", w.Body.String(), err)
			}
			claims, err := auth.ParseToken(testSecret, out.Token)
			if err != nil {
				t.Fatalf("ParseToken: %v", err)
			}
			// 回调请求 RemoteAddr = httptest 缺省 192.0.2.1(Router 的
			// SetTrustedProxies(nil) 姿态下 ClientIP = RemoteAddr)。
			if want := bindingFingerprint(mode, ua, "192.0.2.1"); claims.Binding != want {
				t.Fatalf("bnd = %q, want %q(回调请求指纹,mode=%s)", claims.Binding, want, mode)
			}
			// 中间件「同 UA 放行/异 UA 拒」的闭环不在此重复锚(nil-DB
			// 桩面,authed 路由触 DB;middleware_test 与 login_test 已
			// 各自闭环)。
		})
	}
}
