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
// 中间件零改动、CSRF 免疫;CSP 收 XSS 面)。登录审计 = login_audit 独立表
// (0005,append-only 账号域事实,不混入工作流事实域 audit_log);限流/账号
// 锁定 = loginDefenseGate(IP 固定窗口 + username 连败锁,429 同形双防枚举;
// 旋钮 Config 化;T11.3 起 DB 锚定 = login_throttle 表 0010,多副本共享)。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"braid/internal/auth"
	"braid/internal/metrics"
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

// mLogin 是登录指标计数的 nil-safe 发射(T6.4)。
func (s *Server) mLogin(result string) {
	if s.loginMetrics != nil {
		s.loginMetrics.Attempts.WithLabelValues(result).Inc()
	}
}

// loginAuditEvent 落审计行 + 指标(同一发射点双半边,词面单源映射)。
// method = 登录方法(0013:口令/OIDC 可区分,调用面显式传参——
// 漏传即 fail-closed 响亮,不静默落不可归因行)。
// ctx = 请求上下文(镜像 writeTriggerAudit 契约,rev2 advisory)。
func (s *Server) loginAuditEvent(ctx context.Context, eventType, method, ip, username, reason string) {
	s.mLogin(loginResultLabel(eventType))
	s.writeLoginAudit(ctx, eventType, method, ip, username, reason)
}

// loginDefenseGate 是认证前的防御闸(rev3 提取:防御逻辑单点)。两段:
// body 形态无关段(超限/坏 JSON,计入 IP-only 窗口)与 username 段(锁定
// × IP 窗口)。防御拒绝一律 429 同形(防枚举 oracle)且只发指标不落 DB
// ——拒绝路径的请求频率由攻击者控制,逐行落库即写放大;登录判定
// (succeeded/failed)才是审计行的事实域。alreadyResponded = 调用方
// 不得再写响应(readBodyLimited 已写 413/400,二次写 = 双写缺陷)。
// DB 故障 → 500(防御不可用 = 登录不可用,fail-closed;认证同库,无新
// fail-open 面,D-t113 #2);nil DB 桩面防御跳过(loginThrottle 内判)。
func (s *Server) loginDefenseGate(c *gin.Context, ip, username string, alreadyResponded bool) bool {
	if username == "" {
		limited, err := s.loginThrottle.checkIP(c.Request.Context(), ip)
		if err != nil {
			s.loginDefenseUnavailable(c, err, alreadyResponded)
			return false
		}
		if limited {
			s.mLogin(metrics.ResultRateLimited)
			if !alreadyResponded {
				c.JSON(http.StatusTooManyRequests, gin.H{"error": "too_many_attempts"})
			}
			return false
		}
		if alreadyResponded {
			s.mLogin(metrics.ResultFailed)
			return false // 响应已由 readBodyLimited 落定:拦截,调用方裸返回
		}
		return true // 未限流且响应未写:调用方继续(自己的 400)
	}
	locked, limited, err := s.loginThrottle.check(c.Request.Context(), ip, username)
	if err != nil {
		s.loginDefenseUnavailable(c, err, alreadyResponded)
		return false
	}
	if locked || limited {
		if locked {
			s.mLogin(metrics.ResultLocked)
		} else {
			s.mLogin(metrics.ResultRateLimited)
		}
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too_many_attempts"})
		return false
	}
	return true
}

// loginDefenseUnavailable 是防御层 DB 故障的统一面:不付 bcrypt、不计数
// (身份源同库本就不可用)、500 响亮且日志携根因(排障须能区分 DB 断/
// 1205 锁等待/授权缺失;响应体保持 generic)。alreadyResponded 时只拦
// 不二写。
func (s *Server) loginDefenseUnavailable(c *gin.Context, err error, alreadyResponded bool) {
	slog.Error("api: login defense unavailable", "err", err)
	if !alreadyResponded {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
	}
}

func (s *Server) login(c *gin.Context) {
	ip := c.ClientIP()
	var req loginRequest
	b, ok := readBodyLimited(c)
	if !ok {
		// readBodyLimited 已写终态响应(413/400):此处只消费窗口与发指标,
		// 不再二次写响应(rev3 双写缺陷修复)。超限 body 同样计入 IP 窗口。
		s.loginDefenseGate(c, ip, "", true)
		return
	}
	if err := json.Unmarshal(b, &req); err != nil {
		// 坏 JSON 同样计入 IP 固定窗口(review HIGH:此路径原先绕过防御层,
		// 每请求一行审计 = 未限流的持久写放大)。只发指标不落 DB——
		// username 不可知,持久细节留给后续可解析请求的 failed 行。
		if !s.loginDefenseGate(c, ip, "", false) {
			return
		}
		s.mLogin(metrics.ResultFailed)
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
		return
	}
	// 认证前防御闸(不付 bcrypt;锁定与限流同形 429,防枚举口径见上)。
	if !s.loginDefenseGate(c, ip, req.Username, false) {
		return
	}
	user, err := s.idp.Authenticate(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		var creds *auth.CredentialsError
		switch {
		case errors.As(err, &creds):
			// 主体名有界化进日志(见 logUsername);Reason 是日志词
			// (auth/errors.go 契约),绝不进响应;审计行携 Reason(DB 面
			// 不对外,防枚举契约不受影响)。
			slog.Info("api: login rejected", "username", logUsername(creds.Username), "reason", creds.Reason)
			s.loginThrottle.record(c.Request.Context(), req.Username, false)
			s.loginAuditEvent(c.Request.Context(), loginEventFailed, loginMethodPassword, ip, creds.Username, creds.Reason)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_credentials"})
		case errors.Is(err, auth.ErrUserDisabled):
			s.loginThrottle.record(c.Request.Context(), req.Username, false)
			s.loginAuditEvent(c.Request.Context(), loginEventFailed, loginMethodPassword, ip, req.Username, "user_disabled")
			c.JSON(http.StatusForbidden, gin.H{"error": "user_disabled"})
		default:
			// 身份源故障不是登录判定结果:不计数不审计(与 500 同判,
			// 旁路观测由日志承载)。
			slog.Error("api: login authenticate", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		}
		return
	}
	s.loginThrottle.record(c.Request.Context(), req.Username, true)
	s.loginAuditEvent(c.Request.Context(), loginEventSucceeded, loginMethodPassword, ip, user.Username, "")
	ttl := auth.DefaultTokenTTL
	// token 绑定(T11.3):按当前模式携 bnd claim(off = 空串不带;
	// 与 Authenticate 校验共用 bindingFingerprint 单点,oidc 回调同契约)。
	// 租户快照(D-t112):user.TenantID → tnt claim;两签发点同契约。
	// 作用域(D-t131 决策 3 + T17.1/D-t171):login 签发按角色默认全量——
	// REST 面逐端点执法该集(scp 在场即闸,缺席透传 = 存量兼容),MCP 面
	// 对新 token 全量、对存量(无 scp)fail-closed。
	token, err := auth.SignTokenScoped(s.cfg.JWTSecret, ttl, user.Username, user.Roles, user.TenantID,
		bindingFingerprint(s.cfg.TokenBinding, c.Request.UserAgent(), ip),
		auth.DefaultScopesForRoles(user.Roles))
	if err != nil {
		// 词表外角色/签名密钥问题:启动期 Validate 已拦密钥面,到达此处即
		// 数据或接线异常,响亮 500 不伪造成功。
		slog.Error("api: login sign token", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		return
	}
	// token 出现在响应体:禁中间层缓存(T11.1 起 token 携带面统一契约,
	// oidc 回调同判)。
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, loginResponse{
		Token:     token,
		TokenType: "Bearer",
		ExpiresAt: time.Now().UTC().Add(ttl).Format(time.RFC3339),
		Username:  user.Username,
		Roles:     user.Roles,
	})
}
