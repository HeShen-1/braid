package api

// T5.2 换装测试(D-t42 §7 两个换装点):
//   - ① POST decision 角色闸:DSL approvers[] 名单之上叠加 approver|admin
//     角色门——名单内但无角色 → 403 role_denied + approval.decision_denied
//     审计行(reason=role_denied);名单内且持 admin → 放行(角色面与
//     definitions/trigger 写面的 admin 豁免惯例一致)。
//   - ② GET 审批详情读面收窄:任意有效 token → 名单内审批人;名单外
//     (含 admin——D-t42 钦定严格名单内)→ 403;未挂起(无快照可证
//     名单)fail-closed 403,状态读面仍在 GET /executions/:id。
//
// 变异自查:角色闸移除 → carol-no-role 201/409 断言红;GET 收窄移除 →
// alice 200 断言变红(反向);reason 丢维度 → role_denied 审计行断言红。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"braid/internal/audit"
	"braid/internal/auth"
)

// TestApprovalDecisionRoleGate:换装①——角色闸叠加在 DSL 名单门之上。
func TestApprovalDecisionRoleGate(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	execID, nodeID := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol")

	// carol 在名单内但仅持 author 角色(签发面恒 ≥1 角色,「无角色」不可
	// 构造;越权真面 = 角色不匹配)→ 403 role_denied + 审计。
	carolAuthor := signSubject(t, "carol", auth.RoleAuthor)
	code, body := doJSON(t, r, http.MethodPost, decisionPath(execID), carolAuthor,
		[]byte(`{"decision":"approve"}`))
	if code != http.StatusForbidden || body["error"] != "role_denied" {
		t.Fatalf("listed author-only = %d %v, want 403 role_denied", code, body)
	}
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND event_type=? AND actor_id='carol'`,
		execID, audit.EventApprovalDecisionDenied); n != 1 {
		t.Fatalf("decision_denied rows = %d, want 1(角色闸拒绝必须落审计)", n)
	}
	var reason []byte
	if err := dbh.QueryRow(`SELECT payload FROM audit_log WHERE execution_id=? AND event_type=? AND actor_id='carol'`,
		execID, audit.EventApprovalDecisionDenied).Scan(&reason); err != nil {
		t.Fatalf("read denial payload: %v", err)
	}
	if !containsJSONString(t, reason, "role_denied") {
		t.Fatalf("payload = %s, want reason role_denied", reason)
	}
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM approval_decision WHERE node_execution_id=?`, nodeID); n != 0 {
		t.Fatalf("decision rows after role denial = %d, want 0", n)
	}

	// carol 在名单内且持 admin → 角色面放行(approver|admin 同权,豁免
	// 惯例与 definitions/trigger 写面一致;DSL 名单仍是执行时权威)。
	carolAdmin := signSubject(t, "carol", auth.RoleAdmin)
	code, body = doJSON(t, r, http.MethodPost, decisionPath(execID), carolAdmin,
		[]byte(`{"decision":"approve"}`))
	if code != http.StatusCreated {
		t.Fatalf("listed admin decide = %d %v, want 201", code, body)
	}

	// 防灌水(sec 评审 MEDIUM):角色闸对幻影节点照旧 403,但拒绝审计
	// 只锚定「真实 approval 节点且快照在册」——幻影 node_id 与未挂起
	// PENDING 节点都不落行。
	ghost := signSubject(t, "eve", auth.RoleAuthor)
	code, _ = doJSON(t, r, http.MethodPost,
		fmt.Sprintf("/api/v1/executions/%d/approvals/ghost/decision", execID), ghost,
		[]byte(`{"decision":"approve"}`))
	if code != http.StatusForbidden {
		t.Fatalf("phantom-node role denial = %d, want 403", code)
	}
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM audit_log WHERE actor_id='eve'`); n != 0 {
		t.Fatalf("phantom-node denial rows = %d, want 0(无锚定不落审计)", n)
	}
	// PENDING(未挂起,快照 NULL,名单不可证)真实 approval 节点:同不
	// 落审计(对抗审计收紧:membership 审计同一谓词面)。
	pendExec, _ := seedSuspendedApproval(t, dbh, "RUNNING", "PENDING", "carol")
	if _, err := dbh.Exec(`UPDATE node_execution SET input_snapshot=NULL WHERE execution_id=?`, pendExec); err != nil {
		t.Fatalf("null snapshot: %v", err)
	}
	code, _ = doJSON(t, r, http.MethodPost, decisionPath(pendExec), ghost,
		[]byte(`{"decision":"approve"}`))
	if code != http.StatusForbidden {
		t.Fatalf("pending-node role denial = %d, want 403", code)
	}
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND actor_id='eve'`,
		pendExec); n != 0 {
		t.Fatalf("pending-node denial rows = %d, want 0(快照不在册不落审计)", n)
	}
}

// TestApprovalDetailMemberOnly:换装②——读面收窄到名单内审批人。
func TestApprovalDetailMemberOnly(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	execID, _ := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol")

	// 名单外有效 token(含审批角色)→ 403 not_approver。
	alice := signSubject(t, "alice", auth.RoleApprover)
	code, body := doJSON(t, r, http.MethodGet, approvalPath(execID), alice, nil)
	if code != http.StatusForbidden || body["error"] != "not_approver" {
		t.Fatalf("non-member detail = %d %v, want 403 not_approver", code, body)
	}

	// admin 也不豁免读面(D-t42 §7:收窄到名单内审批人,非名单∪admin)。
	bob := signSubject(t, "bob", auth.RoleAdmin)
	code, _ = doJSON(t, r, http.MethodGet, approvalPath(execID), bob, nil)
	if code != http.StatusForbidden {
		t.Fatalf("non-member admin detail = %d, want 403", code)
	}

	// 名单内审批人 → 200 完整快照面 + 决定时间线。
	carol := signSubject(t, "carol", auth.RoleApprover)
	code, body = doJSON(t, r, http.MethodGet, approvalPath(execID), carol, nil)
	if code != http.StatusOK {
		t.Fatalf("member detail = %d %v, want 200", code, body)
	}
	if body["node_status"] != "WAITING_APPROVAL" || body["instructions"] != "请审批" {
		t.Fatalf("member detail = %v, want snapshot face", body)
	}
	if got := body["approvers"].([]any); len(got) != 1 || got[0] != "carol" {
		t.Fatalf("approvers = %v, want [carol]", body["approvers"])
	}

	// 未挂起节点(PENDING,快照 NULL,无名单可证)→ fail-closed 403;
	// 状态读面不丢:GET /executions/:id 仍开放节点状态。
	pendExec, _ := seedSuspendedApproval(t, dbh, "RUNNING", "PENDING", "carol")
	if _, err := dbh.Exec(`UPDATE node_execution SET input_snapshot=NULL WHERE execution_id=?`, pendExec); err != nil {
		t.Fatalf("null snapshot: %v", err)
	}
	code, _ = doJSON(t, r, http.MethodGet, approvalPath(pendExec), carol, nil)
	if code != http.StatusForbidden {
		t.Fatalf("pending-node detail = %d, want 403 fail-closed", code)
	}

	// 坏快照(合法 JSON 但非 ApprovalInput 形态)→ 500 不伪造名单
	// (WAITING ⇒ 快照可解析是 engine 不变量,违例是数据完整性面,不是
	// 权限面;纯语法坏 JSON 进不了 JSON 列,MySQL 写入即校验)。
	badExec, _ := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol")
	if _, err := dbh.Exec(`UPDATE node_execution SET input_snapshot='[]' WHERE execution_id=?`, badExec); err != nil {
		t.Fatalf("corrupt snapshot: %v", err)
	}
	code, _ = doJSON(t, r, http.MethodGet, approvalPath(badExec), carol, nil)
	if code != http.StatusInternalServerError {
		t.Fatalf("bad-snapshot detail = %d, want 500", code)
	}
}

// containsJSONString 判 payload.reason 是否为 want(JSON 列经 MySQL
// 规范化,断言走解码而非子串)。
func containsJSONString(t *testing.T, raw []byte, want string) bool {
	t.Helper()
	var p audit.ApprovalDecisionDeniedPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("payload unmarshal: %v (raw=%s)", err, raw)
	}
	return p.Reason == want
}
