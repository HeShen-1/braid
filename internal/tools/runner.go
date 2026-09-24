package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// 失败码词表(node_execution.error_code 列开放词表,DDL 前置说明;词表
// 锚点 = internal/db/migrations/0001_init.up.sql 的 error_code 列注释,
// tool_ssrf_blocked 等同族)。业务失败(nodes 侧 Outcome{FAILED} 收口)与
// 基础设施失败(裸 error,attempt 留 RUNNING)的分类单点在本包。
const (
	CodeToolNotFound  = "tool_not_found" // registry 无此名(定义引用了未注册工具)
	CodeTokenExpired  = "tool_token_expired"
	CodeTokenInvalid  = "tool_token_invalid"
	CodeToolFailed    = "tool_failed"     // 工具自身报错(含子进程非零退出)
	CodeBadToolOutput = "bad_tool_output" // 产出非 JSON 对象/声明抽取不履约
	// CodeSSRFBlocked:受防客户端拒连(禁段 IP / 非 https / 越限重定向),
	// 与 DDL node_execution.error_code 注释预留的 tool_ssrf_blocked 对齐。
	// 映射契约:工具(T3.5 http)返回携带 ErrSSRFBlocked 哨兵的裸错误,
	// Run 以 errors.Is 映射到此码——分类单点在 runner,工具不自行包
	// CallError(避免双重包装遮蔽内层码,nodes 消费侧 errors.As 取最外层)。
	CodeSSRFBlocked = "tool_ssrf_blocked"
	// CodeHostBlocked / CodePathBlocked:T3.5 白名单策略拒连(http host 逐跳
	// 白名单 / file_read 目录白名单),独立于 SSRF 守卫分桶——策略拒绝与
	// 网络位置守卫在审计/运维面上是两类事件(开放词表加法)。哨兵
	// ErrHostBlocked/ErrPathBlocked 与 ErrSSRFBlocked 同款映射契约。
	CodeHostBlocked = "tool_host_blocked"
	CodePathBlocked = "tool_path_blocked"
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
		// 拒连哨兵(SSRF 守卫 + 两个白名单门)先于 ctx 门:哨兵存在是拒绝
		// 的确定性证据,ctx.Err() 是环境态(deadline 与拒连同时发生时不得
		// 吞掉业务分类)。
		switch {
		case errors.Is(err, ErrSSRFBlocked):
			return Result{}, &CallError{Code: CodeSSRFBlocked, Err: err}
		case errors.Is(err, ErrHostBlocked):
			return Result{}, &CallError{Code: CodeHostBlocked, Err: err}
		case errors.Is(err, ErrPathBlocked):
			return Result{}, &CallError{Code: CodePathBlocked, Err: err}
		}
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

// DefaultMaxToolOutput 是工具产出序列化后的统一上限(T3.5 审计 MEDIUM:
// json.Marshal 默认 HTML 转义 1 字节→6 字节,8MiB 文本输入最坏放大成
// 48MiB 产出并叠加下游 string/marshal 拷贝链,10 并发瞬时 >1GiB 可 OOM
// worker)。2× 输入上限,吸收结构开销;超出 = bad_tool_output 业务失败。
const DefaultMaxToolOutput = 16 << 20 // 16 MiB

// validateObjectOutput 统一收口工具产出契约:顶层 JSON 对象(声明抽取的
// 原料)+ 尺寸上限。不信任工具自查——CommandTool 原样透传 stdout,进程
// 内工具同样只在此处校验一次。
func validateObjectOutput(out json.RawMessage) error {
	if len(out) == 0 {
		return errors.New("tools: empty tool output")
	}
	if len(out) > DefaultMaxToolOutput {
		return fmt.Errorf("tools: tool output exceeds %d bytes", DefaultMaxToolOutput)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		return fmt.Errorf("tools: tool output is not a JSON object: %w", err)
	}
	return nil
}

// marshalToolOutput 把进程内工具的产出对象序列化为 Result.Output(与
// json.Marshal 键序一致、形态一致,除两条纪律):
//   - SetEscapeHTML(false):<>& 原样,砍掉 6x 放大的主项(下游
//     extractOutputs 自行重序列化,本层无谓放大;产出里 < 不构成注入面,
//     JSON 转义与 HTML 语境无关);
//   - limitBuffer 封顶(CommandTool 同款):超出 DefaultMaxToolOutput =
//     错误返回,序列化内存上界 = 常量而非内容决定(控制字节仍 \u00XX
//     转义,残差放大由此封顶)。
func marshalToolOutput(out map[string]any) (json.RawMessage, error) {
	buf := &limitBuffer{limit: DefaultMaxToolOutput}
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	if buf.overflow {
		return nil, fmt.Errorf("output exceeds %d bytes", DefaultMaxToolOutput)
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}
