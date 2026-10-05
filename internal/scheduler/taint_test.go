package scheduler

// T18.3(D-2026-10-05-t183-taint-tracking)验收守护:污点传播证据行
// node.taint_propagated 的发射锚与 payload 形状。单元面(静态分析)归
// internal/engine/dsl/taint_test.go,词表闭集归 internal/audit/writer_test.go。

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"

	"braid/internal/audit"
	"braid/internal/engine"
)

// taintFlowDSL:输入 q 直染 a(llm)→ 闭包染 b(tool);br 对输入 flag 本地路由;
// c 直染;d 无引用(路由不达 + 恒净双保险)。
const taintFlowDSL = `{"dsl":"1.0","name":"taint-flow-t",
 "inputs":[{"name":"q","type":"string"},{"name":"flag","type":"boolean"}],
 "edges":[{"from":"a","to":"b"},{"from":"b","to":"br"},{"from":"br","to":"c"},{"from":"br","to":"d"}],
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"gen","user_data":[{"name":"q","ref":"{{inputs.q}}"}]},"outputs":{"text":"string"}},
  {"id":"b","type":"tool","tool":"echo","inputs":{"cmd":"use {{nodes.a.output.text}}"},"outputs":{"out":"string"}},
  {"id":"br","type":"branch","cases":[{"when":{"left":"{{inputs.flag}}","op":"eq","right":true},"next":"c"}],"default":"d"},
  {"id":"c","type":"llm","prompt":{"template":"c","user_data":[{"name":"q","ref":"{{inputs.q}}"}]},"outputs":{"text":"string"}},
  {"id":"d","type":"llm","prompt":{"template":"d"},"outputs":{"text":"string"}}],
 "outputs":{"final":"{{nodes.c.output.text}}"}}`

// taintRetryDSL:带污点引用的可重试节点——重试 = 每 attempt 各一行证据。
const taintRetryDSL = `{"dsl":"1.0","name":"taint-retry-t",
 "inputs":[{"name":"q","type":"string"}],
 "nodes":[{"id":"a","type":"llm","retry":{"max_attempts":2},
  "prompt":{"template":"x","user_data":[{"name":"q","ref":"{{inputs.q}}"}]},"outputs":{"text":"string"}}]}`

// taintRow 是 node.taint_propagated 行的断言投影。
type taintRow struct {
	nodeID    string
	sink      string
	paths     []string
	attemptID sql.NullInt64
}

// taintRows 读该执行全部污点证据行(先排空暂存,与 auditEvents 同口径)。
func taintRows(t *testing.T, d *sql.DB, execID uint64) []taintRow {
	t.Helper()
	if _, err := audit.FlushAll(context.Background(), d, 500, 10000); err != nil {
		t.Fatalf("flush staged audit: %v", err)
	}
	rows, err := d.Query(`
SELECT node_id, JSON_UNQUOTE(JSON_EXTRACT(payload,'$.sink')), JSON_EXTRACT(payload,'$.paths'), attempt_id
  FROM audit_log WHERE execution_id=? AND event_type='node.taint_propagated' ORDER BY id`, execID)
	if err != nil {
		t.Fatalf("query taint rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []taintRow
	for rows.Next() {
		var r taintRow
		var pathsRaw string
		if err := rows.Scan(&r.nodeID, &r.sink, &pathsRaw, &r.attemptID); err != nil {
			t.Fatalf("scan taint row: %v", err)
		}
		if err := json.Unmarshal([]byte(pathsRaw), &r.paths); err != nil {
			t.Fatalf("decode taint paths %s: %v", pathsRaw, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("taint rows: %v", err)
	}
	return out
}

// TestAuditTaintFlowLifecycle:a(llm,直染)→ b(tool,闭包染)→ br(branch,
// 本地消费)→ c(llm,直染);d 路由不达恒零行。行序与 payload 双断言。
func TestAuditTaintFlowLifecycle(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, taintFlowDSL, `{"q":"secret-1","flag":true}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	attA := seedAttempt(t, d, execID, a.id, 1)
	mustComplete(t, s, execID, a.id, attA, engine.AttemptSucceed, json.RawMessage(`{"text":"t1"}`))

	b := waitNodeStatus(t, d, execID, "b", engine.NodeExecuting)
	attB := seedAttempt(t, d, execID, b.id, 1)
	mustComplete(t, s, execID, b.id, attB, engine.AttemptSucceed, json.RawMessage(`{"out":"o1"}`))

	// br 内联收口(路由 c,剪枝 d),无 attempt 行。
	waitNodeStatus(t, d, execID, "br", engine.NodeSucceeded)
	c := waitNodeStatus(t, d, execID, "c", engine.NodeExecuting)
	attC := seedAttempt(t, d, execID, c.id, 1)
	mustComplete(t, s, execID, c.id, attC, engine.AttemptSucceed, json.RawMessage(`{"text":"t2"}`))
	waitExecStatus(t, d, execID, engine.WorkflowSucceeded)

	got := taintRows(t, d, execID)
	want := []taintRow{
		{nodeID: "a", sink: audit.SinkLLM, paths: []string{"inputs.q"}, attemptID: sql.NullInt64{Int64: int64(attA), Valid: true}},
		{nodeID: "b", sink: audit.SinkTool, paths: []string{"inputs.q"}, attemptID: sql.NullInt64{Int64: int64(attB), Valid: true}},
		{nodeID: "br", sink: audit.SinkNone, paths: []string{"inputs.flag"}},
		{nodeID: "c", sink: audit.SinkLLM, paths: []string{"inputs.q"}, attemptID: sql.NullInt64{Int64: int64(attC), Valid: true}},
	}
	if len(got) != len(want) {
		t.Fatalf("taint rows = %d, want %d\ngot: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].nodeID != want[i].nodeID || got[i].sink != want[i].sink ||
			got[i].attemptID != want[i].attemptID || !reflect.DeepEqual(got[i].paths, want[i].paths) {
			t.Fatalf("taint[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// 行序:taint 行夹在 claim 迁移与收口迁移之间(Complete 收口段先于节点收口);
	// branch 的 taint 行在 branch.evaluated 之后。
	assertAuditSequence(t, d, execID, []auditEvent{
		wantEvent(audit.EventNodeStateChanged, "a", 0),
		wantEvent(audit.EventNodeTaintPropagated, "a", attA),
		wantEvent(audit.EventNodeStateChanged, "a", attA),
		wantEvent(audit.EventNodeStateChanged, "b", 0),
		wantEvent(audit.EventNodeTaintPropagated, "b", attB),
		wantEvent(audit.EventNodeStateChanged, "b", attB),
		wantEvent(audit.EventNodeStateChanged, "br", 0),
		wantEvent(audit.EventNodeStateChanged, "br", 0),
		wantEvent(audit.EventBranchEvaluated, "br", 0),
		wantEvent(audit.EventNodeTaintPropagated, "br", 0),
		wantEvent(audit.EventNodeStateChanged, "d", 0),
		wantEvent(audit.EventNodeStateChanged, "c", 0),
		wantEvent(audit.EventNodeTaintPropagated, "c", attC),
		wantEvent(audit.EventNodeStateChanged, "c", attC),
	})
}

// TestAuditTaintFlowRetryPerAttempt:失败重试的每次 attempt 各留一行
// (每次真实进通道;attempt_id 逐行锚定)。
func TestAuditTaintFlowRetryPerAttempt(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, taintRetryDSL, `{"q":"secret-2"}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	att1 := seedAttempt(t, d, execID, a.id, 1)
	mustComplete(t, s, execID, a.id, att1, engine.AttemptFailed, nil)

	a2 := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	att2 := seedAttempt(t, d, execID, a2.id, 2)
	mustComplete(t, s, execID, a2.id, att2, engine.AttemptSucceed, json.RawMessage(`{"text":"ok"}`))
	waitExecStatus(t, d, execID, engine.WorkflowSucceeded)

	got := taintRows(t, d, execID)
	if len(got) != 2 {
		t.Fatalf("taint rows = %d, want 2 (per attempt)\ngot: %+v", len(got), got)
	}
	if got[0].attemptID.Int64 != int64(att1) || got[1].attemptID.Int64 != int64(att2) {
		t.Fatalf("taint attempt anchors = [%d,%d], want [%d,%d]",
			got[0].attemptID.Int64, got[1].attemptID.Int64, att1, att2)
	}
	for _, r := range got {
		if r.sink != audit.SinkLLM || !reflect.DeepEqual(r.paths, []string{"inputs.q"}) {
			t.Fatalf("taint row %+v, want sink=llm paths=[inputs.q]", r)
		}
	}
}

// TestAuditTaintFlowParallelInstances(T18.3):parallel 实例行按 body 类型
// 定 sink、控制行(gather 锚)不经 Complete 零行、干净前驱(pre)零行、
// 后继经 results 继承传递集。复用 parallelDoc:fan = foreach {{inputs.reviews}}
// + body user_data {{item}}/{{nodes.pre.output.text}}(pre 恒净 → 传递集
// 恰为 foreach 源)。
func TestAuditTaintFlowParallelInstances(t *testing.T) {
	s, d := newSched(t)
	execID, instances := runToFanReady(t, s, d, parallelDoc, `{"reviews": ["r0", "r1", "r2"]}`, "fan")
	if len(instances) != 3 {
		t.Fatalf("instances = %d, want 3", len(instances))
	}
	// 负向控制:pre 无 input 引用,fan 收口前零 taint 行。
	if rows := taintRows(t, d, execID); len(rows) != 0 {
		t.Fatalf("taint rows before fan close = %+v, want none (pre clean)", rows)
	}
	atts := make([]uint64, len(instances))
	for i, ne := range instances {
		atts[i] = seedAttempt(t, d, execID, ne, 1)
		mustComplete(t, s, execID, ne, atts[i], engine.AttemptSucceed, json.RawMessage(`{"verdict":"v"}`))
	}
	post := waitNodeStatus(t, d, execID, "post", engine.NodeExecuting)
	attPost := seedAttempt(t, d, execID, post.id, 1)
	mustComplete(t, s, execID, post.id, attPost, engine.AttemptSucceed, json.RawMessage(`{"done":true}`))
	waitExecStatus(t, d, execID, engine.WorkflowSucceeded)

	got := taintRows(t, d, execID)
	want := []taintRow{
		{nodeID: "fan", sink: audit.SinkLLM, paths: []string{"inputs.reviews"},
			attemptID: sql.NullInt64{Int64: int64(atts[0]), Valid: true}},
		{nodeID: "fan", sink: audit.SinkLLM, paths: []string{"inputs.reviews"},
			attemptID: sql.NullInt64{Int64: int64(atts[1]), Valid: true}},
		{nodeID: "fan", sink: audit.SinkLLM, paths: []string{"inputs.reviews"},
			attemptID: sql.NullInt64{Int64: int64(atts[2]), Valid: true}},
		{nodeID: "post", sink: audit.SinkTool, paths: []string{"inputs.reviews"},
			attemptID: sql.NullInt64{Int64: int64(attPost), Valid: true}},
	}
	if len(got) != len(want) {
		t.Fatalf("taint rows = %d, want %d\ngot: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].nodeID != want[i].nodeID || got[i].sink != want[i].sink ||
			got[i].attemptID != want[i].attemptID || !reflect.DeepEqual(got[i].paths, want[i].paths) {
			t.Fatalf("taint[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// taintGuardDSL:e1 恒净终败(fail-fast)→ e2(染污)迟到成功:终态守卫
// 只阻断推进,不阻断收口段 —— taint 行仍须落地(3c 锚位的语义钉)。
const taintGuardDSL = `{"dsl":"1.0","name":"taint-guard-t",
 "inputs":[{"name":"q","type":"string"}],
 "edges":[{"from":"e1","to":"x"},{"from":"e2","to":"y"},{"from":"y","to":"z"}],
 "nodes":[
  {"id":"e1","type":"llm","prompt":{"template":"boom"},"outputs":{"t":"string"}},
  {"id":"e2","type":"llm","prompt":{"template":"e2","user_data":[{"name":"q","ref":"{{inputs.q}}"}]},"outputs":{"t":"string"}},
  {"id":"x","type":"llm","prompt":{"template":"x"},"outputs":{"t":"string"}},
  {"id":"y","type":"llm","prompt":{"template":"y","user_data":[{"name":"v","ref":"{{nodes.e2.output.t}}"}]},"outputs":{"t":"string"}},
  {"id":"z","type":"llm","prompt":{"template":"z"},"outputs":{"t":"string"}}]}`

// TestAuditTaintFlowTerminalGuardLateClose:终态执行内的迟到收口仍发 taint
// 证据行(镜像 TestTerminalGuardStopsAdvancement 的 state_changed 断言)。
func TestAuditTaintFlowTerminalGuardLateClose(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, taintGuardDSL, `{"q":"late"}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	e1 := waitNodeStatus(t, d, execID, "e1", engine.NodeExecuting)
	e2 := waitNodeStatus(t, d, execID, "e2", engine.NodeExecuting)

	attE1 := seedAttempt(t, d, execID, e1.id, 1)
	mustComplete(t, s, execID, e1.id, attE1, engine.AttemptFailed, nil)
	waitExecStatus(t, d, execID, engine.WorkflowFailed)

	attE2 := seedAttempt(t, d, execID, e2.id, 1)
	mustComplete(t, s, execID, e2.id, attE2, engine.AttemptSucceed, json.RawMessage(`{"t":"v"}`))

	got := taintRows(t, d, execID)
	if len(got) != 1 || got[0].nodeID != "e2" || got[0].sink != audit.SinkLLM ||
		!reflect.DeepEqual(got[0].paths, []string{"inputs.q"}) ||
		got[0].attemptID.Int64 != int64(attE2) {
		t.Fatalf("taint rows = %+v, want single e2/llm/[inputs.q] row (late close still evidences)", got)
	}
}
