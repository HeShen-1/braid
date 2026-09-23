package scheduler

import (
	"bytes"
	"encoding/json"
	"fmt"

	"braid/internal/engine/dsl"
)

// 输入快照求值器(T2.4):在派发事务内把节点 DSL 的全部 {{...}} 引用对
// workflow_execution.input 根 + 已终态节点 output 求值,产出 ClaimNode 的
// input_snapshot(契约:已解析 JSON,复现真相源)。
//
// 作用域 = 祖先而非直接前驱(T1.2 校验口径):按 node_id 解析全执行任一节点;
// 派发时全部祖先已可证 SUCCEEDED(任一路径的首边被 claim 的前提是全部前驱
// SUCCEEDED,归纳成立),output 终态不可变 ⇒ 求值无 TOCTOU。
//
// JSON 数值保真:解码一律 UseNumber(json.Number 原文往返),避免 float64
// 大整数精度失真。

// refEnv 是纯引用的解析环境:工作流输入 + 节点输出(node_id → output 对象)。
// SKIPPED 节点天然不在 outputs 里(无 output)——工作流级 outputs 引用被剪枝臂
// 时解析为 null(validate 豁免其祖先约束,合法场景);节点快照求值语境下未命中
// 则是错误(调用方决定语义)。
type refEnv struct {
	input   map[string]any
	outputs map[string]map[string]any
}

// newRefEnv 构造解析环境。wfInput 为空按 {} 处理;outputs 的 nil/非对象值
// (数据缺陷)按「该节点无可用输出」处理,引用落空交由调用方裁决。
func newRefEnv(wfInput json.RawMessage, outputs map[string]json.RawMessage) (*refEnv, error) {
	e := &refEnv{
		input:   map[string]any{},
		outputs: make(map[string]map[string]any, len(outputs)),
	}
	if len(wfInput) > 0 {
		if err := unmarshalUseNumber(wfInput, &e.input); err != nil {
			return nil, fmt.Errorf("scheduler: workflow input: %w", err)
		}
	}
	for id, raw := range outputs {
		if len(raw) == 0 {
			continue
		}
		var m map[string]any
		if err := unmarshalUseNumber(raw, &m); err != nil {
			continue // 非对象 output:引用落空,不炸构造
		}
		e.outputs[id] = m
	}
	return e, nil
}

// resolve 解析结构化引用;ok=false = 未命中(声明外输入/未知节点/字段缺失)。
func (e *refEnv) resolve(r dsl.Ref) (any, bool) {
	switch r.Kind {
	case "input":
		v, ok := e.input[r.Name]
		return v, ok
	case "node":
		m := e.outputs[r.Name]
		if m == nil {
			return nil, false
		}
		v, ok := m[r.Field]
		return v, ok
	}
	return nil, false
}

// resolveText 解析纯引用原文(形态钉死由 dsl.ParsePureRef 承担)。
func (e *refEnv) resolveText(ref string) (any, bool) {
	r, ok := dsl.ParsePureRef(ref)
	if !ok {
		return nil, false
	}
	return e.resolve(r)
}

func unmarshalUseNumber(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(v)
}

// buildSnapshot 产出节点 input_snapshot(形状按类型文档化,T3.x executor 消费):
//   - llm:      {system, template, user_data:[{name,value}], model, temperature, max_tokens, response_format}
//   - tool:     {tool, inputs:{k:已解析}} —— 纯引用串 → 引用值(保类型);内嵌引用 → 文本替换
//   - approval: {approvers, mode, timeout_hours, on_timeout, instructions}(静态;T4.1 前按普通节点派发)
//   - branch:   不经此处——branch 快照 = branchOperands(operands 形状,路由前构建)
func buildSnapshot(n *dsl.Node, env *refEnv) (json.RawMessage, error) {
	switch {
	case n.LLM != nil:
		return llmSnapshot(n, env)
	case n.Tool != nil:
		return toolSnapshot(n, env)
	case n.Approval != nil:
		return json.Marshal(approvalSnapshotData{
			Approvers:    n.Approval.Approvers,
			Mode:         n.Approval.Mode,
			TimeoutHours: n.Approval.TimeoutHours,
			OnTimeout:    n.Approval.OnTimeout,
			Instructions: n.Approval.Instructions,
		})
	}
	return nil, fmt.Errorf("scheduler: node %s type %s has no snapshot builder", n.ID, n.Type)
}

// LLMInput 是 llm 节点 input_snapshot 的形状契约(执行器消费单源:T3.6
// nodes 包按本类型解码,形状漂移在编译期暴露而非运行期 JSON 缺字段)。
type LLMInput struct {
	System         string          `json:"system"`
	Template       string          `json:"template"`
	UserData       []LLMInputDatum `json:"user_data"`
	Model          string          `json:"model"`
	Temperature    float64         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens"`
	ResponseFormat string          `json:"response_format"`
}

// LLMInputDatum 是单条已求值 user_data(name 标签名 + 解析后值)。
type LLMInputDatum struct {
	Name  string `json:"name"`
	Value any    `json:"value"`
}

func llmSnapshot(n *dsl.Node, env *refEnv) (json.RawMessage, error) {
	p := n.LLM.Prompt
	data := LLMInput{
		System:         p.System,
		Template:       p.Template,
		UserData:       make([]LLMInputDatum, 0, len(p.UserData)),
		Model:          n.LLM.Model,
		Temperature:    n.LLM.Temperature,
		MaxTokens:      n.LLM.MaxTokens,
		ResponseFormat: n.LLM.ResponseFormat,
	}
	for _, ud := range p.UserData {
		r, ok := dsl.ParsePureRef(ud.Ref)
		if !ok {
			return nil, fmt.Errorf("node %s user_data %s: %s is not a pure reference", n.ID, ud.Name, ud.Ref)
		}
		v, ok := env.resolve(r)
		if !ok {
			return nil, fmt.Errorf("node %s user_data %s: ref %s unresolved", n.ID, ud.Name, ud.Ref)
		}
		data.UserData = append(data.UserData, LLMInputDatum{Name: ud.Name, Value: v})
	}
	return json.Marshal(data)
}

type toolSnapshotData struct {
	Tool   string         `json:"tool"`
	Inputs map[string]any `json:"inputs"`
}

func toolSnapshot(n *dsl.Node, env *refEnv) (json.RawMessage, error) {
	data := toolSnapshotData{Tool: n.Tool.Tool, Inputs: make(map[string]any, len(n.Tool.Inputs))}
	for k, raw := range n.Tool.Inputs {
		v, err := evalToolValue(raw, env, n.ID, k)
		if err != nil {
			return nil, err
		}
		data.Inputs[k] = v
	}
	return json.Marshal(data)
}

// evalToolValue 解析 tool.inputs 单值:字符串为纯引用 → 引用值(保类型);
// 含内嵌引用 → 文本替换(字符串值按原文、其余按紧凑 JSON);数组/对象递归;
// 其余字面量原样。未命中引用 = 错误(输入不完整,节点不可派发)。
func evalToolValue(raw json.RawMessage, env *refEnv, nodeID, key string) (any, error) {
	var v any
	if err := unmarshalUseNumber(raw, &v); err != nil {
		return nil, fmt.Errorf("node %s tool.inputs %s: %w", nodeID, key, err)
	}
	return evalToolAny(v, env, nodeID, key)
}

func evalToolAny(v any, env *refEnv, nodeID, path string) (any, error) {
	switch x := v.(type) {
	case string:
		if r, ok := dsl.ParsePureRef(x); ok {
			rv, ok := env.resolve(r)
			if !ok {
				return nil, fmt.Errorf("node %s tool.inputs %s: ref %s unresolved", nodeID, path, x)
			}
			return rv, nil
		}
		out, miss := dsl.ExpandStringRefs(x, func(r dsl.Ref) (string, bool) {
			rv, ok := env.resolve(r)
			if !ok {
				return "", false
			}
			return RenderText(rv), true
		})
		if miss {
			return nil, fmt.Errorf("node %s tool.inputs %s: embedded reference unresolved", nodeID, path)
		}
		return out, nil
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, el := range x {
			ev, err := evalToolAny(el, env, nodeID, path+"."+k)
			if err != nil {
				return nil, err
			}
			m[k] = ev
		}
		return m, nil
	case []any:
		s := make([]any, len(x))
		for i, el := range x {
			ev, err := evalToolAny(el, env, nodeID, fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return nil, err
			}
			s[i] = ev
		}
		return s, nil
	}
	return v, nil
}

// RenderText 渲染值为替换/拼接文本:字符串按原文(拼接语境不加引号),
// 其余按紧凑 JSON(json.Number 原文往返)。导出 = 求值器与节点执行器
// (T3.6 prompt user_data 标签块)同一渲染口径,漂移面归零。
func RenderText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

type approvalSnapshotData struct {
	Approvers    []string `json:"approvers"`
	Mode         string   `json:"mode"`
	TimeoutHours float64  `json:"timeout_hours"`
	OnTimeout    string   `json:"on_timeout"`
	Instructions string   `json:"instructions"`
}

// branchOperands 构建 branch 节点的 input_snapshot:{"operands":{引用原文:值}}。
// 收集 cases 全部纯引用(when.left 恒为;when.right 为引用字符串时亦入),仅含
// 命中项(未命中即随后的路由错误,快照不伪造 null)。形状对 T3.4 稳定:完整
// 求值器需要同一组操作数。
func branchOperands(b *dsl.BranchNode, env *refEnv) json.RawMessage {
	op := map[string]any{}
	add := func(refText string) {
		if v, ok := env.resolveText(refText); ok {
			op[refText] = v
		}
	}
	for i := range b.Cases {
		add(b.Cases[i].When.Left)
		if s := rightRefText(b.Cases[i].When.Right); s != "" {
			add(s)
		}
	}
	out, err := json.Marshal(map[string]any{"operands": op})
	if err != nil {
		return []byte(`{"operands":{}}`)
	}
	return out
}

// rightRefText 提取 when.right 的纯引用字符串形态;字面量(非引用)返回空。
func rightRefText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	if _, ok := dsl.ParsePureRef(s); !ok {
		return ""
	}
	return s
}

// materializeOutputs 物化工作流级 outputs(name → 纯引用):引用被剪枝臂/
// 未产出节点 → null(键保留——形状跨路由稳定,DECISIONS D-2026-09-22-t24)。
// map 序列化键有序 ⇒ 输出确定性。
func materializeOutputs(outs map[string]string, env *refEnv) json.RawMessage {
	m := make(map[string]any, len(outs))
	for name, ref := range outs {
		if v, ok := env.resolveText(ref); ok {
			m[name] = v
		} else {
			m[name] = nil
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}
