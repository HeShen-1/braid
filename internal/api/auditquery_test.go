// 审计查询面集成测试(T6.3,D-2026-09-25-t63-audit-query-api):
// 三维度过滤(execution/trace/人)+ 角色闸 + 分页/时间范围 + 400 面 +
// 可见性口径(仅读 audit_log:暂存行 flush 前不可见)。
package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"braid/internal/audit"
	"braid/internal/auth"
	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
)

// auditTestTrace / auditTestTraceEmpty 词表内合法 trace(32 hex 小写;
// T6.2 前生产恒 NULL,词表校验允许测试注入——端点就位待接线)。
const (
	auditTestTrace      = "0123456789abcdef0123456789abcdef"
	auditTestTraceEmpty = "ffffffffffffffffffffffffffffffff"
)

// seedIDs 独立雪花生成器(node 2,与 newTestServer 的 node 1 错开):种行
// id 只需表内唯一,不与服务器面共享生成器。
func seedIDs(t *testing.T) IDGen {
	t.Helper()
	ids, err := snowflake.New(2)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	return ids
}

// seedAuditExec 直插定义 + 执行两行:审计查询测试的变量只在 audit_log,
// 钉版链不在断言面(绕过 API 面换确定性)。dsl_hash 与 dsl 字节自洽。
func seedAuditExec(t *testing.T, dbh *sql.DB, ids IDGen, name string) uint64 {
	t.Helper()
	defID, err := ids.Next()
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	execID, err := ids.Next()
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	sum := sha256.Sum256([]byte("{}"))
	h := hex.EncodeToString(sum[:])
	if _, err := dbh.Exec(`
INSERT INTO workflow_definition (id, name, dsl_version, dsl, dsl_hash, created_by, updated_by)
VALUES (?, ?, '1.0', '{}', ?, 'seed', 'seed')`, defID, name, h); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	if _, err := dbh.Exec(`
INSERT INTO workflow_execution
  (id, definition_id, definition_name, definition_version, definition_hash, triggered_by, input)
VALUES (?, ?, ?, 1, ?, 'seed', '{}')`, execID, defID, name, h); err != nil {
		t.Fatalf("seed execution: %v", err)
	}
	return execID
}

// appendAuditRow 经同步 Append 直写(audit 包闭集校验 + exec_date 物化
// 全继承),event_at = DB NOW(3)。
func appendAuditRow(t *testing.T, dbh *sql.DB, ids IDGen, evt audit.Event) {
	t.Helper()
	id, err := ids.Next()
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	evt.ID = id
	if err := WithTx(context.Background(), dbh, func(tx DBTX) error {
		return audit.Append(context.Background(), tx, evt)
	}); err != nil {
		t.Fatalf("append audit row: %v", err)
	}
}

// enqueueAuditRow 经异步 Enqueue 暂存 audit_pending,返回行 id(flush
// 原样搬运 = 同 id 落 audit_log)。
func enqueueAuditRow(t *testing.T, dbh *sql.DB, ids IDGen, evt audit.Event) uint64 {
	t.Helper()
	id, err := ids.Next()
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	evt.ID = id
	if err := WithTx(context.Background(), dbh, func(tx DBTX) error {
		return audit.Enqueue(context.Background(), tx, evt)
	}); err != nil {
		t.Fatalf("enqueue audit row: %v", err)
	}
	return id
}

// auditItems 解包响应 items;调用方已断言 code=200。
func auditItems(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items = %T, want array", body["items"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		m, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("item = %T, want object", it)
		}
		out = append(out, m)
	}
	return out
}

func auditTotal(t *testing.T, body map[string]any) int {
	t.Helper()
	page, ok := body["page"].(map[string]any)
	if !ok {
		t.Fatalf("page = %T, want object", body["page"])
	}
	total, ok := page["total"].(float64)
	if !ok {
		t.Fatalf("total = %T, want number", page["total"])
	}
	return int(total)
}

// TestHTTPAuditQueryByExecution 维度 1:exec_date 钉分区 + execution_id
// 过滤只回该执行行;404(执行缺行)/ 400(形态)面。
func TestHTTPAuditQueryByExecution(t *testing.T) {
	r, dbh, sign := newTestServer(t)
	ids := seedIDs(t)
	author := sign(auth.RoleAuthor)
	execA := seedAuditExec(t, dbh, ids, uniqName("aud-a"))
	execB := seedAuditExec(t, dbh, ids, uniqName("aud-b"))

	appendAuditRow(t, dbh, ids, audit.Event{
		EventType: audit.EventNodeStateChanged, ExecutionID: execA,
		ActorType: audit.ActorUser, ActorID: "bob",
		Payload: json.RawMessage(`{"from":"PENDING","to":"EXECUTING"}`),
	})
	appendAuditRow(t, dbh, ids, audit.Event{
		EventType: audit.EventBranchEvaluated, ExecutionID: execA,
		ActorType: audit.ActorSystem, ActorID: "scheduler", TraceID: auditTestTrace,
	})
	appendAuditRow(t, dbh, ids, audit.Event{
		EventType: audit.EventToolCallCompleted, ExecutionID: execB,
		ActorType: audit.ActorWorker, ActorID: "worker-1",
	})

	code, body := doJSON(t, r, http.MethodGet,
		fmt.Sprintf("/api/v1/audit?execution_id=%d", execA), author, nil)
	if code != http.StatusOK {
		t.Fatalf("query by execution = %d %v, want 200", code, body)
	}
	items := auditItems(t, body)
	if len(items) != 2 || auditTotal(t, body) != 2 {
		t.Fatalf("items = %d total = %d, want 2/2", len(items), auditTotal(t, body))
	}
	// DESC:后插的 branch.evaluated 在前(event_at DESC, id DESC tie-break)。
	if items[0]["event_type"] != audit.EventBranchEvaluated {
		t.Fatalf("items[0].event_type = %v, want %s", items[0]["event_type"], audit.EventBranchEvaluated)
	}
	execAStr := strconv.FormatUint(execA, 10)
	for _, m := range items {
		if m["execution_id"] != execAStr {
			t.Fatalf("execution_id = %v, want %s", m["execution_id"], execAStr)
		}
		if _, ok := m["payload"].(map[string]any); !ok {
			t.Fatalf("payload = %T, want JSON object", m["payload"])
		}
		if _, ok := m["id"].(string); !ok {
			t.Fatalf("id = %T, want string (2^53 contract)", m["id"])
		}
	}
	// payload 内容保真锚(对抗审计 tests MEDIUM:DTO 投影换常量 {} 套件曾绿)
	// ——state_changed 行的 from/to 正文逐字断言。DESC 序:items[1] 是首插行。
	if p := items[1]["payload"].(map[string]any); p["from"] != "PENDING" || p["to"] != "EXECUTING" {
		t.Fatalf("node.state_changed payload = %v, want from=PENDING to=EXECUTING", p)
	}

	// T9.3 链列进 DTO(D-t63 #4「启链时加」兑现):DESC 序 items[1]=链首
	// (prev_hash=""),items[0] 的 prev_hash 咬合 items[1] 的 row_hash。
	prev0, _ := items[0]["prev_hash"].(string)
	prev1, _ := items[1]["prev_hash"].(string)
	row1, _ := items[1]["row_hash"].(string)
	if prev1 != "" {
		t.Fatalf("链首 prev_hash = %q, want \"\"", prev1)
	}
	if len(row1) != 64 {
		t.Fatalf("row_hash = %q, want 64-hex", row1)
	}
	if prev0 != row1 {
		t.Fatalf("items[0].prev_hash = %q, want 咬合 items[1].row_hash = %q", prev0, row1)
	}

	// 维度独立:execB 只回自己的行。
	code, body = doJSON(t, r, http.MethodGet,
		fmt.Sprintf("/api/v1/audit?execution_id=%d", execB), author, nil)
	if code != http.StatusOK || auditTotal(t, body) != 1 {
		t.Fatalf("query execB = %d total %v, want 200/1", code, body)
	}
	if items := auditItems(t, body); items[0]["event_type"] != audit.EventToolCallCompleted {
		t.Fatalf("execB event_type = %v", items[0]["event_type"])
	}

	// 404:执行缺行(GetExecution 钉 exec_date 的缺行面)。
	code, body = doJSON(t, r, http.MethodGet,
		"/api/v1/audit?execution_id=999999999999999999", author, nil)
	if code != http.StatusNotFound || body["error"] != "not_found" {
		t.Fatalf("missing execution = %d %v, want 404 not_found", code, body)
	}
	// 400:id 形态(非数字 / 0)。
	for _, bad := range []string{"abc", "0", "-1"} {
		code, body = doJSON(t, r, http.MethodGet,
			"/api/v1/audit?execution_id="+bad, author, nil)
		if code != http.StatusBadRequest || body["error"] != "bad_execution_id" {
			t.Fatalf("execution_id=%s = %d %v, want 400 bad_execution_id", bad, code, body)
		}
	}
}

// TestHTTPAuditQueryByTrace 维度 2:trace_id 跨执行过滤(T6.2 前生产恒
// NULL——端点就位,词表允许测试注入);无行 trace 200 空面(无父资源,
// 无 404 语义);坏格式 400。
func TestHTTPAuditQueryByTrace(t *testing.T) {
	r, dbh, sign := newTestServer(t)
	ids := seedIDs(t)
	author := sign(auth.RoleAuthor)
	execA := seedAuditExec(t, dbh, ids, uniqName("aud-ta"))
	execB := seedAuditExec(t, dbh, ids, uniqName("aud-tb"))

	appendAuditRow(t, dbh, ids, audit.Event{
		EventType: audit.EventNodeStateChanged, ExecutionID: execA,
		ActorType: audit.ActorUser, ActorID: "alice", TraceID: auditTestTrace,
	})
	appendAuditRow(t, dbh, ids, audit.Event{
		EventType: audit.EventToolCallCompleted, ExecutionID: execB,
		ActorType: audit.ActorWorker, ActorID: "worker-1", TraceID: auditTestTrace,
	})
	appendAuditRow(t, dbh, ids, audit.Event{
		EventType: audit.EventBranchEvaluated, ExecutionID: execA,
		ActorType: audit.ActorSystem, ActorID: "scheduler", // 无 trace
	})

	code, body := doJSON(t, r, http.MethodGet,
		"/api/v1/audit?trace_id="+auditTestTrace, author, nil)
	if code != http.StatusOK {
		t.Fatalf("query by trace = %d %v, want 200", code, body)
	}
	items := auditItems(t, body)
	if len(items) != 2 || auditTotal(t, body) != 2 {
		t.Fatalf("items = %d total = %d, want 2/2", len(items), auditTotal(t, body))
	}
	for _, m := range items {
		if m["trace_id"] != auditTestTrace {
			t.Fatalf("trace_id = %v, want %s", m["trace_id"], auditTestTrace)
		}
		if m["event_type"] == audit.EventBranchEvaluated {
			t.Fatalf("untracked row leaked into trace dimension")
		}
	}

	// 无行 trace:200 空面。
	code, body = doJSON(t, r, http.MethodGet,
		"/api/v1/audit?trace_id="+auditTestTraceEmpty, author, nil)
	if code != http.StatusOK || auditTotal(t, body) != 0 || len(auditItems(t, body)) != 0 {
		t.Fatalf("empty trace = %d total %v, want 200/0", code, body)
	}
	// 400:非 32 hex 小写。
	for _, bad := range []string{"xyz", "0123456789ABCDEF0123456789ABCDEF", "0123456789abcdef0123456789abcde"} {
		code, body = doJSON(t, r, http.MethodGet,
			"/api/v1/audit?trace_id="+bad, author, nil)
		if code != http.StatusBadRequest || body["error"] != "bad_trace_id" {
			t.Fatalf("trace_id=%s = %d %v, want 400 bad_trace_id", bad, code, body)
		}
	}
}

// TestHTTPAuditQueryByActorRoleGate 维度 3:actor_id 维度 admin-only
// (D-t63 #2)——author/approver 403 role_denied,admin 200;长度超宽 400。
func TestHTTPAuditQueryByActorRoleGate(t *testing.T) {
	r, dbh, sign := newTestServer(t)
	ids := seedIDs(t)
	exec := seedAuditExec(t, dbh, ids, uniqName("aud-ac"))
	appendAuditRow(t, dbh, ids, audit.Event{
		EventType: audit.EventNodeStateChanged, ExecutionID: exec,
		ActorType: audit.ActorUser, ActorID: "bob",
	})
	appendAuditRow(t, dbh, ids, audit.Event{
		EventType: audit.EventApprovalDecided, ExecutionID: exec,
		ActorType: audit.ActorUser, ActorID: "alice",
	})

	for _, roles := range [][]auth.Role{{auth.RoleAuthor}, {auth.RoleApprover}} {
		code, body := doJSON(t, r, http.MethodGet,
			"/api/v1/audit?actor_id=bob", sign(roles...), nil)
		if code != http.StatusForbidden || body["error"] != "role_denied" {
			t.Fatalf("roles %v = %d %v, want 403 role_denied", roles, code, body)
		}
	}
	// 400 先于 403:超宽 actor 在闸前被拒,非 admin 同形(参数校验与角色
	// 闸的次序锚——403 只对「可解释的查询」发放,防以 403 探测域宽)。
	long := strings.Repeat("x", 129)
	code, body := doJSON(t, r, http.MethodGet,
		"/api/v1/audit?actor_id="+long, sign(auth.RoleAuthor), nil)
	if code != http.StatusBadRequest || body["error"] != "bad_actor_id" {
		t.Fatalf("non-admin overlong actor = %d %v, want 400 bad_actor_id", code, body)
	}

	admin := sign(auth.RoleAdmin)
	code, body = doJSON(t, r, http.MethodGet, "/api/v1/audit?actor_id=bob", admin, nil)
	if code != http.StatusOK || auditTotal(t, body) != 1 {
		t.Fatalf("admin by actor = %d total %v, want 200/1", code, body)
	}
	if items := auditItems(t, body); items[0]["actor_id"] != "bob" {
		t.Fatalf("actor_id = %v, want bob", items[0]["actor_id"])
	}

	// admin 查无行人:200 空面。
	code, body = doJSON(t, r, http.MethodGet, "/api/v1/audit?actor_id=carol", admin, nil)
	if code != http.StatusOK || auditTotal(t, body) != 0 {
		t.Fatalf("admin unknown actor = %d total %v, want 200/0", code, body)
	}
	// 400:超宽(DDL actor_id VARCHAR(128)),admin 同面。
	code, body = doJSON(t, r, http.MethodGet, "/api/v1/audit?actor_id="+long, admin, nil)
	if code != http.StatusBadRequest || body["error"] != "bad_actor_id" {
		t.Fatalf("long actor = %d %v, want 400 bad_actor_id", code, body)
	}
	// 合法上界内侧锚(对抗审计 LOW:只锚越界侧时收紧常量无报警)。
	code, body = doJSON(t, r, http.MethodGet,
		"/api/v1/audit?actor_id="+strings.Repeat("y", 128), admin, nil)
	if code != http.StatusOK {
		t.Fatalf("128-char actor = %d %v, want 200", code, body)
	}
	// 字节精确语义锚(对抗审计 sql MEDIUM):actor_id 列继承 ai_ci 大小写
	// 折叠,谓词已显式 COLLATE utf8mb4_bin——大小写互异主体不得并进同一结果
	// (llm 事件 actor = 模型名,外部字符串)。
	for _, actor := range []string{"GPT-4o", "gpt-4o"} {
		appendAuditRow(t, dbh, ids, audit.Event{
			EventType: audit.EventLLMCallCompleted, ExecutionID: exec,
			ActorType: audit.ActorWorker, ActorID: actor,
		})
	}
	code, body = doJSON(t, r, http.MethodGet, "/api/v1/audit?actor_id=GPT-4o", admin, nil)
	if code != http.StatusOK || auditTotal(t, body) != 1 {
		t.Fatalf("case-fold probe = %d total %v, want 200/1", code, body)
	}
}

// TestHTTPAuditQueryValidation 契约 400 面:维度互斥恰一 / 时间范围 /
// 分页上界;401 未认证。
func TestHTTPAuditQueryValidation(t *testing.T) {
	r, _, sign := newTestServer(t)
	author := sign(auth.RoleAuthor)
	cases := []struct {
		name    string
		query   string
		wantErr string
	}{
		{"no dimension", "/api/v1/audit", "bad_query"},
		{"all three dimensions", "/api/v1/audit?execution_id=1&trace_id=" + auditTestTrace + "&actor_id=bob", "bad_query"},
		{"two dimensions", "/api/v1/audit?trace_id=" + auditTestTrace + "&actor_id=bob", "bad_query"},
		{"bad from format", "/api/v1/audit?trace_id=" + auditTestTrace + "&from=notatime", "bad_time_range"},
		{"bad to format", "/api/v1/audit?trace_id=" + auditTestTrace + "&to=notatime", "bad_time_range"},
		{"from after to", "/api/v1/audit?trace_id=" + auditTestTrace +
			"&from=2026-09-25T10:00:00Z&to=2026-09-25T09:00:00Z", "bad_time_range"},
		{"zero limit", "/api/v1/audit?trace_id=" + auditTestTrace + "&limit=0", "bad_limit"},
		{"limit over max", "/api/v1/audit?trace_id=" + auditTestTrace + "&limit=101", "bad_limit"},
		{"negative offset", "/api/v1/audit?trace_id=" + auditTestTrace + "&offset=-1", "bad_offset"},
		{"offset over max", "/api/v1/audit?trace_id=" + auditTestTrace + "&offset=10001", "bad_offset"},
	}
	for _, tc := range cases {
		code, body := doJSON(t, r, http.MethodGet, tc.query, author, nil)
		if code != http.StatusBadRequest || body["error"] != tc.wantErr {
			t.Fatalf("%s = %d %v, want 400 %s", tc.name, code, body, tc.wantErr)
		}
	}
	// 401:未认证(路由挂 Authenticate)。
	code, body := doJSON(t, r, http.MethodGet,
		"/api/v1/audit?trace_id="+auditTestTrace, "", nil)
	if code != http.StatusUnauthorized || body["error"] != "missing_token" {
		t.Fatalf("unauthenticated = %d %v, want 401 missing_token", code, body)
	}
}

// TestHTTPAuditQueryTimeRangeAndPaging:event_at 闭区间过滤 + limit/offset
// 分页 + DESC 序。
func TestHTTPAuditQueryTimeRangeAndPaging(t *testing.T) {
	r, dbh, sign := newTestServer(t)
	ids := seedIDs(t)
	author := sign(auth.RoleAuthor)
	exec := seedAuditExec(t, dbh, ids, uniqName("aud-pg"))
	appendAuditRow(t, dbh, ids, audit.Event{
		EventType: audit.EventNodeStateChanged, ExecutionID: exec,
		ActorType: audit.ActorSystem, ActorID: "scheduler",
	})
	appendAuditRow(t, dbh, ids, audit.Event{
		EventType: audit.EventBranchEvaluated, ExecutionID: exec,
		ActorType: audit.ActorSystem, ActorID: "scheduler",
	})
	appendAuditRow(t, dbh, ids, audit.Event{
		EventType: audit.EventToolCallCompleted, ExecutionID: exec,
		ActorType: audit.ActorWorker, ActorID: "worker-1",
	})
	base := fmt.Sprintf("/api/v1/audit?execution_id=%d", exec)
	lo := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	hi := time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)

	// 范围包住全部落行时刻。
	code, body := doJSON(t, r, http.MethodGet, base+"&from="+lo+"&to="+hi, author, nil)
	if code != http.StatusOK || auditTotal(t, body) != 3 {
		t.Fatalf("range covering = %d total %v, want 200/3", code, body)
	}
	// to 在落行前:空(event_at = DB NOW(3),恒 now 侧)。
	code, body = doJSON(t, r, http.MethodGet, base+"&to="+past, author, nil)
	if code != http.StatusOK || auditTotal(t, body) != 0 {
		t.Fatalf("range excluding = %d total %v, want 200/0", code, body)
	}
	// from==to 闭区间接受(单点区间,空窗 200 面)。
	code, body = doJSON(t, r, http.MethodGet, base+"&from="+past+"&to="+past, author, nil)
	if code != http.StatusOK || auditTotal(t, body) != 0 {
		t.Fatalf("point range = %d total %v, want 200/0", code, body)
	}

	// 分页:DESC 顺序逐页。
	code, body = doJSON(t, r, http.MethodGet, base+"&limit=2", author, nil)
	if code != http.StatusOK {
		t.Fatalf("page1 = %d %v", code, body)
	}
	items := auditItems(t, body)
	if len(items) != 2 || auditTotal(t, body) != 3 {
		t.Fatalf("page1 items = %d total = %d, want 2/3", len(items), auditTotal(t, body))
	}
	wantOrder := []string{audit.EventToolCallCompleted, audit.EventBranchEvaluated}
	for i, want := range wantOrder {
		if items[i]["event_type"] != want {
			t.Fatalf("page1[%d].event_type = %v, want %s", i, items[i]["event_type"], want)
		}
	}
	code, body = doJSON(t, r, http.MethodGet, base+"&limit=2&offset=2", author, nil)
	if code != http.StatusOK {
		t.Fatalf("page2 = %d %v", code, body)
	}
	if items := auditItems(t, body); len(items) != 1 || items[0]["event_type"] != audit.EventNodeStateChanged {
		t.Fatalf("page2 = %v, want [node.state_changed]", items)
	}

	// 闭区间 + 列选 + id tie-break 锚(对抗审计 tests MEDIUM×2 / LOW):raw
	// INSERT 两行同 event_at 定值、id 递增,事件时刻(2026-01-02)与落库
	// 时刻(created_at=NOW)结构性分离。from==to=事件时刻原值必须命中两行
	// 且 id DESC;谓词若换 created_at、开闭区间互换、tie-break 反向即红。
	// (行落执行同分区:exec_date 从执行行 DATE_FORMAT 单源读回。)
	var execDate string
	if err := dbh.QueryRow(
		`SELECT DATE_FORMAT(created_at, '%Y-%m-%d') FROM workflow_execution WHERE id = ?`,
		exec).Scan(&execDate); err != nil {
		t.Fatalf("read exec_date: %v", err)
	}
	fixedAt := time.Date(2026, 1, 2, 3, 4, 5, 123000000, time.UTC)
	anchorLow, err := ids.Next()
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	anchorHigh, err := ids.Next()
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	for _, c := range []struct {
		id  uint64
		evt string
	}{
		{anchorLow, audit.EventNodeDeadlineExtended},
		{anchorHigh, audit.EventNodeStateChanged},
	} {
		if _, err := dbh.Exec(`
INSERT INTO audit_log (id, exec_date, event_at, event_type, execution_id, actor_type, actor_id, payload)
VALUES (?, ?, ?, ?, ?, 'user', 'anchor', '{}')`,
			c.id, execDate, fixedAt, c.evt, exec); err != nil {
			t.Fatalf("seed anchor rows: %v", err)
		}
	}
	fixed := fixedAt.Format(time.RFC3339Nano)
	code, body = doJSON(t, r, http.MethodGet, base+"&from="+fixed+"&to="+fixed, author, nil)
	if code != http.StatusOK || auditTotal(t, body) != 2 {
		t.Fatalf("closed-boundary probe = %d total %v, want 200/2", code, body)
	}
	anchorItems := auditItems(t, body)
	if anchorItems[0]["id"] != strconv.FormatUint(anchorHigh, 10) ||
		anchorItems[1]["id"] != strconv.FormatUint(anchorLow, 10) {
		t.Fatalf("id DESC tie-break = %v,%v, want %d,%d",
			anchorItems[0]["id"], anchorItems[1]["id"], anchorHigh, anchorLow)
	}
	above := fixedAt.Add(time.Millisecond).Format(time.RFC3339Nano)
	code, body = doJSON(t, r, http.MethodGet, base+"&from="+above+"&to="+above, author, nil)
	if code != http.StatusOK || auditTotal(t, body) != 0 {
		t.Fatalf("above-boundary probe = %d total %v, want 200/0", code, body)
	}
}

// TestHTTPAuditQueryUsageColumns usage/cost 冗余列组(llm.call.completed
// 专属)与非空节点列的扫描面:列序/类型回归在此钉死(T6.4 成本聚合的消费
// 前置);exec_date 线格式与 GET /executions/:id 审计指针逐字同形
// (DATE_FORMAT 单源,防 DATE 直扫的 RFC3339Nano 漂移)。
func TestHTTPAuditQueryUsageColumns(t *testing.T) {
	r, dbh, sign := newTestServer(t)
	ids := seedIDs(t)
	author := sign(auth.RoleAuthor)
	exec := seedAuditExec(t, dbh, ids, uniqName("aud-usage"))
	nodeExec, err := ids.Next()
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	attempt, err := ids.Next()
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	appendAuditRow(t, dbh, ids, audit.Event{
		EventType: audit.EventLLMCallCompleted, ExecutionID: exec,
		NodeExecID: nodeExec, NodeID: "summarize", AttemptID: attempt,
		ActorType: audit.ActorWorker, ActorID: "worker-1",
		Usage: &engine.AttemptUsage{
			Model: "test-model", PromptTokens: 10, CompletionTokens: 20,
			TotalTokens: 30, Billed: true, CostAmount: "2.500000",
			CostCurrency: "USD", Estimated: true,
		},
	})
	// pii_masked=true 读面分支锚(对抗审计 LOW:写侧 sanitize 有覆盖,读侧
	// scan→DTO 的 true 侧无人行使——21 列手扫错位正是高危回归形态)。
	appendAuditRow(t, dbh, ids, audit.Event{
		EventType: audit.EventNodeStateChanged, ExecutionID: exec, NodeID: "piirow",
		ActorType: audit.ActorUser, ActorID: "bob",
		Payload: json.RawMessage(`{"user_data":{"note":"sekret"}}`),
	})

	code, body := doJSON(t, r, http.MethodGet,
		fmt.Sprintf("/api/v1/audit?execution_id=%d", exec), author, nil)
	if code != http.StatusOK || auditTotal(t, body) != 2 {
		t.Fatalf("usage query = %d total %v, want 200/2", code, body)
	}
	m := auditItems(t, body)[1] // DESC 序:pii 行最新在前,llm 行第二
	if pi := auditItems(t, body)[0]; pi["pii_masked"] != true {
		t.Fatalf("pii_masked = %v, want true", pi["pii_masked"])
	} else if ud, ok := pi["payload"].(map[string]any)["user_data"].(map[string]any); !ok || ud["note"] != "***" {
		t.Fatalf("user_data leaf = %v, want *** (write-side mask visible on read)", pi["payload"])
	}
	if m["model"] != "test-model" || m["prompt_tokens"] != float64(10) ||
		m["completion_tokens"] != float64(20) || m["total_tokens"] != float64(30) {
		t.Fatalf("usage columns = %v", m)
	}
	if m["cost_amount"] != float64(2.5) || m["cost_currency"] != "USD" || m["cost_estimated"] != true {
		t.Fatalf("cost columns = %v", m)
	}
	if m["node_id"] != "summarize" ||
		m["node_execution_id"] != strconv.FormatUint(nodeExec, 10) ||
		m["attempt_id"] != strconv.FormatUint(attempt, 10) {
		t.Fatalf("node columns = %v", m)
	}
	if m["pii_masked"] != false {
		t.Fatalf("pii_masked = %v, want false", m["pii_masked"])
	}
	// exec_date 线格式:与执行行 DATE_FORMAT 单源逐字同形(期望值从 DB 读,
	// 不引入宿主钟)。
	var wantDate string
	if err := dbh.QueryRow(
		`SELECT DATE_FORMAT(created_at, '%Y-%m-%d') FROM workflow_execution WHERE id = ?`,
		exec).Scan(&wantDate); err != nil {
		t.Fatalf("read exec_date: %v", err)
	}
	if m["exec_date"] != wantDate {
		t.Fatalf("exec_date = %v, want %q", m["exec_date"], wantDate)
	}
}

// TestHTTPAuditVisibilityFlushSemantics D-t63 #1 契约锚:audit_pending
// 暂存行 flush 前不可见、FlushAll 后可见(仅读 audit_log,无 UNION)。
func TestHTTPAuditVisibilityFlushSemantics(t *testing.T) {
	r, dbh, sign := newTestServer(t)
	ids := seedIDs(t)
	author := sign(auth.RoleAuthor)
	exec := seedAuditExec(t, dbh, ids, uniqName("aud-fl"))
	q := fmt.Sprintf("/api/v1/audit?execution_id=%d", exec)

	code, body := doJSON(t, r, http.MethodGet, q, author, nil)
	if code != http.StatusOK || auditTotal(t, body) != 0 {
		t.Fatalf("baseline = %d total %v, want 200/0", code, body)
	}

	staged := enqueueAuditRow(t, dbh, ids, audit.Event{
		EventType: audit.EventNodeStateChanged, ExecutionID: exec,
		ActorType: audit.ActorSystem, ActorID: "scheduler",
	})
	code, body = doJSON(t, r, http.MethodGet, q, author, nil)
	if code != http.StatusOK || auditTotal(t, body) != 0 {
		t.Fatalf("staged visible before flush = %d total %v, want 200/0", code, body)
	}

	moved, err := audit.FlushAll(context.Background(), dbh, 500, 10)
	if err != nil || moved.Flushed != 1 { // T6.4:聚合签名(Flushed = 搬运行数)
		t.Fatalf("FlushAll = %+v, %v, want Flushed=1/<nil>", moved, err)
	}
	code, body = doJSON(t, r, http.MethodGet, q, author, nil)
	if code != http.StatusOK || auditTotal(t, body) != 1 {
		t.Fatalf("after flush = %d total %v, want 200/1", code, body)
	}
	items := auditItems(t, body)
	if items[0]["id"] != strconv.FormatUint(staged, 10) {
		t.Fatalf("id = %v, want staged %d (flush 保 id 搬运)", items[0]["id"], staged)
	}
}

// TestHTTPAuditTriggeredImmediatelyVisible 同步路径可见性:真链触发后
// execution.triggered(Append 直写)立即可查;actor = JWT sub。
func TestHTTPAuditTriggeredImmediatelyVisible(t *testing.T) {
	dbh := sqltest.MustOpenAt(t, "braid_api_test")
	cleanExecutionChain(t, dbh)
	schedIDs, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	sched, err := scheduler.New(scheduler.Deps{DB: dbh, IDs: schedIDs})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	r, _, sign := newTestServerWithTrigger(t, sched)
	author := sign(auth.RoleAuthor)
	name := uniqName("aud-int")
	publishViaAPI(t, r, author, name)

	code, body := doJSON(t, r, http.MethodPost, "/api/v1/executions", author,
		[]byte(fmt.Sprintf(`{"definition_name":%q}`, name)))
	if code != http.StatusCreated {
		t.Fatalf("trigger = %d %v, want 201", code, body)
	}
	execID := body["id"].(string)

	code, body = doJSON(t, r, http.MethodGet,
		"/api/v1/audit?execution_id="+execID, author, nil)
	if code != http.StatusOK || auditTotal(t, body) < 1 {
		t.Fatalf("query after trigger = %d total %v, want 200/≥1", code, body)
	}
	found := false
	for _, m := range auditItems(t, body) {
		if m["event_type"] == audit.EventExecutionTriggered && m["actor_id"] == "alice" {
			found = true
		}
	}
	if !found {
		t.Fatalf("execution.triggered by alice not found in %v", body)
	}
}
