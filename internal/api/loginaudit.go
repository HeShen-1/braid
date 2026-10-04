// 登录审计(T6.4,D-t44 §7-① 第三件):login_audit 表(迁移 0005)的
// 词表与写入器。append-only 账号域事实,与 audit_log(工作流事实域,
// T2.6 契约)物理分表;Prometheus braid_login_attempts_total 是告警消费
// 半边,本表是可 SQL 审计的历史半边。
//
// T17.2(0013)起本表升格为账号域安全事件流双面:登录事件(login.*,
// 携 method = 口令/OIDC 可区分)+ 自窄化签发事件(scoped_token_issued
// ,主体/作用域/TTL/token 哈希——D-t131 ④收编;audit_log 结构性
// 进不去,无执行锚的账号域事实归本表,0005 头注释同判)。
//
// 写入契约(镜像 writeTriggerAudit 先例):独立小事务 + best-effort——
// 登录判定/签发响应不被旁路审计失败翻转;密码/token 永不入行
// (铁律——签发行的 token_hash 是 SHA-256 hex,哈希至多)。
package api

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"

	"braid/internal/auth"
	"braid/internal/engine"
	"braid/internal/metrics"
)

// loginAuditEvent 是 login_audit.event_type 词表(闭集,与 DDL ENUM
// 同步;追加 = DDL 末尾 + 此处 + 测试锚同 PR 旅行)。locked/rate_limited
// 当前零持久发射面(防御拒绝 metric-only,rev2 拍板):两词是词表预留
// (与指标 label 同形),未来需要持久防御事件时开启;append-only 词表
// 不回缩。scoped_token_issued(T17.2,D-t131 ④收编):/auth/tokens 自
// 窄化签发事实——签发行走 writeScopedTokenAudit 专用写入器,不进
// loginResultLabel/mLogin(登录指标是登录判定的观测半边,签发计数
// 混入会把 label 词面污染成不可归因的混合体)。
const (
	loginEventSucceeded   = "login.succeeded"
	loginEventFailed      = "login.failed"
	loginEventLocked      = "login.locked"
	loginEventRateLimited = "login.rate_limited"
	loginEventTokenIssued = "scoped_token_issued"
)

var knownLoginEvents = map[string]bool{
	loginEventSucceeded:   true,
	loginEventFailed:      true,
	loginEventLocked:      true,
	loginEventRateLimited: true,
	loginEventTokenIssued: true,
}

// loginMethod 是 login_audit.method 词域(0013 立,T17.2;0014 尾追加
// 'ldap' 兑现预留,T17.6)。闭集仅末尾追加,与 DDL ENUM 同步锚 =
// TestKnownLoginMethodsClosedVocabulary;ldap = 企业树 entldap 凭证
// 核验缝换装时的审计归因词(装配面 CredentialLoginMethod 旋钮)。
// login.* 事件必携非空值,scoped_token_issued 行恒 NULL
// (chk_login_audit_method 双向钉死)。存量行(0013 前)两路混流不可
// 辨、一律 backfill password——口径钉在 0013 列注释:0013 前 password
// 计数含 OIDC,统计消费方须知。
const (
	loginMethodOIDC     = "oidc"
	loginMethodPassword = "password"
	loginMethodLDAP     = "ldap"
)

// LoginMethodLDAP 是企业树 entldap 换装凭证核验缝时的审计归因词导出面
// (0014;组合根 cmd/api 装配面引用——CredentialLoginMethod 的调用方
// 不落裸字面量,词域单源)。
const LoginMethodLDAP = loginMethodLDAP

var knownLoginMethods = map[string]bool{
	loginMethodOIDC:     true,
	loginMethodPassword: true,
	loginMethodLDAP:     true,
}

// maxAuditUsername 对齐 login_audit.username VARCHAR(128):审计记原样,
// rune 尾部安全截断(域外/恶意输入同样留痕,但不撑列宽)。
const maxAuditUsername = 128

func auditUsername(s string) string {
	if len(s) <= maxAuditUsername {
		return s
	}
	cut := s[:maxAuditUsername]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// writeLoginAudit 落一行登录事件(best-effort:失败只记日志)。method =
// 登录方法(0013:口令/OIDC 可区分,login.* 事件必携,闭集 fail-closed
// ——漏传在此响亮,不静默落不可归因行)。reason 仅 login.failed 携带
// (auth 内部日志词表:invalid_input/unknown_user/wrong_password/
// user_disabled),其余事件恒空串写 NULL。scoped_token_issued 不进本
// 写入器:签发行的列形状与不变量不同(method 恒 NULL),走
// writeScopedTokenAudit。
func (s *Server) writeLoginAudit(ctx context.Context, eventType, method, ip, username, reason string) {
	if !knownLoginEvents[eventType] || eventType == loginEventTokenIssued {
		// 闭集 fail-closed:调用面词表漂移在此响亮,不静默落垃圾行。
		slog.Error("api: login audit event outside vocabulary", "event", eventType)
		return
	}
	if !knownLoginMethods[method] {
		slog.Error("api: login audit method outside vocabulary", "event", eventType, "method", method)
		return
	}
	if s.ids == nil || s.dbh == nil {
		return // 无生成器/无 DB 的桩构造(零栈测试面):审计面不阻塞
	}
	id, err := s.ids.Next()
	if err != nil {
		slog.Error("api: login audit id", "err", err)
		return
	}
	u := auditUsername(username)
	var reasonArg any
	if reason != "" {
		reasonArg = reason
	}
	if err := engine.WithTx(ctx, s.dbh, func(q engine.DBTX) error {
		_, err := q.ExecContext(ctx, `
INSERT INTO login_audit (id, event_type, method, username, client_ip, reason)
VALUES (?, ?, ?, ?, ?, ?)`, id, eventType, method, u, ip, reasonArg)
		return err
	}); err != nil {
		slog.Error("api: login audit write",
			"event", eventType, "username", u, "err", err)
	}
}

// tokenHashHexRe 是 token_hash 的域(SHA-256 hex,恰 64 小写十六进制;
// writeScopedTokenAudit 的防御闸——调用方传错在此响亮,不落坏行)。
var tokenHashHexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// writeScopedTokenAudit 落一行 scoped_token_issued(0013;签发面 =
// tokens.go issueToken,签发成功后发射,best-effort——旁路审计失败
// 不翻转签发响应,writeLoginAudit 同判)。主体复用 username 列(审计
// 记原样有界)、来源 IP 复用 client_ip 列;scopes 逗号 join(词表
// resource:action 无逗号,无歧义)、ttlSeconds = clamp 后实际授予值
// (子 TTL ≤ 呈现 token 剩余寿命的收窄事实)、tokenHash = 签发 token
// 的 SHA-256 hex——token 原值绝不入审计(铁律),哈希至多。
func (s *Server) writeScopedTokenAudit(ctx context.Context, ip, username string, scopes []auth.Scope, ttlSeconds int64, tokenHash string) {
	if len(scopes) == 0 || ttlSeconds <= 0 || !tokenHashHexRe.MatchString(tokenHash) {
		slog.Error("api: scoped token audit invalid args",
			"scopes", len(scopes), "ttl_seconds", ttlSeconds, "hash_len", len(tokenHash))
		return
	}
	if s.ids == nil || s.dbh == nil {
		return // 无生成器/无 DB 的桩构造(零栈测试面):审计面不阻塞
	}
	id, err := s.ids.Next()
	if err != nil {
		slog.Error("api: scoped token audit id", "err", err)
		return
	}
	parts := make([]string, len(scopes))
	for i, sc := range scopes {
		parts[i] = string(sc)
	}
	u := auditUsername(username)
	if err := engine.WithTx(ctx, s.dbh, func(q engine.DBTX) error {
		// method 显式 NULL:省列会落 DEFAULT 'password',被
		// chk_login_audit_method 拒(3819)——DB 侧同款 fail-closed。
		_, err := q.ExecContext(ctx, `
INSERT INTO login_audit (id, event_type, method, username, client_ip, scopes, ttl_seconds, token_hash)
VALUES (?, 'scoped_token_issued', NULL, ?, ?, ?, ?, ?)`,
			id, u, ip, strings.Join(parts, ","), ttlSeconds, tokenHash)
		return err
	}); err != nil {
		slog.Error("api: scoped token audit write", "username", u, "err", err)
	}
}

// loginResultLabel 把审计事件映射为指标 label(词表单源复用 metrics
// 包常量,防两套词面漂移——cmd/worker 锚测试同判)。
func loginResultLabel(eventType string) string {
	switch eventType {
	case loginEventSucceeded:
		return metrics.ResultSucceeded
	case loginEventLocked:
		return metrics.ResultLocked
	case loginEventRateLimited:
		return metrics.ResultRateLimited
	default:
		return metrics.ResultFailed
	}
}
