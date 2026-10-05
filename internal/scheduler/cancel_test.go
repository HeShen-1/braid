package scheduler

// T19.3 执行级取消测试(D-2026-10-05-t193-execution-cancel-and-rollback-
// whitelist):楔源预扫(全部非终态行,控制行+实例行,同事务逐行 CancelNode)
// × wf CANCELLED 原子收口(零「全终态但 wf 未收口」窗口)× 受理面
// (RUNNING/WA;终态 not_cancellable,幂等重取消报错非重放)× 取消后
// 迟到收口竞态收敛(attempt DEAD 幂等 + 终态守卫跳推进)× 审计载荷 ×
// 容量闸(与回滚共闸 BRAID_ROLLBACK_MAX_ROWS / Deps.MaxRollbackRows)。
//
// 变异自查(实现错 → 哪条红):
//   - 只置 wf 不楔停行 → wedge_all_running 的节点状态断言红(且 reaper 孤儿
//     收口语义漂移);
//   - 楔停与 wf 收口拆两事务 → 窗口内 finishIfDone/reaper ④b 可误判
//     SUCCEEDED,wf 状态断言红;
//   - RUNNING attempt 不收口 → attempt DEAD 断言红(worker 完成面双跑);
//   - 终态放行 → not_cancellable 族红;
//   - CancelNode 谓词外状态(PENDING 冻结行)漏楔 → b/c 状态断言红。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"braid/internal/engine"
)

func TestCancelExecution(t *testing.T) {
	s, d := newSched(t)

	t.Run("wedge_all_running", func(t *testing.T) {
		execID := rbStart(t, s, d, rbChainDSL, `{"q":"hi"}`)
		a := readNode(t, d, execID, "a")
		if a.status != engine.NodeExecuting {
			t.Fatalf("种子:a 应 EXECUTING,got %s", a.status)
		}
		attA := seedAttempt(t, d, execID, a.id, 1)
		b := readNode(t, d, execID, "b")
		c := readNode(t, d, execID, "c")
		if b.status != engine.NodePending || c.status != engine.NodePending {
			t.Fatalf("种子:b/c 应 PENDING 冻结,got %s/%s", b.status, c.status)
		}

		res, err := s.CancelExecution(context.Background(), CancelParams{
			ExecID: execID, WorkerID: "api:cancel", ActorID: "op"})
		if err != nil {
			t.Fatalf("CancelExecution: %v", err)
		}
		if res.CancelledCount != 3 {
			t.Errorf("cancelled_count = %d, want 3(控制面非终态行恰三行)", res.CancelledCount)
		}
		if res.FromStatus != engine.WorkflowRunning {
			t.Errorf("from_status = %s, want RUNNING", res.FromStatus)
		}
		for _, id := range []string{"a", "b", "c"} {
			if st := readNode(t, d, execID, id).status; st != engine.NodeCancelled {
				t.Errorf("节点 %s = %s, want CANCELLED", id, st)
			}
		}
		if at := readAttempt(t, d, attA); at != engine.AttemptDead {
			t.Errorf("attempt a = %s, want DEAD(取消事务同事务收口)", at)
		}
		if e := readExec(t, d, execID); e.status != engine.WorkflowCancelled {
			t.Fatalf("wf = %s, want CANCELLED", e.status)
		}
	})

	t.Run("wedge_waiting_approval", func(t *testing.T) {
		execID := rbStart(t, s, d, rbChainDSL, `{"q":"hi"}`)
		// WA 来源判别:种态走既有 helper(engine.SuspendExecution);节点行
		// 停留 EXECUTING 的非契约形态仅作 wf 来源面——取消面只楔停非终态行,
		// 不依赖挂起契约形状(真审批挂起旅程见 wedge_real_approval)。
		if err := engine.SuspendExecution(context.Background(), d, execID); err != nil {
			t.Fatalf("种 WA: %v", err)
		}
		res, err := s.CancelExecution(context.Background(), CancelParams{
			ExecID: execID, WorkerID: "api:cancel", ActorID: "op"})
		if err != nil {
			t.Fatalf("CancelExecution(WA): %v", err)
		}
		if res.FromStatus != engine.WorkflowWaitingApproval {
			t.Errorf("from_status = %s, want WAITING_APPROVAL", res.FromStatus)
		}
		if e := readExec(t, d, execID); e.status != engine.WorkflowCancelled {
			t.Fatalf("wf = %s, want CANCELLED", e.status)
		}
		// 审计行 from_status 锚(对抗审计 C4:变异 FromStatus 硬编码 RUNNING
		// 须红——append-only 面的合规轨迹不能错记来源)。
		var payload []byte
		if err := d.QueryRow(`SELECT payload FROM audit_log
		  WHERE execution_id=? AND event_type='execution.cancelled'`, execID).Scan(&payload); err != nil {
			t.Fatalf("read cancelled audit row: %v", err)
		}
		var p struct {
			FromStatus     string `json:"from_status"`
			CancelledCount int    `json:"cancelled_count"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if p.FromStatus != "WAITING_APPROVAL" || p.CancelledCount != 3 {
			t.Fatalf("WA 取消 payload = %+v, want from=WAITING_APPROVAL count=3", p)
		}
	})

	t.Run("wedge_real_approval", func(t *testing.T) {
		// 真审批挂起旅程(对抗审计 C7):approval 节点真 Suspend(WA 契约形状:
		// 节点行 WAITING_APPROVAL + attempt SUCCEEDED 收口)→ 取消 → WA 行
		// 楔停 CANCELLED;此后再决定 = 非法边收敛(挂起已灭)。
		execID := rbStart(t, s, d, approvalDSL, `{"q":"hi"}`)
		a := readNode(t, d, execID, "a")
		mustComplete(t, s, execID, a.id, seedAttempt(t, d, execID, a.id, 1),
			engine.AttemptSucceed, json.RawMessage(`{"text":"va"}`))
		b := readNode(t, d, execID, "b") // approval
		attB := seedAttempt(t, d, execID, b.id, 1)
		if err := s.Suspend(context.Background(), SuspendRequest{
			ExecID: execID, NodeExecID: b.id, AttemptID: attB, WorkerID: "w1"}); err != nil {
			t.Fatalf("Suspend: %v", err)
		}
		if st := readNode(t, d, execID, "b").status; st != engine.NodeWaitingApproval {
			t.Fatalf("种子:b 应 WAITING_APPROVAL,got %s", st)
		}
		if e := readExec(t, d, execID); e.status != engine.WorkflowWaitingApproval {
			t.Fatalf("种子:wf 应 WAITING_APPROVAL,got %s", e.status)
		}
		if _, err := s.CancelExecution(context.Background(), CancelParams{
			ExecID: execID, WorkerID: "api:cancel", ActorID: "op"}); err != nil {
			t.Fatalf("CancelExecution(approval WA): %v", err)
		}
		if st := readNode(t, d, execID, "b").status; st != engine.NodeCancelled {
			t.Fatalf("取消后 b = %s, want CANCELLED(WA 行照楔)", st)
		}
		if e := readExec(t, d, execID); e.status != engine.WorkflowCancelled {
			t.Fatalf("wf = %s, want CANCELLED", e.status)
		}
	})

	t.Run("not_cancellable: 终态三族 + PENDING", func(t *testing.T) {
		// FAILED
		execID, _, _ := rbFailChain(t, s, d)
		_, err := s.CancelExecution(context.Background(), CancelParams{
			ExecID: execID, WorkerID: "api:cancel", ActorID: "op"})
		wantCancelErr(t, err, CancelNotCancellable)

		// PENDING(Start 积压窗;对抗审计 C2:不受理以保 Start 锁序例外)。
		defID, pendingID := inst(t, s, d, rbChainDSL, `{"q":"hi"}`)
		_ = defID
		_, err = s.CancelExecution(context.Background(), CancelParams{
			ExecID: pendingID, WorkerID: "api:cancel", ActorID: "op"})
		wantCancelErr(t, err, CancelNotCancellable)

		// SUCCEEDED:全链完成
		execID = rbStart(t, s, d, rbChainDSL, `{"q":"hi"}`)
		for _, id := range []string{"a", "b", "c"} {
			n := readNode(t, d, execID, id)
			if err := s.Complete(context.Background(), CompletionRequest{
				ExecID: execID, NodeExecID: n.id, AttemptID: seedAttempt(t, d, execID, n.id, 1),
				WorkerID: "w-test", Status: engine.AttemptSucceed, Output: json.RawMessage(`{"text":"v"}`),
			}); err != nil {
				t.Fatalf("种 SUCCEEDED(%s): %v", id, err)
			}
		}
		if e := readExec(t, d, execID); e.status != engine.WorkflowSucceeded {
			t.Fatalf("种子:wf 应 SUCCEEDED,got %s", e.status)
		}
		_, err = s.CancelExecution(context.Background(), CancelParams{
			ExecID: execID, WorkerID: "api:cancel", ActorID: "op"})
		wantCancelErr(t, err, CancelNotCancellable)

		// 双取消(幂等重取消报错非重放)
		execID = rbStart(t, s, d, rbChainDSL, `{"q":"hi"}`)
		if _, err := s.CancelExecution(context.Background(), CancelParams{
			ExecID: execID, WorkerID: "api:cancel", ActorID: "op"}); err != nil {
			t.Fatalf("首次取消: %v", err)
		}
		_, err = s.CancelExecution(context.Background(), CancelParams{
			ExecID: execID, WorkerID: "api:cancel", ActorID: "op"})
		wantCancelErr(t, err, CancelNotCancellable)
	})

	t.Run("complete_after_cancel_converges", func(t *testing.T) {
		execID := rbStart(t, s, d, rbChainDSL, `{"q":"hi"}`)
		a := readNode(t, d, execID, "a")
		attA := seedAttempt(t, d, execID, a.id, 1)
		if _, err := s.CancelExecution(context.Background(), CancelParams{
			ExecID: execID, WorkerID: "api:cancel", ActorID: "op"}); err != nil {
			t.Fatalf("取消: %v", err)
		}
		// 取消后在飞 worker 迟到报成功:attempt 已 DEAD ⇒ CloseAttempt
		// Conflict ⇒ 幂等哨兵被 Complete 层吞为 nil(worker 视角收口成功);
		// 节点已 CANCELLED ⇒ 终态守卫跳推进。牙在状态断言:若未走哨兵
		// (attempt 复活)或未跳推进,节点/wf 状态红。
		err := s.Complete(context.Background(), CompletionRequest{
			ExecID: execID, NodeExecID: a.id, AttemptID: attA, WorkerID: "w-test",
			Status: engine.AttemptSucceed, Output: json.RawMessage(`{"text":"late"}`),
		})
		if err != nil {
			t.Fatalf("迟到收口 err = %v, want nil(幂等哨兵)", err)
		}
		if st := readNode(t, d, execID, "a").status; st != engine.NodeCancelled {
			t.Errorf("迟到收口后 a = %s, want CANCELLED(终态守卫零推进)", st)
		}
		if e := readExec(t, d, execID); e.status != engine.WorkflowCancelled {
			t.Errorf("迟到收口后 wf = %s, want CANCELLED", e.status)
		}

		// 失败变体(对抗审计 L10):worker 迟到报失败走同一 DEAD 哨兵路径,
		// 收敛对称。
		execID = rbStart(t, s, d, rbChainDSL, `{"q":"hi"}`)
		a = readNode(t, d, execID, "a")
		attA = seedAttempt(t, d, execID, a.id, 1)
		if _, err := s.CancelExecution(context.Background(), CancelParams{
			ExecID: execID, WorkerID: "api:cancel", ActorID: "op"}); err != nil {
			t.Fatalf("取消(失败变体): %v", err)
		}
		if err := s.Complete(context.Background(), CompletionRequest{
			ExecID: execID, NodeExecID: a.id, AttemptID: attA, WorkerID: "w-test",
			Status: engine.AttemptFailed, Code: "late_boom",
		}); err != nil {
			t.Fatalf("迟到失败收口 err = %v, want nil(同哨兵对称)", err)
		}
		if st := readNode(t, d, execID, "a").status; st != engine.NodeCancelled {
			t.Errorf("迟到失败后 a = %s, want CANCELLED", st)
		}
		if e := readExec(t, d, execID); e.status != engine.WorkflowCancelled {
			t.Errorf("迟到失败后 wf = %s, want CANCELLED", e.status)
		}
	})

	t.Run("audit_payload", func(t *testing.T) {
		execID := rbStart(t, s, d, rbChainDSL, `{"q":"hi"}`)
		if _, err := s.CancelExecution(context.Background(), CancelParams{
			ExecID: execID, WorkerID: "api:cancel", ActorID: "op-1"}); err != nil {
			t.Fatalf("取消: %v", err)
		}
		var actor, payload []byte
		if err := d.QueryRow(`SELECT actor_id, payload FROM audit_log
		  WHERE execution_id=? AND event_type='execution.cancelled'`, execID).
			Scan(&actor, &payload); err != nil {
			t.Fatalf("read cancelled audit row: %v", err)
		}
		if string(actor) != "op-1" {
			t.Errorf("actor_id = %s, want op-1", actor)
		}
		var p struct {
			FromStatus     string `json:"from_status"`
			CancelledCount int    `json:"cancelled_count"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if p.FromStatus != "RUNNING" || p.CancelledCount != 3 {
			t.Fatalf("payload = %+v, want from=RUNNING count=3", p)
		}
		// 逐行楔停审计:node.state_changed from=非终态 to=CANCELLED 走暂存
		//(stageRollbackTransition = 回滚/取消共用的节点级 user 过渡单点,
		// 高频类暂存、flush 搬运——断言面在 audit_pending)。
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM audit_pending
		  WHERE execution_id=? AND event_type='node.state_changed'
		    AND JSON_UNQUOTE(JSON_EXTRACT(payload,'$.to'))='CANCELLED'`, execID).Scan(&n); err != nil {
			t.Fatalf("count cancelled node rows: %v", err)
		}
		if n != 3 {
			t.Errorf("node.state_changed→CANCELLED = %d 行, want 3", n)
		}
	})
}

// TestCancelRowCap:容量闸与回滚共闸(Deps.MaxRollbackRows 同数值)——取消
// 是全行锁事务,锁面随行数线性增长,与回滚同一容量护栏。
func TestCancelRowCap(t *testing.T) {
	t.Run("over_cap_denied", func(t *testing.T) {
		s, d := newCappedSched(t, 2) // 三行链 → 超
		execID := rbStart(t, s, d, rbChainDSL, `{"q":"hi"}`)
		_, err := s.CancelExecution(context.Background(), CancelParams{
			ExecID: execID, WorkerID: "api:cancel", ActorID: "op"})
		wantCancelErr(t, err, CancelTooLarge)
		if e := readExec(t, d, execID); e.status != engine.WorkflowRunning {
			t.Fatalf("闸拒绝后 wf = %s, want RUNNING(拒绝零副作用)", e.status)
		}
	})
	t.Run("zero_disables", func(t *testing.T) {
		s, d := newCappedSched(t, 0)
		execID := rbStart(t, s, d, rbChainDSL, `{"q":"hi"}`)
		if _, err := s.CancelExecution(context.Background(), CancelParams{
			ExecID: execID, WorkerID: "api:cancel", ActorID: "op"}); err != nil {
			t.Fatalf("0=关闭哨兵下取消: %v", err)
		}
	})
}

func wantCancelErr(t *testing.T, err error, kind CancelErrKind) {
	t.Helper()
	var ce *CancelError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *CancelError(%s)", err, kind)
	}
	if ce.Kind != kind {
		t.Fatalf("kind = %s, want %s (detail %s)", ce.Kind, kind, ce.Detail)
	}
}

// TestCancelDynamicFanout:T19.3 × T19.2(D-t192)联动——取消楔停含动态扇出
// 实例行(控制行+实例行全收);取消产生的 CANCELLED 执行经回滚 Reset 后由
// 下轮 reconcile 重裁(D-t192 决策 4 的取消侧兑现:盈余行 SKIPPED 保留证据,
// results 长度恒 = 当前轮 M)。
//
// 变异自查(实现错 → 哪条红):
//   - 楔源漏实例行(rows.instances)→ wedge_instances 子测实例行状态红;
//   - 取消的行集读取漏控制行 → 控制器状态红;
//   - 回滚闭包不收 CANCELLED 行 → shrink_then_regrow 子测 post/fan 滞留
//     CANCELLED,finishIfDone 永不收敛,wf 终态断言红;
//   - reconcile 不重裁 → 盈余行滞留 PENDING,状态断言红。
func TestCancelDynamicFanout(t *testing.T) {
	s, d := newSched(t)

	t.Run("wedge_instances", func(t *testing.T) {
		execID, instances := runDynToFanReady(t, s, d, dynDoc(), `{"topic": "x"}`, preOut(`["a","b","c"]`))
		if len(instances) != 3 {
			t.Fatalf("instances = %d, want 3", len(instances))
		}
		// 实例行 RUNNING attempt 种子(对抗审计 C3:真实形态下实例行 attempt
		// 在 worker 认领时物化——不种子的取消测试对 attempt 收口环零锚,
		// 变异「收口环滤掉实例行」整套件绿)。
		var instAtts []uint64
		for _, id := range instances {
			instAtts = append(instAtts, seedAttempt(t, d, execID, id, 1))
		}
		res, err := s.CancelExecution(context.Background(), CancelParams{
			ExecID: execID, WorkerID: "api:cancel", ActorID: "op"})
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		// 楔源 = pre 外全部行:fan 控制行 + 3 实例行 + post 冻结行 = 5。
		if res.CancelledCount != 5 {
			t.Errorf("cancelled_count = %d, want 5(控制行+3 实例行+post)", res.CancelledCount)
		}
		if st := instanceRowOf(t, d, execID, "fan", 0).status; st != engine.NodeCancelled {
			t.Errorf("fan 控制器 = %s, want CANCELLED", st)
		}
		for i := uint16(1); i <= 3; i++ {
			if st := instanceRowOf(t, d, execID, "fan", i).status; st != engine.NodeCancelled {
				t.Errorf("fan 实例 %d = %s, want CANCELLED", i, st)
			}
		}
		// 实例行 attempt 收口锚:DEAD + code(对抗审计 C3/L8)。
		for j, attID := range instAtts {
			if at := readAttempt(t, d, attID); at != engine.AttemptDead {
				t.Errorf("实例 %d attempt = %s, want DEAD(取消事务收口实例行 attempt)", j+1, at)
			}
			var code string
			if err := d.QueryRow(`SELECT error_code FROM node_attempt WHERE id=?`, attID).Scan(&code); err != nil || code != "execution_cancelled" {
				t.Errorf("实例 %d attempt code = %q(%v), want execution_cancelled", j+1, code, err)
			}
		}
		if e := readExec(t, d, execID); e.status != engine.WorkflowCancelled {
			t.Fatalf("wf = %s, want CANCELLED", e.status)
		}
	})

	t.Run("resume_targets_wedge_then_reconcile_shrink", func(t *testing.T) {
		// 取消(3 实例)→ resume(楔源自动定位)→ 闭包 Reset(全 CANCELLED 行
		// 回 PENDING)→ advance 重派:pre 闭包外保持 SUCCEEDED,list 不变 ⇒
		// reconcile 零补插零盈余,3 实例整组重跑;完成全旅程收敛 SUCCEEDED。
		execID, _ := runDynToFanReady(t, s, d, dynDoc(), `{"topic": "x"}`, preOut(`["a","b","c"]`))
		if _, err := s.CancelExecution(context.Background(), CancelParams{
			ExecID: execID, WorkerID: "api:cancel", ActorID: "op"}); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		res, err := s.RollbackTo(context.Background(), RollbackParams{
			ExecID: execID, WorkerID: "api-rb", ActorID: "op"})
		if err != nil {
			t.Fatalf("resume cancelled: %v", err)
		}
		// 节点字典序 fan < post < pre ⇒ 楔源 = fan 控制行;fan 是 parallel
		// (调度器内联)⇒ 走 Reset 重评估,advance 重派。
		if res.TargetNodeID != "fan" || res.RerunTarget {
			t.Fatalf("res = %+v, want target=fan rerun=false", res)
		}
		for i := 1; i <= 3; i++ {
			if st := instanceRowOf(t, d, execID, "fan", uint16(i)).status; st != engine.NodeExecuting {
				t.Errorf("回滚后实例 %d = %s, want EXECUTING(reconcile 零盈余整组重跑)", i, st)
			}
		}
		for i := 1; i <= 3; i++ {
			row := instanceRowOf(t, d, execID, "fan", uint16(i))
			attemptID := seedAttempt(t, d, execID, row.id, 1)
			mustComplete(t, s, execID, row.id, attemptID, engine.AttemptSucceed,
				json.RawMessage(fmt.Sprintf(`{"verdict":"v%d"}`, i)))
		}
		if err := completeNode(t, s, d, execID, "post", engine.AttemptSucceed,
			json.RawMessage(`{"done":true}`)); err != nil {
			t.Fatalf("complete post: %v", err)
		}
		if e := readExec(t, d, execID); e.status != engine.WorkflowSucceeded {
			t.Fatalf("wf = %s, want SUCCEEDED(取消→回滚→重跑全旅程收敛)", e.status)
		}
		var results sql.NullString
		if err := d.QueryRow(`SELECT output FROM node_execution WHERE execution_id=? AND node_id='fan' AND instance_no=0`,
			execID).Scan(&results); err != nil || !results.Valid {
			t.Fatalf("fan 控制行 results: %v(%v)", results, err)
		}
		if got := countJSONNulls(t, results.String); got != 0 {
			t.Errorf("results 含 %d 个 null 位,want 0(全部活跃实例收敛)", got)
		}
	})
}

// countJSONNulls 统计控制行 output 里 results 数组的顶层 null 元素数
// (盈余位混入 results 的探针——D-t192 决策 4「results 长度恒 = 当前轮 M,
// 盈余位不进」)。output 形如 {"results":[...]}。
func countJSONNulls(t *testing.T, raw string) int {
	t.Helper()
	var obj struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatalf("decode results %q: %v", raw, err)
	}
	n := 0
	for _, e := range obj.Results {
		if string(e) == "null" {
			n++
		}
	}
	return n
}
