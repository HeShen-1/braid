package scheduler

// T9.1(D-2026-09-28-t91-parallel-scatter-gather)调度器面测试:
// Instantiate 展开(foreach 求值 → 控制行 + N 实例行)→ fan-out 派发 →
// gather 收敛与部分失败语义 → NodeDefinition 实例缝。e2e(真 asynq)归
// internal/nodes;本文件钉 DB 行为面。
//
// 变异自查(实现错 → 哪条红):
//   - 实例行 type 落 'parallel' 而非 body 类型 → 展开/派发子测试红(worker 面
//     无法执行);
//   - instance_no 从 1 起改为 0 起撞控制行 → uk 1062,展开子测试红;
//   - 展开后行集不全(漏实例)→ lockAllRows「无行」数据缺陷错误,派发子测试红;
//   - gather 不在末实例收口同事务 → 收敛子测试出现控制器滞留 EXECUTING。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"braid/internal/engine"
)

// parallelDoc:pre(llm)→ fan(parallel,body llm retry 3、user_data {{item}} +
// 祖先引用)→ post(tool,内嵌引用 results)。body outputs {verdict} 供 gather。
const parallelDoc = `{
  "dsl": "1.0",
  "name": "parallel-wf",
  "inputs": [{"name": "reviews", "type": "array", "required": true}],
  "nodes": [
    {"id": "pre", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
    {"id": "fan", "type": "parallel", "parallel": {
      "foreach": "{{inputs.reviews}}",
      "body": {"type": "llm", "retry": {"max_attempts": 3},
        "prompt": {"template": "评审",
          "user_data": [{"name": "review", "ref": "{{item}}"},
                        {"name": "ctx", "ref": "{{nodes.pre.output.text}}"}]},
        "outputs": {"verdict": "string"}}
    }},
    {"id": "post", "type": "tool", "tool": "http",
     "inputs": {"payload": "{{nodes.fan.output.results}}"}, "outputs": {"done": "boolean"}}
  ],
  "edges": [{"from": "pre", "to": "fan"}, {"from": "fan", "to": "post"}],
  "outputs": {"all": "{{nodes.fan.output.results}}"}
}`

// nodeInstanceRow 是断言用的实例维度节点行(node_id, instance_no)。
type nodeInstanceRow struct {
	nodeID      string
	instanceNo  uint16
	typ         string
	status      engine.NodeState
	maxAttempts uint16
}

func TestInstantiateParallelExpansion(t *testing.T) {
	s, d := newSched(t)

	instRows := func(t *testing.T, execID uint64) []nodeInstanceRow {
		t.Helper()
		rs, err := d.Query(`
SELECT node_id, instance_no, type, status, max_attempts
  FROM node_execution WHERE execution_id=?
  ORDER BY node_id, instance_no`, execID)
		if err != nil {
			t.Fatalf("query nodes: %v", err)
		}
		defer func() { _ = rs.Close() }()
		var out []nodeInstanceRow
		for rs.Next() {
			var r nodeInstanceRow
			if err := rs.Scan(&r.nodeID, &r.instanceNo, &r.typ, &r.status, &r.maxAttempts); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, r)
		}
		return out
	}

	t.Run("expands_controller_and_instances", func(t *testing.T) {
		defID := seedDef(t, d, parallelDoc)
		execID, err := s.Instantiate(context.Background(), InstantiateParams{
			DefinitionID: defID, Input: json.RawMessage(`{"reviews": ["r0", "r1", "r2"]}`), TriggeredBy: "test",
		})
		if err != nil {
			t.Fatalf("Instantiate: %v", err)
		}
		t.Cleanup(func() { cleanupExec(t, d, execID) })
		got := instRows(t, execID)
		want := []nodeInstanceRow{
			{"fan", 0, "parallel", engine.NodePending, 1},
			{"fan", 1, "llm", engine.NodePending, 3},
			{"fan", 2, "llm", engine.NodePending, 3},
			{"fan", 3, "llm", engine.NodePending, 3},
			{"post", 0, "tool", engine.NodePending, 1},
			{"pre", 0, "llm", engine.NodePending, 1},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("rows = %+v, want %+v", got, want)
		}
	})

	t.Run("empty_array_controller_only", func(t *testing.T) {
		defID := seedDef(t, d, parallelDoc)
		execID, err := s.Instantiate(context.Background(), InstantiateParams{
			DefinitionID: defID, Input: json.RawMessage(`{"reviews": []}`), TriggeredBy: "test",
		})
		if err != nil {
			t.Fatalf("Instantiate: %v", err)
		}
		t.Cleanup(func() { cleanupExec(t, d, execID) })
		got := instRows(t, execID)
		var fan []nodeInstanceRow
		for _, r := range got {
			if r.nodeID == "fan" {
				fan = append(fan, r)
			}
		}
		want := []nodeInstanceRow{{"fan", 0, "parallel", engine.NodePending, 1}}
		if !reflect.DeepEqual(fan, want) {
			t.Fatalf("fan rows = %+v, want %+v", fan, want)
		}
	})

	// 拒绝面:foreach 求值在事务外先验,execution 行不落库。
	overLimitInput := `{"reviews": [` + strings.Repeat(`"x",`, MaxParallelInstances) + `"y"]}`
	rejected := []struct {
		name  string
		input string
		want  string
	}{
		{"non_array", `{"reviews": "nope"}`, "must resolve to a JSON array"},
		{"missing_input", `{}`, "foreach"},
		{"over_limit", overLimitInput, "parallel_fanout_limit"},
	}
	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			defID := seedDef(t, d, parallelDoc)
			_, err := s.Instantiate(context.Background(), InstantiateParams{
				DefinitionID: defID, Input: json.RawMessage(tt.input), TriggeredBy: "test",
			})
			if err == nil {
				t.Fatalf("expected rejection, got nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.want)
			}
		})
	}
}

// parallelNoRetryDoc / parallelWaitDoc:失败语义测试体——body 无 retry(单次
// 失败即终败),wait 版钉 on_failure=wait_all。元素为对象,覆盖 {{item.<field>}}。
const parallelNoRetryBody = `{
  "dsl": "1.0",
  "name": "parallel-wf",
  "inputs": [{"name": "items", "type": "array", "required": true}],
  "nodes": [
    {"id": "pre", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
    {"id": "fan", "type": "parallel", "parallel": {
      "foreach": "{{inputs.items}}",
      %s
      "body": {"type": "llm",
        "prompt": {"template": "评审",
          "user_data": [{"name": "name", "ref": "{{item.name}}"},
                        {"name": "ctx", "ref": "{{nodes.pre.output.text}}"}]},
        "outputs": {"verdict": "string"}}
    }},
    {"id": "post", "type": "tool", "tool": "http",
     "inputs": {"payload": "{{nodes.fan.output.results}}"}, "outputs": {"done": "boolean"}}
  ],
  "edges": [{"from": "pre", "to": "fan"}, {"from": "fan", "to": "post"}],
  "outputs": {"all": "{{nodes.fan.output.results}}"}
}`

// decodeSem 是语义比较解码(UseNumber;MySQL JSON 列读回重排版,断言按
// 语义不按字节——run_test 既有口径)。
func decodeSem(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return v
}

// instanceRowOf 读单实例行(含类型/instance_no 维度)。
func instanceRowOf(t testing.TB, d *sql.DB, execID uint64, nodeID string, instanceNo uint16) nodeSnapshotRow {
	t.Helper()
	var r nodeSnapshotRow
	var typ string
	var ino uint16
	var deadline sql.NullTime
	err := d.QueryRow(`
SELECT id, status, version, attempts, max_attempts, worker_id, input_snapshot, output, deadline_at, type, instance_no
  FROM node_execution WHERE execution_id=? AND node_id=? AND instance_no=?`,
		execID, nodeID, instanceNo).
		Scan(&r.id, &r.status, &r.version, &r.attempts, &r.maxA, &r.workerID, &r.snapshot, &r.output, &deadline, &typ, &ino)
	if err != nil {
		t.Fatalf("read node %s[%d]: %v", nodeID, instanceNo, err)
	}
	if ino != instanceNo {
		t.Fatalf("read node %s[%d]: got instance %d", nodeID, instanceNo, ino)
	}
	r.deadlineOK = deadline.Valid
	return r
}

// runToFanReady 物化 + Start + 完成 pre,使 fan 到达派发位。返回 fan 实例行 id 集(输入序)。
func runToFanReady(t testing.TB, s *Scheduler, d *sql.DB, doc, input string, fanID string) (execID uint64, instances []uint64) {
	t.Helper()
	_, execID = inst(t, s, d, doc, input)
	if err := s.Start(context.Background(), execID, "w1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := completeNode(t, s, d, execID, "pre", engine.AttemptSucceed, json.RawMessage(`{"text":"ctx-out"}`)); err != nil {
		t.Fatalf("complete pre: %v", err)
	}
	rows := readAllInstanceRowsOf(t, d, execID, fanID)
	for _, r := range rows {
		if r.instanceNo > 0 {
			instances = append(instances, r.id)
		}
	}
	return execID, instances
}

func readAllInstanceRowsOf(t testing.TB, d *sql.DB, execID uint64, nodeID string) []struct {
	id         uint64
	instanceNo uint16
	status     engine.NodeState
} {
	t.Helper()
	rs, err := d.Query(`
SELECT id, instance_no, status FROM node_execution WHERE execution_id=? AND node_id=? ORDER BY instance_no`,
		execID, nodeID)
	if err != nil {
		t.Fatalf("query %s rows: %v", nodeID, err)
	}
	defer func() { _ = rs.Close() }()
	var out []struct {
		id         uint64
		instanceNo uint16
		status     engine.NodeState
	}
	for rs.Next() {
		var r struct {
			id         uint64
			instanceNo uint16
			status     engine.NodeState
		}
		if err := rs.Scan(&r.id, &r.instanceNo, &r.status); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func TestParallelDispatchGather(t *testing.T) {
	s, d := newSched(t)

	t.Run("fanout_snapshots_and_att_events", func(t *testing.T) {
		execID, instances := runToFanReady(t, s, d, parallelDoc, `{"reviews": ["r0", "r1", "r2"]}`, "fan")
		if len(instances) != 3 {
			t.Fatalf("instances = %d, want 3", len(instances))
		}
		// 控制行 EXECUTING、deadline NULL(无驻留,branch 同款);实例行 EXECUTING、
		// deadline 有效(body 类型超时)。
		ctrl := instanceRowOf(t, d, execID, "fan", 0)
		if ctrl.status != engine.NodeExecuting || ctrl.deadlineOK {
			t.Fatalf("controller = %s deadline=%v, want EXECUTING/NULL", ctrl.status, ctrl.deadlineOK)
		}
		for i := 1; i <= 3; i++ {
			r := instanceRowOf(t, d, execID, "fan", uint16(i))
			if r.status != engine.NodeExecuting || !r.deadlineOK {
				t.Fatalf("instance %d = %s deadline=%v, want EXECUTING/valid", i, r.status, r.deadlineOK)
			}
			if !r.snapshot.Valid {
				t.Fatalf("instance %d has no input_snapshot", i)
			}
			var snap LLMInput
			if err := json.Unmarshal([]byte(r.snapshot.String), &snap); err != nil {
				t.Fatalf("instance %d snapshot decode: %v", i, err)
			}
			if len(snap.UserData) != 2 ||
				snap.UserData[0].Value != fmt.Sprintf("r%d", i-1) || snap.UserData[1].Value != "ctx-out" {
				t.Fatalf("instance %d user_data = %+v, want item=r%d ctx=ctx-out", i, snap.UserData, i-1)
			}
		}
		// att 事件:pre 1 + 实例 3;task_id 按 att:<row>:<attempt>。
		atts := 0
		for _, id := range taskEvents(t, d, execID) {
			if strings.HasPrefix(id, "att:") {
				atts++
			}
		}
		if atts != 4 {
			t.Fatalf("att events = %d, want 4 (pre + 3 instances)", atts)
		}
	})

	t.Run("gather_converges_in_input_order", func(t *testing.T) {
		execID, instances := runToFanReady(t, s, d, parallelDoc, `{"reviews": ["r0", "r1", "r2"]}`, "fan")
		// 乱序完成 3,1,2:results 仍按 instance 序。
		order := []int{2, 0, 1}
		for _, idx := range order {
			attemptID := seedAttempt(t, d, execID, instances[idx], 1)
			mustComplete(t, s, execID, instances[idx], attemptID, engine.AttemptSucceed,
				json.RawMessage(fmt.Sprintf(`{"verdict":"v%d"}`, idx)))
		}
		ctrl := instanceRowOf(t, d, execID, "fan", 0)
		if ctrl.status != engine.NodeSucceeded {
			t.Fatalf("controller = %s, want SUCCEEDED", ctrl.status)
		}
		wantResults := []any{
			map[string]any{"verdict": "v0"},
			map[string]any{"verdict": "v1"},
			map[string]any{"verdict": "v2"},
		}
		if !ctrl.output.Valid {
			t.Fatalf("controller has no output")
		}
		if got := decodeSem(t, ctrl.output.String).(map[string]any)["results"]; !reflect.DeepEqual(got, wantResults) {
			t.Fatalf("controller results = %v, want %v (输入序)", got, wantResults)
		}
		// post 已派发;完成 post 后执行收口 SUCCEEDED,工作流 outputs 物化。
		post := readNode(t, d, execID, "post")
		if post.status != engine.NodeExecuting {
			t.Fatalf("post = %s, want EXECUTING", post.status)
		}
		if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
			t.Fatalf("complete post: %v", err)
		}
		ex := readExec(t, d, execID)
		if ex.status != engine.WorkflowSucceeded {
			t.Fatalf("execution = %s, want SUCCEEDED", ex.status)
		}
		if !ex.outputs.Valid {
			t.Fatalf("workflow outputs missing")
		}
		if got := decodeSem(t, ex.outputs.String).(map[string]any)["all"]; !reflect.DeepEqual(got, wantResults) {
			t.Fatalf("workflow outputs.all = %v, want %v", got, wantResults)
		}
	})

	t.Run("empty_array_immediate_converge", func(t *testing.T) {
		execID, instances := runToFanReady(t, s, d, parallelDoc, `{"reviews": []}`, "fan")
		if len(instances) != 0 {
			t.Fatalf("instances = %d, want 0", len(instances))
		}
		ctrl := instanceRowOf(t, d, execID, "fan", 0)
		if ctrl.status != engine.NodeSucceeded || !ctrl.output.Valid {
			t.Fatalf("controller = %s output %v, want SUCCEEDED with results", ctrl.status, ctrl.output.String)
		}
		if got := decodeSem(t, ctrl.output.String).(map[string]any)["results"]; !reflect.DeepEqual(got, []any{}) {
			t.Fatalf("controller results = %v, want []", got)
		}
		if post := readNode(t, d, execID, "post"); post.status != engine.NodeExecuting {
			t.Fatalf("post = %s, want EXECUTING (vacuous gather 派发后继)", post.status)
		}
	})
}

func TestParallelPartialFailure(t *testing.T) {
	s, d := newSched(t)
	input := `{"items": [{"name":"a"},{"name":"b"},{"name":"c"}]}`

	t.Run("fail_fast_default", func(t *testing.T) {
		doc := fmt.Sprintf(parallelNoRetryBody, "")
		execID, instances := runToFanReady(t, s, d, doc, input, "fan")
		// 实例 2 终败 → 控制器 FAILED、执行 FAILED、后继滞留 PENDING、在飞实例留 EXECUTING。
		a := seedAttempt(t, d, execID, instances[1], 1)
		mustComplete(t, s, execID, instances[1], a, engine.AttemptFailed, nil)
		if ctrl := instanceRowOf(t, d, execID, "fan", 0); ctrl.status != engine.NodeFailed {
			t.Fatalf("controller = %s, want FAILED (fail_fast)", ctrl.status)
		}
		ex := readExec(t, d, execID)
		if ex.status != engine.WorkflowFailed || ex.errorCode.String != "node_failed" {
			t.Fatalf("execution = %s code %v, want FAILED/node_failed", ex.status, ex.errorCode.String)
		}
		if r := instanceRowOf(t, d, execID, "fan", 2); r.status != engine.NodeFailed {
			t.Fatalf("failed instance = %s, want FAILED", r.status)
		}
		if r := instanceRowOf(t, d, execID, "fan", 3); r.status != engine.NodeExecuting {
			t.Fatalf("in-flight instance = %s, want EXECUTING (自然跑完不再推进)", r.status)
		}
		if post := readNode(t, d, execID, "post"); post.status != engine.NodePending {
			t.Fatalf("post = %s, want PENDING", post.status)
		}
	})

	t.Run("wait_all_collects_then_fails", func(t *testing.T) {
		doc := fmt.Sprintf(parallelNoRetryBody, `"on_failure": "wait_all",`)
		execID, instances := runToFanReady(t, s, d, doc, input, "fan")
		// 实例 2 终败:执行仍 RUNNING、控制器仍 EXECUTING(收齐再判)。
		a := seedAttempt(t, d, execID, instances[1], 1)
		mustComplete(t, s, execID, instances[1], a, engine.AttemptFailed, nil)
		if ex := readExec(t, d, execID); ex.status != engine.WorkflowRunning {
			t.Fatalf("execution = %s, want RUNNING (wait_all 未收齐)", ex.status)
		}
		if ctrl := instanceRowOf(t, d, execID, "fan", 0); ctrl.status != engine.NodeExecuting {
			t.Fatalf("controller = %s, want EXECUTING (wait_all)", ctrl.status)
		}
		// 收齐(1、3 成功):控制器 FAILED、执行 FAILED。
		for _, idx := range []int{0, 2} {
			at := seedAttempt(t, d, execID, instances[idx], 1)
			mustComplete(t, s, execID, instances[idx], at, engine.AttemptSucceed,
				json.RawMessage(fmt.Sprintf(`{"verdict":"v%d"}`, idx)))
		}
		if ctrl := instanceRowOf(t, d, execID, "fan", 0); ctrl.status != engine.NodeFailed {
			t.Fatalf("controller = %s, want FAILED (wait_all 收齐且含败)", ctrl.status)
		}
		ex := readExec(t, d, execID)
		if ex.status != engine.WorkflowFailed || ex.errorCode.String != "node_failed" {
			t.Fatalf("execution = %s code %v, want FAILED/node_failed", ex.status, ex.errorCode.String)
		}
		if post := readNode(t, d, execID, "post"); post.status != engine.NodePending {
			t.Fatalf("post = %s, want PENDING", post.status)
		}
	})
}

// TestParallelSnapshotEvalZeroResidue(对抗审计 MEDIUM 回归):异构数组使第 2
// 实例快照求值失败({{item.name}} 未命中)→ input_eval 终败且零派发残留——
// 控制器/实例恒 PENDING、零 att 事件(半程 fan-out 会空烧供应商调用)。
func TestParallelSnapshotEvalZeroResidue(t *testing.T) {
	s, d := newSched(t)
	doc := fmt.Sprintf(parallelNoRetryBody, "")
	execID, _ := runToFanReady(t, s, d, doc, `{"items": [{"name":"a"}, "scalar"]}`, "fan")
	ex := readExec(t, d, execID)
	if ex.status != engine.WorkflowFailed || ex.errorCode.String != "input_eval" {
		t.Fatalf("execution = %s code %v, want FAILED/input_eval", ex.status, ex.errorCode.String)
	}
	ctrl := instanceRowOf(t, d, execID, "fan", 0)
	if ctrl.status != engine.NodePending {
		t.Fatalf("controller = %s, want PENDING(快照求值先于 claim,零残留)", ctrl.status)
	}
	for i := uint16(1); i <= 2; i++ {
		if st := instanceStateNow(t, d, execID, "fan", i); st != engine.NodePending {
			t.Fatalf("instance %d = %s, want PENDING", i, st)
		}
	}
	var atts int
	if err := d.QueryRow(`SELECT COUNT(*) FROM outbox WHERE execution_id=? AND task_id LIKE 'att:%'`,
		execID).Scan(&atts); err != nil {
		t.Fatalf("count att events: %v", err)
	}
	if atts != 1 { // pre 的 att;fan 零派发
		t.Fatalf("att events = %d, want 1 (pre only, fan 零派发)", atts)
	}
}

// instanceStateNow 读实例行当前态(scheduler 侧;nodes 包 e2e 另有同名)。
func instanceStateNow(t *testing.T, d *sql.DB, execID uint64, nodeID string, instanceNo uint16) engine.NodeState {
	t.Helper()
	var st engine.NodeState
	if err := d.QueryRow(`SELECT status FROM node_execution WHERE execution_id=? AND node_id=? AND instance_no=?`,
		execID, nodeID, instanceNo).Scan(&st); err != nil {
		t.Fatalf("read instance: %v", err)
	}
	return st
}

// TestParallelResumeEmptyArrayKeepsInputs(对抗审计 HIGH① 回归):审批续跑驱动
// 的空数组 parallel 空集汇聚后,env 必须仍携工作流输入——下游 {{inputs.*}}
// 引用可解析派发(resume 入口曾漏传 wfInput,convergeParallel 用 nil 重建 env)。
func TestParallelResumeEmptyArrayKeepsInputs(t *testing.T) {
	s, d := newSched(t)
	doc := `{"dsl":"1.0","name":"approval-par-t",
 "inputs":[{"name":"items","type":"array"},{"name":"marker","type":"string"}],
 "edges":[{"from":"a","to":"b"},{"from":"b","to":"fan"},{"from":"fan","to":"post"}],
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"gen"},"outputs":{"text":"string"}},
  {"id":"b","type":"approval","approvers":["carol"],"instructions":"请审批","outputs":{"comment":"string"}},
  {"id":"fan","type":"parallel","parallel":{"foreach":"{{inputs.items}}",
    "body":{"type":"llm","prompt":{"template":"t"},"outputs":{"verdict":"string"}}}},
  {"id":"post","type":"tool","tool":"http","inputs":{"m":"{{inputs.marker}}"},"outputs":{"done":"boolean"}}],
 "outputs":{}}`
	_, execID := inst(t, s, d, doc, `{"items":[],"marker":"M"}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	attemptA := seedAttempt(t, d, execID, a.id, 1)
	mustComplete(t, s, execID, a.id, attemptA, engine.AttemptSucceed, json.RawMessage(`{"text":"v"}`))
	b := waitNodeStatus(t, d, execID, "b", engine.NodeExecuting)
	attemptB := seedAttempt(t, d, execID, b.id, 1)
	mustSuspend(t, s, execID, b.id, attemptB)
	decisionID := seedDecision(t, d, execID, b.id, "b", "carol", DecisionApproved, "可以")
	mustResume(t, s, execID, b.id, decisionID, b.attempts+1)

	// fan 空集汇聚 SUCCEEDED(results=[]);post 的 {{inputs.marker}} 必须解析
	// 成功并派发(HIGH① 修前此处 input_eval 误终败)。
	ctrl := instanceRowOf(t, d, execID, "fan", 0)
	if ctrl.status != engine.NodeSucceeded {
		t.Fatalf("controller = %s, want SUCCEEDED (空集汇聚)", ctrl.status)
	}
	post := waitNodeStatus(t, d, execID, "post", engine.NodeExecuting)
	snap := decodeSem(t, post.snapshot.String).(map[string]any)
	inputs, _ := snap["inputs"].(map[string]any)
	if inputs["m"] != "M" {
		t.Fatalf("post snapshot inputs = %v, want m=M(inputs.marker 解析值)", inputs)
	}
}
