package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// 失败码词表(error_code 列,DDL §4 开放词表;tool_* 前缀与 DDL 注释中
// tool_ssrf_blocked 等同族)。业务失败(nodes 侧 Outcome{FAILED} 收口)与
// 基础设施失败(裸 error,attempt 留 RUNNING)的分类单点在本包。
const (
	CodeToolNotFound  = "tool_not_found" // registry 无此名(定义引用了未注册工具)
	CodeTokenExpired  = "tool_token_expired"
	CodeTokenInvalid  = "tool_token_invalid"
	CodeToolFailed    = "tool_failed"     // 工具自身报错(含子进程非零退出)
	CodeBadToolOutput = "bad_tool_output" // 产出非 JSON 对象/声明抽取不履约
)

// CallError 是工具调用的业务失败(收口为节点 FAILED,新 attempt 可重试)。
// 与基础设施失败(error 透传、errors.Is 判 ctx)的分立在 Run 的注释。
type CallError struct {
	Code string
	Err  error
}

func (e *CallError) Error() string { return e.Err.Error() }
func (e *CallError) Unwrap() error { return e.Err }

// RunRequest 是节点执行侧的调用请求。Token 为空时由 Runner 按本次 scope
// 现签(per-call);非空时视为外部注入 token(时钟推进后的重放、P1 回调
// 场景),仍经同一校验门。
type RunRequest struct {
	Tool           string
	Token          string
	Inputs         map[string]any
	NodeExecID     uint64
	AttemptNo      uint16
	IdempotencyKey string
}

// LocalRunner 是 ToolRunner 的 P0 本地实现(ARCHITECTURE §3.4):token
// 校验门 → registry 分派 → 产出契约校验。P1 换 Docker 载体时本编排层与
// registry 不动(载体在 Tool 接口之下替换)。
type LocalRunner struct {
	reg    *Registry
	issuer *TokenIssuer
}

// NewLocalRunner 构造 Runner;nil 依赖 panic(装配缺陷启动即现,nodes.NewMux
// 同款惯例)。
func NewLocalRunner(reg *Registry, issuer *TokenIssuer) *LocalRunner {
	if reg == nil || issuer == nil {
		panic("tools: nil Registry or TokenIssuer")
	}
	return &LocalRunner{reg: reg, issuer: issuer}
}

// Run 执行一次工具调用。错误语义:
//   - 业务失败(未注册、token 过期/无效、工具报错、产出不履约)→ *CallError
//     (nodes 侧按 Outcome{FAILED} 收口);
//   - 基础设施失败(ctx 取消/deadline、工具内部 panic 防线)→ 裸 error
//     透传,attempt 不收口留 RUNNING,asynq 重试接管(与 LLM 执行器同语义)。
func (r *LocalRunner) Run(ctx context.Context, req RunRequest) (Result, error) {
	scope := TokenScope{NodeExecID: req.NodeExecID, AttemptNo: req.AttemptNo, Tool: req.Tool}
	token := req.Token
	if token == "" {
		t, err := r.issuer.Issue(scope)
		if err != nil {
			return Result{}, fmt.Errorf("tools: issue token (node_exec %d): %w", req.NodeExecID, err)
		}
		token = t
	}
	if err := r.issuer.Verify(token, scope); err != nil {
		// 校验门在分派之前:过期/无效 token 下工具完全不被触达(验收语义)。
		switch {
		case errors.Is(err, ErrTokenExpired):
			return Result{}, &CallError{Code: CodeTokenExpired, Err: err}
		default:
			return Result{}, &CallError{Code: CodeTokenInvalid, Err: err}
		}
	}
	tool, ok := r.reg.Get(req.Tool)
	if !ok {
		return Result{}, &CallError{
			Code: CodeToolNotFound,
			Err:  fmt.Errorf("tools: tool %q not in registry", req.Tool),
		}
	}
	res, err := tool.Run(ctx, Call{
		Inputs:         req.Inputs,
		Token:          token,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		if ctx.Err() != nil {
			// 基础设施失败:载体已把 ctx 错误链回,原样上抛不收口。
			return Result{}, fmt.Errorf("tools: %s (node_exec %d): %w", req.Tool, req.NodeExecID, err)
		}
		return Result{}, &CallError{Code: CodeToolFailed, Err: err}
	}
	if err := validateObjectOutput(res.Output); err != nil {
		return Result{}, &CallError{Code: CodeBadToolOutput, Err: err}
	}
	return res, nil
}

// validateObjectOutput 统一收口工具产出契约:顶层 JSON 对象(声明抽取的
// 原料)。不信任工具自查——CommandTool 原样透传 stdout,进程内工具同样
// 只在此处校验一次。
func validateObjectOutput(out json.RawMessage) error {
	if len(out) == 0 {
		return errors.New("tools: empty tool output")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		return fmt.Errorf("tools: tool output is not a JSON object: %w", err)
	}
	return nil
}
