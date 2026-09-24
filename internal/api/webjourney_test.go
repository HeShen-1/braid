package api

// T4.4 验收锚:页面动作真实驱动流程续跑。Web 审批页(app.js)的全部
// 动作就是这组 HTTP 序列——登录 → 待办 → 详情 → POST 决定;本测试以
// 同一序列打真实路由(t13),断言决定事务的三写同存亡(decision 行 +
// approval.decided 审计行 + execution.resume outbox 行,四段式 TaskID)。
// outbox 的 execution.resume 事件即续跑触发器;worker 消费侧全链
// (resume → 下游推进 → 终态)已由 T4.2 e2e 钉死,此处钉 HTTP 契约面
// (= 页面动作面)到触发器的闭环。重放 200 同 UI 双击幂等面。
//
// 变异自查:决定事务漏 outbox → resume 行断言红;漏审计 → audit 行
// 断言红;登录换 subject → 名单外 403,决定断言红。

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"braid/internal/auth"
	"braid/internal/scheduler"
)

func TestWebApprovalJourneyDrivesResume(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	for _, q := range []string{"DELETE FROM user_role", "DELETE FROM users"} {
		if _, err := dbh.Exec(q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
	seedUser(t, dbh, "carol", auth.RoleApprover)
	execID, nodeExecID := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol")

	// ① 登录(页面动作起点:换 Bearer token)。
	code, out := doJSON(t, r, http.MethodPost, "/api/v1/auth/login", "",
		[]byte(`{"username":"carol","password":"correct-horse-battery"}`))
	if code != http.StatusOK {
		t.Fatalf("login = %d %v", code, out)
	}
	tok, _ := out["token"].(string)

	// ② 待办列表:页面首屏,carol 恰见本执行。
	code, out = doJSON(t, r, http.MethodGet, "/api/v1/approvals/pending", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("pending = %d", code)
	}
	items, _ := out["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("pending items = %d, want 1", len(items))
	}
	if items[0].(map[string]any)["execution_id"] != strconv.FormatUint(execID, 10) {
		t.Fatalf("pending item = %v, want exec %d", items[0], execID)
	}

	// ③ 详情:页面点击「处理」后的读面。
	code, out = doJSON(t, r, http.MethodGet, approvalPath(execID), tok, nil)
	if code != http.StatusOK {
		t.Fatalf("detail = %d %v", code, out)
	}
	if out["node_status"] != "WAITING_APPROVAL" || out["mode"] != "any" {
		t.Fatalf("detail = %v", out)
	}

	// ④ 决定(页面「批准」):三写同事务。
	code, out = doJSON(t, r, http.MethodPost, decisionPath(execID), tok,
		[]byte(`{"decision":"approve","comment":"页面驱动"}`))
	if code != http.StatusCreated {
		t.Fatalf("decide = %d %v, want 201", code, out)
	}

	// decision 行(决定事实本体)。
	if n := countRows(t, dbh,
		`SELECT COUNT(*) FROM approval_decision WHERE node_execution_id=? AND decided_by='carol' AND decision='APPROVED'`,
		nodeExecID); n != 1 {
		t.Fatalf("decision rows = %d, want 1", n)
	}
	// approval.decided 审计行(裁决证据)。
	if n := countRows(t, dbh,
		`SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND event_type=? AND actor_id='carol'`,
		execID, "approval.decided"); n != 1 {
		t.Fatalf("approval.decided rows = %d, want 1", n)
	}
	// execution.resume outbox 行(续跑触发器;四段式 TaskID,attempt=2)。
	taskID := fmt.Sprintf("resume:%d:boss:2", execID)
	var evtType, queue, status string
	if err := dbh.QueryRow(`SELECT event_type, queue, status FROM outbox WHERE task_id=?`, taskID).
		Scan(&evtType, &queue, &status); err != nil {
		t.Fatalf("read resume outbox row: %v", err)
	}
	if evtType != scheduler.EventTypeExecutionResume || queue != DefaultResumeQueue || status != "PENDING" {
		t.Fatalf("resume row = %s/%s/%s, want execution.resume/%s/PENDING", evtType, queue, status, DefaultResumeQueue)
	}

	// ⑤ 重放(UI 双击幂等面):同决定 200 回显,零新写。
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM approval_decision WHERE node_execution_id=?`, nodeExecID); n != 1 {
		t.Fatalf("pre-replay decisions = %d", n)
	}
	code, out = doJSON(t, r, http.MethodPost, decisionPath(execID), tok,
		[]byte(`{"decision":"approve"}`))
	if code != http.StatusOK {
		t.Fatalf("replay = %d %v, want 200", code, out)
	}
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM approval_decision WHERE node_execution_id=?`, nodeExecID); n != 1 {
		t.Fatalf("replay decisions = %d, want 1(零新写)", n)
	}
}

// TestWebApprovalJourneyRejectDrivesResume:驳回方向的页面动作同样驱动
// 续跑(对抗审计补锚:api 层此前只锚 approve 方向)——REJECTED 决定行 +
// 同款 execution.resume outbox 行(worker 侧 fail-fast 链路由 T4.2 e2e
// 钉死)。REJECTED 也是合法首票:重放 200。
func TestWebApprovalJourneyRejectDrivesResume(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	for _, q := range []string{"DELETE FROM user_role", "DELETE FROM users"} {
		if _, err := dbh.Exec(q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
	seedUser(t, dbh, "carol", auth.RoleApprover)
	execID, nodeExecID := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol")

	code, out := doJSON(t, r, http.MethodPost, "/api/v1/auth/login", "",
		[]byte(`{"username":"carol","password":"correct-horse-battery"}`))
	if code != http.StatusOK {
		t.Fatalf("login = %d", code)
	}
	tok, _ := out["token"].(string)

	code, out = doJSON(t, r, http.MethodPost, decisionPath(execID), tok,
		[]byte(`{"decision":"reject","comment":"不通过"}`))
	if code != http.StatusCreated {
		t.Fatalf("reject = %d %v, want 201", code, out)
	}
	if out["decision"] != "reject" {
		t.Fatalf("decision = %v, want reject 回显", out["decision"])
	}
	if n := countRows(t, dbh,
		`SELECT COUNT(*) FROM approval_decision WHERE node_execution_id=? AND decision='REJECTED' AND decided_by='carol'`,
		nodeExecID); n != 1 {
		t.Fatalf("REJECTED rows = %d, want 1", n)
	}
	// 驳回同样落 resume 事件(方向语义在 worker 侧消费时定局)。
	taskID := fmt.Sprintf("resume:%d:boss:2", execID)
	var evtType string
	if err := dbh.QueryRow(`SELECT event_type FROM outbox WHERE task_id=?`, taskID).Scan(&evtType); err != nil {
		t.Fatalf("read resume outbox row: %v", err)
	}
	if evtType != scheduler.EventTypeExecutionResume {
		t.Fatalf("resume row = %s, want execution.resume", evtType)
	}
	// 驳回重放同为 200(REJECTED 也是权威首票)。
	code, out = doJSON(t, r, http.MethodPost, decisionPath(execID), tok, []byte(`{"decision":"reject"}`))
	if code != http.StatusOK {
		t.Fatalf("reject replay = %d %v, want 200", code, out)
	}
}
