// 登录面(T4.4):POST /api/v1/auth/login —— 凭据换 Bearer token,Web
// 审批页的入口。认证经 s.idp 缝(T5.3,默认内置 auth.BuiltinProvider:
// 防枚举三态统一 + dummy 时序均衡 + DISABLED 密码后判定;替换实现契约
// 见 internal/auth/identity.go)与 auth.SignToken(claims 形状与中间件
// ParseToken 同构,签发即可用);错误面在这里只做映射:
//   - CredentialsError(未知用户/错密码/域外输入)→ 401 invalid_credentials
//     ——Reason 是日志词(auth/errors.go 契约),绝不进响应;
//   - ErrUserDisabled → 403 user_disabled(仅持正确凭证者可达);
//   - 其余 → 500(仅日志)。
//
// token 载体 = Bearer + 前端 sessionStorage(D-2026-09-24-t44-approval-web:
// 中间件零改动、CSRF 免疫;CSP 收 XSS 面)。无登录审计:audit_log 是工作流
// 事实域(T2.6 契约),登录事件流审计归 T6.4 告警面;无独立限流——bcrypt
// ~100ms 即天然节流,基础设施层限流同归 T6.4。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"braid/internal/auth"
)

// loginRequest 登录请求体;域校验(词域/长度)由身份源前置拒绝(内置
// 实现在 auth.Authenticate 内),此处不做第二套(两套判据必漂移)。
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// loginResponse 是登录成功视图。ExpiresAt 是签发时刻+TTL 的重算近似值
// (秒级截断,可与 token exp claim 偏差 ~1s;前端 session 提示用,权威
// 仍在 exp claim——不为此把 claims 穿透出 SignToken)。
type loginResponse struct {
	Token     string      `json:"token"`
	TokenType string      `json:"token_type"`
	ExpiresAt string      `json:"expires_at"`
	Username  string      `json:"username"`
	Roles     []auth.Role `json:"roles"`
}

// maxLogUsername 把登录失败日志的主体名钳在常数:原文是攻击者可控自由
// 输入,且域外输入路径(invalid_input)不付 bcrypt 成本即被拒——「天然
// 节流」论对该路径不成立,未截断的 1MiB username 逐请求全量进日志 =
// 无上界日志放大(对抗审计 HIGH)。截断 + 长度事实保留可诊断性。
const maxLogUsername = 64

// logUsername 是日志用有界形态:UTF-8 尾部安全截断(回退到完整 rune
// 边界,至多收敛 3 字节),超限时附真实长度事实。
func logUsername(s string) string {
	if len(s) <= maxLogUsername {
		return s
	}
	cut := s[:maxLogUsername]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + fmt.Sprintf("...(len=%d)", len(s))
}

func (s *Server) login(c *gin.Context) {
	var req loginRequest
	b, ok := readBodyLimited(c)
	if !ok {
		return
	}
	if err := json.Unmarshal(b, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
		return
	}
	user, err := s.idp.Authenticate(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		var creds *auth.CredentialsError
		switch {
		case errors.As(err, &creds):
			// 主体名有界化进日志(见 logUsername);Reason 是日志词
			// (auth/errors.go 契约),绝不进响应。
			slog.Info("api: login rejected", "username", logUsername(creds.Username), "reason", creds.Reason)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_credentials"})
		case errors.Is(err, auth.ErrUserDisabled):
			c.JSON(http.StatusForbidden, gin.H{"error": "user_disabled"})
		default:
			slog.Error("api: login authenticate", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		}
		return
	}
	ttl := auth.DefaultTokenTTL
	token, err := auth.SignToken(s.cfg.JWTSecret, ttl, user.Username, user.Roles)
	if err != nil {
		// 词表外角色/签名密钥问题:启动期 Validate 已拦密钥面,到达此处即
		// 数据或接线异常,响亮 500 不伪造成功。
		slog.Error("api: login sign token", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		return
	}
	c.JSON(http.StatusOK, loginResponse{
		Token:     token,
		TokenType: "Bearer",
		ExpiresAt: time.Now().UTC().Add(ttl).Format(time.RFC3339),
		Username:  user.Username,
		Roles:     user.Roles,
	})
}
