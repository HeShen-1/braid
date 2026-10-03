// 自窄化 token 签发面(D-2026-09-30-t131 决策 4):POST /api/v1/auth/tokens。
// 已认证主体请求一枚子集 token——给外部 MCP agent 发最小权限 token 的
// 运维面由此成立(作用域不是测试专供)。上界双维(评审 HIGH 修订):
//   - 作用域:scopes 非空且 ⊆ (角色默认集 ∩ 呈现 token 自身 scp)——
//     携 scp 的 token 再签发只能在自己作用域内取子集,「只窄不宽」在
//     签发链上单调收窄,泄露的窄 token 无法经再签发扩权;scp 缺席
//     (存量 login token)= 角色默认集为界。空集/词表外/重复/超界 →
//     400 invalid_scope(超界不给 403——请求面相对自身能力即非法输入,
//     与 OAuth invalid_scope 同判);
//   - 寿命:ttl_seconds 可选,非正或超 MaxTokenTTL(秒域先行比较,
//     防 int64 秒→Duration 换算溢出绕过守卫)→ 400 invalid_ttl;且
//     子 token TTL ≤ 呈现 token 剩余寿命——无状态 JWT 无吊销面,TTL
//     即停用后最大暴露窗(auth/config.go 契约),再签发链不得外扩寿命,
//     泄露 token 无法续命;
//   - 主体/租户 = 签发者 claims 同值透传(tnt 快照同判,租户不可经自窄化
//     漂移);绑定指纹按当前请求复算(与 login 同单点);
//   - token 出现在响应体:禁中间层缓存(login/oidc 同契约)。
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"braid/internal/auth"
)

// issueTokenRequest 是签发请求体。scopes 必填(空/缺席 = 400:签一枚
// 零作用域 token 无意义,能力下限走不带 scp 的存量形态);ttl_seconds
// 可选(缺省 DefaultTokenTTL)。
type issueTokenRequest struct {
	Scopes  []auth.Scope `json:"scopes"`
	TTLSecs *int64       `json:"ttl_seconds"`
}

// tokenResponse 是签发成功视图(镜像 loginResponse 契约;不回显
// username/roles——请求者已知自己是谁,回显集 = 签发事实)。
type tokenResponse struct {
	Token     string       `json:"token"`
	TokenType string       `json:"token_type"`
	ExpiresAt string       `json:"expires_at"`
	Scopes    []auth.Scope `json:"scopes"`
}

// scopesWithin/intersectScopes 已收敛入 auth 包(T17.1,D-t131 登记⑤
// 收编):集合代数与词表同源,api 不留第二套实现——本文件消费
// auth.ScopesWithin / auth.IntersectScopes。

func (s *Server) issueToken(c *gin.Context) {
	claims, ok := ClaimsFrom(c)
	if !ok {
		// 路由挂 Authenticate 后不可达;接线 bug 恒 401(镜像 RequireAnyRole)。
		abort(c, http.StatusUnauthorized, "missing_token")
		return
	}
	b, ok := readBodyLimited(c)
	if !ok {
		return // readBodyLimited 已写终态响应(413/400)
	}
	var req issueTokenRequest
	if err := json.Unmarshal(b, &req); err != nil {
		abort(c, http.StatusBadRequest, "bad_request")
		return
	}
	if len(req.Scopes) == 0 {
		abort(c, http.StatusBadRequest, "invalid_scope")
		return
	}
	if err := auth.ValidateScopes(req.Scopes); err != nil {
		abort(c, http.StatusBadRequest, "invalid_scope")
		return
	}
	allowed := auth.DefaultScopesForRoles(claims.Roles)
	if len(claims.Scopes) > 0 {
		// 链式封堵(评审 HIGH):呈现 token 自带 scp 时上界收紧为交集。
		// 签出面已保证 scp ⊆ 角色默认,交集恒等 scp 自身——交集是防御性
		// 双门(防直接手签的 scp ⊄ 角色默认的怪 token)。
		allowed = auth.IntersectScopes(claims.Scopes, allowed)
	}
	if !auth.ScopesWithin(req.Scopes, allowed) {
		// 超界 = 请求相对自身能力的非法输入(自窄化只窄不宽);拒绝面
		// 不回显上界集——避免把角色能力面变成免费 oracle。
		abort(c, http.StatusBadRequest, "invalid_scope")
		return
	}
	ttl := auth.DefaultTokenTTL
	if req.TTLSecs != nil {
		// 秒域上界先行(评审 LOW:int64 秒→Duration 换算溢出会绕过守卫)。
		if *req.TTLSecs <= 0 || *req.TTLSecs > int64(auth.MaxTokenTTL/time.Second) {
			abort(c, http.StatusBadRequest, "invalid_ttl")
			return
		}
		ttl = time.Duration(*req.TTLSecs) * time.Second
	}
	// 寿命外扩封堵(评审 HIGH):子 TTL ≤ 呈现 token 剩余寿命——签发链
	// 双维(作用域交集 + 寿命收窄)单调不宽,泄露 token 两跳内无法扩权
	// 也无法续命。Authenticate 已拒过期 token,remaining ≤ 0 不可达防御;
	// 阈值取 1s 而非 0(评审 LOW 修):亚秒剩余经 clamp 会签出亚秒 token,
	// 其审计行 ttl_seconds 秒截断为 0 被 writeScopedTokenAudit 防御闸拒收
	// ——「每次签发必落行」契约出现确定性缺口,签发面前置闭合(亚秒
	// token 本无运维意义,与「事实过期」同判)。
	if claims.ExpiresAt != nil {
		remaining := time.Until(claims.ExpiresAt.Time)
		if remaining < time.Second {
			abort(c, http.StatusUnauthorized, "token_expired")
			return
		}
		if ttl > remaining {
			ttl = remaining
		}
	}
	token, err := auth.SignTokenScoped(s.cfg.JWTSecret, ttl, claims.Subject, claims.Roles, claims.Tenant,
		bindingFingerprint(s.cfg.TokenBinding, c.Request.UserAgent(), c.ClientIP()), req.Scopes)
	if err != nil {
		// 前置校验后到达此处即数据或接线异常,响亮 500 不伪造成功。
		slog.Error("api: issue token sign", "err", err)
		abort(c, http.StatusInternalServerError, "internal")
		return
	}
	// 签发是账号域安全事实(T17.2 收口,D-t131 登记④):login_audit 落
	// scoped_token_issued 行——主体/作用域/TTL(clamp 后实际授予值)/
	// token 哈希(SHA-256 hex,原值绝不入审计——铁律);best-effort 独立
	// 小事务,旁路审计失败不翻转签发响应。slog 保留为运维快面,表 =
	// 可 SQL 审计的历史半边。
	slog.Info("api: scoped token issued", "username", logUsername(claims.Subject), "scopes", req.Scopes, "ttl", ttl)
	sum := sha256.Sum256([]byte(token))
	s.writeScopedTokenAudit(c.Request.Context(), c.ClientIP(), claims.Subject, req.Scopes,
		int64(ttl/time.Second), hex.EncodeToString(sum[:]))
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, tokenResponse{
		Token:     token,
		TokenType: "Bearer",
		ExpiresAt: time.Now().UTC().Add(ttl).Format(time.RFC3339),
		Scopes:    req.Scopes,
	})
}
