package reaper

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
	"braid/internal/metrics"
	"braid/internal/outbox"
	"braid/internal/scheduler"
)

// errSkip 是处置事务内的哨兵:锁后复核判定「无需处置」(活 worker 复活 /
// 已收敛 / 活事件闸拦下)。WithTx 回滚该空事务(零写),外层识别为成功。
var errSkip = errors.New("reaper: skip (recheck decided no action)")

// maxTransientRetries 与 scheduler 同款:1213/1205 抖动重试上限。
const maxTransientRetries = 3

// 批形状:单批 100 行,单 tick 每扫描上限 10 批(1000 行),打满记 backlog
// warn(接 T6.4);主键游标分页(id > last)保证终止性——skip 的行不会在
// 本 tick 重复进入处置。
const (
	scanBatchSize = 100
	scanMaxBatch  = 10
)

// entry 标记候选来源扫描,决定锁后复核的入口谓词(设计 C 节:三个扫描
// 入口统一走 reapAttempt 同一分派树)。
type entry int

const (
	entryAttempt  entry = iota // ① 心跳陈旧
	entryDeadline              // ② 节点 deadline 过期
	entryOrphan                // ③ wf 已终态、节点留 EXECUTING
)

// scanAttempts 扫描①:RUNNING 且心跳陈旧的 attempt 候选(索引
// idx_attempt_status_heartbeat 纯区间;DB 端 NOW(3),应用时钟不入判定)。
// 覆盖扫描靶 b:asynq MaxRetry=1 穷尽 archived 后行留 RUNNING = 心跳停更。
func (r *Reaper) scanAttempts(ctx context.Context) error {
	var lastID uint64
	for batch := 0; batch < scanMaxBatch; batch++ {
		rows, err := r.deps.DB.QueryContext(ctx, `
SELECT id, node_execution_id, execution_id
  FROM node_attempt
 WHERE status='RUNNING' AND heartbeat_at < NOW(3) - INTERVAL ? MICROSECOND
   AND id > ?
 ORDER BY id
 LIMIT ?`, r.cfg.DeadGrace.Microseconds(), lastID, scanBatchSize)
		if err != nil {
			return fmt.Errorf("reaper: scan attempts: %w", err)
		}
		type cand struct {
			id, nodeExecID, execID uint64
		}
		var cands []cand
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var c cand
				if err := rows.Scan(&c.id, &c.nodeExecID, &c.execID); err != nil {
					return fmt.Errorf("reaper: scan attempts row: %w", err)
				}
				cands = append(cands, c)
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("reaper: scan attempts rows: %w", err)
			}
			return nil
		}()
		if err != nil {
			return err
		}
		if len(cands) == 0 {
			return nil
		}
		for _, c := range cands {
			lastID = c.id
			if err := r.reapAttempt(ctx, c.nodeExecID, c.execID, entryAttempt); err != nil {
				r.log.Error("reap.attempt_failed",
					"node_execution_id", c.nodeExecID, "attempt_id", c.id, "err", err)
			}
		}
		if len(cands) < scanBatchSize {
			return nil
		}
	}
	r.mBacklog(metrics.ScanAttempt)
	r.log.Warn("scan.backlog", "scanner", "attempts",
		"hint", "candidates exceeded per-tick cap; retrying next tick")
	return nil
}

// reapAttempt 处置事务外层:Transient(1213/1205)抖动重试 ≤3(照抄
// scheduler.Complete 循环形状;不重跑处置逻辑之外的东西)。
func (r *Reaper) reapAttempt(ctx context.Context, nodeExecID, execID uint64, ent entry) error {
	for attempt := 0; ; attempt++ {
		delegate, err := r.reapOnce(ctx, nodeExecID, execID, ent)
		if err == nil {
			if delegate {
				// T9.1:实例耗尽委托在 reapOnce 事务提交后执行(DEAD 收口先落地;
				// scheduler 侧自带全行锁 + 幂等守卫,失败上抛归 tick 日志)。
				if derr := r.deps.Sched.ReapInstanceExhausted(ctx, execID, nodeExecID,
					"reaper:"+r.cfg.ID); derr != nil {
					return derr
				}
			}
			return nil
		}
		if errors.Is(err, errSkip) {
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

// reapOnce 是统一处置事务(设计 C 节分派树,W1)。锁序 = node 行 →(分支 B
// 的 outbox 活事件闸,普通 SELECT 零锁)→ attempt 行 → 分支 CAS/事件 →
// wf 行(最后/FinishExecution 自身),与 Complete/心跳的全局偏序一致。
//
// 锁后复核五项(候选 SELECT 与本事务之间的 TOCTOU 全部在此吸收):
// attempt 收口态 / 心跳新鲜(无条件、与入口无关) / deadline 复活 / wf
// 当前读 / attempt 存在性三分(分支 B)。
// 返回值 delegate:本事务已收口 attempt(DEAD)且节点是耗尽的 parallel 实例,
// 节点级终败/控制器收口须在提交后委托 scheduler.ReapInstanceExhausted
// (T9.1,对抗审计 HIGH②:事务内镜像 completeOnce 会让 wait_all 退化
// fail_fast 且控制器滞留 EXECUTING;委托面自带全行锁序与同款语义)。
func (r *Reaper) reapOnce(ctx context.Context, nodeExecID, execID uint64, ent entry) (delegate bool, err error) {
	delegate = false
	err = engine.WithTx(ctx, r.deps.DB, func(q engine.DBTX) error {
		// [1] 锁 node 行。
		var (
			nodeID       string
			nodeType     string
			nodeStatus   engine.NodeState
			nodeVer      uint32
			instanceNo   uint16
			attempts     uint16
			attemptsBase uint16
			maxA         uint16
			workerID     sql.NullString
			deadline     sql.NullTime
		)
		err := q.QueryRowContext(ctx, `
SELECT node_id, type, status, version, instance_no, attempts, attempts_base, max_attempts, worker_id, deadline_at
  FROM node_execution WHERE id=? FOR UPDATE`, nodeExecID).
			Scan(&nodeID, &nodeType, &nodeStatus, &nodeVer, &instanceNo, &attempts, &attemptsBase, &maxA, &workerID, &deadline)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("reaper: node_execution %d vanished (data defect)", nodeExecID)
		}
		if err != nil {
			return fmt.Errorf("reaper: lock node %d: %w", nodeExecID, err)
		}

		// [2] 锁当前 attempt 行(uk_attempt 前缀定位:attempt_no RUNNING ⇒
		// == node.attempts,Redispatch/Complete 均先收口旧行)。
		var (
			attID     uint64
			attStatus engine.AttemptState
			heartbeat time.Time
			startedAt time.Time
			attWorker sql.NullString
		)
		err = q.QueryRowContext(ctx, `
SELECT id, status, heartbeat_at, started_at, worker_id
  FROM node_attempt WHERE node_execution_id=? AND attempt_no=? FOR UPDATE`,
			nodeExecID, attempts).Scan(&attID, &attStatus, &heartbeat, &startedAt, &attWorker)
		attemptGone := errors.Is(err, sql.ErrNoRows)
		if err != nil && !attemptGone {
			return fmt.Errorf("reaper: lock attempt node %d no %d: %w", nodeExecID, attempts, err)
		}

		// [3] 分派树。裁决时钟 = DB 端 NOW(3)(D-t25 #2:判死谓词全 DB 端,
		// 应用时钟漂移不入判定;扫描候选与锁内裁决同一时钟系)。
		var now time.Time
		if err := q.QueryRowContext(ctx, `SELECT NOW(3)`).Scan(&now); err != nil {
			return fmt.Errorf("reaper: read db clock: %w", err)
		}
		switch {
		case nodeStatus != engine.NodeExecuting:
			// 节点已收口(人工取消/终写)的遗留 RUNNING attempt:收 DEAD
			// ('stale_attempt',设计 C 节首分支)+ Touch;无行/已终态 skip。
			if !attemptGone && attStatus == engine.AttemptRunning {
				if err := engine.CloseAttemptDead(ctx, q, attID, "stale_attempt",
					fmt.Sprintf("node terminal %s, attempt left RUNNING", nodeStatus)); err != nil {
					return err
				}
				r.mReaped(metrics.CodeStaleAttempt)
				if err := r.auditRow(ctx, q, audit.EventAttemptReaped, execID, nodeExecID, attID, nodeID,
					audit.AttemptReapedPayload{
						NodeExecutionID: nodeExecID, Code: "stale_attempt", NodeStatus: string(nodeStatus),
					}); err != nil {
					return err
				}
				if err := engine.TouchExecution(ctx, q, execID); err != nil {
					return err
				}
				r.log.Warn("attempt.dead_stale_on_terminal_node",
					"node_execution_id", nodeExecID, "attempt_id", attID, "node_status", nodeStatus)
				return nil
			}
			return errSkip

		case attemptGone || attStatus != engine.AttemptRunning:
			// 分支 B:EXECUTING 节点 + 无 RUNNING attempt(claim 双败 archived /
			// 派发事件死)。
			// 活派发事件闸(防双派发;LIKE 前缀含 ':' 定界符,'att:12:%' 不匹配
			// 'att:123:x';DEAD 事件行不挡闸——重派事件经 relay 退避转 DEAD 后
			// 必须允许再重派,这是收敛路径不是异常)。
			var live int
			if err := q.QueryRowContext(ctx, `
SELECT COUNT(*) FROM outbox
 WHERE task_id LIKE CONCAT('att:', ?, ':%')
   AND (status='PENDING'
        OR (status='DISPATCHED' AND dispatched_at >= NOW(3) - INTERVAL ? MICROSECOND))`,
				nodeExecID, r.cfg.DispatchStale.Microseconds()).Scan(&live); err != nil {
				return fmt.Errorf("reaper: live event gate node %d: %w", nodeExecID, err)
			}
			if live > 0 {
				r.log.Warn("node.redispatch_deferred_live_event",
					"node_execution_id", nodeExecID, "live_events", live,
					"hint", "dispatch in flight; re-checked next tick")
				return errSkip
			}
			// → [4](无 attempt 可收)。

		case heartbeat.After(now.Add(-r.cfg.DeadGrace)):
			// 活性保护(无条件前置,与入口扫描无关——攻击 17):心跳新鲜。
			// deadline 入口三分(设计 K-5):锁内仍过期(攻击 9:复核 deadline
			// 复活)+ started_at ≥ deadline_at(迟到启动,窗口被排队吃掉)→
			// 一次性重挂不烧预算;started_at < deadline_at(窗口内启动但真
			// 超窗,worker 活也挂死)→ SLA 强制 DEAD——迟到 Complete 走 W1
			// 哨兵幂等让位,双执行安全收敛。
			if ent == entryDeadline && deadline.Valid &&
				deadline.Time.Before(now.Add(-r.cfg.ScanInterval)) {
				if !startedAt.Before(deadline.Time) {
					newDeadline := now.Add(scheduler.NodeTimeout(nodeType))
					if _, err := engine.ExtendNodeDeadline(ctx, q, nodeExecID, nodeVer, newDeadline); err != nil {
						return err
					}
					if err := r.auditRow(ctx, q, audit.EventNodeDeadlineExtended, execID, nodeExecID, 0, nodeID,
						audit.NodeDeadlineExtendedPayload{
							NodeID: nodeID, AttemptNo: attempts, DeadlineAt: newDeadline,
						}); err != nil {
						return err
					}
					if err := engine.TouchExecution(ctx, q, execID); err != nil {
						return err
					}
					r.log.Info("node.deadline_extended",
						"node_execution_id", nodeExecID, "attempt_no", attempts,
						"hint", "late start consumed window; refit once without burning budget")
					return nil
				}
				w := attWorker.String
				if w == "" {
					w = "(unknown)"
				}
				if err := engine.CloseAttemptDead(ctx, q, attID, "deadline_exceeded",
					fmt.Sprintf("first-owner worker=%s: execution window exhausted (SLA)", w)); err != nil {
					return err
				}
				r.mReaped(metrics.CodeDeadlineBurst)
				if err := r.auditRow(ctx, q, audit.EventAttemptReaped, execID, nodeExecID, attID, nodeID,
					audit.AttemptReapedPayload{
						NodeExecutionID: nodeExecID, Code: "deadline_exceeded", WorkerID: w,
					}); err != nil {
					return err
				}
				r.log.Warn("attempt.reaped_sla",
					"node_execution_id", nodeExecID, "attempt_id", attID, "worker", w)
				// → [4]。
				break
			}
			return errSkip

		default:
			// 心跳陈旧:判死。收口码由锁内实测谓词决定(攻击 9):deadline
			// 亦过期 → deadline_exceeded,否则 heartbeat_lost(分码可辨,
			// T6.4 聚合)。msg 携带行内 worker_id(注明首任,D-t25 #8)。
			code := "heartbeat_lost"
			if deadline.Valid && deadline.Time.Before(now.Add(-r.cfg.ScanInterval)) {
				code = "deadline_exceeded"
			}
			stale := now.Sub(heartbeat).Truncate(time.Second)
			w := attWorker.String
			if w == "" {
				w = "(unknown)"
			}
			if err := engine.CloseAttemptDead(ctx, q, attID, code,
				fmt.Sprintf("first-owner worker=%s stale=%s", w, stale)); err != nil {
				return err
			}
			r.mReaped(code)
			if err := r.auditRow(ctx, q, audit.EventAttemptReaped, execID, nodeExecID, attID, nodeID,
				audit.AttemptReapedPayload{
					NodeExecutionID: nodeExecID, Code: code, WorkerID: w, Stale: stale.String(),
				}); err != nil {
				return err
			}
			r.log.Warn("attempt.reaped",
				"node_execution_id", nodeExecID, "attempt_id", attID,
				"code", code, "worker", w, "stale", stale.String())
			// → [4]。
		}

		// [4] 锁 wf 行(最后,当前读)裁决终局。
		var wfStatus engine.WorkflowState
		if err := q.QueryRowContext(ctx,
			`SELECT status FROM workflow_execution WHERE id=? FOR UPDATE`, execID).
			Scan(&wfStatus); err != nil {
			return fmt.Errorf("reaper: lock execution %d: %w", execID, err)
		}
		switch {
		case wfStatus.Terminal():
			// 靶 a:fail-fast 遗留 EXECUTING → CANCELLED(平台裁决,不伪造
			// 业务失败;version 已变/已终态的 Conflict 是良性收敛)。
			cancelVer, err := engine.CancelNode(ctx, q, nodeExecID, nodeVer)
			if err != nil {
				if errors.Is(err, engine.ErrConflict) {
					return errSkip
				}
				return err
			}
			r.mNodeCancelled()
			if err := r.auditRow(ctx, q, audit.EventNodeStateChanged, execID, nodeExecID, attID, nodeID,
				audit.NodeStateChangedPayload{
					From: string(nodeStatus), To: string(engine.NodeCancelled),
					NodeID: nodeID, Version: cancelVer, AttemptNo: attempts, WorkerID: workerID.String,
					InstanceNo: instanceNo,
				}); err != nil {
				return err
			}
			r.log.Info("node.cancelled_orphan",
				"node_execution_id", nodeExecID, "execution_id", execID, "wf_status", wfStatus)
			return nil
		case wfStatus == engine.WorkflowWaitingApproval:
			// T4.1 契约:挂起须 attempt 已收口——EXECUTING+挂起 = 数据缺陷,
			// 响亮 warn 不改写(WAITING_APPROVAL 归审批流管,T4.3)。
			r.log.Error("node.orphan_on_waiting_approval",
				"node_execution_id", nodeExecID, "execution_id", execID,
				"hint", "EXECUTING node under WAITING_APPROVAL violates T4.1 contract")
			return nil
		case instanceNo > 0 && int(attempts)-int(attemptsBase) >= int(maxA):
			// T9.2:预算 = 本轮口径(attempts-attempts_base,回滚后满额)。
			// T9.1:parallel 实例行耗尽——attempt DEAD 收口已在本事务,节点级
			// 终败(fail_fast 收敛控制器 / wait_all 收齐再判)提交后委托
			// scheduler(全行锁序 + 与 completeOnce 同款语义)。直接走下方
			// default 会把 wait_all 塌缩为 fail-fast、控制器滞留 deadline=NULL
			// 的 EXECUTING(对抗审计 HIGH②)。
			delegate = true
			return nil
		case int(attempts)-int(attemptsBase) < int(maxA):
			// 预算内重派:RedispatchNode(平台重试 claim 即本语句)+ 同事务
			// outbox 事件(新 attempt_no ⇒ 新 TaskID,与 asynq 滞留旧副本零冲突)。
			if _, err := engine.RedispatchNode(ctx, q, nodeExecID, nodeVer, engine.RedispatchInput{
				WorkerID:   "reaper:" + r.cfg.ID,
				DeadlineAt: now.Add(scheduler.NodeTimeout(nodeType)),
			}); err != nil {
				return err
			}
			r.mRedispatched(metrics.TriggerReaped)
			if err := r.auditRow(ctx, q, audit.EventNodeRedispatched, execID, nodeExecID, 0, nodeID,
				audit.NodeRedispatchedPayload{
					NodeID: nodeID, AttemptNo: attempts + 1,
					Trigger: audit.TriggerReaped, MaxAttempts: maxA, InstanceNo: instanceNo,
				}); err != nil {
				return err
			}
			if err := r.appendRedispatchEvent(ctx, q, execID, nodeExecID, attempts+1); err != nil {
				return err
			}
			if err := engine.TouchExecution(ctx, q, execID); err != nil {
				return err
			}
			r.log.Info("node.redispatched",
				"node_execution_id", nodeExecID, "execution_id", execID,
				"attempt_no", attempts+1, "max_attempts", maxA)
			return nil
		default:
			// 耗尽:fail-fast 同事务镜像(FailNode + FinishExecution(FAILED)),
			// 与 completeOnce 的 default 分支逐字同式。
			failVer, err := engine.FailNode(ctx, q, nodeExecID, nodeVer, workerID.String,
				"attempts_exhausted",
				fmt.Sprintf("reaped after %d attempts", maxA))
			if err != nil {
				return err
			}
			r.mNodeFailed()
			if err := r.auditRow(ctx, q, audit.EventNodeStateChanged, execID, nodeExecID, attID, nodeID,
				audit.NodeStateChangedPayload{
					From: string(nodeStatus), To: string(engine.NodeFailed),
					NodeID: nodeID, Version: failVer, Code: "attempts_exhausted",
					AttemptNo: attempts, WorkerID: workerID.String,
				}); err != nil {
				return err
			}
			if err := engine.FinishExecution(ctx, q, execID, engine.WorkflowFailed, nil,
				"node_failed",
				fmt.Sprintf("node %d failed: attempts_exhausted (reaper)", nodeExecID)); err != nil {
				// 双收口由 status-only CAS 仲裁:他方已终态 = 良性收敛。
				if errors.Is(err, engine.ErrConflict) {
					return nil
				}
				return err
			}
			r.log.Warn("execution.failed_by_reaper",
				"node_execution_id", nodeExecID, "execution_id", execID, "attempts", maxA)
			return nil
		}
	})
	return delegate, err
}

// appendRedispatchEvent 落重派事件(与 scheduler.appendNodeTask 同构;
// reaper 自带雪花 ID,不 import scheduler 私有面)。原行 trace 自全状态
// MAX 带出(redispatchResume 同式,T8.1:重放 = 原事件同一真相,重执行段
// 不换主链;本节点无任何带 trace 原行时空 = 零注入,不伪造段链)。LIKE
// 前缀含 ':' 定界符,与活闸同式;本行尚未 INSERT,MAX 不含自身。
func (r *Reaper) appendRedispatchEvent(ctx context.Context, q engine.DBTX,
	execID, nodeExecID uint64, attemptNo uint16) error {
	var origTrace sql.NullString
	if err := q.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT MAX(trace_id) FROM outbox
		                  WHERE task_id LIKE CONCAT('att:', ?, ':%')), '')`,
		nodeExecID).Scan(&origTrace); err != nil {
		return fmt.Errorf("reaper: redispatch trace node %d: %w", nodeExecID, err)
	}
	raw, err := json.Marshal(scheduler.NodeTaskPayload{
		ExecutionID: execID, NodeExecutionID: nodeExecID, AttemptNo: attemptNo,
		TraceID: origTrace.String,
	})
	if err != nil {
		return fmt.Errorf("reaper: marshal redispatch payload: %w", err)
	}
	id, err := r.deps.IDs.Next()
	if err != nil {
		return fmt.Errorf("reaper: gen event id: %w", err)
	}
	return outbox.Append(ctx, q, outbox.Event{
		ID: id,
		// TraceID 显式拷原行值(同 redispatchResume:applyCtxTrace 显式优先;
		// reaper tick ctx 无 span,不显式则重执行段换新链)。
		TraceID:     origTrace.String,
		ExecutionID: execID,
		EventType:   scheduler.EventTypeNodeTask,
		TaskID:      fmt.Sprintf("att:%d:%d", nodeExecID, attemptNo),
		Queue:       r.cfg.Queue,
		Payload:     raw,
	})
}

// auditRow 落一条审计行(T2.6 契约:reaper 裁决同事务必有审计行,
// D-2026-09-23-t26-audit-events)。reaper 的裁决 actor = system,归因到实例
// (actor_id=reaper:<id>,与 RedispatchNode 的 worker_id 同形)。
func (r *Reaper) auditRow(ctx context.Context, q engine.DBTX, eventType string,
	execID, nodeExecID, attemptID uint64, nodeID string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("reaper: marshal audit %s payload: %w", eventType, err)
	}
	id, err := r.deps.IDs.Next()
	if err != nil {
		return fmt.Errorf("reaper: gen audit id: %w", err)
	}
	return audit.Append(ctx, q, audit.Event{
		ID: id, EventType: eventType, ExecutionID: execID,
		NodeExecID: nodeExecID, NodeID: nodeID, AttemptID: attemptID,
		ActorType: audit.ActorSystem, ActorID: "reaper:" + r.cfg.ID, Payload: raw,
	})
}
