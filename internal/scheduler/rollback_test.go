package scheduler

// T9.2 回滚/断点续跑测试(D-2026-09-28-t92-rollback-same-branch):
// 守卫矩阵(not_rollbackable / blocked_inflight / bad_node_id / no_resume_target)
// × 目标重跑(RerunNode 快照读回)× 失败源收编(闭包自洽,finishIfDone 僵尸
// 的 CRITICAL 回归)× SKIPPED 重评估 × 自动定位 × RUNNING attempt 同事务收口
// × 新一轮满额预算 × 并发双回滚。
//
// 变异自查(实现错 → 哪条红):
//   - 闭包不含失败源 → 收编子测试 wf RUNNING 永滞(finishIfDone failed>0
//     恒 no-op 的 CRITICAL 形态),断言 wf 状态红;
//   - RestartExecution 晚于 advance → 重派即败场景 wf 复活成僵尸,
//     input_eval 子测试红;
//   - RerunNode 不写 attempts_base → 预算子测试第二轮即 FailNode 红;
//   - RUNNING attempt 不收口 → 收口子测试 attempt 状态红。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"braid/internal/engine"
)

// rbChainDSL:线性 a→b→c(max_attempts 缺省 1)。
const rbChainDSL = `{"dsl":"1.0","name":"rb-chain",
 "edges":[{"from":"a","to":"b"},{"from":"b","to":"c"}],
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"a"},"outputs":{"text":"string"}},
  {"id":"b","type":"llm","prompt":{"template":"b","user_data":[{"name":"v","ref":"{{nodes.a.output.text}}"}]},"outputs":{"text":"string"}},
  {"id":"c","type":"llm","prompt":{"template":"c","user_data":[{"name":"v","ref":"{{nodes.b.output.text}}"}]},"outputs":{"text":"string"}}],
 "outputs":{"done":"{{nodes.c.output.text}}"}}`

// rbRetryDSL:a max_attempts=2(预算面:一轮耗尽 → 回滚 → 新一轮满额)。
const rbRetryDSL = `{"dsl":"1.0","name":"rb-retry",
 "edges":[{"from":"a","to":"b"}],
 "nodes":[
  {"id":"a","type":"llm","retry":{"max_attempts":2},"prompt":{"template":"x"},"outputs":{"text":"string"}},
  {"id":"b","type":"llm","prompt":{"template":"b","user_data":[{"name":"v","ref":"{{nodes.a.output.text}}"}]},"outputs":{"text":"string"}}]}`

// rbBranchDSL:分支 a→br→(c|d),臂不相交(D-t24 #2:branch 臂不可汇合——
// 汇合节点会被剪枝传播跳过,造不出 FAILED 种子);取路臂 c→e(e 可终败)。
const rbBranchDSL = `{"dsl":"1.0","name":"rb-branch",
 "edges":[{"from":"a","to":"br"},{"from":"br","to":"c"},{"from":"br","to":"d"},
          {"from":"c","to":"e"},{"from":"d","to":"f"}],
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"gen"},"outputs":{"flag":"boolean","text":"string"}},
  {"id":"br","type":"branch","cases":[{"when":{"left":"{{nodes.a.output.flag}}","op":"eq","right":true},"next":"c"}],"default":"d"},
  {"id":"c","type":"llm","prompt":{"template":"c","user_data":[{"name":"v","ref":"{{nodes.a.output.text}}"}]},"outputs":{"text":"string"}},
  {"id":"d","type":"llm","prompt":{"template":"d"},"outputs":{"text":"string"}},
  {"id":"e","type":"llm","prompt":{"template":"e","user_data":[{"name":"v","ref":"{{nodes.c.output.text}}"}]},"outputs":{"text":"string"}},
  {"id":"f","type":"llm","prompt":{"template":"f","user_data":[{"name":"v","ref":"{{nodes.d.output.text}}"}]},"outputs":{"text":"string"}}],
 "outputs":{"taken":"{{nodes.c.output.text}}","cut":"{{nodes.d.output.text}}"}}`

// rbDiamondDSL:菱形 a→(b,c)→d(收编面:兄弟支失败,目标为汇聚点)。
const rbDiamondDSL = `{"dsl":"1.0","name":"rb-diamond",
 "edges":[{"from":"a","to":"b"},{"from":"a","to":"c"},{"from":"b","to":"d"},{"from":"c","to":"d"}],
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"a"},"outputs":{"text":"string"}},
  {"id":"b","type":"llm","prompt":{"template":"b","user_data":[{"name":"v","ref":"{{nodes.a.output.text}}"}]},"outputs":{"text":"string"}},
  {"id":"c","type":"llm","prompt":{"template":"c","user_data":[{"name":"v","ref":"{{nodes.a.output.text}}"}]},"outputs":{"text":"string"}},
  {"id":"d","type":"llm","prompt":{"template":"d","user_data":[{"name":"v","ref":"{{nodes.b.output.text}}"},{"name":"w","ref":"{{nodes.c.output.text}}"}]},"outputs":{"text":"string"}}]}`

// rbStart 物化并起跑,返回 execID。
func rbStart(t *testing.T, s *Scheduler, d *sql.DB, doc, input string) uint64 {
	t.Helper()
	_, execID := inst(t, s, d, doc, input)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return execID
}

// rbFailChain 把 a→b→c 链推进到 b 终败(a SUCCEEDED、b FAILED、c 冻结 PENDING、
// wf FAILED),返回 b 的行 id 与旧轮快照(重跑读回断言的锚)。
func rbFailChain(t *testing.T, s *Scheduler, d *sql.DB) (execID, bNodeID uint64, oldSnap sql.NullString) {
	t.Helper()
	execID = rbStart(t, s, d, rbChainDSL, `{"q":"hi"}`)
	a := readNode(t, d, execID, "a")
	attA := seedAttempt(t, d, execID, a.id, 1)
	mustComplete(t, s, execID, a.id, attA, engine.AttemptSucceed, json.RawMessage(`{"text":"va"}`))
	b := readNode(t, d, execID, "b")
	attB := seedAttempt(t, d, execID, b.id, 1)
	mustComplete(t, s, execID, b.id, attB, engine.AttemptFailed, nil)
	b = readNode(t, d, execID, "b")
	if b.status != engine.NodeFailed {
		t.Fatalf("种子:b 应 FAILED,got %s", b.status)
	}
	if e := readExec(t, d, execID); e.status != engine.WorkflowFailed {
		t.Fatalf("种子:wf 应 FAILED,got %s", e.status)
	}
	return execID, b.id, b.snapshot
}

func wantRollbackErr(t *testing.T, err error, kind RollbackErrKind) {
	t.Helper()
	var re *RollbackError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want *RollbackError(%s)", err, kind)
	}
	if re.Kind != kind {
		t.Fatalf("kind = %s, want %s (detail %s)", re.Kind, kind, re.Detail)
	}
}

// wantRollbackErrBlocked 是 blocked_inflight 的载荷断言变体(阻塞清单面)。
func wantRollbackErrBlocked(t *testing.T, err error) *RollbackError {
	t.Helper()
	wantRollbackErr(t, err, RollbackBlockedInflight)
	var re *RollbackError
	if !errors.As(err, &re) {
		t.Fatalf("unreachable: kind checked above")
	}
	return re
}

func TestRollbackGuards(t *testing.T) {
	s, d := newSched(t)

	t.Run("not_rollbackable: RUNNING", func(t *testing.T) {
		execID := rbStart(t, s, d, rbChainDSL, `{}`)
		_, err := s.RollbackTo(context.Background(), RollbackParams{
			ExecID: execID, TargetNodeID: "b", WorkerID: "api-rb", ActorID: "op"})
		wantRollbackErr(t, err, RollbackNotRollbackable)
	})

	t.Run("not_rollbackable: SUCCEEDED", func(t *testing.T) {
		execID := rbStart(t, s, d, rbChainDSL, `{"q":"hi"}`)
		a := readNode(t, d, execID, "a")
		mustComplete(t, s, execID, a.id, seedAttempt(t, d, execID, a.id, 1),
			engine.AttemptSucceed, json.RawMessage(`{"text":"x"}`))
		b := readNode(t, d, execID, "b")
		mustComplete(t, s, execID, b.id, seedAttempt(t, d, execID, b.id, 1),
			engine.AttemptSucceed, json.RawMessage(`{"text":"y"}`))
		c := readNode(t, d, execID, "c")
		mustComplete(t, s, execID, c.id, seedAttempt(t, d, execID, c.id, 1),
			engine.AttemptSucceed, json.RawMessage(`{"text":"z"}`))
		if e := readExec(t, d, execID); e.status != engine.WorkflowSucceeded {
			t.Fatalf("种子:wf 应 SUCCEEDED,got %s", e.status)
		}
		_, err := s.RollbackTo(context.Background(), RollbackParams{
			ExecID: execID, TargetNodeID: "b", WorkerID: "api-rb", ActorID: "op"})
		wantRollbackErr(t, err, RollbackNotRollbackable)
	})

	t.Run("blocked_inflight: EXECUTING 兄弟残留", func(t *testing.T) {
		execID := rbStart(t, s, d, rbDiamondDSL, `{"q":"hi"}`)
		a := readNode(t, d, execID, "a")
		mustComplete(t, s, execID, a.id, seedAttempt(t, d, execID, a.id, 1),
			engine.AttemptSucceed, json.RawMessage(`{"text":"va"}`))
		b := readNode(t, d, execID, "b")
		mustComplete(t, s, execID, b.id, seedAttempt(t, d, execID, b.id, 1),
			engine.AttemptFailed, nil) // fail-fast:wf FAILED,c 仍 EXECUTING
		_, err := s.RollbackTo(context.Background(), RollbackParams{
			ExecID: execID, TargetNodeID: "b", WorkerID: "api-rb", ActorID: "op"})
		re := wantRollbackErrBlocked(t, err)
		found := false
		for _, x := range re.Blocked {
			if strings.HasPrefix(x, "c:") {
				found = true
			}
		}
		if !found {
			t.Fatalf("blocked 清单应含 c,got %v", re.Blocked)
		}
	})

	t.Run("bad_node_id: 不在钉住 DSL", func(t *testing.T) {
		execID, _, _ := rbFailChain(t, s, d)
		_, err := s.RollbackTo(context.Background(), RollbackParams{
			ExecID: execID, TargetNodeID: "nope", WorkerID: "api-rb", ActorID: "op"})
		wantRollbackErr(t, err, RollbackBadNode)
	})

	t.Run("bad_node_id: 闭包外 SKIPPED 祖先(剪枝臂下目标)", func(t *testing.T) {
		execID := rbStart(t, s, d, rbBranchDSL, `{}`)
		a := readNode(t, d, execID, "a")
		mustComplete(t, s, execID, a.id, seedAttempt(t, d, execID, a.id, 1),
			engine.AttemptSucceed, json.RawMessage(`{"flag":true,"text":"va"}`))
		// flag=true → 路由 c;d/f 剪枝传播;e 随 c 派发后终败 → wf FAILED。
		c := readNode(t, d, execID, "c")
		mustComplete(t, s, execID, c.id, seedAttempt(t, d, execID, c.id, 1),
			engine.AttemptSucceed, json.RawMessage(`{"text":"vc"}`))
		e := readNode(t, d, execID, "e")
		mustComplete(t, s, execID, e.id, seedAttempt(t, d, execID, e.id, 1),
			engine.AttemptFailed, nil)
		if e := readExec(t, d, execID); e.status != engine.WorkflowFailed {
			t.Fatalf("种子:wf 应 FAILED,got %s", e.status)
		}
		// f 在剪枝臂 d 之下(d SKIPPED、闭包外)→ 剪枝传播将静默吃掉 f。
		_, err := s.RollbackTo(context.Background(), RollbackParams{
			ExecID: execID, TargetNodeID: "f", WorkerID: "api-rb", ActorID: "op"})
		wantRollbackErr(t, err, RollbackBadNode)
	})
}

func TestRollbackRerunTarget(t *testing.T) {
	s, d := newSched(t)
	execID, bID, oldSnap := rbFailChain(t, s, d)

	res, err := s.RollbackTo(context.Background(), RollbackParams{
		ExecID: execID, TargetNodeID: "b", WorkerID: "api-rb", ActorID: "op"})
	if err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	if res.TargetNodeID != "b" || !res.RerunTarget {
		t.Fatalf("res = %+v, want target b rerun", res)
	}
	// b:RerunNode 快照读回重跑(attempts+1、基线=旧轮总数、新租约)。
	b := readNode(t, d, execID, "b")
	if b.status != engine.NodeExecuting {
		t.Fatalf("b = %s, want EXECUTING", b.status)
	}
	if b.attempts != 2 {
		t.Fatalf("b attempts = %d, want 2(单调续号)", b.attempts)
	}
	if !b.snapshot.Valid || b.snapshot.String != oldSnap.String {
		t.Fatalf("b 快照应逐字读回:got %v want %v", b.snapshot, oldSnap)
	}
	if !b.workerID.Valid || b.workerID.String != "api-rb" {
		t.Fatalf("b worker = %v, want api-rb", b.workerID)
	}
	// c:冻结 PENDING 在闭包内但零动作(已是目标态);a:闭包外不动。
	if c := readNode(t, d, execID, "c"); c.status != engine.NodePending {
		t.Fatalf("c = %s, want PENDING", c.status)
	}
	if a := readNode(t, d, execID, "a"); a.status != engine.NodeSucceeded || a.attempts != 1 {
		t.Fatalf("a 闭包外应不动: %+v", a)
	}
	// wf:复活 RUNNING,终态列组清空。
	e := readExec(t, d, execID)
	if e.status != engine.WorkflowRunning || e.outputs.Valid || e.errorCode.Valid {
		t.Fatalf("wf = %+v, want RUNNING 清终态列", e)
	}
	// 审计:execution.rolled_back 直写 audit_log;重置/重跑行进暂存面。
	var rolled int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log
		WHERE execution_id=? AND event_type='execution.rolled_back'`, execID).Scan(&rolled); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	if rolled != 1 {
		t.Fatalf("execution.rolled_back 行 = %d, want 1", rolled)
	}
	var staged int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_pending
		WHERE execution_id=? AND event_type='node.state_changed'`, execID).Scan(&staged); err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if staged == 0 {
		t.Fatalf("重置/重跑应暂存 node.state_changed 行")
	}
	// outbox:目标重跑派发事件(att:<b>:2)。
	var att2 int
	if err := d.QueryRow(`SELECT COUNT(*) FROM outbox WHERE execution_id=? AND task_id=?`,
		execID, "att:"+u64str(bID)+":2").Scan(&att2); err != nil {
		t.Fatalf("outbox count: %v", err)
	}
	if att2 != 1 {
		t.Fatalf("att:<b>:2 事件 = %d, want 1", att2)
	}
}

func TestRollbackAnnexFailureSources(t *testing.T) {
	s, d := newSched(t)
	// 菱形:b 终败(fail-fast,c 在飞)→ c 迟到成功(termGuard 仍收口节点)
	// → d 冻结 PENDING。回滚目标 d:闭包必须收编失败源 b(否则 wf 复活后
	// finishIfDone failed>0 恒 no-op,僵尸 RUNNING——对抗攻击 CRITICAL 形态)。
	execID := rbStart(t, s, d, rbDiamondDSL, `{"q":"hi"}`)
	a := readNode(t, d, execID, "a")
	mustComplete(t, s, execID, a.id, seedAttempt(t, d, execID, a.id, 1),
		engine.AttemptSucceed, json.RawMessage(`{"text":"va"}`))
	b := readNode(t, d, execID, "b")
	mustComplete(t, s, execID, b.id, seedAttempt(t, d, execID, b.id, 1),
		engine.AttemptFailed, nil)
	c := readNode(t, d, execID, "c")
	mustComplete(t, s, execID, c.id, seedAttempt(t, d, execID, c.id, 1),
		engine.AttemptSucceed, json.RawMessage(`{"text":"vc"}`))

	res, err := s.RollbackTo(context.Background(), RollbackParams{
		ExecID: execID, TargetNodeID: "d", WorkerID: "api-rb", ActorID: "op"})
	if err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	if res.RerunTarget {
		t.Fatalf("d 为冻结 PENDING,应走重评估(非 Rerun)")
	}
	// b:收编 → 重置 → advance 重派(新鲜求值,前驱 a 终态不可变 ⇒ 值恒同)。
	b2 := readNode(t, d, execID, "b")
	if b2.status != engine.NodeExecuting || b2.attempts != 2 {
		t.Fatalf("b 应被收编重派: %+v", b2)
	}
	// d:闭包内保持 PENDING(等待 b 新轮完成)。
	if d := readNode(t, d, execID, "d"); d.status != engine.NodePending {
		t.Fatalf("d = %s, want PENDING", d.status)
	}
	if e := readExec(t, d, execID); e.status != engine.WorkflowRunning {
		t.Fatalf("wf = %s, want RUNNING(收编保证终态可达,非僵尸)", e.status)
	}
}

func TestRollbackSkippedReeval(t *testing.T) {
	s, d := newSched(t)
	execID := rbStart(t, s, d, rbBranchDSL, `{}`)
	a := readNode(t, d, execID, "a")
	mustComplete(t, s, execID, a.id, seedAttempt(t, d, execID, a.id, 1),
		engine.AttemptSucceed, json.RawMessage(`{"flag":true,"text":"va"}`))
	c := readNode(t, d, execID, "c")
	mustComplete(t, s, execID, c.id, seedAttempt(t, d, execID, c.id, 1),
		engine.AttemptSucceed, json.RawMessage(`{"text":"vc"}`))
	e := readNode(t, d, execID, "e")
	mustComplete(t, s, execID, e.id, seedAttempt(t, d, execID, e.id, 1),
		engine.AttemptFailed, nil)
	// 种子断言:d/f 已剪枝(臂不相交,无汇合拖累)。
	if dRow := readNode(t, d, execID, "d"); dRow.status != engine.NodeSkipped {
		t.Fatalf("种子:d 应 SKIPPED,got %s", dRow.status)
	}
	if fRow := readNode(t, d, execID, "f"); fRow.status != engine.NodeSkipped {
		t.Fatalf("种子:f 应 SKIPPED(剪枝传播),got %s", fRow.status)
	}

	// 回滚目标 = branch 节点 br:闭包含 br 全下游(c/d/e/f),branch 重跑重新路由。
	res, err := s.RollbackTo(context.Background(), RollbackParams{
		ExecID: execID, TargetNodeID: "br", WorkerID: "api-rb", ActorID: "op"})
	if err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	if res.RerunTarget {
		t.Fatalf("branch 是调度器内联类型,应走重评估")
	}
	// br:重置 PENDING → advance 内联路由再成功。
	if br := readNode(t, d, execID, "br"); br.status != engine.NodeSucceeded {
		t.Fatalf("br = %s, want SUCCEEDED(重路由内联)", br.status)
	}
	// c:重置重派(flag 仍 true → 再取 c 臂);d/f:确定性重剪枝(SKIPPED)。
	if c := readNode(t, d, execID, "c"); c.status != engine.NodeExecuting {
		t.Fatalf("c = %s, want EXECUTING(重派)", c.status)
	}
	if dRow := readNode(t, d, execID, "d"); dRow.status != engine.NodeSkipped {
		t.Fatalf("d = %s, want SKIPPED(确定性重剪枝)", dRow.status)
	}
	if fRow := readNode(t, d, execID, "f"); fRow.status != engine.NodeSkipped {
		t.Fatalf("f = %s, want SKIPPED(确定性重剪枝)", fRow.status)
	}
	if e := readExec(t, d, execID); e.status != engine.WorkflowRunning {
		t.Fatalf("wf = %s, want RUNNING", e.status)
	}
}

func TestRollbackAutoTargetAndNoTarget(t *testing.T) {
	s, d := newSched(t)

	t.Run("自动定位:字典序首个 FAILED", func(t *testing.T) {
		execID, _, _ := rbFailChain(t, s, d)
		res, err := s.RollbackTo(context.Background(), RollbackParams{
			ExecID: execID, WorkerID: "api-rb", ActorID: "op"})
		if err != nil {
			t.Fatalf("RollbackTo: %v", err)
		}
		if res.TargetNodeID != "b" || !res.RerunTarget {
			t.Fatalf("res = %+v, want auto b rerun", res)
		}
	})

	t.Run("无 FAILED 退化:前驱就绪的 PENDING 冻结行(input_eval 型终败)", func(t *testing.T) {
		execID := rbStart(t, s, d, evalFailDSL, `{}`)
		a := readNode(t, d, execID, "a")
		// a 声明 text 但产出不含 → b 的 input_eval 失败(节点留 PENDING)。
		mustComplete(t, s, execID, a.id, seedAttempt(t, d, execID, a.id, 1),
			engine.AttemptSucceed, json.RawMessage(`{"wrong":"shape"}`))
		if e := readExec(t, d, execID); e.status != engine.WorkflowFailed {
			t.Fatalf("种子:wf 应 FAILED(input_eval),got %s", e.status)
		}
		res, err := s.RollbackTo(context.Background(), RollbackParams{
			ExecID: execID, WorkerID: "api-rb", ActorID: "op"})
		if err != nil {
			t.Fatalf("RollbackTo: %v", err)
		}
		if res.TargetNodeID != "b" {
			t.Fatalf("res.TargetNodeID = %q, want b(就绪冻结行)", res.TargetNodeID)
		}
		// 重派即再败:advance 内 failExecution 需要 RUNNING→FAILED 边可达
		//(RestartExecution 先于 advance——否则 Conflict 被吞后僵尸 RUNNING)。
		if e := readExec(t, d, execID); e.status != engine.WorkflowFailed {
			t.Fatalf("wf = %s, want FAILED(如实终败,非僵尸)", e.status)
		}
	})

	t.Run("no_resume_target: 全终态无候选", func(t *testing.T) {
		execID := rbStart(t, s, d, rbChainDSL, `{"q":"hi"}`)
		// 全链成功后人工把 wf 置 FAILED(reaper PENDING→FAILED 型终局的镜像)。
		for _, id := range []string{"a", "b", "c"} {
			n := readNode(t, d, execID, id)
			mustComplete(t, s, execID, n.id, seedAttempt(t, d, execID, n.id, 1),
				engine.AttemptSucceed, json.RawMessage(`{"text":"v"}`))
		}
		if _, err := d.Exec(`UPDATE workflow_execution SET status='FAILED' WHERE id=?`, execID); err != nil {
			t.Fatalf("seed FAILED: %v", err)
		}
		_, err := s.RollbackTo(context.Background(), RollbackParams{
			ExecID: execID, WorkerID: "api-rb", ActorID: "op"})
		wantRollbackErr(t, err, RollbackNoTarget)
	})
}

func TestRollbackClosesRunningAttempts(t *testing.T) {
	s, d := newSched(t)
	execID, bID, _ := rbFailChain(t, s, d)
	// 模拟 scan③→scan① 两拍窗:节点已终态、attempt 行滞留 RUNNING。
	attID := seedAttempt(t, d, execID, bID, 2)

	if _, err := s.RollbackTo(context.Background(), RollbackParams{
		ExecID: execID, TargetNodeID: "b", WorkerID: "api-rb", ActorID: "op"}); err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	var status string
	var code sql.NullString
	if err := d.QueryRow(`SELECT status, error_code FROM node_attempt WHERE id=?`, attID).
		Scan(&status, &code); err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	if status != "DEAD" || code.String != "rollback_reset" {
		t.Fatalf("attempt = %s/%v, want DEAD/rollback_reset", status, code)
	}
}

func TestRollbackFreshBudget(t *testing.T) {
	s, d := newSched(t)
	execID := rbStart(t, s, d, rbRetryDSL, `{"q":"hi"}`)
	// 第一轮:max_attempts=2 → 两次失败耗尽(第二次走 Redispatch)。
	a := readNode(t, d, execID, "a")
	mustComplete(t, s, execID, a.id, seedAttempt(t, d, execID, a.id, 1), engine.AttemptFailed, nil)
	a = readNode(t, d, execID, "a")
	if a.status != engine.NodeExecuting || a.attempts != 2 {
		t.Fatalf("第一次失败后应 Redispatch(attempts=2): %+v", a)
	}
	mustComplete(t, s, execID, a.id, seedAttempt(t, d, execID, a.id, 2), engine.AttemptFailed, nil)
	a = readNode(t, d, execID, "a")
	if a.status != engine.NodeFailed {
		t.Fatalf("第二轮耗尽应 FailNode: %s", a.status)
	}
	if e := readExec(t, d, execID); e.status != engine.WorkflowFailed {
		t.Fatalf("wf = %s, want FAILED", e.status)
	}

	// 回滚 a(RerunNode:attempts=3、base=2 ⇒ 新一轮预算满额 2)。
	if _, err := s.RollbackTo(context.Background(), RollbackParams{
		ExecID: execID, TargetNodeID: "a", WorkerID: "api-rb", ActorID: "op"}); err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	a = readNode(t, d, execID, "a")
	if a.status != engine.NodeExecuting || a.attempts != 3 {
		t.Fatalf("回滚后 a: %+v", a)
	}
	// 新一轮第 1 次失败:3-2=1 < 2 ⇒ Redispatch(attempts=4),不是 FailNode。
	mustComplete(t, s, execID, a.id, seedAttempt(t, d, execID, a.id, 3), engine.AttemptFailed, nil)
	a = readNode(t, d, execID, "a")
	if a.status != engine.NodeExecuting || a.attempts != 4 {
		t.Fatalf("新一轮首次失败应 Redispatch(attempts=4): %+v", a)
	}
	// 第 2 次失败:4-2=2 ≮ 2 ⇒ FailNode,wf 再入 FAILED。
	mustComplete(t, s, execID, a.id, seedAttempt(t, d, execID, a.id, 4), engine.AttemptFailed, nil)
	a = readNode(t, d, execID, "a")
	if a.status != engine.NodeFailed {
		t.Fatalf("新一轮耗尽应 FailNode: %s", a.status)
	}
	if e := readExec(t, d, execID); e.status != engine.WorkflowFailed {
		t.Fatalf("wf = %s, want FAILED", e.status)
	}
}

func TestRollbackConcurrentDouble(t *testing.T) {
	s, d := newSched(t)
	execID, _, _ := rbFailChain(t, s, d)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.RollbackTo(context.Background(), RollbackParams{
				ExecID: execID, TargetNodeID: "b", WorkerID: "api-rb", ActorID: "op"})
		}(i)
	}
	wg.Wait()
	var okCount, conflictCount int
	for _, err := range errs {
		switch {
		case err == nil:
			okCount++
		case errors.Is(err, engine.ErrConflict):
			// 败者形态一:RestartExecution CAS 0 行(守卫读为陈旧 FAILED 快照,
			// 终裁在 CAS)。
			conflictCount++
		default:
			var re *RollbackError
			if errors.As(err, &re) && re.Kind == RollbackNotRollbackable {
				// 败者形态二:锁等待后守卫读到已提交的 RUNNING(当前读链路收敛)。
				conflictCount++
				continue
			}
			t.Fatalf("非预期错误: %v", err)
		}
	}
	if okCount != 1 || conflictCount != 1 {
		t.Fatalf("恰一胜一败: ok=%d conflict=%d errs=%v", okCount, conflictCount, errs)
	}
	if b := readNode(t, d, execID, "b"); b.attempts != 2 {
		t.Fatalf("双回滚并发后 b attempts = %d, want 2(恰一次重跑)", b.attempts)
	}
}

// u64str 是断言用 uint64 → string(outbox TaskID 拼接镜像)。
func u64str(v uint64) string { return strconv.FormatUint(v, 10) }

// TestRollbackAnnexCancelledOrphans:M6 锚——CANCELLED 连坐行(reaper ③ 收口
// 产物)在闭包内被收编重置重派;收编条件若缩成「只收 FAILED」,本测红
// (fail-fast 遗留 EXECUTING 兄弟被 reaper 取消后,回滚必须一并复活)。
func TestRollbackAnnexCancelledOrphans(t *testing.T) {
	s, d := newSched(t)
	execID := rbStart(t, s, d, rbDiamondDSL, `{"q":"hi"}`)
	a := readNode(t, d, execID, "a")
	mustComplete(t, s, execID, a.id, seedAttempt(t, d, execID, a.id, 1),
		engine.AttemptSucceed, json.RawMessage(`{"text":"va"}`))
	// b 终败(fail-fast);c 兄弟在飞 → wf FAILED 后人工收口为 CANCELLED
	//(reaper ③ 的连坐形态;快照保留——自 EXECUTING 取消)。
	b := readNode(t, d, execID, "b")
	mustComplete(t, s, execID, b.id, seedAttempt(t, d, execID, b.id, 1),
		engine.AttemptFailed, nil)
	c := readNode(t, d, execID, "c")
	if _, err := engine.CancelNode(context.Background(), d, c.id, c.version); err != nil {
		t.Fatalf("cancel c: %v", err)
	}
	if _, err := s.RollbackTo(context.Background(), RollbackParams{
		ExecID: execID, TargetNodeID: "d", WorkerID: "api-rb", ActorID: "op"}); err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	// c:CANCELLED → 收编 → advance 重派(fresh eval);d 保持 PENDING 等待。
	c2 := readNode(t, d, execID, "c")
	if c2.status != engine.NodeExecuting {
		t.Fatalf("c = %s, want EXECUTING(CANCELLED 连坐行收编重派)", c2.status)
	}
	if dRow := readNode(t, d, execID, "d"); dRow.status != engine.NodePending {
		t.Fatalf("d = %s, want PENDING", dRow.status)
	}
	if e := readExec(t, d, execID); e.status != engine.WorkflowRunning {
		t.Fatalf("wf = %s, want RUNNING", e.status)
	}
}

// TestRollbackAuditPayload:M7 锚——execution.rolled_back 载荷字段是「两条
// 路径按轮可追溯」的机器判据(决策 9),字段零断言 = 恒真测试。
func TestRollbackAuditPayload(t *testing.T) {
	s, d := newSched(t)
	execID, _, _ := rbFailChain(t, s, d)
	// 目标 = c(冻结 PENDING):失败源 b 被收编重置(reset_count 有牙)、
	// 目标本身走重评估(rerun_target=false)——两字段各锚一面。
	if _, err := s.RollbackTo(context.Background(), RollbackParams{
		ExecID: execID, TargetNodeID: "c", WorkerID: "api-rb", ActorID: "op"}); err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	var payload []byte
	if err := d.QueryRow(`SELECT payload FROM audit_log
	  WHERE execution_id=? AND event_type='execution.rolled_back'`, execID).Scan(&payload); err != nil {
		t.Fatalf("read rolled_back payload: %v", err)
	}
	var p struct {
		TargetNodeID string `json:"target_node_id"`
		FromStatus   string `json:"from_status"`
		ResetCount   int    `json:"reset_count"`
		RerunTarget  bool   `json:"rerun_target"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.TargetNodeID != "c" || p.FromStatus != "FAILED" || p.RerunTarget {
		t.Fatalf("payload = %+v, want target=c from=FAILED rerun=false", p)
	}
	if p.ResetCount != 1 {
		t.Fatalf("reset_count = %d, want 1(失败源 b 恰一行重置)", p.ResetCount)
	}
}
