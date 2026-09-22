package scheduler

import (
	"testing"

	"braid/internal/engine/dsl"
)

// parseOrFatal 以合法 DSL 直接构造(纯单测不经 DB;dsl 自身校验有独立测试)。
func parseOrFatal(t *testing.T, doc string) *dsl.Workflow {
	t.Helper()
	wf, err := dsl.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("dsl.Parse: %v", err)
	}
	return wf
}

func TestBuildDAGTopology(t *testing.T) {
	wf := parseOrFatal(t, `{
 "dsl":"1.0","name":"dag-t","inputs":[],
 "edges":[{"from":"a","to":"b"},{"from":"a","to":"c"},{"from":"b","to":"d"},{"from":"c","to":"d"},{"from":"e","to":"d"}],
 "nodes":[
  {"id":"d","type":"llm","prompt":{"template":"x"}},
  {"id":"a","type":"llm","prompt":{"template":"x"}},
  {"id":"e","type":"llm","prompt":{"template":"x"}},
  {"id":"c","type":"llm","prompt":{"template":"x"}},
  {"id":"b","type":"llm","prompt":{"template":"x"}}
 ]
}`)
	g, err := buildDAG(wf)
	if err != nil {
		t.Fatalf("buildDAG: %v", err)
	}
	// 多入口(入度 0:a 与 e),升序。
	assertEqList(t, "entry", g.entry, []string{"a", "e"})
	// 前驱/后继按 node_id 升序、去重。
	assertEqList(t, "preds[d]", g.preds["d"], []string{"b", "c", "e"})
	assertEqList(t, "succs[a]", g.succs["a"], []string{"b", "c"})
	if len(g.succs["d"]) != 0 {
		t.Fatalf("succs[d]=%v, want empty (sink)", g.succs["d"])
	}
	if g.nodes["a"] == nil || g.nodes["a"].Type != "llm" {
		t.Fatalf("nodes map 缺 a 或类型错: %+v", g.nodes["a"])
	}
}

func TestBuildDAGDefense(t *testing.T) {
	// 防御路径:dsl.Parse 正常会拒的形状,直接构造后喂 buildDAG 也必须响亮报错。
	wf := parseOrFatal(t, `{"dsl":"1.0","name":"e1","nodes":[{"id":"a","type":"llm","prompt":{"template":"x"}}]}`)
	wf.Edges = append(wf.Edges, dsl.Edge{From: "ghost", To: "a"})
	if _, err := buildDAG(wf); err == nil {
		t.Fatal("buildDAG 应拒未知端点(防御)")
	}

	// 重复节点 id:T1.2 语义层已拒,验证该层确实拦截(防御层的前提)。
	if _, err := dsl.Parse([]byte(`{"dsl":"1.0","name":"e2","nodes":[` +
		`{"id":"a","type":"llm","prompt":{"template":"x"}},{"id":"a","type":"llm","prompt":{"template":"x"}}]}`)); err == nil {
		t.Fatal("dsl.Parse 应拒重复节点 id")
	}
}

func assertEqList(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v (序或元素不符)", what, got, want)
		}
	}
}
