package scheduler

// T4.1 审批挂起收口:approval 节点在 worker 侧的执行是一个纯决策(无外部
// 调用),attempt 以 SUCCEEDED 收口、节点挂起 WAITING_APPROVAL、worker 任务
// 正常结束释放并发槽(ARCHITECTURE Q4/L67:worker 不阻塞)。本文件是调度器
// 侧的挂起事务;执行器半边在 internal/nodes/approval.go,worker 通道半边在
// internal/worker(Outcome.Suspend)。
//
// 事务形状与 Complete 同构(锁序/终态守卫/幂等哨兵全部沿用):
//
//	lockAllRows(全行集,node_id 升序)→ 快照守卫 + 类型守卫 →
//	CloseAttempt(RUNNING→SUCCEEDED)→ SuspendNode(EXECUTING→
//	WAITING_APPROVAL,deadline=now+DSL timeout_hours)→ 双审计行 →
//	静默检查 → SuspendExecution | TouchExecution
//
// 与 Complete 的语义分立(勿合并):Complete 是「attempt 结果 → 节点终态/重派
// + 下游推进」;Suspend 是「attempt 成功 + 节点挂起 + 下游零推进」。挂起节点
// 的推进权威在 T4.2(审批 API → outbox resume:<exec>:<node>)。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"braid/internal/audit"
	"braid/internal/engine"
	"braid/internal/engine/dsl"
)

// SuspendRequest 是审批挂起收口的请求(worker 适配 approval Outcome 产出)。
// 无 Output/Usage/ToolCall 挂载面:审批挂起无产出(决策在 T4.2 续跑时落
// output)、无外部调用(零台账/观测)——worker 侧已按此校验。
type SuspendRequest struct {
	ExecID     uint64
	NodeExecID uint64
	AttemptID  uint64
	WorkerID   string // 挂起方(执行 attempt 的 worker 进程)
}

// approvalDefaultTimeoutHours 镜像 dsl/schema.json $defs.approvalNode 的
// timeout_hours default(Q9:默认 72h)。解析面不物化 schema default(缺省字段
// = Go 零值),生效值在此单点补齐,三个消费面同源:快照物化(eval.go
// buildSnapshot,审批人可读面)、DDL deadline、审计 payload——三处口径一致
// (对抗审计 MEDIUM 修:原实现快照存 DSL 零值,与另两处分叉)。不用
// nodeTimeouts["approval"](claim 执行超时词表,语义不同源,数值恰好相同而已)。
const approvalDefaultTimeoutHours = 72

// effectiveApprovalSpec 返回 approval 节点的生效参数(schema default 补齐)。
func effectiveApprovalSpec(a *dsl.ApprovalNode) (mode, onTimeout string, timeout time.Duration) {
	mode, onTimeout = a.Mode, a.OnTimeout
	if mode == "" {
		mode = "any"
	}
	if onTimeout == "" {
		onTimeout = "escalate"
	}
	hours := a.TimeoutHours
	if hours <= 0 {
		hours = approvalDefaultTimeoutHours
	}
	return mode, onTimeout, time.Duration(hours * float64(time.Hour))
}

// Suspend 是审批挂起回写:单事务 = 锁全行集 → 守卫(类型/快照)→
// CloseAttempt(SUCCEEDED)→ SuspendNode → 双审计行 → 静默检查 → 执行级
// 镜像或活跃刷新。1213/1205 与 Complete 同款内部重试(只重试回写事务,
// 不重跑 executor)。
func (s *Scheduler) Suspend(ctx context.Context, req SuspendRequest) error {
	switch {
	case req.ExecID == 0 || req.NodeExecID == 0 || req.AttemptID == 0:
		return errors.New("scheduler: zero id field(s) in suspend request")
	case req.WorkerID == "":
		return errors.New("scheduler: empty worker id in suspend request")
	}
	exec, err := s.loadExecution(ctx, req.ExecID)
	if err != nil {
		return err
	}
	wf, g, err := s.loadWorkflow(ctx, exec.definitionID)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		err := s.suspendOnce(ctx, req, wf, g, exec.input)
		if err == nil {
			return nil
		}
		if errors.Is(err, errAttemptAlreadyClosed) {
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

// suspendOnce 是挂起的事务体(Complete 的 completeOnce 同构)。
func (s *Scheduler) suspendOnce(ctx context.Context, req SuspendRequest,
	wf *dsl.Workflow, g *dag, wfInput json.RawMessage) error {
	var termGuard bool
	err := engine.WithTx(ctx, s.deps.DB, func(q engine.DBTX) error {
		// 1. 有序全行锁(与 completeOnce 同集同序:并发 Complete/Suspend 在
		// 该序列上串行,静默检查的「数在飞兄弟」不可能读到过期快照)。
		rows, err := lockAllRows(ctx, q, req.ExecID, g)
		if err != nil {
			return err
		}
		n := rows.byID[req.NodeExecID]
		if n == nil {
			return fmt.Errorf("%w: node_execution %d not in execution %d",
				engine.ErrNotFound, req.NodeExecID, req.ExecID)
		}

		// 2. 终态守卫(快照读,与 completeOnce 同款):已终态执行内的迟到
		// 挂起只收口 attempt,不再挂起/转执行态(镜像 Complete 失败分支的
		// 语义:迟到者不产生新语义,节点留 EXECUTING 归 T2.5 判死扫描)。
		var wfStatus engine.WorkflowState
		if err := q.QueryRowContext(ctx,
			`SELECT status FROM workflow_execution WHERE id=?`, req.ExecID).Scan(&wfStatus); err != nil {
			return fmt.Errorf("scheduler: read execution status %d: %w", req.ExecID, err)
		}
		termGuard = wfStatus.Terminal()

		// 3. 类型守卫:非 approval 节点的挂起请求 = 装配缺陷(nodes Mux 误
		// 路由),响亮拒绝防 llm/tool 冒用挂起通道(镜像 ToolCall 类型校验)。
		if n.nodeType != "approval" {
			return fmt.Errorf("scheduler: suspend on non-approval node %q (type %q)",
				n.nodeID, n.nodeType)
		}
		spec := g.nodes[n.nodeID].Approval
		if spec == nil {
			return fmt.Errorf("scheduler: approval node %q has no spec in pinned dsl (drift)", n.nodeID)
		}

		// 4. 快照守卫:engine 不变量 WAITING_APPROVAL ⇒ input_snapshot NOT
		// NULL(审批人可读面 = 快照真相源)。生产路径不可达(ClaimNode 必写,
		// SuspendNode 不触碰),破坏即数据缺陷,响亮拒绝优于静默落违约行。
		var snap []byte
		if err := q.QueryRowContext(ctx,
			`SELECT input_snapshot FROM node_execution WHERE id=?`, n.id).Scan(&snap); err != nil {
			return fmt.Errorf("scheduler: read snapshot node %d: %w", n.id, err)
		}
		if snap == nil {
			return fmt.Errorf("scheduler: suspend node %d without input_snapshot (engine invariant)", n.id)
		}

		// 5. attempt 收口(SUCCEEDED:worker 完成了它的任务——识别审批并
		// 挂起;DDL 挂起契约:与节点挂起同事务,否则留 RUNNING attempt 被
		// 心跳判死扫描误报)。Conflict = 收口权旁落 ⇒ 幂等(Complete 同款);
		// Observed==DEAD 时落结构化 warn(W1 哨兵镜像 completeOnce:判死双
		// 执行的观测信号,对抗审计修,零行为变更)。
		if err := engine.CloseAttempt(ctx, q, req.AttemptID, engine.AttemptSucceed, nil, "", "", nil); err != nil {
			if errors.Is(err, engine.ErrConflict) {
				var ce *engine.ConflictError
				if errors.As(err, &ce) && ce.Observed == engine.NodeState(engine.AttemptDead) {
					s.log.Warn("suspend.discarded_by_reaper",
						"execution_id", req.ExecID, "node_execution_id", req.NodeExecID,
						"attempt_id", req.AttemptID)
				}
				return errAttemptAlreadyClosed
			}
			return err
		}
		if termGuard {
			return nil // 迟到挂起:只收口 attempt(见 2),不触节点/执行态
		}

		// 6. 节点挂起:EXECUTING→WAITING_APPROVAL,deadline=now+生效超时
		// (T4.3 扫描字源;worker_id 保留归因)。
		mode, onTimeout, timeout := effectiveApprovalSpec(spec)
		deadline := time.Now().Add(timeout)
		newVer, err := engine.SuspendNode(ctx, q, n.id, n.version, n.workerID.String, deadline)
		if err != nil {
			return err
		}
		n.status = engine.NodeWaitingApproval
		n.version = newVer

		// 7. 双审计行(同事务):迁移行(与全部既有迁移一致)+ 审批域事件
		// 行(T4.4 待办/T6.4 积压指标字源;DDL 0001 词表注释预留同名)。
		st := newExecState(s, ctx, q, req.ExecID, wf, g, rows, req.WorkerID, wfInput)
		if err := st.stageRow(audit.EventNodeStateChanged, n, req.AttemptID, audit.NodeStateChangedPayload{
			From: string(engine.NodeExecuting), To: string(engine.NodeWaitingApproval),
			NodeID: n.nodeID, Version: newVer, AttemptNo: n.attempts, WorkerID: n.workerID.String,
		}); err != nil {
			return err
		}
		if err := st.auditRow(audit.EventApprovalRequested, n, req.AttemptID, audit.ApprovalRequestedPayload{
			NodeID: n.nodeID, Approvers: spec.Approvers, Mode: mode,
			TimeoutHours: timeout.Hours(), OnTimeout: onTimeout, DeadlineAt: deadline,
		}); err != nil {
			return err
		}

		// 8. 静默检查(D-2026-09-23-t41-approval-suspend):全行集在手,数在飞
		// 兄弟——无其他 EXECUTING ⇒ 执行级镜像 RUNNING→WAITING_APPROVAL
		// (reaper 扫描④对它刻意排除,执行级合法静默);有(合法 fan-out)⇒
		// 留 RUNNING(否则制造「WAITING_APPROVAL 执行下有 EXECUTING 节点」的
		// reaper 违约态),此时仍刷新活跃时刻(DDL 契约:节点/attempt 推进
		// 同事务刷新)。Conflict 良性:守卫读后他方终态化(取消/fail-fast 竞
		// 速),节点已挂起合法,执行态以先落者为准。
		for _, r := range rows.byNodeID {
			if r != n && r.status == engine.NodeExecuting {
				return engine.TouchExecution(ctx, q, req.ExecID)
			}
		}
		if err := engine.SuspendExecution(ctx, q, req.ExecID); err != nil {
			if errors.Is(err, engine.ErrConflict) {
				s.log.Warn("suspend.execution_conflict",
					"execution_id", req.ExecID, "node_execution_id", n.id)
				return nil // TransitionWorkflow 已刷活跃时刻
			}
			return err
		}
		s.log.Info("execution.suspended",
			"execution_id", req.ExecID, "node_execution_id", n.id, "deadline", deadline)
		return nil
	})
	if err != nil {
		return err
	}
	// 提交后无终态补判:挂起节点非终态,finishIfDone 恒 no-op(镜像省略)。
	return nil
}
