// execution 查询原语(T5.5):workflow_execution / node_execution 的读面。
// 读路径无事务(钉版契约下行集在物化时一次建立;并发 CAS 迁移间的两次
// 读轻微不一致是 P0 可接受的读语义),连接由调用方持有(与 store.go 同形)。
package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Execution 是 workflow_execution 一行的查询面模型。
type Execution struct {
	ID                uint64
	DefinitionID      uint64
	DefinitionName    string
	DefinitionVersion uint32
	DefinitionHash    string // 钉版四件套第四件:与 definition.dsl_hash 比对即知漂移(DDL :71,T8.1 对账证据)
	Status            string
	TriggerType       string
	TriggeredBy       string
	TraceID           *string // T6.2 前恒 NULL(不伪造)
	Input             []byte
	Outputs           []byte // nil = NULL(未终态)
	ErrorCode         *string
	ErrorMessage      *string
	CreatedAt         time.Time
	StartedAt         *time.Time
	FinishedAt        *time.Time
	// ExecDate 是审计行分区定位键:DATE_FORMAT(created_at,'%Y-%m-%d') 在
	// DB 端单源计算,与 audit writer 物化公式逐字同式(writer.go 注释:
	// 字符串往返不经 driver loc 解析,日期语义完全留在 DB 端)。
	ExecDate string
}

// NodeExecution 是 node_execution 一行的查询面模型。
type NodeExecution struct {
	ID           uint64
	NodeID       string
	Type         string
	Status       string
	Attempts     uint16
	MaxAttempts  uint16
	ErrorCode    *string
	ErrorMessage *string
	StartedAt    *time.Time
	FinishedAt   *time.Time
}

// execCols 是 detail 路径全量列;exec_date 由 DATE_FORMAT 直出字符串(单源,
// 见 Execution.ExecDate)。
const execCols = `id, definition_id, definition_name, definition_version, definition_hash,
 status, trigger_type, triggered_by, trace_id, input, outputs, error_code, error_message,
 created_at, started_at, finished_at,
 DATE_FORMAT(created_at, '%Y-%m-%d')`

// execListCols 是列表路径的瘦列集:剔除 input/outputs 大列——响应侧 DTO
// 已不带(1MiB×limit 100 放大面),DB 读侧同排,否则投影修复只挡住 HTTP
// 面挡不住进程内存(对抗审计 MEDIUM)。definition_hash 仅 detail 携带。
const execListCols = `id, definition_id, definition_name, definition_version, status,
 trigger_type, triggered_by, trace_id, error_code, error_message,
 created_at, started_at, finished_at,
 DATE_FORMAT(created_at, '%Y-%m-%d')`

// GetExecution 按 id 读执行行(任意状态;404 判定)。
func (s *Store) GetExecution(ctx context.Context, q DBTX, id uint64) (Execution, error) {
	row := q.QueryRowContext(ctx,
		"SELECT "+execCols+" FROM workflow_execution WHERE id = ?", id)
	exec, err := scanExecution(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Execution{}, fmt.Errorf("%w: execution %d", ErrNotFound, id)
	}
	return exec, err
}

// ListExecutions 分页列出执行(name 空 = 全部工作流,给定时走
// idx_exec_name_created——DDL 钦定的「按工作流列执行」;status 空 = 全部
// 状态)。顺序 created_at DESC, id DESC(最新在前,snowflake id 时序一致)。
// 已知取舍:name 缺省(全量/status-only)路径 created_at 无前导索引 ⇒
// 全表扫 + filesort——本表是全系统无上界增长表,limit≤100/offset≤10000
// 硬上界下 P0 量级可接受,劣化阈值归 T6.4 告警,补 (status,created_at)
// 索引归后续迁移任务(对抗审计 LOW,钉住防静默)。
// 返回 (当前页, 满足条件的总行数),与 List(definitions)同形。
func (s *Store) ListExecutions(ctx context.Context, q DBTX, name, status string, limit, offset int) ([]Execution, int, error) {
	where := ""
	args := make([]any, 0, 4)
	if name != "" {
		where = " WHERE definition_name = ?"
		args = append(args, name)
	}
	if status != "" {
		if where == "" {
			where = " WHERE status = ?"
		} else {
			where += " AND status = ?"
		}
		args = append(args, status)
	}
	var total int
	if err := q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM workflow_execution"+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("api: count executions: %w", err)
	}
	rows, err := q.QueryContext(ctx,
		"SELECT "+execListCols+" FROM workflow_execution"+where+
			" ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?",
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("api: list executions: %w", err)
	}
	defer rows.Close() //nolint:errcheck // 只读路径,关闭错误无处置义
	var out []Execution
	for rows.Next() {
		e, err := scanExecutionList(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("api: scan execution: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("api: iterate executions: %w", err)
	}
	return out, total, nil
}

// ListNodeExecutions 列某执行的全部节点行(node_id 字典序 = uk_node_exec
// 索引序,DDL 钦定的确定性排序,与到达/重试顺序无关)。
func (s *Store) ListNodeExecutions(ctx context.Context, q DBTX, execID uint64) ([]NodeExecution, error) {
	rows, err := q.QueryContext(ctx, `
SELECT id, node_id, type, status, attempts, max_attempts, error_code, error_message,
       started_at, finished_at
FROM node_execution WHERE execution_id = ? ORDER BY node_id`, execID)
	if err != nil {
		return nil, fmt.Errorf("api: list node executions of %d: %w", execID, err)
	}
	defer rows.Close() //nolint:errcheck // 只读路径,关闭错误无处置义
	var out []NodeExecution
	for rows.Next() {
		var n NodeExecution
		if err := rows.Scan(&n.ID, &n.NodeID, &n.Type, &n.Status, &n.Attempts, &n.MaxAttempts,
			&n.ErrorCode, &n.ErrorMessage, &n.StartedAt, &n.FinishedAt); err != nil {
			return nil, fmt.Errorf("api: scan node execution: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("api: iterate node executions: %w", err)
	}
	return out, nil
}

func scanExecution(r rowScanner) (Execution, error) {
	var (
		e          Execution
		traceID    sql.NullString
		input      []byte
		outputs    []byte
		errorCode  sql.NullString
		errorMsg   sql.NullString
		createdAt  sql.NullTime
		startedAt  sql.NullTime
		finishedAt sql.NullTime
	)
	if err := r.Scan(&e.ID, &e.DefinitionID, &e.DefinitionName, &e.DefinitionVersion,
		&e.DefinitionHash, &e.Status, &e.TriggerType, &e.TriggeredBy, &traceID,
		&input, &outputs, &errorCode, &errorMsg, &createdAt, &startedAt, &finishedAt,
		&e.ExecDate); err != nil {
		return Execution{}, err
	}
	if traceID.Valid {
		t := traceID.String
		e.TraceID = &t
	}
	e.Input = input
	e.Outputs = outputs
	if errorCode.Valid {
		c := errorCode.String
		e.ErrorCode = &c
	}
	if errorMsg.Valid {
		m := errorMsg.String
		e.ErrorMessage = &m
	}
	e.CreatedAt = createdAt.Time
	if startedAt.Valid {
		st := startedAt.Time
		e.StartedAt = &st
	}
	if finishedAt.Valid {
		ft := finishedAt.Time
		e.FinishedAt = &ft
	}
	return e, nil
}

// scanExecutionList 扫描 execListCols(无 input/outputs/definition_hash;
// 列序与常量逐字对应)。NULL 规范化与 scanExecution 同规。
func scanExecutionList(r rowScanner) (Execution, error) {
	var (
		e          Execution
		traceID    sql.NullString
		errorCode  sql.NullString
		errorMsg   sql.NullString
		createdAt  sql.NullTime
		startedAt  sql.NullTime
		finishedAt sql.NullTime
	)
	if err := r.Scan(&e.ID, &e.DefinitionID, &e.DefinitionName, &e.DefinitionVersion,
		&e.Status, &e.TriggerType, &e.TriggeredBy, &traceID,
		&errorCode, &errorMsg, &createdAt, &startedAt, &finishedAt, &e.ExecDate); err != nil {
		return Execution{}, err
	}
	if traceID.Valid {
		t := traceID.String
		e.TraceID = &t
	}
	if errorCode.Valid {
		c := errorCode.String
		e.ErrorCode = &c
	}
	if errorMsg.Valid {
		m := errorMsg.String
		e.ErrorMessage = &m
	}
	e.CreatedAt = createdAt.Time
	if startedAt.Valid {
		st := startedAt.Time
		e.StartedAt = &st
	}
	if finishedAt.Valid {
		ft := finishedAt.Time
		e.FinishedAt = &ft
	}
	return e, nil
}
