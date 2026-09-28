// Package worker 实现 T2.3 Asynq 执行器:消费 relay 投递的任务,以 attempt
// 唯一键为执行级幂等锚,完成节点 attempt 的记录、执行与收口;T2.4 起尾部
// 接 scheduler.Complete(节点收口 + 入度推进 + 执行终态回写)与
// scheduler.Start(execution.start → 入口派发)。
//
// 引擎消费链上的位置:
//
//	调度器(scheduler 包)事务:ClaimNode(PENDING→EXECUTING,attempts+1)+
//	    outbox.Append(event_type=node.task_enqueue,
//	                  task_id=att:<node_execution_id>:<attempt_no>)
//	→ relay 投递(MaxRetry=1、确定性 TaskID,T2.2)
//	→ 本包:mux 按 event_type 路由 → claimAttempt(幂等锚)→ Executor
//	    执行缝(心跳并行)→ scheduler.Complete(同事务收口 attempt + 节点 +
//	    推进 + 刷 execution 活跃时刻)
//
// 幂等层次(执行级幂等锚 = uk_attempt,不是 asynq TaskID——T2.2 审计移交①):
//   - asynq ErrTaskIDConflict 仅在任务记录存活期内收敛(无 Retention 下完成
//     即删,之后重投会再次成功入队),relay 用它加速收敛,但它不是永久锚;
//   - 本包以 engine.InsertAttempt 撞 uk_attempt(node_execution_id, attempt_no)
//     裁决:首达建 RUNNING 行;重达读行——已终态 → 幂等返回(晚到重投不二次
//     执行,行状态即收敛证据);RUNNING → 接管重跑(asynq 内部重试 / T2.5
//     判死重入队后的必然重跑;副作用收敛依赖供应商幂等键 = 同 TaskID,T3.1);
//   - attempt 行终态后禁改(无 updated_at,收口是一次终写)。
package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel/trace"

	"braid/internal/engine"
	"braid/internal/metrics"
	"braid/internal/notify"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
)

// EventTypeNodeTask 是 attempt 派发事件的 event_type 路由键,词表真源在
// scheduler 包(其 outbox 事件即消费对象,单一定义无漂移面)。
const EventTypeNodeTask = scheduler.EventTypeNodeTask

// nodeTaskPayload 是 worker 侧对 scheduler.NodeTaskPayload 的别名(存量代码
// 引用面不变;类型本体单一,JSON tag 无第二份定义)。
type nodeTaskPayload = scheduler.NodeTaskPayload

// Request 是 Executor 的执行请求。TaskID 即外部调用幂等键(与
// node_attempt.idempotency_key 同值):供应商侧同键去重(AGENTS 铁律
// 「所有外部调用带 attempt 幂等键」)。
type Request struct {
	ExecutionID     uint64
	NodeExecutionID uint64
	AttemptID       uint64 // worker 生成的 node_attempt.id
	AttemptNo       uint16
	TaskID          string
	WorkerID        string
}

// Outcome 是 Executor 的业务结果。Status 仅允许 SUCCEEDED/FAILED。
//
// 语义分立(勿混用,收口路径依赖它):业务失败(LLM 429 重试穷尽、工具报错)
// = Outcome{FAILED} + handler 收口,重试权威在平台节点层(T2.4,新 attempt =
// 新 TaskID);基础设施失败(DB 断连等)= error 返回值,attempt 不收口、行留
// RUNNING,asynq 内部重试接管重跑(见 RetryDelayFunc 决策注释)。
//
// Output 是 SUCCEEDED 时的节点输出({{nodes.<id>.output.<field>}} 解析真相源),
// 经 scheduler.Complete → engine.SucceedNode 落 node_execution.output;nil → NULL。
//
// Usage 是 LLM attempt 的台账参数(T3.1,仅 SUCCEEDED 携带——失败路径 P0
// 不计费):由 llm.Cost.AttemptUsage 产出,经 scheduler.Complete 随收口终写
// 落 node_attempt usage/cost 列并同事务发 llm.call.completed 审计行(双录)。
//
// ToolCall 是 tool attempt 的调用观测参数(T3.2,仅 SUCCEEDED 携带):由
// nodes.ToolExecutor 产出,经 scheduler.Complete 同事务发 tool.call.completed
// 审计行(actor=tool/工具名)。
//
// Suspend 是审批节点的挂起信号(T4.1,Q4/L67:worker 不阻塞):仅可配
// Status==SUCCEEDED 且不得携 Output/Usage/ToolCall(审批挂起无产出、无外部
// 调用)。handler 据此走 Sched.Suspend 收口(attempt SUCCEEDED + 节点
// WAITING_APPROVAL + 执行级镜像),而非 Complete 的 SucceedNode 分支(那会把
// 节点收成 SUCCEEDED 并推进下游)。合法形由 suspendOK 校验。
type Outcome struct {
	Status   engine.AttemptState
	Code     string
	Msg      string
	Output   json.RawMessage
	Usage    *engine.AttemptUsage
	ToolCall *scheduler.ToolCallInfo
	Suspend  bool
}

// suspendOK 校验挂起信号的合法形(非挂起 Outcome 恒过)。独立函数 = 零 DB
// 单测面;畸形 = 执行器装配缺陷(非 approval 执行器误发/字段误挂),响亮
// 拒绝优于静默走错收口路径。
func suspendOK(o Outcome) error {
	if !o.Suspend {
		return nil
	}
	if o.Status != engine.AttemptSucceed {
		return fmt.Errorf("worker: suspend outcome status %q: want SUCCEEDED", o.Status)
	}
	if len(o.Output) > 0 || o.Usage != nil || o.ToolCall != nil {
		return errors.New("worker: suspend outcome carries output/usage/toolcall (approval suspend has none)")
	}
	return nil
}

// Executor 是节点执行缝:T2.3 只落框架与幂等/记账契约,T2.4 已接调度回写,
// T3.x 接真实节点实现(llm/tool)。与 relay.Enqueuer 同款依赖倒置:生产实现
// 唯一,fake 只存在于 _test.go。
type Executor interface {
	Execute(ctx context.Context, req Request) (Outcome, error)
}

// ApprovalNotifier 是审批通知发射缝(T4.5,D-t43 #3 锁定的换装点):消费
// approval.notify 事件后的发送面。生产实现 notify.Sender(结构化满足,
// cmd/worker 装配;发送/去重/审计编排全在 notify 包)。nil = 未配置 SMTP
// 的降级面——保留 T4.3 占位 log-only ack 语义(dev 栈无 SMTP 可用;送达
// 缺口由 T6.4 的 escalated-vs-notified 审计对账暴露,非静默)。
type ApprovalNotifier interface {
	NotifyApproval(ctx context.Context, execID, nodeExecID uint64, reason string) error
}

// Deps 是运行依赖。Logger 为 nil 时用 slog.Default()。Notifier 为 nil 是
// 合法降级形态(见 ApprovalNotifier);其余依赖 nil 拒启动。
type Deps struct {
	DB       *sql.DB
	IDs      *snowflake.Generator // attempt 行 id 生成(应用侧 Snowflake)
	Executor Executor
	Sched    *scheduler.Scheduler // T2.4 调度器(完成回写 + execution.start);nil 拒启动
	Notifier ApprovalNotifier     // T4.5 审批通知面;nil = SMTP 未配置降级
	Logger   *slog.Logger
	// Metrics 是观测面(T6.4):nil = 关闭(测试默认)。notify 结果计数 +
	// asynq archived 死信 gauge(探测 goroutine 随 Run 生命周期)。
	Metrics *metrics.WorkerSet
	// Tracer 是 OTel 消费半边(T6.2):nil = 关闭(测试默认,payload 内
	// trace_id 不 extract,ctx 无 span,audit 落 NULL——与 api Tracing(nil)
	// 同口径)。非 nil 时每任务起段根 span(remote parent = payload 携带的
	// trace,续链不新开 trace;D-2026-09-27-t62-otel-trace)。
	Tracer trace.Tracer
}

// Run 阻塞运行 worker 直到 ctx 取消(优雅停机受 cfg.ShutdownTimeout 界住,
// 超时强杀的在飞 attempt 留 RUNNING 归 T2.5 判死)。返回非 nil 仅当依赖接线
// 缺损或 server 启动失败;运行期 handler 错误由 asynq 自管(重试→archived)。
func Run(ctx context.Context, d Deps, cfg Config, opt asynq.RedisClientOpt) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if d.DB == nil || d.IDs == nil || d.Executor == nil || d.Sched == nil {
		return errors.New("worker: nil DB, IDs, Executor or Sched")
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	h := &attHandler{deps: d, cfg: cfg, log: log}

	// T6.4:asynq archived 死信探测(Redis 侧零 DB 痕迹的唯一可观测面;
	// D-t42 §8-②/T4.5 已知边界的告警字源)。观测 goroutine,失败只记日志。
	if d.Metrics != nil {
		go h.probeAsynqArchived(ctx, opt)
	}

	// RetryDelayFunc 显式钉死(T2.2 审计移交②的决策落地):MaxRetry=1 的
	// asynq 实际语义 = handler 首败后自管**一次**内部重试(server 默认
	// 15-45s 随机退避),二次失败才 archived。本包 handler 返回 error 仅当
	// attempt 未收口的基础设施失败(见 Outcome 语义分立),故该内部重试的
	// 唯一职责 = 接管未收口的 RUNNING attempt 重跑:固定短延迟(默认 5s)
	// 优于随机 15-45s——瞬态 DB 故障亚秒级恢复,随机长退避只拉长恢复时延;
	// 穷尽后任务 archived,行留 RUNNING 交 T2.5 判死重入队。与平台重试零
	// 叠加:asynq 内部重试永不产生新 attempt(同 TaskID = 同 attempt_no),
	// 节点级重试权威在调度层。
	srv := asynq.NewServer(opt, asynq.Config{
		Concurrency:     cfg.Concurrency,
		Queues:          cfg.Queues,
		ShutdownTimeout: cfg.ShutdownTimeout,
		RetryDelayFunc:  func(n int, e error, t *asynq.Task) time.Duration { return cfg.RetryDelay },
	})
	mux := asynq.NewServeMux()
	mux.Use(traceMiddleware(d.Tracer, log))
	mux.HandleFunc(EventTypeNodeTask, h.handleNodeTask)
	mux.HandleFunc(scheduler.EventTypeExecutionStart, h.handleExecStart)
	mux.HandleFunc(scheduler.EventTypeExecutionResume, h.handleNodeResume)
	mux.HandleFunc(scheduler.EventTypeApprovalNotify, h.handleApprovalNotify)

	if err := srv.Start(mux); err != nil {
		return fmt.Errorf("worker: start server: %w", err)
	}
	log.Info("worker.started",
		"concurrency", cfg.Concurrency, "queues", cfg.Queues,
		"worker_id", cfg.WorkerID, "heartbeat_interval", cfg.HeartbeatInterval,
		"retry_delay", cfg.RetryDelay, "shutdown_timeout", cfg.ShutdownTimeout)
	defer log.Info("worker.stopped")
	<-ctx.Done()
	srv.Shutdown()
	return nil
}

// attHandler 承载 handler 依赖(与 relay.runner 同款形状)。
type attHandler struct {
	deps Deps
	cfg  Config
	log  *slog.Logger
}

// handleNodeTask 处理 node.task_enqueue:幂等认领 → 执行 → 收口。
// 返回 error = asynq 视角失败(内部重试一次后 archived)。
func (h *attHandler) handleNodeTask(ctx context.Context, t *asynq.Task) error {
	taskID, ok := asynq.GetTaskID(ctx)
	if !ok {
		// asynq 保证 handler ctx 必携 ID(v0.26 processor 注入);缺失 = 框架
		// 契约破坏,响亮失败。
		return errors.New("worker: no task id in handler context")
	}
	var p nodeTaskPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		// 数据缺陷:重试注定同败,但走 archived 留死信痕迹(T6.4 可见)
		// 优于静默 ack 丢弃。
		return fmt.Errorf("worker: task %s payload: %w", taskID, err)
	}
	if p.ExecutionID == 0 || p.NodeExecutionID == 0 || p.AttemptNo == 0 {
		return fmt.Errorf("worker: task %s payload: zero id field(s) %+v", taskID, p)
	}

	attemptID, err := h.claimAttempt(ctx, taskID, p)
	if err != nil {
		return err
	}
	if attemptID == 0 {
		return nil // 已终态:晚到重投幂等收敛
	}

	// 心跳与执行同生命周期:executor 返回即停(defer 在 return 求值后执行,
	// 心跳事务与收口事务可能短暂并发)。安全不变量 = 两者对 attempt →
	// workflow_execution 同序加锁(无 AB-BA 面),且心跳 UPDATE 带
	// WHERE status='RUNNING'、CloseAttempt 撞并发收口有结构化 ErrConflict:
	// 任意交错都收敛(后提交的心跳 0 行静默让位)。
	hbCtx, stopHeartbeat := context.WithCancel(ctx)
	go h.heartbeat(hbCtx, attemptID, p.ExecutionID)
	defer stopHeartbeat()

	outcome, err := h.deps.Executor.Execute(ctx, Request{
		ExecutionID:     p.ExecutionID,
		NodeExecutionID: p.NodeExecutionID,
		AttemptID:       attemptID,
		AttemptNo:       p.AttemptNo,
		TaskID:          taskID,
		WorkerID:        h.cfg.WorkerID,
	})
	if err != nil {
		// 基础设施失败:不收口,行留 RUNNING,asynq 重试接管。
		return fmt.Errorf("worker: execute attempt %d (task %s): %w", attemptID, taskID, err)
	}
	if outcome.Status != engine.AttemptSucceed && outcome.Status != engine.AttemptFailed {
		return fmt.Errorf("worker: attempt %d outcome status %q: want SUCCEEDED|FAILED", attemptID, outcome.Status)
	}
	if err := suspendOK(outcome); err != nil {
		return fmt.Errorf("worker: attempt %d (task %s): %w", attemptID, taskID, err)
	}
	if outcome.Suspend {
		return h.suspendNode(ctx, taskID, attemptID, p)
	}
	return h.completeNode(ctx, taskID, attemptID, p, outcome)
}

// claimAttempt 是执行级幂等锚:首达 InsertAttempt 建行;撞 uk_attempt(1062
// → engine.ErrConflict)读行裁决——终态返回 0(幂等返回),RUNNING 返回行 id
// (接管:上一发未收口,asynq 内部重试 / T2.5 判死重入队后重跑)。
func (h *attHandler) claimAttempt(ctx context.Context, taskID string, p nodeTaskPayload) (uint64, error) {
	id, err := h.deps.IDs.Next()
	if err != nil {
		return 0, fmt.Errorf("worker: gen attempt id: %w", err)
	}
	// T9.2 台账:attempt 行出生即抄输入快照(旧轮输入证据物理载体,
	// D-2026-09-28-t92 决策 3①)。node 行此刻的快照即本 attempt 的输入
	// (dispatch 侧 ClaimNode/RerunNode 已写就;重派不重写、值恒同)。
	// 行缺失 = 数据缺陷,响亮拒绝(与 readAttempt 同判)。
	var snap []byte
	if err := h.deps.DB.QueryRowContext(ctx,
		`SELECT input_snapshot FROM node_execution WHERE id=?`, p.NodeExecutionID).Scan(&snap); err != nil {
		return 0, fmt.Errorf("worker: read node snapshot for attempt ledger (node %d): %w", p.NodeExecutionID, err)
	}
	err = engine.InsertAttempt(ctx, h.deps.DB, engine.AttemptParams{
		ID:             id,
		NodeExecID:     p.NodeExecutionID,
		ExecID:         p.ExecutionID,
		AttemptNo:      p.AttemptNo,
		WorkerID:       h.cfg.WorkerID,
		IdempotencyKey: taskID,
		AsynqTaskID:    taskID,
		InputSnapshot:  json.RawMessage(snap),
	})
	switch {
	case err == nil:
		h.log.Info("attempt.claimed", "attempt_id", id, "task_id", taskID, "node_execution_id", p.NodeExecutionID)
		return id, nil
	case errors.Is(err, engine.ErrConflict):
		rowID, status, rerr := h.readAttempt(ctx, p)
		if rerr != nil {
			return 0, rerr
		}
		if status != engine.AttemptRunning {
			h.log.Info("attempt.already_final",
				"attempt_id", rowID, "task_id", taskID, "status", status)
			return 0, nil
		}
		h.log.Info("attempt.takeover", "attempt_id", rowID, "task_id", taskID)
		// 接管即重置存活窗口:行上 heartbeat_at 停在上一任最后心跳,最坏
		// 陈旧度 ≈ RetryDelay + HeartbeatInterval(默认 15s)。重置在行锁上
		// 复核 RUNNING(D-2026-09-22-t25-reaper-semantics #8 接管原子性):
		// 接管的活性证明与 reaper 的死亡裁决在 attempt 行锁上确定性串行——
		// 重置先落 ⇒ reaper 锁内复核见新鲜心跳 skip;裁决先落 ⇒ 此处见非
		// RUNNING 放弃接管直接 ack,消灭「接管与判死同拍」的重叠执行窗口。
		// reset 事务自身失败仍是尽力语义(warn 不阻断):本发执行的常规心跳
		// 随即接管窗口。
		var observed engine.AttemptState
		aborted := false
		if err := engine.WithTx(ctx, h.deps.DB, func(q engine.DBTX) error {
			if err := q.QueryRowContext(ctx,
				`SELECT status FROM node_attempt WHERE id=? FOR UPDATE`, rowID).Scan(&observed); err != nil {
				return err
			}
			if observed != engine.AttemptRunning {
				aborted = true
				return nil // 空提交释放锁:终局已由收口方裁决,接管让位
			}
			return engine.TouchAttemptHeartbeat(ctx, q, rowID, p.ExecutionID)
		}); err != nil {
			h.log.Warn("takeover.heartbeat_reset_fail", "attempt_id", rowID, "err", err)
		} else if aborted {
			h.log.Info("attempt.already_final",
				"attempt_id", rowID, "task_id", taskID, "status", observed)
			return 0, nil
		}
		return rowID, nil
	default:
		return 0, fmt.Errorf("worker: insert attempt (task %s): %w", taskID, err)
	}
}

// readAttempt 按 uk 定位既有 attempt 行(撞键后的裁决读)。
func (h *attHandler) readAttempt(ctx context.Context, p nodeTaskPayload) (uint64, engine.AttemptState, error) {
	var id uint64
	var status engine.AttemptState
	err := h.deps.DB.QueryRowContext(ctx,
		`SELECT id, status FROM node_attempt WHERE node_execution_id=? AND attempt_no=?`,
		p.NodeExecutionID, p.AttemptNo).Scan(&id, &status)
	if errors.Is(err, sql.ErrNoRows) {
		// 撞唯一键却读不到行:行不可能被删(FK 链无 DELETE 路径),数据缺陷。
		return 0, "", fmt.Errorf("worker: attempt row vanished for node_exec=%d no=%d", p.NodeExecutionID, p.AttemptNo)
	}
	if err != nil {
		return 0, "", fmt.Errorf("worker: read attempt node_exec=%d no=%d: %w", p.NodeExecutionID, p.AttemptNo, err)
	}
	return id, status, nil
}

// suspendNode 把审批挂起交给调度器收口(T4.1):与 completeNode 平行的另一条
// 收口通道(单事务 attempt SUCCEEDED + 节点 WAITING_APPROVAL + 执行级镜像/活跃
// 刷新,事务体在 scheduler.Suspend)。返回 nil = 任务正常 ack,worker 槽释放,
// 无阻塞残留(Q4 验收)。
func (h *attHandler) suspendNode(ctx context.Context, taskID string, attemptID uint64, p nodeTaskPayload) error {
	err := h.deps.Sched.Suspend(ctx, scheduler.SuspendRequest{
		ExecID:     p.ExecutionID,
		NodeExecID: p.NodeExecutionID,
		AttemptID:  attemptID,
		WorkerID:   h.cfg.WorkerID,
	})
	if err != nil {
		return fmt.Errorf("worker: suspend node %d (task %s): %w", p.NodeExecutionID, taskID, err)
	}
	h.log.Info("attempt.suspended",
		"attempt_id", attemptID, "node_execution_id", p.NodeExecutionID)
	return nil
}

// completeNode 把 attempt 业务结果交给调度器收口(T2.4):单事务内 CloseAttempt +
// 节点收口 + 入度推进 + 刷 execution 活跃时刻 + 提交后终态判定。attempt 已被
// 早前一次 Complete 收口(ErrConflict → 调度器哨兵)在此收敛为幂等成功。
func (h *attHandler) completeNode(ctx context.Context, taskID string, attemptID uint64, p nodeTaskPayload, o Outcome) error {
	err := h.deps.Sched.Complete(ctx, scheduler.CompletionRequest{
		ExecID:     p.ExecutionID,
		NodeExecID: p.NodeExecutionID,
		AttemptID:  attemptID,
		Status:     o.Status,
		Code:       o.Code,
		Msg:        o.Msg,
		Output:     o.Output,
		WorkerID:   h.cfg.WorkerID,
		Usage:      o.Usage,
		ToolCall:   o.ToolCall,
	})
	if err != nil {
		return fmt.Errorf("worker: complete node %d (task %s): %w", p.NodeExecutionID, taskID, err)
	}
	h.log.Info("attempt.closed",
		"attempt_id", attemptID, "status", o.Status, "node_execution_id", p.NodeExecutionID)
	return nil
}

// handleExecStart 处理 execution.start:调度器起跑执行(PENDING→RUNNING +
// 入口派发)。双投递幂等在调度器内裁决(非 PENDING → no-op)。
func (h *attHandler) handleExecStart(ctx context.Context, t *asynq.Task) error {
	taskID, ok := asynq.GetTaskID(ctx)
	if !ok {
		return errors.New("worker: no task id in handler context")
	}
	var p scheduler.ExecStartPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("worker: task %s payload: %w", taskID, err)
	}
	if p.ExecutionID == 0 {
		return fmt.Errorf("worker: task %s payload: zero execution_id", taskID)
	}
	if err := h.deps.Sched.Start(ctx, p.ExecutionID, h.cfg.WorkerID); err != nil {
		return fmt.Errorf("worker: start execution %d (task %s): %w", p.ExecutionID, taskID, err)
	}
	return nil
}

// handleNodeResume 处理 execution.resume(T4.2 审批续跑):调度器复入审批节点
// 并按决定收口(批准→SUCCEEDED+下游推进;驳回→FAILED+fail-fast)。幂等收敛
// 在调度器事务内裁决(节点已非 WAITING_APPROVAL → no-op);决策复核(事件
// 引用的 approval_decision 行)也在调度器——payload 仅 id 引用,消费必回表。
func (h *attHandler) handleNodeResume(ctx context.Context, t *asynq.Task) error {
	taskID, ok := asynq.GetTaskID(ctx)
	if !ok {
		return errors.New("worker: no task id in handler context")
	}
	var p scheduler.ResumePayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("worker: task %s payload: %w", taskID, err)
	}
	if p.ExecutionID == 0 || p.NodeExecutionID == 0 || p.DecisionID == 0 {
		return fmt.Errorf("worker: task %s payload: zero id field(s) %+v", taskID, p)
	}
	if err := h.deps.Sched.Resume(ctx, scheduler.ResumeRequest{
		ExecID:     p.ExecutionID,
		NodeExecID: p.NodeExecutionID,
		DecisionID: p.DecisionID,
		AttemptNo:  p.AttemptNo,
		WorkerID:   h.cfg.WorkerID,
	}); err != nil {
		return fmt.Errorf("worker: resume node %d (task %s): %w", p.NodeExecutionID, taskID, err)
	}
	h.log.Info("approval.resume_processed",
		"execution_id", p.ExecutionID, "node_execution_id", p.NodeExecutionID, "task_id", taskID)
	return nil
}

// handleApprovalNotify 处理 approval.notify(T4.3 审批超时升级通知;T4.5
// 起换装为 SMTP 发送面,注册行不动)。P0 语义 = 解析校验 + reason 词表闸 +
// Notifier 分派:通知的事实记录在 outbox 事件行与 approval.escalated 审计行
// (reaper 同事务落定);通知事件不写节点状态——发送/去重(approval.notified
// 审计行锚)/邮件渲染全在 notify 包。Notifier nil(SMTP 未配置)= 降级
// log-only ack(T4.3 占位语义的显式保留)。错误上抛 ⇒ asynq 内部重试一次
// 后 archived(死信可见,notify 无死信网——D-t43 已知边界,告警归 T6.4);
// 重投的幂等收敛 = notify.ErrAlreadyNotified → ack。不触节点执行器。
func (h *attHandler) handleApprovalNotify(ctx context.Context, t *asynq.Task) error {
	taskID, ok := asynq.GetTaskID(ctx)
	if !ok {
		return errors.New("worker: no task id in handler context")
	}
	return h.dispatchApprovalNotify(ctx, taskID, t.Payload())
}

// dispatchApprovalNotify 是 notify 消费核心(asynq 壳之外可单测:指标四分支
// 的零栈断言面)。
func (h *attHandler) dispatchApprovalNotify(ctx context.Context, taskID string, payload []byte) error {
	var p scheduler.ApprovalNotifyPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("worker: task %s payload: %w", taskID, err)
	}
	if p.ExecutionID == 0 || p.NodeExecutionID == 0 {
		return fmt.Errorf("worker: task %s payload: zero id field(s) %+v", taskID, p)
	}
	// 成因词表闸(scheduler.ReasonApproval* 常量单源,P0 仅 escalated;
	// 扩词 = 常量 + reaper 发射分支 + 此闸同 PR 旅行):词表外值 = 发射面
	// 漂移,响亮失败 archived 优于静默发送语义不明的邮件。
	if p.Reason != scheduler.ReasonApprovalEscalated {
		return fmt.Errorf("worker: task %s notify reason %q outside vocabulary", taskID, p.Reason)
	}
	if h.deps.Notifier == nil {
		h.log.Info("approval.notify_degraded",
			"execution_id", p.ExecutionID, "node_execution_id", p.NodeExecutionID,
			"reason", p.Reason, "task_id", taskID,
			"hint", "smtp unconfigured; log-only ack (T4.5)")
		h.mNotify(metrics.NotifyOutcomeDegraded)
		return nil
	}
	err := h.deps.Notifier.NotifyApproval(ctx, p.ExecutionID, p.NodeExecutionID, p.Reason)
	switch {
	case err == nil:
		h.log.Info("approval.notified",
			"execution_id", p.ExecutionID, "node_execution_id", p.NodeExecutionID,
			"reason", p.Reason, "task_id", taskID)
		h.mNotify(metrics.NotifyOutcomeSent)
		return nil
	case errors.Is(err, notify.ErrAlreadyNotified):
		h.log.Info("approval.notify_deduped",
			"execution_id", p.ExecutionID, "node_execution_id", p.NodeExecutionID,
			"reason", p.Reason, "task_id", taskID)
		h.mNotify(metrics.NotifyOutcomeDeduped)
		return nil
	default:
		h.mNotify(metrics.NotifyOutcomeFailed)
		return fmt.Errorf("worker: notify approval node %d (task %s): %w", p.NodeExecutionID, taskID, err)
	}
}

// asynqProbeInterval 是 archived/队列深度探测周期(观测粒度,非运维旋钮)。
const asynqProbeInterval = 30 * time.Second

// probeAsynqArchived 周期汇总各消费队列的 archived 存量。失败记日志继续
// (探测不可用 ≠ worker 不可用;长故障由扫描间隔外的告警静默兜底)。
func (h *attHandler) probeAsynqArchived(ctx context.Context, opt asynq.RedisClientOpt) {
	insp := asynq.NewInspector(opt)
	defer insp.Close() //nolint:errcheck // 停机路径,关闭错误无处置义
	t := time.NewTicker(asynqProbeInterval)
	defer t.Stop()
	probe := func() {
		var archived int
		probed := 0
		for q := range h.cfg.Queues {
			info, err := insp.GetQueueInfo(q)
			if err != nil {
				// 单队列失败(未实例化/瞬断)不清零整个探测面:该队列按
				// 0 计,其余照常(review A1——一个配错队列永久致盲整指标)。
				if errors.Is(err, asynq.ErrQueueNotFound) {
					probed++ // 配置了但从未投递的队列,archived 恒 0,是结论不是盲区
					continue
				}
				h.log.Warn("metrics.asynq_queue", "queue", q, "err", err)
				continue
			}
			archived += info.Archived
			probed++
		}
		// 全队列探测失败(Redis 故障)不写假零(对抗审计 A4):保持上轮
		// 值,死信告警恰在故障期最需要活着;零成功 = 无结论。
		if probed > 0 {
			h.deps.Metrics.AsynqArchived.Set(float64(archived))
		}
	}
	probe()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			probe()
		}
	}
}

// mNotify 是 notify 结果计数的 nil-safe 发射(T6.4)。
func (h *attHandler) mNotify(outcome string) {
	if h.deps.Metrics != nil {
		h.deps.Metrics.NotifyResults.WithLabelValues(outcome).Inc()
	}
}

// heartbeat 周期刷新 attempt 心跳(尽力语义:失败记日志不中断执行——判死
// 归 T2.5,心跳窗口内单次失败不构成失联证据)。
func (h *attHandler) heartbeat(ctx context.Context, attemptID, execID uint64) {
	t := time.NewTicker(h.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			err := engine.WithTx(ctx, h.deps.DB, func(q engine.DBTX) error {
				return engine.TouchAttemptHeartbeat(ctx, q, attemptID, execID)
			})
			if err != nil && ctx.Err() == nil {
				h.log.Warn("heartbeat.fail", "attempt_id", attemptID, "err", err)
			}
		}
	}
}
