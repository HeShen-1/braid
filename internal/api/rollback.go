package api

// T9.2 回滚/断点续跑端点(D-2026-09-28-t92-rollback-same-branch 决策 5/8):
// POST /executions/:id/rollback {node_id 必填} 与 POST /executions/:id/resume
// {node_id 可选(自动定位)}——同一 scheduler.RollbackTo 原语(resume 是
// 语法糖),api 同步事务、零新队列/词表。幂等 = wf status-only CAS 败者 409
//(重放 200 语义停车场);RBAC = author|admin in-handler 闸(拒绝必须落审计
// rollback.denied,decideApproval 先例——路由中间件 403 无审计面)。

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"braid/internal/audit"
	"braid/internal/auth"
	"braid/internal/engine"
	"braid/internal/scheduler"
)

// rollbackRequest 是两端点共用请求体(resume 的 node_id 可缺省)。
type rollbackRequest struct {
	NodeID string `json:"node_id"`
}

// rollbackDTO 是回滚成功响应(201;契约回显,镜像 executionAckDTO 纪律——
// snowflake 字符串化防 IEEE754 折叠)。
type rollbackDTO struct {
	ExecutionID  string `json:"execution_id"`
	TargetNodeID string `json:"target_node_id"`
	RerunTarget  bool   `json:"rerun_target"`
	ResetCount   int    `json:"reset_count"`
}

// rollbackExecution:POST /executions/:id/rollback —— node_id 必填。
func (s *Server) rollbackExecution(c *gin.Context) {
	s.handleRollback(c, false)
}

// resumeExecution:POST /executions/:id/resume —— node_id 缺省自动定位
// (字典序首个 FAILED,退化到前驱就绪的 PENDING 冻结行,D-t92 决策 5)。
func (s *Server) resumeExecution(c *gin.Context) {
	s.handleRollback(c, true)
}

// handleRollback 是两端点的单实现(allowAutoTarget 区分 node_id 缺省语义:
// rollback 缺省 = 400,resume 缺省 = 自动定位)。
func (s *Server) handleRollback(c *gin.Context, allowAutoTarget bool) {
	execID, ok := parseIDParam(c)
	if !ok {
		return
	}
	var req rollbackRequest
	// 空 body(resume 无参语法糖)合法:EOF 视作零值请求,仅真畸形体 400。
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body"})
		return
	}
	if req.NodeID == "" && !allowAutoTarget {
		c.JSON(http.StatusBadRequest, gin.H{"error": "node_id_required"})
		return
	}
	// in-handler 角色闸(author|admin,与触发面同门):拒绝必须落审计
	// rollback.denied(独立小事务,writeDecisionDeniedAudit 同款纪律)。
	claims, ok := ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing_token"})
		return
	}
	if !claimsHaveAnyRole(claims, auth.RoleAuthor, auth.RoleAdmin) {
		s.writeRollbackDeniedAudit(c, execID, req.NodeID, claims.Subject, "role_denied")
		if s.rollbackMetrics != nil {
			s.rollbackMetrics.Denied.WithLabelValues("role_denied").Inc()
		}
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}
	if s.rollbackMetrics != nil {
		s.rollbackMetrics.Requested.Inc()
	}

	res, err := s.trig.RollbackTo(c.Request.Context(), scheduler.RollbackParams{
		ExecID:       execID,
		TargetNodeID: req.NodeID,
		// 派发归因(RerunNode worker_id + att 事件):api 侧固定身份形态,
		// 与 reaper:<id> 同式的进程词表。
		WorkerID: "api:rollback",
		ActorID:  claims.Subject,
	})
	if err != nil {
		if s.rollbackMetrics != nil {
			var re *scheduler.RollbackError
			if errors.As(err, &re) {
				s.rollbackMetrics.Denied.WithLabelValues(string(re.Kind)).Inc()
			} else {
				s.rollbackMetrics.Denied.WithLabelValues("internal").Inc()
			}
		}
		s.mapRollbackError(c, err)
		return
	}
	if s.rollbackMetrics != nil {
		s.rollbackMetrics.Applied.Inc()
	}
	c.JSON(http.StatusCreated, rollbackDTO{
		ExecutionID:  strconv.FormatUint(execID, 10),
		TargetNodeID: res.TargetNodeID,
		RerunTarget:  res.RerunTarget,
		ResetCount:   res.ResetCount,
	})
}

// mapRollbackError 是回滚面错误映射第三单点(mapApprovalError 先例):
// 结构化 RollbackError 按 Kind 分面;engine.ErrNotFound → 404;
// 裸 ErrConflict = 并发双回滚败者(锁后当前读/CAS 双兜底收敛面)。
// 业务拒绝不重复落审计——决定事务自身的 execution.rolled_back 行是成功面
// 事实源,拒绝面的可观测走 metrics(T6.4 侧)。
func (s *Server) mapRollbackError(c *gin.Context, err error) {
	var re *scheduler.RollbackError
	switch {
	case errors.As(err, &re):
		switch re.Kind {
		case scheduler.RollbackNotRollbackable:
			c.JSON(http.StatusConflict, gin.H{"error": "not_rollbackable", "detail": re.Detail})
		case scheduler.RollbackBlockedInflight:
			// Retry-After = reaper settle 的量级(orphanQuiet 2min + tick);
			// 阻塞清单引导运维/自动化按行退避(D-t92 决策 4②)。
			c.Header("Retry-After", "120")
			c.JSON(http.StatusConflict, gin.H{
				"error": "rollback_blocked_inflight", "detail": re.Detail, "blocked": re.Blocked})
		case scheduler.RollbackBadNode:
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad_node_id", "detail": re.Detail})
		case scheduler.RollbackNoTarget:
			c.JSON(http.StatusBadRequest, gin.H{"error": "no_resume_target"})
		default:
			slog.Error("api: rollback failure", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		}
	case errors.Is(err, engine.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
	case errors.Is(err, engine.ErrConflict):
		c.JSON(http.StatusConflict, gin.H{"error": "conflict"})
	default:
		slog.Error("api: rollback store failure", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
	}
}

// writeRollbackDeniedAudit:越权拒绝落审计(独立小事务,best-effort——
// 403 的裁决不受旁路写入失败影响;执行缺行时 Append 的分区键点查失败
// = 天然防灌水,镜像 decided 面的锚定语义)。
func (s *Server) writeRollbackDeniedAudit(c *gin.Context, execID uint64, nodeID, actor, reason string) {
	id, err := s.ids.Next()
	if err != nil {
		return
	}
	raw, err := json.Marshal(audit.RollbackDeniedPayload{
		Reason: reason, TargetNodeID: nodeID,
	})
	if err != nil {
		return
	}
	if err := audit.Append(c.Request.Context(), s.dbh, audit.Event{
		ID: id, EventType: audit.EventRollbackDenied, ExecutionID: execID,
		NodeID: nodeID, ActorType: audit.ActorUser, ActorID: actor, Payload: raw,
	}); err != nil {
		slog.Error("api: anchor rollback.denied", "err", err, "execution_id", execID)
	}
}
