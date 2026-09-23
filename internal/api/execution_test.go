package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"braid/internal/audit"
	"braid/internal/auth"
	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
)

// cleanExecutionChain 按子→父清空六表(FK RESTRICT 决定顺序)。真实链
// 集成测试会落 outbox/execution/audit 行,共享 schema 下后续用例的
// workflow_definition 单表清理会被孤儿引用阻拦(FK 1701)——统一走本链。
func cleanExecutionChain(t *testing.T, dbh *sql.DB) {
	t.Helper()
	for _, table := range []string{
		"audit_log", "outbox", "node_attempt", "node_execution", "workflow_execution", "workflow_definition",
	} {
		if _, err := dbh.Exec("DELETE FROM " + table); err != nil {
			t.Fatalf("clean %s: %v", table, err)
		}
	}
}

// fakeTrigger 是 ExecutionTrigger 缝的替身:回放固定 id 或注入错误,零
// DB 写入——「触发后三表行集」的真链断言归真 scheduler 装配的集成测试
// (server_test.go),此处只钉 HTTP 契约面。
type fakeTrigger struct {
	id  uint64
	err error
}

func (f fakeTrigger) Instantiate(context.Context, scheduler.InstantiateParams) (uint64, error) {
	return f.id, f.err
}

// publishViaAPI 建草稿并发布(触发前置:目标必须 PUBLISHED),返回版本号。
func publishViaAPI(t *testing.T, r *gin.Engine, token, name string) uint32 {
	t.Helper()
	created := createViaAPI(t, r, token, dslDoc(name))
	id := created["id"].(string)
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/drafts/"+id+"/publish", token, nil)
	if code != http.StatusOK {
		t.Fatalf("publish %s: code %d, body %v", name, code, body)
	}
	return uint32(body["version"].(float64))
}

// TestHTTPTriggerExecution 钉死触发面契约(fake 缝):
//   - 201 回显:id 字符串化(2^53 精度契约)、PENDING/API/钉版回显;
//   - 400 面:缺 name / version=0 / input 非对象 / 坏 JSON;
//   - 404 面:无发布版本的 name;
//   - 409 面:显式指向 ARCHIVED 版本(缺省解析对全归档 name 回 404)。
func TestHTTPTriggerExecution(t *testing.T) {
	// 2^53+1:float64 精确整数界外第一奇数——id 若被数值化,JS/Python
	// 消费方静默折叠成相邻 id(definitionDTO 同款契约锚)。
	const execID = uint64(1)<<53 + 1
	r, _, sign := newTestServerWithTrigger(t, fakeTrigger{id: execID})
	author := sign(auth.RoleAuthor)
	name := uniqName("trig")
	version := publishViaAPI(t, r, author, name)

	code, body := doJSON(t, r, http.MethodPost, "/api/v1/executions", author,
		[]byte(fmt.Sprintf(`{"definition_name":%q,"input":{"q":"hi"}}`, name)))
	if code != http.StatusCreated {
		t.Fatalf("trigger latest = %d %v, want 201", code, body)
	}
	if body["id"] != "9007199254740993" {
		t.Fatalf("id = %v (%T), want string \"9007199254740993\"", body["id"], body["id"])
	}
	if body["status"] != "PENDING" || body["trigger_type"] != "API" || body["triggered_by"] != "alice" {
		t.Fatalf("ack = %v", body)
	}
	if body["definition_name"] != name || body["definition_version"] != float64(version) {
		t.Fatalf("pinned echo = %v (want %s v%d)", body, name, version)
	}

	// 显式版本触发(钉住指定版本而非 Latest)。
	code, body = doJSON(t, r, http.MethodPost, "/api/v1/executions", author,
		[]byte(fmt.Sprintf(`{"definition_name":%q,"version":%d}`, name, version)))
	if code != http.StatusCreated || body["definition_version"] != float64(version) {
		t.Fatalf("trigger pinned = %d %v, want 201 v%d", code, body, version)
	}

	// input 显式 null 与缺省同义(Instantiate 零输入→{} 契约)。
	code, _ = doJSON(t, r, http.MethodPost, "/api/v1/executions", author,
		[]byte(fmt.Sprintf(`{"definition_name":%q,"input":null}`, name)))
	if code != http.StatusCreated {
		t.Fatalf("trigger null input = %d, want 201", code)
	}

	for _, tc := range []struct {
		what string
		body string
		want int
		code string
	}{
		{"missing name", `{"version":1}`, http.StatusBadRequest, "bad_definition_name"},
		{"bad name chars", `{"definition_name":"Bad_Name"}`, http.StatusBadRequest, "bad_definition_name"},
		{"name too long", fmt.Sprintf(`{"definition_name":"%s"}`, strings.Repeat("a", 65)), http.StatusBadRequest, "bad_definition_name"},
		{"zero version", fmt.Sprintf(`{"definition_name":%q,"version":0}`, name), http.StatusBadRequest, "bad_version"},
		{"array input", fmt.Sprintf(`{"definition_name":%q,"input":[1,2]}`, name), http.StatusBadRequest, "bad_input"},
		{"scalar input", fmt.Sprintf(`{"definition_name":%q,"input":"x"}`, name), http.StatusBadRequest, "bad_input"},
		{"malformed json", `{"definition_name":`, http.StatusBadRequest, "bad_request"},
		{"unknown name", `{"definition_name":"ghost-no-such"}`, http.StatusNotFound, "not_found"},
	} {
		got, body := doJSON(t, r, http.MethodPost, "/api/v1/executions", author, []byte(tc.body))
		if got != tc.want || body["error"] != tc.code {
			t.Fatalf("%s = %d %v, want %d %q", tc.what, got, body, tc.want, tc.code)
		}
	}

	// 显式指向 ARCHIVED 版本 → 409(目标明确,被生命周期闸拒绝);
	// 同 name 全归档后,缺省解析回 404(禁新触发)。
	archName := uniqName("trig-arch")
	archVer := publishViaAPI(t, r, author, archName)
	acode, _ := doJSON(t, r, http.MethodPost,
		"/api/v1/definitions/"+archName+"/versions/"+fmt.Sprint(archVer)+"/archive", author, nil)
	if acode != http.StatusOK {
		t.Fatalf("archive = %d, want 200", acode)
	}
	code, body = doJSON(t, r, http.MethodPost, "/api/v1/executions", author,
		[]byte(fmt.Sprintf(`{"definition_name":%q,"version":%d}`, archName, archVer)))
	if code != http.StatusConflict || body["error"] != "conflict" {
		t.Fatalf("archived trigger = %d %v, want 409 conflict", code, body)
	}
	code, _ = doJSON(t, r, http.MethodPost, "/api/v1/executions", author,
		[]byte(fmt.Sprintf(`{"definition_name":%q}`, archName)))
	if code != http.StatusNotFound {
		t.Fatalf("all-archived latest trigger = %d, want 404", code)
	}
}

// TestHTTPTriggerInstantiateErrorMapping:Instantiate 错误面——
// engine.ErrNotFound(解析→物化之间被归档的 TOCTOU 竞态窗)→ 404;其余
// 错误 → 500 internal 且响应体不携带任何底层细节。
func TestHTTPTriggerInstantiateErrorMapping(t *testing.T) {
	r, _, sign := newTestServerWithTrigger(t,
		fakeTrigger{err: fmt.Errorf("%w: archived in race window", engine.ErrNotFound)})
	author := sign(auth.RoleAuthor)
	name := uniqName("trig-race")
	publishViaAPI(t, r, author, name)
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/executions", author,
		[]byte(fmt.Sprintf(`{"definition_name":%q}`, name)))
	if code != http.StatusNotFound || body["error"] != "not_found" {
		t.Fatalf("race-window archive = %d %v, want 404", code, body)
	}

	r2, _, sign2 := newTestServerWithTrigger(t, fakeTrigger{err: errors.New("boom")})
	author2 := sign2(auth.RoleAuthor)
	name2 := uniqName("trig-boom")
	publishViaAPI(t, r2, author2, name2)
	code, body = doJSON(t, r2, http.MethodPost, "/api/v1/executions", author2,
		[]byte(fmt.Sprintf(`{"definition_name":%q}`, name2)))
	if code != http.StatusInternalServerError || body["error"] != "internal" || len(body) != 1 {
		t.Fatalf("instantiate failure = %d %v, want bare 500 internal", code, body)
	}
}

// TestHTTPTriggerAuditFailureIsNonBlocking:execution.triggered 审计行的
// 二段事务缺口契约(GATE 1 拍板)——审计写失败(此处置身于 fake 缝:执行
// 行不存在,Append 的 execution 一致读 ErrNotFound)只 slog 观测,响应面
// 仍是 201;500 会诱导客户端重试造成重复触发,事实源保底 = 执行行
// triggered_by 列。正向行断言(审计行确实落库)归真链集成测试。
func TestHTTPTriggerAuditFailureIsNonBlocking(t *testing.T) {
	r, _, sign := newTestServerWithTrigger(t, fakeTrigger{id: (uint64(1) << 53) + 1})
	author := sign(auth.RoleAuthor)
	name := uniqName("trig-audit")
	publishViaAPI(t, r, author, name)
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/executions", author,
		[]byte(fmt.Sprintf(`{"definition_name":%q}`, name)))
	if code != http.StatusCreated || body["id"] == nil {
		t.Fatalf("audit-failure trigger = %d %v, want 201 (non-blocking)", code, body)
	}
}

// TestHTTPTriggerExecutionIntegration:真链(t13)——经 API 建草稿→发布→
// POST /executions,直查 DB 断言 Instantiate 三表行集 + execution.triggered
// 审计行(T5.5 验收「触发后 execution 创建并推进」的 api 侧半边)。零
// Redis:scheduler.Deps 仅 MySQL+snowflake,outbox 落行即触发职责边界,
// 推进半边(PENDING→RUNNING)归 relay/worker 进程的既有包测试。
func TestHTTPTriggerExecutionIntegration(t *testing.T) {
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
	name := uniqName("trig-int")
	publishViaAPI(t, r, author, name)

	code, body := doJSON(t, r, http.MethodPost, "/api/v1/executions", author,
		[]byte(fmt.Sprintf(`{"definition_name":%q,"input":{"q":"integration"}}`, name)))
	if code != http.StatusCreated {
		t.Fatalf("trigger = %d %v, want 201", code, body)
	}
	execID := body["id"].(string)

	// workflow_execution:钉版四件套(hash 与定义行逐字一致)+ 触发者
	// 事实源 + input 原文({{inputs.*}} 真相源按原文保留)。
	var (
		wfStatus, trigType, trigBy, defName, defHash, defHash2, inputRaw string
		defID                                                            uint64
		defVer                                                           uint32
	)
	err = dbh.QueryRow(`
SELECT e.status, e.trigger_type, e.triggered_by, e.definition_id, e.definition_name,
       e.definition_version, e.definition_hash, d.dsl_hash, e.input
FROM workflow_execution e JOIN workflow_definition d ON d.id = e.definition_id
WHERE e.id = ?`, execID).Scan(
		&wfStatus, &trigType, &trigBy, &defID, &defName, &defVer, &defHash, &defHash2, &inputRaw)
	if err != nil {
		t.Fatalf("read workflow_execution: %v", err)
	}
	if wfStatus != string(engine.WorkflowPending) || trigType != "API" || trigBy != "alice" {
		t.Fatalf("execution row = %s/%s/%s", wfStatus, trigType, trigBy)
	}
	if defName != name || defHash != defHash2 {
		t.Fatalf("pinned = %s (hash %s vs %s)", defName, defHash, defHash2)
	}
	if body["definition_version"] != float64(defVer) {
		t.Fatalf("ack version %v vs row %d", body["definition_version"], defVer)
	}
	var in map[string]any
	if err := json.Unmarshal([]byte(inputRaw), &in); err != nil || in["q"] != "integration" {
		t.Fatalf("input = %q, want原文 {q:integration}", inputRaw)
	}

	// 显式 null input 归一:DB 落 {} 而非 JSON null(DDL「零输入存 {}」
	// 契约;评审 MEDIUM 回归锚——Instantiate 的 len==0 替换不认 "null"
	// 字面量,api 侧必须前置归一)。
	code, nb := doJSON(t, r, http.MethodPost, "/api/v1/executions", author,
		[]byte(fmt.Sprintf(`{"definition_name":%q,"input":null}`, name)))
	if code != http.StatusCreated {
		t.Fatalf("null-input trigger = %d %v, want 201", code, nb)
	}
	var nullInput string
	if err := dbh.QueryRow(`SELECT input FROM workflow_execution WHERE id = ?`,
		nb["id"].(string)).Scan(&nullInput); err != nil || nullInput != "{}" {
		t.Fatalf("null input stored = %q (err %v), want {}", nullInput, err)
	}

	// node_execution:每 DSL 节点恰一行、全 PENDING、type 与 attempts 快照。
	type nodeRow struct {
		id, typ, status string
		attempts        int
	}
	rows, err := dbh.Query(
		`SELECT node_id, type, status, attempts FROM node_execution WHERE execution_id = ? ORDER BY node_id`, execID)
	if err != nil {
		t.Fatalf("query node_execution: %v", err)
	}
	defer rows.Close() //nolint:errcheck // 只读路径
	var nodes []nodeRow
	for rows.Next() {
		var n nodeRow
		if err := rows.Scan(&n.id, &n.typ, &n.status, &n.attempts); err != nil {
			t.Fatalf("scan node_execution: %v", err)
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate node_execution: %v", err)
	}
	// dslDoc 单节点 greet/llm(uk_node_exec 确定性 = node_id 字典序)。
	if len(nodes) != 1 || nodes[0].id != "greet" || nodes[0].typ != "llm" ||
		nodes[0].status != "PENDING" || nodes[0].attempts != 0 {
		t.Fatalf("nodes = %+v, want single greet/llm/PENDING/0", nodes)
	}

	// outbox:恰一行 execution.start(PENDING),确定性 TaskID exec:<id>。
	var obCount int
	if err := dbh.QueryRow(`SELECT COUNT(*) FROM outbox WHERE execution_id = ?`, execID).Scan(&obCount); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	var obType, taskID, obStatus string
	if err := dbh.QueryRow(
		`SELECT event_type, task_id, status FROM outbox WHERE execution_id = ?`, execID).
		Scan(&obType, &taskID, &obStatus); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	if obCount != 1 || obType != scheduler.EventTypeExecutionStart ||
		taskID != "exec:"+execID || obStatus != "PENDING" {
		t.Fatalf("outbox = %d rows %s/%s/%s", obCount, obType, taskID, obStatus)
	}

	// audit_log:恰一行 execution.triggered,执行级(node_id NULL),actor =
	// JWT sub,exec_date 与执行行同日(writer 单源物化公式),payload 仅
	// id/枚举(ExecutionTriggeredPayload 契约)。
	var aCount int
	if err := dbh.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE execution_id = ?`, execID).Scan(&aCount); err != nil {
		t.Fatalf("count audit_log: %v", err)
	}
	var aType, aActorType, aActorID, aPayload string
	var aNodeID sql.NullString
	if err := dbh.QueryRow(
		`SELECT event_type, actor_type, actor_id, node_id, payload FROM audit_log WHERE execution_id = ?`, execID).
		Scan(&aType, &aActorType, &aActorID, &aNodeID, &aPayload); err != nil {
		t.Fatalf("read audit_log: %v", err)
	}
	if aCount != 1 || aType != audit.EventExecutionTriggered ||
		aActorType != audit.ActorUser || aActorID != "alice" || aNodeID.Valid {
		t.Fatalf("audit = %d rows %s/%s/%s node_id=%v", aCount, aType, aActorType, aActorID, aNodeID)
	}
	var pl audit.ExecutionTriggeredPayload
	if err := json.Unmarshal([]byte(aPayload), &pl); err != nil ||
		pl.DefinitionID != defID || pl.TriggerType != "API" {
		t.Fatalf("audit payload = %q, want def %d/API", aPayload, defID)
	}
	var sameDay bool
	if err := dbh.QueryRow(`
SELECT a.exec_date = DATE(e.created_at) FROM audit_log a
JOIN workflow_execution e ON e.id = a.execution_id WHERE a.execution_id = ?`, execID).
		Scan(&sameDay); err != nil || !sameDay {
		t.Fatalf("exec_date mismatch: %v same=%v", err, sameDay)
	}
}

// realChainServer:真链装配(t13):真 scheduler 触发缝 + 发布一个可触发
// 定义,返回 (路由, 已触发执行 id 字符串)。S4/S5 查询面测试共用。
func realChainServer(t *testing.T) (*gin.Engine, string) {
	t.Helper()
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
	name := uniqName("trig-q")
	publishViaAPI(t, r, author, name)
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/executions", author,
		[]byte(fmt.Sprintf(`{"definition_name":%q,"input":{"q":"integration"}}`, name)))
	if code != http.StatusCreated {
		t.Fatalf("trigger: code %d, body %v", code, body)
	}
	return r, body["id"].(string)
}

// TestHTTPGetExecution:detail 查询契约(T5.5 验收「查询返回节点状态与
// 审计指针」):nodes[] 字典序 + audit 指针(exec_date 定位键;trace_id
// T6.2 前恒 null 不伪造);错误面 bad_id 400 / 未知 id 404;读面任意
// 有效角色。
func TestHTTPGetExecution(t *testing.T) {
	r, execID := realChainServer(t)

	// 读面:approver(非写面角色)可查。
	approver, err := auth.SignToken(testSecret, time.Hour, "alice", []auth.Role{auth.RoleApprover})
	if err != nil {
		t.Fatalf("sign approver: %v", err)
	}
	code, detail := doJSON(t, r, http.MethodGet, "/api/v1/executions/"+execID, approver, nil)
	if code != http.StatusOK {
		t.Fatalf("detail = %d %v, want 200", code, detail)
	}
	if detail["id"] != execID || detail["status"] != "PENDING" || detail["triggered_by"] != "alice" {
		t.Fatalf("detail = %v", detail)
	}
	if in, ok := detail["input"].(map[string]any); !ok || in["q"] != "integration" {
		t.Fatalf("input = %v, want原文回显", detail["input"])
	}
	if detail["outputs"] != nil {
		t.Fatalf("outputs = %v, want null(未终态)", detail["outputs"])
	}
	auditPtr, ok := detail["audit"].(map[string]any)
	if !ok {
		t.Fatalf("audit pointer missing: %v", detail)
	}
	if _, err := time.Parse("2006-01-02", auditPtr["exec_date"].(string)); err != nil {
		t.Fatalf("exec_date = %v, want YYYY-MM-DD", auditPtr["exec_date"])
	}
	if auditPtr["trace_id"] != nil {
		t.Fatalf("trace_id = %v, want null(T6.2 前不伪造)", auditPtr["trace_id"])
	}
	nodes, ok := detail["nodes"].([]any)
	if !ok || len(nodes) != 1 {
		t.Fatalf("nodes = %v, want single row", detail["nodes"])
	}
	n := nodes[0].(map[string]any)
	if n["node_id"] != "greet" || n["type"] != "llm" || n["status"] != "PENDING" || n["attempts"] != float64(0) {
		t.Fatalf("node = %v", n)
	}

	// 错误面:形态非法 400(区别于 404),未知 id 404。
	code, body := doJSON(t, r, http.MethodGet, "/api/v1/executions/abc", approver, nil)
	if code != http.StatusBadRequest || body["error"] != "bad_id" {
		t.Fatalf("bad id = %d %v, want 400 bad_id", code, body)
	}
	code, body = doJSON(t, r, http.MethodGet, "/api/v1/executions/999999", approver, nil)
	if code != http.StatusNotFound || body["error"] != "not_found" {
		t.Fatalf("unknown id = %d %v, want 404", code, body)
	}
}

// TestHTTPListExecutions:列表契约(真链):跨版本 name 过滤(DDL
// idx_exec_name_created 钦定)、status 白名单、分页 envelope 与 definitions
// 同构、created_at DESC 顺序、空结果 items=[] 而非 null。
func TestHTTPListExecutions(t *testing.T) {
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
	nameA, nameB := uniqName("trg-la"), uniqName("trg-lb")
	publishViaAPI(t, r, author, nameA)
	publishViaAPI(t, r, author, nameB)
	var lastID string
	for _, n := range []string{nameA, nameA, nameB} {
		code, body := doJSON(t, r, http.MethodPost, "/api/v1/executions", author,
			[]byte(fmt.Sprintf(`{"definition_name":%q}`, n)))
		if code != http.StatusCreated {
			t.Fatalf("trigger %s: %d %v", n, code, body)
		}
		lastID = body["id"].(string)
	}

	// 全量:total=3,最新在前(created_at DESC, id DESC),行视图无 nodes。
	code, body := doJSON(t, r, http.MethodGet, "/api/v1/executions", author, nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d %v", code, body)
	}
	page := body["page"].(map[string]any)
	items := body["items"].([]any)
	if page["total"] != float64(3) || len(items) != 3 {
		t.Fatalf("page = %v items %d, want total 3", page, len(items))
	}
	first := items[0].(map[string]any)
	if first["id"] != lastID || first["status"] != "PENDING" {
		t.Fatalf("first = %v, want latest %s PENDING", first, lastID)
	}
	if _, hasNodes := first["nodes"]; hasNodes {
		t.Fatalf("list row carries nodes: %v", first)
	}
	if _, hasInput := first["input"]; hasInput {
		t.Fatalf("list row carries input body: %v", first)
	}

	// 跨版本 name 过滤;name+status 组合(动态 WHERE 的 AND 分支)。
	code, body = doJSON(t, r, http.MethodGet, "/api/v1/executions?name="+nameA, author, nil)
	if code != http.StatusOK || body["page"].(map[string]any)["total"] != float64(2) {
		t.Fatalf("name filter = %d %v", code, body)
	}
	code, body = doJSON(t, r, http.MethodGet,
		"/api/v1/executions?name="+nameA+"&status=PENDING", author, nil)
	if code != http.StatusOK || body["page"].(map[string]any)["total"] != float64(2) {
		t.Fatalf("name+status filter = %d %v", code, body)
	}

	// status 面:白名单内 0 命中 = 空数组;词表外 400。
	code, body = doJSON(t, r, http.MethodGet, "/api/v1/executions?status=RUNNING", author, nil)
	if code != http.StatusOK {
		t.Fatalf("status filter = %d %v", code, body)
	}
	if empty, ok := body["items"].([]any); !ok || len(empty) != 0 {
		t.Fatalf("empty items = %v(%T), want []", body["items"], body["items"])
	}
	code, body = doJSON(t, r, http.MethodGet, "/api/v1/executions?status=WEIRD", author, nil)
	if code != http.StatusBadRequest || body["error"] != "bad_status" {
		t.Fatalf("bad status = %d %v", code, body)
	}
	code, body = doJSON(t, r, http.MethodGet, "/api/v1/executions?status=PENDING&limit=2", author, nil)
	if code != http.StatusOK || len(body["items"].([]any)) != 2 {
		t.Fatalf("paging = %d %v", code, body)
	}

	// 分页参数域(与 definitions 同语义)。
	for _, q := range []string{"limit=0", "limit=101", "offset=-1", "offset=10001"} {
		code, _ = doJSON(t, r, http.MethodGet, "/api/v1/executions?"+q, author, nil)
		if code != http.StatusBadRequest {
			t.Fatalf("paging %s = %d, want 400", q, code)
		}
	}
}

// TestHTTPTriggerAuthAndRoles:触发面认证/角色闸与请求体上限(全部在
// store 之前失败,无栈可跑)。写面 = author|admin(与 definition 写面
// 一致),approver 403;体上限与 readDSLBody 同款 413 分界。
func TestHTTPTriggerAuthAndRoles(t *testing.T) {
	r, token := newNoDBServer(t)
	path := "/api/v1/executions"

	code, _ := doJSON(t, r, http.MethodPost, path, "", []byte(`{"definition_name":"x"}`))
	if code != 401 {
		t.Fatalf("unauthenticated trigger = %d, want 401", code)
	}
	approver, err := auth.SignToken(testSecret, time.Hour, "alice", []auth.Role{auth.RoleApprover})
	if err != nil {
		t.Fatalf("sign approver: %v", err)
	}
	code, _ = doJSON(t, r, http.MethodPost, path, approver, []byte(`{"definition_name":"x"}`))
	if code != 403 {
		t.Fatalf("approver trigger = %d, want 403", code)
	}
	// 请求体超限:仅真超限 413(input 是内嵌数据面,沿用 DSL 1MiB 上界)。
	huge := append([]byte(`{"definition_name":"x","input":"`), make([]byte, maxDSLBody)...)
	code, body := doJSON(t, r, http.MethodPost, path, token, huge)
	if code != http.StatusRequestEntityTooLarge || body["error"] != "body_too_large" {
		t.Fatalf("oversized trigger body = %d %v, want 413 body_too_large", code, body)
	}
}
