package api

// T18.4 投影只读端点(D-2026-10-05-t184-projection-read-endpoints):
//   - GET /executions/:id/nodes/:node_id(?instance_no=,缺省 0=控制行)
//     —— 按路径:node_execution 当前投影(output = 最新成功轮投影 +
//     input_snapshot + attempts/attempts_base 轮区间锚);
//   - GET /executions/:id/nodes/:node_id/attempts —— 按轮:全轮次台账
//     瘦列列表(round ≡ attempt_no 升序,与 decisionDTO 词表同轴——
//     D-t92 决策 6「跨轮全时间线」的 ListDecisions 同款读面设计);
//   - GET /executions/:id/nodes/:node_id/attempts/:round —— 按轮:单轮
//     证据四件套(input/output/error/usage,fat 列)。
// 纯读面:路由级 RequireScope(executions:read) 零角色闸、零审计写
// (auditquery 先例;rollback.denied 的 in-handler 闸是写面业务拒绝专用,
// 不搬进读端点)。租户闸 = GetExecution(tenant,id) 前置锚(node 系无
// 租户列,execstore 钦定;跨租户与不存在同面 404)。缺失语义:节点行
// 不存在 = 404;台账空/被 janitor 清走(D-t186 保留期)= 列表 200 变薄、
// 单轮 404(成因同面:未物化/已清理/越界不区分)——「半边消失不报错」。

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// nodeProjectionDTO:按路径响应。output nil = null(无成功轮/未终态,
// 由 status 消歧——D-t92 决策 9 投影语义);input_snapshot nil = null
// (PENDING/SKIPPED 恒 NULL,engine 不变量,缺席不伪造)。JSON 列是
// MySQL 规范形(键序/去空白),不承诺与求值原文字节相等。
type nodeProjectionDTO struct {
	ExecutionID     string          `json:"execution_id"`
	NodeExecutionID string          `json:"node_execution_id"`
	NodeID          string          `json:"node_id"`
	InstanceNo      uint16          `json:"instance_no"` // 形状稳定,非 omitempty
	Type            string          `json:"type"`
	Status          string          `json:"status"`
	Attempts        uint16          `json:"attempts"`      // 恒单调不清零(D-t92 决策 1)
	AttemptsBase    uint16          `json:"attempts_base"` // 回滚基线:旧轮 = [1, base]
	MaxAttempts     uint16          `json:"max_attempts"`
	InputSnapshot   json.RawMessage `json:"input_snapshot"`
	Output          json.RawMessage `json:"output"`
	ErrorCode       *string         `json:"error_code"`
	ErrorMessage    *string         `json:"error_message"`
	StartedAt       *time.Time      `json:"started_at"`
	FinishedAt      *time.Time      `json:"finished_at"`
}

// nodeAttemptSummaryDTO:按轮列表瘦行(零 JSON 大列,镜像 execListCols
// 纪律——大列只进单轮详情)。round ≡ attempt_no(approval_decision.
// round 同一计数器;round 0 是 0007 前存量哨兵语义,attempt_no
// CHECK >= 1,不映射)。attempt_id 字符串化(snowflake 2^53 契约)。
type nodeAttemptSummaryDTO struct {
	AttemptID        string     `json:"attempt_id"`
	Round            uint16     `json:"round"`
	Status           string     `json:"status"`
	WorkerID         *string    `json:"worker_id"`
	Model            *string    `json:"model"`
	PromptTokens     *uint32    `json:"prompt_tokens"`
	CompletionTokens *uint32    `json:"completion_tokens"`
	TotalTokens      *uint32    `json:"total_tokens"`
	CostAmount       *float64   `json:"cost_amount"` // DECIMAL(14,6) 全值域 < 2^53,数值保真
	CostCurrency     *string    `json:"cost_currency"`
	Estimated        bool       `json:"estimated"`
	ErrorCode        *string    `json:"error_code"`
	ErrorMessage     *string    `json:"error_message"`
	StartedAt        time.Time  `json:"started_at"`
	FinishedAt       *time.Time `json:"finished_at"` // DEAD 恒 null
}

// nodeAttemptDTO:单轮详情 = 列表瘦行 + 证据四件套 fat 列(嵌入平铺)。
type nodeAttemptDTO struct {
	nodeAttemptSummaryDTO
	InputSnapshot json.RawMessage `json:"input_snapshot"` // NULL = pre-T9.2 存量/派发链外
	Output        json.RawMessage `json:"output"`         // FAILED/DEAD 恒 null(未产出,由 status 消歧)
}

// parseInstanceNoQuery 解析 ?instance_no=(缺省 0=控制行;0006 uk 第三列,
// SMALLINT UNSIGNED 上界 65535)。形状先于租户闸(纯语法面,零存在性
// 泄露,rollback 端点 node_id 形状闸同判)。
func parseInstanceNoQuery(c *gin.Context) (uint16, bool) {
	raw := c.Query("instance_no")
	if raw == "" {
		return 0, true
	}
	v, err := strconv.ParseUint(raw, 10, 16)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_instance_no"})
		return 0, false
	}
	return uint16(v), true
}

// parseRoundParam 解析 :round 路径段([1, 65535];0 拒绝——attempt_no
// CHECK >= 1,且 approval round=0 是存量哨兵语义不得映射 attempt)。
func parseRoundParam(c *gin.Context) (uint16, bool) {
	v, err := strconv.ParseUint(c.Param("round"), 10, 16)
	if err != nil || v < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_round"})
		return 0, false
	}
	return uint16(v), true
}

// resolveProjectionNode 三端点共用前置:node_id 形状闸 → instance_no
// 形状闸 → 租户闸(GetExecution 前置锚)→ 节点行解析。thin=true 走
// GetNodeExecutionID 锚解析(按轮端点,零 fat 列);false 走
// GetNodeProjection(按路径端点,投影列进读)。
func (s *Server) resolveProjectionNode(c *gin.Context, thin bool) (execID, nodeExecID uint64, np *NodeProjection, ok bool) {
	execID, ok = parseIDParam(c)
	if !ok {
		return 0, 0, nil, false
	}
	nodeID := c.Param("node_id")
	if !nodeIDRe.MatchString(nodeID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_node_id"})
		return 0, 0, nil, false
	}
	instanceNo, ok := parseInstanceNoQuery(c)
	if !ok {
		return 0, 0, nil, false
	}
	if _, err := s.st.GetExecution(c.Request.Context(), s.dbh, tenantFrom(c), execID); err != nil {
		mapStoreError(c, err)
		return 0, 0, nil, false
	}
	if thin {
		id, err := s.st.GetNodeExecutionID(c.Request.Context(), s.dbh, execID, nodeID, instanceNo)
		if err != nil {
			mapStoreError(c, err)
			return 0, 0, nil, false
		}
		return execID, id, nil, true
	}
	p, err := s.st.GetNodeProjection(c.Request.Context(), s.dbh, execID, nodeID, instanceNo)
	if err != nil {
		mapStoreError(c, err)
		return 0, 0, nil, false
	}
	return execID, p.ID, &p, true
}

// getNodeProjection:GET /executions/:id/nodes/:node_id —— 按路径投影。
func (s *Server) getNodeProjection(c *gin.Context) {
	execID, _, np, ok := s.resolveProjectionNode(c, false)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, nodeProjectionDTO{
		ExecutionID:     strconv.FormatUint(execID, 10),
		NodeExecutionID: strconv.FormatUint(np.ID, 10),
		NodeID:          np.NodeID,
		InstanceNo:      np.InstanceNo,
		Type:            np.Type,
		Status:          np.Status,
		Attempts:        np.Attempts,
		AttemptsBase:    np.AttemptsBase,
		MaxAttempts:     np.MaxAttempts,
		InputSnapshot:   json.RawMessage(np.InputSnapshot),
		Output:          json.RawMessage(np.Output),
		ErrorCode:       np.ErrorCode,
		ErrorMessage:    np.ErrorMessage,
		StartedAt:       np.StartedAt,
		FinishedAt:      np.FinishedAt,
	})
}

// listNodeAttempts:GET /executions/:id/nodes/:node_id/attempts —— 按轮
// 瘦列列表(items 恒非缺席:零台账 = 空数组,不报错不伪造)。
func (s *Server) listNodeAttempts(c *gin.Context) {
	_, nodeExecID, _, ok := s.resolveProjectionNode(c, true)
	if !ok {
		return
	}
	attempts, err := s.st.ListNodeAttempts(c.Request.Context(), s.dbh, nodeExecID)
	if err != nil {
		mapStoreError(c, err)
		return
	}
	items := make([]nodeAttemptSummaryDTO, 0, len(attempts))
	for _, a := range attempts {
		items = append(items, toNodeAttemptSummaryDTO(a))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// getNodeAttempt:GET /executions/:id/nodes/:node_id/attempts/:round ——
// 单轮四件套。缺行 404(未物化/已清理/越界同面,不区分成因)。
func (s *Server) getNodeAttempt(c *gin.Context) {
	round, ok := parseRoundParam(c)
	if !ok {
		return
	}
	_, nodeExecID, _, ok := s.resolveProjectionNode(c, true)
	if !ok {
		return
	}
	a, err := s.st.GetNodeAttempt(c.Request.Context(), s.dbh, nodeExecID, round)
	if err != nil {
		mapStoreError(c, err)
		return
	}
	dto := nodeAttemptDTO{
		nodeAttemptSummaryDTO: toNodeAttemptSummaryDTO(a),
		InputSnapshot:         json.RawMessage(a.InputSnapshot),
		Output:                json.RawMessage(a.Output),
	}
	c.JSON(http.StatusOK, dto)
}

func toNodeAttemptSummaryDTO(a NodeAttempt) nodeAttemptSummaryDTO {
	return nodeAttemptSummaryDTO{
		AttemptID:        strconv.FormatUint(a.ID, 10),
		Round:            a.AttemptNo,
		Status:           a.Status,
		WorkerID:         a.WorkerID,
		Model:            a.Model,
		PromptTokens:     a.PromptTokens,
		CompletionTokens: a.CompletionTokens,
		TotalTokens:      a.TotalTokens,
		CostAmount:       a.CostAmount,
		CostCurrency:     a.CostCurrency,
		Estimated:        a.Estimated,
		ErrorCode:        a.ErrorCode,
		ErrorMessage:     a.ErrorMessage,
		StartedAt:        a.StartedAt,
		FinishedAt:       a.FinishedAt,
	}
}
