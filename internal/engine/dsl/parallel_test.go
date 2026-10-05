package dsl

import (
	"testing"
)

// T9.1(D-2026-09-28-t91-parallel-scatter-gather)验收对照:
//   parallel 节点 = foreach(纯引用,指向声明 type=array 的输入)+
//   body(llm|tool 单节点模板,无 id)+ on_failure(fail_fast|wait_all,缺省 fail_fast);
//   item 引用形态 {{item}} / {{item.<field>}} 仅在 body 内合法(外部出现 = bad_ref),
//   绑定关键字钉死 "item"(自定义名与引用形态钉死冲突,不设旋钮);
//   节点输出契约固定 results:array(gather 输入序,下游 {{nodes.<id>.output.results}})。
//   结构层负例另见 testdata/invalid(wantCause 钉拒绝原因),与 T1.1 同款分工。

// parallelTmpl:pre → fan(parallel,body llm,{{item}} 纯引用 + 祖先 output 引用)→
// post(tool,内嵌引用 fan.output.results);工作流 outputs 同时引用 fan 与 post。
// 全链语义合法,覆盖 D-2026-09-28-t91 的引用面全集。
const parallelTmpl = `{
  "dsl": "1.0",
  "name": "parallel-wf",
  "inputs": [{"name": "reviews", "type": "array", "required": true}],
  "nodes": [
    {"id": "pre", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
    {"id": "fan", "type": "parallel", "title": "并行评审", "parallel": {
      "foreach": "{{inputs.reviews}}",
      "on_failure": "wait_all",
      "body": {"type": "llm", "retry": {"max_attempts": 3},
        "prompt": {"template": "评审以下内容",
          "user_data": [{"name": "review", "ref": "{{item}}"},
                        {"name": "ctx", "ref": "{{nodes.pre.output.text}}"}]},
        "outputs": {"verdict": "string"}}
    }},
    {"id": "post", "type": "tool", "tool": "http",
     "inputs": {"payload": "{{nodes.fan.output.results}}"}, "outputs": {"done": "boolean"}}
  ],
  "edges": [{"from": "pre", "to": "fan"}, {"from": "fan", "to": "post"}],
  "outputs": {"all": "{{nodes.fan.output.results}}", "done": "{{nodes.post.output.done}}"}
}`

func TestParallelValid(t *testing.T) {
	wf, err := Parse([]byte(parallelTmpl))
	if err != nil {
		t.Fatalf("expected valid, got error: %v", err)
	}
	var fan *Node
	for i := range wf.Nodes {
		if wf.Nodes[i].ID == "fan" {
			fan = &wf.Nodes[i]
		}
	}
	if fan == nil || fan.Parallel == nil {
		t.Fatalf("node fan not decoded as parallel: %+v", fan)
	}
	p := fan.Parallel
	if p.Foreach != "{{inputs.reviews}}" {
		t.Errorf("foreach = %q, want {{inputs.reviews}}", p.Foreach)
	}
	if p.OnFailure != "wait_all" {
		t.Errorf("on_failure = %q, want wait_all", p.OnFailure)
	}
	if p.Body.ID != "" || p.Body.LLM == nil {
		t.Errorf("body = id %q llm %v, want empty id with llm body", p.Body.ID, p.Body.LLM)
	}
	if p.Body.Retry == nil || p.Body.Retry.MaxAttempts != 3 {
		t.Errorf("body retry = %+v, want max_attempts 3", p.Body.Retry)
	}
}

// parallelDefaults:item/on_failure 缺省可省(生效值由 scheduler 物化点补齐,
// approval mode/timeout 同款约定);两者为空是合法形态。
func TestParallelDefaultsAbsent(t *testing.T) {
	doc := `{
	  "dsl": "1.0", "name": "parallel-wf",
	  "inputs": [{"name": "reviews", "type": "array", "required": true}],
	  "nodes": [
	    {"id": "fan", "type": "parallel", "parallel": {
	      "foreach": "{{inputs.reviews}}",
	      "body": {"type": "tool", "tool": "http", "inputs": {"q": "{{item}}"}}
	    }}
	  ],
	  "edges": []
	}`
	wf, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("expected valid, got error: %v", err)
	}
	p := wf.Nodes[0].Parallel
	if p.OnFailure != "" {
		t.Errorf("default should stay empty (scheduler materializes), got on_failure=%q", p.OnFailure)
	}
	if p.MinSuccess != 0 {
		t.Errorf("min_success absent should decode 0 (= 未启用, D-t191), got %d", p.MinSuccess)
	}
	if p.Body.Tool == nil || p.Body.LLM != nil {
		t.Errorf("tool body not decoded: %+v", p.Body)
	}
}

// TestParallelMinSuccess(T19.1,D-2026-10-05-t191):min_success 独立整数字段与
// on_failure 正交;failed_count 输出契约仅随 min_success 启用登记(契约与运行时
// 形状同构:启用 ⇒ gather 物化恒含 failed_count,未启用 ⇒ 恒无)。
func TestParallelMinSuccess(t *testing.T) {
	t.Run("valid_min_success_enables_failed_count_ref", func(t *testing.T) {
		doc := `{
		  "dsl": "1.0", "name": "parallel-wf",
		  "inputs": [{"name": "reviews", "type": "array", "required": true}],
		  "nodes": [
		    {"id": "fan", "type": "parallel", "parallel": {
		      "foreach": "{{inputs.reviews}}",
		      "on_failure": "wait_all",
		      "min_success": 2,
		      "body": {"type": "llm", "prompt": {"template": "t"}}
		    }}
		  ],
		  "edges": [],
		  "outputs": {
		    "all": "{{nodes.fan.output.results}}",
		    "partial": "{{nodes.fan.output.failed_count}}"
		  }
		}`
		wf, err := Parse([]byte(doc))
		if err != nil {
			t.Fatalf("expected valid, got error: %v", err)
		}
		p := wf.Nodes[0].Parallel
		if p.MinSuccess != 2 {
			t.Errorf("min_success = %d, want 2", p.MinSuccess)
		}
		if p.OnFailure != "wait_all" {
			t.Errorf("on_failure = %q, want wait_all (正交共存)", p.OnFailure)
		}
	})
	t.Run("failed_count_ref_requires_min_success", func(t *testing.T) {
		doc := `{
		  "dsl": "1.0", "name": "parallel-wf",
		  "inputs": [{"name": "reviews", "type": "array", "required": true}],
		  "nodes": [
		    {"id": "fan", "type": "parallel", "parallel": {
		      "foreach": "{{inputs.reviews}}",
		      "body": {"type": "llm", "prompt": {"template": "t"}}
		    }}
		  ],
		  "edges": [],
		  "outputs": {"partial": "{{nodes.fan.output.failed_count}}"}
		}`
		_, err := Parse([]byte(doc))
		if err == nil {
			t.Fatalf("expected dangling-ref issue (failed_count 未随 min_success 登记), got nil")
		}
		got := issueCodesOf(t, err)
		if !equalStrings(got, []string{IssueDanglingRef}) {
			t.Fatalf("issue codes = %v, want [%s]\nfull error:\n%s", got, IssueDanglingRef, err)
		}
	})
}

// TestParallelDynamicForeach(T19.2,D-2026-10-05-t192):foreach 来源放开到
// 祖先节点的 array 输出字段({{nodes.<id>.output.<field>}})——实例数在
// Instantiate 不可知,展开移到派发位(scheduler 面)。语义约束全复用 checkRef
// 既有面:目标存在、非 branch、字段声明 type=array、祖先约束;input 引用
// 路径逐字节不变(极限退化)。嵌套合法:foreach 可引另一 parallel 的 results。
func TestParallelDynamicForeach(t *testing.T) {
	t.Run("valid_node_ref", func(t *testing.T) {
		doc := `{
		  "dsl": "1.0", "name": "parallel-dynamic",
		  "inputs": [{"name": "topic", "type": "string", "required": true}],
		  "nodes": [
		    {"id": "gen", "type": "llm", "prompt": {"template": "t"}, "outputs": {"subs": "array"}},
		    {"id": "fan", "type": "parallel", "parallel": {
		      "foreach": "{{nodes.gen.output.subs}}",
		      "body": {"type": "llm", "prompt": {"template": "t",
		        "user_data": [{"name": "sub", "ref": "{{item}}"}]}}
		    }}
		  ],
		  "edges": [{"from": "gen", "to": "fan"}]
		}`
		wf, err := Parse([]byte(doc))
		if err != nil {
			t.Fatalf("expected valid, got error: %v", err)
		}
		if p := wf.Nodes[1].Parallel; p.Foreach != "{{nodes.gen.output.subs}}" {
			t.Errorf("foreach = %q, want node ref preserved verbatim", p.Foreach)
		}
	})
	t.Run("valid_nested_parallel_results", func(t *testing.T) {
		doc := `{
		  "dsl": "1.0", "name": "parallel-nested",
		  "inputs": [{"name": "rows", "type": "array", "required": true}],
		  "nodes": [
		    {"id": "fan1", "type": "parallel", "parallel": {
		      "foreach": "{{inputs.rows}}",
		      "body": {"type": "llm", "prompt": {"template": "t"}}
		    }},
		    {"id": "fan2", "type": "parallel", "parallel": {
		      "foreach": "{{nodes.fan1.output.results}}",
		      "body": {"type": "llm", "prompt": {"template": "t"}}
		    }}
		  ],
		  "edges": [{"from": "fan1", "to": "fan2"}]
		}`
		if _, err := Parse([]byte(doc)); err != nil {
			t.Fatalf("expected valid (results 声明 array), got error: %v", err)
		}
	})
	// 非法族:非 array 字段 / 未知节点 / 非祖先 / branch 输出。
	tests := []struct {
		name string
		dsl  string
		want []string
	}{
		{"node_ref_non_array_field", `{
		  "dsl": "1.0", "name": "parallel-dynamic",
		  "nodes": [
		    {"id": "gen", "type": "llm", "prompt": {"template": "t"}, "outputs": {"subs": "string"}},
		    {"id": "fan", "type": "parallel", "parallel": {
		      "foreach": "{{nodes.gen.output.subs}}",
		      "body": {"type": "llm", "prompt": {"template": "t"}}
		    }}
		  ],
		  "edges": [{"from": "gen", "to": "fan"}]
		}`, []string{IssueBadForeach}},
		{"node_ref_unknown_node", `{
		  "dsl": "1.0", "name": "parallel-dynamic",
		  "nodes": [
		    {"id": "fan", "type": "parallel", "parallel": {
		      "foreach": "{{nodes.ghost.output.subs}}",
		      "body": {"type": "llm", "prompt": {"template": "t"}}
		    }}
		  ],
		  "edges": []
		}`, []string{IssueDanglingRef}},
		{"node_ref_not_ancestor", `{
		  "dsl": "1.0", "name": "parallel-dynamic",
		  "nodes": [
		    {"id": "fan", "type": "parallel", "parallel": {
		      "foreach": "{{nodes.gen.output.subs}}",
		      "body": {"type": "llm", "prompt": {"template": "t"}}
		    }},
		    {"id": "gen", "type": "llm", "prompt": {"template": "t"}, "outputs": {"subs": "array"}}
		  ],
		  "edges": [{"from": "fan", "to": "gen"}]
		}`, []string{IssueNotAncestor}},
		{"node_ref_branch_output", `{
		  "dsl": "1.0", "name": "parallel-dynamic",
		  "inputs": [{"name": "rows", "type": "array", "required": true}],
		  "nodes": [
		    {"id": "gen", "type": "llm", "prompt": {"template": "t"}, "outputs": {"subs": "array"}},
		    {"id": "gate", "type": "branch",
		     "cases": [{"when": {"left": "{{inputs.rows}}", "op": "exists"}, "next": "fan"}],
		     "default": "fan"},
		    {"id": "fan", "type": "parallel", "parallel": {
		      "foreach": "{{nodes.gate.output.subs}}",
		      "body": {"type": "llm", "prompt": {"template": "t"}}
		    }}
		  ],
		  "edges": [{"from": "gen", "to": "gate"}, {"from": "gate", "to": "fan"}]
		}`, []string{IssueDanglingRef}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.dsl))
			if err == nil {
				t.Fatalf("expected issues %v, got nil", tt.want)
			}
			got := issueCodesOf(t, err)
			if !equalStrings(got, tt.want) {
				t.Fatalf("issue codes = %v, want %v\nfull error:\n%s", got, tt.want, err)
			}
		})
	}
}

func TestParallelSemanticIssues(t *testing.T) {
	tests := []struct {
		name string
		dsl  string
		want []string
	}{
		// —— foreach 语义 ——
		{"foreach_not_array_input", `{
		  "dsl": "1.0", "name": "parallel-wf",
		  "inputs": [{"name": "reviews", "type": "string", "required": true}],
		  "nodes": [
		    {"id": "fan", "type": "parallel", "parallel": {
		      "foreach": "{{inputs.reviews}}",
		      "body": {"type": "llm", "prompt": {"template": "t"}}
		    }}
		  ],
		  "edges": []
		}`, []string{IssueBadForeach}},
		{"foreach_undeclared_input", `{
		  "dsl": "1.0", "name": "parallel-wf",
		  "nodes": [
		    {"id": "fan", "type": "parallel", "parallel": {
		      "foreach": "{{inputs.reviews}}",
		      "body": {"type": "llm", "prompt": {"template": "t"}}
		    }}
		  ],
		  "edges": []
		}`, []string{IssueDanglingRef}},

		// —— item 引用语境 ——
		{"item_ref_outside_body", `{
		  "dsl": "1.0", "name": "parallel-wf",
		  "inputs": [{"name": "reviews", "type": "array", "required": true}],
		  "nodes": [
		    {"id": "solo", "type": "llm",
		     "prompt": {"template": "t", "user_data": [{"name": "one", "ref": "{{item}}"}]},
		     "outputs": {"text": "string"}},
		    {"id": "fan", "type": "parallel", "parallel": {
		      "foreach": "{{inputs.reviews}}",
		      "body": {"type": "llm", "prompt": {"template": "t"}}
		    }}
		  ],
		  "edges": []
		}`, []string{IssueBadRef}},

		// —— body 内引用约束 ——
		{"body_ref_not_ancestor", `{
		  "dsl": "1.0", "name": "parallel-wf",
		  "inputs": [{"name": "reviews", "type": "array", "required": true}],
		  "nodes": [
		    {"id": "fan", "type": "parallel", "parallel": {
		      "foreach": "{{inputs.reviews}}",
		      "body": {"type": "llm",
		        "prompt": {"template": "t",
		          "user_data": [{"name": "late", "ref": "{{nodes.post.output.done}}"}]}}
		    }},
		    {"id": "post", "type": "tool", "tool": "http", "inputs": {"q": "x"},
		     "outputs": {"done": "boolean"}}
		  ],
		  "edges": [{"from": "fan", "to": "post"}]
		}`, []string{IssueNotAncestor}},

		// —— 输出契约:parallel 节点仅声明 results ——
		{"downstream_unknown_output_field", `{
		  "dsl": "1.0", "name": "parallel-wf",
		  "inputs": [{"name": "reviews", "type": "array", "required": true}],
		  "nodes": [
		    {"id": "fan", "type": "parallel", "parallel": {
		      "foreach": "{{inputs.reviews}}",
		      "body": {"type": "llm", "prompt": {"template": "t"}}
		    }},
		    {"id": "post", "type": "tool", "tool": "http",
		     "inputs": {"q": "{{nodes.fan.output.summary}}"}, "outputs": {"done": "boolean"}}
		  ],
		  "edges": [{"from": "fan", "to": "post"}]
		}`, []string{IssueDanglingRef}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.dsl))
			if err == nil {
				t.Fatalf("expected issues %v, got nil", tt.want)
			}
			got := issueCodesOf(t, err)
			if !equalStrings(got, tt.want) {
				t.Fatalf("issue codes = %v, want %v\nfull error:\n%s", got, tt.want, err)
			}
		})
	}
}

// TestParallelDefensive:绕过 schema 直调语义校验(hand-built Workflow),
// 钉防御分支——body 非 llm|tool、foreach 引非 array 输出字段(T19.2 起 node
// 引用合法,防御面收窄为类型裁决)。Parse 路径上 schema 先拦,此处锁「跳过
// schema 的调用方」不落静默。
func TestParallelDefensive(t *testing.T) {
	rev := []Input{{Name: "reviews", Type: "array"}}
	fan := func(p *ParallelNode) Node {
		n := Node{ID: "fan", Type: "parallel", Parallel: p}
		return n
	}
	tests := []struct {
		name string
		wf   *Workflow
		want []string
	}{
		{"body_branch", &Workflow{
			Inputs: rev,
			Nodes: []Node{fan(&ParallelNode{
				Foreach: "{{inputs.reviews}}",
				Body:    Node{Type: "branch", Branch: &BranchNode{Cases: []BranchCase{{When: Condition{Left: "{{inputs.reviews}}", Op: "exists"}}}, Default: "x"}},
			})},
		}, []string{IssueBadForeach}},
		{"foreach_node_ref", &Workflow{
			Inputs: rev,
			Nodes: []Node{
				{ID: "pre", Type: "llm", LLM: &LLMNode{Prompt: Prompt{Template: "t"}, Outputs: map[string]string{"text": "string"}}},
				fan(&ParallelNode{Foreach: "{{nodes.pre.output.text}}", Body: Node{Type: "llm", LLM: &LLMNode{Prompt: Prompt{Template: "t"}}}}),
			},
			// pre 是 fan 的祖先(否则 not_ancestor 混入):本例钉「node 引用
			// 合法化后防御面收窄」的剩余裁决——非 array 输出字段。
			Edges: []Edge{{From: "pre", To: "fan"}},
		}, []string{IssueBadForeach}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issues := tt.wf.validate(parseConfig{})
			var got []string
			for _, is := range issues {
				got = append(got, is.Code)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("issue codes = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("issue codes = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// TestParallelItemRefForms:refs.go 的 item 形态钉死(纯引用 + 内嵌扫描 + 展开)。
func TestParallelItemRefForms(t *testing.T) {
	t.Run("pure", func(t *testing.T) {
		r, ok := parsePureRef("{{item}}")
		if !ok || r.Kind != "item" || r.Field != "" {
			t.Errorf("{{item}} = %+v ok=%v, want item/empty field", r, ok)
		}
		r, ok = parsePureRef("{{item.score}}")
		if !ok || r.Kind != "item" || r.Field != "score" {
			t.Errorf("{{item.score}} = %+v ok=%v, want item/score", r, ok)
		}
		for _, bad := range []string{"{{itemx}}", "{{item.A}}", "{{item.a.b}}", "{{ item }}", "{{item.}}"} {
			if _, ok := parsePureRef(bad); ok {
				t.Errorf("%q should not parse as item reference", bad)
			}
		}
	})
	t.Run("scan_embedded", func(t *testing.T) {
		refs, bad := scanStringRefs("a {{item.x}} b {{inputs.y}} c {{nodes.n.output.f}} d")
		if len(bad) != 0 {
			t.Errorf("bad = %v, want none", bad)
		}
		if len(refs) != 3 || refs[0].Kind != "item" || refs[1].Kind != "input" || refs[2].Kind != "node" {
			t.Errorf("refs = %+v, want item,input,node", refs)
		}
		if refs[0].Field != "x" {
			t.Errorf("item field = %q, want x", refs[0].Field)
		}
	})
	t.Run("expand", func(t *testing.T) {
		out, miss := ExpandStringRefs("x {{item}} y {{item.k}} z", func(r Ref) (string, bool) {
			if r.Kind != "item" {
				return "", false
			}
			if r.Field == "" {
				return "ELEM", true
			}
			return "F:" + r.Field, true
		})
		if miss || out != "x ELEM y F:k z" {
			t.Errorf("expand = %q miss=%v", out, miss)
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
