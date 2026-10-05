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
// (字典序首个 FAILED → 字典序首个 CANCELLED[T19.3 楔源档,D-2026-10-05-t193
// 决策 1]→ 前驱就绪的 PENDING 冻结行;三档全空 → 400 no_resume_target)。
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
	// 体上界与全仓写面单源(readBodyLimited/MaxBytesReader;对抗审计 HIGH 修:
	// 裸 ShouldBindJSON 是全仓唯一无体上界的写面,且体解析先于角色闸)。
	// 空 body(resume 无参语法糖)合法:零字节 → 零值请求。
	var req rollbackRequest
	b, ok := readBodyLimited(c)
	if !ok {
		return
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
			return
		}
	}
	// node_id 形状闸(镜像审批面):先于角色闸/审计,防超长值穿透到
	// 审计行的 NodeID 校验(对抗审计 MEDIUM 修:denied 审计静默吞)。
	if req.NodeID != "" && !nodeIDRe.MatchString(req.NodeID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_node_id"})
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
		// 落行闸(D-t112 审计收口):同 decideApproval——拒绝审计只对
		// 本租户可见执行落,跨租户/不存在不注入受害者证据链。
		if s.execVisibleInTenant(c, execID) {
			s.writeRollbackDeniedAudit(c, execID, req.NodeID, claims.Subject, "role_denied", 0)
		}
		if s.rollbackMetrics != nil {
			s.rollbackMetrics.Denied.WithLabelValues("role_denied").Inc()
		}
		c.JSON(http.StatusForbidden, gin.H{"error": "role_denied"})
		return
	}
	if s.rollbackMetrics != nil {
		s.rollbackMetrics.Requested.Inc()
	}

	// 租户闸(D-t112):跨租户 execID 直取 = 404(与「不存在」同面)。
	// 执行前置检查在 RollbackTo 的执行读取之外,但行的 tenant_id 不可变
	// (无 UPDATE 路径)⇒ 无 TOCTOU;深读面(节点/快照)全部经本闸后
	// 才可达。decideApproval 的 GetExecution 前置同构。
	if _, err := s.st.GetExecution(c.Request.Context(), s.dbh, tenantFrom(c), execID); err != nil {
		mapStoreError(c, err)
		return
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
		var re *scheduler.RollbackError
		if errors.As(err, &re) {
			// 业务拒绝全分面落审计(对抗审计 HIGH 修:此前仅 role_denied
			// 落行,与 D-t92 决策 9/10 及告警 runbook 引用的证据源脱节)。
			// infra 错误/conflict(瞬态/并发收敛面)不落——自愈路径不灌水。
			s.writeRollbackDeniedAudit(c, execID, req.NodeID, claims.Subject, string(re.Kind), len(re.Blocked))
		}
		if s.rollbackMetrics != nil {
			if re != nil {
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
		case scheduler.RollbackTooLarge:
			// T15.5 容量闸:锁窗超档拒绝(与状态族同为 409——请求本身合法,
			// 是当前资源形态不可受理)。闸是执行级前置判定,收窄目标不适用;
			// 运维出路 = 拆执行 / 调 BRAID_ROLLBACK_MAX_ROWS(0 = 关闭),不重试。
			c.JSON(http.StatusConflict, gin.H{"error": "rollback_too_large", "detail": re.Detail})
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

// writeRollbackDeniedAudit:拒绝落审计(独立小事务,best-effort——裁决不受
// 旁路写入失败影响;失败仅 slog 零遥测不可接受,对抗审计 LOW 修)。全部
// 业务分面(role_denied/not_rollbackable/blocked_inflight/bad_node_id/
// no_resume_target)都落行:告警 runbook 与 T6.4 denied 计数器的证据源
// (对抗审计 HIGH 修:此前仅 role_denied 落行)。执行缺行时 Append 的分区键
// 点查失败 = 天然防灌水,镜像 decided 面的锚定语义。
func (s *Server) writeRollbackDeniedAudit(c *gin.Context, execID uint64, nodeID, actor, reason string, blockedCount int) {
	id, err := s.ids.Next()
	if err != nil {
		slog.Error("api: gen rollback.denied id", "err", err)
		return
	}
	raw, err := json.Marshal(audit.RollbackDeniedPayload{
		Reason: reason, TargetNodeID: nodeID, BlockedCount: blockedCount,
	})
	if err != nil {
		slog.Error("api: marshal rollback.denied payload", "err", err)
		return
	}
	// T9.3:串链协议要求调用方事务(裸池句柄 ⇒ 链叉);"独立小事务"从
	// 注释变为事实。
	if err := engine.WithTx(c.Request.Context(), s.dbh, func(q engine.DBTX) error {
		return audit.Append(c.Request.Context(), q, audit.Event{
			ID: id, EventType: audit.EventRollbackDenied, ExecutionID: execID,
			NodeID: nodeID, ActorType: audit.ActorUser, ActorID: actor, Payload: raw,
		})
	}); err != nil {
		slog.Error("api: anchor rollback.denied", "err", err, "execution_id", execID)
	}
}
