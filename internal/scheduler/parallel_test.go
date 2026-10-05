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
		// 整图断言(T19.1 审计 MEDIUM② 锚):未启用 min_success 时恰
		// {"results":[…]}——恒发 failed_count 的变异在此红(禁用方向零键)。
		if got := decodeSem(t, ctrl.output.String); !reflect.DeepEqual(got,
			map[string]any{"results": wantResults}) {
			t.Fatalf("controller output = %v, want exactly %v (无 failed_count)", got, wantResults)
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

// ---- T19.1 min_success 部分容忍(D-2026-10-05-t191)----
//
// 语义:独立字段 [1,N] 与 on_failure 正交。fail_fast+k = 双向尽早收敛
// (成功数 ≥ k 即收敛、不可能达标即早败);wait_all+k = 收齐再判(≥k 过、<k 败);
// k=N 精确退化为 T9.1 现语义。物化:results 长度恒 N 输入序,失败/在飞位
// null;failed_count 恒随启用(全成功 = 0;fail_fast 早收敛 = 收敛时刻终败数,
// 迟到终败不回填)。finishIfDone 仅控制面行 FAILED 阻塞 SUCCEEDED 收口。
//
// 变异自查(实现错 → 哪条红;T19.1 审计 HIGH 修:wait_all 提前收敛方向
// 原为假锚,补 no_early_converge 真锚):
//   - 阈值判定漏「不可能达标」早败 → impossible_early_fail 红(执行滞留 RUNNING);
//   - wait_all+k 提前收敛(succeeded≥k 先判、inFlight 守卫后置)→
//     no_early_converge 红(两成功后控制器仍须 EXECUTING 且无 output);
//   - wait_all+k 删 inFlight 守卫(中途误判终败)→ below_threshold 中间态红;
//   - 控制器终态守卫缺失(早收敛后迟到收口重收敛)→ early_converge 红
//     (SucceedNode 撞已终态行 = Conflict 上抛);
//   - finishIfDone 仍按全行 FAILED 阻塞 → accumulate_to_threshold 终态断言红;
//   - convergeParallel 无条件发 failed_count → gather_converges_in_input_order
//     整图断言红(审计 MEDIUM② 锚);
//   - 收敛后迟到失败仍重派(烧预算)→ converged_sibling_no_retry 红
//     (行须 FAILED/discarded_after_gather 且无第二次 att 事件);
//   - ReapInstanceExhausted 缺提交后 finishIfDone → reap_after_converge 红
//     (全行终态滞留 RUNNING,审计 MEDIUM① 锚)。

func TestParallelMinSuccessConverge(t *testing.T) {
	s, d := newSched(t)
	input := `{"items": [{"name":"a"},{"name":"b"},{"name":"c"}]}`

	verdict := func(i int) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"verdict":"v%d"}`, i))
	}

	t.Run("fail_fast_accumulate_to_threshold", func(t *testing.T) {
		doc := fmt.Sprintf(parallelNoRetryBody, `"min_success": 2,`)
		execID, instances := runToFanReady(t, s, d, doc, input, "fan")
		// 实例 2 终败:0+2 在飞 ≥ 2,可能达标 → 不早败(wait/controller 均 EXECUTING)。
		a := seedAttempt(t, d, execID, instances[1], 1)
		mustComplete(t, s, execID, instances[1], a, engine.AttemptFailed, nil)
		if ex := readExec(t, d, execID); ex.status != engine.WorkflowRunning {
			t.Fatalf("execution = %s, want RUNNING (仍可能达标)", ex.status)
		}
		// 实例 1 成功:1+1 ≥ 2 可能、1 < 2 未达标 → 仍等。
		a = seedAttempt(t, d, execID, instances[0], 1)
		mustComplete(t, s, execID, instances[0], a, engine.AttemptSucceed, verdict(0))
		if ctrl := instanceRowOf(t, d, execID, "fan", 0); ctrl.status != engine.NodeExecuting {
			t.Fatalf("controller = %s, want EXECUTING (1 < min_success 2)", ctrl.status)
		}
		// 实例 3 成功:2 = k → 部分容忍收敛;null 保位 + failed_count=1;post 推进。
		a = seedAttempt(t, d, execID, instances[2], 1)
		mustComplete(t, s, execID, instances[2], a, engine.AttemptSucceed, verdict(2))
		ctrl := instanceRowOf(t, d, execID, "fan", 0)
		if ctrl.status != engine.NodeSucceeded {
			t.Fatalf("controller = %s, want SUCCEEDED (阈值收敛)", ctrl.status)
		}
		if got, want := decodeSem(t, ctrl.output.String),
			decodeSem(t, `{"results":[{"verdict":"v0"},null,{"verdict":"v2"}],"failed_count":1}`); !reflect.DeepEqual(got, want) {
			t.Fatalf("gather output = %v, want %v", got, want)
		}
		if post := readNode(t, d, execID, "post"); post.status != engine.NodeExecuting {
			t.Fatalf("post = %s, want EXECUTING (部分容忍收敛推进后继)", post.status)
		}
		// post 完成 → finishIfDone:实例 2 FAILED 行不得阻塞 SUCCEEDED 收口。
		if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
			t.Fatalf("complete post: %v", err)
		}
		ex := readExec(t, d, execID)
		if ex.status != engine.WorkflowSucceeded {
			t.Fatalf("execution = %s, want SUCCEEDED (实例 FAILED 行不阻塞控制面判定)", ex.status)
		}
		if got, want := decodeSem(t, ex.outputs.String),
			decodeSem(t, `{"all":[{"verdict":"v0"},null,{"verdict":"v2"}]}`); !reflect.DeepEqual(got, want) {
			t.Fatalf("workflow outputs = %v, want %v", got, want)
		}
	})

	t.Run("fail_fast_early_converge_inflight_not_backfilled", func(t *testing.T) {
		doc := fmt.Sprintf(parallelNoRetryBody, `"min_success": 1,`)
		execID, instances := runToFanReady(t, s, d, doc, input, "fan")
		// 实例 1 成功即达标 → 立即收敛;在飞实例 2/3 位 = null,failed_count=0。
		a := seedAttempt(t, d, execID, instances[0], 1)
		mustComplete(t, s, execID, instances[0], a, engine.AttemptSucceed, verdict(0))
		ctrl := instanceRowOf(t, d, execID, "fan", 0)
		if ctrl.status != engine.NodeSucceeded {
			t.Fatalf("controller = %s, want SUCCEEDED (1 ≥ min_success 1 即收敛)", ctrl.status)
		}
		if got, want := decodeSem(t, ctrl.output.String),
			decodeSem(t, `{"results":[{"verdict":"v0"},null,null],"failed_count":0}`); !reflect.DeepEqual(got, want) {
			t.Fatalf("gather output = %v, want %v", got, want)
		}
		if ex := readExec(t, d, execID); ex.status != engine.WorkflowRunning {
			t.Fatalf("execution = %s, want RUNNING (在飞实例未收齐)", ex.status)
		}
		// 迟到成功:行终态但输出不回填(results 单次物化)。
		a = seedAttempt(t, d, execID, instances[1], 1)
		mustComplete(t, s, execID, instances[1], a, engine.AttemptSucceed, verdict(1))
		if st := instanceStateNow(t, d, execID, "fan", 2); st != engine.NodeSucceeded {
			t.Fatalf("late instance = %s, want SUCCEEDED", st)
		}
		// 迟到终败:控制器已终态 → 不重收敛、不终败执行(守卫)。
		a = seedAttempt(t, d, execID, instances[2], 1)
		mustComplete(t, s, execID, instances[2], a, engine.AttemptFailed, nil)
		if ex := readExec(t, d, execID); ex.status != engine.WorkflowRunning {
			t.Fatalf("execution = %s, want RUNNING (收敛后迟到终败不影响已定收敛)", ex.status)
		}
		ctrl = instanceRowOf(t, d, execID, "fan", 0)
		if got, want := decodeSem(t, ctrl.output.String),
			decodeSem(t, `{"results":[{"verdict":"v0"},null,null],"failed_count":0}`); !reflect.DeepEqual(got, want) {
			t.Fatalf("gather output = %v, want %v (不回填,failed_count 收敛时刻下界)", got, want)
		}
		// post 完成 → 全行终态 → SUCCEEDED(含实例 3 FAILED 行)。
		if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
			t.Fatalf("complete post: %v", err)
		}
		if ex := readExec(t, d, execID); ex.status != engine.WorkflowSucceeded {
			t.Fatalf("execution = %s, want SUCCEEDED", ex.status)
		}
	})

	t.Run("fail_fast_impossible_early_fail", func(t *testing.T) {
		// k=N 精确退化:单败即不可能达标 → 与 T9.1 fail_fast 同形。
		doc := fmt.Sprintf(parallelNoRetryBody, `"min_success": 3,`)
		execID, instances := runToFanReady(t, s, d, doc, input, "fan")
		a := seedAttempt(t, d, execID, instances[1], 1)
		mustComplete(t, s, execID, instances[1], a, engine.AttemptFailed, nil)
		ctrl := instanceRowOf(t, d, execID, "fan", 0)
		if ctrl.status != engine.NodeFailed {
			t.Fatalf("controller = %s, want FAILED (不可能达标早败)", ctrl.status)
		}
		if code := nodeErrorCode(t, d, ctrl.id); code != "parallel_instance_failed" {
			t.Fatalf("controller code = %q, want parallel_instance_failed", code)
		}
		ex := readExec(t, d, execID)
		if ex.status != engine.WorkflowFailed || ex.errorCode.String != "node_failed" {
			t.Fatalf("execution = %s code %v, want FAILED/node_failed", ex.status, ex.errorCode.String)
		}
		if r := instanceRowOf(t, d, execID, "fan", 3); r.status != engine.NodeExecuting {
			t.Fatalf("in-flight instance = %s, want EXECUTING (自然跑完)", r.status)
		}
	})

	t.Run("wait_all_judges_at_threshold", func(t *testing.T) {
		doc := fmt.Sprintf(parallelNoRetryBody, `"on_failure": "wait_all", "min_success": 2,`)
		execID, instances := runToFanReady(t, s, d, doc, input, "fan")
		// 实例 2 终败:wait_all 不早判(含败也等收齐)。
		a := seedAttempt(t, d, execID, instances[1], 1)
		mustComplete(t, s, execID, instances[1], a, engine.AttemptFailed, nil)
		if ctrl := instanceRowOf(t, d, execID, "fan", 0); ctrl.status != engine.NodeExecuting {
			t.Fatalf("controller = %s, want EXECUTING (wait_all 收齐再判)", ctrl.status)
		}
		// 收齐(1、3 成功):2 ≥ 2 → 部分容忍收敛。
		for _, idx := range []int{0, 2} {
			at := seedAttempt(t, d, execID, instances[idx], 1)
			mustComplete(t, s, execID, instances[idx], at, engine.AttemptSucceed, verdict(idx))
		}
		ctrl := instanceRowOf(t, d, execID, "fan", 0)
		if ctrl.status != engine.NodeSucceeded {
			t.Fatalf("controller = %s, want SUCCEEDED (收齐且 2 ≥ min_success 2)", ctrl.status)
		}
		if got, want := decodeSem(t, ctrl.output.String),
			decodeSem(t, `{"results":[{"verdict":"v0"},null,{"verdict":"v2"}],"failed_count":1}`); !reflect.DeepEqual(got, want) {
			t.Fatalf("gather output = %v, want %v", got, want)
		}
		if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
			t.Fatalf("complete post: %v", err)
		}
		if ex := readExec(t, d, execID); ex.status != engine.WorkflowSucceeded {
			t.Fatalf("execution = %s, want SUCCEEDED", ex.status)
		}
	})

	t.Run("wait_all_no_early_converge", func(t *testing.T) {
		// 审计 HIGH 修锚:N=4、k=2——两实例先成功(s=2 ≥ k 且在飞 2)时
		// wait_all 必须仍 EXECUTING 且未物化(收齐再判,中途不早判);
		// 随后一败一成收齐 → 部分容忍收敛(null 保位 + failed_count)。
		doc := fmt.Sprintf(parallelNoRetryBody, `"on_failure": "wait_all", "min_success": 2,`)
		execID, instances := runToFanReady(t, s, d, doc,
			`{"items": [{"name":"a"},{"name":"b"},{"name":"c"},{"name":"d"}]}`, "fan")
		for _, idx := range []int{0, 1} {
			at := seedAttempt(t, d, execID, instances[idx], 1)
			mustComplete(t, s, execID, instances[idx], at, engine.AttemptSucceed, verdict(idx))
		}
		ctrl := instanceRowOf(t, d, execID, "fan", 0)
		if ctrl.status != engine.NodeExecuting || ctrl.output.Valid {
			t.Fatalf("controller = %s output %v, want EXECUTING 无 output (wait_all 收齐再判,succeeded=2 ≥ k 但在飞 2)",
				ctrl.status, ctrl.output.Valid)
		}
		a := seedAttempt(t, d, execID, instances[2], 1)
		mustComplete(t, s, execID, instances[2], a, engine.AttemptFailed, nil)
		a = seedAttempt(t, d, execID, instances[3], 1)
		mustComplete(t, s, execID, instances[3], a, engine.AttemptSucceed, verdict(3))
		ctrl = instanceRowOf(t, d, execID, "fan", 0)
		if ctrl.status != engine.NodeSucceeded {
			t.Fatalf("controller = %s, want SUCCEEDED (收齐且 2 ≥ min_success 2)", ctrl.status)
		}
		if got, want := decodeSem(t, ctrl.output.String),
			decodeSem(t, `{"results":[{"verdict":"v0"},{"verdict":"v1"},null,{"verdict":"v3"}],"failed_count":1}`); !reflect.DeepEqual(got, want) {
			t.Fatalf("gather output = %v, want %v", got, want)
		}
	})

	t.Run("wait_all_below_threshold_fails", func(t *testing.T) {
		doc := fmt.Sprintf(parallelNoRetryBody, `"on_failure": "wait_all", "min_success": 2,`)
		execID, instances := runToFanReady(t, s, d, doc, input, "fan")
		for _, idx := range []int{0, 1} {
			at := seedAttempt(t, d, execID, instances[idx], 1)
			mustComplete(t, s, execID, instances[idx], at, engine.AttemptFailed, nil)
		}
		if ctrl := instanceRowOf(t, d, execID, "fan", 0); ctrl.status != engine.NodeExecuting {
			t.Fatalf("controller = %s, want EXECUTING (未收齐)", ctrl.status)
		}
		// 收齐仅 1 成功 < 2 → 控制器/执行终败。
		at := seedAttempt(t, d, execID, instances[2], 1)
		mustComplete(t, s, execID, instances[2], at, engine.AttemptSucceed, verdict(2))
		ctrl := instanceRowOf(t, d, execID, "fan", 0)
		if ctrl.status != engine.NodeFailed {
			t.Fatalf("controller = %s, want FAILED (1 < min_success 2)", ctrl.status)
		}
		if code := nodeErrorCode(t, d, ctrl.id); code != "parallel_instance_failed" {
			t.Fatalf("controller code = %q, want parallel_instance_failed", code)
		}
		if ex := readExec(t, d, execID); ex.status != engine.WorkflowFailed {
			t.Fatalf("execution = %s, want FAILED", ex.status)
		}
		if post := readNode(t, d, execID, "post"); post.status != engine.NodePending {
			t.Fatalf("post = %s, want PENDING", post.status)
		}
	})
}

// nodeErrorCode 读节点行 error_code(失败收口归因断言)。
func nodeErrorCode(t *testing.T, d *sql.DB, nodeExecID uint64) string {
	t.Helper()
	var code sql.NullString
	if err := d.QueryRow(`SELECT error_code FROM node_execution WHERE id=?`, nodeExecID).
		Scan(&code); err != nil {
		t.Fatalf("read error_code node %d: %v", nodeExecID, err)
	}
	return code.String
}

// TestParallelMinSuccessInstantiateRejects:min_success > N(含空数组 + k≥1)
// 在 planFanouts 先验拒绝,错误码 parallel_min_success_limit,零残留。
func TestParallelMinSuccessInstantiateRejects(t *testing.T) {
	s, d := newSched(t)
	for _, tt := range []struct {
		name  string
		extra string
		input string
	}{
		{"over_n", `"min_success": 4,`, `{"items": [{"name":"a"},{"name":"b"},{"name":"c"}]}`},
		{"empty_array", `"min_success": 1,`, `{"items": []}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			doc := fmt.Sprintf(parallelNoRetryBody, tt.extra)
			defID := seedDef(t, d, doc)
			_, err := s.Instantiate(context.Background(), InstantiateParams{
				DefinitionID: defID, Input: json.RawMessage(tt.input), TriggeredBy: "test",
			})
			if err == nil || !strings.Contains(err.Error(), "parallel_min_success_limit") {
				t.Fatalf("error = %v, want parallel_min_success_limit", err)
			}
			var rows int
			if err := d.QueryRow(`SELECT COUNT(*) FROM workflow_execution WHERE definition_id=?`, defID).
				Scan(&rows); err != nil {
				t.Fatalf("count executions: %v", err)
			}
			if rows != 0 {
				t.Fatalf("execution rows = %d, want 0 (先验拒绝零残留)", rows)
			}
		})
	}
}

// TestParallelMinSuccessReapExhausted:reaper 实例耗尽收口经 gatherCheck 阈值
// 判定(T19.1 分流):可能达标 → 等;不可能达标 → 控制器终败(与 completeOnce
// 实例终败分支同款语义,D-t91 ReapInstanceExhausted 契约)。
func TestParallelMinSuccessReapExhausted(t *testing.T) {
	s, d := newSched(t)
	doc := fmt.Sprintf(parallelNoRetryBody, `"min_success": 2,`)
	execID, instances := runToFanReady(t, s, d, doc,
		`{"items": [{"name":"a"},{"name":"b"},{"name":"c"}]}`, "fan")
	// 实例 2 耗尽判死:0 成功 + 2 在飞 ≥ 2 → 仍可能达标,控制器继续等。
	if err := s.ReapInstanceExhausted(context.Background(), execID, instances[1], "reaper-test"); err != nil {
		t.Fatalf("reap instance 2: %v", err)
	}
	if st := instanceStateNow(t, d, execID, "fan", 2); st != engine.NodeFailed {
		t.Fatalf("reaped instance = %s, want FAILED", st)
	}
	ctrl := instanceRowOf(t, d, execID, "fan", 0)
	if ctrl.status != engine.NodeExecuting {
		t.Fatalf("controller = %s, want EXECUTING (仍可能达标)", ctrl.status)
	}
	if ex := readExec(t, d, execID); ex.status != engine.WorkflowRunning {
		t.Fatalf("execution = %s, want RUNNING", ex.status)
	}
	// 实例 1 也耗尽:0 + 1 在飞 < 2 → 不可能达标 → 控制器/执行终败。
	if err := s.ReapInstanceExhausted(context.Background(), execID, instances[0], "reaper-test"); err != nil {
		t.Fatalf("reap instance 1: %v", err)
	}
	ctrl = instanceRowOf(t, d, execID, "fan", 0)
	if ctrl.status != engine.NodeFailed {
		t.Fatalf("controller = %s, want FAILED (不可能达标)", ctrl.status)
	}
	if code := nodeErrorCode(t, d, ctrl.id); code != "parallel_instance_failed" {
		t.Fatalf("controller code = %q, want parallel_instance_failed", code)
	}
	if ex := readExec(t, d, execID); ex.status != engine.WorkflowFailed {
		t.Fatalf("execution = %s, want FAILED", ex.status)
	}
	if st := instanceStateNow(t, d, execID, "fan", 3); st != engine.NodeExecuting {
		t.Fatalf("surviving instance = %s, want EXECUTING (自然跑完)", st)
	}
}

// parallelMinSuccessRetryDoc:parallelNoRetryBody 同形 + min_success 1 +
// body retry 3(收敛后迟到失败不重试的审计 LOW① 锚;预算 3 供重派观察)。
const parallelMinSuccessRetryDoc = `{
  "dsl": "1.0",
  "name": "parallel-wf",
  "inputs": [{"name": "items", "type": "array", "required": true}],
  "nodes": [
    {"id": "pre", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
    {"id": "fan", "type": "parallel", "parallel": {
      "foreach": "{{inputs.items}}",
      "min_success": 1,
      "body": {"type": "llm", "retry": {"max_attempts": 3},
        "prompt": {"template": "评审",
          "user_data": [{"name": "name", "ref": "{{item.name}}"},
                        {"name": "ctx", "ref": "{{nodes.pre.output.text}}"}]},
        "outputs": {"verdict": "string"}}}},
    {"id": "post", "type": "tool", "tool": "http",
     "inputs": {"payload": "{{nodes.fan.output.results}}"}, "outputs": {"done": "boolean"}}
  ],
  "edges": [{"from": "pre", "to": "fan"}, {"from": "fan", "to": "post"}],
  "outputs": {"all": "{{nodes.fan.output.results}}"}
}`

// TestParallelMinSuccessConvergedSiblingNoRetry(T19.1 审计 LOW①):控制器已
// 收敛后的实例迟到失败(预算未耗尽)不重派——results 已单次物化,重试 =
// 空烧供应商预算(legacy fail-fast 的 termGuard「迟到失败不再重试」在
// RUNNING 执行内的对应物):行直接 FAILED/discarded_after_gather,零新 att 事件。
func TestParallelMinSuccessConvergedSiblingNoRetry(t *testing.T) {
	s, d := newSched(t)
	execID, instances := runToFanReady(t, s, d, parallelMinSuccessRetryDoc,
		`{"items": [{"name":"a"},{"name":"b"},{"name":"c"}]}`, "fan")
	// 实例 1 成功即收敛(min_success 1),控制器 SUCCEEDED。
	a := seedAttempt(t, d, execID, instances[0], 1)
	mustComplete(t, s, execID, instances[0], a, engine.AttemptSucceed, json.RawMessage(`{"verdict":"v0"}`))
	if ctrl := instanceRowOf(t, d, execID, "fan", 0); ctrl.status != engine.NodeSucceeded {
		t.Fatalf("controller = %s, want SUCCEEDED", ctrl.status)
	}
	// 实例 2 首败(预算 1/3 仍在):不重派,直接终败收口。
	a = seedAttempt(t, d, execID, instances[1], 1)
	mustComplete(t, s, execID, instances[1], a, engine.AttemptFailed, nil)
	r := instanceRowOf(t, d, execID, "fan", 2)
	if r.status != engine.NodeFailed {
		t.Fatalf("late-failed sibling = %s, want FAILED (不重派)", r.status)
	}
	if code := nodeErrorCode(t, d, r.id); code != "discarded_after_gather" {
		t.Fatalf("sibling code = %q, want discarded_after_gather", code)
	}
	if r.attempts != 1 {
		t.Fatalf("sibling attempts = %d, want 1 (零重派)", r.attempts)
	}
	redispatchTask := fmt.Sprintf("att:%d:2", r.id)
	for _, id := range taskEvents(t, d, execID) {
		if id == redispatchTask {
			t.Fatalf("redispatch event %s emitted, want none (结果已物化,重试空烧)", redispatchTask)
		}
	}
	// 收尾:实例 3 成功 + post 完成 → 执行 SUCCEEDED(流程可终局)。
	a = seedAttempt(t, d, execID, instances[2], 1)
	mustComplete(t, s, execID, instances[2], a, engine.AttemptSucceed, json.RawMessage(`{"verdict":"v2"}`))
	if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
		t.Fatalf("complete post: %v", err)
	}
	if ex := readExec(t, d, execID); ex.status != engine.WorkflowSucceeded {
		t.Fatalf("execution = %s, want SUCCEEDED", ex.status)
	}
}

// TestParallelMinSuccessReapAfterConvergeFinishes(T19.1 审计 MEDIUM①):部分
// 收敛后末位在飞实例经 ReapInstanceExhausted 收口时,提交后 finishIfDone
// 委托须立即终局——全行终态零 RUNNING 幽灵窗(原只能等扫描④b ExecStale)。
func TestParallelMinSuccessReapAfterConvergeFinishes(t *testing.T) {
	s, d := newSched(t)
	doc := fmt.Sprintf(parallelNoRetryBody, `"min_success": 1,`)
	execID, instances := runToFanReady(t, s, d, doc,
		`{"items": [{"name":"a"},{"name":"b"},{"name":"c"}]}`, "fan")
	// 实例 1 成功即收敛;post 完成;实例 2/3 仍在飞 → 执行 RUNNING。
	a := seedAttempt(t, d, execID, instances[0], 1)
	mustComplete(t, s, execID, instances[0], a, engine.AttemptSucceed, json.RawMessage(`{"verdict":"v0"}`))
	if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
		t.Fatalf("complete post: %v", err)
	}
	if ex := readExec(t, d, execID); ex.status != engine.WorkflowRunning {
		t.Fatalf("execution = %s, want RUNNING (在飞实例未收齐)", ex.status)
	}
	// 末位在飞实例逐一判死:最后一位收口后,本入口提交后判定须立即 SUCCEEDED。
	if err := s.ReapInstanceExhausted(context.Background(), execID, instances[1], "reaper-test"); err != nil {
		t.Fatalf("reap instance 2: %v", err)
	}
	if ex := readExec(t, d, execID); ex.status != engine.WorkflowRunning {
		t.Fatalf("execution = %s, want RUNNING (实例 3 仍在飞)", ex.status)
	}
	if err := s.ReapInstanceExhausted(context.Background(), execID, instances[2], "reaper-test"); err != nil {
		t.Fatalf("reap instance 3: %v", err)
	}
	if ex := readExec(t, d, execID); ex.status != engine.WorkflowSucceeded {
		t.Fatalf("execution = %s, want SUCCEEDED (全行终态即时收口,不等扫描④b)", ex.status)
	}
	if got, want := decodeSem(t, readExec(t, d, execID).outputs.String),
		decodeSem(t, `{"all":[{"verdict":"v0"},null,null]}`); !reflect.DeepEqual(got, want) {
		t.Fatalf("workflow outputs = %v, want %v (判死实例位 null 保位)", got, want)
	}
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
