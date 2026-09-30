package api

// T14.2 会签(mode=all)契约面:部分批准中间态(落行+审计、零 resume
// 事件、节点保持 WAITING)、终票定局(全员 approve 或任一 reject 落 resume
// 事件)、定局后续票 409、重放/改判语义不随 mode 变化、GET 详情
// pending_approvers(t14.1 画布数据契约)、待办列表排除本轮已投者。
// 判据单点 = scheduler.ApprovalOutcome(D-2026-09-30-t142-and-approval)。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"braid/internal/audit"
	"braid/internal/auth"
	"braid/internal/scheduler"
)

// TestApprovalAllPartialApproveWaits:首票 approve = 中间态——201(无 final)
// + decision 行 + approval.decided 审计行,但零 resume outbox 事件、节点
// 保持 WAITING_APPROVAL(续跑权威在 worker,未定局不产生续跑意图)。
func TestApprovalAllPartialApproveWaits(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	execID, nodeID := seedSuspendedApprovalMode(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "all", "carol", "dave")
	carol := signSubject(t, "carol", auth.RoleApprover)

	code, body := doJSON(t, r, http.MethodPost, decisionPath(execID), carol,
		[]byte(`{"decision":"approve","comment":"第一批"}`))
	if code != http.StatusCreated {
		t.Fatalf("partial decide = %d %v, want 201", code, body)
	}
	if body["decided_by"] != "carol" || body["decision"] != "approve" {
		t.Fatalf("body = %v, want decision 回显", body)
	}
	// 契约(t14.1):非定局票 final 字段缺省(omitempty)。
	if _, has := body["final"]; has {
		t.Fatalf("partial approve must omit final, got %v", body["final"])
	}
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM approval_decision WHERE node_execution_id=?`, nodeID); n != 1 {
		t.Fatalf("decision rows = %d, want 1", n)
	}
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM outbox WHERE execution_id=? AND event_type=?`,
		execID, scheduler.EventTypeExecutionResume); n != 0 {
		t.Fatalf("resume events = %d, want 0(未定局零续跑意图)", n)
	}
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND event_type='`+
		audit.EventApprovalDecided+`' AND actor_id='carol'`, execID); n != 1 {
		t.Fatalf("approval.decided rows = %d, want 1(每票一行)", n)
	}
	var st string
	if err := dbh.QueryRow(`SELECT status FROM node_execution WHERE id=?`, nodeID).Scan(&st); err != nil {
		t.Fatalf("read node: %v", err)
	}
	if st != "WAITING_APPROVAL" {
		t.Fatalf("node status = %s, want WAITING_APPROVAL", st)
	}
}

// TestApprovalAllFullApproveFinalizes:末票 approve(票齐)→ 201 + final=true
// + resume 事件恰一条(TaskID 四段式锚末票 DecisionID 语义由 worker 复核
// 承载,api 只落一条)。
func TestApprovalAllFullApproveFinalizes(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	execID, nodeID := seedSuspendedApprovalMode(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "all", "carol", "dave")
	carol := signSubject(t, "carol", auth.RoleApprover)
	dave := signSubject(t, "dave", auth.RoleApprover)

	if code, body := doJSON(t, r, http.MethodPost, decisionPath(execID), carol,
		[]byte(`{"decision":"approve"}`)); code != http.StatusCreated {
		t.Fatalf("first decide = %d %v, want 201", code, body)
	}
	code, body := doJSON(t, r, http.MethodPost, decisionPath(execID), dave,
		[]byte(`{"decision":"approve"}`))
	if code != http.StatusCreated {
		t.Fatalf("final decide = %d %v, want 201", code, body)
	}
	if body["final"] != true || body["decided_by"] != "dave" {
		t.Fatalf("final body = %v, want final=true by dave", body)
	}
	taskID := fmt.Sprintf("resume:%d:boss:2", execID)
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM outbox WHERE task_id=?`, taskID); n != 1 {
		t.Fatalf("resume events = %d, want 1(末票恰一条)", n)
	}
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM approval_decision WHERE node_execution_id=?`, nodeID); n != 2 {
		t.Fatalf("decision rows = %d, want 2", n)
	}
}

// TestApprovalAllRejectFinalizesImmediately:任一驳回即败——首票 reject 即
// 定局(201 + final=true + resume 事件),即使名单未投满。
func TestApprovalAllRejectFinalizesImmediately(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	execID, _ := seedSuspendedApprovalMode(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "all", "carol", "dave")
	carol := signSubject(t, "carol", auth.RoleApprover)

	code, body := doJSON(t, r, http.MethodPost, decisionPath(execID), carol,
		[]byte(`{"decision":"reject"}`))
	if code != http.StatusCreated {
		t.Fatalf("reject = %d %v, want 201", code, body)
	}
	if body["final"] != true {
		t.Fatalf("body = %v, want final=true(任一驳回即定局)", body)
	}
	taskID := fmt.Sprintf("resume:%d:boss:2", execID)
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM outbox WHERE task_id=?`, taskID); n != 1 {
		t.Fatalf("resume events = %d, want 1", n)
	}
}

// TestApprovalAllVoteAfterFinal:定局后的新主体投票 → 409 already_decided
// (all:已全员批准或已驳回;语义面与或签首票后一致)。
func TestApprovalAllVoteAfterFinal(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	execID, _ := seedSuspendedApprovalMode(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "all", "carol", "dave")
	carol := signSubject(t, "carol", auth.RoleApprover)
	dave := signSubject(t, "dave", auth.RoleApprover)

	if code, _ := doJSON(t, r, http.MethodPost, decisionPath(execID), carol,
		[]byte(`{"decision":"reject"}`)); code != http.StatusCreated {
		t.Fatalf("reject = %d, want 201", code)
	}
	code, body := doJSON(t, r, http.MethodPost, decisionPath(execID), dave,
		[]byte(`{"decision":"approve"}`))
	if code != http.StatusConflict || body["error"] != "already_decided" {
		t.Fatalf("post-final vote = %d %v, want 409 already_decided", code, body)
	}
}

// TestApprovalAllReplayAndFlip:重放/改判语义不随 mode 变化——中间态同人
// 同决定重放 200(仍无 final),改判 409 decision_conflict。
func TestApprovalAllReplayAndFlip(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	execID, _ := seedSuspendedApprovalMode(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "all", "carol", "dave")
	carol := signSubject(t, "carol", auth.RoleApprover)

	if code, _ := doJSON(t, r, http.MethodPost, decisionPath(execID), carol,
		[]byte(`{"decision":"approve"}`)); code != http.StatusCreated {
		t.Fatalf("first = %d, want 201", code)
	}
	code, body := doJSON(t, r, http.MethodPost, decisionPath(execID), carol,
		[]byte(`{"decision":"approve"}`))
	if code != http.StatusOK {
		t.Fatalf("replay = %d %v, want 200", code, body)
	}
	if _, has := body["final"]; has {
		t.Fatalf("replay of non-final vote must omit final, got %v", body["final"])
	}
	code, body = doJSON(t, r, http.MethodPost, decisionPath(execID), carol,
		[]byte(`{"decision":"reject"}`))
	if code != http.StatusConflict || body["error"] != "decision_conflict" {
		t.Fatalf("flip = %d %v, want 409 decision_conflict", code, body)
	}
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM approval_decision WHERE execution_id=?`, execID); n != 1 {
		t.Fatalf("decision rows = %d, want 1(重放/改判零新写)", n)
	}
}

// TestApprovalDetailPendingApprovers:GET 详情的会签中间态字段(t14.1 画布
// 数据契约):pending_approvers = 名单 − 本轮已投者(WAITING 时);定局后
// (仍 WAITING 的异步窗)为空;decisions[].round 分组维度已在(T9.2)。
func TestApprovalDetailPendingApprovers(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	execID, _ := seedSuspendedApprovalMode(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "all", "carol", "dave")
	carol := signSubject(t, "carol", auth.RoleApprover)
	dave := signSubject(t, "dave", auth.RoleApprover)

	dec, err := json.Marshal(map[string]string{"decision": "approve"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if code, _ := doJSON(t, r, http.MethodPost, decisionPath(execID), carol, dec); code != http.StatusCreated {
		t.Fatalf("partial decide = %d, want 201", code)
	}

	code, body := doJSON(t, r, http.MethodGet, approvalPath(execID), carol, nil)
	if code != http.StatusOK {
		t.Fatalf("detail = %d, want 200", code)
	}
	if body["mode"] != "all" {
		t.Fatalf("mode = %v, want all", body["mode"])
	}
	pending, ok := body["pending_approvers"].([]any)
	if !ok || len(pending) != 1 || pending[0] != "dave" {
		t.Fatalf("pending_approvers = %v, want [dave]", body["pending_approvers"])
	}
	rounds := body["decisions"].([]any)
	if len(rounds) != 1 || rounds[0].(map[string]any)["round"] != float64(1) {
		t.Fatalf("decisions rounds = %v, want one round-1 row", rounds)
	}

	// dave 视角可读(名单内),补票后 pending 清空(节点异步窗内仍 WAITING)。
	if code, _ = doJSON(t, r, http.MethodPost, decisionPath(execID), dave,
		[]byte(`{"decision":"approve"}`)); code != http.StatusCreated {
		t.Fatalf("final decide = %d, want 201", code)
	}
	code, body = doJSON(t, r, http.MethodGet, approvalPath(execID), dave, nil)
	if code != http.StatusOK {
		t.Fatalf("detail after final = %d, want 200", code)
	}
	if got := body["pending_approvers"].([]any); len(got) != 0 {
		t.Fatalf("pending_approvers after final = %v, want empty", got)
	}
}

// TestApprovalsPendingExcludesVotedApprover:待办列表排除本轮已投者——
// 会签挂起期长,已投者无可做动作不得再见待办(any 模式异步窗同改善)。
func TestApprovalsPendingExcludesVotedApprover(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	execID, _ := seedSuspendedApprovalMode(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "all", "carol", "dave")
	carol := signSubject(t, "carol", auth.RoleApprover)
	dave := signSubject(t, "dave", auth.RoleApprover)

	visible := func(token string) int {
		t.Helper()
		code, body := doJSON(t, r, http.MethodGet, "/api/v1/approvals/pending", token, nil)
		if code != http.StatusOK {
			t.Fatalf("pending list = %d %v", code, body)
		}
		return int(body["page"].(map[string]any)["total"].(float64))
	}
	if n := visible(carol); n != 1 {
		t.Fatalf("carol pre-vote total = %d, want 1", n)
	}
	if code, _ := doJSON(t, r, http.MethodPost, decisionPath(execID), carol,
		[]byte(`{"decision":"approve"}`)); code != http.StatusCreated {
		t.Fatalf("partial decide = %d, want 201", code)
	}
	if n := visible(carol); n != 0 {
		t.Fatalf("carol post-vote total = %d, want 0(已投者排除)", n)
	}
	if n := visible(dave); n != 1 {
		t.Fatalf("dave total = %d, want 1(未投者仍见)", n)
	}
}

// TestApprovalDetailRejectFinalAsyncWindow:定局异步窗契约锚(对抗审计
// MEDIUM:注释如实化的测试面)——carol reject 定局后,worker 续跑前节点仍
// WAITING,dave 仍见 pending_approvers=[dave](拍板公式 = 名单−本轮已投,
// 不按定局清空);其投票 409 already_decided(窗口有界,收口后自愈清空)。
func TestApprovalDetailRejectFinalAsyncWindow(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	execID, _ := seedSuspendedApprovalMode(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "all", "carol", "dave")
	carol := signSubject(t, "carol", auth.RoleApprover)
	dave := signSubject(t, "dave", auth.RoleApprover)

	code, body := doJSON(t, r, http.MethodPost, decisionPath(execID), carol,
		[]byte(`{"decision":"reject"}`))
	if code != http.StatusCreated || body["final"] != true {
		t.Fatalf("reject = %d %v, want 201 final=true", code, body)
	}
	code, body = doJSON(t, r, http.MethodGet, approvalPath(execID), dave, nil)
	if code != http.StatusOK {
		t.Fatalf("detail in async window = %d, want 200", code)
	}
	if body["node_status"] != "WAITING_APPROVAL" {
		t.Fatalf("node_status = %v, want WAITING_APPROVAL(异步窗)", body["node_status"])
	}
	pending, ok := body["pending_approvers"].([]any)
	if !ok || len(pending) != 1 || pending[0] != "dave" {
		t.Fatalf("pending_approvers = %v, want [dave](定局不清空,拍板公式)", body["pending_approvers"])
	}
	// 未投者投票 → 409(定局守卫先于状态守卫)。
	code, body = doJSON(t, r, http.MethodPost, decisionPath(execID), dave,
		[]byte(`{"decision":"approve"}`))
	if code != http.StatusConflict || body["error"] != "already_decided" {
		t.Fatalf("post-final vote = %d %v, want 409 already_decided", code, body)
	}
}
