package scheduler

import (
	"encoding/json"
	"strings"
	"testing"

	"braid/internal/engine/dsl"
)

// 求值器纯单测:ref 解析(输入/节点输出/未命中)、四种快照形状、tool 值的三种
// 引用形态、大整数保真、outputs 物化(含剪枝臂 → null)。
//
// 变异自查:去掉 UseNumber → 大整数子测试红;纯引用串被字符串化 → 类型保持
// 子测试红;outputs 漏掉剪枝 null 键 → 键保留断言红。

func testEnv(t *testing.T, wfInput string, outputs map[string]string) *refEnv {
	t.Helper()
	outs := make(map[string]json.RawMessage, len(outputs))
	for id, out := range outputs {
		outs[id] = json.RawMessage(out)
	}
	env, err := newRefEnv(json.RawMessage(wfInput), outs)
	if err != nil {
		t.Fatalf("newRefEnv: %v", err)
	}
	return env
}

func TestRefEnvResolve(t *testing.T) {
	env := testEnv(t, `{"mode":"fast","n":3}`, map[string]string{
		"a": `{"flag":true,"text":"hi","nested":{"k":1}}`,
		"b": `not-json`, // 非对象 output:引用落空而非炸构造
		"c": `"scalar"`, // 标量 output:同上
	})
	tests := []struct {
		ref  string
		want any
		ok   bool
	}{
		{"{{inputs.mode}}", "fast", true},
		{"{{inputs.n}}", json.Number("3"), true},
		{"{{inputs.missing}}", nil, false},
		{"{{nodes.a.output.flag}}", true, true},
		{"{{nodes.a.output.text}}", "hi", true},
		{"{{nodes.a.output.missing}}", nil, false},
		{"{{nodes.b.output.any}}", nil, false},
		{"{{nodes.c.output.any}}", nil, false},
		{"{{nodes.ghost.output.x}}", nil, false},
		{"not-a-ref", nil, false},
	}
	for _, tt := range tests {
		got, ok := env.resolveText(tt.ref)
		if ok != tt.ok {
			t.Fatalf("%s: ok=%v, want %v", tt.ref, ok, tt.ok)
		}
		if !ok {
			continue
		}
		if wantNum, isNum := tt.want.(json.Number); isNum {
			gn, gok := got.(json.Number)
			if !gok || gn.String() != wantNum.String() {
				t.Fatalf("%s: got %#v, want json.Number(%s)", tt.ref, got, wantNum.String())
			}
			continue
		}
		if got != tt.want {
			t.Fatalf("%s: got %#v, want %#v", tt.ref, got, tt.want)
		}
	}
}

func TestNewRefEnvNonObjectInput(t *testing.T) {
	if _, err := newRefEnv(json.RawMessage(`[1,2]`), nil); err == nil {
		t.Fatal("非对象工作流输入应报错(inputs.* 解析面不存在)")
	}
}

func TestLLMSnapshot(t *testing.T) {
	env := testEnv(t, `{"q":"什么是 braid"}`, map[string]string{
		"a": `{"summary":"编排平台"}`,
	})
	wf := parseOrFatal(t, `{"dsl":"1.0","name":"snap-t","inputs":[{"name":"q","type":"string"}],
	 "edges":[{"from":"a","to":"n"}],
	 "nodes":[
	 {"id":"a","type":"llm","prompt":{"template":"gen"},"outputs":{"summary":"string"}},
	 {"id":"n","type":"llm","model":"gpt-test","temperature":0.2,"max_tokens":128,
	  "response_format":"json",
	  "prompt":{"system":"你是助手","template":"回答用户问题",
	            "user_data":[{"name":"question","ref":"{{inputs.q}}"},
	                         {"name":"ctx","ref":"{{nodes.a.output.summary}}"}]},
	  "outputs":{"answer":"string"}}]}`)
	snap, err := buildSnapshot(wf2node(t, wf, "n"), env)
	if err != nil {
		t.Fatalf("buildSnapshot: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(snap, &got); err != nil {
		t.Fatalf("snapshot 非法 JSON: %v", err)
	}
	ud, _ := got["user_data"].([]any)
	if len(ud) != 2 {
		t.Fatalf("user_data = %v, want 2 项(已解析值)", ud)
	}
	if ud[0].(map[string]any)["value"] != "什么是 braid" {
		t.Fatalf("user_data[0].value = %v", ud[0])
	}
	if ud[1].(map[string]any)["value"] != "编排平台" {
		t.Fatalf("user_data[1].value = %v(应取前驱 output)", ud[1])
	}
	if got["template"] != "回答用户问题" {
		t.Fatalf("template = %v(受信指令原文透传)", got["template"])
	}
	if got["model"] != "gpt-test" {
		t.Fatalf("model = %v", got["model"])
	}
}

func TestLLMSnapshotUnresolved(t *testing.T) {
	env := testEnv(t, `{}`, nil)
	wf := parseOrFatal(t, `{"dsl":"1.0","name":"snap-t","inputs":[{"name":"missing","type":"string"}],
	 "nodes":[
	 {"id":"n","type":"llm","prompt":{"template":"t","user_data":[{"name":"x","ref":"{{inputs.missing}}"}]}}]}`)
	if _, err := buildSnapshot(&wf.Nodes[0], env); err == nil {
		t.Fatal("未命中引用应报错(输入不完整,节点不可派发)")
	}
}

func TestToolSnapshot(t *testing.T) {
	env := testEnv(t, `{"region":"cn"}`, map[string]string{
		"a": `{"count":42,"name":"braid","tags":["x","y"]}`,
	})
	wf := parseOrFatal(t, `{"dsl":"1.0","name":"snap-t","inputs":[{"name":"region","type":"string"}],
	 "edges":[{"from":"a","to":"n"}],
	 "nodes":[
	 {"id":"a","type":"llm","prompt":{"template":"gen"},
	  "outputs":{"count":"number","name":"string","tags":"array"}},
	 {"id":"n","type":"tool","tool":"http",
	  "inputs":{
	    "path":"/reports/{{inputs.region}}/full",
	    "count":"{{nodes.a.output.count}}",
	    "plain":"literal",
	    "nested":{"who":"{{nodes.a.output.name}}","keep":7},
	    "list":["{{nodes.a.output.tags}}"]
	  }}]}`)
	snap, err := buildSnapshot(wf2node(t, wf, "n"), env)
	if err != nil {
		t.Fatalf("buildSnapshot: %v", err)
	}
	var got struct {
		Tool   string         `json:"tool"`
		Inputs map[string]any `json:"inputs"`
	}
	if err := unmarshalUseNumber(snap, &got); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got.Inputs["path"] != "/reports/cn/full" {
		t.Fatalf("内嵌替换: path = %v", got.Inputs["path"])
	}
	// 纯引用串 → 引用值保类型(number 不字符串化)。
	if n, ok := got.Inputs["count"].(json.Number); !ok || n.String() != "42" {
		t.Fatalf("纯引用保类型: count = %#v, want json.Number(42)", got.Inputs["count"])
	}
	if got.Inputs["plain"] != "literal" {
		t.Fatalf("字面量: plain = %v", got.Inputs["plain"])
	}
	nested := got.Inputs["nested"].(map[string]any)
	if nested["who"] != "braid" {
		t.Fatalf("嵌套对象内引用: who = %v", nested["who"])
	}
	if k, ok := nested["keep"].(json.Number); !ok || k.String() != "7" {
		t.Fatalf("嵌套字面量: keep = %#v", nested["keep"])
	}
	list := got.Inputs["list"].([]any)
	if tags, ok := list[0].([]any); !ok || len(tags) != 2 {
		t.Fatalf("数组内纯引用 → 数组值: list = %#v", list)
	}
}

func TestApprovalSnapshot(t *testing.T) {
	env := testEnv(t, `{}`, nil)
	wf := parseOrFatal(t, `{"dsl":"1.0","name":"snap-t","nodes":[
	 {"id":"n","type":"approval","approvers":["u1","u2"],"mode":"any","instructions":"请审"}]}`)
	snap, err := buildSnapshot(wf2node(t, wf, "n"), env)
	if err != nil {
		t.Fatalf("buildSnapshot: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(snap, &got); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got["mode"] != "any" || got["instructions"] != "请审" {
		t.Fatalf("approval 快照 = %v", got)
	}
	// schema default 生效值物化(T4.1:缺省 timeout_hours/on_timeout 不落零值,
	// 与 deadline/审计 payload 三处同源 effectiveApprovalSpec——对抗审计修)。
	if got["timeout_hours"] != float64(72) || got["on_timeout"] != "escalate" {
		t.Fatalf("approval 快照缺省字段 = %v, want 72/escalate(生效值)", got)
	}
}

func TestBranchOperands(t *testing.T) {
	env := testEnv(t, `{"flag":true}`, map[string]string{
		"a": `{"score":9}`,
	})
	b := &dsl.BranchNode{Default: "d"}
	b.Cases = append(b.Cases,
		dsl.BranchCase{When: dsl.Condition{Left: "{{inputs.flag}}", Op: "eq"}, Next: "x"},
		dsl.BranchCase{When: dsl.Condition{
			Left:  "{{nodes.a.output.score}}",
			Op:    "gt",
			Right: json.RawMessage(`"{{inputs.flag}}"`),
		}, Next: "y"},
	)
	snap := branchOperands(b, env)
	var got struct {
		Operands map[string]any `json:"operands"`
	}
	if err := unmarshalUseNumber(snap, &got); err != nil {
		t.Fatalf("operands: %v", err)
	}
	if len(got.Operands) != 2 {
		t.Fatalf("operands = %v, want left×2 + 右引用(布尔右值是字面量不入)", got.Operands)
	}
	if got.Operands["{{inputs.flag}}"] != true {
		t.Fatalf("left 布尔: %v", got.Operands)
	}
	if n, ok := got.Operands["{{nodes.a.output.score}}"].(json.Number); !ok || n.String() != "9" {
		t.Fatalf("left 数值: %#v", got.Operands["{{nodes.a.output.score}}"])
	}
}

func TestMaterializeOutputs(t *testing.T) {
	// b 被剪枝(无 output 行);c 的 output 缺字段 → null;a 正常。
	env := testEnv(t, `{"who":"river"}`, map[string]string{
		"a": `{"text":"done"}`,
		"c": `{"other":1}`,
	})
	out := materializeOutputs(map[string]string{
		"report": "{{nodes.a.output.text}}",
		"cut":    "{{nodes.b.output.text}}", // 剪枝臂 → null(键保留)
		"miss":   "{{nodes.c.output.text}}", // 字段缺失 → null
		"who":    "{{inputs.who}}",
	}, env)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("outputs: %v", err)
	}
	if got["report"] != "done" || got["who"] != "river" {
		t.Fatalf("outputs = %v", got)
	}
	for _, k := range []string{"cut", "miss"} {
		if v, ok := got[k]; !ok || v != nil {
			t.Fatalf("outputs[%s] = %#v (ok=%v), want 显式 null 键", k, v, ok)
		}
	}
}

func TestBigNumberFidelity(t *testing.T) {
	env := testEnv(t, `{"id":9007199254740993}`, nil) // 2^53+1,float64 必失真
	v, ok := env.resolveText("{{inputs.id}}")
	if !ok {
		t.Fatal("resolve 失败")
	}
	n, isNum := v.(json.Number)
	if !isNum || n.String() != "9007199254740993" {
		t.Fatalf("大整数保真: %#v(UseNumber 缺失则 float64 四舍五入)", v)
	}
	b, err := json.Marshal(map[string]any{"id": v})
	if err != nil || !strings.Contains(string(b), "9007199254740993") {
		t.Fatalf("重序列化保真: %s %v", b, err)
	}
}

// wf2node 从解析产物按 id 取节点(语义校验后的引用都指向真实节点)。
func wf2node(t *testing.T, wf *dsl.Workflow, id string) *dsl.Node {
	t.Helper()
	for i := range wf.Nodes {
		if wf.Nodes[i].ID == id {
			return &wf.Nodes[i]
		}
	}
	t.Fatalf("node %s not found", id)
	return nil
}
