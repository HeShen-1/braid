package api

// T5.3 IdentityProvider 替换验收(零栈):nil dbh 注入 ProviderFunc 桩,
// 登录全路径不触 DB——「桩实现可替换内置认证」的运行时证明(ROADMAP
// T5.3 验收)。既有 TestLogin(t13 真库)经同一缝跑内置 BuiltinProvider,
// 是换装后登录面行为不变的回归网。
//
// 变异自查:login.go 回退为直调 auth.Authenticate → 桩被旁路,nil dbh
// 触库路径运行期 panic 兜 500,success 与 401/403 行红(注意:nil
// *sql.DB 静默满足 DBTX,回退可编译——杀伤是运行期红,非编译红);
// 错误映射互换 → credentials/disabled 行红;SignToken 换 secret →
// success 行 token 逆解红。

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"braid/internal/auth"
	"braid/internal/snowflake"
	"braid/internal/tools"
)

// newIDPTestRouter:登录面路由 + 注入的身份源,nil dbh——桩即唯一身份
// 源,任何 DB 访问在此构造下不可达(nil 池指针解引用即 panic,被
// gin.Recovery 兜成 500,与测试断言面冲突即红)。
func newIDPTestRouter(t *testing.T, idp auth.IdentityProvider) *gin.Engine {
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
	// 登录防御旋钮放宽(同 newTestServer 判:防御行为由专用紧旋钮用例锚)。
	return NewServer(Config{Addr: DefaultAddr, JWTSecret: testSecret, SnowflakeNode: snowflake.Explicit(1),
		LoginRateLimit: 100000, LoginRateWindow: time.Minute,
		LoginMaxFailures: 100000, LoginLockout: time.Minute},
		nil, ids, reg, fakeTrigger{id: 1}, idp).Router()
}

func TestLoginIdentityProviderReplacement(t *testing.T) {
	stubUser := auth.User{
		ID:       42,
		Username: "stub-ada",
		Roles:    []auth.Role{auth.RoleApprover, auth.RoleAuthor},
		Status:   auth.StatusActive,
	}
	login := func(t *testing.T, r *gin.Engine, body string) (int, map[string]any) {
		t.Helper()
		return doJSON(t, r, http.MethodPost, "/api/v1/auth/login", "", []byte(body))
	}

	t.Run("success_stub_user_token_roundtrip", func(t *testing.T) {
		var seenUser, seenPass string
		r := newIDPTestRouter(t, auth.ProviderFunc(
			func(_ context.Context, username, password string) (auth.User, error) {
				seenUser, seenPass = username, password
				return stubUser, nil
			}))
		code, out := login(t, r, `{"username":"stub-ada","password":"whatever-stub-1"}`)
		if code != http.StatusOK {
			t.Fatalf("code = %d, body %v", code, out)
		}
		if out["username"] != "stub-ada" {
			t.Fatalf("username = %v, want 桩用户回显", out["username"])
		}
		// 响应体契约键集钉死(对抗审计 LOW 收口):token/token_type/
		// expires_at/username/roles 五键,tag 漂移即红而非静默存活。
		if len(out) != 5 {
			t.Fatalf("响应体键集 = %v, want 恰五键", slices.Sorted(maps.Keys(out)))
		}
		if out["token_type"] != "Bearer" {
			t.Fatalf("token_type = %v, want Bearer", out["token_type"])
		}
		if exp, _ := out["expires_at"].(string); exp == "" {
			t.Fatalf("expires_at 缺失: %v", out["expires_at"])
		}
		rolesAny, _ := out["roles"].([]any)
		if len(rolesAny) != len(stubUser.Roles) {
			t.Fatalf("roles = %v, want %v", out["roles"], stubUser.Roles)
		}
		for i, r := range rolesAny {
			if r != string(stubUser.Roles[i]) {
				t.Fatalf("roles[%d] = %v, want %v", i, r, stubUser.Roles[i])
			}
		}
		// 凭据逐字到达桩:缝不加工输入。
		if seenUser != "stub-ada" || seenPass != "whatever-stub-1" {
			t.Fatalf("凭据未逐字到达桩: (%q, %q)", seenUser, seenPass)
		}
		// 签出的 token 必须逆解回桩用户的 sub+roles:身份源已换,token
		// 面形状不变(SignToken 与身份源正交)。
		tok, _ := out["token"].(string)
		if tok == "" {
			t.Fatalf("no token in response: %v", out)
		}
		claims, err := auth.ParseToken(testSecret, tok)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if claims.Subject != "stub-ada" {
			t.Fatalf("sub = %q, want stub-ada", claims.Subject)
		}
		if !slices.Equal(claims.Roles, stubUser.Roles) {
			t.Fatalf("roles = %v, want %v", claims.Roles, stubUser.Roles)
		}
	})

	opaque := errors.New("idp: downstream unavailable")
	errCases := []struct {
		name     string
		stubErr  error
		wantCode int
		wantErr  string
	}{
		{
			name:     "credentials_401",
			stubErr:  &auth.CredentialsError{Username: "stub-ada", Reason: auth.ReasonWrongPassword},
			wantCode: http.StatusUnauthorized, wantErr: "invalid_credentials",
		},
		{
			name:     "disabled_403",
			stubErr:  fmt.Errorf("%w: %q", auth.ErrUserDisabled, "stub-ada"),
			wantCode: http.StatusForbidden, wantErr: "user_disabled",
		},
		{
			name:     "opaque_500",
			stubErr:  opaque,
			wantCode: http.StatusInternalServerError, wantErr: "internal",
		},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			r := newIDPTestRouter(t, auth.ProviderFunc(
				func(context.Context, string, string) (auth.User, error) {
					return auth.User{}, tc.stubErr
				}))
			code, out := login(t, r, `{"username":"stub-ada","password":"whatever-stub-1"}`)
			if code != tc.wantCode || out["error"] != tc.wantErr {
				t.Fatalf("code=%d error=%v, want %d %q", code, out["error"], tc.wantCode, tc.wantErr)
			}
			// 不透明故障无泄漏契约(server_test.go TestHTTPInternalErrorMapping
			// 同款):500 响应体只许携带 error 键,桩的底层错误文本不出网。
			if tc.wantErr == "internal" && len(out) != 1 {
				t.Fatalf("500 响应体键集 = %v, want 仅 error", slices.Sorted(maps.Keys(out)))
			}
		})
	}
}

// TestNewServerRejectsTypedNilProvider:typed-nil 探测(对抗审计 MEDIUM
// 收口)——ProviderFunc(nil) 装箱后接口值非 nil,== 检查放行会把
// panic 推迟到首个登录请求(空 body 500);构造期必须响亮失败。
func TestNewServerRejectsTypedNilProvider(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatalf("ProviderFunc(nil) 未被构造期拒绝")
		}
	}()
	newIDPTestRouter(t, auth.ProviderFunc(nil))
}
