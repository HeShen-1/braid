package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"braid/internal/auth"
)

// expiredToken 直构过期 token:SignToken 只签正 TTL(生产面无负 TTL 合法
// 出口),过期面只能绕过它——与 auth 包内 signClaims 的测试策略同源。
// iss/aud 值与 auth 包常量逐字一致(单部署自签自验契约)。
func expiredToken(t *testing.T, secret, sub string, roles []auth.Role) string {
	t.Helper()
	c := auth.Claims{
		Roles: roles,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   sub,
			Issuer:    "braid",
			Audience:  jwt.ClaimStrings{"braid-api"},
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
		},
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign expired token: %v", err)
	}
	return s
}

// newTestRouter 挂认证中间件 + 可选角色闸(roles 为 nil = 仅认证),
// handler 回显 claims.Subject。绑定模式固定 off(既有契约面零变化;
// 绑定行为走 newBoundTestRouter)。
func newTestRouter(secret string, roles []auth.Role) *gin.Engine {
	return newBoundTestRouter(secret, TokenBindingOff, roles)
}

// newBoundTestRouter 是 newTestRouter 的绑定模式可控变体(T11.3)。
// 显式 SetTrustedProxies(nil):镜像生产空配置的 fail-closed 姿态
// (ClientIP 恒 RemoteAddr)——gin.New() 缺省信任一切代理,测试若跑在
// 缺省下则「未受信 XFF 不得影响绑定指纹」零锚(对抗审计 MEDIUM)。
func newBoundTestRouter(secret string, binding TokenBindingMode, roles []auth.Role) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if err := r.SetTrustedProxies(nil); err != nil {
		panic("set trusted proxies: " + err.Error())
	}
	h := func(c *gin.Context) {
		claims, _ := ClaimsFrom(c)
		c.String(http.StatusOK, "sub=%s", claims.Subject)
	}
	if roles == nil {
		r.GET("/x", Authenticate(secret, binding), h)
		return r
	}
	r.GET("/x", Authenticate(secret, binding), RequireAnyRole(roles...), h)
	return r
}

// doBound 对 newBoundTestRouter 发带 UA/RemoteAddr/Bearer 的请求,回
// (状态码, body)——绑定判定只依赖这三项,其余面沿用 doJSON 家族语义。
func doBound(r *gin.Engine, token, userAgent, remoteAddr string) (int, string) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("User-Agent", userAgent)
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// doBoundXFF 是 doBound 的 X-Forwarded-For 可控变体(TrustedProxies
// 契约传导锚专用)。
func doBoundXFF(r *gin.Engine, token, userAgent, remoteAddr, xff string) int {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Forwarded-For", xff)
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

// TestAuthenticateBinding:绑定三态契约(T11.3,D-2026-09-29-t113 #1)。
// 变异自查:删掉 middleware 的指纹比对 → same_ua/同网段用例红;把缺席
// 放行 → unbound_fail_closed 用例红;off 分支误入绑定判定 →
// off_zero_change 用例红。
func TestAuthenticateBinding(t *testing.T) {
	const ua = "braid-bind-test/1.0"
	const otherUA = "curl/8.0"
	boundUA, err := auth.SignTokenWithBinding(testSecret, time.Hour, "alice",
		[]auth.Role{auth.RoleAuthor}, 0, bindingFingerprint(TokenBindingUA, ua, "192.0.2.1"))
	if err != nil {
		t.Fatalf("sign bound token: %v", err)
	}
	unbound, err := auth.SignToken(testSecret, time.Hour, "alice", []auth.Role{auth.RoleAuthor}, 0)
	if err != nil {
		t.Fatalf("sign unbound token: %v", err)
	}

	t.Run("ua_mode", func(t *testing.T) {
		r := newBoundTestRouter(testSecret, TokenBindingUA, nil)
		if code, body := doBound(r, boundUA, ua, ""); code != 200 || body != "sub=alice" {
			t.Fatalf("同 UA: code=%d body=%q, want 200 sub=alice", code, body)
		}
		if code, body := doBound(r, boundUA, otherUA, ""); code != 401 || !strings.Contains(body, "token_binding_mismatch") {
			t.Fatalf("异 UA: code=%d body=%q, want 401 token_binding_mismatch", code, body)
		}
		// IP 不参与 ua 模式:RemoteAddr 漂移不误杀。
		if code, _ := doBound(r, boundUA, ua, "10.9.9.9:1234"); code != 200 {
			t.Fatalf("ua 模式随 IP 漂移误杀: code=%d", code)
		}
	})

	t.Run("unbound_fail_closed", func(t *testing.T) {
		// 模式开启下缺席 claim 一律拒(fail-closed:模式切换 = 全量换发
		// 语义,存量 token 的过渡放行不做)。
		r := newBoundTestRouter(testSecret, TokenBindingUA, nil)
		if code, body := doBound(r, unbound, ua, ""); code != 401 || !strings.Contains(body, "token_binding_mismatch") {
			t.Fatalf("无 claim: code=%d body=%q, want 401 token_binding_mismatch", code, body)
		}
	})

	t.Run("off_zero_change", func(t *testing.T) {
		// off = 零介入:无 claim 照常 200;带 claim 的 token 也不被绑定
		// 判定波及(claim 被忽略,语义归签发时刻模式)。
		r := newBoundTestRouter(testSecret, TokenBindingOff, nil)
		if code, _ := doBound(r, unbound, ua, ""); code != 200 {
			t.Fatalf("off 无 claim 被拒: code=%d", code)
		}
		if code, _ := doBound(r, boundUA, otherUA, ""); code != 200 {
			t.Fatalf("off 带 claim 异 UA 被拒: code=%d", code)
		}
	})

	t.Run("ua_ip24_prefix_tolerance", func(t *testing.T) {
		bound, err := auth.SignTokenWithBinding(testSecret, time.Hour, "alice",
			[]auth.Role{auth.RoleAuthor}, 0, bindingFingerprint(TokenBindingUAIP24, ua, "10.1.2.3"))
		if err != nil {
			t.Fatalf("sign ip24 token: %v", err)
		}
		r := newBoundTestRouter(testSecret, TokenBindingUAIP24, nil)
		// 同 /24 漂移容忍(dhcp/VPN 同网段换址不误杀)。
		if code, _ := doBound(r, bound, ua, "10.1.2.254:1111"); code != 200 {
			t.Fatalf("同 /24 误杀: code=%d", code)
		}
		// 跨网段 → 失配。
		if code, body := doBound(r, bound, ua, "10.1.3.1:1111"); code != 401 || !strings.Contains(body, "token_binding_mismatch") {
			t.Fatalf("跨 /24: code=%d body=%q, want 401", code, body)
		}
		// UA 漂移在 ip24 模式同样失配。
		if code, _ := doBound(r, bound, otherUA, "10.1.2.3:1111"); code != 401 {
			t.Fatalf("ip24 模式 UA 漂移未拒: code=%d", code)
		}
		// 未受信 XFF 不得影响指纹:测试路由 SetTrustedProxies(nil)
		// (镜像生产空配置),XFF 声称的地址既不解锁也不收紧判定。
		if code := doBoundXFF(r, bound, ua, "10.1.2.3:1111", "10.1.2.254"); code != 200 {
			t.Fatalf("未受信 XFF 干扰放行判定: code=%d", code)
		}
		if code := doBoundXFF(r, bound, ua, "10.1.3.1:1111", "10.1.2.3"); code != 401 {
			t.Fatalf("未受信 XFF 解锁跨段判定: code=%d", code)
		}
	})

	t.Run("cross_mode_reissue_semantics", func(t *testing.T) {
		// 模式切换 = 全量换发的 ua↔ua_ip24 半边:ua 指纹 token 打 ip24
		// router(及反向)必须 401——两模式指纹输入域不同,跨模式残留
		// token 不存在过渡放行(对抗审计 LOW)。
		ipTok, err := auth.SignTokenWithBinding(testSecret, time.Hour, "alice",
			[]auth.Role{auth.RoleAuthor}, 0, bindingFingerprint(TokenBindingUAIP24, ua, "10.1.2.3"))
		if err != nil {
			t.Fatalf("sign ip24 token: %v", err)
		}
		rIP := newBoundTestRouter(testSecret, TokenBindingUAIP24, nil)
		if code, _ := doBound(rIP, boundUA, ua, "10.1.2.3:1111"); code != 401 {
			t.Fatalf("ua 指纹 token 过 ip24 router: code=%d, want 401", code)
		}
		rUA := newBoundTestRouter(testSecret, TokenBindingUA, nil)
		if code, _ := doBound(rUA, ipTok, ua, ""); code != 401 {
			t.Fatalf("ip24 指纹 token 过 ua router: code=%d, want 401", code)
		}
	})

	t.Run("trusted_proxy_xff_participates", func(t *testing.T) {
		// 受信代理半边:显式信任回环代理时 XFF 参与指纹(XFF→ClientIP→
		// bindingFingerprint 传导链的信任态锚;TrustedProxies 配置契约
		// 的消费面在 server.go,此处钉中间件对 gin ClientIP 的消费)。
		gin.SetMode(gin.TestMode)
		r := gin.New()
		if err := r.SetTrustedProxies([]string{"127.0.0.1"}); err != nil {
			t.Fatalf("set trusted proxies: %v", err)
		}
		r.GET("/x", Authenticate(testSecret, TokenBindingUAIP24), func(c *gin.Context) {
			c.String(http.StatusOK, "sub=%s", c.ClientIP())
		})
		bound, err := auth.SignTokenWithBinding(testSecret, time.Hour, "alice",
			[]auth.Role{auth.RoleAuthor}, 0, bindingFingerprint(TokenBindingUAIP24, ua, "10.1.2.3"))
		if err != nil {
			t.Fatalf("sign ip24 token: %v", err)
		}
		if code := doBoundXFF(r, bound, ua, "127.0.0.1:9999", "10.1.2.3"); code != 200 {
			t.Fatalf("受信 XFF 同段应放行: code=%d", code)
		}
		if code := doBoundXFF(r, bound, ua, "127.0.0.1:9999", "10.1.9.9"); code != 401 {
			t.Fatalf("受信 XFF 跨段应拒: code=%d", code)
		}
		// 代理地址不在信任名单:RemoteAddr 直连判定(XFF 不采纳)。
		if code := doBoundXFF(r, bound, ua, "10.1.2.3:1111", "10.9.9.9"); code != 200 {
			t.Fatalf("未受信来源 XFF 干扰判定: code=%d", code)
		}
	})
}

func TestAuthenticateMiddleware(t *testing.T) {
	valid, err := auth.SignToken(testSecret, time.Hour, "alice", []auth.Role{auth.RoleAuthor}, 0)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	wrongKey, err := auth.SignToken("another-jwt-secret-placeholder-made-of-plain-words", time.Hour, "alice", []auth.Role{auth.RoleAuthor}, 0)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	tests := []struct {
		name     string
		header   string
		wantCode int
		wantBody string
	}{
		{"no header", "", 401, "missing_token"},
		{"non-bearer scheme", "Token " + valid, 401, "missing_token"},
		{"garbage token", "Bearer not-a-jwt", 401, "token_invalid"},
		{"wrong signing key", "Bearer " + wrongKey, 401, "token_invalid"},
		{"expired", "Bearer " + expiredToken(t, testSecret, "alice", []auth.Role{auth.RoleAuthor}), 401, "token_expired"},
		{"valid author reaches handler", "Bearer " + valid, 200, "sub=alice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRouter(testSecret, nil)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			r.ServeHTTP(w, req)
			if w.Code != tt.wantCode {
				t.Fatalf("code = %d, want %d, body %q", w.Code, tt.wantCode, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tt.wantBody) {
				t.Fatalf("body %q, want containing %q", w.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestRequireAnyRole(t *testing.T) {
	sign := func(t *testing.T, roles ...auth.Role) string {
		t.Helper()
		s, err := auth.SignToken(testSecret, time.Hour, "alice", roles, 0)
		if err != nil {
			t.Fatalf("sign token: %v", err)
		}
		return s
	}
	tests := []struct {
		name     string
		roles    []auth.Role // 路由声明的所需角色
		token    string
		wantCode int
		wantBody string
	}{
		{
			name:     "author passes author gate",
			roles:    []auth.Role{auth.RoleAuthor},
			token:    sign(t, auth.RoleAuthor),
			wantCode: 200,
			wantBody: "sub=alice",
		},
		{
			name:     "approver rejected by author gate",
			roles:    []auth.Role{auth.RoleAuthor},
			token:    sign(t, auth.RoleApprover),
			wantCode: 403,
			wantBody: "forbidden",
		},
		{
			name:     "admin passes author-or-admin gate",
			roles:    []auth.Role{auth.RoleAuthor, auth.RoleAdmin},
			token:    sign(t, auth.RoleAdmin),
			wantCode: 200,
			wantBody: "sub=alice",
		},
		{
			name:     "multi-role token passes single gate",
			roles:    []auth.Role{auth.RoleApprover},
			token:    sign(t, auth.RoleAuthor, auth.RoleApprover),
			wantCode: 200,
			wantBody: "sub=alice",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRouter(testSecret, tt.roles)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set("Authorization", "Bearer "+tt.token)
			r.ServeHTTP(w, req)
			if w.Code != tt.wantCode {
				t.Fatalf("code = %d, want %d, body %q", w.Code, tt.wantCode, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tt.wantBody) {
				t.Fatalf("body %q, want containing %q", w.Body.String(), tt.wantBody)
			}
		})
	}
}

// TestRequireScopeHelper:REST scp 执法闸(T17.1 立;T20.2/D-2026-10-07
// -t202-scp-denied-audit 起中间件形态退役,语义原样迁 in-handler helper)
// 四面钉死——
//   - scp 缺席(存量/手签形态,claim 不携带)→ 透传:全角色凭证现状
//     兼容裁定(issue #118 钉死;与 MCP 面 fail-closed 刻意不同面);
//   - scp 在场且持所需作用域 → 放行;
//   - scp 在场但不持 → 403 forbidden(与角色闸同响应体,不泄露哪道闸)
//   - onDeny 恰被回调一次(拒绝审计半边的接线面;401/500/放行不回调);
//   - 未认证调用(handler 前无 Authenticate 是接线 bug)→ 401 fail-closed
//     (镜像 TestRequireAnyRoleWithoutAuth);
//   - want 词表外 = 接线 bug → 500 响亮不伪造 403。
func TestRequireScopeHelper(t *testing.T) {
	sign := func(t *testing.T, scopes ...auth.Scope) string {
		t.Helper()
		s, err := auth.SignTokenScoped(testSecret, time.Hour, "alice", []auth.Role{auth.RoleAdmin}, 0, "", scopes)
		if err != nil {
			t.Fatalf("sign token: %v", err)
		}
		return s
	}
	// denied 记录 onDeny 回调(subject 恰一次)。
	var denied []string
	newScoped := func(want auth.Scope) *gin.Engine {
		denied = nil
		gin.SetMode(gin.TestMode)
		r := gin.New()
		r.GET("/x", Authenticate(testSecret, TokenBindingOff), func(c *gin.Context) {
			if !requireScope(c, want, func(subject string) { denied = append(denied, subject) }) {
				return
			}
			c.String(http.StatusOK, "sub=%s", c.MustGet(claimsCtxKey).(auth.Claims).Subject)
		})
		return r
	}
	do := func(r *gin.Engine, token string) (int, string) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}

	r := newScoped(auth.ScopeWorkflowsWrite)
	if code, body := do(r, sign(t, auth.ScopeWorkflowsWrite)); code != 200 || body != "sub=alice" {
		t.Fatalf("held scope: code=%d body=%q, want 200 sub=alice", code, body)
	}
	if len(denied) != 0 {
		t.Fatalf("held scope: onDeny called %v, want never", denied)
	}
	if code, body := do(r, sign(t, auth.ScopeWorkflowsRead, auth.ScopeExecutionsRead)); code != 403 || !strings.Contains(body, "forbidden") {
		t.Fatalf("unheld scope: code=%d body=%q, want 403 forbidden", code, body)
	}
	if len(denied) != 1 || denied[0] != "alice" {
		t.Fatalf("unheld scope: onDeny = %v, want [alice] exactly once", denied)
	}
	denied = nil
	if code, body := do(r, sign(t)); code != 200 || body != "sub=alice" {
		t.Fatalf("absent scp must pass through (现状兼容钉死): code=%d body=%q, want 200", code, body)
	}
	if len(denied) != 0 {
		t.Fatalf("absent scp: onDeny called %v, want never", denied)
	}
	denied = nil
	if code, _ := do(r, ""); code != 401 {
		t.Fatalf("no token: code=%d, want 401", code)
	}
	if len(denied) != 0 {
		t.Fatalf("401 face: onDeny called %v, want never(接线 bug 无证据语义)", denied)
	}

	// 接线 bug 面:want 词表外 → 500 响亮(auth.RequireScope 判定核
	// ErrInvalidInput 透传,不伪装成调用方 403;onDeny 不回调——接线
	// bug 无证据语义)。
	gin.SetMode(gin.TestMode)
	denied = nil
	bad := gin.New()
	bad.GET("/x", Authenticate(testSecret, TokenBindingOff), func(c *gin.Context) {
		if !requireScope(c, "audit:read", func(subject string) { denied = append(denied, subject) }) {
			return
		}
		c.String(http.StatusOK, "ok")
	})
	if code, body := do(bad, sign(t, auth.ScopeWorkflowsRead)); code != 500 || !strings.Contains(body, "internal") {
		t.Fatalf("vocab-external want: code=%d body=%q, want 500 internal", code, body)
	}
	if len(denied) != 0 {
		t.Fatalf("vocab-external want: onDeny called %v, want never", denied)
	}
}

// TestRequireScopeWithoutAuth:scp 闸在未认证上下文调用恒 401(镜像角色闸
// 接线错误面——handler 挂上未过 Authenticate 的路由是接线 bug)。
func TestRequireScopeWithoutAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", func(c *gin.Context) {
		if !requireScope(c, auth.ScopeWorkflowsRead, nil) {
			return
		}
		c.String(http.StatusOK, "ok")
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if w.Code != 401 {
		t.Fatalf("code = %d, want 401", w.Code)
	}
}

// TestRequireAnyRoleWithoutAuth 钉死接线错误面:角色闸挂在未认证路由上
// 恒 401 fail-closed,不静默放行。
func TestRequireAnyRoleWithoutAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", RequireAnyRole(auth.RoleAuthor))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if w.Code != 401 {
		t.Fatalf("code = %d, want 401", w.Code)
	}
}

// TestClaimsFromUnauthenticatedRoute 钉死 ClaimsFrom 在未认证路由的
// ok=false 契约(调用方不得假设 claims 存在)。
func TestClaimsFromUnauthenticatedRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var gotOK bool
	r := gin.New()
	r.GET("/x", func(c *gin.Context) {
		_, gotOK = ClaimsFrom(c)
		c.Status(http.StatusOK)
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if gotOK {
		t.Fatal("ClaimsFrom on unauthenticated route = ok, want false")
	}
}
