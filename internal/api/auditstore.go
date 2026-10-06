// audit_log 查询原语(T6.3):审计读面,纯 SELECT 零第二物化(execstore
// 先例)。可见性口径 = 仅读 audit_log(D-2026-09-25-t63-audit-query-api #1):
// 高频四类滞后上界 = AuditFlushInterval(默认 1s,D-t61 #6),audit_pending
// WAL 暂存面不进查询。读路径无事务,连接由调用方持有(与 execstore 同形)。
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"braid/internal/audit"
)

// AuditEvent 是 audit_log 一行的查询面模型。prev_hash / row_hash 已随
// T9.3 启链进查询面(D-t63 #4「启链时加」兑现;RowHash nil = 存量未启链
// 行);linked_trace_id 仍不在查询面(无索引,T6.2 接线时定——D-t63 #4)。
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
	PrevHash    string  // T9.3:前驱行 row_hash;'' = 链首(或存量未启链行)
	RowHash     *string // T9.3:本行规范化哈希;nil = 存量未启链行(校验跳过)
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
// event_at 闭区间(事件时刻,≠ 落库时刻 created_at)。TenantID 是读者
// 归属租户(D-t112 #4):恒进谓词(残差过滤,不建新索引)——模型名
// actor 的 llm 查询无法经 user 行判租,行上 tenant_id 是唯一判据。
type AuditQuery struct {
	ExecutionID uint64
	ExecDate    string
	TraceID     string
	ActorID     string
	TenantID    uint64
	From, To    *time.Time
}

// auditCols 全量投影(剔除 linked_trace_id,见模型注释;prev_hash/row_hash
// 已随 T9.3 启链进投影——D-t63 #4)。
// exec_date 由 DATE_FORMAT 直出字符串(与 execCols 同式单源):DATE 列直扫
// *string 在 parseTime DSN 下经 driver 变 time.Time → RFC3339Nano,与
// GET /executions/:id 审计指针(executionAuditPointer)的 "YYYY-MM-DD"
// 线格式漂移;字符串往返日期语义完全留在 DB 端(writer.go 同判)。
const auditCols = `id, DATE_FORMAT(exec_date, '%Y-%m-%d'), event_at, created_at,
 event_type, execution_id, node_execution_id, node_id, attempt_id, actor_type,
 actor_id, trace_id, pii_masked, prev_hash, row_hash,
 model, prompt_tokens, completion_tokens,
 total_tokens, cost_amount, cost_currency, cost_estimated, payload`

// auditPredicate 单源构造 WHERE:租户恒携(行级隔离执法点,D-t112)+
// 维度三选一 + event_at 闭区间。COUNT 与页行同谓词(与 ListExecutions
// 同形),防两处漂移。租户谓词是残差过滤(维度索引探测后逐行比对,
// 不建新索引):trace/actor 维度行集小(单 trace/单主体的执行事件),
// execution 维度已被 GetExecution 租户闸先行裁决(谓词 = 纵深冗余)。
func auditPredicate(aq AuditQuery) (string, []any) {
	conds := make([]string, 0, 5)
	args := make([]any, 0, 5)
	conds = append(conds, "tenant_id = ?")
	args = append(args, aq.TenantID)
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

// AuditCursor keyset 游标(T12.2,D-t63 #5 预登记收敛路径):值 = 展示序
// (event_at DESC, id DESC)上一页最后一行的 (event_at, id),语义 = 全序中
// 的排他下界。与 D-t93 哈希链序(prev_hash 链接序)无关——分页序是稳定
// 展示序,不是因果/链序(id 序≠链序,D-t93 #3);游标不依赖任何 hash 列。
type AuditCursor struct {
	EventAt time.Time
	ID      uint64
}

// ListAuditEvents 按维度过滤列审计行,keyset 语义:cursor 非空时只返回
// (event_at,id) 全序中严格位于 cursor 之下的行;顺序 event_at DESC,
// id DESC。尾页判定 = 取 limit+1 行(多出即有后续,裁回 limit 并回传下一
// 页游标;nil = 无更多行)。响应不携 total:COUNT 与页行同事务的一致快照
// 诉求随 OFFSET 契约一并退役(D-t63 #5——total 低报早停/跨页漂移两个
// 已知边界都由游标语义闭合),读路径回到 execstore「无事务」先例。
func (s *Store) ListAuditEvents(ctx context.Context, q DBTX, aq AuditQuery, cursor *AuditCursor, limit int) ([]AuditEvent, *AuditCursor, error) {
	where, args := auditPredicate(aq)
	if cursor != nil {
		// DESC 全序排他下界:同行刻毫秒按 id 递降续走(id 全局唯一,元组
		// 严格全序;访问路径 = 维度索引 + 残差过滤,深页严格优于 OFFSET)。
		where += " AND (event_at < ? OR (event_at = ? AND id < ?))"
		args = append(args, cursor.EventAt, cursor.EventAt, cursor.ID)
	}
	rows, err := q.QueryContext(ctx,
		"SELECT "+auditCols+" FROM audit_log"+where+
			" ORDER BY event_at DESC, id DESC LIMIT ?",
		append(args, limit+1)...)
	if err != nil {
		return nil, nil, fmt.Errorf("api: list audit events: %w", err)
	}
	defer rows.Close() //nolint:errcheck // 只读路径,关闭错误无处置义
	var out []AuditEvent
	for rows.Next() {
		e, err := scanAuditEvent(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("api: scan audit event: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("api: iterate audit events: %w", err)
	}
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		return out, &AuditCursor{EventAt: last.EventAt, ID: last.ID}, nil
	}
	return out, nil, nil
}

// LatestRolledBackPayload 读回执行最新一条 execution.rolled_back 的
// payload(T18.5 重放重建源):rolled_back 是同步直写词,首答提交即可见。
// 访问路径与 auditPredicate 执行维度同式——exec_date 钉分区裁剪 +
// idx_audit_exec + event_type 残差过滤,LIMIT 1 免翻页;found=false =
// 无台账,调用方按原拒绝语义直通。
func (s *Store) LatestRolledBackPayload(ctx context.Context, q DBTX, tenantID uint64,
	execID uint64, execDate string) (audit.ExecutionRolledBackPayload, bool, error) {
	var raw []byte
	if err := q.QueryRowContext(ctx,
		`SELECT payload FROM audit_log
		 WHERE tenant_id=? AND exec_date=? AND execution_id=?
		   AND event_type='execution.rolled_back'
		 ORDER BY event_at DESC, id DESC LIMIT 1`,
		tenantID, execDate, execID).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return audit.ExecutionRolledBackPayload{}, false, nil
		}
		return audit.ExecutionRolledBackPayload{}, false, fmt.Errorf("api: latest rolled_back: %w", err)
	}
	var p audit.ExecutionRolledBackPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return audit.ExecutionRolledBackPayload{}, false, fmt.Errorf("api: unmarshal rolled_back payload: %w", err)
	}
	return p, true, nil
}

func scanAuditEvent(r rowScanner) (AuditEvent, error) {
	var (
		e        AuditEvent
		nodeExec sql.NullInt64
		nodeID   sql.NullString
		attempt  sql.NullInt64
		traceID  sql.NullString
		rowHash  sql.NullString
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
		&traceID, &e.PIIMasked, &e.PrevHash, &rowHash,
		&model, &prompt, &compl, &totalTok,
		&costAmt, &costCur, &e.CostEstimated, &payload); err != nil {
		return AuditEvent{}, err
	}
	e.NodeExecID = nullInt64ToUint64(nodeExec)
	e.NodeID = nullStringPtr(nodeID)
	e.AttemptID = nullInt64ToUint64(attempt)
	e.TraceID = nullStringPtr(traceID)
	e.RowHash = nullStringPtr(rowHash)
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
