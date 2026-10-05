package scheduler

// T19.2(D-2026-10-05-t192-foreach-dynamic-fanout)动态扇出调度器面测试:
// foreach 指向祖先节点的 array 输出 ⇒ Instantiate 仅落控制行,实例行在派发位
// 展开(dispatch-time);回滚后重派 = reconcile(缺行补插 / 盈余 PENDING→
// SKIPPED 保留证据行,gather 只收活跃集)。不变式锚(D- 三面):
//   - finishIfDone:控制器非终态期间实例行以非终态原子可见(不提前/不漏判);
//   - lockAllRows:实例号连续追加、永不删行(连续性检查零红);
//   - uk_node_exec:每执行每节点每实例恰一行(追加编号 = 现有行数+1 起)。
//
// 变异自查(实现错 → 哪条红):
//   - Instantiate 仍预落实例行 → instantiate_controller_only 红;
//   - 派发位不展开 → dispatch_gather 红(零实例行);
//   - 盈余行留 PENDING 不剪 → rollback_shrink 红(阻塞 finishIfDone 或 gather
//     误计失败);
//   - gather 不过滤 SKIPPED → rollback_shrink 红(控制器被误终败);
//   - 回滚后行数不 reconcile → rollback_grow 红(新行缺位,drift 响亮报错)。

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

// parallelDynamicDoc:pre(llm,outputs list:array + text:string)→ fan
// (foreach = {{nodes.pre.output.list}},body llm 无 retry,user_data {{item}} +
// 祖先引用)→ post(tool,内嵌引用 results)。"%%s" 注入 on_failure/min_success
// 变体字段;pre 完成输出控制扇出集。
const parallelDynamicDoc = `{
  "dsl": "1.0",
  "name": "parallel-dynamic",
  "inputs": [{"name": "topic", "type": "string", "required": true}],
  "nodes": [
    {"id": "pre", "type": "llm", "prompt": {"template": "t"}, "outputs": {"list": "array", "text": "string"}},
    {"id": "fan", "type": "parallel", "parallel": {
      "foreach": "{{nodes.pre.output.list}}",
      %s
      "body": {"type": "llm",
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

// dynDoc 返回无变体注入的基础动态扇出文档。
func dynDoc() string { return fmt.Sprintf(parallelDynamicDoc, "") }

// preOut 构造 pre 的完成输出(list = 扇出集)。
func preOut(list string) string { return `{"list": ` + list + `, "text": "ctx-out"}` }

// runDynToFanReady:Instantiate → Start → 完成 pre(自定义输出,扇出集真相源)。
// 返回 fan 实例行 id 集(输入序);动态扇出下实例行此刻才存在。
func runDynToFanReady(t testing.TB, s *Scheduler, d *sql.DB, doc, input, preOutput string) (execID uint64, instances []uint64) {
	t.Helper()
	_, execID = inst(t, s, d, doc, input)
	if err := s.Start(context.Background(), execID, "w1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := completeNode(t, s, d, execID, "pre", engine.AttemptSucceed, json.RawMessage(preOutput)); err != nil {
		t.Fatalf("complete pre: %v", err)
	}
	for _, r := range readAllInstanceRowsOf(t, d, execID, "fan") {
		if r.instanceNo > 0 {
			instances = append(instances, r.id)
		}
	}
	return execID, instances
}

// TestDynamicFanoutInstantiate:T19.2 Instantiate 分流——node 引用来源仅落
// 控制行(实例行派发位展开),零实例行即合法 Instantiate。
func TestDynamicFanoutInstantiate(t *testing.T) {
	s, d := newSched(t)
	execID, err := s.Instantiate(context.Background(), InstantiateParams{
		DefinitionID: seedDef(t, d, dynDoc()),
		Input:        json.RawMessage(`{"topic": "x"}`), TriggeredBy: "test",
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	t.Cleanup(func() { cleanupExec(t, d, execID) })
	rows := readAllInstanceRowsOf(t, d, execID, "fan")
	want := []struct {
		id         uint64
		instanceNo uint16
		status     engine.NodeState
	}{{id: rows[0].id, instanceNo: 0, status: engine.NodePending}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("fan rows = %+v, want controller-only PENDING (动态来源 Instantiate 不预落实例行)", rows)
	}
}

// TestDynamicFanoutDispatchGather:派发位展开 + 输入序汇聚全旅程。
func TestDynamicFanoutDispatchGather(t *testing.T) {
	s, d := newSched(t)

	t.Run("fanout_snapshots_and_att_events", func(t *testing.T) {
		execID, instances := runDynToFanReady(t, s, d, dynDoc(), `{"topic": "x"}`, preOut(`["a","b","c"]`))
		if len(instances) != 3 {
			t.Fatalf("instances = %d, want 3(派发位展开)", len(instances))
		}
		ctrl := instanceRowOf(t, d, execID, "fan", 0)
		if ctrl.status != engine.NodeExecuting || ctrl.deadlineOK {
			t.Fatalf("controller = %s deadline=%v, want EXECUTING/NULL", ctrl.status, ctrl.deadlineOK)
		}
		// 控制行快照 = 展开事实复现面:{foreach, count=live M}。
		var snap struct {
			Foreach string `json:"foreach"`
			Count   int    `json:"count"`
		}
		if err := json.Unmarshal([]byte(ctrl.snapshot.String), &snap); err != nil {
			t.Fatalf("controller snapshot decode: %v", err)
		}
		if snap.Foreach != "{{nodes.pre.output.list}}" || snap.Count != 3 {
			t.Fatalf("controller snapshot = %+v, want foreach 原文 + count 3", snap)
		}
		for i := 1; i <= 3; i++ {
			r := instanceRowOf(t, d, execID, "fan", uint16(i))
			if r.status != engine.NodeExecuting || !r.deadlineOK || !r.snapshot.Valid {
				t.Fatalf("instance %d = %s deadline=%v snap=%v, want EXECUTING/valid/valid", i, r.status, r.deadlineOK, r.snapshot.Valid)
			}
			var ls LLMInput
			if err := json.Unmarshal([]byte(r.snapshot.String), &ls); err != nil {
				t.Fatalf("instance %d snapshot decode: %v", i, err)
			}
			want := fmt.Sprintf("%c", 'a'+i-1)
			if len(ls.UserData) != 2 || ls.UserData[0].Value != want || ls.UserData[1].Value != "ctx-out" {
				t.Fatalf("instance %d user_data = %+v, want item=%q ctx=ctx-out", i, ls.UserData, want)
			}
		}
		// att 事件:pre 1 + 实例 3。
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
		execID, instances := runDynToFanReady(t, s, d, dynDoc(), `{"topic": "x"}`, preOut(`["a","b","c"]`))
		for _, idx := range []int{2, 0, 1} { // 乱序完成,results 仍按输入序
			attemptID := seedAttempt(t, d, execID, instances[idx], 1)
			mustComplete(t, s, execID, instances[idx], attemptID, engine.AttemptSucceed,
				json.RawMessage(fmt.Sprintf(`{"verdict":"v%d"}`, idx)))
		}
		ctrl := instanceRowOf(t, d, execID, "fan", 0)
		if ctrl.status != engine.NodeSucceeded {
			t.Fatalf("controller = %s, want SUCCEEDED", ctrl.status)
		}
		want := map[string]any{"results": []any{
			map[string]any{"verdict": "v0"},
			map[string]any{"verdict": "v1"},
			map[string]any{"verdict": "v2"},
		}}
		if got := decodeSem(t, ctrl.output.String); !reflect.DeepEqual(got, want) {
			t.Fatalf("controller output = %v, want %v", got, want)
		}
		if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
			t.Fatalf("complete post: %v", err)
		}
		ex := readExec(t, d, execID)
		if ex.status != engine.WorkflowSucceeded {
			t.Fatalf("execution = %s, want SUCCEEDED", ex.status)
		}
		if got := decodeSem(t, ex.outputs.String).(map[string]any)["all"]; !reflect.DeepEqual(got, want["results"]) {
			t.Fatalf("workflow outputs.all = %v, want %v", got, want["results"])
		}
	})

	t.Run("empty_array_immediate_converge", func(t *testing.T) {
		execID, instances := runDynToFanReady(t, s, d, dynDoc(), `{"topic": "x"}`, preOut(`[]`))
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
			t.Fatalf("post = %s, want EXECUTING", post.status)
		}
	})
}

// TestDynamicFanoutDispatchRejected:派发位拒绝族(执行级失败,零派发残留
// 契约——控制器/实例行零 claim、零 att 事件增量):
//   - 非 array 输出(上游违约)→ input_eval;
//   - min_success > 动态 N(D-t191 先验移派发位)→ parallel_min_success_limit;
//   - 超 MaxParallelInstances → parallel_fanout_limit。
func TestDynamicFanoutDispatchRejected(t *testing.T) {
	s, d := newSched(t)
	overLimit := `[` + strings.Repeat(`"x",`, 1000) + `"y"]`
	tests := []struct {
		name      string
		extra     string
		preOutput string
		wantErr   string
	}{
		{"non_array_output", "", preOut(`"nope"`), "must resolve to a JSON array"},
		{"min_success_over_dynamic_n", `"on_failure": "wait_all", "min_success": 4,`, preOut(`["a","b","c"]`), "parallel_min_success_limit"},
		{"min_success_empty_dynamic", `"min_success": 1,`, preOut(`[]`), "parallel_min_success_limit"},
		{"over_limit", "", preOut(overLimit), "parallel_fanout_limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := fmt.Sprintf(parallelDynamicDoc, tt.extra)
			execID, err := s.Instantiate(context.Background(), InstantiateParams{
				DefinitionID: seedDef(t, d, doc),
				Input:        json.RawMessage(`{"topic": "x"}`), TriggeredBy: "test",
			})
			if err != nil {
				t.Fatalf("Instantiate: %v", err)
			}
			t.Cleanup(func() { cleanupExec(t, d, execID) })
			if err := s.Start(context.Background(), execID, "w1"); err != nil {
				t.Fatalf("Start: %v", err)
			}
			// complete pre 触发 fan 派发失败:failExecution 同事务收口,Complete
			// 本身 nil 返回。
			if err := completeNode(t, s, d, execID, "pre", engine.AttemptSucceed, json.RawMessage(tt.preOutput)); err != nil {
				t.Fatalf("complete pre: %v", err)
			}
			ex := readExec(t, d, execID)
			if ex.status != engine.WorkflowFailed {
				t.Fatalf("execution = %s(%s), want FAILED", ex.status, ex.errorCode.String)
			}
			if ex.errorCode.String != "input_eval" {
				t.Fatalf("error code = %q, want input_eval", ex.errorCode.String)
			}
			// 零残留:控制器 PENDING、零实例行、fan 零 att 事件。
			rows := readAllInstanceRowsOf(t, d, execID, "fan")
			if len(rows) != 1 || rows[0].status != engine.NodePending {
				t.Fatalf("fan rows = %+v, want controller-only PENDING (零派发残留)", rows)
			}
			if msg := execErrMsg(t, d, execID); !strings.Contains(msg, tt.wantErr) {
				t.Fatalf("execution error %q does not contain %q", msg, tt.wantErr)
			}
		})
	}
}

// execErrMsg 读执行的 error_message(拒绝语义词锚)。
func execErrMsg(t testing.TB, d *sql.DB, execID uint64) string {
	t.Helper()
	var msg sql.NullString
	if err := d.QueryRow(`SELECT error_message FROM workflow_execution WHERE id=?`, execID).Scan(&msg); err != nil {
		t.Fatalf("read execution error_message: %v", err)
	}
	return msg.String
}

// TestDynamicFanoutRollbackReconcile:回滚后重派 reconcile——盈余行剪枝保留
// (shrink)与缺行补插(grow)双方向;gather 只收活跃集(SKIPPED 不计失败、
// 不进 results)。T19.2 D- 决策 3/4 的行为锚。
func TestDynamicFanoutRollbackReconcile(t *testing.T) {
	s, d := newSched(t)

	rollback := func(t *testing.T, execID uint64) {
		t.Helper()
		if _, err := s.RollbackTo(context.Background(), RollbackParams{
			ExecID: execID, TargetNodeID: "pre", WorkerID: "api:rb", ActorID: "alice",
		}); err != nil {
			t.Fatalf("RollbackTo: %v", err)
		}
	}
	statusOf := func(t *testing.T, rows []struct {
		id         uint64
		instanceNo uint16
		status     engine.NodeState
	}, instanceNo uint16) engine.NodeState {
		t.Helper()
		for _, r := range rows {
			if r.instanceNo == instanceNo {
				return r.status
			}
		}
		t.Fatalf("instance %d row missing", instanceNo)
		return ""
	}

	t.Run("shrink_surplus_rows_skipped", func(t *testing.T) {
		// 轮 1:list 3 元素,实例 1/2 成功、3 终败(fail_fast)→ 控制器 FAILED。
		execID, instances := runDynToFanReady(t, s, d, dynDoc(), `{"topic": "x"}`, preOut(`["a","b","c"]`))
		if len(instances) != 3 {
			t.Fatalf("round 1 instances = %d, want 3", len(instances))
		}
		for idx := 0; idx < 2; idx++ {
			attemptID := seedAttempt(t, d, execID, instances[idx], 1)
			mustComplete(t, s, execID, instances[idx], attemptID, engine.AttemptSucceed,
				json.RawMessage(fmt.Sprintf(`{"verdict":"v%d"}`, idx)))
		}
		failAtt := seedAttempt(t, d, execID, instances[2], 1)
		mustComplete(t, s, execID, instances[2], failAtt, engine.AttemptFailed, nil)
		if ex := readExec(t, d, execID); ex.status != engine.WorkflowFailed {
			t.Fatalf("round 1 execution = %s, want FAILED", ex.status)
		}
		// 回滚 pre:fan 全行集(控制行 + 3 实例行)重置 PENDING,pre Rerun EXECUTING。
		rollback(t, execID)
		rows := readAllInstanceRowsOf(t, d, execID, "fan")
		for _, want := range []uint16{0, 1, 2, 3} {
			if st := statusOf(t, rows, want); st != engine.NodePending {
				t.Fatalf("after rollback instance %d = %s, want PENDING", want, st)
			}
		}
		// 轮 2:pre 产出 2 元素 → 实例 1/2 重用 claim,3 = 盈余 SKIPPED。
		if err := completeNode(t, s, d, execID, "pre", engine.AttemptSucceed, json.RawMessage(preOut(`["x","y"]`))); err != nil {
			t.Fatalf("complete pre round 2: %v", err)
		}
		rows = readAllInstanceRowsOf(t, d, execID, "fan")
		if st := statusOf(t, rows, 0); st != engine.NodeExecuting {
			t.Fatalf("controller = %s, want EXECUTING", st)
		}
		for _, i := range []uint16{1, 2} {
			if st := statusOf(t, rows, i); st != engine.NodeExecuting {
				t.Fatalf("live instance %d = %s, want EXECUTING(重用)", i, st)
			}
		}
		if st := statusOf(t, rows, 3); st != engine.NodeSkipped {
			t.Fatalf("surplus instance 3 = %s, want SKIPPED(盈余剪枝保留)", st)
		}
		// 收敛:活跃集 = 2,results 长度 2(输入序),盈余行不进 results。
		for idx := 0; idx < 2; idx++ {
			attemptID := seedAttempt(t, d, execID, rows[idx+1].id, 2)
			mustComplete(t, s, execID, rows[idx+1].id, attemptID, engine.AttemptSucceed,
				json.RawMessage(fmt.Sprintf(`{"verdict":"w%d"}`, idx)))
		}
		ctrl := instanceRowOf(t, d, execID, "fan", 0)
		if ctrl.status != engine.NodeSucceeded {
			t.Fatalf("controller = %s, want SUCCEEDED", ctrl.status)
		}
		want := map[string]any{"results": []any{
			map[string]any{"verdict": "w0"},
			map[string]any{"verdict": "w1"},
		}}
		if got := decodeSem(t, ctrl.output.String); !reflect.DeepEqual(got, want) {
			t.Fatalf("controller output = %v, want %v(盈余位不进 results)", got, want)
		}
		if st := statusOf(t, readAllInstanceRowsOf(t, d, execID, "fan"), 3); st != engine.NodeSkipped {
			t.Fatalf("surplus instance 3 = %s, want SKIPPED(终态保持)", st)
		}
		if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
			t.Fatalf("complete post: %v", err)
		}
		if ex := readExec(t, d, execID); ex.status != engine.WorkflowSucceeded {
			t.Fatalf("execution = %s, want SUCCEEDED(SKIPPED 盈余行不阻塞 finishIfDone)", ex.status)
		}
	})

	t.Run("grow_missing_rows_inserted", func(t *testing.T) {
		// 轮 1:list 2 元素,实例 2 终败 → FAILED。
		execID, instances := runDynToFanReady(t, s, d, dynDoc(), `{"topic": "x"}`, preOut(`["a","b"]`))
		if len(instances) != 2 {
			t.Fatalf("round 1 instances = %d, want 2", len(instances))
		}
		att := seedAttempt(t, d, execID, instances[0], 1)
		mustComplete(t, s, execID, instances[0], att, engine.AttemptSucceed, json.RawMessage(`{"verdict":"v0"}`))
		failAtt := seedAttempt(t, d, execID, instances[1], 1)
		mustComplete(t, s, execID, instances[1], failAtt, engine.AttemptFailed, nil)
		if ex := readExec(t, d, execID); ex.status != engine.WorkflowFailed {
			t.Fatalf("round 1 execution = %s, want FAILED", ex.status)
		}
		rollback(t, execID)
		// 轮 2:list 4 元素 → 实例 1/2 重用 + 3/4 补插(uk 连续追加)。
		if err := completeNode(t, s, d, execID, "pre", engine.AttemptSucceed, json.RawMessage(preOut(`["p","q","r","s"]`))); err != nil {
			t.Fatalf("complete pre round 2: %v", err)
		}
		rows := readAllInstanceRowsOf(t, d, execID, "fan")
		if len(rows) != 5 { // 控制行 + 4
			t.Fatalf("fan rows = %d, want 5(controller + 4 instances)", len(rows))
		}
		for i := uint16(1); i <= 4; i++ {
			if st := statusOf(t, rows, i); st != engine.NodeExecuting {
				t.Fatalf("instance %d = %s, want EXECUTING(重用/补插)", i, st)
			}
		}
		// 快照按新轮元素绑定(1..4 = p/q/r/s)。
		for i := uint16(1); i <= 4; i++ {
			r := instanceRowOf(t, d, execID, "fan", i)
			var ls LLMInput
			if err := json.Unmarshal([]byte(r.snapshot.String), &ls); err != nil {
				t.Fatalf("instance %d snapshot decode: %v", i, err)
			}
			want := fmt.Sprintf("%c", 'p'+i-1)
			if len(ls.UserData) != 2 || ls.UserData[0].Value != want {
				t.Fatalf("instance %d user_data = %+v, want item=%q", i, ls.UserData, want)
			}
		}
		for _, r := range rows {
			if r.instanceNo == 0 {
				continue
			}
			att := seedAttempt(t, d, execID, r.id, 2)
			mustComplete(t, s, execID, r.id, att, engine.AttemptSucceed,
				json.RawMessage(fmt.Sprintf(`{"verdict":"g%d"}`, r.instanceNo-1)))
		}
		ctrl := instanceRowOf(t, d, execID, "fan", 0)
		if ctrl.status != engine.NodeSucceeded {
			t.Fatalf("controller = %s, want SUCCEEDED", ctrl.status)
		}
		want := map[string]any{"results": []any{
			map[string]any{"verdict": "g0"},
			map[string]any{"verdict": "g1"},
			map[string]any{"verdict": "g2"},
			map[string]any{"verdict": "g3"},
		}}
		if got := decodeSem(t, ctrl.output.String); !reflect.DeepEqual(got, want) {
			t.Fatalf("controller output = %v, want %v", got, want)
		}
		if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
			t.Fatalf("complete post: %v", err)
		}
		if ex := readExec(t, d, execID); ex.status != engine.WorkflowSucceeded {
			t.Fatalf("execution = %s, want SUCCEEDED", ex.status)
		}
	})

	// shrink_then_rollback_grow_back_reuse(对抗审计 LOW 修):D- 决策 4 的
	// 承载性声明——「回滚 Reset 把盈余行回 PENDING 后由下轮 reconcile 重裁」
	// 无锚则 reconcileFanout 的 surplus-want-PENDING drift 检查前提无测试信号。
	// 轮 1 败(3 实例)→ 回滚 → 轮 2 缩(2 元素,行 3 SKIPPED)且再败 → 回滚
	//(SKIPPED→PENDING)→ 轮 3 涨回 3:行 3 复用为活跃,results 长度 3。
	t.Run("shrink_then_rollback_grow_back_reuse", func(t *testing.T) {
		execID, instances := runDynToFanReady(t, s, d, dynDoc(), `{"topic": "x"}`, preOut(`["a","b","c"]`))
		if len(instances) != 3 {
			t.Fatalf("round 1 instances = %d, want 3", len(instances))
		}
		// 轮 1:1/2 成功、3 终败 → 执行 FAILED。
		for idx := 0; idx < 2; idx++ {
			att := seedAttempt(t, d, execID, instances[idx], 1)
			mustComplete(t, s, execID, instances[idx], att, engine.AttemptSucceed,
				json.RawMessage(fmt.Sprintf(`{"verdict":"v%d"}`, idx)))
		}
		failAtt := seedAttempt(t, d, execID, instances[2], 1)
		mustComplete(t, s, execID, instances[2], failAtt, engine.AttemptFailed, nil)
		if ex := readExec(t, d, execID); ex.status != engine.WorkflowFailed {
			t.Fatalf("round 1 execution = %s, want FAILED", ex.status)
		}
		rollback(t, execID)
		// 轮 2 缩:2 元素 → 行 3 SKIPPED;实例 1 终败 → 执行 FAILED(可再回滚)。
		if err := completeNode(t, s, d, execID, "pre", engine.AttemptSucceed, json.RawMessage(preOut(`["x","y"]`))); err != nil {
			t.Fatalf("complete pre round 2: %v", err)
		}
		rows := readAllInstanceRowsOf(t, d, execID, "fan")
		if st := statusOf(t, rows, 3); st != engine.NodeSkipped {
			t.Fatalf("surplus instance 3 = %s, want SKIPPED", st)
		}
		// 实例 2 先成功再败实例 1:全行终态才可再回滚(守卫②拒 EXECUTING 残留)。
		okAtt := seedAttempt(t, d, execID, rows[2].id, 2)
		mustComplete(t, s, execID, rows[2].id, okAtt, engine.AttemptSucceed, json.RawMessage(`{"verdict":"w1"}`))
		failAtt2 := seedAttempt(t, d, execID, rows[1].id, 2)
		mustComplete(t, s, execID, rows[1].id, failAtt2, engine.AttemptFailed, nil)
		if ex := readExec(t, d, execID); ex.status != engine.WorkflowFailed {
			t.Fatalf("round 2 execution = %s, want FAILED", ex.status)
		}
		// 回滚:盈余 SKIPPED 行被闭包重置回 PENDING(Reset 谓词吃全部终态)。
		rollback(t, execID)
		rows = readAllInstanceRowsOf(t, d, execID, "fan")
		for _, want := range []uint16{0, 1, 2, 3} {
			if st := statusOf(t, rows, want); st != engine.NodePending {
				t.Fatalf("after rollback instance %d = %s, want PENDING (SKIPPED 盈余行同被重置)", want, st)
			}
		}
		// 轮 3 涨回 3:行 3 复用为活跃(非补插——instance_no 连续无新行)。
		if err := completeNode(t, s, d, execID, "pre", engine.AttemptSucceed, json.RawMessage(preOut(`["p","q","r"]`))); err != nil {
			t.Fatalf("complete pre round 3: %v", err)
		}
		rows = readAllInstanceRowsOf(t, d, execID, "fan")
		if len(rows) != 4 { // 控制行 + 3,零新行 = 行 3 复用
			t.Fatalf("fan rows = %d, want 4 (surplus reclaimed, no new insert)", len(rows))
		}
		for i := uint16(1); i <= 3; i++ {
			if st := statusOf(t, rows, i); st != engine.NodeExecuting {
				t.Fatalf("instance %d = %s, want EXECUTING (SKIPPED 行复用)", i, st)
			}
		}
		for _, r := range rows {
			if r.instanceNo == 0 {
				continue
			}
			att := seedAttempt(t, d, execID, r.id, 3)
			mustComplete(t, s, execID, r.id, att, engine.AttemptSucceed,
				json.RawMessage(fmt.Sprintf(`{"verdict":"u%d"}`, r.instanceNo-1)))
		}
		ctrl := instanceRowOf(t, d, execID, "fan", 0)
		if ctrl.status != engine.NodeSucceeded {
			t.Fatalf("controller = %s, want SUCCEEDED", ctrl.status)
		}
		want := map[string]any{"results": []any{
			map[string]any{"verdict": "u0"},
			map[string]any{"verdict": "u1"},
			map[string]any{"verdict": "u2"},
		}}
		if got := decodeSem(t, ctrl.output.String); !reflect.DeepEqual(got, want) {
			t.Fatalf("controller output = %v, want %v", got, want)
		}
		if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
			t.Fatalf("complete post: %v", err)
		}
		if ex := readExec(t, d, execID); ex.status != engine.WorkflowSucceeded {
			t.Fatalf("execution = %s, want SUCCEEDED", ex.status)
		}
	})
}

// TestDynamicFanoutShrinkToEmpty(对抗审计 HIGH① 修,活体变异实证):唯一暴露
// 「reconcile 剪枝与控制器收敛同事务共览内存行集」的入口旅程——删掉
// reconcileFanout 的 surplus 内存回写(surplus.status = NodeSkipped)后全套件
// 绿(变异逃逸),本旅程红:盈余行内存驻留 PENDING 混进 results 成 null 垃圾位,
// 违反 D- 决策 4「results 长度恒 = 当前轮 M」。其余子测的收敛均发生在独立
// Complete 事务(每事务 lockAllRows 重读 DB,内存陈旧不可见),锚不住该序。
func TestDynamicFanoutShrinkToEmpty(t *testing.T) {
	s, d := newSched(t)
	execID, instances := runDynToFanReady(t, s, d, dynDoc(), `{"topic": "x"}`, preOut(`["a","b","c"]`))
	if len(instances) != 3 {
		t.Fatalf("round 1 instances = %d, want 3", len(instances))
	}
	for idx := 0; idx < 2; idx++ {
		att := seedAttempt(t, d, execID, instances[idx], 1)
		mustComplete(t, s, execID, instances[idx], att, engine.AttemptSucceed,
			json.RawMessage(fmt.Sprintf(`{"verdict":"v%d"}`, idx)))
	}
	failAtt := seedAttempt(t, d, execID, instances[2], 1)
	mustComplete(t, s, execID, instances[2], failAtt, engine.AttemptFailed, nil)
	if ex := readExec(t, d, execID); ex.status != engine.WorkflowFailed {
		t.Fatalf("round 1 execution = %s, want FAILED", ex.status)
	}
	if _, err := s.RollbackTo(context.Background(), RollbackParams{
		ExecID: execID, TargetNodeID: "pre", WorkerID: "api:rb", ActorID: "alice",
	}); err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	// 轮 2 空数组:reconcile 剪 3 盈余行 + 控制器收敛同事务——results 必恰 []
	//(盈余位不进 results,D- 决策 4;内存回写缺位时此处红)。
	if err := completeNode(t, s, d, execID, "pre", engine.AttemptSucceed, json.RawMessage(preOut(`[]`))); err != nil {
		t.Fatalf("complete pre round 2: %v", err)
	}
	ctrl := instanceRowOf(t, d, execID, "fan", 0)
	if ctrl.status != engine.NodeSucceeded {
		t.Fatalf("controller = %s, want SUCCEEDED (空集立即收敛)", ctrl.status)
	}
	if got := decodeSem(t, ctrl.output.String); !reflect.DeepEqual(got, map[string]any{"results": []any{}}) {
		t.Fatalf("controller output = %v, want exactly {results:[]}(盈余位不进 results)", got)
	}
	for i := uint16(1); i <= 3; i++ {
		if st := instanceStateNow(t, d, execID, "fan", i); st != engine.NodeSkipped {
			t.Fatalf("surplus instance %d = %s, want SKIPPED", i, st)
		}
	}
	if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
		t.Fatalf("complete post: %v", err)
	}
	if ex := readExec(t, d, execID); ex.status != engine.WorkflowSucceeded {
		t.Fatalf("execution = %s, want SUCCEEDED (SKIPPED 盈余行不阻塞 finishIfDone)", ex.status)
	}
}

// TestDynamicFanoutMinSuccess(对抗审计 LOW 修):min_success × 动态扇出的
// 「启用且 k≤M 收敛」组合端到端锚——此前仅 k>M 拒绝方向有锚,「SKIPPED 滤除
// 被误解为仅属未启用分支」类回归零信号(静态锚碰巧共享代码路径才间接覆盖)。
func TestDynamicFanoutMinSuccess(t *testing.T) {
	s, d := newSched(t)

	t.Run("wait_all_k1_partial_converge", func(t *testing.T) {
		doc := fmt.Sprintf(parallelDynamicDoc, `"on_failure": "wait_all", "min_success": 1,`)
		execID, instances := runDynToFanReady(t, s, d, doc, `{"topic": "x"}`, preOut(`["a","b"]`))
		if len(instances) != 2 {
			t.Fatalf("instances = %d, want 2", len(instances))
		}
		failAtt := seedAttempt(t, d, execID, instances[0], 1)
		mustComplete(t, s, execID, instances[0], failAtt, engine.AttemptFailed, nil)
		if st := instanceStateNow(t, d, execID, "fan", 0); st != engine.NodeExecuting {
			t.Fatalf("controller = %s after one failure with one in flight, want EXECUTING (wait_all 未收齐)", st)
		}
		okAtt := seedAttempt(t, d, execID, instances[1], 1)
		mustComplete(t, s, execID, instances[1], okAtt, engine.AttemptSucceed, json.RawMessage(`{"verdict":"v1"}`))
		ctrl := instanceRowOf(t, d, execID, "fan", 0)
		if ctrl.status != engine.NodeSucceeded {
			t.Fatalf("controller = %s, want SUCCEEDED (k=1 达标收敛)", ctrl.status)
		}
		wantResults := []any{nil, map[string]any{"verdict": "v1"}}
		got := decodeSem(t, ctrl.output.String).(map[string]any)
		if !reflect.DeepEqual(got["results"], wantResults) {
			t.Fatalf("results = %v, want [null, v1](失败位 null 保位)", got["results"])
		}
		if fc, ok := got["failed_count"].(json.Number); !ok || fc.String() != "1" {
			t.Fatalf("failed_count = %v, want 1", got["failed_count"])
		}
		if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
			t.Fatalf("complete post: %v", err)
		}
		if ex := readExec(t, d, execID); ex.status != engine.WorkflowSucceeded {
			t.Fatalf("execution = %s, want SUCCEEDED", ex.status)
		}
	})

	t.Run("k_equals_m_exact_accept", func(t *testing.T) {
		doc := fmt.Sprintf(parallelDynamicDoc, `"on_failure": "fail_fast", "min_success": 2,`)
		execID, instances := runDynToFanReady(t, s, d, doc, `{"topic": "x"}`, preOut(`["a","b"]`))
		for idx := 0; idx < 2; idx++ {
			att := seedAttempt(t, d, execID, instances[idx], 1)
			mustComplete(t, s, execID, instances[idx], att, engine.AttemptSucceed,
				json.RawMessage(fmt.Sprintf(`{"verdict":"v%d"}`, idx)))
		}
		ctrl := instanceRowOf(t, d, execID, "fan", 0)
		if ctrl.status != engine.NodeSucceeded {
			t.Fatalf("controller = %s, want SUCCEEDED (k==M 全成恰接受)", ctrl.status)
		}
		got := decodeSem(t, ctrl.output.String).(map[string]any)
		if fc, ok := got["failed_count"].(json.Number); !ok || fc.String() != "0" {
			t.Fatalf("failed_count = %v, want 0", got["failed_count"])
		}
		if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
			t.Fatalf("complete post: %v", err)
		}
		if ex := readExec(t, d, execID); ex.status != engine.WorkflowSucceeded {
			t.Fatalf("execution = %s, want SUCCEEDED", ex.status)
		}
	})
}

// TestDynamicFanoutNested:fan1(静态 inputs)→ fan2(foreach =
// {{nodes.fan1.output.results}})——嵌套动态扇出,消费方派发晚于产方控制器
// 收敛,results 已物化。D- 决策 6 行为锚。
func TestDynamicFanoutNested(t *testing.T) {
	s, d := newSched(t)
	doc := `{
  "dsl": "1.0",
  "name": "parallel-nested-dynamic",
  "inputs": [{"name": "rows", "type": "array", "required": true}],
  "nodes": [
    {"id": "fan1", "type": "parallel", "parallel": {
      "foreach": "{{inputs.rows}}",
      "body": {"type": "llm",
        "prompt": {"template": "评审", "user_data": [{"name": "review", "ref": "{{item}}"}]},
        "outputs": {"verdict": "string"}}
    }},
    {"id": "fan2", "type": "parallel", "parallel": {
      "foreach": "{{nodes.fan1.output.results}}",
      "body": {"type": "llm",
        "prompt": {"template": "复核", "user_data": [{"name": "src", "ref": "{{item}}"}]},
        "outputs": {"note": "string"}}
    }},
    {"id": "post", "type": "tool", "tool": "http",
     "inputs": {"payload": "{{nodes.fan2.output.results}}"}, "outputs": {"done": "boolean"}}
  ],
  "edges": [{"from": "fan1", "to": "fan2"}, {"from": "fan2", "to": "post"}],
  "outputs": {"all": "{{nodes.fan2.output.results}}"}
}`
	execID, err := s.Instantiate(context.Background(), InstantiateParams{
		DefinitionID: seedDef(t, d, doc),
		Input:        json.RawMessage(`{"rows": ["r0", "r1"]}`), TriggeredBy: "test",
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	t.Cleanup(func() { cleanupExec(t, d, execID) })
	if err := s.Start(context.Background(), execID, "w1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// fan1 静态展开 2 实例;完成 → fan1 控制器 results 物化 → fan2 动态展开。
	f1 := readAllInstanceRowsOf(t, d, execID, "fan1")
	if len(f1) != 3 { // 控制行 + 2
		t.Fatalf("fan1 rows = %d, want 3", len(f1))
	}
	for _, r := range f1 {
		if r.instanceNo == 0 {
			continue
		}
		att := seedAttempt(t, d, execID, r.id, 1)
		mustComplete(t, s, execID, r.id, att, engine.AttemptSucceed,
			json.RawMessage(fmt.Sprintf(`{"verdict":"v%d"}`, r.instanceNo-1)))
	}
	f2 := readAllInstanceRowsOf(t, d, execID, "fan2")
	if len(f2) != 3 { // 控制行 + 2(动态 = fan1 results 长度)
		t.Fatalf("fan2 rows = %d, want 3(动态展开 = fan1 results 长度)", len(f2))
	}
	// fan2 实例快照绑定 item = fan1 results 元素(对象)。
	for i := uint16(1); i <= 2; i++ {
		r := instanceRowOf(t, d, execID, "fan2", i)
		var ls LLMInput
		if err := json.Unmarshal([]byte(r.snapshot.String), &ls); err != nil {
			t.Fatalf("fan2 instance %d snapshot decode: %v", i, err)
		}
		want := map[string]any{"verdict": fmt.Sprintf("v%d", i-1)}
		if len(ls.UserData) != 1 || !reflect.DeepEqual(ls.UserData[0].Value, want) {
			t.Fatalf("fan2 instance %d user_data = %+v, want item=%v", i, ls.UserData, want)
		}
	}
	for _, r := range f2 {
		if r.instanceNo == 0 {
			continue
		}
		att := seedAttempt(t, d, execID, r.id, 1)
		mustComplete(t, s, execID, r.id, att, engine.AttemptSucceed,
			json.RawMessage(fmt.Sprintf(`{"note":"n%d"}`, r.instanceNo-1)))
	}
	if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
		t.Fatalf("complete post: %v", err)
	}
	ex := readExec(t, d, execID)
	if ex.status != engine.WorkflowSucceeded {
		t.Fatalf("execution = %s, want SUCCEEDED", ex.status)
	}
	want := []any{map[string]any{"note": "n0"}, map[string]any{"note": "n1"}}
	if got := decodeSem(t, ex.outputs.String).(map[string]any)["all"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("workflow outputs.all = %v, want %v", got, want)
	}
}
