// audit_log 查询原语(T6.3):审计读面,纯 SELECT 零第二物化(execstore
// 先例)。可见性口径 = 仅读 audit_log(D-2026-09-25-t63-audit-query-api #1):
// 高频四类滞后上界 = AuditFlushInterval(默认 1s,D-t61 #6),audit_pending
// WAL 暂存面不进查询。读路径无事务,连接由调用方持有(与 execstore 同形)。
package api

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// AuditEvent 是 audit_log 一行的查询面模型。prev_hash / row_hash P1 启链时
// 再进;linked_trace_id 不在查询面(无索引,T6.2 接线时定——D-t63 #4)。
type AuditEvent struct {
	ID          uint64
	ExecDate    string
	EventAt     time.Time
	CreatedAt   time.Time // 落库时刻:异步面滞后观测(D-t61 #7 语义)
	EventType   string
	ExecutionID uint64
	NodeExecID  *uint64
	NodeID      *string
	AttemptID   *uint64
	ActorType   string
	ActorID     string
	TraceID     *string
	PIIMasked   bool
	// usage/cost 冗余列组(llm.call.completed|discarded 双词;与 node_attempt 双录)。
	Model            *string
	PromptTokens     *uint32
	CompletionTokens *uint32
	TotalTokens      *uint32
	CostAmount       *float64
	CostCurrency     *string
	CostEstimated    bool
	Payload          []byte
}

// AuditQuery 三维度互斥恰一(D-t63 #3,handler 侧已校验):ExecutionID>0
// 时 ExecDate 必填(GetExecution 钉分区 → 单分区裁剪 + idx_audit_exec);
// TraceID / ActorID 走分区本地索引逐分区探测(0001 DDL 钦定取舍,保留期
// 有界;延迟超标演化路径 = audit_trace_map 旁路表,v1 不建)。From/To 是
// event_at 闭区间(事件时刻,≠ 落库时刻 created_at)。
type AuditQuery struct {
	ExecutionID uint64
	ExecDate    string
	TraceID     string
	ActorID     string
	From, To    *time.Time
}

// auditCols 全量投影(剔除 prev_hash/row_hash/linked_trace_id,见模型注释)。
// exec_date 由 DATE_FORMAT 直出字符串(与 execCols 同式单源):DATE 列直扫
// *string 在 parseTime DSN 下经 driver 变 time.Time → RFC3339Nano,与
// GET /executions/:id 审计指针(executionAuditPointer)的 "YYYY-MM-DD"
// 线格式漂移;字符串往返日期语义完全留在 DB 端(writer.go 同判)。
const auditCols = `id, DATE_FORMAT(exec_date, '%Y-%m-%d'), event_at, created_at,
 event_type, execution_id, node_execution_id, node_id, attempt_id, actor_type,
 actor_id, trace_id, pii_masked, model, prompt_tokens, completion_tokens,
 total_tokens, cost_amount, cost_currency, cost_estimated, payload`

// auditPredicate 单源构造 WHERE:维度三选一 + event_at 闭区间。COUNT 与页行
// 同谓词(与 ListExecutions 同形),防两处漂移。
func auditPredicate(aq AuditQuery) (string, []any) {
	conds := make([]string, 0, 4)
	args := make([]any, 0, 4)
	switch {
	case aq.ExecutionID != 0:
		// exec_date 恒为该执行分区(一执行的全部审计行同分区,0001 DDL):
		// 分区裁剪 + idx_audit_exec(InnoDB 隐式拼 PK,物理序含 id)。
		conds = append(conds, "exec_date = ?", "execution_id = ?")
		args = append(args, aq.ExecDate, aq.ExecutionID)
	case aq.TraceID != "":
		conds = append(conds, "trace_id = ?")
		args = append(args, aq.TraceID)
	default:
		// actor 等值显式钉二进制排序规则(对抗审计 sql MEDIUM):actor_id
		// 列继承表默认 utf8mb4_0900_ai_ci,大小写折叠会把互异主体串并进
		// 同一结果(llm 事件 actor = 模型名,外部字符串,'GPT-4o'/'gpt-4o'
		// 是不同主体)。EXPLAIN 实证:ref→range 退化但仍走 idx_audit_actor;
		// 列级显式 collation 属迁移面(铁律),先在谓词侧钉死(对照先例:
		// idempotency_key 列 DDL 显式 utf8mb4_bin 防大小写折叠误判同键)。
		conds = append(conds, "actor_id = ? COLLATE utf8mb4_bin")
		args = append(args, aq.ActorID)
	}
	if aq.From != nil {
		conds = append(conds, "event_at >= ?")
		args = append(args, *aq.From)
	}
	if aq.To != nil {
		conds = append(conds, "event_at <= ?")
		args = append(args, *aq.To)
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListAuditEvents 按维度过滤列审计行,返回 (当前页, 满足条件的总行数),
// 顺序 event_at DESC, id DESC(最新在前,snowflake tie-break;与既有列表
// 面同向)。LIMIT/OFFSET 上界由 handler 侧 parsePageParams 钉死。
func (s *Store) ListAuditEvents(ctx context.Context, q DBTX, aq AuditQuery, limit, offset int) ([]AuditEvent, int, error) {
	where, args := auditPredicate(aq)
	var total int
	if err := q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM audit_log"+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("api: count audit events: %w", err)
	}
	rows, err := q.QueryContext(ctx,
		"SELECT "+auditCols+" FROM audit_log"+where+
			" ORDER BY event_at DESC, id DESC LIMIT ? OFFSET ?",
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("api: list audit events: %w", err)
	}
	defer rows.Close() //nolint:errcheck // 只读路径,关闭错误无处置义
	var out []AuditEvent
	for rows.Next() {
		e, err := scanAuditEvent(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("api: scan audit event: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("api: iterate audit events: %w", err)
	}
	return out, total, nil
}

func scanAuditEvent(r rowScanner) (AuditEvent, error) {
	var (
		e        AuditEvent
		nodeExec sql.NullInt64
		nodeID   sql.NullString
		attempt  sql.NullInt64
		traceID  sql.NullString
		model    sql.NullString
		prompt   sql.NullInt64
		compl    sql.NullInt64
		totalTok sql.NullInt64
		costAmt  sql.NullFloat64
		costCur  sql.NullString
		payload  []byte
	)
	if err := r.Scan(&e.ID, &e.ExecDate, &e.EventAt, &e.CreatedAt, &e.EventType,
		&e.ExecutionID, &nodeExec, &nodeID, &attempt, &e.ActorType, &e.ActorID,
		&traceID, &e.PIIMasked, &model, &prompt, &compl, &totalTok,
		&costAmt, &costCur, &e.CostEstimated, &payload); err != nil {
		return AuditEvent{}, err
	}
	e.NodeExecID = nullInt64ToUint64(nodeExec)
	e.NodeID = nullStringPtr(nodeID)
	e.AttemptID = nullInt64ToUint64(attempt)
	e.TraceID = nullStringPtr(traceID)
	e.Model = nullStringPtr(model)
	if prompt.Valid {
		v := uint32(prompt.Int64) // INT UNSIGNED ≤ 2^32-1,int64 无损承载
		e.PromptTokens = &v
	}
	if compl.Valid {
		v := uint32(compl.Int64)
		e.CompletionTokens = &v
	}
	if totalTok.Valid {
		v := uint32(totalTok.Int64)
		e.TotalTokens = &v
	}
	if costAmt.Valid {
		v := costAmt.Float64
		e.CostAmount = &v
	}
	e.CostCurrency = nullStringPtr(costCur)
	e.Payload = payload
	return e, nil
}

func nullInt64ToUint64(v sql.NullInt64) *uint64 {
	if !v.Valid {
		return nil
	}
	u := uint64(v.Int64)
	return &u
}

func nullStringPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}
