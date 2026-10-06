package scheduler

// T19.5(D-2026-10-06-t195-parallel-incremental-recovery)增量恢复调度器面测试:
// 回滚闭包重置对 parallel 节点按「单实例粒度」保活——SUCCEEDED 实例行(及其
// output/finished_at/attempts)经三重门控(意图/快照依赖/行集原子性)后跳过
// ResetNodeToPending,重派只 claim 非 SUCCEEDED 行,gather 原样收割旧输出。
//
// 门控面(实现错 → 哪条红):
//   - 意图门缺失(SUCCEEDED 执行 + 显式 target=parallel 仍保活)→
//     refresh_intent_full_rerun 红(整行集重跑是再生语义,T19.3 不可塌缩);
//   - 依赖门缺失(数据依赖在闭包内仍保活,旧输出对不上新输入)→
//     dependency_gate_conservative 红;
//   - 行集原子性缺失(SUCCEEDED 控制行下重置实例 → 悬死 PENDING)或
//     整行集保活误重置 → controller_succeeded_whole_rowset 红;
//   - 派发 claim 过滤缺失(保活行被重 claim)→ keeps_succeeded_instances 红
//     (attempts/version 漂移);
//   - 派发尾 gatherCheck 缺失(全保活零重跑时控制器滞留 EXECUTING)→
//     all_kept_converges_at_dispatch 红;
//   - reconcileFanout 断言未放宽(动态扇出保活行误判 drift)→
//     dynamic_fanout_kept_rows 红;
//   - 取消楔源恢复不增量(cancel 整执行楔停后重烧全行集)→
//     cancel_wedge_incremental 红。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"braid/internal/engine"
)

// parallelNoDepBody:pre→fan 边存在但 body 零节点引用(仅 {{item.name}})——
// fan 的快照数据依赖集为空,是「上游重跑但非数据依赖 ⇒ 仍可保活」的精确门控
// 对照面(parallelNoRetryBody 的 body 引用 {{nodes.pre.output.text}},依赖集
// = {pre},同文档族两态对照)。
const parallelNoDepBody = `{
  "dsl": "1.0",
  "name": "parallel-wf",
  "inputs": [{"name": "items", "type": "array", "required": true}],
  "nodes": [
    {"id": "pre", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
    {"id": "fan", "type": "parallel", "parallel": {
      "foreach": "{{inputs.items}}",
      "body": {"type": "llm",
        "prompt": {"template": "评审", "user_data": [{"name": "name", "ref": "{{item.name}}"}]},
        "outputs": {"verdict": "string"}}
    }},
    {"id": "post", "type": "tool", "tool": "http",
     "inputs": {"payload": "{{nodes.fan.output.results}}"}, "outputs": {"done": "boolean"}}
  ],
  "edges": [{"from": "pre", "to": "fan"}, {"from": "fan", "to": "post"}],
  "outputs": {"all": "{{nodes.fan.output.results}}"}
}`

// recoverRow 是增量恢复断言用的实例/控制行快照(attempts_base 与 finished_at
// 是保活语义的关键列:保活 = base 不动 + finished 保留;重置 = base=attempts
// + finished 清列)。
type recoverRow struct {
	status       engine.NodeState
	version      uint32
	attempts     uint16
	attemptsBase uint16
	output       sql.NullString
	finished     sql.NullTime
}

func recoverRowOf(t testing.TB, d *sql.DB, execID uint64, nodeID string, instanceNo uint16) recoverRow {
	t.Helper()
	var r recoverRow
	err := d.QueryRow(`
SELECT status, version, attempts, attempts_base, output, finished_at
  FROM node_execution WHERE execution_id=? AND node_id=? AND instance_no=?`,
		execID, nodeID, instanceNo).
		Scan(&r.status, &r.version, &r.attempts, &r.attemptsBase, &r.output, &r.finished)
	if err != nil {
		t.Fatalf("read node %s[%d]: %v", nodeID, instanceNo, err)
	}
	return r
}

// assertFanResults 断言 fan 控制器已收敛且 results 与 want 语义相等
//(UseNumber 解码;合并新旧/全旧保活/全新重跑三态共用,评审 LOW 收敛)。
func assertFanResults(t *testing.T, d *sql.DB, execID uint64, want []any) {
	t.Helper()
	ctrl := instanceRowOf(t, d, execID, "fan", 0)
	if ctrl.status != engine.NodeSucceeded || !ctrl.output.Valid {
		t.Fatalf("fan controller = %s output=%v, want SUCCEEDED with results",
			ctrl.status, ctrl.output.Valid)
	}
	if got := decodeSem(t, ctrl.output.String); !reflect.DeepEqual(got,
		map[string]any{"results": want}) {
		t.Fatalf("fan controller output = %v, want results %v", got, want)
	}
}

// rollbackAuto 是无显式 target 的回滚快捷(自动定位恢复点)。
func rollbackAuto(t testing.TB, s *Scheduler, execID uint64) *RollbackResult {
	t.Helper()
	res, err := s.RollbackTo(context.Background(), RollbackParams{
		ExecID: execID, WorkerID: "api-rb", ActorID: "op"})
	if err != nil {
		t.Fatalf("RollbackTo(auto): %v", err)
	}
	return res
}

func rollbackTarget(t testing.TB, s *Scheduler, execID uint64, target string) *RollbackResult {
	t.Helper()
	res, err := s.RollbackTo(context.Background(), RollbackParams{
		ExecID: execID, TargetNodeID: target, WorkerID: "api-rb", ActorID: "op"})
	if err != nil {
		t.Fatalf("RollbackTo(target=%s): %v", target, err)
	}
	return res
}

// TestRecoverKeepsSucceededInstances:旗舰旅程——wait_all 部分失败 → 回滚 →
// SUCCEEDED 实例保活(version/attempts/finished/output 全不动),仅终败实例
// 重跑(预算满额:attempts_base=attempts),收敛 results 合并新旧输出。
func TestRecoverKeepsSucceededInstances(t *testing.T) {
	s, d := newSched(t)
	doc := fmt.Sprintf(parallelNoRetryBody, `"on_failure": "wait_all",`)
	input := `{"items": [{"name":"a"},{"name":"b"},{"name":"c"}]}`
	execID, instances := runToFanReady(t, s, d, doc, input, "fan")
	t.Cleanup(func() { cleanupExec(t, d, execID) })

	// instances[1](instance_no 2)终败、instances[0]/[2](instance_no 1/3)成功
	//(wait_all 收齐再判 → 控制器 FAILED、执行 FAILED)。
	at := seedAttempt(t, d, execID, instances[1], 1)
	mustComplete(t, s, execID, instances[1], at, engine.AttemptFailed, nil)
	for _, idx := range []int{0, 2} {
		a := seedAttempt(t, d, execID, instances[idx], 1)
		mustComplete(t, s, execID, instances[idx], a, engine.AttemptSucceed,
			json.RawMessage(fmt.Sprintf(`{"verdict":"v%d"}`, idx)))
	}
	if ctrl := instanceRowOf(t, d, execID, "fan", 0); ctrl.status != engine.NodeFailed {
		t.Fatalf("controller = %s, want FAILED (wait_all 收齐含败)", ctrl.status)
	}
	keptBefore := [3]recoverRow{
		recoverRowOf(t, d, execID, "fan", 1),
		recoverRowOf(t, d, execID, "fan", 2),
		recoverRowOf(t, d, execID, "fan", 3),
	}

	res := rollbackAuto(t, s, execID)
	// 重置面收敛:控制行 + 终败实例恰两行(保活行不入 ResetCount)。
	if res.ResetCount != 2 {
		t.Fatalf("reset_count = %d, want 2 (controller + failed instance; kept rows excluded)", res.ResetCount)
	}
	if ex := readExec(t, d, execID); ex.status != engine.WorkflowRunning {
		t.Fatalf("execution = %s, want RUNNING (回滚复活)", ex.status)
	}
	if ctrl := recoverRowOf(t, d, execID, "fan", 0); ctrl.status != engine.NodeExecuting {
		t.Fatalf("controller = %s, want EXECUTING (重派)", ctrl.status)
	}
	// 保活断言:0/2 实例零写(version/attempts/base/finished/output 逐列不动)。
	for _, ino := range []uint16{1, 3} {
		after := recoverRowOf(t, d, execID, "fan", ino)
		before := keptBefore[ino-1]
		if after.status != engine.NodeSucceeded || after.version != before.version ||
			after.attempts != before.attempts || after.attemptsBase != before.attemptsBase ||
			!after.finished.Valid || after.output.String != before.output.String {
			t.Fatalf("kept instance %d = %+v, want zero-write of %+v", ino, after, before)
		}
	}
	// 重跑断言:实例 1 重 claim(attempts=2、base=1、快照重写、deadline 挂新)。
	rerun := instanceRowOf(t, d, execID, "fan", 2)
	if rerun.status != engine.NodeExecuting || rerun.attempts != 2 || !rerun.deadlineOK {
		t.Fatalf("re-run instance = %s attempts=%d deadline=%v, want EXECUTING/2/valid",
			rerun.status, rerun.attempts, rerun.deadlineOK)
	}
	if base := recoverRowOf(t, d, execID, "fan", 2); base.attemptsBase != 1 {
		t.Fatalf("re-run attempts_base = %d, want 1 (预算满额:base=重置时 attempts)", base.attemptsBase)
	}

	// 重跑实例新输出 → 收敛 results = [旧 v0, 新 v1, 旧 v2](旧输出原样收割)。
	at2 := seedAttempt(t, d, execID, instances[1], 2)
	mustComplete(t, s, execID, instances[1], at2, engine.AttemptSucceed,
		json.RawMessage(`{"verdict":"v1new"}`))
	assertFanResults(t, d, execID, []any{
		map[string]any{"verdict": "v0"},
		map[string]any{"verdict": "v1new"},
		map[string]any{"verdict": "v2"},
	})
	if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
		t.Fatalf("complete post: %v", err)
	}
	if ex := readExec(t, d, execID); ex.status != engine.WorkflowSucceeded {
		t.Fatalf("execution = %s, want SUCCEEDED", ex.status)
	}
}

// TestRecoverCancelWedgeIncremental:D-t193 联动——取消楔停(控制器 + 在飞/
// 冻结实例 CANCELLED,SUCCEEDED 实例非楔源 untouched)→ 回滚自动定位首个
// CANCELLED 控制器 → 保活 SUCCEEDED 实例,仅楔源行重跑;遗留 RUNNING attempt
// 由回滚闭包同事务 DEAD 收口。
func TestRecoverCancelWedgeIncremental(t *testing.T) {
	s, d := newSched(t)
	doc := fmt.Sprintf(parallelNoRetryBody, "") // fail_fast 缺省
	input := `{"items": [{"name":"a"},{"name":"b"},{"name":"c"}]}`
	execID, instances := runToFanReady(t, s, d, doc, input, "fan")
	t.Cleanup(func() { cleanupExec(t, d, execID) })

	// instances[0](instance_no 1)先成功;instances[1](instance_no 2)在飞
	//(种 RUNNING attempt 模拟 worker 持有)。
	a0 := seedAttempt(t, d, execID, instances[0], 1)
	mustComplete(t, s, execID, instances[0], a0, engine.AttemptSucceed, json.RawMessage(`{"verdict":"v0"}`))
	att1 := seedAttempt(t, d, execID, instances[1], 1)

	cres, err := s.CancelExecution(context.Background(), CancelParams{
		ExecID: execID, WorkerID: "api:cancel", ActorID: "op"})
	if err != nil {
		t.Fatalf("CancelExecution: %v", err)
	}
	// 楔源 = 非终态行:控制行 + 实例 1/2 + post 恰四行。
	if cres.CancelledCount != 4 {
		t.Fatalf("cancelled_count = %d, want 4", cres.CancelledCount)
	}
	if ex := readExec(t, d, execID); ex.status != engine.WorkflowCancelled {
		t.Fatalf("execution = %s, want CANCELLED", ex.status)
	}

	res := rollbackAuto(t, s, execID)
	if res.TargetNodeID != "fan" {
		t.Fatalf("target = %q, want fan (首个 CANCELLED 控制器)", res.TargetNodeID)
	}
	if res.ResetCount != 4 {
		t.Fatalf("reset_count = %d, want 4 (controller+inst1+inst2+post; 保活 inst0 除外)", res.ResetCount)
	}
	// 保活:实例 0 SUCCEEDED 输出原样;楔源行重跑:1/2 EXECUTING attempts=2。
	if kept := recoverRowOf(t, d, execID, "fan", 1); kept.status != engine.NodeSucceeded ||
		kept.attempts != 1 || kept.output.String != `{"verdict": "v0"}` {
		t.Fatalf("kept instance 1 = %+v, want SUCCEEDED/attempts=1/old output", kept)
	}
	for _, ino := range []uint16{2, 3} {
		if r := instanceRowOf(t, d, execID, "fan", ino); r.status != engine.NodeExecuting || r.attempts != 2 {
			t.Fatalf("wedge instance %d = %s attempts=%d, want EXECUTING/2", ino, r.status, r.attempts)
		}
	}
	// 遗留 RUNNING attempt DEAD 收口('rollback_reset')。
	var attStatus string
	if err := d.QueryRow(`SELECT status FROM node_attempt WHERE id=?`, att1).Scan(&attStatus); err != nil {
		t.Fatalf("read attempt %d: %v", att1, err)
	}
	if attStatus != "DEAD" {
		t.Fatalf("leftover attempt = %s, want DEAD", attStatus)
	}
	// 楔源重跑完成 → 收敛合并 [v0, v1, v2];post 完成 → 执行 SUCCEEDED。
	for _, idx := range []int{1, 2} {
		a := seedAttempt(t, d, execID, instances[idx], 2)
		mustComplete(t, s, execID, instances[idx], a, engine.AttemptSucceed,
			json.RawMessage(fmt.Sprintf(`{"verdict":"v%d"}`, idx)))
	}
	assertFanResults(t, d, execID, []any{
		map[string]any{"verdict": "v0"},
		map[string]any{"verdict": "v1"},
		map[string]any{"verdict": "v2"},
	})
	if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
		t.Fatalf("complete post: %v", err)
	}
	if ex := readExec(t, d, execID); ex.status != engine.WorkflowSucceeded {
		t.Fatalf("execution = %s, want SUCCEEDED", ex.status)
	}
}

// TestRecoverRefreshIntentFullRerun:意图门——SUCCEEDED 执行 + 显式
// target=parallel = 再生语义(重跑可能产出不同结果,D-t193 决策 3),不保活:
// 整行集重置重跑(依赖门本可放行,此处被意图门一票否决)。
func TestRecoverRefreshIntentFullRerun(t *testing.T) {
	s, d := newSched(t)
	doc := fmt.Sprintf(parallelNoRetryBody, "")
	input := `{"items": [{"name":"a"},{"name":"b"},{"name":"c"}]}`
	execID, instances := runToFanReady(t, s, d, doc, input, "fan")
	t.Cleanup(func() { cleanupExec(t, d, execID) })

	for idx := range instances {
		a := seedAttempt(t, d, execID, instances[idx], 1)
		mustComplete(t, s, execID, instances[idx], a, engine.AttemptSucceed,
			json.RawMessage(fmt.Sprintf(`{"verdict":"v%d"}`, idx)))
	}
	if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
		t.Fatalf("complete post: %v", err)
	}
	if ex := readExec(t, d, execID); ex.status != engine.WorkflowSucceeded {
		t.Fatalf("execution = %s, want SUCCEEDED (前置)", ex.status)
	}

	res := rollbackTarget(t, s, execID, "fan")
	if res.ResetCount != 5 {
		t.Fatalf("reset_count = %d, want 5 (再生意图:控制行+3实例+post 整行集重置,零保活)", res.ResetCount)
	}
	if ctrl := recoverRowOf(t, d, execID, "fan", 0); ctrl.status != engine.NodeExecuting {
		t.Fatalf("controller = %s, want EXECUTING (重派)", ctrl.status)
	}
	for ino := uint16(1); ino <= 3; ino++ {
		r := instanceRowOf(t, d, execID, "fan", ino)
		rec := recoverRowOf(t, d, execID, "fan", ino)
		if r.status != engine.NodeExecuting || r.attempts != 2 {
			t.Fatalf("instance %d = %s attempts=%d, want EXECUTING/2 (全量重跑)", ino, r.status, r.attempts)
		}
		if rec.output.Valid || rec.finished.Valid {
			t.Fatalf("instance %d output/finished 未清列: %+v", ino, rec)
		}
	}
	// 全新重跑收敛(旧输出零残留)。
	for idx := range instances {
		a := seedAttempt(t, d, execID, instances[idx], 2)
		mustComplete(t, s, execID, instances[idx], a, engine.AttemptSucceed,
			json.RawMessage(fmt.Sprintf(`{"verdict":"fresh%d"}`, idx)))
	}
	assertFanResults(t, d, execID, []any{
		map[string]any{"verdict": "fresh0"},
		map[string]any{"verdict": "fresh1"},
		map[string]any{"verdict": "fresh2"},
	})
}

// TestRecoverDependencyGateConservative:依赖门——显式 target=pre(刷新上游)
// 时 fan ∈ downstream(pre) 闭包;body 引用 {{nodes.pre.output.text}} ⇒ fan
// 数据依赖 {pre} ∩ closure ≠ ∅ ⇒ 保守整行集重跑(旧输出对不上新输入)。
// 与 flagship(同文档族、依赖在闭包外 ⇒ 保活)构成两态对照。
func TestRecoverDependencyGateConservative(t *testing.T) {
	s, d := newSched(t)
	doc := fmt.Sprintf(parallelNoRetryBody, "")
	input := `{"items": [{"name":"a"},{"name":"b"},{"name":"c"}]}`
	execID, instances := runToFanReady(t, s, d, doc, input, "fan")
	t.Cleanup(func() { cleanupExec(t, d, execID) })

	for idx := range instances {
		a := seedAttempt(t, d, execID, instances[idx], 1)
		mustComplete(t, s, execID, instances[idx], a, engine.AttemptSucceed,
			json.RawMessage(fmt.Sprintf(`{"verdict":"v%d"}`, idx)))
	}
	if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
		t.Fatalf("complete post: %v", err)
	}
	if ex := readExec(t, d, execID); ex.status != engine.WorkflowSucceeded {
		t.Fatalf("execution = %s, want SUCCEEDED (前置)", ex.status)
	}

	res := rollbackTarget(t, s, execID, "pre")
	if !res.RerunTarget {
		t.Fatalf("rerun_target = false, want true (pre 是 worker 可执行类型快照重跑)")
	}
	// pre 重跑完成(fan 的前驱就绪)后 fan 才重派。
	if err := completeNode(t, s, d, execID, "pre", engine.AttemptSucceed, json.RawMessage(`{"text":"ctx-out-2"}`)); err != nil {
		t.Fatalf("complete pre: %v", err)
	}
	// 依赖门保守:fan 整行集重跑(pre ∈ refs(fan) ∩ closure)。
	for ino := uint16(1); ino <= 3; ino++ {
		r := instanceRowOf(t, d, execID, "fan", ino)
		if r.status != engine.NodeExecuting || r.attempts != 2 {
			t.Fatalf("instance %d = %s attempts=%d, want EXECUTING/2 (依赖在闭包内 ⇒ 不保活)",
				ino, r.status, r.attempts)
		}
	}
}

// TestRecoverControllerSucceededWholeRowset:行集原子性 + 精确依赖门——
// dep-free 文档(body 零节点引用)下显式 target=pre:fan ∈ 闭包但依赖集空 ⇒
// 门控放行;控制行 SUCCEEDED ⇒ 整行集保活(零重置零重派:version/output/
// finished 逐列不动),post 直接骑保活控制器重派。
func TestRecoverControllerSucceededWholeRowset(t *testing.T) {
	s, d := newSched(t)
	input := `{"items": [{"name":"a"},{"name":"b"},{"name":"c"}]}`
	execID, instances := runToFanReady(t, s, d, parallelNoDepBody, input, "fan")
	t.Cleanup(func() { cleanupExec(t, d, execID) })

	for idx := range instances {
		a := seedAttempt(t, d, execID, instances[idx], 1)
		mustComplete(t, s, execID, instances[idx], a, engine.AttemptSucceed,
			json.RawMessage(fmt.Sprintf(`{"verdict":"v%d"}`, idx)))
	}
	if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
		t.Fatalf("complete post: %v", err)
	}
	before := [4]recoverRow{
		recoverRowOf(t, d, execID, "fan", 0),
		recoverRowOf(t, d, execID, "fan", 1),
		recoverRowOf(t, d, execID, "fan", 2),
		recoverRowOf(t, d, execID, "fan", 3),
	}

	res := rollbackTarget(t, s, execID, "pre")
	if !res.RerunTarget {
		t.Fatalf("rerun_target = false, want true")
	}
	// 重置面只含 post(pre 走 RerunNode 不计 reset;fan 整行集零写)。
	if res.ResetCount != 1 {
		t.Fatalf("reset_count = %d, want 1 (post;pre=RerunNode,fan 整行集保活)", res.ResetCount)
	}
	for ino := uint16(0); ino <= 3; ino++ {
		after := recoverRowOf(t, d, execID, "fan", ino)
		b := before[ino]
		if after != b {
			t.Fatalf("fan row %d = %+v, want zero-write of %+v (整行集保活)", ino, after, b)
		}
	}
	// post 骑保活控制器(fan SUCCEEDED)直接重派。
	if post := readNode(t, d, execID, "post"); post.status != engine.NodeExecuting {
		t.Fatalf("post = %s, want EXECUTING (骑保活控制器重派)", post.status)
	}
	// 完成 pre(新输出)与 post:执行 SUCCEEDED,results 与保活值一致。
	if err := completeNode(t, s, d, execID, "pre", engine.AttemptSucceed, json.RawMessage(`{"text":"ctx-out-2"}`)); err != nil {
		t.Fatalf("complete pre: %v", err)
	}
	if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
		t.Fatalf("complete post: %v", err)
	}
	ex := readExec(t, d, execID)
	if ex.status != engine.WorkflowSucceeded || !ex.outputs.Valid {
		t.Fatalf("execution = %s outputs=%v, want SUCCEEDED with outputs", ex.status, ex.outputs.Valid)
	}
	wantResults := []any{
		map[string]any{"verdict": "v0"},
		map[string]any{"verdict": "v1"},
		map[string]any{"verdict": "v2"},
	}
	if got := decodeSem(t, ex.outputs.String).(map[string]any)["all"]; !reflect.DeepEqual(got, wantResults) {
		t.Fatalf("workflow outputs.all = %v, want %v (保活 results 原样)", got, wantResults)
	}
}

// TestRecoverAllKeptConvergesAtDispatch:全保活零重跑——控制器重派后 claim
// 循环零对象,收敛必须发生在派发事务内(尾部 gatherCheck);缺失 ⇒ 控制器
// 滞留 EXECUTING 无人推进(无 worker 驱动面)。种态走 D-t193 取消竞速形状:
// 末实例收口前取消取胜(控制器 CANCELLED、全实例 SUCCEEDED 非楔源)。
func TestRecoverAllKeptConvergesAtDispatch(t *testing.T) {
	s, d := newSched(t)
	doc := fmt.Sprintf(parallelNoRetryBody, `"on_failure": "wait_all",`)
	input := `{"items": [{"name":"a"},{"name":"b"},{"name":"c"}]}`
	execID, instances := runToFanReady(t, s, d, doc, input, "fan")
	t.Cleanup(func() { cleanupExec(t, d, execID) })

	// instances[0]/[2] 常规成功;instances[1](instance_no 2)绕过 Complete 直收
	// SUCCEEDED(engine CAS helper,「种 WA」先例)——控制器滞留 EXECUTING、
	// 三实例全 SUCCEEDED 的竞速形状。
	for _, idx := range []int{0, 2} {
		a := seedAttempt(t, d, execID, instances[idx], 1)
		mustComplete(t, s, execID, instances[idx], a, engine.AttemptSucceed,
			json.RawMessage(fmt.Sprintf(`{"verdict":"v%d"}`, idx)))
	}
	r1 := instanceRowOf(t, d, execID, "fan", 2)
	if _, err := engine.SucceedNode(context.Background(), d, r1.id, r1.version,
		"sched-test", json.RawMessage(`{"verdict":"v1late"}`)); err != nil {
		t.Fatalf("plant instance 1 succeed: %v", err)
	}
	if _, err := s.CancelExecution(context.Background(), CancelParams{
		ExecID: execID, WorkerID: "api:cancel", ActorID: "op"}); err != nil {
		t.Fatalf("CancelExecution: %v", err)
	}
	if ctrl := recoverRowOf(t, d, execID, "fan", 0); ctrl.status != engine.NodeCancelled {
		t.Fatalf("controller = %s, want CANCELLED (取消取胜竞速形状)", ctrl.status)
	}

	res := rollbackAuto(t, s, execID)
	if res.ResetCount != 2 {
		t.Fatalf("reset_count = %d, want 2 (controller + post;三实例全保活)", res.ResetCount)
	}
	// 核心断言:控制器同事务收敛(results 全旧输出),非滞留 EXECUTING。
	assertFanResults(t, d, execID, []any{
		map[string]any{"verdict": "v0"},
		map[string]any{"verdict": "v1late"},
		map[string]any{"verdict": "v2"},
	})
	if post := readNode(t, d, execID, "post"); post.status != engine.NodeExecuting {
		t.Fatalf("post = %s, want EXECUTING (收敛后推进)", post.status)
	}
	if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
		t.Fatalf("complete post: %v", err)
	}
	if ex := readExec(t, d, execID); ex.status != engine.WorkflowSucceeded {
		t.Fatalf("execution = %s, want SUCCEEDED", ex.status)
	}
}

// TestRecoverDynamicFanoutKeptRows:D-t192 联动——动态扇出(foreach = node
// 引用,来源 pre 在闭包外)部分失败回滚,保活行骑 reconcile 已锁行集过断言
// (活跃集内 SUCCEEDED 合法),重跑实例收敛后 results 合并新旧输出。
func TestRecoverDynamicFanoutKeptRows(t *testing.T) {
	s, d := newSched(t)
	doc := fmt.Sprintf(parallelDynamicDoc, `"on_failure": "wait_all",`)
	execID, instances := runDynToFanReady(t, s, d, doc, `{"topic":"x"}`, preOut(`["a","b","c"]`))
	t.Cleanup(func() { cleanupExec(t, d, execID) })

	at := seedAttempt(t, d, execID, instances[1], 1)
	mustComplete(t, s, execID, instances[1], at, engine.AttemptFailed, nil)
	for _, idx := range []int{0, 2} {
		a := seedAttempt(t, d, execID, instances[idx], 1)
		mustComplete(t, s, execID, instances[idx], a, engine.AttemptSucceed,
			json.RawMessage(fmt.Sprintf(`{"verdict":"v%d"}`, idx)))
	}
	if ctrl := instanceRowOf(t, d, execID, "fan", 0); ctrl.status != engine.NodeFailed {
		t.Fatalf("controller = %s, want FAILED (前置)", ctrl.status)
	}

	res := rollbackAuto(t, s, execID)
	if res.ResetCount != 2 {
		t.Fatalf("reset_count = %d, want 2 (controller + 失败实例)", res.ResetCount)
	}
	if kept := recoverRowOf(t, d, execID, "fan", 1); kept.status != engine.NodeSucceeded || kept.attempts != 1 {
		t.Fatalf("dynamic kept instance 1 = %+v, want SUCCEEDED/attempts=1", kept)
	}
	if r := instanceRowOf(t, d, execID, "fan", 2); r.status != engine.NodeExecuting || r.attempts != 2 {
		t.Fatalf("dynamic re-run instance = %s attempts=%d, want EXECUTING/2", r.status, r.attempts)
	}
	at2 := seedAttempt(t, d, execID, instances[1], 2)
	mustComplete(t, s, execID, instances[1], at2, engine.AttemptSucceed,
		json.RawMessage(`{"verdict":"v1new"}`))
	assertFanResults(t, d, execID, []any{
		map[string]any{"verdict": "v0"},
		map[string]any{"verdict": "v1new"},
		map[string]any{"verdict": "v2"},
	})
}

// parallelRecoverBranchDSL:对抗审计 HIGH 修锚——pre(flag)→ gate(branch)→
// fan(数据依赖空的 parallel)→ post 一臂,alt 另一臂(臂不相交,D-t24 #2)。
// 依赖门对 fan 放行(body 零节点引用),whole-keep 的控制依赖门必须拦:
// gate ∈ 闭包重评估可翻转路由,保活的 SUCCEEDED 控制器会逃逸 dispatchBranch
// 的 PENDING-only 剪枝面(废弃臂急派 + 残留 SUCCEEDED 入终态)。
const parallelRecoverBranchDSL = `{
  "dsl": "1.0",
  "name": "pr-branch-wf",
  "inputs": [{"name": "items", "type": "array", "required": true}],
  "nodes": [
    {"id": "pre", "type": "llm", "prompt": {"template": "gen"}, "outputs": {"flag": "boolean"}},
    {"id": "gate", "type": "branch",
     "cases": [{"when": {"left": "{{nodes.pre.output.flag}}", "op": "eq", "right": true}, "next": "fan"}],
     "default": "alt"},
    {"id": "fan", "type": "parallel", "parallel": {
      "foreach": "{{inputs.items}}",
      "body": {"type": "llm",
        "prompt": {"template": "评审", "user_data": [{"name": "name", "ref": "{{item.name}}"}]},
        "outputs": {"text": "string"}}}},
    {"id": "post", "type": "llm",
     "prompt": {"template": "p", "user_data": [{"name": "v", "ref": "{{nodes.fan.output.results}}"}]},
     "outputs": {"text": "string"}},
    {"id": "alt", "type": "llm", "prompt": {"template": "alt"}, "outputs": {"text": "string"}}],
  "edges": [{"from": "pre", "to": "gate"}, {"from": "gate", "to": "fan"},
            {"from": "gate", "to": "alt"}, {"from": "fan", "to": "post"}],
  "outputs": {"alt": "{{nodes.alt.output.text}}"}
}`

// TestRecoverBranchAncestorBlocksWholeKeep:对抗审计 HIGH 修锚——branch 祖先
// 在闭包内时 whole-keep 关闭(整行集重跑),翻转路由后废弃臂正确剪枝;
// 变异(删控制依赖门)下 fan 保活 SUCCEEDED ⇒ post 在旧 results 上急派且
// 翻转后不剪 ⇒ fan/post 终态与路由矛盾,本测试红。
func TestRecoverBranchAncestorBlocksWholeKeep(t *testing.T) {
	s, d := newSched(t)
	input := `{"items": [{"name":"a"},{"name":"b"},{"name":"c"}]}`
	// 手动种子(runToFanReady 的 pre 输出硬编码 text,本 DSL 需要 flag 路由)。
	_, execID := inst(t, s, d, parallelRecoverBranchDSL, input)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := completeNode(t, s, d, execID, "pre", engine.AttemptSucceed, json.RawMessage(`{"flag":true}`)); err != nil {
		t.Fatalf("complete pre: %v", err)
	}
	var instances []uint64
	for _, r := range readAllInstanceRowsOf(t, d, execID, "fan") {
		if r.instanceNo > 0 {
			instances = append(instances, r.id)
		}
	}
	if len(instances) != 3 {
		t.Fatalf("instances = %d, want 3 (flag=true 走 fan 臂)", len(instances))
	}

	// 第一轮:flag=true → fan 臂;全实例成功 → post 成功 → wf SUCCEEDED。
	for idx := range instances {
		a := seedAttempt(t, d, execID, instances[idx], 1)
		mustComplete(t, s, execID, instances[idx], a, engine.AttemptSucceed,
			json.RawMessage(fmt.Sprintf(`{"text":"v%d"}`, idx)))
	}
	if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"text":"p1"}`)); err != nil {
		t.Fatalf("complete post: %v", err)
	}
	if ex := readExec(t, d, execID); ex.status != engine.WorkflowSucceeded {
		t.Fatalf("execution = %s, want SUCCEEDED (前置)", ex.status)
	}
	if alt := readNode(t, d, execID, "alt"); alt.status != engine.NodeSkipped {
		t.Fatalf("alt = %s, want SKIPPED (前置:flag=true 走 fan 臂)", alt.status)
	}

	// 回滚 target=pre(重跑翻转 flag):闭包含 gate ⇒ 控制依赖门关 ⇒ fan
	// 整行集重跑(非 whole-keep)。
	res := rollbackTarget(t, s, execID, "pre")
	if res.ResetCount != 7 {
		t.Fatalf("reset_count = %d, want 7 (gate+fan4+post+alt 整闭包重置,fan 不 whole-keep)", res.ResetCount)
	}

	// flag=false → gate 重评估翻转到 alt 臂:fan(已 PENDING)被剪 + 传播
	// post;alt 执行 → wf SUCCEEDED,终态只含取路臂。
	if err := completeNode(t, s, d, execID, "pre", engine.AttemptSucceed, json.RawMessage(`{"flag":false}`)); err != nil {
		t.Fatalf("complete pre(flip): %v", err)
	}
	if ctrl := instanceRowOf(t, d, execID, "fan", 0); ctrl.status != engine.NodeSkipped {
		t.Fatalf("fan controller = %s, want SKIPPED (翻转后废弃臂重剪)", ctrl.status)
	}
	for ino := uint16(1); ino <= 3; ino++ {
		if r := instanceRowOf(t, d, execID, "fan", ino); r.status != engine.NodeSkipped {
			t.Fatalf("fan instance %d = %s, want SKIPPED (剪枝传播)", ino, r.status)
		}
	}
	if post := readNode(t, d, execID, "post"); post.status != engine.NodeSkipped {
		t.Fatalf("post = %s, want SKIPPED (废弃臂传播;急派旧 results = 控制依赖门失效)", post.status)
	}
	if err := completeNode(t, s, d, execID, "alt", engine.AttemptSucceed, json.RawMessage(`{"text":"alt-out"}`)); err != nil {
		t.Fatalf("complete alt: %v", err)
	}
	ex := readExec(t, d, execID)
	if ex.status != engine.WorkflowSucceeded || !ex.outputs.Valid {
		t.Fatalf("execution = %s outputs=%v, want SUCCEEDED with outputs", ex.status, ex.outputs.Valid)
	}
	if got := decodeSem(t, ex.outputs.String).(map[string]any)["alt"]; got != "alt-out" {
		t.Fatalf("outputs.alt = %v, want alt-out (只含取路臂)", got)
	}
}

// parallelNoDepMSBody:parallelNoDepBody + min_success=1 wait_all——「控制行
// SUCCEEDED + FAILED 证据行」部分容忍形状的载体(对抗审计 MEDIUM① 锚)。
const parallelNoDepMSBody = `{
  "dsl": "1.0",
  "name": "parallel-wf",
  "inputs": [{"name": "items", "type": "array", "required": true}],
  "nodes": [
    {"id": "pre", "type": "llm", "prompt": {"template": "t"}, "outputs": {"text": "string"}},
    {"id": "fan", "type": "parallel", "parallel": {
      "foreach": "{{inputs.items}}",
      "min_success": 1, "on_failure": "wait_all",
      "body": {"type": "llm",
        "prompt": {"template": "评审", "user_data": [{"name": "name", "ref": "{{item.name}}"}]},
        "outputs": {"text": "string"}}}},
    {"id": "post", "type": "tool", "tool": "http",
     "inputs": {"payload": "{{nodes.fan.output.results}}"}, "outputs": {"done": "boolean"}}
  ],
  "edges": [{"from": "pre", "to": "fan"}, {"from": "fan", "to": "post"}],
  "outputs": {"all": "{{nodes.fan.output.results}}"}
}`

// TestRecoverWholeKeepEvidenceRows:对抗审计 MEDIUM① 锚——部分容忍收敛后的
// FAILED 证据行在 whole-keep 下零重置(决策 4 半区);变异(whole 分支收窄
// 为仅控制行+SUCCEEDED 实例)下证据行被重置为悬死 PENDING ⇒ finishIfDone
// 恒 no-op ⇒ wf 永不收敛,本测试红。
func TestRecoverWholeKeepEvidenceRows(t *testing.T) {
	s, d := newSched(t)
	input := `{"items": [{"name":"a"},{"name":"b"},{"name":"c"}]}`
	execID, instances := runToFanReady(t, s, d, parallelNoDepMSBody, input, "fan")
	t.Cleanup(func() { cleanupExec(t, d, execID) })

	// 0/2 成功、1 终败:wait_all+k=1 收齐 converged(SUCCEEDED + failed_count=1)。
	at := seedAttempt(t, d, execID, instances[1], 1)
	mustComplete(t, s, execID, instances[1], at, engine.AttemptFailed, nil)
	for _, idx := range []int{0, 2} {
		a := seedAttempt(t, d, execID, instances[idx], 1)
		mustComplete(t, s, execID, instances[idx], a, engine.AttemptSucceed,
			json.RawMessage(fmt.Sprintf(`{"text":"v%d"}`, idx)))
	}
	if ctrl := instanceRowOf(t, d, execID, "fan", 0); ctrl.status != engine.NodeSucceeded {
		t.Fatalf("controller = %s, want SUCCEEDED (部分容忍收敛)", ctrl.status)
	}
	if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
		t.Fatalf("complete post: %v", err)
	}
	if ex := readExec(t, d, execID); ex.status != engine.WorkflowSucceeded {
		t.Fatalf("execution = %s, want SUCCEEDED (前置)", ex.status)
	}
	evBefore := recoverRowOf(t, d, execID, "fan", 2)

	// 回滚 target=pre:whole-keep 含 FAILED 证据行(零重置)。
	res := rollbackTarget(t, s, execID, "pre")
	if res.ResetCount != 1 {
		t.Fatalf("reset_count = %d, want 1 (post;fan 整行集含证据行保活)", res.ResetCount)
	}
	evAfter := recoverRowOf(t, d, execID, "fan", 2)
	if evAfter != evBefore || evAfter.status != engine.NodeFailed || !evAfter.finished.Valid {
		t.Fatalf("evidence row = %+v → %+v, want zero-write FAILED(重置 = 悬死 PENDING 变异)", evBefore, evAfter)
	}

	// pre 重跑 → post 骑保活控制器重派 → wf 再收敛(证据行不阻塞
	// finishIfDone 的实例容忍面)。
	if err := completeNode(t, s, d, execID, "pre", engine.AttemptSucceed, json.RawMessage(`{"text":"ctx-2"}`)); err != nil {
		t.Fatalf("complete pre: %v", err)
	}
	if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
		t.Fatalf("complete post: %v", err)
	}
	if ex := readExec(t, d, execID); ex.status != engine.WorkflowSucceeded {
		t.Fatalf("execution = %s, want SUCCEEDED (证据行零重置下终态可达)", ex.status)
	}
}

// parallelDynNoRefBody:动态扇出 + body 零节点引用——foreach 来源捕获是唯一
// 依赖门输入的形状(对抗审计 MEDIUM② 锚:NodeDataDeps 丢来源捕获的变异下,
// 来源 ∈ 闭包的门应关而误开,shrink 撞保活 SUCCEEDED 盈余行 drift 报错)。
const parallelDynNoRefBody = `{
  "dsl": "1.0",
  "name": "parallel-dyn-noref",
  "inputs": [{"name": "topic", "type": "string", "required": true}],
  "nodes": [
    {"id": "pre", "type": "llm", "prompt": {"template": "t"}, "outputs": {"list": "array", "text": "string"}},
    {"id": "fan", "type": "parallel", "parallel": {
      "foreach": "{{nodes.pre.output.list}}",
      "on_failure": "wait_all",
      "body": {"type": "llm",
        "prompt": {"template": "评审", "user_data": [{"name": "review", "ref": "{{item}}"}]},
        "outputs": {"text": "string"}}}},
    {"id": "post", "type": "tool", "tool": "http",
     "inputs": {"payload": "{{nodes.fan.output.results}}"}, "outputs": {"done": "boolean"}}
  ],
  "edges": [{"from": "pre", "to": "fan"}, {"from": "fan", "to": "post"}],
  "outputs": {"all": "{{nodes.fan.output.results}}"}
}`

// TestRecoverDynamicSourceInClosureFullRerun:对抗审计 MEDIUM② 锚——来源 ∈
// 闭包(target=pre 重跑、list 收缩)⇒ 依赖门关 ⇒ fan 整行集重跑,盈余行
// 走 PENDING→SKIPPED 正路;results = 新轮 M 零旧值残留。
func TestRecoverDynamicSourceInClosureFullRerun(t *testing.T) {
	s, d := newSched(t)
	execID, instances := runDynToFanReady(t, s, d, parallelDynNoRefBody, `{"topic":"x"}`, preOut(`["a","b","c"]`))
	t.Cleanup(func() { cleanupExec(t, d, execID) })

	// instances[1](instance_no 2)终败、instances[0]/[2] 成功(wait_all 收齐含败
	// → FAILED)。
	at := seedAttempt(t, d, execID, instances[1], 1)
	mustComplete(t, s, execID, instances[1], at, engine.AttemptFailed, nil)
	for _, idx := range []int{0, 2} {
		a := seedAttempt(t, d, execID, instances[idx], 1)
		mustComplete(t, s, execID, instances[idx], a, engine.AttemptSucceed,
			json.RawMessage(fmt.Sprintf(`{"text":"v%d"}`, idx)))
	}
	if ctrl := instanceRowOf(t, d, execID, "fan", 0); ctrl.status != engine.NodeFailed {
		t.Fatalf("controller = %s, want FAILED (前置)", ctrl.status)
	}

	// 回滚 target=pre:foreach 来源 ∈ refs ∩ closure ⇒ 依赖门关 ⇒ 整行集重跑。
	res := rollbackTarget(t, s, execID, "pre")
	if res.ResetCount != 4 {
		t.Fatalf("reset_count = %d, want 4 (fan 控制行+3 实例整组;来源在闭包 ⇒ 零保活)", res.ResetCount)
	}

	// pre 重跑产出收缩 list [a,b] → 重派 reconcile:盈余实例 3 PENDING→SKIPPED,
	// 两实例新轮重跑 → results 长度 2 全新值。
	if err := completeNode(t, s, d, execID, "pre", engine.AttemptSucceed, json.RawMessage(preOut(`["a","b"]`))); err != nil {
		t.Fatalf("complete pre(shrink): %v", err)
	}
	if r := instanceRowOf(t, d, execID, "fan", 3); r.status != engine.NodeSkipped {
		t.Fatalf("surplus instance 3 = %s, want SKIPPED (盈余正路;撞保活 SUCCEEDED = 依赖门变异)", r.status)
	}
	for _, idx := range []int{0, 1} {
		a := seedAttempt(t, d, execID, instances[idx], 2)
		mustComplete(t, s, execID, instances[idx], a, engine.AttemptSucceed,
			json.RawMessage(fmt.Sprintf(`{"text":"fresh%d"}`, idx)))
	}
	assertFanResults(t, d, execID, []any{
		map[string]any{"text": "fresh0"},
		map[string]any{"text": "fresh1"},
	})
	if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed, json.RawMessage(`{"done":true}`)); err != nil {
		t.Fatalf("complete post: %v", err)
	}
	if ex := readExec(t, d, execID); ex.status != engine.WorkflowSucceeded {
		t.Fatalf("execution = %s, want SUCCEEDED", ex.status)
	}
}
