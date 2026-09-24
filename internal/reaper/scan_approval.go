package reaper

// T4.3 审批超时扫描器(scan_approvals 作业,docs/designs 拍板 D-2026-09-24-
// t43-approval-timeout):WAITING_APPROVAL 面的周期对账,三个处置分支——
//
//	⑤a 超时(deadline_at 过期,idx_node_status_deadline 的第二消费面;策略
//	  真源 = input_snapshot 的 on_timeout,schema enum escalate|fail):
//	    已决票            → resume 死信网最小版:活事件闸 → 幂等重投(同 TaskID
//	                       resume:<exec>:<node>:<attempts+1>,D-t42 §8-② 移交;
//	                       archived 滞留副本靠 relay 冲突收敛,P0 手动恢复 =
//	                       asynq Inspector RunTask,archived 告警归 T6.4)
//	    未决票 + escalate → 升级通知恰好一次(escalated_at 谓词锚)+ 保持等待
//	                       (Q9:永不自动通过);通知 = approval.notify outbox 事件
//	                       (首个发射点,worker 占位消费,T4.5 SMTP 换装)
//	    未决票 + fail     → FailSuspendedNode(approval_timeout)显式终败 +
//	                       执行终局化 FAILED(ROADMAP T4.3 验收面)
//	⑤b 孤儿(wf 已终态,fail-fast 遗留;resume.go 终态守卫移交)→ 平台裁决
//	   CANCELLED(镜像扫描③;cancel 优先于重投——终态执行的续跑是永久 no-op)。
//
// 处置事务锁序 = node → wf(与 reapOnce 全局偏序一致);裁决时钟 = DB 端
// NOW(3)(D-t25 #2)。策略/审批人真相源在 input_snapshot(DSL 钉版物化),
// reaper 零策略配置面——on_timeout 是每节点 DSL 规格,非全局 env。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"braid/internal/audit"
	"braid/internal/engine"
	"braid/internal/outbox"
	"braid/internal/scheduler"
)

// approvalEntry 标记扫描⑤的入口,决定锁后复核面(孤儿入口只做终态收口,
// 超时入口跑完整分派树——两入口在 wf 终态分叉上汇合)。
type approvalEntry int

const (
	entryApprovalTimeout approvalEntry = iota // ⑤a deadline 过期
	entryApprovalOrphan                       // ⑤b wf 已终态
)

// scanApprovals 扫描⑤:超时 + 孤儿,共用 reapApproval 处置事务。
func (r *Reaper) scanApprovals(ctx context.Context) error {
	if err := r.scanApprovalTimeouts(ctx); err != nil {
		return err
	}
	return r.scanApprovalOrphans(ctx)
}

// scanApprovalTimeouts 扫描⑤a:WAITING_APPROVAL 且挂起截止过期(skew margin
// = SCAN_INTERVAL,镜像扫描②:deadline 由 suspend 应用侧算入,偏差由 reconciler
// 阈值吸收;NULL deadline 谓词恒假天然排除)。
//
// 已升级裁剪(对抗审计 LOW 修):升级后未决票的行再无 P0 裁决可做(Q9 策略
// 钉死快照、escalate 恰好一次),deadline 又被刻意保留 → 不裁剪则它们永久
// 命中谓词,每 tick 白付锁+复核事务,≥1000 行时吃满单 tick 预算饿死高位
// 候选(含 fail 策略节点)。谓词因此收紧为「未升级 OR 已决票」——已决票
// 半边保死信网对 escalate→后决定→事件死序的修复路径(便宜修法
// escalated_at IS NULL 会错杀该路径);事务内 escalatedAt.Valid 复核保留为
// TOCTOU 守卫。纯效率谓词:去掉它行为不变(多付每 tick 空转),不设变异锚。
func (r *Reaper) scanApprovalTimeouts(ctx context.Context) error {
	var lastID uint64
	for batch := 0; batch < scanMaxBatch; batch++ {
		rows, err := r.deps.DB.QueryContext(ctx, `
SELECT id, execution_id
  FROM node_execution
 WHERE status='WAITING_APPROVAL' AND deadline_at < NOW(3) - INTERVAL ? MICROSECOND
   AND (escalated_at IS NULL
        OR EXISTS(SELECT 1 FROM approval_decision d
                   WHERE d.node_execution_id = node_execution.id))
   AND id > ?
 ORDER BY id
 LIMIT ?`, r.cfg.ScanInterval.Microseconds(), lastID, scanBatchSize)
		if err != nil {
			return fmt.Errorf("reaper: scan approval timeouts: %w", err)
		}
		cands, err := collectNodeCands(rows, &lastID)
		if err != nil {
			return err
		}
		if len(cands) == 0 {
			return nil
		}
		for _, c := range cands {
			if err := r.reapApproval(ctx, c.id, c.execID, entryApprovalTimeout); err != nil {
				r.log.Error("reap.approval_timeout_failed",
					"node_execution_id", c.id, "err", err)
			}
		}
		if len(cands) < scanBatchSize {
			return nil
		}
	}
	r.log.Warn("scan.backlog", "scanner", "approval_timeouts",
		"hint", "candidates exceeded per-tick cap; retrying next tick")
	return nil
}

// scanApprovalOrphans 扫描⑤b:WAITING_APPROVAL 且 wf 已终态(fail-fast 遗留;
// 静默窗镜像扫描③,避开终局化提交与扫描之间的在飞窗口)。
func (r *Reaper) scanApprovalOrphans(ctx context.Context) error {
	var lastID uint64
	for batch := 0; batch < scanMaxBatch; batch++ {
		rows, err := r.deps.DB.QueryContext(ctx, `
SELECT n.id, n.execution_id
  FROM node_execution n
  JOIN workflow_execution w ON w.id = n.execution_id
 WHERE n.status = 'WAITING_APPROVAL'
   AND w.status IN ('SUCCEEDED','FAILED','CANCELLED')
   AND w.last_activity_at < NOW(3) - INTERVAL ? MICROSECOND
   AND n.id > ?
 ORDER BY n.id
 LIMIT ?`, orphanQuiet.Microseconds(), lastID, scanBatchSize)
		if err != nil {
			return fmt.Errorf("reaper: scan approval orphans: %w", err)
		}
		cands, err := collectNodeCands(rows, &lastID)
		if err != nil {
			return err
		}
		if len(cands) == 0 {
			return nil
		}
		for _, c := range cands {
			if err := r.reapApproval(ctx, c.id, c.execID, entryApprovalOrphan); err != nil {
				r.log.Error("reap.approval_orphan_failed",
					"node_execution_id", c.id, "err", err)
			}
		}
		if len(cands) < scanBatchSize {
			return nil
		}
	}
	r.log.Warn("scan.backlog", "scanner", "approval_orphans",
		"hint", "candidates exceeded per-tick cap; retrying next tick")
	return nil
}

// reapApproval 处置事务外层:Transient(1213/1205)抖动重试 ≤3(reapAttempt
// 同款循环形状)。
func (r *Reaper) reapApproval(ctx context.Context, nodeExecID, execID uint64, ent approvalEntry) error {
	for attempt := 0; ; attempt++ {
		err := r.reapApprovalOnce(ctx, nodeExecID, execID, ent)
		if err == nil || errors.Is(err, errSkip) {
			return nil
		}
		if !engine.Transient(err) || attempt >= maxTransientRetries {
			return err
		}
		backoff := time.Duration(float64(time.Millisecond*25<<(attempt+1)) * (1 + rand.Float64()))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
}

// reapApprovalOnce 是扫描⑤的统一处置事务:锁 node → 复核 WAITING_APPROVAL →
// DB 时钟 → 锁 wf → 分派树(头注释三分支)。锁后复核(TOCTOU 吸收):状态
// 复活 / deadline 复活 / wf 复活各自让位(errSkip 空事务回滚)。
func (r *Reaper) reapApprovalOnce(ctx context.Context, nodeExecID, execID uint64, ent approvalEntry) error {
	return engine.WithTx(ctx, r.deps.DB, func(q engine.DBTX) error {
		// [1] 锁 node 行。
		var (
			nodeID      string
			nodeStatus  engine.NodeState
			nodeVer     uint32
			attempts    uint16
			workerID    sql.NullString
			deadline    sql.NullTime
			escalatedAt sql.NullTime
			snapshot    []byte
		)
		err := q.QueryRowContext(ctx, `
SELECT node_id, status, version, attempts, worker_id, deadline_at, escalated_at, input_snapshot
  FROM node_execution WHERE id=? FOR UPDATE`, nodeExecID).
			Scan(&nodeID, &nodeStatus, &nodeVer, &attempts, &workerID, &deadline, &escalatedAt, &snapshot)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("reaper: node_execution %d vanished (data defect)", nodeExecID)
		}
		if err != nil {
			return fmt.Errorf("reaper: lock node %d: %w", nodeExecID, err)
		}
		if nodeStatus != engine.NodeWaitingApproval {
			return errSkip // 已被续跑/取消/终局化的竞速收敛
		}

		// [2] DB 时钟(裁决同钟系,与候选扫描同一时钟)。
		var now time.Time
		if err := q.QueryRowContext(ctx, `SELECT NOW(3)`).Scan(&now); err != nil {
			return fmt.Errorf("reaper: read db clock: %w", err)
		}

		// [3] 锁 wf 行。终态执行 → 孤儿收口(cancel 优先:终态执行上的一切
		// 后续处置都是永久 no-op;镜像扫描③的平台裁决语义)。
		var wfStatus engine.WorkflowState
		if err := q.QueryRowContext(ctx,
			`SELECT status FROM workflow_execution WHERE id=? FOR UPDATE`, execID).
			Scan(&wfStatus); err != nil {
			return fmt.Errorf("reaper: lock execution %d: %w", execID, err)
		}
		if wfStatus.Terminal() {
			cancelVer, err := engine.CancelNode(ctx, q, nodeExecID, nodeVer)
			if err != nil {
				if errors.Is(err, engine.ErrConflict) {
					return errSkip
				}
				return err
			}
			if err := r.auditRow(ctx, q, audit.EventNodeStateChanged, execID, nodeExecID, 0, nodeID,
				audit.NodeStateChangedPayload{
					From: string(nodeStatus), To: string(engine.NodeCancelled),
					NodeID: nodeID, Version: cancelVer, AttemptNo: attempts, WorkerID: workerID.String,
				}); err != nil {
				return err
			}
			r.log.Warn("approval.orphan_cancelled",
				"node_execution_id", nodeExecID, "execution_id", execID, "wf_status", wfStatus)
			return nil
		}
		// 孤儿入口的非终态复审 = wf 在扫描后复活(竞速收敛),本轮让位;超时
		// 语义由扫描⑤a 承载,不在此入口混入。
		if ent == entryApprovalOrphan {
			return errSkip
		}

		// [4] 超时锁内复核(deadline 复活让位;skew margin 镜像扫描②)。
		if !deadline.Valid || !deadline.Time.Before(now.Add(-r.cfg.ScanInterval)) {
			return errSkip
		}

		// [5] 已决票?(或签首票定局,P0 至多一行,取最早;决定方向不入判据
		// ——重投事件仅锚 DecisionID,worker 续跑事务回表复核)。
		var decisionID uint64
		decErr := q.QueryRowContext(ctx,
			`SELECT id FROM approval_decision WHERE node_execution_id=? ORDER BY decided_at, id LIMIT 1`,
			nodeExecID).Scan(&decisionID)
		if decErr != nil && !errors.Is(decErr, sql.ErrNoRows) {
			return fmt.Errorf("reaper: read decision node %d: %w", nodeExecID, decErr)
		}
		if decErr == nil {
			return r.redispatchResume(ctx, q, execID, nodeExecID, nodeID, attempts, decisionID)
		}

		// [6] 未决票 → 策略(快照真相源;WAITING_APPROVAL ⇒ snapshot NOT NULL
		// 是 engine 不变量,resume.go 同依据)。
		var snap scheduler.ApprovalInput
		if err := json.Unmarshal(snapshot, &snap); err != nil {
			return fmt.Errorf("reaper: approval snapshot node %d: %w", nodeExecID, err)
		}
		switch snap.OnTimeout {
		case "escalate", "": // 空 = schema default(effectiveApprovalSpec 物化兜底)
			if escalatedAt.Valid {
				return errSkip // Q9:escalate 恰好一次;已升级 = 保持等待(永不自动通过)
			}
			if _, err := engine.MarkNodeEscalated(ctx, q, nodeExecID, nodeVer); err != nil {
				return err
			}
			if err := r.auditRow(ctx, q, audit.EventApprovalEscalated, execID, nodeExecID, 0, nodeID,
				audit.ApprovalEscalatedPayload{NodeID: nodeID, DeadlineAt: deadline.Time}); err != nil {
				return err
			}
			if err := r.appendApprovalNotifyEvent(ctx, q, execID, nodeExecID); err != nil {
				return err
			}
			if err := engine.TouchExecution(ctx, q, execID); err != nil {
				return err
			}
			r.log.Warn("approval.escalated",
				"node_execution_id", nodeExecID, "execution_id", execID,
				"deadline_at", deadline.Time, "hint", "policy=escalate: notified once, still waiting")
			return nil

		case "fail":
			failVer, err := engine.FailSuspendedNode(ctx, q, nodeExecID, nodeVer,
				"approval_timeout", "approval deadline exceeded (policy=fail, reaper)")
			if err != nil {
				return err
			}
			if err := r.auditRow(ctx, q, audit.EventNodeStateChanged, execID, nodeExecID, 0, nodeID,
				audit.NodeStateChangedPayload{
					From: string(nodeStatus), To: string(engine.NodeFailed),
					NodeID: nodeID, Version: failVer, Code: "approval_timeout",
					AttemptNo: attempts, WorkerID: workerID.String,
				}); err != nil {
				return err
			}
			// wf 镜像先行(wfEdges 无 WAITING_APPROVAL→FAILED 直达边,resume.go
			// 同款:先回 RUNNING 再终局化,中间态同事务不可见),再 fail-fast。
			if wfStatus == engine.WorkflowWaitingApproval {
				if err := engine.ResumeExecution(ctx, q, execID); err != nil &&
					!errors.Is(err, engine.ErrConflict) {
					return err
				}
			}
			if err := engine.FinishExecution(ctx, q, execID, engine.WorkflowFailed, nil,
				"node_failed",
				fmt.Sprintf("node %s failed: approval_timeout (reaper)", nodeID)); err != nil {
				if errors.Is(err, engine.ErrConflict) {
					return nil // 他方先终局化 = 良性收敛
				}
				return err
			}
			r.log.Warn("approval.timeout_failed",
				"node_execution_id", nodeExecID, "execution_id", execID,
				"attempts", attempts, "hint", "policy=fail: node explicitly FAILED")
			return nil

		default:
			return fmt.Errorf("reaper: approval node %d on_timeout %q outside vocabulary (snapshot drift)",
				nodeExecID, snap.OnTimeout)
		}
	})
}

// redispatchResume 是已决票 + 超阈分支(resume 死信网最小版):活事件闸 →
// 同 TaskID 幂等重投。TaskID 四段式在锁内确定性重导出(attempts+1 = 续跑将
// 创建的 attempt 号,D-t42 §1 形态)——节点未离开 WAITING_APPROVAL ⇒ 与
// api 原发事件同 ID:asynq 活副本冲突即收敛,relay 耗尽(asynq 从未见任务)
// 则新投落地。重投后节点零变化(续跑权威在 worker)。
func (r *Reaper) redispatchResume(ctx context.Context, q engine.DBTX,
	execID, nodeExecID uint64, nodeID string, attempts uint16, decisionID uint64) error {
	taskID := fmt.Sprintf("resume:%d:%s:%d", execID, nodeID, attempts+1)
	var live int
	if err := q.QueryRowContext(ctx, `
SELECT COUNT(*) FROM outbox
 WHERE task_id=?
   AND (status='PENDING'
        OR (status='DISPATCHED' AND dispatched_at >= NOW(3) - INTERVAL ? MICROSECOND))`,
		taskID, r.cfg.DispatchStale.Microseconds()).Scan(&live); err != nil {
		return fmt.Errorf("reaper: resume live gate node %d: %w", nodeExecID, err)
	}
	if live > 0 {
		r.log.Warn("approval.resume_deferred_live_event",
			"node_execution_id", nodeExecID, "live_events", live,
			"hint", "resume event in flight; re-checked next tick")
		return errSkip
	}
	raw, err := json.Marshal(scheduler.ResumePayload{
		ExecutionID: execID, NodeExecutionID: nodeExecID,
		DecisionID: decisionID, AttemptNo: attempts + 1,
	})
	if err != nil {
		return fmt.Errorf("reaper: marshal resume payload: %w", err)
	}
	id, err := r.deps.IDs.Next()
	if err != nil {
		return fmt.Errorf("reaper: gen event id: %w", err)
	}
	if err := outbox.Append(ctx, q, outbox.Event{
		ID:          id,
		ExecutionID: execID,
		EventType:   scheduler.EventTypeExecutionResume,
		TaskID:      taskID,
		Queue:       r.cfg.Queue,
		Payload:     raw,
	}); err != nil {
		return err
	}
	if err := r.auditRow(ctx, q, audit.EventApprovalResumeRedispatched, execID, nodeExecID, 0, nodeID,
		audit.ApprovalResumeRedispatchedPayload{
			NodeID: nodeID, DecisionID: decisionID, AttemptNo: attempts + 1,
		}); err != nil {
		return err
	}
	if err := engine.TouchExecution(ctx, q, execID); err != nil {
		return err
	}
	r.log.Warn("approval.resume_redispatched",
		"node_execution_id", nodeExecID, "execution_id", execID,
		"task_id", taskID, "decision_id", decisionID,
		"hint", "decided but resume event dead; re-enqueued with same task id")
	return nil
}

// appendApprovalNotifyEvent 落升级通知事件(approval.notify 首个发射点;
// 恰好一次锚在 escalated_at——本调用只在 MarkNodeEscalated 同事务后到达,
// 同节点永无第二条通知意图,TaskID notify:<node_exec_id> 满足词表不变量)。
// 消费面:worker 占位 handler(log-only ack),T4.5 SMTP 换装。
func (r *Reaper) appendApprovalNotifyEvent(ctx context.Context, q engine.DBTX,
	execID, nodeExecID uint64) error {
	raw, err := json.Marshal(scheduler.ApprovalNotifyPayload{
		ExecutionID: execID, NodeExecutionID: nodeExecID, Reason: "escalated",
	})
	if err != nil {
		return fmt.Errorf("reaper: marshal notify payload: %w", err)
	}
	id, err := r.deps.IDs.Next()
	if err != nil {
		return fmt.Errorf("reaper: gen event id: %w", err)
	}
	return outbox.Append(ctx, q, outbox.Event{
		ID:          id,
		ExecutionID: execID,
		EventType:   scheduler.EventTypeApprovalNotify,
		TaskID:      fmt.Sprintf("notify:%d", nodeExecID),
		Queue:       r.cfg.Queue,
		Payload:     raw,
	})
}
