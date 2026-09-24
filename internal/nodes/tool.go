package nodes

// Tool 节点执行器(T3.2):input_snapshot(求值真相源)→ tools.ToolRunner
// 本地实现(token 门 + registry 分派)→ DSL outputs 声明抽取 → 调用观测
// 桥(scheduler.ToolCallInfo,随收口发 tool.call.completed 审计行)。
//
// 凭证纪律(验收面):scoped token 由 runner per-call 签发并经 Call 传递
// (子进程载体 = env 白名单注入);本执行器与审计 payload 都不触碰 token
// 本体,平台主凭证全链路结构性不可达。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"braid/internal/engine"
	"braid/internal/engine/dsl"
	"braid/internal/scheduler"
	"braid/internal/tools"
	"braid/internal/worker"
)

// ToolRunner 是 tool 节点的执行面(Mux 内部路由缝;生产实现唯一
// *ToolExecutor,fake 只存在于本包 _test.go——依赖倒置同 LLMRunner)。
type ToolRunner interface {
	Run(ctx context.Context, req worker.Request, spec *dsl.ToolNode) (worker.Outcome, error)
}

// ToolRuntime 是工具运行时缝(ToolExecutor 内层;生产实现唯一
// *tools.LocalRunner,按 tools.RunRequest 编排 token 门/registry/载体)。
type ToolRuntime interface {
	Run(ctx context.Context, req tools.RunRequest) (tools.Result, error)
}

// ToolExecutor 执行 tool 节点。Runner 为工具运行时缝(T3.2,生产
// LocalRunner + Registry);DB 只读 input_snapshot(engine.ReadNodeInput)。
type ToolExecutor struct {
	Runner ToolRuntime
	DB     *sql.DB
}

// Run 执行单个 tool attempt。业务失败(工具未注册、token 过期/无效、工具
// 报错、产出不履约、快照畸形)→ Outcome{FAILED} 收口;基础设施失败(读
// 快照 DB 错、调用被 ctx 取消)→ error 返回,attempt 不收口留 RUNNING,
// asynq 重试接管/T2.5 判死——语义与 LLMExecutor 逐字对齐。
func (e *ToolExecutor) Run(ctx context.Context, req worker.Request, spec *dsl.ToolNode) (worker.Outcome, error) {
	raw, err := engine.ReadNodeInput(ctx, e.DB, req.NodeExecutionID)
	if err != nil {
		return worker.Outcome{}, fmt.Errorf("nodes: read input snapshot (node_exec %d): %w", req.NodeExecutionID, err)
	}
	if len(raw) == 0 {
		return failedOutcome("bad_input_snapshot",
			fmt.Sprintf("node_execution %d has NULL input_snapshot for RUNNING attempt", req.NodeExecutionID)), nil
	}
	var in scheduler.ToolInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // 数值保真:与求值器同一解码口径
	if err := dec.Decode(&in); err != nil {
		return failedOutcome("bad_input_snapshot", err.Error()), nil
	}
	return e.executeCall(ctx, req, in, spec)
}

// executeCall 是 DB 之后的执行半边(独立函数便于失败路径单测:失败码分类、
// ctx 分类、声明抽取)。失败码词表单源在 tools 包(CodeToolNotFound 等),
// 本层只透传与兜底。
func (e *ToolExecutor) executeCall(ctx context.Context, req worker.Request, in scheduler.ToolInput, spec *dsl.ToolNode) (worker.Outcome, error) {
	// 预算 ctx(T3.5 审计 HIGH 修复):全链(asynq handler ctx 原样下传、
	// relay 入队不带 Timeout)此前无任何一层派生 deadline,真实外层 =
	// asynq 默认 30min,与 tool 节点 10min SLA(deadline_at,reaper 判死)
	// 脱节——挂死端点(完成 TLS 后不回响应头)让僵尸调用每轮净占 2 个
	// worker 槽 20min。以 NodeTimeout("tool") 单源锚定预算:ctx 到期 =
	// 基础设施失败留 RUNNING(reaper 按 SLA 判死重派),worker 槽位与
	// DB deadline 对齐。file_read 同链受益(读前检查;os.Open 无 ctx
	// 中断原语,NFS 挂死仍由 reaper 兜底)。
	ctx, cancel := context.WithTimeout(ctx, scheduler.NodeTimeout("tool"))
	defer cancel()
	start := time.Now()
	res, err := e.Runner.Run(ctx, tools.RunRequest{
		Tool:           in.Tool,
		Inputs:         in.Inputs,
		NodeExecID:     req.NodeExecutionID,
		AttemptNo:      req.AttemptNo,
		IdempotencyKey: req.TaskID, // att:<node_exec_id>:<attempt_no>,工具侧幂等键传递面
	})
	duration := uint64(time.Since(start).Milliseconds())
	if err != nil {
		// 分类优先序(CallError 先于 ctx 门):runner 对「哨兵 + ctx 同时
		// 到期」已按哨兵确定性裁成业务失败,本层同序尊重;纯 ctx 取消
		// (deadline/停机)= 基础设施失败,error 返回留 RUNNING,asynq
		// 重试接管同 attempt 重跑。ctx.Err() 环境态门兜在链判定之后:
		// deadline 触发与传输错误竞态时 url.Error 只包传输错不含 ctx 链
		// (T3.5 审计 MEDIUM:双层谓词不一致曾使该现场落兜底 tool_failed)。
		var ce *tools.CallError
		if errors.As(err, &ce) {
			return failedOutcome(ce.Code, err.Error()), nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return worker.Outcome{}, fmt.Errorf("nodes: tool call cancelled (node_exec %d): %w", req.NodeExecutionID, err)
		}
		if errors.Is(err, tools.ErrAuthorizerUnavailable) {
			// 授权面基础设施故障(T5.2 go 评审 HIGH):attempt 留 RUNNING
			// 由重试接管——不落未分类兜底误收口 tool_failed(默认
			// max_attempts=1 的一次性业务失败)。
			return worker.Outcome{}, fmt.Errorf("nodes: tool call authorization failed (node_exec %d): %w", req.NodeExecutionID, err)
		}
		return failedOutcome(tools.CodeToolFailed, err.Error()), nil // 未分类兜底(防御,正常路径不达)
	}
	out, err := extractOutputs(string(res.Output), spec.Outputs)
	if err != nil {
		return failedOutcome(tools.CodeBadToolOutput, err.Error()), nil
	}
	return worker.Outcome{
		Status:   engine.AttemptSucceed,
		Output:   out,
		ToolCall: &scheduler.ToolCallInfo{Name: in.Tool, DurationMs: duration},
	}, nil
}
