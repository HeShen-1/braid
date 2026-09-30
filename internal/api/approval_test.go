package api

// T4.2 审批面 HTTP 契约测试(t13 真 MySQL,不可达自动 skip)。覆盖:
//   - 决定 POST:鉴权(未认证 401 / 越权 403 + approval.decision_denied
//     审计行)、守卫链(404/400/409 全分面)、或签首票定局(第二票 409)、
//     同人重放 200 / 改判 409、落库三写同事务(decision 行 + approval.decided
//     审计行 + execution.resume outbox 行,task_id 四段式);
//   - 详情 GET:快照可读面 + 决定时间线;
//   - Idempotency-Key(D-t55 §5,T4.2 兑现):重放 200 同执行、跨主体/跨
//     定义不串、超长 400、作用域哈希帧确定性。
//
// 变异自查:越权不落审计 → denial 断言红;首票定局守卫移除 → 第二票
// 409 断言红;重放误判新写 → outbox 行数断言红;作用域哈希丢主体维度 →
// 跨主体不串断言红。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"braid/internal/audit"
	"braid/internal/auth"
	"braid/internal/db/sqltest"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
	"braid/internal/tracing/tracingtest"
)

// signSubject 签发指定主体的 token(approval 测试需要 carol/bob 等多主体,
// newTestServer 的 sign 固定 alice)。
func signSubject(t *testing.T, subject string, roles ...auth.Role) string {
	t.Helper()
	s, err := auth.SignToken(testSecret, time.Hour, subject, roles, 0)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return s
}

// doJSONHeaders 是 doJSON 的带自定义头变体(Idempotency-Key 面)。
func doJSONHeaders(t *testing.T, r *gin.Engine, method, path, token string, body []byte, headers map[string]string) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("non-JSON body (code %d): %q", w.Code, w.Body.String())
		}
	}
	return w.Code, out
}

// approvalTestIDs 是审批面测试的包级雪花源:独立节点号 7(其余 api 测试
// 用 1、scheduler 装配用 1/3、worker 用 0/2),单例序列单调——每次调用
// 新建生成器会在同毫秒撞首号(PK 1062 假红)。
var approvalTestIDs, _ = snowflake.New(7)

// seedSuspendedApproval 直插一个挂起审批节点的执行(生产链 = T4.1 worker
// 挂起;此处 SQL 种子钉 HTTP 契约面,免拉全链)。快照 = ApprovalInput 生效值
// 形状(scheduler.buildSnapshot 同构)。mode 固定 any(既有或签用例);
// 会签用例走 seedSuspendedApprovalMode。
func seedSuspendedApproval(t *testing.T, dbh *sql.DB, wfStatus string, nodeStatus string, approvers ...string) (uint64, uint64) {
	return seedSuspendedApprovalMode(t, dbh, wfStatus, nodeStatus, "any", approvers...)
}

// seedSuspendedApprovalMode 是 seedSuspendedApproval 的 mode 参数化变体
// (T14.2 会签:快照 mode = any|all)。
func seedSuspendedApprovalMode(t *testing.T, dbh *sql.DB, wfStatus string, nodeStatus string, mode string, approvers ...string) (uint64, uint64) {
	t.Helper()
	defID, err := approvalTestIDs.Next()
	if err != nil {
		t.Fatalf("gen def id: %v", err)
	}
	execID, err := approvalTestIDs.Next()
	if err != nil {
		t.Fatalf("gen exec id: %v", err)
	}
	nodeID, err := approvalTestIDs.Next()
	if err != nil {
		t.Fatalf("gen node id: %v", err)
	}
	name := fmt.Sprintf("approval-seed-%d", defID)
	snap, err := json.Marshal(scheduler.ApprovalInput{
		Approvers: approvers, Mode: mode, TimeoutHours: 24, OnTimeout: "fail", Instructions: "请审批",
	})
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if _, err := dbh.Exec(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', '{}', ?, 'seed', 'seed', 'seed', NOW(3))`,
		defID, name, strings.Repeat("0", 64)); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	if _, err := dbh.Exec(`INSERT INTO workflow_execution
	  (id, definition_id, definition_name, definition_version, definition_hash, status,
	   trigger_type, triggered_by, input)
	  VALUES (?,?,?,1,?,?,'API','seed','{}')`,
		execID, defID, name, strings.Repeat("0", 64), wfStatus); err != nil {
		t.Fatalf("seed execution: %v", err)
	}
	if _, err := dbh.Exec(`INSERT INTO node_execution
	  (id, execution_id, node_id, type, status, attempts, max_attempts, version,
	   worker_id, input_snapshot, deadline_at)
	  VALUES (?,?, 'boss', 'approval', ?, 1, 1, 2, 'seed', ?, DATE_ADD(NOW(3), INTERVAL 24 HOUR))`,
		nodeID, execID, nodeStatus, snap); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	return execID, nodeID
}

func decisionPath(execID uint64) string {
	return fmt.Sprintf("/api/v1/executions/%d/approvals/boss/decision", execID)
}

func approvalPath(execID uint64) string {
	return fmt.Sprintf("/api/v1/executions/%d/approvals/boss", execID)
}

// countRows 计数助手(清理后共享 schema 的断言面)。
func countRows(t *testing.T, dbh *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := dbh.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// TestApprovalDecisionHappyPath:合法决定 → 201 + 三写同事务(decision 行 +
// approval.decided 审计行 + execution.resume outbox 行,task_id 四段式
// resume:<exec>:boss:2)+ 节点零状态变化(续跑异步权威在 worker)。
func TestApprovalDecisionHappyPath(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	execID, nodeID := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol")
	carol := signSubject(t, "carol", auth.RoleApprover)

	code, body := doJSON(t, r, http.MethodPost, decisionPath(execID), carol,
		[]byte(`{"decision":"approve","comment":"可以"}`))
	if code != http.StatusCreated {
		t.Fatalf("decide = %d %v, want 201", code, body)
	}
	if body["decision"] != "approve" || body["decided_by"] != "carol" {
		t.Fatalf("body = %v, want decision/decided_by 回显", body)
	}
	// decision 行。
	var dbDec, comment string
	if err := dbh.QueryRow(`SELECT decision, comment FROM approval_decision WHERE node_execution_id=?`,
		nodeID).Scan(&dbDec, &comment); err != nil {
		t.Fatalf("read decision row: %v", err)
	}
	if dbDec != "APPROVED" || comment != "可以" {
		t.Fatalf("decision row = %s %q, want APPROVED 可以", dbDec, comment)
	}
	// outbox 行:四段式 TaskID + 默认队列 + PENDING。
	taskID := fmt.Sprintf("resume:%d:boss:2", execID)
	var evtType, queue, status string
	if err := dbh.QueryRow(`SELECT event_type, queue, status FROM outbox WHERE task_id=?`, taskID).
		Scan(&evtType, &queue, &status); err != nil {
		t.Fatalf("read resume event: %v", err)
	}
	if evtType != scheduler.EventTypeExecutionResume || queue != "default" || status != "PENDING" {
		t.Fatalf("resume event = %s/%s/%s, want execution.resume/default/PENDING", evtType, queue, status)
	}
	// 审计行:approval.decided,actor=carol。
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND event_type=? AND actor_id='carol'`,
		execID, audit.EventApprovalDecided); n != 1 {
		t.Fatalf("approval.decided rows = %d, want 1", n)
	}
	// 节点零状态变化(续跑在 worker)。
	var nodeStatus string
	if err := dbh.QueryRow(`SELECT status FROM node_execution WHERE id=?`, nodeID).Scan(&nodeStatus); err != nil {
		t.Fatalf("read node: %v", err)
	}
	if nodeStatus != "WAITING_APPROVAL" {
		t.Fatalf("node status = %s, want WAITING_APPROVAL(api 不迁状态)", nodeStatus)
	}
}

// TestApprovalDecisionReplayAndFlip:同人同决定重放 → 200 零新写;改判 → 409。
func TestApprovalDecisionReplayAndFlip(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	execID, nodeID := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol")
	carol := signSubject(t, "carol", auth.RoleApprover)

	code, _ := doJSON(t, r, http.MethodPost, decisionPath(execID), carol, []byte(`{"decision":"approve"}`))
	if code != http.StatusCreated {
		t.Fatalf("first decide = %d, want 201", code)
	}
	code, body := doJSON(t, r, http.MethodPost, decisionPath(execID), carol, []byte(`{"decision":"approve"}`))
	if code != http.StatusOK {
		t.Fatalf("replay = %d %v, want 200", code, body)
	}
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM outbox WHERE execution_id=? AND event_type=?`,
		execID, scheduler.EventTypeExecutionResume); n != 1 {
		t.Fatalf("resume events after replay = %d, want 1(重放零新写)", n)
	}
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM approval_decision WHERE node_execution_id=?`, nodeID); n != 1 {
		t.Fatalf("decision rows after replay = %d, want 1", n)
	}
	code, body = doJSON(t, r, http.MethodPost, decisionPath(execID), carol, []byte(`{"decision":"reject"}`))
	if code != http.StatusConflict || body["error"] != "decision_conflict" {
		t.Fatalf("flip = %d %v, want 409 decision_conflict", code, body)
	}
}

// TestApprovalDecisionFirstVoteWins:或签首票定局——他人后续票 409
// already_decided(节点仍 WAITING 的异步窗内),零新事件。
func TestApprovalDecisionFirstVoteWins(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	execID, _ := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol", "dave")
	carol := signSubject(t, "carol", auth.RoleApprover)
	dave := signSubject(t, "dave", auth.RoleApprover)

	if code, _ := doJSON(t, r, http.MethodPost, decisionPath(execID), carol, []byte(`{"decision":"approve"}`)); code != http.StatusCreated {
		t.Fatalf("carol decide = %d, want 201", code)
	}
	code, body := doJSON(t, r, http.MethodPost, decisionPath(execID), dave, []byte(`{"decision":"reject"}`))
	if code != http.StatusConflict || body["error"] != "already_decided" {
		t.Fatalf("dave decide = %d %v, want 409 already_decided", code, body)
	}
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM outbox WHERE execution_id=? AND event_type=?`,
		execID, scheduler.EventTypeExecutionResume); n != 1 {
		t.Fatalf("resume events = %d, want 1(首票定局)", n)
	}
}

// TestApprovalDecisionAuthz:未认证 401;越权 403 + approval.decision_denied
// 审计行落库 + 决定/事件零写入(验收:未认证或非审批人调用被拒)。
func TestApprovalDecisionAuthz(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	execID, nodeID := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol")

	code, body := doJSON(t, r, http.MethodPost, decisionPath(execID), "", []byte(`{"decision":"approve"}`))
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d %v, want 401", code, body)
	}
	bob := signSubject(t, "bob", auth.RoleAdmin) // admin 角色也不放行:门 = DSL 名单
	code, body = doJSON(t, r, http.MethodPost, decisionPath(execID), bob, []byte(`{"decision":"approve"}`))
	if code != http.StatusForbidden || body["error"] != "not_approver" {
		t.Fatalf("non-approver = %d %v, want 403 not_approver", code, body)
	}
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND event_type=? AND actor_id='bob'`,
		execID, audit.EventApprovalDecisionDenied); n != 1 {
		t.Fatalf("decision_denied rows = %d, want 1(越权拒绝必须落审计)", n)
	}
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM approval_decision WHERE node_execution_id=?`, nodeID); n != 0 {
		t.Fatalf("decision rows after denial = %d, want 0", n)
	}
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM outbox WHERE execution_id=? AND event_type=?`,
		execID, scheduler.EventTypeExecutionResume); n != 0 {
		t.Fatalf("resume events after denial = %d, want 0", n)
	}
}

// TestApprovalDecisionGuardFaces:404(执行/节点)与 400(bad_node_id/
// bad_decision/bad_comment)、409(not_approval/not_waiting/execution_terminal)
// 全分面。
func TestApprovalDecisionGuardFaces(t *testing.T) {
	r, dbh, sign := newTestServer(t)
	token := sign(auth.RoleApprover)

	// 404:未知执行。
	if code, _ := doJSON(t, r, http.MethodPost, decisionPath(999), token, []byte(`{"decision":"approve"}`)); code != http.StatusNotFound {
		t.Fatalf("unknown exec = %d, want 404", code)
	}
	// 400:node_id 形态。
	execID, _ := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "alice")
	if code, body := doJSON(t, r, http.MethodPost,
		fmt.Sprintf("/api/v1/executions/%d/approvals/Boss/decision", execID), token,
		[]byte(`{"decision":"approve"}`)); code != http.StatusBadRequest || body["error"] != "bad_node_id" {
		t.Fatalf("bad node id = %d %v, want 400 bad_node_id", code, body)
	}
	// 404:执行在、节点不在。
	if code, _ := doJSON(t, r, http.MethodPost,
		fmt.Sprintf("/api/v1/executions/%d/approvals/ghost/decision", execID), token,
		[]byte(`{"decision":"approve"}`)); code != http.StatusNotFound {
		t.Fatalf("unknown node = %d, want 404", code)
	}
	// 400:决定词表外。
	if code, body := doJSON(t, r, http.MethodPost, decisionPath(execID), token,
		[]byte(`{"decision":"maybe"}`)); code != http.StatusBadRequest || body["error"] != "bad_decision" {
		t.Fatalf("bad decision = %d %v, want 400 bad_decision", code, body)
	}
	// 400:comment 超 2000 rune。
	if code, body := doJSON(t, r, http.MethodPost, decisionPath(execID), token,
		[]byte(`{"decision":"approve","comment":"`+strings.Repeat("字", 2001)+`"}`)); code != http.StatusBadRequest || body["error"] != "bad_comment" {
		t.Fatalf("bad comment = %d %v, want 400 bad_comment", code, body)
	}
	// 409:非 approval 节点。
	nonApprovalExec, _ := seedNonApprovalNode(t, dbh)
	if code, body := doJSON(t, r, http.MethodPost, decisionPath(nonApprovalExec), token,
		[]byte(`{"decision":"approve"}`)); code != http.StatusConflict || body["error"] != "not_approval" {
		t.Fatalf("non-approval = %d %v, want 409 not_approval", code, body)
	}
	// 409:节点非 WAITING(已 SUCCEEDED)。
	doneExec, _ := seedSuspendedApproval(t, dbh, "SUCCEEDED", "SUCCEEDED", "alice")
	if code, body := doJSON(t, r, http.MethodPost, decisionPath(doneExec), token,
		[]byte(`{"decision":"approve"}`)); code != http.StatusConflict || body["error"] != "not_waiting" {
		t.Fatalf("resolved node = %d %v, want 409 not_waiting", code, body)
	}
	// 409:执行终态(节点仍 WAITING——fail-fast 竞速残留)。
	termExec, _ := seedSuspendedApproval(t, dbh, "FAILED", "WAITING_APPROVAL", "alice")
	if code, body := doJSON(t, r, http.MethodPost, decisionPath(termExec), token,
		[]byte(`{"decision":"approve"}`)); code != http.StatusConflict || body["error"] != "execution_terminal" {
		t.Fatalf("terminal exec = %d %v, want 409 execution_terminal", code, body)
	}
}

// seedNonApprovalNode 种一个 llm 类型节点(approvers 名单含 alice,类型守卫面)。
func seedNonApprovalNode(t *testing.T, dbh *sql.DB) (uint64, uint64) {
	t.Helper()
	defID, err := approvalTestIDs.Next()
	if err != nil {
		t.Fatalf("gen def id: %v", err)
	}
	execID, err := approvalTestIDs.Next()
	if err != nil {
		t.Fatalf("gen exec id: %v", err)
	}
	nodeID, err := approvalTestIDs.Next()
	if err != nil {
		t.Fatalf("gen node id: %v", err)
	}
	name := fmt.Sprintf("nonapproval-seed-%d", defID)
	if _, err := dbh.Exec(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', '{}', ?, 'seed', 'seed', 'seed', NOW(3))`,
		defID, name, strings.Repeat("0", 64)); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	if _, err := dbh.Exec(`INSERT INTO workflow_execution
	  (id, definition_id, definition_name, definition_version, definition_hash, status, trigger_type, triggered_by, input)
	  VALUES (?,?,?,1,?,'RUNNING','API','seed','{}')`,
		execID, defID, name, strings.Repeat("0", 64)); err != nil {
		t.Fatalf("seed execution: %v", err)
	}
	// 节点 id 恰用 boss(approval 名单面在快照里,但类型守卫先行拒绝)。
	if _, err := dbh.Exec(`INSERT INTO node_execution
	  (id, execution_id, node_id, type, status, attempts, max_attempts, version, worker_id, input_snapshot)
	  VALUES (?,?, 'boss', 'llm', 'EXECUTING', 1, 1, 2, 'seed', '{"model":"m"}')`,
		nodeID, execID); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	return execID, nodeID
}

// TestApprovalDetail:GET 详情 = 节点状态 + 快照可读面 + 决定时间线;
// 未认证 401;未知执行 404;非 approval 409。
func TestApprovalDetail(t *testing.T) {
	r, dbh, sign := newTestServer(t)
	execID, _ := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol")
	token := sign(auth.RoleApprover)

	if code, _ := doJSON(t, r, http.MethodGet, approvalPath(execID), "", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated detail = %d, want 401", code)
	}
	// T5.2 换装②:读面收窄到名单内审批人——alice(名单外)403。
	code, body := doJSON(t, r, http.MethodGet, approvalPath(execID), token, nil)
	if code != http.StatusForbidden || body["error"] != "not_approver" {
		t.Fatalf("non-member detail = %d %v, want 403 not_approver", code, body)
	}
	carol := signSubject(t, "carol", auth.RoleApprover)
	code, body = doJSON(t, r, http.MethodGet, approvalPath(execID), carol, nil)
	if code != http.StatusOK {
		t.Fatalf("detail = %d %v, want 200", code, body)
	}
	if body["node_status"] != "WAITING_APPROVAL" || body["mode"] != "any" || body["instructions"] != "请审批" {
		t.Fatalf("detail = %v, want snapshot face", body)
	}
	if got := body["approvers"].([]any); len(got) != 1 || got[0] != "carol" {
		t.Fatalf("approvers = %v, want [carol]", body["approvers"])
	}
	if body["timeout_hours"] != float64(24) || body["on_timeout"] != "fail" {
		t.Fatalf("detail timeout face = %v", body)
	}
	if decisions, ok := body["decisions"].([]any); !ok || len(decisions) != 0 {
		t.Fatalf("decisions = %v, want []", body["decisions"])
	}
	// 决定后回读:时间线出现。
	if code, _ := doJSON(t, r, http.MethodPost, decisionPath(execID), carol, []byte(`{"decision":"reject","comment":"不行"}`)); code != http.StatusCreated {
		t.Fatalf("decide = %d, want 201", code)
	}
	_, body = doJSON(t, r, http.MethodGet, approvalPath(execID), carol, nil)
	decisions := body["decisions"].([]any)
	if len(decisions) != 1 {
		t.Fatalf("decisions after reject = %v, want 1", decisions)
	}
	d := decisions[0].(map[string]any)
	if d["decision"] != "reject" || d["decided_by"] != "carol" || d["comment"] != "不行" {
		t.Fatalf("decision row = %v, want reject/carol/不行", d)
	}
	// 未知执行 404;非 approval 409。
	if code, _ := doJSON(t, r, http.MethodGet, approvalPath(999), token, nil); code != http.StatusNotFound {
		t.Fatalf("unknown exec detail = %d, want 404", code)
	}
	nonApprovalExec, _ := seedNonApprovalNode(t, dbh)
	if code, body := doJSON(t, r, http.MethodGet, approvalPath(nonApprovalExec), token, nil); code != http.StatusConflict || body["error"] != "not_approval" {
		t.Fatalf("non-approval detail = %d %v, want 409 not_approval", code, body)
	}
}

// TestTriggerIdempotencyKey:D-t55 §5 兑现——同键同主体同定义重放 200 回原
// 执行;跨主体/跨定义不串;超长 400。
func TestTriggerIdempotencyKey(t *testing.T) {
	dbh := openAPITestDB(t)
	schedIDs, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	sched, err := scheduler.New(scheduler.Deps{DB: dbh, IDs: schedIDs})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	r, _, _ := newTestServerWithTrigger(t, sched)
	author := signSubject(t, "alice", auth.RoleAuthor)
	other := signSubject(t, "bob", auth.RoleAuthor)
	name1 := uniqName("idem-a")
	name2 := uniqName("idem-b")
	publishViaAPI(t, r, author, name1)
	publishViaAPI(t, r, author, name2)
	hdr := map[string]string{IdempotencyKeyHeader: "run-001"}

	post := func(token, name string, headers map[string]string) (int, map[string]any) {
		return doJSONHeaders(t, r, http.MethodPost, "/api/v1/executions", token,
			[]byte(fmt.Sprintf(`{"definition_name":%q}`, name)), headers)
	}

	// 首发创建。
	code, first := post(author, name1, hdr)
	if code != http.StatusCreated {
		t.Fatalf("first trigger = %d %v, want 201", code, first)
	}
	// 同键重放:200 + 同 id,零新执行。
	code, replay := post(author, name1, hdr)
	if code != http.StatusOK {
		t.Fatalf("replay trigger = %d %v, want 200", code, replay)
	}
	if replay["id"] != first["id"] {
		t.Fatalf("replay id = %v, want %v", replay["id"], first["id"])
	}
	// 跨主体同键:新执行(作用域二维)。
	code, bobExec := post(other, name1, hdr)
	if code != http.StatusCreated || bobExec["id"] == first["id"] {
		t.Fatalf("cross-subject = %d %v, want 201 + distinct id", code, bobExec)
	}
	// 同主体跨定义同键:新执行。
	code, otherDef := post(author, name2, hdr)
	if code != http.StatusCreated || otherDef["id"] == first["id"] {
		t.Fatalf("cross-definition = %d %v, want 201 + distinct id", code, otherDef)
	}
	// 无键照旧 201。
	if code, _ := post(author, name1, nil); code != http.StatusCreated {
		t.Fatalf("no-key trigger = %d, want 201", code)
	}
	// 超长键 400。
	long := map[string]string{IdempotencyKeyHeader: strings.Repeat("k", maxIdempotencyKeyLen+1)}
	if code, body := post(author, name1, long); code != http.StatusBadRequest || body["error"] != "bad_idempotency_key" {
		t.Fatalf("long key = %d %v, want 400 bad_idempotency_key", code, body)
	}
	// 幂等键行数:3 个执行共 3 行非 NULL(无键那次不计)。
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM workflow_execution WHERE idempotency_key IS NOT NULL`); n != 3 {
		t.Fatalf("idempotent executions = %d, want 3", n)
	}
}

// openAPITestDB 暴露 newTestServerWithTrigger 同款 DB(幂等键测试自建
// scheduler 装配)。
func openAPITestDB(t *testing.T) *sql.DB {
	t.Helper()
	return sqltest.MustOpenAt(t, "braid_api_test")
}

// TestScopedIdempotencyKeyFraming:长度前缀帧的确定性/防碰撞(纯函数面)。
func TestScopedIdempotencyKeyFraming(t *testing.T) {
	a := scopedIdempotencyKey("alice", "wf", "key1")
	if a != scopedIdempotencyKey("alice", "wf", "key1") {
		t.Fatal("scoped key not deterministic")
	}
	if len(a) != 64 {
		t.Fatalf("scoped key len = %d, want 64 hex", len(a))
	}
	// 任一维度变 → 值变。
	if a == scopedIdempotencyKey("alice2", "wf", "key1") ||
		a == scopedIdempotencyKey("alice", "wf2", "key1") ||
		a == scopedIdempotencyKey("alice", "wf", "key2") {
		t.Fatal("scope dimension collapsed")
	}
	// 分隔符歧义面:("a","b","c") vs ("ab","","c") 等拼接恒不等。
	if scopedIdempotencyKey("a", "b", "c") == scopedIdempotencyKey("ab", "", "c") {
		t.Fatal("length-framing collision")
	}
}

// TestResumePayloadSpanIDEmitted(D-t121 #3 发射面半边,review HIGH 修):
// 决策请求 ctx 携 span 时,execution.resume payload 的 span_id == 该 span
// id(续跑段根的真 parent)。删掉 appendResumeEvent 的 SpanID 接线 → 红。
// 断言 trace 链(SpanContext 整对)由 nodes e2e 真链层承担,这里钉发射值。
func TestResumePayloadSpanIDEmitted(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	execID, nodeID := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol")
	carol := signSubject(t, "carol", auth.RoleApprover)

	tp, _ := tracingtest.New()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	sctx, span := tp.Tracer("braid/test").Start(context.Background(), "decide")
	defer span.End()
	want := span.SpanContext().SpanID().String()

	req := httptest.NewRequest(http.MethodPost, decisionPath(execID),
		bytes.NewReader([]byte(`{"decision":"approve","comment":"可以"}`)))
	req = req.WithContext(sctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+carol)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("decide = %d %s, want 201", w.Code, w.Body.String())
	}

	var payload []byte
	taskID := fmt.Sprintf("resume:%d:boss:2", execID)
	if err := dbh.QueryRow(`SELECT payload FROM outbox WHERE task_id=?`, taskID).Scan(&payload); err != nil {
		t.Fatalf("read resume event: %v", err)
	}
	var rp struct {
		SpanID string `json:"span_id"`
	}
	if err := json.Unmarshal(payload, &rp); err != nil {
		t.Fatalf("resume payload: %v", err)
	}
	if rp.SpanID != want {
		t.Fatalf("resume payload span_id = %q, want decision span %q", rp.SpanID, want)
	}
	_ = nodeID
}
