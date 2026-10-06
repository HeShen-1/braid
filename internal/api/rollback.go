package api

// T9.2 回滚/断点续跑端点(D-2026-09-28-t92-rollback-same-branch 决策 5/8):
// POST /executions/:id/rollback {node_id 必填} 与 POST /executions/:id/resume
// {node_id 可选(自动定位)}——同一 scheduler.RollbackTo 原语(resume 是
// 语法糖),api 同步事务、零新队列/词表。幂等 = 重放 200(T18.5 兑现
// D-t92 决策 8 停车场:重复/并发败者读回最新 execution.rolled_back 台账
// 匹配 target → 200 回显首答,审批同轮重放先例;无台账/异 target 保持
// 409 拒绝);RBAC = author|admin in-handler 闸(拒绝必须落审计
// rollback.denied,decideApproval 先例——路由中间件 403 无审计面)。

import (
	"database/sql"
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

// rollbackRequest 是两端点共用请求体(resume 的 node_id 可缺省)。quiesce
// 是 T19.6(D-2026-10-06-t196 决策 1)收敛旗标:true 时 EXECUTING/WA 残留
// 在回滚事务内楔停(缺省 false = 409 blocked_inflight 等 reaper settle,
// 等待保值语义:残留自然完成可出闭包并保活)。
type rollbackRequest struct {
	NodeID  string `json:"node_id"`
	Quiesce bool   `json:"quiesce"`
}

// rollbackDTO 是回滚成功响应(201;契约回显,镜像 executionAckDTO 纪律——
// snowflake 字符串化防 IEEE754 折叠)。quiesced_count = 楔停收敛的残留行数
// (重放重建同源,T18.5 台账镜像)。
type rollbackDTO struct {
	ExecutionID   string `json:"execution_id"`
	TargetNodeID  string `json:"target_node_id"`
	RerunTarget   bool   `json:"rerun_target"`
	ResetCount    int    `json:"reset_count"`
	QuiescedCount int    `json:"quiesced_count"`
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
			s.writeRollbackDeniedAudit(c, execID, req.NodeID, claims.Subject, "role_denied", 0, req.Quiesce)
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
		Quiesce:  req.Quiesce,
	})
	if err != nil {
		// T18.5 重放收敛:重复请求(顺序重复 = 守卫① not_rollbackable)与
		// 并发双回滚败者(裸 ErrConflict CAS 败)读回台账匹配 target →
		// 200 回显首答(零新写、不灌 denied——T6.4 denied 计数器/告警只吃
		// 真拒绝)。blocked_inflight/too_large 等是首答未生效的新拒绝,
		// 重放旧答案会失真,不进漏斗。
		if rollbackReplayable(err) {
			if dto, ok := s.tryRollbackReplay(c, execID, req.NodeID); ok {
				if s.rollbackMetrics != nil {
					s.rollbackMetrics.Replayed.Inc()
				}
				c.JSON(http.StatusOK, dto)
				return
			}
		}
		var re *scheduler.RollbackError
		if errors.As(err, &re) {
			// 业务拒绝全分面落审计(对抗审计 HIGH 修:此前仅 role_denied
			// 落行,与 D-t92 决策 9/10 及告警 runbook 引用的证据源脱节)。
			// infra 错误/conflict(瞬态/并发收敛面)不落——自愈路径不灌水。
			s.writeRollbackDeniedAudit(c, execID, req.NodeID, claims.Subject, string(re.Kind), len(re.Blocked), req.Quiesce)
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
		if res.QuiescedCount > 0 {
			// T19.6:楔停收敛回滚单独计数(Replayed 先例——只计真发生
			// 楔停的成功面,零残留 no-op 不灌水)。
			s.rollbackMetrics.Quiesced.Inc()
		}
	}
	c.JSON(http.StatusCreated, rollbackDTO{
		ExecutionID:   strconv.FormatUint(execID, 10),
		TargetNodeID:  res.TargetNodeID,
		RerunTarget:   res.RerunTarget,
		ResetCount:    res.ResetCount,
		QuiescedCount: res.QuiescedCount,
	})
}

// rollbackReplayable 报告错误是否落在重放收敛面:守卫① not_rollbackable
// (wf 非终态——顺序重复回滚与并发败者的当前读形态)或裸 ErrConflict
// (并发败者的 CAS 形态)。其余拒绝面语义各异(在飞残留等 settle / 容量
// 闸),读回旧答案会失真。
func rollbackReplayable(err error) bool {
	var re *scheduler.RollbackError
	if errors.As(err, &re) {
		return re.Kind == scheduler.RollbackNotRollbackable
	}
	return errors.Is(err, engine.ErrConflict)
}

// tryRollbackReplay 读回该执行最新 execution.rolled_back 行重建首答 DTO。
// 显式 target 须与台账一致;resume(auto target)在 wf 非终态下无法重解析
// (失败源已被首答收编),采信台账 target。读失败/无台账/目标不符 =
// false,调用方直通原拒绝语义(不吞原调度器错误:重放是旁路不是兜底;
// 自身读失败留 slog——零遥测的错误吸收点是本 handler 唯一例外,不做)。
func (s *Server) tryRollbackReplay(c *gin.Context, execID uint64, nodeID string) (rollbackDTO, bool) {
	// 薄读(对抗审计 MEDIUM 修):GetExecution 全列投影拖 input/outputs
	// 大 blob——重试风暴面恰是重放要吸收的,读侧不放大;exec_date 与
	// execCols 同式 DB 端单源。ErrConflict 形态下 wf 若仍终态 = 冲突源
	// 未必是「回滚已被并发赢家兑现」(本调用面裸 ErrConflict 源 =
	// RestartExecution status-only CAS 败,快照窗后 wf 已被他者迁移),
	// 终态时重放旧纪元答案是失真,保持冲突语义。
	var status, execDate string
	if err := s.dbh.QueryRowContext(c.Request.Context(),
		`SELECT status, DATE_FORMAT(created_at,'%Y-%m-%d') FROM workflow_execution WHERE id=?`,
		execID).Scan(&status, &execDate); err != nil {
		slog.Error("api: rollback replay read execution", "err", err, "execution_id", execID)
		return rollbackDTO{}, false
	}
	if engine.WorkflowState(status).Terminal() {
		return rollbackDTO{}, false
	}
	p, found, err := s.st.LatestRolledBackPayload(c.Request.Context(), s.dbh,
		tenantFrom(c), execID, execDate)
	if err != nil {
		slog.Error("api: rollback replay read ledger", "err", err, "execution_id", execID)
		return rollbackDTO{}, false
	}
	if !found {
		return rollbackDTO{}, false
	}
	if nodeID != "" && nodeID != p.TargetNodeID {
		return rollbackDTO{}, false
	}
	return rollbackDTO{
		ExecutionID:   strconv.FormatUint(execID, 10),
		TargetNodeID:  p.TargetNodeID,
		RerunTarget:   p.RerunTarget,
		ResetCount:    p.ResetCount,
		QuiescedCount: p.QuiescedCount, // T19.6:台账整结构体回读自动流过
	}, true
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
			// 阻塞清单引导运维/自动化按行退避(D-t92 决策 4②)。T19.6 起
			// 收敛出路 = 重试携 quiesce=true(detail 同提示,协议自描述)。
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
// (对抗审计 HIGH 修:此前仅 role_denied 落行)。quiesce = 请求旗标镜像
// (T19.6:破坏性 opt-in 的拒绝面证据)。执行缺行时 Append 的分区键
// 点查失败 = 天然防灌水,镜像 decided 面的锚定语义。
func (s *Server) writeRollbackDeniedAudit(c *gin.Context, execID uint64, nodeID, actor, reason string, blockedCount int, quiesce bool) {
	// T18.5 ⑦:root X 池上点查(镜像 writeDecisionDeniedAudit)——denied
	// 行自身 trace = 拒绝请求段链 Z,linked 指回执行主链;best-effort 面
	// 瞬态故障跳过但零遥测不可接受(同审批先例)。
	var rootTrace sql.NullString
	if err := s.dbh.QueryRowContext(c.Request.Context(),
		`SELECT trace_id FROM workflow_execution WHERE id=?`, execID).
		Scan(&rootTrace); err != nil {
		slog.Error("api: anchor rollback.denied trace", "err", err, "execution_id", execID)
		return
	}
	id, err := s.ids.Next()
	if err != nil {
		slog.Error("api: gen rollback.denied id", "err", err)
		return
	}
	raw, err := json.Marshal(audit.RollbackDeniedPayload{
		Reason: reason, TargetNodeID: nodeID, BlockedCount: blockedCount,
		Quiesce: quiesce,
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
			NodeID: nodeID, ActorType: audit.ActorUser, ActorID: actor,
			LinkedTraceID: rootTrace.String, Payload: raw,
		})
	}); err != nil {
		slog.Error("api: anchor rollback.denied", "err", err, "execution_id", execID)
	}
}
