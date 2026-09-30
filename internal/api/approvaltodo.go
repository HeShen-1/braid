// 待办列表(T4.4):GET /api/v1/approvals/pending —— 主体的待审批节点
// 瘦列表。DB 缝 = idx_node_status_deadline 前缀扫(status='WAITING_APPROVAL',
// ORDER BY deadline_at 即索引序,零 filesort)+ 快照名单过滤:真相源 =
// input_snapshot.approvers(issue #43 硬约束:勿造第二判据),JSON_CONTAINS
// 精确成员判定(carol ≠ caroline,LIKE 语义在此是越权面)。执行侧过滤
// 非终态(RUNNING | WAITING_APPROVAL,镜像 POST 决定的终态守卫:终态执行
// 上的迟到决定不产生新语义,不得以待办形态引诱点击;孤儿行的最终裁决
// 归 reaper 扫描⑤b)。
//
// 读面无事务(钉版契约同 GET /executions/:id:并发迁移间的轻微不一致
// P0 可接受);行视图是瘦字段集——快照全文(instructions 等)走既有
// GET /executions/:id/approvals/:node_id 详情端点,列表不放大 JSON 列。
package api

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"braid/internal/engine"
)

// PendingApproval 是待办列表行模型(node_execution × workflow_execution
// 瘦投影)。
type PendingApproval struct {
	ExecutionID       uint64
	NodeID            string
	DeadlineAt        time.Time
	DefinitionName    string
	DefinitionVersion uint32
	ExecutionStatus   string
}

// pendingWhere 是待办列表的公共谓词(计数与取页同一拼法,单源防两处漂移):
// 节点 WAITING_APPROVAL + 执行非终态 + 快照名单精确成员 + 执行租户归属
// (D-t112:跨租户 DSL 命名他租户主体的面在此闭合——主体名单命中且执行
// 属读者租户才可见;node 系无租户列,经 we.tenant_id 谓词)。
const pendingWhere = ` WHERE ne.status = ? AND we.status IN (?, ?)
  AND JSON_CONTAINS(ne.input_snapshot, JSON_QUOTE(?), '$.approvers')
  AND we.tenant_id = ?`

// ListPendingApprovals 分页列出主体的待办审批(任意有效 token,主体
// 过滤即边界;非审批人恒空集)。顺序 deadline_at, id 升序(最紧急优先;
// id tiebreak 同 created_at,id 先例——snowflake 时序一致)。返回(当前
// 页, 满足条件的总行数),与 ListExecutions 同形。
func (s *Store) ListPendingApprovals(ctx context.Context, q DBTX, tenant uint64, username string, limit, offset int) ([]PendingApproval, int, error) {
	args := []any{string(engine.NodeWaitingApproval), string(engine.WorkflowRunning),
		string(engine.WorkflowWaitingApproval), username, tenant}
	var total int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM node_execution ne JOIN workflow_execution we ON we.id = ne.execution_id`+pendingWhere,
		args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("api: count pending approvals of %q: %w", username, err)
	}
	rows, err := q.QueryContext(ctx, `
SELECT ne.execution_id, ne.node_id, ne.deadline_at, we.definition_name, we.definition_version, we.status
  FROM node_execution ne JOIN workflow_execution we ON we.id = ne.execution_id`+pendingWhere+`
 ORDER BY ne.deadline_at, ne.id LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("api: list pending approvals of %q: %w", username, err)
	}
	defer rows.Close() //nolint:errcheck // 只读路径,关闭错误无处置义
	var out []PendingApproval
	for rows.Next() {
		var (
			p        PendingApproval
			deadline sql.NullTime
		)
		if err := rows.Scan(&p.ExecutionID, &p.NodeID, &deadline,
			&p.DefinitionName, &p.DefinitionVersion, &p.ExecutionStatus); err != nil {
			return nil, 0, fmt.Errorf("api: scan pending approval: %w", err)
		}
		// WAITING ⇒ deadline NOT NULL 是 engine 不变量(T4.1 挂起必置);
		// 违反即数据异常,响亮失败不静默吞。
		if !deadline.Valid {
			return nil, 0, fmt.Errorf("api: pending approval exec %d node %q has NULL deadline (engine invariant violated)",
				p.ExecutionID, p.NodeID)
		}
		p.DeadlineAt = deadline.Time
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("api: iterate pending approvals: %w", err)
	}
	return out, total, nil
}

// pendingApprovalDTO 是待办行视图(execution_id 字符串化契约同
// definitionDTO,2^53 精度)。
type pendingApprovalDTO struct {
	ExecutionID       string    `json:"execution_id"`
	NodeID            string    `json:"node_id"`
	DefinitionName    string    `json:"definition_name"`
	DefinitionVersion uint32    `json:"definition_version"`
	ExecutionStatus   string    `json:"execution_status"`
	DeadlineAt        time.Time `json:"deadline_at"`
}

func toPendingDTO(p PendingApproval) pendingApprovalDTO {
	return pendingApprovalDTO{
		ExecutionID:       strconv.FormatUint(p.ExecutionID, 10),
		NodeID:            p.NodeID,
		DefinitionName:    p.DefinitionName,
		DefinitionVersion: p.DefinitionVersion,
		ExecutionStatus:   p.ExecutionStatus,
		DeadlineAt:        p.DeadlineAt,
	}
}

// listPendingApprovals:GET /api/v1/approvals/pending?limit&offset。任意
// 有效 token(名单内过滤即读边界,与 GET 详情收窄面同真相源)。
func (s *Server) listPendingApprovals(c *gin.Context) {
	limit, offset, ok := parsePageParams(c)
	if !ok {
		return
	}
	items, total, err := s.st.ListPendingApprovals(c.Request.Context(), s.dbh, tenantFrom(c), actorFrom(c), limit, offset)
	if err != nil {
		slog.Error("api: pending approvals query", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		return
	}
	out := make([]pendingApprovalDTO, 0, len(items))
	for _, p := range items {
		out = append(out, toPendingDTO(p))
	}
	c.JSON(http.StatusOK, gin.H{
		"items": out,
		"page":  gin.H{"limit": limit, "offset": offset, "total": total},
	})
}
