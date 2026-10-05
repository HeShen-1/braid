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
	Parallel *ParallelNode `json:"-"`
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
	case "parallel":
		// parallel 专有字段嵌套在 "parallel" 键下(其余节点类型平铺),多一层拆包。
		var w struct {
			Parallel *ParallelNode `json:"parallel"`
		}
		if err := json.Unmarshal(b, &w); err != nil {
			return err
		}
		if w.Parallel == nil {
			// schema 已 required "parallel";此处为防御(跳过 schema 直调时兜底)。
			return fmt.Errorf("dsl: parallel node %q missing parallel block", env.ID)
		}
		n.Parallel = w.Parallel
		return nil
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
	Next string    `json:"next"`
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
	Mode         string            `json:"mode"` // any=或签(默认)| all=会签(T14.2:全部批准才过、任一驳回即败);判据单点 scheduler.ApprovalOutcome
	TimeoutHours float64           `json:"timeout_hours"`
	OnTimeout    string            `json:"on_timeout"`
	Instructions string            `json:"instructions"`
	Outputs      map[string]string `json:"outputs"`
}

// ParallelNode 并行节点(T9.1 Scatter-Gather,D-2026-09-28-t91):foreach 纯引用
// 指向 array——{{inputs.<name>}}(声明 type=array;T9.1)在 Instantiate 展开为
// N 实例行,{{nodes.<id>.output.<field>}}(祖先节点的 array 输出字段;T19.2
// D-2026-10-05-t192 动态扇出)N 派发位才知,实例行 dispatch-time 展开(Instantiate
// 仅落控制行)。两来源下实例行同构:同 node_id、instance_no 1..N(当前轮输入
// 序)、body 类型;节点自身落控制行(instance 0,gather 锚)。输出契约固定:
// results = body 输出对象的数组,输入序。元素绑定关键字钉死 "item"(自定义名
// 与引用形态钉死冲突,不设旋钮);OnFailure 为空 = fail_fast(生效值由
// scheduler 物化点补齐,approval mode/timeout 同款约定);Body 是 llm|tool 单
// 节点模板(ID 恒空,实例共享父 node_id)。
// MinSuccess(T19.1,D-2026-10-05-t191):部分容忍阈值,[1,N] 整数,0 = 未启用
// (缺省);启用 ⇒ gather 恒物化伴生键 failed_count(输出契约随配置登记),
// 与 OnFailure 正交(失败时机 × 成功阈值两旋钮)。input 来源越界由 Instantiate
// 先验拒;node 来源 N 派发位才知,越界拒移派发位(同错误码语义词)。
type ParallelNode struct {
	Foreach    string `json:"foreach"`
	OnFailure  string `json:"on_failure"`  // 空 = "fail_fast"
	MinSuccess int    `json:"min_success"` // 0 = 未启用;[1,N] 由 scheduler Instantiate 先验
	Body       Node   `json:"body"`        // llm|tool 模板;ID 恒空
}
