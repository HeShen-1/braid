package api

import (
	"errors"
	"strings"

	"github.com/gin-gonic/gin"

	"braid/internal/auth"
)

// claimsCtxKey 是 claims 的 gin context 键(gin 键域是 string,用私有常量
// 占位,仓内唯一前缀防碰撞)。
const claimsCtxKey = "braid.api.claims"

// Authenticate 是最小认证中间件(T5.2 落地前的接线形态):Authorization:
// Bearer <token> → auth.ParseToken → claims 入 context。失败面貌:
//   - 缺头/非 Bearer 形态 → 401 missing_token;
//   - 过期 → 401 token_expired(auth 侧唯一单列的失败类);
//   - 其余(篡改/错密钥/alg 混淆/词表外角色)→ 401 token_invalid。
//
// 不在这里做角色判定:路由级 RequireAnyRole 组合,读面与写面各自声明。
func Authenticate(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.GetHeader("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(h, prefix) {
			abort(c, 401, "missing_token")
			return
		}
		claims, err := auth.ParseToken(secret, strings.TrimPrefix(h, prefix))
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrTokenExpired):
				abort(c, 401, "token_expired")
			case errors.Is(err, auth.ErrTokenInvalid):
				abort(c, 401, "token_invalid")
			default:
				// ErrInvalidInput(secret 过短)是配置误用,ParseToken 契约
				// 下不应在请求路径出现;按 invalid 对外,细节留在错误链里。
				abort(c, 401, "token_invalid")
			}
			return
		}
		c.Set(claimsCtxKey, claims)
		c.Next()
	}
}

// RequireAnyRole 是路由级角色闸:claims 命中任一所需角色放行,不足 403。
// 无 claims(路由未挂 Authenticate)是接线 bug,恒 401 fail-closed。
func RequireAnyRole(roles ...auth.Role) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := ClaimsFrom(c)
		if !ok {
			abort(c, 401, "missing_token")
			return
		}
		if claimsHaveAnyRole(claims, roles...) {
			c.Next()
			return
		}
		abort(c, 403, "forbidden")
	}
}

// ClaimsFrom 取 Authenticate 放入的 claims;未认证路由返回 ok=false。
func ClaimsFrom(c *gin.Context) (auth.Claims, bool) {
	v, ok := c.Get(claimsCtxKey)
	if !ok {
		return auth.Claims{}, false
	}
	claims, ok := v.(auth.Claims)
	return claims, ok
}

// claimsHaveAnyRole 是 RequireAnyRole 的判定核(T5.2 抽出共享):命中
// 任一所需角色放行。in-handler 角色闸(如审批决定面,拒绝需落审计与
// 分面错误码,路由中间件面做不到)与路由级中间件共用同一判定,避免
// 两处漂移。
func claimsHaveAnyRole(claims auth.Claims, roles ...auth.Role) bool {
	for _, want := range roles {
		if auth.RequireRole(claims, want) == nil {
			return true
		}
	}
	return false
}

// abort 统一错误响应体:{ "error": "<code>" }(DSL 校验类另带 issues)。
func abort(c *gin.Context, status int, code string) {
	c.AbortWithStatusJSON(status, gin.H{"error": code})
}
