package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/go-sql-driver/mysql"
)

// node_attempt 消费面(T2.3 派发 / T4.1 挂起收口)。T2.1 只落此薄面:
// attempt 的心跳刷新与 DEAD 判死归 T2.5,不在本任务预判。

// AttemptParams 是 attempt 行的派发事务内写入参数(DDL:行只在派发事务内创建,
// 无 PENDING;heartbeat_at 初值=派发时刻由 DEFAULT CURRENT_TIMESTAMP(3) 承担)。
type AttemptParams struct {
	ID             uint64 // Snowflake,应用生成
	NodeExecID     uint64
	ExecID         uint64
	AttemptNo      uint16 // 1 起;(node_execution_id, attempt_no) 唯一
	WorkerID       string
	IdempotencyKey string          // att:<node_exec_id>:<attempt_no>,供应商侧幂等去重
	AsynqTaskID    string          // 与 outbox.task_id 同值域,队列↔DB 对账连接列
	InputSnapshot  json.RawMessage // T9.2 台账:本 attempt 输入快照抄档(与 node 行同事务一致);空 → NULL
}

// InsertAttempt 在派发事务内创建 RUNNING attempt 行。
// uk_attempt(node_execution_id, attempt_no) 或 uk_attempt_idem 冲突 → ErrConflict
// (T2.3 双投递第二发命中唯一键即幂等返回的 DB 层锚)。
func InsertAttempt(ctx context.Context, q DBTX, p AttemptParams) error {
	_, err := q.ExecContext(ctx, `
INSERT INTO node_attempt
  (id, node_execution_id, execution_id, attempt_no, worker_id, status,
   idempotency_key, asynq_task_id, input_snapshot, started_at)
VALUES (?,?,?,?,?,'RUNNING',?,?,?,NOW(3))`,
		p.ID, p.NodeExecID, p.ExecID, p.AttemptNo, p.WorkerID,
		p.IdempotencyKey, p.AsynqTaskID, nullableJSON(p.InputSnapshot))
	if err != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1062 {
			return fmt.Errorf("%w: duplicate attempt node_exec=%d no=%d: %s",
				ErrConflict, p.NodeExecID, p.AttemptNo, me.Message)
		}
		return fmt.Errorf("engine: insert attempt %d: %w", p.ID, err)
	}
	return nil
}

// TouchAttemptHeartbeat 刷新 attempt 心跳,同事务刷 workflow_execution.last_activity_at
// (DDL §2:attempt 心跳 UPDATE 也同刷本列,否则健康长执行会被 reconciler 误报;
// 调用方 = T2.3 worker 心跳器,长外部调用期间按间隔续写)。
//
// 心跳是尽力语义,与收口的裁决语义刻意分立:attempt 已非 RUNNING(并发收口/DEAD
// 竞态)→ 0 行静默返回——「我还在跑」信号对已终态行本就无意义,executor 随后的
// CloseAttempt 撞同竞态会拿到结构化 ErrConflict,那才是该报错的 0 行。execution
// 缺行经 TouchExecution 报 ErrNotFound(FK 保证下不可达,响亮为敬)。
func TouchAttemptHeartbeat(ctx context.Context, q DBTX, attemptID, execID uint64) error {
	res, err := q.ExecContext(ctx,
		`UPDATE node_attempt SET heartbeat_at=NOW(3) WHERE id=? AND status='RUNNING'`, attemptID)
	if err != nil {
		return fmt.Errorf("engine: heartbeat attempt %d: %w", attemptID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("engine: rows affected (attempt %d heartbeat): %w", attemptID, err)
	}
	if n == 0 {
		return nil // 行已终态:心跳静默让位(见上)
	}
	return TouchExecution(ctx, q, execID)
}

// AttemptUsage 是 attempt 收口时随终写落列的 usage/cost 台账参数(DDL §4,
// T3.1 双录的 node_attempt 半边)。计费真相源 = API 返回 usage(§2 技术选型),
// 三值原样存不求和;金额由 llm.Cost 整数定点产出为十进制字符串,engine 不做
// 算术只落列。Billed=false ⇒ cost 两列写 NULL(NULL=不计费,与 0 免费模型
// 严格区分;CHECK 成对钉死)。audit.Event.Usage 复用本结构(双录同源防漂移)。
type AttemptUsage struct {
	Model            string // 实际解析后的模型名(计费归因;与 audit 行冗余)
	PromptTokens     uint32 // API 返回原值
	CompletionTokens uint32
	TotalTokens      uint32
	CostAmount       string // 十进制字符串("2.500000");Billed=false 时必须为空
	CostCurrency     string // ISO-4217;Billed=false 时必须为空
	Estimated        bool   // true=未知模型估算 fallback,非精确计费
	Billed           bool   // false=本 attempt 不计费(cost 列 NULL)
}

// Validate 镜像 DDL 域约束(调用方 bug 启动即现,不等 DB CHECK 报错):
// model ≤ VARCHAR(100);Billed ⇒ 金额形状 ^\d{1,8}(\.\d{1,6})?$(DECIMAL(14,6)
// 域:8 位整数 + 6 位小数)且币种恰 3 大写字母;非 Billed ⇒ 两字段必须为空
// (写 NULL 语义不被字符串悄悄破坏)。
func (u *AttemptUsage) Validate() error {
	switch {
	case u == nil:
		return nil
	case len(u.Model) == 0 || len(u.Model) > 100:
		return fmt.Errorf("engine: attempt usage model %q empty or >100", u.Model)
	}
	if !u.Billed {
		if u.CostAmount != "" || u.CostCurrency != "" {
			return fmt.Errorf("engine: attempt usage unbilled carries cost %q %q (want empty → NULL)", u.CostAmount, u.CostCurrency)
		}
		return nil
	}
	if !amountRe.MatchString(u.CostAmount) {
		return fmt.Errorf("engine: attempt usage cost %q violates DECIMAL(14,6)", u.CostAmount)
	}
	if !currencyRe.MatchString(u.CostCurrency) {
		return fmt.Errorf("engine: attempt usage currency %q not ISO-4217 (3 uppercase)", u.CostCurrency)
	}
	return nil
}

var (
	amountRe   = regexp.MustCompile(`^\d{1,8}(\.\d{1,6})?$`)
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
)

// CloseAttempt 收口 attempt:RUNNING→SUCCEEDED|FAILED(观察到的终态)。
// usage 非 nil 时同一条终写落 usage/cost 台账列(单次 UPDATE,收口与台账
// 原子;T3.1)。output 非 nil(仅 SUCCEEDED 语义位)时同语句落 T9.2 留档列
// (D-2026-09-28-t92 决策 3①:旧轮输出证据物理载体;FAILED 恒 NULL=未产出,
// 调用方传值与终态矛盾属调用方 bug,此处不做二次守卫——观察面收口方与节点
// CAS 同事务,源头已收敛)。DEAD 边已归 reaper(CloseAttemptDead,同一收口
// 形状的裁决分立)。WHERE status='RUNNING' 保证终态后禁改(行无 updated_at,
// 收口是一次终写)。
func CloseAttempt(ctx context.Context, q DBTX, attemptID uint64, to AttemptState, output json.RawMessage, code, msg string, usage *AttemptUsage) error {
	if to != AttemptSucceed && to != AttemptFailed {
		return fmt.Errorf("%w: attempt →%s(收口仅 SUCCEEDED/FAILED;DEAD 归 T2.5)", ErrIllegalTransition, to)
	}
	if err := usage.Validate(); err != nil {
		return err
	}
	var codeArg, msgArg any
	if code != "" {
		codeArg = code
	}
	if msg != "" {
		msgArg = msg
	}
	var res sql.Result
	var err error
	if usage == nil {
		res, err = q.ExecContext(ctx, `
UPDATE node_attempt
   SET status=?, error_code=?, error_message=?, finished_at=NOW(3), output=?
 WHERE id=? AND status='RUNNING'`,
			to, codeArg, msgArg, nullableJSON(output), attemptID)
	} else {
		var costArg, currencyArg any
		if usage.Billed {
			costArg, currencyArg = usage.CostAmount, usage.CostCurrency
		}
		res, err = q.ExecContext(ctx, `
UPDATE node_attempt
   SET status=?, error_code=?, error_message=?, finished_at=NOW(3), output=?,
       model=?, prompt_tokens=?, completion_tokens=?, total_tokens=?,
       cost_amount=?, cost_currency=?, estimated=?
 WHERE id=? AND status='RUNNING'`,
			to, codeArg, msgArg, nullableJSON(output), usage.Model,
			usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens,
			costArg, currencyArg, usage.Estimated, attemptID)
	}
	if err != nil {
		return fmt.Errorf("engine: close attempt %d: %w", attemptID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("engine: rows affected (attempt %d): %w", attemptID, err)
	}
	if n == 1 {
		return nil
	}
	var got AttemptState
	// FOR UPDATE 当前读,理由同 checkNodeCAS(防 RR read view 过期快照)。
	err = q.QueryRowContext(ctx, `SELECT status FROM node_attempt WHERE id=? FOR UPDATE`, attemptID).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: node_attempt %d", ErrNotFound, attemptID)
	}
	if err != nil {
		return fmt.Errorf("engine: recheck node_attempt %d: %w", attemptID, err)
	}
	// ConflictError 以 NodeState 承载状态字符串(三张表词表同底 string,显式转换;
	// workflow 侧同款)。字段语义 = 期望 vs 实际,与表无关。
	return &ConflictError{ID: attemptID, Expect: NodeState(AttemptRunning), Observed: NodeState(got)}
}

// CloseAttemptDead 收口 attempt:RUNNING→DEAD(reaper 判持有者已死,T2.5)。
// 与 CloseAttempt 的裁决分立:观察终态(SUCCEEDED/FAILED)归执行方,DEAD 归
// reaper(唯一调用方)。finished_at 恒不写(DDL:DEAD 未观察到终态);worker_id
// 不改写(跨接管归因指首任,D-2026-09-22-t25-reaper-semantics #8)。
// 新鲜度不入谓词:行锁下心跳/收口不可并发改行,新鲜度由调用方事务内 FOR UPDATE
// 复核承载,谓词再含只会把 0 行语义二元膨胀为三元,零安全性增益。
func CloseAttemptDead(ctx context.Context, q DBTX, attemptID uint64, code, msg string) error {
	var codeArg, msgArg any
	if code != "" {
		codeArg = code
	}
	if msg != "" {
		msgArg = msg
	}
	res, err := q.ExecContext(ctx, `
UPDATE node_attempt
   SET status='DEAD', error_code=?, error_message=?
 WHERE id=? AND status='RUNNING'`,
		codeArg, msgArg, attemptID)
	if err != nil {
		return fmt.Errorf("engine: close attempt dead %d: %w", attemptID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("engine: rows affected (attempt %d dead): %w", attemptID, err)
	}
	if n == 1 {
		return nil
	}
	var got AttemptState
	err = q.QueryRowContext(ctx, `SELECT status FROM node_attempt WHERE id=? FOR UPDATE`, attemptID).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: node_attempt %d", ErrNotFound, attemptID)
	}
	if err != nil {
		return fmt.Errorf("engine: recheck node_attempt %d: %w", attemptID, err)
	}
	return &ConflictError{ID: attemptID, Expect: NodeState(AttemptRunning), Observed: NodeState(got)}
}
