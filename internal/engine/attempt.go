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

// CloseAttempt 收口 attempt:RUNNING→SUCCEEDED|FAILED(观察到的终态)。
// DEAD 置位归 T2.5(reconciler 判死),同一条边,此处不开放。
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
