package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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
	IdempotencyKey string // att:<node_exec_id>:<attempt_no>,供应商侧幂等去重
	AsynqTaskID    string // 与 outbox.task_id 同值域,队列↔DB 对账连接列
}

// InsertAttempt 在派发事务内创建 RUNNING attempt 行。
// uk_attempt(node_execution_id, attempt_no) 或 uk_attempt_idem 冲突 → ErrConflict
// (T2.3 双投递第二发命中唯一键即幂等返回的 DB 层锚)。
func InsertAttempt(ctx context.Context, q DBTX, p AttemptParams) error {
	_, err := q.ExecContext(ctx, `
INSERT INTO node_attempt
  (id, node_execution_id, execution_id, attempt_no, worker_id, status,
   idempotency_key, asynq_task_id, started_at)
VALUES (?,?,?,?,?,'RUNNING',?,?,NOW(3))`,
		p.ID, p.NodeExecID, p.ExecID, p.AttemptNo, p.WorkerID, p.IdempotencyKey, p.AsynqTaskID)
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

// CloseAttempt 收口 attempt:RUNNING→SUCCEEDED|FAILED(观察到的终态)。
// DEAD 边已归 reaper(CloseAttemptDead,同一收口形状的裁决分立)。
// WHERE status='RUNNING' 保证终态后禁改(行无 updated_at,收口是一次终写)。
func CloseAttempt(ctx context.Context, q DBTX, attemptID uint64, to AttemptState, code, msg string) error {
	if to != AttemptSucceed && to != AttemptFailed {
		return fmt.Errorf("%w: attempt →%s(收口仅 SUCCEEDED/FAILED;DEAD 归 T2.5)", ErrIllegalTransition, to)
	}
	var codeArg, msgArg any
	if code != "" {
		codeArg = code
	}
	if msg != "" {
		msgArg = msg
	}
	res, err := q.ExecContext(ctx, `
UPDATE node_attempt
   SET status=?, error_code=?, error_message=?, finished_at=NOW(3)
 WHERE id=? AND status='RUNNING'`,
		to, codeArg, msgArg, attemptID)
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
