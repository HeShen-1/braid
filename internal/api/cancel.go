package api

// T19.3 执行取消端点(D-2026-10-05-t193-execution-cancel-and-rollback-
// whitelist 决策 5):POST /executions/:id/cancel——scheduler.CancelExecution
// 原语(RUNNING/WA 楔停)的 api 同步事务暴露,零新队列/词表(rollback 端点
// 同款论证)。幂等 = FinishExecution status-only CAS 败者 409(重复点击报错
// 非重放);RBAC = author|admin in-handler 闸(拒绝必须落审计 cancel.denied,
// decideApproval/rollback 双先例)。

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"braid/internal/audit"
	"braid/internal/auth"
	"braid/internal/engine"
	"braid/internal/scheduler"
)

// cancelDTO 是取消成功响应(200;契约回显,snowflake 字符串化防 IEEE754 折叠)。
type cancelDTO struct {
	ExecutionID    string `json:"execution_id"`
	FromStatus     string `json:"from_status"`
	CancelledCount int    `json:"cancelled_count"`
}

// cancelExecution:POST /executions/:id/cancel。
func (s *Server) cancelExecution(c *gin.Context) {
	execID, ok := parseIDParam(c)
	if !ok {
		return
	}
	// in-handler 角色闸(与回滚/触发同门):拒绝必须落审计 cancel.denied
	//(独立小事务,writeRollbackDeniedAudit 同款纪律)。
	claims, ok := ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing_token"})
		return
	}
	if !claimsHaveAnyRole(claims, auth.RoleAuthor, auth.RoleAdmin) {
		if s.execVisibleInTenant(c, execID) {
			s.writeCancelDeniedAudit(c, execID, claims.Subject, "role_denied")
		}
		if s.cancelMetrics != nil {
			s.cancelMetrics.Denied.WithLabelValues("role_denied").Inc()
		}
		c.JSON(http.StatusForbidden, gin.H{"error": "role_denied"})
		return
	}
	if s.cancelMetrics != nil {
		s.cancelMetrics.Requested.Inc()
	}

	// 租户闸(D-t112):跨租户 execID 直取 = 404(与「不存在」同面);
	// 行的 tenant_id 不可变 ⇒ 无 TOCTOU(decideApproval/rollback 同构)。
	if _, err := s.st.GetExecution(c.Request.Context(), s.dbh, tenantFrom(c), execID); err != nil {
		mapStoreError(c, err)
		return
	}

	res, err := s.trig.CancelExecution(c.Request.Context(), scheduler.CancelParams{
		ExecID:   execID,
		WorkerID: "api:cancel",
		ActorID:  claims.Subject,
	})
	if err != nil {
		var ce *scheduler.CancelError
		if errors.As(err, &ce) {
			s.writeCancelDeniedAudit(c, execID, claims.Subject, string(ce.Kind))
		}
		if s.cancelMetrics != nil {
			if ce != nil {
				s.cancelMetrics.Denied.WithLabelValues(string(ce.Kind)).Inc()
			} else {
				s.cancelMetrics.Denied.WithLabelValues("internal").Inc()
			}
		}
		s.mapCancelError(c, err)
		return
	}
	if s.cancelMetrics != nil {
		s.cancelMetrics.Applied.Inc()
	}
	c.JSON(http.StatusOK, cancelDTO{
		ExecutionID:    strconv.FormatUint(execID, 10),
		FromStatus:     string(res.FromStatus),
		CancelledCount: res.CancelledCount,
	})
}

// mapCancelError 是取消面错误映射(mapRollbackError 同构):结构化
// CancelError 按 Kind 分面;engine.ErrNotFound → 404;裸 ErrConflict =
// 并发终局化败者(守卫后不可达路径的 CAS 兜底)。
func (s *Server) mapCancelError(c *gin.Context, err error) {
	var ce *scheduler.CancelError
	switch {
	case errors.As(err, &ce):
		switch ce.Kind {
		case scheduler.CancelNotCancellable:
			c.JSON(http.StatusConflict, gin.H{"error": "not_cancellable", "detail": ce.Detail})
		case scheduler.CancelTooLarge:
			// 与回滚共闸(BRAID_ROLLBACK_MAX_ROWS 单源):运维出路同
			// rollback_too_large——调闸或拆执行,不重试。
			c.JSON(http.StatusConflict, gin.H{"error": "cancel_too_large", "detail": ce.Detail})
		default:
			slog.Error("api: cancel failure", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		}
	case errors.Is(err, engine.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
	case errors.Is(err, engine.ErrConflict):
		c.JSON(http.StatusConflict, gin.H{"error": "conflict"})
	default:
		slog.Error("api: cancel store failure", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
	}
}

// writeCancelDeniedAudit:拒绝落审计(独立小事务,best-effort;镜像
// writeRollbackDeniedAudit 全部纪律——执行缺行时 Append 分区键点查失败 =
// 天然防灌水)。
func (s *Server) writeCancelDeniedAudit(c *gin.Context, execID uint64, actor, reason string) {
	id, err := s.ids.Next()
	if err != nil {
		slog.Error("api: gen cancel.denied id", "err", err)
		return
	}
	raw, err := json.Marshal(audit.CancelDeniedPayload{Reason: reason})
	if err != nil {
		slog.Error("api: marshal cancel.denied payload", "err", err)
		return
	}
	// T9.3 串链协议要求调用方事务(裸池句柄 ⇒ 链叉)。
	if err := engine.WithTx(c.Request.Context(), s.dbh, func(q engine.DBTX) error {
		return audit.Append(c.Request.Context(), q, audit.Event{
			ID: id, EventType: audit.EventCancelDenied, ExecutionID: execID,
			ActorType: audit.ActorUser, ActorID: actor, Payload: raw,
		})
	}); err != nil {
		slog.Error("api: anchor cancel.denied", "err", err, "execution_id", execID)
	}
}
