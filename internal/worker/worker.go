// Package worker 实现 T2.3 Asynq 执行器:消费 relay 投递的任务,以 attempt
// 唯一键为执行级幂等锚,完成节点 attempt 的记录、执行与收口。
//
// 引擎消费链上的位置:
//
//	派发方(T2.4 调度器)事务:ClaimNode(PENDING→EXECUTING,attempts+1)+
//	    outbox.Append(event_type=node.task_enqueue,
//	                  task_id=att:<node_execution_id>:<attempt_no>)
//	→ relay 投递(MaxRetry=1、确定性 TaskID,T2.2)
//	→ 本包:mux 按 event_type 路由 → claimAttempt(幂等锚)→ Executor
//	    执行缝(心跳并行)→ 同事务收口 attempt + 刷 execution 活跃时刻
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

	"braid/internal/engine"
	"braid/internal/snowflake"
)

// EventTypeNodeTask 是 attempt 派发事件的 event_type,亦是 asynq task type
// (mux 路由键;relay 侧 task type = event_type 的约定承接)。exec:/resume:
// 事件的 handler 注册点归 T2.4(mux 天然可扩展)。
const EventTypeNodeTask = "node.task_enqueue"

// nodeTaskPayload 是 node.task_enqueue 事件的自描述体(outbox 契约:仅 id
// 引用,业务大数据留在业务表由 worker 回读)。attempt_no 由派发方从
// node_execution.attempts+1 钉进 TaskID 与本体;TaskID 字符串本身
// (att:<node_execution_id>:<attempt_no>)不作为数据载体被解析。
type nodeTaskPayload struct {
	ExecutionID     uint64 `json:"execution_id"`
	NodeExecutionID uint64 `json:"node_execution_id"`
	AttemptNo       uint16 `json:"attempt_no"`
}

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
type Outcome struct {
	Status engine.AttemptState
	Code   string
	Msg    string
}

// Executor 是节点执行缝:T2.3 只落框架与幂等/记账契约,T2.4 接调度回写,
// T3.x 接真实节点实现(llm/tool)。与 relay.Enqueuer 同款依赖倒置:生产实现
// 唯一,fake 只存在于 _test.go。
type Executor interface {
	Execute(ctx context.Context, req Request) (Outcome, error)
}

// Deps 是运行依赖。Logger 为 nil 时用 slog.Default()。
type Deps struct {
	DB       *sql.DB
	IDs      *snowflake.Generator // attempt 行 id 生成(应用侧 Snowflake)
	Executor Executor
	Logger   *slog.Logger
}

// Run 阻塞运行 worker 直到 ctx 取消(优雅停机受 cfg.ShutdownTimeout 界住,
// 超时强杀的在飞 attempt 留 RUNNING 归 T2.5 判死)。返回非 nil 仅当依赖接线
// 缺损或 server 启动失败;运行期 handler 错误由 asynq 自管(重试→archived)。
func Run(ctx context.Context, d Deps, cfg Config, opt asynq.RedisClientOpt) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if d.DB == nil || d.IDs == nil || d.Executor == nil {
		return errors.New("worker: nil DB, IDs or Executor")
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	h := &attHandler{deps: d, cfg: cfg, log: log}

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
		Concurrency:      cfg.Concurrency,
		Queues:           cfg.Queues,
		ShutdownTimeout:  cfg.ShutdownTimeout,
		RetryDelayFunc:   func(n int, e error, t *asynq.Task) time.Duration { return cfg.RetryDelay },
	})
	mux := asynq.NewServeMux()
	mux.HandleFunc(EventTypeNodeTask, h.handleNodeTask)

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
	return h.closeAttempt(ctx, attemptID, p.ExecutionID, outcome)
}

// claimAttempt 是执行级幂等锚:首达 InsertAttempt 建行;撞 uk_attempt(1062
// → engine.ErrConflict)读行裁决——终态返回 0(幂等返回),RUNNING 返回行 id
// (接管:上一发未收口,asynq 内部重试 / T2.5 判死重入队后重跑)。
func (h *attHandler) claimAttempt(ctx context.Context, taskID string, p nodeTaskPayload) (uint64, error) {
	id, err := h.deps.IDs.Next()
	if err != nil {
		return 0, fmt.Errorf("worker: gen attempt id: %w", err)
	}
	err = engine.InsertAttempt(ctx, h.deps.DB, engine.AttemptParams{
		ID:             id,
		NodeExecID:     p.NodeExecutionID,
		ExecID:         p.ExecutionID,
		AttemptNo:      p.AttemptNo,
		WorkerID:       h.cfg.WorkerID,
		IdempotencyKey: taskID,
		AsynqTaskID:    taskID,
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
		// 陈旧度 ≈ RetryDelay + HeartbeatInterval(默认 15s),不重置则 T2.5
		// 判死 grace 必须显式覆盖该缺口(该假设已钉进 T2.5 issue)。尽力
		// 语义:失败记日志不阻断接管——本发执行的常规心跳随即接管窗口。
		if err := engine.WithTx(ctx, h.deps.DB, func(q engine.DBTX) error {
			return engine.TouchAttemptHeartbeat(ctx, q, rowID, p.ExecutionID)
		}); err != nil {
			h.log.Warn("takeover.heartbeat_reset_fail", "attempt_id", rowID, "err", err)
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

// closeAttempt 同事务收口 attempt + 刷 execution 活跃时刻(DDL:任一 attempt
// 推进同事务刷新 last_activity_at)。CloseAttempt 撞 ErrConflict(行已被并发
// 收口 / T2.5 置 DEAD)→ 幂等返回:终局已定,不重复裁决。
func (h *attHandler) closeAttempt(ctx context.Context, attemptID, execID uint64, o Outcome) error {
	err := engine.WithTx(ctx, h.deps.DB, func(q engine.DBTX) error {
		if err := engine.CloseAttempt(ctx, q, attemptID, o.Status, o.Code, o.Msg); err != nil {
			return err
		}
		return engine.TouchExecution(ctx, q, execID)
	})
	if errors.Is(err, engine.ErrConflict) {
		h.log.Info("attempt.close_conflict", "attempt_id", attemptID, "status", o.Status)
		return nil
	}
	if err != nil {
		return fmt.Errorf("worker: close attempt %d: %w", attemptID, err)
	}
	h.log.Info("attempt.closed", "attempt_id", attemptID, "status", o.Status)
	return nil
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
