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
//   - 其余(篡改/错密钥/alg 混淆/词表外角色)→ 401 token_invalid;
//   - 绑定模式开启(ua/ua_ip24,T11.3)下 bnd claim 与当前请求指纹
//     失配或缺席 → 401 token_binding_mismatch(fail-closed:模式切换即
//     全量换发,存量 token ≤24h TTL 界损失)。off = 零介入。
//
// 不在这里做角色判定:路由级 RequireAnyRole 组合,读面与写面各自声明。
func Authenticate(secret string, binding TokenBindingMode) gin.HandlerFunc {
	return func(c *gin.Context) {
		authenticate(c, secret, binding)
	}
}

// authenticate 是认证判定核(T13.1 抽出):REST 路由组中间件与 MCP 面
// 单路由入口共用同一判定,失败面貌/绑定校验单点防双面漂移。失败时
// Abort 写终态响应;成功时 claims 入 context(c.Next 由调用方形态决定:
// 中间件链自然续行,单入口由调用方显式检查 IsAborted)。
func authenticate(c *gin.Context, secret string, binding TokenBindingMode) {
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
	if binding == TokenBindingUA || binding == TokenBindingUAIP24 {
		// 签发与校验共用 bindingFingerprint 单点(见 binding.go);
		// 失配与缺席同面(claims.Binding 零值 "" 恒不等于指纹)。
		if claims.Binding != bindingFingerprint(binding, c.Request.UserAgent(), c.ClientIP()) {
			abort(c, 401, "token_binding_mismatch")
			return
		}
	}
	c.Set(claimsCtxKey, claims)
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

// requireScope 是 in-handler scp 执法闸(T20.2,D-2026-10-07-t202-scp-
// denied-audit:自 T17.1 的路由中间件形态迁移——中间件面短路 403 无审计
// 半边,闸与 denied 写入器同域后 scoped 拒绝可落既有审计事件族)。判定
// 核仍是 auth.RequireScope(MCP 面单点,双面漂移面不存在);判定语义与
// 响应面逐字继承:scp 在场的 token 必须持有所需作用域,不足 403
// {"error":"forbidden"}(与角色闸同判,不泄露被哪道闸拦——拒绝面零
// 信息增益)。
//
// scp 缺席(claim 不携带;nil/空集在签发原语结构性同判)→ 透传:全角色
// 凭证现状兼容裁定,issue #118 钉死——与 MCP 面「缺席 = 零权限
// fail-closed」刻意不同面:MCP 是 T13.1 新面无存量,REST 面 100% 存量
// 兼容(角色闸独挑授权;平台签发口自 T13.1 起已全量携带默认 scp,
// 缺席形态仅剩手签/遗留)。
//
// onDeny 是拒绝半边的旁路钩子(执行锚 action 路由传落审计闭包——
// writeXDeniedAudit reason="scope_denied" + Denied 计数器,镜像 role_denied
// 分支;非锚路由 nil,边界登记见 D-t202 决策 5)。401/500 面不回调
// (接线 bug 无证据语义)。落位纪律:非审计路由 = handler 首行(响应面
// 与中间件形态逐字等价);审计路由 = 全部形状闸之后、role 闸之前
// (「形状闸先于审计闸」先例——审计行携带已验形的请求上下文,代价 =
// 形状非法×scp 不足组合 403→400/413,D-t202 决策 3 登记)。
//
// 无 claims(路由未挂 Authenticate)是接线 bug,恒 401 fail-closed;
// want 词表外同为接线 bug,500 响亮(auth.RequireScope 判定核的
// ErrInvalidInput 透传成 internal,不伪装成调用方 403)。
func requireScope(c *gin.Context, want auth.Scope, onDeny func(subject string)) bool {
	claims, ok := ClaimsFrom(c)
	if !ok {
		abort(c, 401, "missing_token")
		return false
	}
	if len(claims.Scopes) == 0 {
		// scp 缺席 = 全角色凭证(现状兼容,见上)。
		return true
	}
	if err := auth.RequireScope(claims, want); err != nil {
		if errors.Is(err, auth.ErrInvalidInput) {
			abort(c, 500, "internal")
			return false
		}
		if onDeny != nil {
			onDeny(claims.Subject)
		}
		abort(c, 403, "forbidden")
		return false
	}
	return true
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
