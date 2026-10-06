package reaper

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"braid/internal/engine"
	"braid/internal/metrics"
)

// scanExecs 扫描④:执行级对账(索引 idx_exec_status_activity;status 等值
// 前缀 ⇒ 驱动集 = 活跃集,天然有界;WAITING_APPROVAL 合法挂起刻意排除)。
//
//	④a PENDING 滞留(执行启动事件死/丢)→ reapPendingExec 判死(W2)。
//	④b RUNNING 且静默(全节点终态但终态判定未落——Complete 提交后
//	  finishIfDone 失败与崩溃同级)→ 委托 scheduler.FinishIfDone(W3)。
func (r *Reaper) scanExecs(ctx context.Context) error {
	if err := r.scanPendingExecs(ctx); err != nil {
		return err
	}
	return r.scanStaleRunning(ctx)
}

func (r *Reaper) scanPendingExecs(ctx context.Context) error {
	var lastID uint64
	for batch := 0; batch < scanMaxBatch; batch++ {
		rows, err := r.deps.DB.QueryContext(ctx, `
SELECT id FROM workflow_execution
 WHERE status='PENDING' AND last_activity_at < NOW(3) - INTERVAL ? MICROSECOND
   AND id > ?
 ORDER BY id
 LIMIT ?`, r.cfg.PendingTTL.Microseconds(), lastID, scanBatchSize)
		if err != nil {
			return fmt.Errorf("reaper: scan pending execs: %w", err)
		}
		ids, err := collectIDs(rows, &lastID)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			if err := r.reapPendingExec(ctx, id); err != nil {
				r.log.Error("reap.pending_exec_failed", "execution_id", id, "err", err)
			}
		}
		if len(ids) < scanBatchSize {
			return nil
		}
	}
	r.mBacklog(metrics.ScanExecPending)
	r.log.Warn("scan.backlog", "scanner", "pending_execs",
		"hint", "candidates exceeded per-tick cap; retrying next tick")
	return nil
}

// reapPendingExec 是 W2:PENDING 执行判死。锁序 = wf 行先行(Start 同例外:
// PENDING 执行无 claim/attempt,不存在 attempt→wf 在飞写者,靠 wf 行锁与
// Start 互斥串行,败方复核后 skip——锁序不变量的唯一既定例外,不新增环)。
// 事件活性闸:exec: 启动事件 PENDING 恒活(仍在退避重投,攻击 4 吸收);
// DISPATCHED 界于 DISPATCH_STALE(asynq 队列积压容忍窗);DEAD 不挡闸
// (事件穷尽即判死依据)。
func (r *Reaper) reapPendingExec(ctx context.Context, execID uint64) error {
	err := engine.WithTx(ctx, r.deps.DB, func(q engine.DBTX) error {
		// [0] fence 首置(T20.1,D-2026-10-06-t201 决策 4;对抗审计 MEDIUM
		// 修复——PENDING 执行终局化同挂,zombie reaper 不得判 FAILED)。
		if ferr := r.fence(ctx, q, "pending_exec", "execution_id", execID); ferr != nil {
			return ferr
		}
		var status engine.WorkflowState
		if err := q.QueryRowContext(ctx,
			`SELECT status FROM workflow_execution WHERE id=? FOR UPDATE`, execID).
			Scan(&status); err != nil {
			return fmt.Errorf("reaper: lock execution %d: %w", execID, err)
		}
		if status != engine.WorkflowPending {
			return errSkip // 已被 Start 拉起或他方收口
		}
		var live int
		if err := q.QueryRowContext(ctx, `
SELECT COUNT(*) FROM outbox
 WHERE execution_id=? AND event_type='execution.start'
   AND (status='PENDING'
        OR (status='DISPATCHED' AND dispatched_at >= NOW(3) - INTERVAL ? MICROSECOND))`,
			execID, r.cfg.DispatchStale.Microseconds()).Scan(&live); err != nil {
			return fmt.Errorf("reaper: exec start gate %d: %w", execID, err)
		}
		if live > 0 {
			r.log.Warn("execution.pending_deferred_live_event",
				"execution_id", execID, "live_events", live)
			return errSkip
		}
		if err := engine.FinishExecution(ctx, q, execID, engine.WorkflowFailed, nil,
			"reconciler_timeout", "execution.start event dead: PENDING beyond ttl"); err != nil {
			return err
		}
		r.log.Warn("execution.failed_pending_timeout", "execution_id", execID)
		return nil
	})
	if errors.Is(err, errSkip) {
		return nil
	}
	return err
}

func (r *Reaper) scanStaleRunning(ctx context.Context) error {
	var lastID uint64
	for batch := 0; batch < scanMaxBatch; batch++ {
		rows, err := r.deps.DB.QueryContext(ctx, `
SELECT id FROM workflow_execution
 WHERE status='RUNNING' AND last_activity_at < NOW(3) - INTERVAL ? MICROSECOND
   AND id > ?
 ORDER BY id
 LIMIT ?`, r.cfg.ExecStale.Microseconds(), lastID, scanBatchSize)
		if err != nil {
			return fmt.Errorf("reaper: scan stale running: %w", err)
		}
		ids, err := collectIDs(rows, &lastID)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			// W3:委托调度器终态补判(零锁,与 Complete 提交后判定完全同路径;
			// 双收口由 FinishExecution 的 status-only CAS 仲裁,Conflict 良性)。
			if err := r.deps.Sched.FinishIfDone(ctx, id); err != nil {
				r.log.Warn("execution.finish_check_failed",
					"execution_id", id, "err", err)
			}
		}
		if len(ids) < scanBatchSize {
			return nil
		}
	}
	r.mBacklog(metrics.ScanExecStale)
	r.log.Warn("scan.backlog", "scanner", "stale_running",
		"hint", "candidates exceeded per-tick cap; retrying next tick")
	return nil
}

// collectIDs 是执行级候选收集(游标推进 + 关闭;与 collectNodeCands 同款)。
func collectIDs(rows *sql.Rows, lastID *uint64) ([]uint64, error) {
	defer func() { _ = rows.Close() }()
	var ids []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("reaper: scan execs row: %w", err)
		}
		*lastID = id
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reaper: scan execs rows: %w", err)
	}
	return ids, nil
}
