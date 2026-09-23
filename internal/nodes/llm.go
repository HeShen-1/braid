package nodes

// LLM 节点执行器(T3.6):input_snapshot(复现真相源)→ prompt 标签块渲染
// → llm.Client 调用(平台词表)→ DSL outputs 声明抽取 → 台账桥
// (Cost.AttemptUsage,worker.Outcome.Usage 携带)。
//
// prompt 渲染契约(schema.json prompt 描述的运行时半边):user content =
// template + 逐条 user_data 以『<name>: 渲染值』标签块追加;渲染口径与
// 求值器同源(scheduler.RenderText),指令与数据分离在此收口——受信指令
// 原文透传,不可信数据只经标签块进入。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"braid/internal/engine"
	"braid/internal/engine/dsl"
	"braid/internal/llm"
	"braid/internal/scheduler"
	"braid/internal/worker"
)

// LLMExecutor 执行 llm 节点。Client 为供应商缝(T3.1,生产 OpenAICompat);
// Prices 承担计费(BRAID_LLM_PRICE_TABLE);DB 只读 input_snapshot
// (engine.ReadNodeInput,复现入口)。
type LLMExecutor struct {
	Client llm.Client
	Prices llm.PriceTable
	DB     *sql.DB
}

// Run 执行单个 llm attempt。业务失败(调用穷尽、输出不履约、计费溢出、
// 快照畸形)→ Outcome{FAILED} 收口,节点级重试权威在调度层;基础设施失败
// (读快照 DB 错、调用被 ctx 取消——deadline/停机)→ error 返回,attempt
// 不收口留 RUNNING,asynq 内部重试接管/T2.5 判死(取消不是节点业务失败)。
func (e *LLMExecutor) Run(ctx context.Context, req worker.Request, spec *dsl.LLMNode) (worker.Outcome, error) {
	raw, err := engine.ReadNodeInput(ctx, e.DB, req.NodeExecutionID)
	if err != nil {
		return worker.Outcome{}, fmt.Errorf("nodes: read input snapshot (node_exec %d): %w", req.NodeExecutionID, err)
	}
	if len(raw) == 0 {
		return failedOutcome("bad_input_snapshot",
			fmt.Sprintf("node_execution %d has NULL input_snapshot for RUNNING attempt", req.NodeExecutionID)), nil
	}
	var in scheduler.LLMInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // 数值保真:与求值器同一解码口径
	if err := dec.Decode(&in); err != nil {
		return failedOutcome("bad_input_snapshot", err.Error()), nil
	}
	return e.executeCall(ctx, req, in, spec)
}

// executeCall 是 DB 之后的执行半边(独立函数便于失败路径单测:ctx 分类、
// StatusError 细分、坏输出、计费溢出)。适配器已穷尽单 attempt 内瞬态重试,
// 非 ctx 错误到达即业务失败,DSL retry 走新 attempt。
func (e *LLMExecutor) executeCall(ctx context.Context, req worker.Request, in scheduler.LLMInput, spec *dsl.LLMNode) (worker.Outcome, error) {
	resp, err := e.Client.ChatCompletion(ctx, llm.Request{
		Model:          in.Model,
		Messages:       renderMessages(in),
		Temperature:    in.Temperature,
		MaxTokens:      in.MaxTokens,
		ResponseFormat: in.ResponseFormat, // 平台词表(text|json);方言翻译在适配器
		IdempotencyKey: req.TaskID,        // att:<node_exec_id>:<attempt_no>,供应商侧同键去重
	})
	if err != nil {
		// ctx 取消(deadline/停机)= 基础设施失败:error 返回让 handler 不收口,
		// asynq 重试接管同 attempt 重跑。适配器已把外层取消优先于穷尽包装
		// (openai.go 决策注释),errors.Is 穿透 %w 链恒可判。
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return worker.Outcome{}, fmt.Errorf("nodes: llm call cancelled (node_exec %d): %w", req.NodeExecutionID, err)
		}
		return failedOutcome(callErrorCode(err), err.Error()), nil
	}
	if resp.Model == "" {
		// 台账归因列(AttemptUsage.Model)不容空;缺 model 的 2xx = 供应商
		// 响应不履约,响亮失败可重试好于静默丢归因。
		return failedOutcome("llm_bad_response", "provider 2xx response missing model field (billing attribution)"), nil
	}

	out, err := extractOutputs(resp.Content, spec.Outputs)
	if err != nil {
		return failedOutcome("bad_llm_output", err.Error()), nil
	}

	cost, err := e.Prices.Cost(resp.Model, resp.Usage)
	if err != nil {
		return failedOutcome("llm_cost_overflow", err.Error()), nil
	}
	usage := cost.AttemptUsage(resp.Model, resp.Usage) // 台账双录单点桥(T3.1)
	return worker.Outcome{
		Status: engine.AttemptSucceed,
		Output: out,
		Usage:  &usage,
	}, nil
}

// callErrorCode 细分供应商失败(llm.StatusError 消费契约,DDL §4 error_code
// 词表:429 率是 T6.4 指标字源):429 → llm_rate_limited;其余统一
// llm_call_failed(超时/网络/5xx 已被适配器重试吸收,细分价值留给 T6.4 需求
// 时扩展,不预铺词表)。
func callErrorCode(err error) string {
	var se *llm.StatusError
	if errors.As(err, &se) && se.StatusCode == 429 {
		return "llm_rate_limited"
	}
	return "llm_call_failed"
}

// renderMessages 把快照渲染为 Chat Completions 消息:可选 system(受信指令
// 原文)+ 单条 user(template + user_data 标签块,块间空行分隔)。
func renderMessages(in scheduler.LLMInput) []llm.Message {
	parts := make([]string, 0, len(in.UserData)+1)
	parts = append(parts, in.Template)
	for _, ud := range in.UserData {
		parts = append(parts, ud.Name+": "+scheduler.RenderText(ud.Value))
	}
	msgs := make([]llm.Message, 0, 2)
	if in.System != "" {
		msgs = append(msgs, llm.Message{Role: "system", Content: in.System})
	}
	msgs = append(msgs, llm.Message{Role: "user", Content: strings.Join(parts, "\n\n")})
	return msgs
}

// extractOutputs 把响应 content 物化为节点输出:
//   - 无声明 → {"content": 全文}(仅供审计观察;T1.2 保证未声明字段不可被
//     引用,语义上等价无输出但不丢运行证据);
//   - 有声明 → content 必须是 JSON 对象,逐声明字段抽取并校验类型
//     (json.Number 原文往返,大整数不失真);任一缺字段/类型不符/非对象
//     即失败——声明即契约,响亮失败优于静默错值(GATE 1 语义 a)。
func extractOutputs(content string, outs map[string]string) (json.RawMessage, error) {
	if len(outs) == 0 {
		return json.Marshal(map[string]any{"content": content})
	}
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(content))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("outputs declared but content is not a JSON object: %w", err)
	}
	out := make(map[string]any, len(outs))
	for name, decl := range outs {
		v, ok := m[name]
		if !ok {
			return nil, fmt.Errorf("declared output %q missing in response", name)
		}
		if !outputTypeMatches(v, decl) {
			return nil, fmt.Errorf("declared output %q wants %s, got %T", name, decl, v)
		}
		out[name] = v
	}
	return json.Marshal(out)
}

// outputTypeMatches 校验值与 DSL 类型声明词表(string|number|boolean|object|array)。
func outputTypeMatches(v any, decl string) bool {
	switch decl {
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	default:
		return false // 词表外声明(schema 已拦;防御跳过 schema 的直调方)
	}
}

// failedOutcome 构造业务失败收口;msg 过 clampMessage(rune 安全截断 + 单行化,
// VARCHAR(2000) 是字符语义,字节切割会产生非法 UTF-8 被 MySQL strict mode
// 1366 拒绝 → 收口事务确定性失败 → attempt 卡 RUNNING 被 reaper 循环重派
// 重复计费——T3.6 对抗审计实证路径)。
func failedOutcome(code, msg string) worker.Outcome {
	return worker.Outcome{Status: engine.AttemptFailed, Code: code, Msg: clampMessage(msg)}
}

// clampMessage 把供应商错误文本整形成可安全入列形态:换行单行化(防日志
// 伪造记录,llm 包 CredentialsError %q 同款动机)+ 按 rune 截断到 2000 字符
// (llm.clampRunes 同款习惯;跨 rune 字节切割见 failedOutcome 注释)。
func clampMessage(msg string) string {
	msg = strings.ReplaceAll(msg, "\r", "\\r")
	msg = strings.ReplaceAll(msg, "\n", "\\n")
	if r := []rune(msg); len(r) > 2000 {
		return string(r[:2000])
	}
	return msg
}
