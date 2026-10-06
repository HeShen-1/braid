package scheduler

// T19.6 在飞回滚 quiesce 测试(D-2026-10-06-t196-inflight-rollback-quiesce):
// 守卫② 残留行(EXECUTING/WA)在 Quiesce=true 时同事务楔停收敛——
// attempt 收口(code=rollback_quiesce)× CancelNode(轮次单调)× 审计
// from=EXECUTING/WA × 闭包归属=步骤5收编谓词零追加(控制行键控;早收敛
// 实例残行 = CANCELLED 证据行)× 显式 target=残行走 Rerun 分流(楔停保
// 快照)× 零残留 no-op × 旗标缺省 409 面逐字不变(TestRollbackGuards.
// blocked_inflight 既有锚)。
//
// 变异自查(实现错 → 哪条红):
//   - 楔停漏 RUNNING attempt 收口 → wedge_executing 的 attempt DEAD 断言红;
//   - quiesce 不 CancelNode 放行残行 → reset 谓词只吃终态,c 状态断言红
//    (滞留 EXECUTING,wf 复活后无人推进);
//   - addDown 追加收编重引入 → early_converged 的独立下游 tail 断言红
//    (连带重置;fan 行集内 collateral 被 T19.5 whole-keep 吸收,变异盲
//    已由独立下游补齐——实证);
//   - 显式 target 残行在楔停前判 Terminal → rerun 分流红(RerunTarget=false);
//   - payload 漏 quiesced_count → payload 子测红;
//   - WA 残行漏楔(守卫② 面 ≠ CancelNode 谓词面)→ WA 子测状态断言红。

import (
	"context"
	"encoding/json"
	"testing"

	"braid/internal/engine"
)

// rbApprovalDiamondDSL:菱形 a→(b,c)→d,c = approval(挂起面 WA 残留形状:
// c 挂起 → b 终败 → wf FAILED 下 c 行 WAITING_APPROVAL 残留)。
const rbApprovalDiamondDSL = `{"dsl":"1.0","name":"rb-appr-d",
 "edges":[{"from":"a","to":"b"},{"from":"a","to":"c"},{"from":"b","to":"d"},{"from":"c","to":"d"}],
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"a"},"outputs":{"text":"string"}},
  {"id":"b","type":"llm","prompt":{"template":"b"},"outputs":{"text":"string"}},
  {"id":"c","type":"approval","approvers":["carol"],"mode":"any","timeout_hours":24,
   "on_timeout":"fail","instructions":"请审批","outputs":{"comment":"string"}},
  {"id":"d","type":"llm","prompt":{"template":"d","user_data":[{"name":"v","ref":"{{nodes.b.output.text}}"},{"name":"w","ref":"{{nodes.c.output.comment}}"}]},"outputs":{"text":"string"}}],
 "outputs":{"done":"{{nodes.d.output.text}}"}}`

// rbQuiesceEarlyDoc:早收敛锚专用——pre→(fan∥sib)、fan→(post∥tail);
// min_success=1 fail_fast(实例 1 成功即收敛控制器,实例 2 留在飞);sib =
// 独立失败源;tail = fan 的独立下游(addDown 变异的观测面——并行文档
// parallelDynamicDoc 的 fan 下游唯 post,无独立下游时 T19.5 whole-keep 恰好
// 吸收行集内 collateral,变异盲)。
const rbQuiesceEarlyDoc = `{
  "dsl": "1.0",
  "name": "rb-quiesce-early",
  "inputs": [{"name": "topic", "type": "string", "required": true}],
  "nodes": [
    {"id": "pre", "type": "llm", "prompt": {"template": "t"}, "outputs": {"list": "array", "text": "string"}},
    {"id": "fan", "type": "parallel", "parallel": {
      "foreach": "{{nodes.pre.output.list}}",
      "min_success": 1, "on_failure": "fail_fast",
      "body": {"type": "llm",
        "prompt": {"template": "评审",
          "user_data": [{"name": "review", "ref": "{{item}}"},
                        {"name": "ctx", "ref": "{{nodes.pre.output.text}}"}]},
        "outputs": {"verdict": "string"}}
    }},
    {"id": "sib", "type": "llm", "prompt": {"template": "s"}, "outputs": {"text": "string"}},
    {"id": "post", "type": "tool", "tool": "http",
     "inputs": {"payload": "{{nodes.fan.output.results}}"}, "outputs": {"done": "boolean"}},
    {"id": "tail", "type": "llm",
     "prompt": {"template": "tail", "user_data": [{"name": "v", "ref": "{{nodes.pre.output.text}}"}]},
     "outputs": {"text": "string"}}
  ],
  "edges": [{"from": "pre", "to": "fan"}, {"from": "pre", "to": "sib"},
            {"from": "fan", "to": "post"}, {"from": "fan", "to": "tail"}],
  "outputs": {"all": "{{nodes.fan.output.results}}"}}
`

// TestRollbackQuiesce:旗标两态协议主体。
func TestRollbackQuiesce(t *testing.T) {
	s, d := newSched(t)

	t.Run("wedge_executing_residue", func(t *testing.T) {
		execID := rbStart(t, s, d, rbDiamondDSL, `{"q":"hi"}`)
		a := readNode(t, d, execID, "a")
		mustComplete(t, s, execID, a.id, seedAttempt(t, d, execID, a.id, 1),
			engine.AttemptSucceed, json.RawMessage(`{"text":"va"}`))
		b := readNode(t, d, execID, "b")
		mustComplete(t, s, execID, b.id, seedAttempt(t, d, execID, b.id, 1),
			engine.AttemptFailed, nil) // fail-fast:wf FAILED,c 仍 EXECUTING
		c := readNode(t, d, execID, "c")
		attC := seedAttempt(t, d, execID, c.id, 1)

		// 协议观测态:无旗标 → 409,残留在场、执行零副作用。
		_, err := s.RollbackTo(context.Background(), RollbackParams{
			ExecID: execID, TargetNodeID: "b", WorkerID: "api-rb", ActorID: "op"})
		re := wantRollbackErrBlocked(t, err)
		if !containsBlockedPrefix(re.Blocked, "c:") {
			t.Fatalf("blocked 清单应含 c,got %v", re.Blocked)
		}
		if st := readNode(t, d, execID, "c").status; st != engine.NodeExecuting {
			t.Fatalf("首问后 c = %s, want EXECUTING(409 零副作用)", st)
		}

		// 收敛态:quiesce=true → 单事务楔停 + 回滚。
		res, err := s.RollbackTo(context.Background(), RollbackParams{
			ExecID: execID, TargetNodeID: "b", WorkerID: "api-rb", ActorID: "op", Quiesce: true})
		if err != nil {
			t.Fatalf("RollbackTo(quiesce): %v", err)
		}
		if res.TargetNodeID != "b" || res.QuiescedCount != 1 {
			t.Fatalf("res = %+v, want target=b quiesced=1", res)
		}
		// 残留 attempt:DEAD + code(楔停收口归因,区别 rollback_reset)。
		var status, code string
		if err := d.QueryRow(`SELECT status, error_code FROM node_attempt WHERE id=?`, attC).
			Scan(&status, &code); err != nil {
			t.Fatalf("read attempt: %v", err)
		}
		if status != "DEAD" || code != "rollback_quiesce" {
			t.Fatalf("attempt = %s/%q, want DEAD/rollback_quiesce", status, code)
		}
		// c:楔停 CANCELLED → 收编重置 → advance 重派(前驱 a 终态)。
		c2 := readNode(t, d, execID, "c")
		if c2.status != engine.NodeExecuting || c2.attempts != 2 {
			t.Fatalf("c = %+v, want EXECUTING attempts=2(楔停→重置→重派)", c2)
		}
		// b:目标重跑。
		if b2 := readNode(t, d, execID, "b"); b2.status != engine.NodeExecuting || b2.attempts != 2 {
			t.Fatalf("b = %+v, want EXECUTING attempts=2(rerun)", b2)
		}
		if e := readExec(t, d, execID); e.status != engine.WorkflowRunning {
			t.Fatalf("wf = %s, want RUNNING(单事务收敛)", e.status)
		}
		// 楔停审计锚:node.state_changed from=EXECUTING to=CANCELLED(暂存面)。
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM audit_pending
		  WHERE execution_id=? AND event_type='node.state_changed'
		    AND JSON_UNQUOTE(JSON_EXTRACT(payload,'$.from'))='EXECUTING'
		    AND JSON_UNQUOTE(JSON_EXTRACT(payload,'$.to'))='CANCELLED'`, execID).Scan(&n); err != nil {
			t.Fatalf("count wedge node rows: %v", err)
		}
		if n != 1 {
			t.Fatalf("from=EXECUTING to=CANCELLED = %d 行, want 1(c 楔停)", n)
		}
	})

	t.Run("wedge_wa_residue", func(t *testing.T) {
		execID := rbStart(t, s, d, rbApprovalDiamondDSL, `{"q":"hi"}`)
		a := readNode(t, d, execID, "a")
		mustComplete(t, s, execID, a.id, seedAttempt(t, d, execID, a.id, 1),
			engine.AttemptSucceed, json.RawMessage(`{"text":"va"}`))
		// c(approval)挂起:wf WA、c 行 WA;b 仍在飞 → 终败 → wf FAILED
		// 下 c 行 WAITING_APPROVAL 残留。
		c := readNode(t, d, execID, "c")
		attC := seedAttempt(t, d, execID, c.id, 1)
		if err := s.Suspend(context.Background(), SuspendRequest{
			ExecID: execID, NodeExecID: c.id, AttemptID: attC, WorkerID: "w1"}); err != nil {
			t.Fatalf("Suspend: %v", err)
		}
		b := readNode(t, d, execID, "b")
		mustComplete(t, s, execID, b.id, seedAttempt(t, d, execID, b.id, 1),
			engine.AttemptFailed, nil)
		if st := readNode(t, d, execID, "c").status; st != engine.NodeWaitingApproval {
			t.Fatalf("种子:c 应 WA 残留,got %s", st)
		}
		if e := readExec(t, d, execID); e.status != engine.WorkflowFailed {
			t.Fatalf("种子:wf 应 FAILED,got %s", e.status)
		}

		res, err := s.RollbackTo(context.Background(), RollbackParams{
			ExecID: execID, WorkerID: "api-rb", ActorID: "op", Quiesce: true})
		if err != nil {
			t.Fatalf("RollbackTo(quiesce WA): %v", err)
		}
		if res.TargetNodeID != "b" || res.QuiescedCount != 1 {
			t.Fatalf("res = %+v, want target=b quiesced=1", res)
		}
		// c:WA 楔停 → 收编重置 → advance 重派(approval 节点 EXECUTING)。
		if st := readNode(t, d, execID, "c").status; st != engine.NodeExecuting {
			t.Fatalf("c = %s, want EXECUTING(WA 楔停→重置→重派)", st)
		}
		// 审计锚:from=WAITING_APPROVAL(Append-only 来源不错记)。
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM audit_pending
		  WHERE execution_id=? AND event_type='node.state_changed'
		    AND JSON_UNQUOTE(JSON_EXTRACT(payload,'$.from'))='WAITING_APPROVAL'
		    AND JSON_UNQUOTE(JSON_EXTRACT(payload,'$.to'))='CANCELLED'`, execID).Scan(&n); err != nil {
			t.Fatalf("count WA wedge rows: %v", err)
		}
		if n != 1 {
			t.Fatalf("from=WAITING_APPROVAL to=CANCELLED = %d 行, want 1", n)
		}
		if e := readExec(t, d, execID); e.status != engine.WorkflowRunning {
			t.Fatalf("wf = %s, want RUNNING", e.status)
		}
	})

	t.Run("wedge_instance_residue", func(t *testing.T) {
		// 动态扇出 fail-fast:实例 0 终败 → 控制行随 fail-fast 收敛 FAILED、
		// wf FAILED,实例 1 EXECUTING 残留(守卫② 含实例行的锚);显式
		// target=fan(parallel 调度器内联,走重评估)。
		execID, instances := runDynToFanReady(t, s, d, dynDoc(), `{"topic": "x"}`, preOut(`["a","b"]`))
		if len(instances) != 2 {
			t.Fatalf("instances = %d, want 2", len(instances))
		}
		attInst1 := seedAttempt(t, d, execID, instances[1], 1)
		mustComplete(t, s, execID, instances[0], seedAttempt(t, d, execID, instances[0], 1),
			engine.AttemptFailed, nil)
		if st := instanceRowOf(t, d, execID, "fan", 2).status; st != engine.NodeExecuting {
			t.Fatalf("种子:实例 1 应 EXECUTING 残留,got %s", st)
		}

		res, err := s.RollbackTo(context.Background(), RollbackParams{
			ExecID: execID, TargetNodeID: "fan", WorkerID: "api-rb", ActorID: "op", Quiesce: true})
		if err != nil {
			t.Fatalf("RollbackTo(quiesce instances): %v", err)
		}
		// 楔面 = 实例 1 恰一行(控制行已 FAILED 走收编谓词,不在楔面)。
		if res.TargetNodeID != "fan" || res.QuiescedCount != 1 {
			t.Fatalf("res = %+v, want target=fan quiesced=1(实例 1 残行)", res)
		}
		var status, code string
		if err := d.QueryRow(`SELECT status, error_code FROM node_attempt WHERE id=?`, attInst1).
			Scan(&status, &code); err != nil {
			t.Fatalf("read attempt: %v", err)
		}
		if status != "DEAD" || code != "rollback_quiesce" {
			t.Fatalf("实例 attempt = %s/%q, want DEAD/rollback_quiesce", status, code)
		}
		// 行集原子性:控制行重置 ⇒ 整组重跑(无 SUCCEEDED 保活面)。
		for i := uint16(1); i <= 2; i++ {
			if st := instanceRowOf(t, d, execID, "fan", i).status; st != engine.NodeExecuting {
				t.Errorf("实例 %d = %s, want EXECUTING(收编整组重跑)", i, st)
			}
		}
		if e := readExec(t, d, execID); e.status != engine.WorkflowRunning {
			t.Fatalf("wf = %s, want RUNNING", e.status)
		}
	})

	t.Run("early_converged_fan_keeps_evidence_semantics", func(t *testing.T) {
		// 对抗审计 M1/M2 锚:min_success=1 fail_fast 早收敛(fan 控制器
		// SUCCEEDED + 实例 2 在飞)× 兄弟 sib 终败 wf FAILED——楔停实例 2 后
		// 闭包归属 = 步骤 5 谓词(fan SUCCEEDED 不收编):fan 行集与下游
		// post/tail 零重置、楔停行 = CANCELLED 证据行(与 FAILED 证据行
		// 同判)。变异(addDown 追加收编)下 fan 入闭包 → whole-keep 保住
		// 行集但**独立下游 tail 被拖入重置** → tail 状态断言红(变异 B
		// 实证:无独立下游的形状对该变异盲,T19.5 闸恰好吸收行集内 collateral)。
		execID, instances := runDynToFanReady(t, s, d, rbQuiesceEarlyDoc, `{"topic": "x"}`, preOut(`["a","b"]`))
		if len(instances) != 2 {
			t.Fatalf("instances = %d, want 2", len(instances))
		}
		// 实例 1 成功 → gatherCheck(min_success=1)立即收敛:控制器
		// SUCCEEDED、实例 2 留在飞(无真 worker,不完成即滞留);post/tail
		// 随 fan 终态派发。
		mustComplete(t, s, execID, instances[0], seedAttempt(t, d, execID, instances[0], 1),
			engine.AttemptSucceed, json.RawMessage(`{"verdict":"v1"}`))
		if st := instanceRowOf(t, d, execID, "fan", 0).status; st != engine.NodeSucceeded {
			t.Fatalf("种子:fan 控制器应 SUCCEEDED(早收敛),got %s", st)
		}
		// 下游先收敛(全 SUCCEEDED 后再引失败源,残面 = 实例 2 恰一行)。
		if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed,
			json.RawMessage(`{"done":true}`)); err != nil {
			t.Fatalf("complete post: %v", err)
		}
		if err := completeNode(t, s, d, execID, "tail", engine.AttemptSucceed,
			json.RawMessage(`{"text":"vt"}`)); err != nil {
			t.Fatalf("complete tail: %v", err)
		}
		if err := completeNode(t, s, d, execID, "sib", engine.AttemptFailed, nil); err != nil {
			t.Fatalf("complete sib(失败源): %v", err)
		}
		if e := readExec(t, d, execID); e.status != engine.WorkflowFailed {
			t.Fatalf("种子:wf 应 FAILED,got %s", e.status)
		}

		res, err := s.RollbackTo(context.Background(), RollbackParams{
			ExecID: execID, TargetNodeID: "sib", WorkerID: "api-rb", ActorID: "op", Quiesce: true})
		if err != nil {
			t.Fatalf("RollbackTo(quiesce early-converged): %v", err)
		}
		if res.QuiescedCount != 1 {
			t.Fatalf("quiesced_count = %d, want 1(实例 2 残行)", res.QuiescedCount)
		}
		// fan 行集证据保留(闭包外零重置;addDown 变异红点在下游 tail)。
		if st := instanceRowOf(t, d, execID, "fan", 0).status; st != engine.NodeSucceeded {
			t.Fatalf("fan 控制器 = %s, want SUCCEEDED(证据保留)", st)
		}
		if st := instanceRowOf(t, d, execID, "fan", 1).status; st != engine.NodeSucceeded {
			t.Fatalf("实例 1 = %s, want SUCCEEDED(证据保留)", st)
		}
		if st := instanceRowOf(t, d, execID, "fan", 2).status; st != engine.NodeCancelled {
			t.Fatalf("实例 2 = %s, want CANCELLED(楔停证据行)", st)
		}
		// 独立下游(非 target 路径)零连带:tail SUCCEEDED 保持 = addDown
		// 变异的直接杀点;post 同判。
		for _, id := range []string{"post", "tail"} {
			if st := readNode(t, d, execID, id).status; st != engine.NodeSucceeded {
				t.Fatalf("%s = %s, want SUCCEEDED(输入未变下游零连带重跑)", id, st)
			}
		}
		// wf 复活;收敛性:sib 重跑成功 → finishIfDone 对 CANCELLED 实例
		// 证据行容忍(T19.1 实例容忍)→ 终态可达。
		if e := readExec(t, d, execID); e.status != engine.WorkflowRunning {
			t.Fatalf("wf = %s, want RUNNING", e.status)
		}
		if err := completeNode(t, s, d, execID, "sib", engine.AttemptSucceed,
			json.RawMessage(`{"text":"vs"}`)); err != nil {
			t.Fatalf("complete sib(重跑): %v", err)
		}
		if e := readExec(t, d, execID); e.status != engine.WorkflowSucceeded {
			t.Fatalf("wf = %s, want SUCCEEDED(CANCELLED 证据行容忍下收敛)", e.status)
		}
	})

	t.Run("explicit_target_on_residue_takes_rerun", func(t *testing.T) {
		execID := rbStart(t, s, d, rbDiamondDSL, `{"q":"hi"}`)
		a := readNode(t, d, execID, "a")
		mustComplete(t, s, execID, a.id, seedAttempt(t, d, execID, a.id, 1),
			engine.AttemptSucceed, json.RawMessage(`{"text":"va"}`))
		b := readNode(t, d, execID, "b")
		mustComplete(t, s, execID, b.id, seedAttempt(t, d, execID, b.id, 1),
			engine.AttemptFailed, nil)
		c := readNode(t, d, execID, "c")
		seedAttempt(t, d, execID, c.id, 1)
		oldSnap := c.snapshot

		// 显式 target = 残留行:楔停后 CANCELLED ∈ Terminal ⇒ 走 RerunNode
		// 快照读回(CancelNode 不清快照),attempts+1。
		res, err := s.RollbackTo(context.Background(), RollbackParams{
			ExecID: execID, TargetNodeID: "c", WorkerID: "api-rb", ActorID: "op", Quiesce: true})
		if err != nil {
			t.Fatalf("RollbackTo(quiesce target=c): %v", err)
		}
		if res.TargetNodeID != "c" || !res.RerunTarget || res.QuiescedCount != 1 {
			t.Fatalf("res = %+v, want target=c rerun=true quiesced=1", res)
		}
		c2 := readNode(t, d, execID, "c")
		if c2.status != engine.NodeExecuting || c2.attempts != 2 {
			t.Fatalf("c = %+v, want EXECUTING attempts=2(残行 rerun)", c2)
		}
		if !c2.snapshot.Valid || c2.snapshot.String != oldSnap.String {
			t.Fatalf("c 快照应逐字读回:got %v want %v", c2.snapshot, oldSnap)
		}
		// b:失败源收编重置重派;d:冻结 PENDING 等待。
		if b2 := readNode(t, d, execID, "b"); b2.status != engine.NodeExecuting {
			t.Fatalf("b = %s, want EXECUTING(收编重派)", b2.status)
		}
		if d2 := readNode(t, d, execID, "d"); d2.status != engine.NodePending {
			t.Fatalf("d = %s, want PENDING", d2.status)
		}
	})

	t.Run("zero_residue_is_noop", func(t *testing.T) {
		execID, _, _ := rbFailChain(t, s, d)
		res, err := s.RollbackTo(context.Background(), RollbackParams{
			ExecID: execID, TargetNodeID: "b", WorkerID: "api-rb", ActorID: "op", Quiesce: true})
		if err != nil {
			t.Fatalf("RollbackTo(quiesce noop): %v", err)
		}
		if res.TargetNodeID != "b" || !res.RerunTarget || res.QuiescedCount != 0 {
			t.Fatalf("res = %+v, want target=b rerun=true quiesced=0", res)
		}
		if e := readExec(t, d, execID); e.status != engine.WorkflowRunning {
			t.Fatalf("wf = %s, want RUNNING", e.status)
		}
	})
}

// TestRollbackQuiesceAuditPayload:rolled_back payload 的 quiesced_count 锚
// (观测面机器判据;缺字段 = 恒真测试)。
func TestRollbackQuiesceAuditPayload(t *testing.T) {
	s, d := newSched(t)
	execID := rbStart(t, s, d, rbDiamondDSL, `{"q":"hi"}`)
	a := readNode(t, d, execID, "a")
	mustComplete(t, s, execID, a.id, seedAttempt(t, d, execID, a.id, 1),
		engine.AttemptSucceed, json.RawMessage(`{"text":"va"}`))
	b := readNode(t, d, execID, "b")
	mustComplete(t, s, execID, b.id, seedAttempt(t, d, execID, b.id, 1),
		engine.AttemptFailed, nil)
	c := readNode(t, d, execID, "c")
	seedAttempt(t, d, execID, c.id, 1)

	if _, err := s.RollbackTo(context.Background(), RollbackParams{
		ExecID: execID, TargetNodeID: "b", WorkerID: "api-rb", ActorID: "op", Quiesce: true}); err != nil {
		t.Fatalf("RollbackTo(quiesce): %v", err)
	}
	var payload []byte
	if err := d.QueryRow(`SELECT payload FROM audit_log
	  WHERE execution_id=? AND event_type='execution.rolled_back'`, execID).Scan(&payload); err != nil {
		t.Fatalf("read rolled_back payload: %v", err)
	}
	var p struct {
		TargetNodeID  string `json:"target_node_id"`
		ResetCount    int    `json:"reset_count"`
		QuiescedCount int    `json:"quiesced_count"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	// target=b 走 Rerun 分流不计 reset;c 楔停后重置恰一行。
	if p.TargetNodeID != "b" || p.ResetCount != 1 || p.QuiescedCount != 1 {
		t.Fatalf("payload = %+v, want target=b reset=1 quiesced=1", p)
	}
}

// containsBlockedPrefix 报告 blocked 清单是否含指定前缀的行。
func containsBlockedPrefix(blocked []string, prefix string) bool {
	for _, x := range blocked {
		if len(x) >= len(prefix) && x[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}
