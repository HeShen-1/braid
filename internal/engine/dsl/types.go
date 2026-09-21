package dsl

import (
	"encoding/json"
	"fmt"
)

// Workflow 是 DSL v1 文档的结构化模型(schema.json 的 Go 侧契约)。
// metadata.canvas 为画布预留、引擎忽略(Q17),解析时丢弃。
type Workflow struct {
	DSL         string            `json:"dsl"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Inputs      []Input           `json:"inputs"`
	Edges       []Edge            `json:"edges"`
	Nodes       []Node            `json:"nodes"`
	Outputs     map[string]string `json:"outputs"` // 工作流输出:name → 纯引用
}

// Input 工作流触发参数声明。触发值属不可信数据:prompt 中只能经 user_data 进入。
type Input struct {
	Name        string `json:"name"`
	Type        string `json:"type"` // string|number|boolean|object|array
	Required    bool   `json:"required"`
	Description string `json:"description"`
}

// Edge 控制流边。
type Edge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label"`
}

// Node 类型判别后的节点联合:公共字段在此,各类型专有字段在对应指针里,
// 恰有一个非空,与 Type 一致。
type Node struct {
	ID          string `json:"id"`
	Type        string `json:"type"` // llm|tool|branch|approval
	Title       string `json:"title"`
	Description string `json:"description"`
	Retry       *Retry `json:"retry"`

	LLM      *LLMNode      `json:"-"`
	Tool     *ToolNode     `json:"-"`
	Branch   *BranchNode   `json:"-"`
	Approval *ApprovalNode `json:"-"`
}

// UnmarshalJSON 两遍解码:先取公共字段与 type,再解到对应具体结构。
func (n *Node) UnmarshalJSON(b []byte) error {
	var env struct {
		ID          string `json:"id"`
		Type        string `json:"type"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Retry       *Retry `json:"retry"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return err
	}
	n.ID, n.Type, n.Title, n.Description, n.Retry =
		env.ID, env.Type, env.Title, env.Description, env.Retry
	switch env.Type {
	case "llm":
		n.LLM = new(LLMNode)
		return json.Unmarshal(b, n.LLM)
	case "tool":
		n.Tool = new(ToolNode)
		return json.Unmarshal(b, n.Tool)
	case "branch":
		n.Branch = new(BranchNode)
		return json.Unmarshal(b, n.Branch)
	case "approval":
		n.Approval = new(ApprovalNode)
		return json.Unmarshal(b, n.Approval)
	default:
		// schema 已拦截未知类型;到达此处说明调用方跳过了 schema 校验,保持防御。
		return fmt.Errorf("dsl: unknown node type %q (node %q)", env.Type, env.ID)
	}
}

// Retry 节点级重试声明。
type Retry struct {
	MaxAttempts int `json:"max_attempts"`
}

// LLMNode 大模型节点。
type LLMNode struct {
	Model          string            `json:"model"`
	Temperature    float64           `json:"temperature"`
	MaxTokens      int               `json:"max_tokens"`
	ResponseFormat string            `json:"response_format"`
	Prompt         Prompt            `json:"prompt"`
	Outputs        map[string]string `json:"outputs"`
}

// Prompt 指令与数据强制分离:system/template 是受信作者指令(禁 {{...}});
// user_data 是数据通道,运行时以『<name>: 渲染值』标签块追加在 template 之后。
type Prompt struct {
	System   string     `json:"system"`
	Template string     `json:"template"`
	UserData []UserData `json:"user_data"`
}

// UserData 追加式标签块声明:name 为标签名,ref 必须是纯引用。
type UserData struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
}

// ToolNode 工具节点。Inputs 保留原始 JSON:值可为字面量、纯引用或内嵌引用的字符串。
type ToolNode struct {
	Tool    string                     `json:"tool"`
	Inputs  map[string]json.RawMessage `json:"inputs"`
	Outputs map[string]string          `json:"outputs"`
}

// BranchNode 条件分支节点:无 outputs,不可被数据引用。
type BranchNode struct {
	Cases   []BranchCase `json:"cases"`
	Default string       `json:"default"`
}

// BranchCase 单路路由:when 命中则走 next。
type BranchCase struct {
	When Condition `json:"when"`
	Next  string   `json:"next"`
}

// Condition 谓词:Left 为纯引用;Right 可缺省(exists),可为字面量或纯引用字符串。
type Condition struct {
	Left  string          `json:"left"`
	Op    string          `json:"op"`
	Right json.RawMessage `json:"right"`
}

// ApprovalNode 人工审批节点。
type ApprovalNode struct {
	Approvers    []string          `json:"approvers"`
	Mode         string            `json:"mode"`
	TimeoutHours float64           `json:"timeout_hours"`
	OnTimeout    string            `json:"on_timeout"`
	Instructions string            `json:"instructions"`
	Outputs      map[string]string `json:"outputs"`
}
