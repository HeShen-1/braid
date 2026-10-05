package scheduler

// T9.2 回滚/断点续跑(D-2026-09-28-t92-rollback-same-branch):同 execution
// 分支——目标节点 RerunNode 快照读回重跑 + 闭包 ResetNodeToPending 重置,
// 失败源自动收编(闭包外仅允许 SUCCEEDED/SKIPPED,终态可达结构性保证)。
// 通道 = api 同步事务(approval「等用户输入」先例动机不适用;Instantiate
// 直调先例),不经 outbox ⇒ 零新词表/零 worker handler/零队列配置。
// 锁序铁律:lockAllRows(node 全行集)→ node_attempt(RUNNING 收口)→
// workflow_execution(RestartExecution 最后)——与 reaper reapOnce 同序,无 AB-BA。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strconv"
	"time"

	"braid/internal/audit"
	"braid/internal/engine"
	"braid/internal/engine/dsl"
)

// RollbackErrKind 是回滚业务拒绝面(api 4xx 映射的真源;infra 错误走裸 error)。
type RollbackErrKind string

const (
	// RollbackNotRollbackable:wf 非终态白名单(RUNNING/WA/PENDING;白名单
	// = {FAILED, SUCCEEDED, CANCELLED},T19.3 D-2026-10-05-t193 决策 3 自
	// D-t92 决策 4「仅 FAILED」扩列;并发双回滚败者也落此面——
	// RestartExecution CAS 0 行)。
	RollbackNotRollbackable RollbackErrKind = "not_rollbackable"
	// RollbackBlockedInflight:全行集含 EXECUTING/WAITING_APPROVAL 残留
	//(fail-fast 不等兄弟 + orphanQuiet 静默窗,D-t92 决策 4②:409 等 reaper
	// settle,不做事务内收口)。Blocked 携带节点清单引导运维。
	RollbackBlockedInflight RollbackErrKind = "blocked_inflight"
	// RollbackBadNode:目标不在钉住 DSL,或前驱锥存在闭包外 SKIPPED
	//(剪枝传播必将目标静默剪掉,正确落点 = branch 节点)。
	RollbackBadNode RollbackErrKind = "bad_node_id"
	// RollbackNoTarget:自动定位空集(三档退化链全落空——FAILED 行、
	// CANCELLED 楔源行[T19.3]、前驱就绪的 PENDING 冻结行;典型 =
	// input_eval 型终败或 SUCCEEDED 执行走 resume,resume 面的显式分面)。
	RollbackNoTarget RollbackErrKind = "no_resume_target"
	// RollbackTooLarge:全行集行数超 Deps.MaxRollbackRows(lockAllRows 的
	// 锁面随行数线性增长,D-t24#6 全行锁)——容量闸拒绝面,T15.5 实测定档
	// (api 409 + denied 审计同分面)。
	RollbackTooLarge RollbackErrKind = "too_large"
)

// DefaultMaxRollbackRows 是 MaxRollbackRowsFromEnv 的缺省档:T15.5 压测定档
// 值(docs/CAPACITY.md 锁窗曲线的 SLA 交点),生产装配缺省即受闸保护。
const DefaultMaxRollbackRows uint32 = 4000

// MaxRollbackRowsFromEnv 读 BRAID_ROLLBACK_MAX_ROWS(空 = DefaultMaxRollbackRows,
// "0" = 显式关闭闸;cmd 层 env 缝,tracing.ConfigFromEnv 先例)。坏值启动
// 即败:静默回退默认会把运维的显式配置错误吞成未知档位。
func MaxRollbackRowsFromEnv(getenv func(string) string) (uint32, error) {
	raw := getenv("BRAID_ROLLBACK_MAX_ROWS")
	if raw == "" {
		return DefaultMaxRollbackRows, nil
	}
	n, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("scheduler: BRAID_ROLLBACK_MAX_ROWS=%q: want 0..4294967295 (0 disables the cap)", raw)
	}
	return uint32(n), nil
}

// RollbackError 是回滚拒绝的结构化载荷(api 按 Kind 映射状态码,Detail/Blocked
// 进响应体;审计 rollback.denied 同源消费)。
type RollbackError struct {
	Kind    RollbackErrKind
	Detail  string
	Blocked []string // blocked_inflight 专属:node_id(:instance_no) 清单
}

func (e *RollbackError) Error() string {
	if len(e.Blocked) > 0 {
		return fmt.Sprintf("scheduler: rollback %s: %s: blocked %v", e.Kind, e.Detail, e.Blocked)
	}
	return fmt.Sprintf("scheduler: rollback %s: %s", e.Kind, e.Detail)
}

// RollbackParams 是 RollbackTo 的入参。
type RollbackParams struct {
	ExecID       uint64
	TargetNodeID string // "" = 自动定位(resume 语法糖):字典序首个 FAILED → 字典序首个 CANCELLED(T19.3 楔源档)→ 前驱全就绪的 PENDING 冻结行
	WorkerID     string // 派发归因(RerunNode worker_id + att 事件;api 侧建议 api:rollback:<exec> 形态)
	ActorID      string // 操作者主体(审计 actor_id;api 侧 JWT sub)
}

// RollbackResult 是回滚成功的事务结果(响应体与审计 payload 同源)。
type RollbackResult struct {
	TargetNodeID string
	RerunTarget  bool // true = RerunNode 快照读回重跑;false = 重评估(advance 重派)
	ResetCount   int  // ResetNodeToPending 命中行数(含实例行)
}

// RollbackTo 单事务执行回滚:守卫链 → 闭包计算(自动收编)→ RUNNING attempt
// 同事务收口 → 闭包重置 → 目标重跑分流 → 执行复活 → advance 全图重派。
// 提交后镜像 finishIfDone(全内联闭包立即终局,completeOnce 同款)。
func (s *Scheduler) RollbackTo(ctx context.Context, p RollbackParams) (*RollbackResult, error) {
	if p.ExecID == 0 || p.WorkerID == "" || p.ActorID == "" {
		return nil, errors.New("scheduler: rollback zero exec id / empty worker or actor")
	}
	exec, err := s.loadExecution(ctx, p.ExecID)
	if err != nil {
		return nil, err
	}
	wf, g, err := s.loadWorkflow(ctx, exec.definitionID)
	if err != nil {
		return nil, err
	}
	var res *RollbackResult
	err = s.rollbackWithRetry(ctx, p, wf, g, exec.input, &res)
	if err != nil {
		return nil, err
	}
	// 提交后新事务判终态:与 completeOnce 尾部同款(失败只记日志——行集全
	// 终态的滞留由 reaper ④b FinishIfDone 兜底,回滚本身已原子生效)。
	if ferr := s.finishIfDone(ctx, p.ExecID, wf, exec.input); ferr != nil {
		s.log.Error("rollback.finish_check_failed", "execution_id", p.ExecID, "err", ferr)
	}
	return res, nil
}

// rollbackWithRetry 套 1213/1205 瞬态抖动重试环(对抗审计 LOW 修:回滚是
// 全库锁窗口最长的写事务,却是唯一无该环的事务入口——Complete/Resume/
// Suspend/reapAttempt 四入口同款形态;守卫拒绝面 *RollbackError 非 Transient
// 自然直通)。
func (s *Scheduler) rollbackWithRetry(ctx context.Context, p RollbackParams,
	wf *dsl.Workflow, g *dag, wfInput json.RawMessage, out **RollbackResult) error {
	for attempt := 0; ; attempt++ {
		res, err := s.rollbackOnce(ctx, p, wf, g, wfInput)
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

func (s *Scheduler) rollbackOnce(ctx context.Context, p RollbackParams,
	wf *dsl.Workflow, g *dag, wfInput json.RawMessage) (*RollbackResult, error) {
	var res *RollbackResult
	err := engine.WithTx(ctx, s.deps.DB, func(q engine.DBTX) error {
		// 0. 容量闸(T15.5,D-t92 停车场 MaxRollbackRows 兑现):COUNT 走
		// uk_node_exec 索引范围扫;普通读足够——闸是容量面非正确性面,锁定前
		// 拒绝零副作用。T19.2(D-2026-10-05-t192)后行集不再「Instantiate 后
		// 固定」:动态扇出的实例行在派发位增长,锁前 COUNT 可能低计并发扇出
		// ——持锁后(lockAllRows 当前读)锁定集仍准确,闸保护的是实际锁定行
		// 数,语义不变;每并行节点行数上界仍受 MaxParallelInstances 封顶。
		// 0 = 关闭(测试缺省)。
		if max := s.deps.MaxRollbackRows; max > 0 {
			var rowCnt int
			if err := q.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM node_execution WHERE execution_id=?`, p.ExecID).Scan(&rowCnt); err != nil {
				return fmt.Errorf("scheduler: rollback count rows %d: %w", p.ExecID, err)
			}
			if uint64(rowCnt) > uint64(max) { // 无符号比较:32 位平台 int(max) 可溢出为负翻转闸向
				return &RollbackError{Kind: RollbackTooLarge,
					Detail: fmt.Sprintf("execution has %d node rows, rollback cap %d (lock-window guard)", rowCnt, max)}
			}
		}

		// 1. 全行锁(node-first 锁序铁律;当前读 = 守卫与重置的裁决基础)。
		rows, err := lockAllRows(ctx, q, p.ExecID, g)
		if err != nil {
			return err
		}

		// 2. 守卫①:wf ∈ 终态白名单 {FAILED, SUCCEEDED, CANCELLED}
		//(T19.3 D-2026-10-05-t193 决策 3;D-t92 决策 4 原口径「仅 FAILED」
		// 随 SUCCEEDED/CANCELLED 放行扩列)。普通读即可——终裁在
		// RestartExecution 的 status-only CAS(并发双回滚败者在此收敛 409)。
		var wfStatus engine.WorkflowState
		if err := q.QueryRowContext(ctx,
			`SELECT status FROM workflow_execution WHERE id=?`, p.ExecID).Scan(&wfStatus); err != nil {
			return fmt.Errorf("scheduler: rollback read execution status %d: %w", p.ExecID, err)
		}
		if !wfStatus.Terminal() {
			return &RollbackError{Kind: RollbackNotRollbackable,
				Detail: fmt.Sprintf("execution status %s (rollback whitelist: FAILED, SUCCEEDED, CANCELLED)", wfStatus)}
		}

		// 3. 守卫②:全行集(控制行+实例行)零 EXECUTING/WAITING_APPROVAL。
		var blocked []string
		for _, id := range sortedNodes(g.nodes) {
			rowSet := append([]*nodeRow{rows.byNodeID[id]}, rows.instances[id]...)
			for _, r := range rowSet {
				if r.status == engine.NodeExecuting || r.status == engine.NodeWaitingApproval {
					blocked = append(blocked, fmt.Sprintf("%s:%d", r.nodeID, r.instanceNo))
				}
			}
		}
		if len(blocked) > 0 {
			sort.Strings(blocked)
			return &RollbackError{Kind: RollbackBlockedInflight,
				Detail:  "in-flight residue; wait for reaper settle (orphanQuiet) and retry",
				Blocked: blocked}
		}

		// 4. 目标解析:显式(守卫③ ∈ 钉住 DSL)或自动(字典序首个 FAILED →
		// 字典序首个 CANCELLED[T19.3 楔源档:取消楔住的恢复点,修复失败源
		// 优先于恢复取消点]→ 前驱全就绪的 PENDING 冻结行——input_eval 型
		// 终败的 resume 面)。SUCCEEDED 执行三档全空 ⇒ no_resume_target,
		// 只走 rollback 端点显式 target(「重跑哪个成功节点」无可推断语义)。
		target := p.TargetNodeID
		if target == "" {
			for _, id := range sortedNodes(g.nodes) {
				if rows.byNodeID[id].status == engine.NodeFailed {
					target = id
					break
				}
			}
		}
		if target == "" {
			for _, id := range sortedNodes(g.nodes) {
				if rows.byNodeID[id].status == engine.NodeCancelled {
					target = id
					break
				}
			}
		}
		if target == "" {
			for _, id := range sortedNodes(g.nodes) {
				row := rows.byNodeID[id]
				if row.status != engine.NodePending {
					continue
				}
				ready := true
				for _, pred := range g.preds[id] {
					switch rows.byNodeID[pred].status {
					case engine.NodeSucceeded, engine.NodeSkipped:
					default:
						ready = false
					}
				}
				if ready {
					target = id
					break
				}
			}
		}
		if target == "" {
			return &RollbackError{Kind: RollbackNoTarget,
				Detail: "no FAILED node and no ready PENDING node to resume"}
		}
		if _, ok := g.nodes[target]; !ok {
			return &RollbackError{Kind: RollbackBadNode,
				Detail: fmt.Sprintf("node %q not in pinned DSL", target)}
		}

		// 5. 闭包 = downstream(target) ∪ 全部非 {SUCCEEDED,SKIPPED} 行(失败源
		// 收编,D-t92 决策 4)∪ downstream(收编源)。收编保证重置后闭包外仅
		// SUCCEEDED/SKIPPED ⇒ finishIfDone(failed>0 恒 no-op)终态可达。
		closure := map[string]bool{}
		var addDown func(id string)
		addDown = func(id string) {
			if closure[id] {
				return
			}
			closure[id] = true
			for _, u := range g.succs[id] {
				addDown(u)
			}
		}
		addDown(target)
		for _, id := range sortedNodes(g.nodes) {
			st := rows.byNodeID[id].status
			if st != engine.NodeSucceeded && st != engine.NodeSkipped {
				addDown(id)
			}
		}

		// 6. 守卫④:目标前驱锥存在闭包外 SKIPPED(闭包内的 SKIPPED 已被重置
		// 重评估——路由可能改变,是收编语义的一部分;闭包外的将把目标静默剪掉)。
		ancestors := map[string]bool{}
		stack := append([]string(nil), g.preds[target]...)
		for len(stack) > 0 {
			a := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if ancestors[a] {
				continue
			}
			ancestors[a] = true
			stack = append(stack, g.preds[a]...)
		}
		for a := range ancestors {
			if rows.byNodeID[a].status == engine.NodeSkipped && !closure[a] {
				return &RollbackError{Kind: RollbackBadNode,
					Detail: fmt.Sprintf("node %q is under a pruned branch (skipped ancestor %q); rollback the branch node instead", target, a)}
			}
		}

		// 7. 目标重跑分流:worker 可执行类型(llm/tool/approval)且旧轮快照
		// 在 ⇒ RerunNode 读回重跑(「以 input_snapshot 重跑」逐字兑现);
		// branch/parallel(调度器内联)与快照已灭失的行 ⇒ 走闭包重置,由
		// advance 重派(新鲜求值/重新路由)。快照读取先于重置(Reset 清列)。
		ctrl := rows.byNodeID[target]
		rerun := false
		var snap []byte
		if ctrl.status.Terminal() && isWorkerExecType(ctrl.nodeType) {
			if err := q.QueryRowContext(ctx,
				`SELECT input_snapshot FROM node_execution WHERE id=?`, ctrl.id).Scan(&snap); err != nil {
				return fmt.Errorf("scheduler: rollback read target snapshot %d: %w", ctrl.id, err)
			}
			rerun = snap != nil
		}

		// 8. RUNNING attempt 同事务收口(锁序 node→attempt;两拍窗 scan③→
		// scan① 的遗留行,D-t92 决策 4⑤)。「事件已入 asynq 但 worker 未认领
		// (attempt 行未物化)」的旧轮积压任务不在本面拦——其命中时事件必然
		// 已过 DispatchStale 窗(活事件闸结构性错过目标),结构性封口在
		// worker.claimAttempt 的轮次校验(attempt_no ≤ attempts_base ⇒ 幂等
		// ack 零物化零执行,对抗审计 HIGH 修的采纳形态);fresh 窗内拦反而
		// 把「刚失败就回滚」误伤成 30 分钟 409。多行枚举走 RowsDBTX 面
		// (lockAllRows 同款)。
		rq, ok := q.(engine.RowsDBTX)
		if !ok {
			return fmt.Errorf("scheduler: DBTX lacks the multi-row facet (engine.RowsDBTX)")
		}
		for _, id := range sortedNodes(g.nodes) {
			if !closure[id] {
				continue
			}
			rowSet := append([]*nodeRow{rows.byNodeID[id]}, rows.instances[id]...)
			for _, r := range rowSet {
				rs, err := rq.QueryContext(ctx,
					`SELECT id FROM node_attempt WHERE node_execution_id=? AND status='RUNNING' FOR UPDATE`, r.id)
				if err != nil {
					return fmt.Errorf("scheduler: rollback lock attempts node %s: %w", r.nodeID, err)
				}
				var attIDs []uint64
				for rs.Next() {
					var attID uint64
					if err := rs.Scan(&attID); err != nil {
						_ = rs.Close()
						return fmt.Errorf("scheduler: rollback scan attempt node %s: %w", r.nodeID, err)
					}
					attIDs = append(attIDs, attID)
				}
				if err := rs.Err(); err != nil {
					_ = rs.Close()
					return fmt.Errorf("scheduler: rollback attempts node %s: %w", r.nodeID, err)
				}
				if err := rs.Close(); err != nil {
					return fmt.Errorf("scheduler: rollback close attempts node %s: %w", r.nodeID, err)
				}
				for _, attID := range attIDs {
					if err := engine.CloseAttemptDead(ctx, q, attID, "rollback_reset", "rolled back"); err != nil {
						return fmt.Errorf("scheduler: rollback close attempt %d: %w", attID, err)
					}
				}
			}
		}

		// 9. 闭包重置:每 node_id 全行集(控制行+实例行,行集一致性地整组);
		// 已 PENDING 的行零动作(EXECUTING/WA 已被守卫②拦,Reset 谓词只吃终态)。
		resetCount := 0
		for _, id := range sortedNodes(g.nodes) {
			if !closure[id] {
				continue
			}
			rowSet := append([]*nodeRow{rows.byNodeID[id]}, rows.instances[id]...)
			for _, r := range rowSet {
				if !r.status.Terminal() {
					continue
				}
				if rerun && r == ctrl {
					continue // 目标控制行走 Rerun 分流
				}
				from := r.status
				newVer, err := engine.ResetNodeToPending(ctx, q, r.id, r.version)
				if err != nil {
					return err
				}
				if err := stageRollbackTransition(ctx, q, s, p.ExecID, p.ActorID, r, from, engine.NodePending, newVer, r.attempts); err != nil {
					return err
				}
				r.status = engine.NodePending
				r.version = newVer
				r.output = sql.NullString{}
				r.workerID = sql.NullString{}
				resetCount++
			}
		}

		// 10. 目标重跑:RerunNode(快照读回重写 + attempts+1 即新轮 claim)
		// + att 事件(attempt 行由 worker 认领时建,台账快照抄档同源)。
		if rerun {
			from := ctrl.status
			newVer, err := engine.RerunNode(ctx, q, ctrl.id, ctrl.version, engine.ClaimInput{
				WorkerID:      p.WorkerID,
				InputSnapshot: snap,
				DeadlineAt:    time.Now().Add(NodeTimeout(ctrl.nodeType)),
			})
			if err != nil {
				return err
			}
			ctrl.attempts++
			if err := stageRollbackTransition(ctx, q, s, p.ExecID, p.ActorID, ctrl, from, engine.NodeExecuting, newVer, ctrl.attempts); err != nil {
				return err
			}
			ctrl.status = engine.NodeExecuting
			ctrl.version = newVer
			ctrl.workerID = sql.NullString{String: p.WorkerID, Valid: true}
			st := newExecState(s, ctx, q, p.ExecID, wf, g, rows, p.WorkerID, wfInput)
			if err := st.appendNodeTask(ctrl); err != nil {
				return err
			}
		}

		// 11. 执行复活(wf 最后):先于 advance——advance 内的 failExecution
		// (如重派即 input_eval 失败)需要 RUNNING→FAILED 边可达,否则 Conflict
		// 被吞后复活出无人推进的僵尸 RUNNING(对抗攻击 CRITICAL 的修法)。
		if err := engine.RestartExecution(ctx, q, p.ExecID); err != nil {
			return err
		}

		// 12. 审计:execution.rolled_back(低频运维面,同步直写;actor=user)。
		id, err := s.deps.IDs.Next()
		if err != nil {
			return fmt.Errorf("scheduler: gen rollback audit id: %w", err)
		}
		raw, err := json.Marshal(audit.ExecutionRolledBackPayload{
			TargetNodeID: target,
			FromStatus:   string(wfStatus),
			ResetCount:   resetCount,
			RerunTarget:  rerun,
		})
		if err != nil {
			return fmt.Errorf("scheduler: marshal rollback audit payload: %w", err)
		}
		if err := audit.Append(ctx, q, audit.Event{
			ID: id, EventType: audit.EventExecutionRolledBack, ExecutionID: p.ExecID,
			NodeID: target, ActorType: audit.ActorUser, ActorID: p.ActorID, Payload: raw,
		}); err != nil {
			return err
		}

		// 13. 全图重派:advance 过滤 PENDING(重置集),就绪者当场派发、
		// SKIPPED 前驱传播重剪、前驱未定者等待完成事件级联;目标已 EXECUTING
		// 不在候选面。终态判定归提交后 finishIfDone(第 0 步镜像)。
		st := newExecState(s, ctx, q, p.ExecID, wf, g, rows, p.WorkerID, wfInput)
		if err := st.buildEnv(wfInput); err != nil {
			return err
		}
		if err := st.advance(sortedNodes(g.nodes)); err != nil {
			return err
		}
		res = &RollbackResult{TargetNodeID: target, RerunTarget: rerun, ResetCount: resetCount}
		return engine.TouchExecution(ctx, q, p.ExecID)
	})
	return res, err
}

// isWorkerExecType 报告节点类型是否由 worker 执行器承载(att 任务可派发);
// branch/parallel 是调度器内联语义,重跑必经 Reset→advance 重推导。
func isWorkerExecType(nodeType string) bool {
	switch nodeType {
	case "llm", "tool", "approval":
		return true
	}
	return false
}

// stageRollbackTransition 是回滚/取消事务(T19.3 起共用)节点审计行的单点
// (node.state_changed from=旧终态/EXECUTING/非终态;高频类走暂存;
// actor=user/操作者,区别于 worker 推进路径的 actor=worker;attempt_no
// 携带轮次归属 = 轮次边界证据;worker_id = 行内归因快照,取消路径与 reaper
// 孤儿收口行对称)。
func stageRollbackTransition(ctx context.Context, q engine.DBTX, s *Scheduler,
	execID uint64, actorID string, r *nodeRow,
	from, to engine.NodeState, newVer uint32, attemptNo uint16) error {
	id, err := s.deps.IDs.Next()
	if err != nil {
		return fmt.Errorf("scheduler: gen rollback node audit id: %w", err)
	}
	raw, err := json.Marshal(audit.NodeStateChangedPayload{
		From: string(from), To: string(to),
		NodeID: r.nodeID, Version: newVer, AttemptNo: attemptNo, InstanceNo: r.instanceNo,
		WorkerID: r.workerID.String, // 归因快照(取消楔停行与 reaper 孤儿收口行对称)
	})
	if err != nil {
		return fmt.Errorf("scheduler: marshal rollback node audit payload: %w", err)
	}
	return audit.Enqueue(ctx, q, audit.Event{
		ID: id, EventType: audit.EventNodeStateChanged, ExecutionID: execID,
		NodeExecID: r.id, NodeID: r.nodeID,
		ActorType: audit.ActorUser, ActorID: actorID, Payload: raw,
	})
}
