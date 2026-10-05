// T18.4 投影只读端点的 store 读原语(D-2026-10-05-t184-projection-read-
// endpoints):node_execution 投影列(按路径)与 node_attempt 台账(按轮)。
// 读路径无事务、连接由调用方持有(execstore 同形:钉版行集在物化时一次
// 建立,并发 CAS 迁移间的两次读轻微不一致是既有可接受读语义——纯读
// 面不引入锁读,更不得复用 scheduler.lockAllRows 的 FOR UPDATE 写路径)。
// node_attempt / node_execution 均无租户列:调用方必须先经 GetExecution
// (tenant,id) 过闸再进本文件(node 系经 execution 锚派生,execstore 钦定;
// 直接按 id 查 = 跨租户泄露)。保留期语义(D-t186):janitor 30d 物理删除
// 终态执行的终态 attempt 行(活执行守卫使非终态执行证据永续)——「保留期
// 即留档证据支持窗」是下界承诺,台账变薄是契约内行为,不报错。
package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// NodeProjection 是按路径投影视图的 node_execution 单行查询面模型
// (fat 列:input_snapshot/output 进读——仅节点粒度单行,1MiB 级大列
// 无列表放大面)。output 语义 = 最新成功轮投影(D-t92 决策 9),旧轮
// output 物理载体在 node_attempt;attempts_base 是回滚轮区间锚:
// 旧轮 = attempt_no ∈ [1, attempts_base],当前轮代 = (attempts_base, attempts]。
type NodeProjection struct {
	ID            uint64
	NodeID        string
	InstanceNo    uint16
	Type          string
	Status        string
	Attempts      uint16
	AttemptsBase  uint16
	MaxAttempts   uint16
	InputSnapshot []byte // nil = NULL(PENDING/SKIPPED 行,或 pre-T9.2 存量)
	Output        []byte // nil = NULL:无成功轮或未终态(由 Status 消歧)
	ErrorCode     *string
	ErrorMessage  *string
	StartedAt     *time.Time
	FinishedAt    *time.Time
}

// NodeAttempt 是 node_attempt 一行的查询面模型(0001 建表 + 0007 留档
// 扩列)。usage/cost 列组与 auditEventDTO 同形(仅 llm 轮非空;
// NULL = 无用量语义)。InputSnapshot/Output 仅详情路径(attemptCols)
// 进读——列表路径(attemptListCols)瘦列缺席,镜像 execListCols 纪律。
type NodeAttempt struct {
	ID               uint64
	AttemptNo        uint16
	Status           string // RUNNING / SUCCEEDED / FAILED / DEAD(行只在派发事务内创建,无 PENDING)
	WorkerID         *string
	Model            *string
	PromptTokens     *uint32
	CompletionTokens *uint32
	TotalTokens      *uint32
	CostAmount       *float64
	CostCurrency     *string
	Estimated        bool
	ErrorCode        *string
	ErrorMessage     *string
	StartedAt        time.Time
	FinishedAt       *time.Time // DEAD 恒 NULL(未观察到终态)
	InputSnapshot    []byte     // fat:claim 事务抄档;NULL = pre-T9.2 存量或派发链外直插
	Output           []byte     // fat:收口事务写;FAILED/DEAD 恒 NULL(未产出)
}

// nodeProjCols 是按路径投影列集(node_execution fat:投影两列 + 轮区间锚)。
const nodeProjCols = `id, node_id, instance_no, type, status, attempts, attempts_base, max_attempts,
 input_snapshot, output, error_code, error_message, started_at, finished_at`

// attemptListCols 是按轮列表瘦列集:剔除 input_snapshot/output JSON 大列
// (attempt_no 单调不清零 × 每列 1MiB 级 = 无上界放大;DB 读侧同排,
// execListCols 同判)。usage/cost 小列(≤14 位)无放大面,随列表携带。
const attemptListCols = `id, attempt_no, status, worker_id, model, prompt_tokens, completion_tokens,
 total_tokens, cost_amount, cost_currency, estimated, error_code, error_message, started_at, finished_at`

// attemptCols 是单轮详情全列集(证据四件套 input/output/error/usage)。
const attemptCols = attemptListCols + `, input_snapshot, output`

// GetNodeProjection 按 (execution_id, node_id, instance_no) 点查节点行
// 投影(uk_node_exec 三列唯一寻址;parallel 实例行 1..N,控制行/单实例
// 节点恒 0——instance 维度由调用方解析,缺省 0)。
func (s *Store) GetNodeProjection(ctx context.Context, q DBTX,
	execID uint64, nodeID string, instanceNo uint16) (NodeProjection, error) {
	row := q.QueryRowContext(ctx,
		"SELECT "+nodeProjCols+" FROM node_execution WHERE execution_id = ? AND node_id = ? AND instance_no = ?",
		execID, nodeID, instanceNo)
	np, err := scanNodeProjection(row)
	if errors.Is(err, sql.ErrNoRows) {
		return NodeProjection{}, fmt.Errorf("%w: node %q instance %d of execution %d",
			ErrNotFound, nodeID, instanceNo, execID)
	}
	return np, err
}

// GetNodeExecutionID 是按轮端点的锚解析:同谓词瘦读 node_execution.id
// (零投影大列——attempts 端点不需要 fat 列,不为锚放大)。
func (s *Store) GetNodeExecutionID(ctx context.Context, q DBTX,
	execID uint64, nodeID string, instanceNo uint16) (uint64, error) {
	var id uint64
	err := q.QueryRowContext(ctx,
		`SELECT id FROM node_execution WHERE execution_id = ? AND node_id = ? AND instance_no = ?`,
		execID, nodeID, instanceNo).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: node %q instance %d of execution %d",
			ErrNotFound, nodeID, instanceNo, execID)
	}
	if err != nil {
		return 0, fmt.Errorf("api: resolve node execution %q/%d of %d: %w", nodeID, instanceNo, execID, err)
	}
	return id, nil
}

// ListNodeAttempts 列节点全轮次台账(uk_attempt 最左前缀,attempt_no
// 升序 = 跨轮全时间线,D-t92 决策 6 的 ListDecisions 同款读面设计:
// 旧行是新轮的对照证据)。行数上界 = attempts(单调整数);被 janitor
// 清走/从未物化的轮缺席(变薄,不伪造占位)。
func (s *Store) ListNodeAttempts(ctx context.Context, q DBTX, nodeExecID uint64) ([]NodeAttempt, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT "+attemptListCols+" FROM node_attempt WHERE node_execution_id = ? ORDER BY attempt_no",
		nodeExecID)
	if err != nil {
		return nil, fmt.Errorf("api: list attempts of node execution %d: %w", nodeExecID, err)
	}
	defer rows.Close() //nolint:errcheck // 只读路径,关闭错误无处置义
	var out []NodeAttempt
	for rows.Next() {
		a, err := scanNodeAttemptList(rows)
		if err != nil {
			return nil, fmt.Errorf("api: scan attempt of %d: %w", nodeExecID, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("api: iterate attempts of %d: %w", nodeExecID, err)
	}
	return out, nil
}

// GetNodeAttempt 按 (node_execution_id, attempt_no) 取单轮台账行
// (uk_attempt 点查,全列四件套)。缺行 = 未物化(派发窗/stale ack)/
// 已过保留期(janitor)/ 越界,三者同面由调用方映射 404,不区分成因。
func (s *Store) GetNodeAttempt(ctx context.Context, q DBTX,
	nodeExecID uint64, attemptNo uint16) (NodeAttempt, error) {
	row := q.QueryRowContext(ctx,
		"SELECT "+attemptCols+" FROM node_attempt WHERE node_execution_id = ? AND attempt_no = ?",
		nodeExecID, attemptNo)
	a, err := scanNodeAttempt(row)
	if errors.Is(err, sql.ErrNoRows) {
		return NodeAttempt{}, fmt.Errorf("%w: attempt %d of node execution %d",
			ErrNotFound, attemptNo, nodeExecID)
	}
	return a, err
}

// scanNodeProjection 扫 nodeProjCols(列序与常量逐字对应;NULL 规范化
// 与 scanExecution 同规)。
func scanNodeProjection(r rowScanner) (NodeProjection, error) {
	var (
		np         NodeProjection
		errCode    sql.NullString
		errMsg     sql.NullString
		startedAt  sql.NullTime
		finishedAt sql.NullTime
	)
	if err := r.Scan(&np.ID, &np.NodeID, &np.InstanceNo, &np.Type, &np.Status,
		&np.Attempts, &np.AttemptsBase, &np.MaxAttempts,
		&np.InputSnapshot, &np.Output, &errCode, &errMsg, &startedAt, &finishedAt); err != nil {
		return NodeProjection{}, err
	}
	np.ErrorCode = nullStringPtr(errCode)
	np.ErrorMessage = nullStringPtr(errMsg)
	if startedAt.Valid {
		st := startedAt.Time
		np.StartedAt = &st
	}
	if finishedAt.Valid {
		ft := finishedAt.Time
		np.FinishedAt = &ft
	}
	return np, nil
}

// scanNodeAttemptList 扫 attemptListCols(瘦列,fat 两列零值缺席;
// 列序与常量逐字对应)。usage/cost 扫描与 scanAuditEvent 同规。
func scanNodeAttemptList(r rowScanner) (NodeAttempt, error) {
	return scanAttemptCols(r, false)
}

// scanNodeAttempt 扫 attemptCols(全列四件套,详情路径)。
func scanNodeAttempt(r rowScanner) (NodeAttempt, error) {
	return scanAttemptCols(r, true)
}

// scanAttemptCols 是两列集的共用体(fat 布尔切尾两 JSON 列;
// scanExecution/scanExecutionList 的列集分工同款,但彼处因 NULL 规范化
// 差异拆两函数,此处尾列语义同构收敛单实现)。
func scanAttemptCols(r rowScanner, fat bool) (NodeAttempt, error) {
	var (
		a          NodeAttempt
		workerID   sql.NullString
		model      sql.NullString
		prompt     sql.NullInt64
		compl      sql.NullInt64
		totalTok   sql.NullInt64
		costAmt    sql.NullFloat64
		costCur    sql.NullString
		errCode    sql.NullString
		errMsg     sql.NullString
		finishedAt sql.NullTime
	)
	dest := []any{&a.ID, &a.AttemptNo, &a.Status, &workerID, &model,
		&prompt, &compl, &totalTok, &costAmt, &costCur, &a.Estimated,
		&errCode, &errMsg, &a.StartedAt, &finishedAt}
	if fat {
		dest = append(dest, &a.InputSnapshot, &a.Output)
	}
	if err := r.Scan(dest...); err != nil {
		return NodeAttempt{}, err
	}
	a.WorkerID = nullStringPtr(workerID)
	a.Model = nullStringPtr(model)
	if prompt.Valid {
		v := uint32(prompt.Int64) // INT UNSIGNED ≤ 2^32-1,int64 无损承载
		a.PromptTokens = &v
	}
	if compl.Valid {
		v := uint32(compl.Int64)
		a.CompletionTokens = &v
	}
	if totalTok.Valid {
		v := uint32(totalTok.Int64)
		a.TotalTokens = &v
	}
	if costAmt.Valid {
		v := costAmt.Float64
		a.CostAmount = &v
	}
	a.CostCurrency = nullStringPtr(costCur)
	a.ErrorCode = nullStringPtr(errCode)
	a.ErrorMessage = nullStringPtr(errMsg)
	if finishedAt.Valid {
		ft := finishedAt.Time
		a.FinishedAt = &ft
	}
	return a, nil
}
