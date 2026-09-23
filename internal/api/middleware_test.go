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
// handler 回显 claims.Subject。
func newTestRouter(secret string, roles []auth.Role) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := func(c *gin.Context) {
		claims, _ := ClaimsFrom(c)
		c.String(http.StatusOK, "sub=%s", claims.Subject)
	}
	if roles == nil {
		r.GET("/x", Authenticate(secret), h)
		return r
	}
	r.GET("/x", Authenticate(secret), RequireAnyRole(roles...), h)
	return r
}

func TestAuthenticateMiddleware(t *testing.T) {
	valid, err := auth.SignToken(testSecret, time.Hour, "alice", []auth.Role{auth.RoleAuthor})
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	wrongKey, err := auth.SignToken("another-jwt-secret-placeholder-made-of-plain-words", time.Hour, "alice", []auth.Role{auth.RoleAuthor})
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
		s, err := auth.SignToken(testSecret, time.Hour, "alice", roles)
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
