package dsl

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// T16.4 示例资产守护:docs/examples/agent-council.json 是用户可见示例资产
// (README §2.3 走链引用),此前自认无守护——DSL schema/语义校验变更导致
// 资产烂掉时测试面全绿。本测试以发布同口径守护它:Parse 叠加最严工具
// 白名单态(WithToolAllowlist() 空集 = 禁用一切工具;该资产无 tool 节点,
// 草稿级 Parse 与发布级白名单校验对它等价),通过即「发布校验过 = 守护绿」。
// 同时锚定 README §2.3 的拓扑叙述,防文档与资产漂移:主持人 → 架构/安全/
// 成本三评审员并行 → 首席仲裁菱形汇合 → 人工审批 → branch 按仲裁 verdict
// 双臂路由。真链 E2E(doc-compliance 形态,真栈真审批页)不在此层;真模型
// 走链与输出契约的人工复跑口径见 README 该节。fixture 单一真源 = 资产文件
// 本身(t16.3 seed 脚本共用同一文件),不另立内嵌副本。
func TestExampleAssetAgentCouncil(t *testing.T) {
	const asset = "../../.." + "/docs/examples/agent-council.json"
	data, err := os.ReadFile(asset)
	if err != nil {
		t.Fatalf("read example asset: %v", err)
	}

	wf, err := Parse(data, WithToolAllowlist())
	if err != nil {
		t.Fatalf("agent-council asset must stay publishable (dsl.Parse, strictest allowlist): %v", err)
	}

	if wf.Name != "agent-council" {
		t.Errorf("name = %q, want agent-council (README walk-through references it by name)", wf.Name)
	}

	// 触发契约:README §2.3 的执行 body 只带 proposal;缺声明即触发面 4xx。
	if len(wf.Inputs) != 1 || wf.Inputs[0].Name != "proposal" ||
		wf.Inputs[0].Type != "string" || !wf.Inputs[0].Required {
		t.Errorf("inputs = %+v, want single required string input proposal", wf.Inputs)
	}

	// 节点构成:9 节点 = llm×7(主持人+三评审+仲裁+双臂终稿)+ approval + branch。
	nodes := make(map[string]*Node, len(wf.Nodes))
	kinds := make(map[string]int, 4)
	for i := range wf.Nodes {
		n := &wf.Nodes[i]
		nodes[n.ID] = n
		kinds[n.Type]++
	}
	if len(wf.Nodes) != 9 {
		t.Errorf("node count = %d, want 9", len(wf.Nodes))
	}
	if kinds["llm"] != 7 || kinds["approval"] != 1 || kinds["branch"] != 1 {
		t.Errorf("node kinds = %v, want llm=7 approval=1 branch=1", kinds)
	}

	// 拓扑锚(README §2.3 叙述):全量出边表对拍,兼顾并行分叉与菱形汇合。
	wantOut := map[string][]string{
		"planner":       {"arch_reviewer", "cost_reviewer", "sec_reviewer"},
		"arch_reviewer": {"judge"},
		"sec_reviewer":  {"judge"},
		"cost_reviewer": {"judge"},
		"judge":         {"human_review"},
		"human_review":  {"gate"},
		"gate":          {"final_report", "revision"},
		"final_report":  nil,
		"revision":      nil,
	}
	gotOut := make(map[string][]string, len(wantOut))
	for _, e := range wf.Edges {
		gotOut[e.From] = append(gotOut[e.From], e.To)
	}
	for from, want := range wantOut {
		got := gotOut[from]
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("out-edges of %s = %v, want %v", from, got, want)
		}
	}
	if len(wf.Edges) != 10 {
		t.Errorf("edge count = %d, want 10", len(wf.Edges))
	}
	for from := range gotOut {
		if _, ok := wantOut[from]; !ok {
			t.Errorf("unexpected edge source %s", from)
		}
	}

	// 审批面:README §2.2 建号 alice/bob 与 DSL 名单双门,漂移即断。
	ap := nodes["human_review"].Approval
	if ap == nil {
		t.Fatal("human_review is not an approval node")
	}
	if !slices.Equal(ap.Approvers, []string{"alice", "bob"}) {
		t.Errorf("approvers = %v, want [alice bob]", ap.Approvers)
	}

	// branch 双臂:仲裁 verdict=approve → 决策层终报,否则修订计划。
	br := nodes["gate"].Branch
	if br == nil {
		t.Fatal("gate is not a branch node")
	}
	if len(br.Cases) != 1 {
		t.Fatalf("gate cases = %d, want 1", len(br.Cases))
	}
	c := br.Cases[0]
	var right string
	if err := json.Unmarshal(c.When.Right, &right); err != nil {
		t.Fatalf("gate case right operand: %v", err)
	}
	if c.When.Left != "{{nodes.judge.output.verdict}}" || c.When.Op != "eq" ||
		right != "approve" || c.Next != "final_report" {
		t.Errorf("gate case = {left %s op %s right %q next %s}, want judge verdict eq approve -> final_report",
			c.When.Left, c.When.Op, right, c.Next)
	}
	if br.Default != "revision" {
		t.Errorf("gate default = %q, want revision", br.Default)
	}

	// 产物面:README 承诺批准 → decision_report、修订 → revision_plan。
	for name, ref := range map[string]string{
		"decision_report": "{{nodes.final_report.output.text}}",
		"revision_plan":   "{{nodes.revision.output.text}}",
	} {
		if wf.Outputs[name] != ref {
			t.Errorf("outputs[%s] = %q, want %q", name, wf.Outputs[name], ref)
		}
	}
}
