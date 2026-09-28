package dsl

import (
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// T1.2 验收对照:
//   环检测 / 悬空引用 / 引用类型不匹配 / inputs·user_data·节点 id 重名 /
//   branch 出边与 cases[].next ∪ {default} 一致 / 发布时工具白名单。
// 负例断言“发现码集合精确相等”:既锁目标规则,又防级联误报;
// 正例锁“语义合法的写法不被过度拒绝”(均取自 T1.1 合法样例的真实形态)。

// issueCodesOf 提取错误中的全部发现码并排序,便于集合比较。
func issueCodesOf(t *testing.T, err error) []string {
	t.Helper()
	var codes []string
	for _, is := range IssuesFrom(err) {
		codes = append(codes, is.Code)
	}
	sort.Strings(codes)
	return codes
}

// branchTmpl:score:number → decide 分支到 hi/lo,路由结构齐全、合法。
// 插槽:op 与 right,用于谓词类型测试。
const branchTmpl = `{
  "dsl": "1.0",
  "name": "test-wf",
  "inputs": [{"name": "score", "type": "number", "required": true}],
  "nodes": [
    {"id": "decide", "type": "branch",
     "cases": [{"when": {"left": "{{inputs.score}}", "op": %q, "right": %s}, "next": "hi"}],
     "default": "lo"},
    {"id": "hi", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
    {"id": "lo", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}}
  ],
  "edges": [{"from": "decide", "to": "hi"}, {"from": "decide", "to": "lo"}]
}`

func TestParseSemanticIssues(t *testing.T) {
	tests := []struct {
		name string
		dsl  string
		want []string
	}{
		// —— 环检测 ——
		{"cycle_two_nodes", `{
		  "dsl": "1.0", "name": "test-wf",
		  "nodes": [
		    {"id": "a", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
		    {"id": "b", "type": "llm",
		     "prompt": {"template": "t", "user_data": [{"name": "prev", "ref": "{{nodes.a.output.text}}"}]},
		     "outputs": {"text": "string"}}
		  ],
		  "edges": [{"from": "a", "to": "b"}, {"from": "b", "to": "a"}]
		}`, []string{IssueCycle}},
		{"cycle_self_loop", `{
		  "dsl": "1.0", "name": "test-wf",
		  "nodes": [{"id": "a", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}}],
		  "edges": [{"from": "a", "to": "a"}]
		}`, []string{IssueCycle}},

		// —— 悬空引用 ——
		{"edge_to_unknown", `{
		  "dsl": "1.0", "name": "test-wf",
		  "nodes": [{"id": "a", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}}],
		  "edges": [{"from": "a", "to": "ghost"}]
		}`, []string{IssueEdgeEndpoint}},
		{"edge_from_unknown", `{
		  "dsl": "1.0", "name": "test-wf",
		  "nodes": [{"id": "a", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}}],
		  "edges": [{"from": "ghost", "to": "a"}]
		}`, []string{IssueEdgeEndpoint}},
		{"duplicate_edge", `{
		  "dsl": "1.0", "name": "test-wf",
		  "nodes": [
		    {"id": "a", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
		    {"id": "b", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}}
		  ],
		  "edges": [{"from": "a", "to": "b"}, {"from": "a", "to": "b"}]
		}`, []string{IssueDuplicateEdge}},
		{"dangling_input_ref", `{
		  "dsl": "1.0", "name": "test-wf",
		  "nodes": [{"id": "a", "type": "llm",
		     "prompt": {"template": "t", "user_data": [{"name": "x", "ref": "{{inputs.nope}}"}]},
		     "outputs": {"text": "string"}}]
		}`, []string{IssueDanglingRef}},
		{"dangling_node_ref", `{
		  "dsl": "1.0", "name": "test-wf",
		  "nodes": [{"id": "a", "type": "llm",
		     "prompt": {"template": "t", "user_data": [{"name": "x", "ref": "{{nodes.ghost.output.text}}"}]},
		     "outputs": {"text": "string"}}]
		}`, []string{IssueDanglingRef}},
		{"undeclared_output_field", `{
		  "dsl": "1.0", "name": "test-wf",
		  "nodes": [
		    {"id": "a", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
		    {"id": "b", "type": "llm",
		     "prompt": {"template": "t", "user_data": [{"name": "x", "ref": "{{nodes.a.output.wat}}"}]},
		     "outputs": {"text": "string"}}
		  ],
		  "edges": [{"from": "a", "to": "b"}]
		}`, []string{IssueDanglingRef}},
		{"node_without_outputs_referenced", `{
		  "dsl": "1.0", "name": "test-wf",
		  "nodes": [
		    {"id": "a", "type": "llm", "prompt": {"template": "t"}},
		    {"id": "b", "type": "llm",
		     "prompt": {"template": "t", "user_data": [{"name": "x", "ref": "{{nodes.a.output.text}}"}]},
		     "outputs": {"text": "string"}}
		  ],
		  "edges": [{"from": "a", "to": "b"}]
		}`, []string{IssueDanglingRef}},
		{"ref_to_branch_output", `{
		  "dsl": "1.0", "name": "test-wf",
		  "nodes": [
		    {"id": "decide", "type": "branch",
		     "cases": [{"when": {"left": "{{inputs.score}}", "op": "exists"}, "next": "b"}],
		     "default": "b"},
		    {"id": "b", "type": "llm",
		     "prompt": {"template": "t", "user_data": [{"name": "x", "ref": "{{nodes.decide.output.x}}"}]},
		     "outputs": {"text": "string"}}
		  ],
		  "inputs": [{"name": "score", "type": "number", "required": true}],
		  "edges": [{"from": "decide", "to": "b"}]
		}`, []string{IssueDanglingRef}},
		{"branch_next_unknown", `{
		  "dsl": "1.0", "name": "test-wf",
		  "inputs": [{"name": "score", "type": "number", "required": true}],
		  "nodes": [
		    {"id": "decide", "type": "branch",
		     "cases": [{"when": {"left": "{{inputs.score}}", "op": "gte", "right": 90}, "next": "ghost"}],
		     "default": "lo"},
		    {"id": "lo", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}}
		  ],
		  "edges": [{"from": "decide", "to": "lo"}]
		}`, []string{IssueDanglingRef}},

		// —— 祖先约束 ——
		{"ref_not_ancestor", `{
		  "dsl": "1.0", "name": "test-wf",
		  "nodes": [
		    {"id": "a", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
		    {"id": "b", "type": "llm",
		     "prompt": {"template": "t", "user_data": [{"name": "x", "ref": "{{nodes.a.output.text}}"}]},
		     "outputs": {"text": "string"}}
		  ]
		}`, []string{IssueNotAncestor}},
		{"ref_to_self", `{
		  "dsl": "1.0", "name": "test-wf",
		  "nodes": [{"id": "a", "type": "llm",
		     "prompt": {"template": "t", "user_data": [{"name": "x", "ref": "{{nodes.a.output.text}}"}]},
		     "outputs": {"text": "string"}}]
		}`, []string{IssueNotAncestor}},

		// —— 重名 ——
		{"dup_node_id", `{
		  "dsl": "1.0", "name": "test-wf",
		  "nodes": [
		    {"id": "a", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
		    {"id": "a", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}}
		  ]
		}`, []string{IssueDupNodeID}},
		{"dup_input", `{
		  "dsl": "1.0", "name": "test-wf",
		  "inputs": [{"name": "score", "type": "number"}, {"name": "score", "type": "string"}],
		  "nodes": [{"id": "a", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}}]
		}`, []string{IssueDupInput}},
		{"dup_user_data", `{
		  "dsl": "1.0", "name": "test-wf",
		  "inputs": [{"name": "q", "type": "string"}],
		  "nodes": [{"id": "a", "type": "llm",
		     "prompt": {"template": "t", "user_data": [
		       {"name": "x", "ref": "{{inputs.q}}"}, {"name": "x", "ref": "{{inputs.q}}"}
		     ]},
		     "outputs": {"text": "string"}}]
		}`, []string{IssueDupUserData}},
		{"input_name_collides_node_id", `{
		  "dsl": "1.0", "name": "test-wf",
		  "inputs": [{"name": "a", "type": "string"}],
		  "nodes": [{"id": "a", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}}]
		}`, []string{IssueNameCollision}},

		// —— branch 出边一致性 ——
		{"branch_edge_not_routable", `{
		  "dsl": "1.0", "name": "test-wf",
		  "inputs": [{"name": "score", "type": "number", "required": true}],
		  "nodes": [
		    {"id": "decide", "type": "branch",
		     "cases": [{"when": {"left": "{{inputs.score}}", "op": "gte", "right": 90}, "next": "hi"}],
		     "default": "lo"},
		    {"id": "hi", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
		    {"id": "lo", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
		    {"id": "extra", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}}
		  ],
		  "edges": [{"from": "decide", "to": "hi"}, {"from": "decide", "to": "lo"}, {"from": "decide", "to": "extra"}]
		}`, []string{IssueBranchEdges}},
		{"branch_default_without_edge", `{
		  "dsl": "1.0", "name": "test-wf",
		  "inputs": [{"name": "score", "type": "number", "required": true}],
		  "nodes": [
		    {"id": "decide", "type": "branch",
		     "cases": [{"when": {"left": "{{inputs.score}}", "op": "gte", "right": 90}, "next": "hi"}],
		     "default": "lo"},
		    {"id": "hi", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
		    {"id": "lo", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}}
		  ],
		  "edges": [{"from": "decide", "to": "hi"}]
		}`, []string{IssueBranchEdges}},

		// —— 引用类型不匹配(branch.when)——
		{"type_ordering_number_vs_string", fmt.Sprintf(branchTmpl, "gte", `"90"`),
			[]string{IssueTypeMismatch}},
		{"type_eq_number_vs_null", fmt.Sprintf(branchTmpl, "eq", `null`),
			[]string{IssueTypeMismatch}},
		{"type_contains_left_number", fmt.Sprintf(branchTmpl, "contains", `"x"`),
			[]string{IssueTypeMismatch}},
		{"type_eq_object_vs_string", `{
		  "dsl": "1.0", "name": "test-wf",
		  "inputs": [{"name": "doc", "type": "object", "required": true}],
		  "nodes": [
		    {"id": "decide", "type": "branch",
		     "cases": [{"when": {"left": "{{inputs.doc}}", "op": "eq", "right": "abc"}, "next": "hi"}],
		     "default": "lo"},
		    {"id": "hi", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
		    {"id": "lo", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}}
		  ],
		  "edges": [{"from": "decide", "to": "hi"}, {"from": "decide", "to": "lo"}]
		}`, []string{IssueTypeMismatch}},
		{"type_ref_vs_ref_mismatch", `{
		  "dsl": "1.0", "name": "test-wf",
		  "inputs": [
		    {"name": "score", "type": "number", "required": true},
		    {"name": "name", "type": "string", "required": true}
		  ],
		  "nodes": [
		    {"id": "decide", "type": "branch",
		     "cases": [{"when": {"left": "{{inputs.score}}", "op": "gt", "right": "{{inputs.name}}"}, "next": "hi"}],
		     "default": "lo"},
		    {"id": "hi", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
		    {"id": "lo", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}}
		  ],
		  "edges": [{"from": "decide", "to": "hi"}, {"from": "decide", "to": "lo"}]
		}`, []string{IssueTypeMismatch}},
		{"type_eq_right_absent", `{
		  "dsl": "1.0", "name": "test-wf",
		  "inputs": [{"name": "score", "type": "number", "required": true}],
		  "nodes": [
		    {"id": "decide", "type": "branch",
		     "cases": [{"when": {"left": "{{inputs.score}}", "op": "eq"}, "next": "hi"}],
		     "default": "lo"},
		    {"id": "hi", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
		    {"id": "lo", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}}
		  ],
		  "edges": [{"from": "decide", "to": "hi"}, {"from": "decide", "to": "lo"}]
		}`, []string{IssueTypeMismatch}},

		// —— tool.inputs 内引用形态钉死 ——
		{"bad_ref_unknown_namespace", `{
		  "dsl": "1.0", "name": "test-wf",
		  "inputs": [{"name": "doc_id", "type": "string", "required": true}],
		  "nodes": [{"id": "fetch", "type": "tool", "tool": "http",
		     "inputs": {"path": "/docs/{{file_id}}"},
		     "outputs": {"body": "string"}}]
		}`, []string{IssueBadRef}},
		{"bad_ref_unclosed", `{
		  "dsl": "1.0", "name": "test-wf",
		  "inputs": [{"name": "doc_id", "type": "string", "required": true}],
		  "nodes": [{"id": "fetch", "type": "tool", "tool": "http",
		     "inputs": {"path": "see {{inputs.doc"},
		     "outputs": {"body": "string"}}]
		}`, []string{IssueBadRef}},
		{"embedded_ref_dangling_in_tool_inputs", `{
		  "dsl": "1.0", "name": "test-wf",
		  "nodes": [{"id": "fetch", "type": "tool", "tool": "http",
		     "inputs": {"path": "/docs/{{inputs.nope}}/x"},
		     "outputs": {"body": "string"}}]
		}`, []string{IssueDanglingRef}},

		// —— 聚合:一次报告多条独立发现 ——
		{"multiple_issues_aggregated", `{
		  "dsl": "1.0", "name": "test-wf",
		  "inputs": [{"name": "q", "type": "string"}, {"name": "q", "type": "string"}],
		  "nodes": [{"id": "a", "type": "llm",
		     "prompt": {"template": "t", "user_data": [{"name": "x", "ref": "{{inputs.nope}}"}]},
		     "outputs": {"text": "string"}}]
		}`, []string{IssueDanglingRef, IssueDupInput}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.dsl))
			if err == nil {
				t.Fatalf("expected issues %v, got nil", tt.want)
			}
			got := issueCodesOf(t, err)
			want := append([]string{}, tt.want...)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("issue codes mismatch:\n want %v\n got  %v\nfull error:\n%s", want, got, err)
			}
		})
	}
}

// TestParseSemanticallyValid 锁定合法形态不被过度拒绝(均来自 T1.1 合法样例的真实用法)。
func TestParseSemanticallyValid(t *testing.T) {
	tests := []struct {
		name string
		dsl  string
	}{
		{"workflow_output_exempts_ancestor", `{
		  "dsl": "1.0", "name": "test-wf",
		  "nodes": [
		    {"id": "a", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
		    {"id": "b", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}}
		  ],
		  "outputs": {"x": "{{nodes.a.output.text}}"}
		}`},
		{"user_data_name_may_equal_input_name", `{
		  "dsl": "1.0", "name": "test-wf",
		  "inputs": [{"name": "order", "type": "object", "required": true}],
		  "nodes": [{"id": "extract", "type": "llm", "response_format": "json",
		     "prompt": {"template": "提取订单金额与币种。",
		       "user_data": [{"name": "order", "ref": "{{inputs.order}}"}]},
		     "outputs": {"amount": "number", "currency": "string"}}],
		  "outputs": {"amount": "{{nodes.extract.output.amount}}"}
		}`},
		{"user_data_name_reusable_across_nodes", `{
		  "dsl": "1.0", "name": "test-wf",
		  "nodes": [
		    {"id": "a", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
		    {"id": "b", "type": "llm",
		     "prompt": {"template": "t", "user_data": [{"name": "s", "ref": "{{nodes.a.output.text}}"}]},
		     "outputs": {"text": "string"}},
		    {"id": "c", "type": "llm",
		     "prompt": {"template": "t", "user_data": [{"name": "s", "ref": "{{nodes.a.output.text}}"}]},
		     "outputs": {"text": "string"}}
		  ],
		  "edges": [{"from": "a", "to": "b"}, {"from": "a", "to": "c"}]
		}`},
		{"tool_inputs_pure_and_embedded_refs", `{
		  "dsl": "1.0", "name": "test-wf",
		  "inputs": [{"name": "tag", "type": "string", "required": true}],
		  "nodes": [
		    {"id": "a", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
		    {"id": "t", "type": "tool", "tool": "http",
		     "inputs": {"q": "{{nodes.a.output.text}}", "note": "pre-{{inputs.tag}}-post"},
		     "outputs": {"r": "string"}}
		  ],
		  "edges": [{"from": "a", "to": "t"}],
		  "outputs": {"r": "{{nodes.t.output.r}}"}
		}`},
		{"branch_string_ordering_ok", fmt.Sprintf(branchTmpl2, "lt", `"b"`)},
		{"branch_exists_without_right_ok", `{
		  "dsl": "1.0", "name": "test-wf",
		  "inputs": [{"name": "tag", "type": "string", "required": true}],
		  "nodes": [
		    {"id": "decide", "type": "branch",
		     "cases": [{"when": {"left": "{{inputs.tag}}", "op": "exists"}, "next": "hi"}],
		     "default": "lo"},
		    {"id": "hi", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
		    {"id": "lo", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}}
		  ],
		  "edges": [{"from": "decide", "to": "hi"}, {"from": "decide", "to": "lo"}]
		}`},
		{"branch_array_contains_ok", `{
		  "dsl": "1.0", "name": "test-wf",
		  "inputs": [{"name": "tags", "type": "array", "required": true}],
		  "nodes": [
		    {"id": "decide", "type": "branch",
		     "cases": [{"when": {"left": "{{inputs.tags}}", "op": "contains", "right": 3}, "next": "hi"}],
		     "default": "lo"},
		    {"id": "hi", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
		    {"id": "lo", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}}
		  ],
		  "edges": [{"from": "decide", "to": "hi"}, {"from": "decide", "to": "lo"}]
		}`},
		{"branch_object_eq_ok", `{
		  "dsl": "1.0", "name": "test-wf",
		  "inputs": [{"name": "doc", "type": "object", "required": true}],
		  "nodes": [
		    {"id": "decide", "type": "branch",
		     "cases": [{"when": {"left": "{{inputs.doc}}", "op": "eq", "right": {}}, "next": "hi"}],
		     "default": "lo"},
		    {"id": "hi", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
		    {"id": "lo", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}}
		  ],
		  "edges": [{"from": "decide", "to": "hi"}, {"from": "decide", "to": "lo"}]
		}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wf, err := Parse([]byte(tt.dsl))
			if err != nil {
				t.Fatalf("expected semantically valid, got:\n%s", err)
			}
			if wf == nil || len(wf.Nodes) == 0 {
				t.Fatal("expected decoded workflow")
			}
		})
	}
}

// branchTmpl2:左值为字符串输入 tag 的谓词模板(正例用)。
const branchTmpl2 = `{
  "dsl": "1.0",
  "name": "test-wf",
  "inputs": [{"name": "tag", "type": "string", "required": true}],
  "nodes": [
    {"id": "decide", "type": "branch",
     "cases": [{"when": {"left": "{{inputs.tag}}", "op": %q, "right": %s}, "next": "hi"}],
     "default": "lo"},
    {"id": "hi", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
    {"id": "lo", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}}
  ],
  "edges": [{"from": "decide", "to": "hi"}, {"from": "decide", "to": "lo"}]
}`

// TestToolAllowlist 发布级工具白名单(T5.4 发布路径将接入此钩子)。
func TestToolAllowlist(t *testing.T) {
	dslDoc := `{
	  "dsl": "1.0", "name": "test-wf",
	  "inputs": [{"name": "doc_path", "type": "string", "required": true}],
	  "nodes": [{"id": "read_doc", "type": "tool", "tool": "file_read",
	     "inputs": {"path": "{{inputs.doc_path}}"},
	     "outputs": {"content": "string"}}]
	}`
	tests := []struct {
		name    string
		opts    []Option
		wantErr bool
		want    string
	}{
		{"no_allowlist_enforced", nil, false, ""},
		{"tool_allowed", []Option{WithToolAllowlist("file_read")}, false, ""},
		{"tool_not_in_allowlist", []Option{WithToolAllowlist("http")}, true, IssueToolNotAllowed},
		{"empty_allowlist_denies_all", []Option{WithToolAllowlist()}, true, IssueToolNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(dslDoc), tt.opts...)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("expected no error, got:\n%s", err)
				}
				return
			}
			if !HasIssue(err, tt.want) {
				t.Fatalf("missing issue %s; got:\n%s", tt.want, err)
			}
		})
	}
}

// TestValidCorpusParsesClean T1.1 合法样例全量过 schema + 语义双层校验,
// 防两层校验的合法集漂移。
func TestValidCorpusParsesClean(t *testing.T) {
	for _, name := range sampleFiles(t, filepath.Join("testdata", "valid")) {
		t.Run(name, func(t *testing.T) {
			wf, err := Parse(readSample(t, filepath.Join("testdata", "valid", name)))
			if err != nil {
				t.Fatalf("expected clean parse, got:\n%s", err)
			}
			if wf.DSL != "1.0" || wf.Name == "" {
				t.Fatalf("unexpected decode: dsl=%q name=%q", wf.DSL, wf.Name)
			}
			for i := range wf.Nodes {
				n := &wf.Nodes[i]
				var typed bool
				switch n.Type {
				case "llm":
					typed = n.LLM != nil
				case "tool":
					typed = n.Tool != nil
				case "branch":
					typed = n.Branch != nil
				case "approval":
					typed = n.Approval != nil
				case "parallel":
					typed = n.Parallel != nil
				}
				if !typed {
					t.Fatalf("node %q type %q has no decoded concrete struct", n.ID, n.Type)
				}
			}
		})
	}
}
