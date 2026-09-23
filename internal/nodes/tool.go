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
		// ctx 取消(deadline/停机)= 基础设施失败:error 返回留 RUNNING,
		// asynq 重试接管同 attempt 重跑(LocalRunner 已把 ctx 错误链回)。
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return worker.Outcome{}, fmt.Errorf("nodes: tool call cancelled (node_exec %d): %w", req.NodeExecutionID, err)
		}
		var ce *tools.CallError
		if errors.As(err, &ce) {
			return failedOutcome(ce.Code, err.Error()), nil
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
