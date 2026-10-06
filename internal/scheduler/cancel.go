package scheduler

// T19.3 执行级取消(D-2026-10-05-t193-execution-cancel-and-rollback-whitelist
// 决策 2):RUNNING/WA 执行的用户主动楔停。楔源预扫 = lockAllRows 当前读裁决
// 全部非终态行(控制行+实例行,PENDING 冻结含),同事务逐行 CancelNode +
// RUNNING attempt 同事务 CloseAttemptDead + FinishExecution(CANCELLED)——
// 单事务原子,零「全终态但 wf 未收口」被 finishIfDone/reaper ④b 误判
// SUCCEEDED 的窗口。在飞 worker 的迟到收口 = attempt DEAD 幂等哨兵 + 终态
// 守卫跳推进(completeOnce 既有收敛,零新面)。
//
// 事务形状与 RollbackTo 同构(锁序 node 全行集→attempt→wf 最后;容量闸与
// 回滚共闸 BRAID_ROLLBACK_MAX_ROWS;瞬态重试环同款)。与回滚守卫②的关键
// 分立:取消语义就是楔停在飞(EXECUTING→CANCELLED,CAS 版本变 ⇒ 在飞收口
// 必败 = 状态机定义内收敛),不等 reaper settle——这是 T19.6 在飞回滚
// quiesce 协议的前置原语。

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
)

// CancelErrKind 是取消业务拒绝面(api 4xx 映射真源;infra 错误走裸 error)。
type CancelErrKind string

const (
	// CancelNotCancellable:wf ∉ {RUNNING, WAITING_APPROVAL}——终态执行
	// 不可取消只可回滚;PENDING(Start 积压窗)不受理,理由与边界见
	// cancelOnce 步骤 2 注释(对抗审计 C1/C2);并发双取消败者也落此面
	//(全行锁串行 + 守卫 FOR UPDATE 当前读,幂等重取消报错非重放,镜像
	// RestartExecution 语义)。
	CancelNotCancellable CancelErrKind = "not_cancellable"
	// CancelTooLarge:全行集行数超 Deps.MaxRollbackRows——与回滚共闸
	//(取消是全行锁事务,锁面随行数线性增长,同一容量护栏单源;「0」= 关闭
	// 同语义)。api 409 + denied 审计同分面。
	CancelTooLarge CancelErrKind = "too_large"
)

// CancelError 是取消拒绝的结构化载荷(api 按 Kind 映射状态码;审计
// cancel.denied 同源消费)。
type CancelError struct {
	Kind   CancelErrKind
	Detail string
}

func (e *CancelError) Error() string {
	return "scheduler: cancel " + string(e.Kind) + ": " + e.Detail
}

// CancelParams 是 CancelExecution 的入参。
type CancelParams struct {
	ExecID   uint64
	WorkerID string // 楔停归因(attempt 收口 code 语境;api 侧 api:cancel 形态)
	ActorID  string // 操作者主体(审计 actor_id;api 侧 JWT sub)
}

// CancelResult 是取消成功的事务结果(响应体与审计 payload 同源)。
type CancelResult struct {
	FromStatus     engine.WorkflowState
	CancelledCount int // CancelNode 命中行数(控制行+实例行;0 = 全终态滞留收口)
}

// CancelExecution 单事务楔停执行:容量闸 → 全行锁 → 受理面守卫(RUNNING/
// WA only,PENDING 不受理——Start 锁序例外面,见 cancelOnce 步骤 2)→ 楔源
// 预扫(RUNNING attempt 先收口,镜像回滚步骤 8→9 两段式)→ 逐行 CancelNode
// → wf 收口(最后)→ 审计。提交后零镜像检查(wf 已终态,finishIfDone 不再
// 可达;reaper ④b 的 CAS 也只会良性 Conflict)。
func (s *Scheduler) CancelExecution(ctx context.Context, p CancelParams) (*CancelResult, error) {
	if p.ExecID == 0 || p.WorkerID == "" || p.ActorID == "" {
		return nil, errors.New("scheduler: cancel zero exec id / empty worker or actor")
	}
	exec, err := s.loadExecution(ctx, p.ExecID)
	if err != nil {
		return nil, err
	}
	// lockAllRows 需要 g(node_id 升序锁面);wf DSL 本体不消费(取消不求值)。
	_, g, err := s.loadWorkflow(ctx, exec.definitionID)
	if err != nil {
		return nil, err
	}
	var res *CancelResult
	err = s.cancelWithRetry(ctx, p, g, &res)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// cancelWithRetry 套 1213/1205 瞬态抖动重试环(rollbackWithRetry 同款:
// 全行锁窗口最长的写事务族;守卫拒绝面 *CancelError 非 Transient 直通)。
func (s *Scheduler) cancelWithRetry(ctx context.Context, p CancelParams,
	g *dag, out **CancelResult) error {
	for attempt := 0; ; attempt++ {
		res, err := s.cancelOnce(ctx, p, g)
		if err == nil {
			*out = res
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

func (s *Scheduler) cancelOnce(ctx context.Context, p CancelParams, g *dag) (*CancelResult, error) {
	var res *CancelResult
	err := engine.WithTx(ctx, s.deps.DB, func(q engine.DBTX) error {
		// 0. 容量闸(与回滚共闸,rollbackOnce 步骤 0 同款:COUNT 走
		// uk_node_exec 索引范围扫,普通读足够——闸是容量面非正确性面;
		// 0 = 关闭)。
		if max := s.deps.MaxRollbackRows; max > 0 {
			var rowCnt int
			if err := q.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM node_execution WHERE execution_id=?`, p.ExecID).Scan(&rowCnt); err != nil {
				return fmt.Errorf("scheduler: cancel count rows %d: %w", p.ExecID, err)
			}
			if uint64(rowCnt) > uint64(max) {
				return &CancelError{Kind: CancelTooLarge,
					Detail: fmt.Sprintf("execution has %d node rows, cancel cap %d (lock-window guard)", rowCnt, max)}
			}
		}

		// 1. 全行锁(node-first 锁序铁律;当前读 = 楔源预扫的裁决基础)。
		rows, err := lockAllRows(ctx, q, p.ExecID, g)
		if err != nil {
			return err
		}

		// 2. 受理面:wf ∈ {RUNNING, WAITING_APPROVAL}。FOR UPDATE 当前读
		//(对抗审计 C1 修):容量闸的 COUNT 是本事务首个一致读,已在闸时刻
		// 建立 RR read view——其后的普通 SELECT 会回放过期快照(镜像
		// checkWfCAS 的 FOR UPDATE 纪律);wf 行锁在 node 全行集之后获取,
		// 锁序铁律(node→attempt→wf 最后)保持。PENDING 不受理(对抗审计
		// C2 拍板):Start 例外先拿 wf 行锁(sched.go 包级备忘的前提 =
		// 「PENDING 执行无并发 node 行锁持有者」),取消若在 Start 积压窗
		// 内楔停 PENDING 执行,将以 node 全行锁 × Start 的 wf 锁成 AB-BA;
		// 未起跑执行的取消等 Start 后再发(幂等重试面)。终裁兜底在
		// FinishExecution 的 status-only CAS。
		var wfStatus engine.WorkflowState
		var rootTrace sql.NullString
		if err := q.QueryRowContext(ctx,
			`SELECT status, trace_id FROM workflow_execution WHERE id=? FOR UPDATE`, p.ExecID).
			Scan(&wfStatus, &rootTrace); err != nil {
			return fmt.Errorf("scheduler: cancel read execution status %d: %w", p.ExecID, err)
		}
		if wfStatus != engine.WorkflowRunning && wfStatus != engine.WorkflowWaitingApproval {
			return &CancelError{Kind: CancelNotCancellable,
				Detail: fmt.Sprintf("execution status %s (cancel accepts RUNNING/WAITING_APPROVAL; terminal executions are rollback-only, PENDING must start first)", wfStatus)}
		}

		// 3. 楔源 = 全部非终态行。先收口 RUNNING attempt(锁序 node→attempt,
		// 回滚步骤 8 同款:在飞 worker 的迟到 Complete 撞 DEAD ⇒ 幂等哨兵;
		// asynq 滞留未认领副本由 worker.claimAttempt 活态闸拦,T19.3 决策 4)。
		// 楔面外(已终态行)零动作——其上的遗留 RUNNING attempt 是数据缺陷
		// 形态,归 reaper 扫描③既有兜底,取消不越楔面收编。msg 携 WorkerID
		// = CancelParams 归因落点(对抗审计 L5:参数必须物化)。
		rq, ok := q.(engine.RowsDBTX)
		if !ok {
			return fmt.Errorf("scheduler: DBTX lacks the multi-row facet (engine.RowsDBTX)")
		}
		var wedge []*nodeRow // 楔源集(非终态行,控制行+实例行)
		for _, id := range sortedNodes(g.nodes) {
			rowSet := append([]*nodeRow{rows.byNodeID[id]}, rows.instances[id]...)
			for _, r := range rowSet {
				if r.status.Terminal() {
					continue
				}
				wedge = append(wedge, r)
			}
		}
		for _, r := range wedge {
			rs, err := rq.QueryContext(ctx,
				`SELECT id FROM node_attempt WHERE node_execution_id=? AND status='RUNNING' FOR UPDATE`, r.id)
			if err != nil {
				return fmt.Errorf("scheduler: cancel lock attempts node %s: %w", r.nodeID, err)
			}
			var attIDs []uint64
			for rs.Next() {
				var attID uint64
				if err := rs.Scan(&attID); err != nil {
					_ = rs.Close()
					return fmt.Errorf("scheduler: cancel scan attempt node %s: %w", r.nodeID, err)
				}
				attIDs = append(attIDs, attID)
			}
			if err := rs.Err(); err != nil {
				_ = rs.Close()
				return fmt.Errorf("scheduler: cancel attempts node %s: %w", r.nodeID, err)
			}
			if err := rs.Close(); err != nil {
				return fmt.Errorf("scheduler: cancel close attempts node %s: %w", r.nodeID, err)
			}
			for _, attID := range attIDs {
				if err := engine.CloseAttemptDead(ctx, q, attID,
					"execution_cancelled",
					fmt.Sprintf("execution cancelled by user (%s)", p.WorkerID)); err != nil {
					return fmt.Errorf("scheduler: cancel close attempt %d: %w", attID, err)
				}
			}
		}

		// 4. 逐行楔停:CancelNode(任意非终态→CANCELLED,attempts 不动——
		// 轮次单调保持)+ 节点审计行(Enqueue 暂存,stageRollbackTransition
		// 是回滚/取消共用的节点级 user 过渡审计单点)。
		cancelled := 0
		for _, r := range wedge {
			from := r.status
			newVer, err := engine.CancelNode(ctx, q, r.id, r.version)
			if err != nil {
				return err
			}
			if err := stageRollbackTransition(ctx, q, s, p.ExecID, p.ActorID, r, from, engine.NodeCancelled, newVer, r.attempts); err != nil {
				return err
			}
			r.status = engine.NodeCancelled
			r.version = newVer
			cancelled++
		}

		// 5. wf 收口(最后;froms={PENDING,RUNNING,WAITING_APPROVAL} 天然
		// 覆盖受理面)。CANCELLED 不携带 outputs/error(列纪律),取消理由
		// 归本审计行。守卫后不可达的 Conflict 由 CAS 兜底上抛(api 409)。
		if err := engine.FinishExecution(ctx, q, p.ExecID, engine.WorkflowCancelled, nil, "", ""); err != nil {
			return err
		}

		// 6. 审计:execution.cancelled(低频运维面,同步直写;actor=user)。
		id, err := s.deps.IDs.Next()
		if err != nil {
			return fmt.Errorf("scheduler: gen cancel audit id: %w", err)
		}
		raw, err := json.Marshal(audit.ExecutionCancelledPayload{
			FromStatus:     string(wfStatus),
			CancelledCount: cancelled,
		})
		if err != nil {
			return fmt.Errorf("scheduler: marshal cancel audit payload: %w", err)
		}
		if err := audit.Append(ctx, q, audit.Event{
			ID: id, EventType: audit.EventExecutionCancelled, ExecutionID: p.ExecID,
			ActorType: audit.ActorUser, ActorID: p.ActorID, Payload: raw,
			LinkedTraceID: rootTrace.String, // T18.5 ⑦:段链 Z 指回执行 root X(回滚 rolled_back 同判)
		}); err != nil {
			return err
		}
		res = &CancelResult{FromStatus: wfStatus, CancelledCount: cancelled}
		return engine.TouchExecution(ctx, q, p.ExecID)
	})
	return res, err
}
