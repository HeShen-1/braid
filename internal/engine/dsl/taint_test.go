package dsl

import (
	"encoding/json"
	"reflect"
	"testing"
)

// T18.3 静态污点分析(D-2026-10-05-t183-taint-tracking):表驱动锚定
// 传递源路径集。结构性前提(不在此重证):引用形态被 refs.go 正则钉死为
// 单段标识符,paths 宇宙 ⊆ DSL 声明 inputs 词表——调用方可控字节不可达。

// llmT 构造 llm 节点,user_data 逐条携带 refs。
func llmT(id string, refs ...string) *Node {
	n := &Node{ID: id, Type: "llm", LLM: &LLMNode{}}
	for _, r := range refs {
		n.LLM.Prompt.UserData = append(n.LLM.Prompt.UserData, UserData{Name: "d", Ref: r})
	}
	return n
}

// toolT 构造 tool 节点,inputs 为 字段→JSON 值(字符串可内嵌引用)。
func toolT(id string, inputs map[string]json.RawMessage) *Node {
	return &Node{ID: id, Type: "tool", Tool: &ToolNode{Tool: "t", Inputs: inputs}}
}

func TestComputeTaintSets(t *testing.T) {
	cases := []struct {
		name string
		wf   *Workflow
		// want: nodeID → 期望传递源路径(升序);不在 map 的节点必须 Sources==nil。
		want map[string][]string
	}{
		{
			name: "llm direct user_data ref",
			wf: &Workflow{Nodes: []Node{
				*llmT("a", "{{inputs.bio}}"),
			}},
			want: map[string][]string{"a": {"inputs.bio"}},
		},
		{
			name: "llm no refs → clean",
			wf: &Workflow{Nodes: []Node{
				*llmT("a"),
			}},
			want: map[string][]string{},
		},
		{
			name: "tool embedded refs + transitive through node output",
			wf: &Workflow{Nodes: []Node{
				*llmT("gen", "{{inputs.y}}"),
				*toolT("agg", map[string]json.RawMessage{
					"cmd": json.RawMessage(`"sum {{inputs.x}} and {{nodes.gen.output.o}}"`),
					"n":   json.RawMessage(`42`),
				}),
			}},
			want: map[string][]string{
				"gen": {"inputs.y"},
				"agg": {"inputs.x", "inputs.y"},
			},
		},
		{
			name: "tool nested JSON string values are scanned",
			wf: &Workflow{Nodes: []Node{
				*toolT("t", map[string]json.RawMessage{
					"cfg": json.RawMessage(`{"inner":"v={{inputs.deep}}","arr":["{{inputs.arr2}}"]}`),
				}),
			}},
			want: map[string][]string{"t": {"inputs.arr2", "inputs.deep"}},
		},
		{
			name: "branch operands both arms",
			wf: &Workflow{Nodes: []Node{
				{ID: "br", Type: "branch", Branch: &BranchNode{Cases: []BranchCase{{
					When: Condition{Left: "{{inputs.flag}}", Op: "gt", Right: json.RawMessage(`"{{inputs.baseline}}"`)},
					Next: "x",
				}}}},
			}},
			want: map[string][]string{"br": {"inputs.baseline", "inputs.flag"}},
		},
		{
			name: "approval has no data channel (outputs are type decls) → always clean",
			wf: &Workflow{Nodes: []Node{
				{ID: "ap", Type: "approval", Approval: &ApprovalNode{
					Approvers: []string{"carol"},
					Outputs:   map[string]string{"comment": "string"},
				}},
			}},
			want: map[string][]string{},
		},
		{
			name: "parallel foreach input + body item and direct refs",
			wf: &Workflow{Nodes: []Node{
				{ID: "par", Type: "parallel", Parallel: &ParallelNode{
					Foreach: "{{inputs.rows}}",
					Body:    *llmT("", "{{item.title}}", "{{inputs.model}}"),
				}},
			}},
			want: map[string][]string{"par": {"inputs.model", "inputs.rows"}},
		},
		{
			name: "parallel foreach node-output dep inherits transitively",
			wf: &Workflow{Nodes: []Node{
				*toolT("gen", map[string]json.RawMessage{"seed": json.RawMessage(`"{{inputs.seed}}"`)}),
				{ID: "par", Type: "parallel", Parallel: &ParallelNode{
					Foreach: "{{nodes.gen.output.list}}",
					Body:    *llmT("", "{{item.t}}"),
				}},
			}},
			want: map[string][]string{
				"gen": {"inputs.seed"},
				"par": {"inputs.seed"},
			},
		},
		{
			name: "ref to branch node output contributes nothing",
			wf: &Workflow{Nodes: []Node{
				{ID: "br", Type: "branch", Branch: &BranchNode{Default: "x"}},
				*toolT("t", map[string]json.RawMessage{
					"v": json.RawMessage(`"{{nodes.br.output.f}}"`),
				}),
			}},
			want: map[string][]string{},
		},
		{
			name: "undeclared input name still recorded (analysis independent of validation)",
			wf: &Workflow{
				Inputs: []Input{{Name: "known", Type: "string"}},
				Nodes:  []Node{*llmT("a", "{{inputs.ghost}}")},
			},
			want: map[string][]string{"a": {"inputs.ghost"}},
		},
		{
			name: "workflow top-level outputs are not a node and add nothing",
			wf: &Workflow{
				Outputs: map[string]string{"out": "{{inputs.z}}"},
				Nodes:   []Node{*llmT("a")},
			},
			want: map[string][]string{},
		},
		{
			name: "out-of-range number literal does not kill sibling ref scanning (UseNumber decode)",
			wf: &Workflow{Nodes: []Node{
				*toolT("t", map[string]json.RawMessage{
					"big": json.RawMessage(`1e999`),
					"cmd": json.RawMessage(`"{{inputs.x}}"`),
				}),
			}},
			want: map[string][]string{"t": {"inputs.x"}},
		},
		{
			name: "multi-predecessor union with chained transitivity",
			wf: &Workflow{Nodes: []Node{
				*llmT("a", "{{inputs.x}}"),
				*llmT("b", "{{inputs.y}}"),
				*toolT("c", map[string]json.RawMessage{
					"p": json.RawMessage(`"{{nodes.a.output.f}}"`),
					"q": json.RawMessage(`"{{nodes.b.output.g}}"`),
					"z": json.RawMessage(`"{{inputs.z}}"`),
				}),
			}},
			want: map[string][]string{
				"a": {"inputs.x"},
				"b": {"inputs.y"},
				"c": {"inputs.x", "inputs.y", "inputs.z"},
			},
		},
		{
			name: "cycle converges to union (defensive; T1.2 rejects upstream)",
			wf: &Workflow{Nodes: []Node{
				*toolT("a", map[string]json.RawMessage{
					"x": json.RawMessage(`"{{nodes.b.output.f}}"`),
					"s": json.RawMessage(`"{{inputs.src}}"`),
				}),
				*toolT("b", map[string]json.RawMessage{
					"y": json.RawMessage(`"{{nodes.a.output.f}}"`),
				}),
			}},
			want: map[string][]string{
				"a": {"inputs.src"},
				"b": {"inputs.src"},
			},
		},
		{
			name: "type-payload mismatch (nil payload) tolerated without panic",
			wf: &Workflow{Nodes: []Node{
				{ID: "ghost", Type: "llm"}, // LLM 指针缺位:手工构造防御面
			}},
			want: map[string][]string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeTaintSets(tc.wf)
			for id, want := range tc.want {
				if src := got.Sources(id); !reflect.DeepEqual(src, want) {
					t.Fatalf("%s: Sources(%q) = %v, want %v", tc.name, id, src, want)
				}
			}
			// 不在 want 的节点必须无污点(遍历全节点断言,防漏网)。
			for i := range tc.wf.Nodes {
				id := tc.wf.Nodes[i].ID
				if _, ok := tc.want[id]; ok {
					continue
				}
				if src := got.Sources(id); src != nil {
					t.Fatalf("%s: Sources(%q) = %v, want nil (clean node)", tc.name, id, src)
				}
			}
		})
	}
}
