package nodes

// LLM 节点执行器(T3.6):input_snapshot(复现真相源)→ prompt 标签块渲染
// → [T18.2 预算闸 pre-call:Σ台账 + 估算超限即响亮拒] → llm.Client 调用
// (平台词表)→ DSL outputs 声明抽取 → 台账桥
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
	"log/slog"
	"strings"
	"unicode/utf8"

	"braid/internal/audit"
	"braid/internal/engine"
	"braid/internal/engine/dsl"
	"braid/internal/llm"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
	"braid/internal/worker"
)

// LLMExecutor 执行 llm 节点。Client 为供应商缝(T3.1,生产 OpenAICompat);
// Prices 承担计费(BRAID_LLM_PRICE_TABLE);DB 只读 input_snapshot
// (engine.ReadNodeInput,复现入口)+ 预算闸门的台账 Σ 点查(T18.2)。
// Budget 是每执行 token 预算闸(D-2026-10-05-t182-token-budget-gate):
// 0 = 关(缺省);IDs 供拒绝审计行 snowflake id,预算关闭时可不装配。
type LLMExecutor struct {
	Client llm.Client
	Prices llm.PriceTable
	DB     *sql.DB
	Budget llm.TokenBudget
	IDs    *snowflake.Generator
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
// StatusError 细分、坏输出、计费溢出、预算拒绝)。适配器已穷尽单 attempt 内
// 瞬态重试,非 ctx 错误到达即业务失败,DSL retry 走新 attempt。
func (e *LLMExecutor) executeCall(ctx context.Context, req worker.Request, in scheduler.LLMInput, spec *dsl.LLMNode) (worker.Outcome, error) {
	msgs := renderMessages(in)
	// T18.2 预算闸 pre-call(D-t182 决策 1):Σ已计费 + 当次估算 + 完成预留
	// 超限即拒,零触达供应商。闸门自身故障 = error 返回(基础设施失败语义)。
	spent, est, reserve, denied, err := e.budgetCheck(ctx, req.ExecutionID, in, msgs)
	if err != nil {
		return worker.Outcome{}, err
	}
	if denied {
		e.emitBudgetDenied(ctx, req, in.Model, spent, est, reserve)
		return failedOutcome("llm_budget_exceeded", fmt.Sprintf(
			"token budget exceeded for execution %d: spent %d + estimated prompt %d + completion reserve %d > budget %d (BRAID_LLM_TOKEN_BUDGET)",
			req.ExecutionID, spent, est, reserve, uint64(e.Budget))), nil
	}
	resp, err := e.Client.ChatCompletion(ctx, llm.Request{
		Model:          in.Model,
		Messages:       msgs,
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

// budgetCheck 评估每执行 token 预算闸(D-t182 决策 1/6):Σ该执行已计费
// usage + 当次 prompt 估算(估算器 T7.2,恒近似)+ max_tokens 完成预留
// (声明时计、未声明 0——预算估算宁可高估)。闸关(0)零查询零估算直通;
// 超限返回 denied=true(业务失败,响亮收口);闸门自身故障(SUM 查询错/
// 估算器错)返回 error = 基础设施失败(attempt 留 RUNNING,asynq 接管)
// ——预算拒绝才是业务失败,闸门故障不是。
func (e *LLMExecutor) budgetCheck(ctx context.Context, execID uint64, in scheduler.LLMInput, msgs []llm.Message) (spent, estPrompt, reserve uint64, denied bool, err error) {
	if !e.Budget.Enabled() {
		return 0, 0, 0, false, nil
	}
	spent, err = e.executionTokensSpent(ctx, execID)
	if err != nil {
		return 0, 0, 0, false, fmt.Errorf("nodes: budget spent sum (exec %d): %w", execID, err)
	}
	est, err := llm.EstimatePromptTokens(in.Model, msgs)
	if err != nil {
		return 0, 0, 0, false, fmt.Errorf("nodes: budget prompt estimate (exec %d): %w", execID, err)
	}
	if in.MaxTokens > 0 {
		reserve = uint64(in.MaxTokens)
	}
	return spent, uint64(est), reserve, e.Budget.Exceeded(spent, uint64(est), reserve), nil
}

// executionTokensSpent Σ 该执行已计费 usage(node_attempt.total_tokens,
// 计费真相源 §2,仅 SUCCEEDED 落账——失败/DEAD/discarded 不进 Σ,系统性
// 低估已登记 D-t182 决策 7②;护栏口径接受)。idx_attempt_exec 免排序
// 聚合点查;SUM 对 INT UNSIGNED 列返回 DECIMAL,driver 以数字串交付,
// convertAssign 直落 uint64。
func (e *LLMExecutor) executionTokensSpent(ctx context.Context, execID uint64) (uint64, error) {
	var spent uint64
	err := e.DB.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(total_tokens), 0) FROM node_attempt WHERE execution_id = ?`,
		execID).Scan(&spent)
	if err != nil {
		return 0, fmt.Errorf("sum node_attempt.total_tokens: %w", err)
	}
	return spent, nil
}

// emitBudgetDenied 落 llm.call.budget_denied 审计行(D-t182 决策 4):拒绝
// 现场独立小事务 best-effort 直写(先例 tool.call.denied——拒绝裁决不受
// 旁路写入失败影响,失败仅 slog);actor=llm/模型名(llm.call.completed
// 同形归因);不携带 usage 列组(调用未发出即无用量)。node_id 需一次
// 点查(执行器上下文只有 node_exec_id)。IDs 未装配时响亮记错跳过
// (装配缺口可观测,拒绝裁决本身不受影响)。
func (e *LLMExecutor) emitBudgetDenied(ctx context.Context, req worker.Request, model string, spent, estPrompt, reserve uint64) {
	if e.IDs == nil {
		slog.Error("nodes: budget denied audit skipped: id generator not wired (assembly bug)",
			"node_exec_id", req.NodeExecutionID, "execution_id", req.ExecutionID)
		return
	}
	var nodeID string
	if err := e.DB.QueryRowContext(ctx,
		`SELECT node_id FROM node_execution WHERE id = ?`, req.NodeExecutionID).Scan(&nodeID); err != nil {
		slog.Error("nodes: budget denied audit node lookup", "err", err, "node_exec_id", req.NodeExecutionID)
		return
	}
	id, err := e.IDs.Next()
	if err != nil {
		slog.Error("nodes: budget denied audit id gen", "err", err, "node_exec_id", req.NodeExecutionID)
		return
	}
	raw, err := json.Marshal(audit.LLMCallBudgetDeniedPayload{
		NodeID: nodeID, Model: model, Budget: uint64(e.Budget), Spent: spent,
		EstimatedPromptTokens: estPrompt, CompletionReserve: reserve,
	})
	if err != nil {
		slog.Error("nodes: marshal budget denied payload", "err", err, "node_exec_id", req.NodeExecutionID)
		return
	}
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		slog.Error("nodes: budget denied audit tx begin", "err", err, "node_exec_id", req.NodeExecutionID)
		return
	}
	// ErrTxDone 时无害(engine/api 同款);best-effort 审计行失败不阻断拒绝收口。
	defer func() { _ = tx.Rollback() }()
	if err := audit.Append(ctx, tx, audit.Event{
		ID: id, EventType: audit.EventLLMBudgetDenied,
		ExecutionID: req.ExecutionID, NodeExecID: req.NodeExecutionID,
		NodeID: nodeID, AttemptID: req.AttemptID,
		// actor_id 列宽 VARCHAR(128) 是字节语义,DSL schema 的 model
		// maxLength=100 是 code point 语义——多字节模型名可合法超列宽,
		// 入列前 rune 安全裁(对抗审计 LOW①);payload.Model 保全文
		// (JSON 无列宽),审计行必落库不因名弃行。
		ActorType: audit.ActorLLM, ActorID: clampActorID(model), Payload: raw,
	}); err != nil {
		slog.Error("nodes: audit budget denied", "err", err, "node_exec_id", req.NodeExecutionID)
		return
	}
	if err := tx.Commit(); err != nil {
		slog.Error("nodes: budget denied audit commit", "err", err, "node_exec_id", req.NodeExecutionID)
	}
}

// clampActorID 把模型名裁到审计 actor_id 列宽内(≤128 字节,rune 边界前向
// 裁防非法 UTF-8——镜像 llm 包 sampleCut 形态;ASCII 恰 128 直通)。
func clampActorID(model string) string {
	const maxActorIDBytes = 128
	if len(model) <= maxActorIDBytes {
		return model
	}
	end := 0
	for end < maxActorIDBytes {
		_, size := utf8.DecodeRuneInString(model[end:])
		if size == 0 || end+size > maxActorIDBytes {
			break
		}
		end += size
	}
	return model[:end]
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
//     (json.Number 原文往返,大整数不失真)+ 数值落 MySQL DOUBLE 范围
//     校验(见 doubleRangeOK);任一缺字段/类型不符/非对象/超范围数值
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
		if err := doubleRangeOK(v, name); err != nil {
			return nil, err
		}
		out[name] = v
	}
	return json.Marshal(out)
}

// doubleRangeOK 递归校验抽取值内的 JSON 数值可落 MySQL DOUBLE 列
// (T3.5 审计 HIGH 修复,管线单点):RFC 8259 数值无范围限制,超
// double 范围的合法 JSON 数(如 1e309)经 UseNumber 原文保真穿到
// node_execution.output(JSON 列)时被 MySQL 以 ERROR 3140 拒绝——
// 收口事务(CloseAttempt+SucceedNode 同事务)整体回滚,attempt 恒
// RUNNING,asynq/reaper 循环重放外部调用且新 attempt 幂等键变化、对
// 非幂等端点形成重复副作用。在抽取单点拦下,覆盖 http body /
// CommandTool stdout / LLM content 三个数值来源。上溢一律拒——数值
// 保真优先,不静默变形;下溢(如 1e-400)Go ParseFloat 归 0 无错误
// 信号,MySQL 同存 0,两侧口径一致放行。
func doubleRangeOK(v any, path string) error {
	switch t := v.(type) {
	case json.Number:
		if _, err := t.Float64(); err != nil {
			return fmt.Errorf("declared output %s: number %s out of double range", path, t.String())
		}
		return nil
	case map[string]any:
		for k, vv := range t {
			if err := doubleRangeOK(vv, path+"."+k); err != nil {
				return err
			}
		}
		return nil
	case []any:
		for i, vv := range t {
			if err := doubleRangeOK(vv, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	default:
		return nil
	}
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
