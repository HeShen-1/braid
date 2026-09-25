// 登录审计(T6.4,D-t44 §7-① 第三件):login_audit 表(迁移 0005)的
// 词表与写入器。append-only 账号域事实,与 audit_log(工作流事实域,
// T2.6 契约)物理分表;Prometheus braid_login_attempts_total 是告警消费
// 半边,本表是可 SQL 审计的历史半边。
//
// 写入契约(镜像 writeTriggerAudit 先例):独立小事务 + best-effort——
// 登录判定不被旁路审计失败翻转;密码/token 永不入行(铁律)。
package api

import (
	"context"
	"log/slog"
	"unicode/utf8"

	"braid/internal/engine"
	"braid/internal/metrics"
)

// loginAuditEvent 是 login_audit.event_type 词表(闭集,与 DDL ENUM
// 同步;追加 = DDL 末尾 + 此处 + 测试锚同 PR 旅行)。locked/rate_limited
// 当前零持久发射面(防御拒绝 metric-only,rev2 拍板):两词是词表预留
// (与指标 label 同形),未来需要持久防御事件时开启;append-only 词表
// 不回缩。
const (
	loginEventSucceeded   = "login.succeeded"
	loginEventFailed      = "login.failed"
	loginEventLocked      = "login.locked"
	loginEventRateLimited = "login.rate_limited"
)

var knownLoginEvents = map[string]bool{
	loginEventSucceeded:   true,
	loginEventFailed:      true,
	loginEventLocked:      true,
	loginEventRateLimited: true,
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

// writeLoginAudit 落一行登录事件(best-effort:失败只记日志)。reason 仅
// login.failed 携带(auth 内部日志词表:invalid_input/unknown_user/
// wrong_password/user_disabled),其余事件恒空串写 NULL。
func (s *Server) writeLoginAudit(ctx context.Context, eventType, ip, username, reason string) {
	if !knownLoginEvents[eventType] {
		// 闭集 fail-closed:调用面词表漂移在此响亮,不静默落垃圾行。
		slog.Error("api: login audit event outside vocabulary", "event", eventType)
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
INSERT INTO login_audit (id, event_type, username, client_ip, reason)
VALUES (?, ?, ?, ?, ?)`, id, eventType, u, ip, reasonArg)
		return err
	}); err != nil {
		slog.Error("api: login audit write",
			"event", eventType, "username", u, "err", err)
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
