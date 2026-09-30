// OIDC redirect 流路由面(T11.1,D-2026-09-28-t111-oidc-subject-anchor):
// GET /api/v1/auth/oidc/login(302 → IdP,握手态下发票根 cookie)与
// GET /api/v1/auth/oidc/callback(claim → 本地锚定 → Bearer token,响应与
// login 同构)。与 POST /api/v1/auth/login 并存(渐进迁移);未注入
// OIDCProvider 即不挂载(未配置即不注册,BuiltinToolsFromEnv 先例)。
//
// 错误面(错误族复用 T5.3 契约,分层映射):
//   - 传输/协议面(state cookie 缺失/拒收、IdP error 重定向、code 缺失/
//     兑换失败)→ 400 bad_request,只发指标不落 login_audit(bad-JSON
//     同判:请求频率攻击者可控,逐行落库 = 写放大);
//   - 身份判定面(claim 域外/本地无账号 → 401;DISABLED → 403)→ 落
//     login_audit + 指标,与口令登录同一发射点;
//   - ID token 验签/nonce 不符(身份源故障)与本地库故障 → 500,不计数
//     不审计(login.go 同判)。
//
// OIDC 路径不进 loginThrottle:无口令面,爆破防御在 IdP 侧;连败计数会把
// IdP 用户的每次回调失败都锁进 DB 锁定行(locked_until,期满才解、跨副本
// 共享)——协议/传输面错误与身份判定面由此不可分,拒绝面失真。

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"braid/internal/auth"
	"braid/internal/metrics"
)

// oidcAuther 是 OIDCProvider 的消费面(测试桩替身缝;生产唯一实现 =
// *auth.OIDCProvider,handler 只依赖这两个方法)。
type oidcAuther interface {
	StartAuth() (auth.OIDCStart, error)
	Complete(ctx context.Context, cookieValue, state, code string) (auth.User, string, error)
}

// oidcCookiePath 把握手态 cookie 收窄到 OIDC 两路由(最小暴露面)。
const oidcCookiePath = "/api/v1/auth/oidc"

// setOIDCStateCookie:HttpOnly(握手态不出 JS)、SameSite=Lax(顶级导航
// 回调可达,跨站 XHR 不可携)、Path 收窄、寿命 = OIDCStateTTL;Secure 随
// 请求 scheme(TLS 或反代 X-Forwarded-Proto 尽力判定——握手态非凭证,
// 漏标只破坏流程不泄漏授权)。maxAge < 0 = 清除(握手态一次性)。
func setOIDCStateCookie(c *gin.Context, value string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     auth.OIDCStateCookie,
		Value:    value,
		Path:     oidcCookiePath,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure: c.Request.TLS != nil ||
			strings.EqualFold(c.Request.Header.Get("X-Forwarded-Proto"), "https"),
		SameSite: http.SameSiteLaxMode,
	})
}

// oidcLoginStart:GET /api/v1/auth/oidc/login。
func (s *Server) oidcLoginStart(c *gin.Context) {
	start, err := s.oidc.StartAuth()
	if err != nil {
		// 熵源故障(系统级):500,不计数不审计(非登录判定结果)。
		slog.Error("api: oidc start", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		return
	}
	setOIDCStateCookie(c, start.CookieValue, int(auth.OIDCStateTTL.Seconds()))
	c.Redirect(http.StatusFound, start.RedirectURL)
}

// oidcCallback:GET /api/v1/auth/oidc/callback。
func (s *Server) oidcCallback(c *gin.Context) {
	ip := c.ClientIP()
	cookie, err := c.Cookie(auth.OIDCStateCookie)
	setOIDCStateCookie(c, "", -1) // 握手态一次性:无论后续成败即清
	if err != nil || cookie == "" {
		s.oidcTransportRejected(c, "state cookie missing")
		return
	}
	if e := c.Query("error"); e != "" {
		// IdP 经 redirect 通报的失败(access_denied 等):协议面,原文只进日志。
		s.oidcTransportRejected(c, "idp error redirect: "+e)
		return
	}
	code := c.Query("code")
	if code == "" {
		s.oidcTransportRejected(c, "code missing")
		return
	}
	user, subject, err := s.oidc.Complete(c.Request.Context(), cookie, c.Query("state"), code)
	if err != nil {
		var creds *auth.CredentialsError
		switch {
		case errors.Is(err, auth.ErrOIDCState) || errors.Is(err, auth.ErrOIDCCode):
			s.oidcTransportRejected(c, err.Error())
		case errors.As(err, &creds):
			// Reason 是日志词(auth/errors.go 契约),绝不进响应。
			slog.Info("api: oidc login rejected", "username", logUsername(creds.Username), "reason", creds.Reason)
			s.loginAuditEvent(c.Request.Context(), loginEventFailed, ip, creds.Username, creds.Reason)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_credentials"})
		case errors.Is(err, auth.ErrUserDisabled):
			// subject 是 Complete 解析出的 claim 值(口令登录 req.Username
			// 的对应物),审计归因不缺位。
			s.loginAuditEvent(c.Request.Context(), loginEventFailed, ip, subject, "user_disabled")
			c.JSON(http.StatusForbidden, gin.H{"error": "user_disabled"})
		default:
			// 身份源/本地库故障:不是登录判定结果,不计数不审计。
			slog.Error("api: oidc complete", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		}
		return
	}
	s.loginAuditEvent(c.Request.Context(), loginEventSucceeded, ip, user.Username, "")
	ttl := auth.DefaultTokenTTL
	// token 绑定(T11.3):与口令登录同契约——按当前模式携 bnd claim,
	// 指纹取回调请求的 UA/IP(与后续 API 调用同一浏览器会话)。
	// 租户快照(D-t112):user.TenantID → tnt claim;两签发点同契约。
	// 作用域(D-t131 决策 3):与口令登录同契约,按角色默认全量。
	token, err := auth.SignTokenScoped(s.cfg.JWTSecret, ttl, user.Username, user.Roles, user.TenantID,
		bindingFingerprint(s.cfg.TokenBinding, c.Request.UserAgent(), ip),
		auth.DefaultScopesForRoles(user.Roles))
	if err != nil {
		// 词表外角色/密钥问题:响亮 500 不伪造成功(login.go 同判)。
		slog.Error("api: oidc sign token", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		return
	}
	// token 出现在响应体:禁中间层缓存(review LOW;login 面同步加,
	// 两路同契约)。
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, loginResponse{
		Token:     token,
		TokenType: "Bearer",
		ExpiresAt: time.Now().UTC().Add(ttl).Format(time.RFC3339),
		Username:  user.Username,
		Roles:     user.Roles,
	})
}

// maxLogWhy 把传输面拒绝日志的 why 钳在常数:回调参数是攻击者可控输入
// (error/code 原文、含 code 的底层错误文本),该路径刻意不进
// loginThrottle——未截断的逐请求全量进日志 = 无上界日志放大(login.go
// maxLogUsername 同族缺陷,review MEDIUM)。截断 + 长度事实保留可诊断性。
const maxLogWhy = 128

func logWhy(s string) string {
	if len(s) <= maxLogWhy {
		return s
	}
	cut := s[:maxLogWhy]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + fmt.Sprintf("...(len=%d)", len(s))
}

// oidcTransportRejected:传输/协议面统一拒绝(400,只指标不落审计;细节
// 只进日志且经 logWhy 有界化,响应文本不回显外部输入)。
func (s *Server) oidcTransportRejected(c *gin.Context, why string) {
	slog.Warn("api: oidc transport rejected", "why", logWhy(why), "ip", c.ClientIP())
	s.mLogin(metrics.ResultFailed)
	c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
}
