package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"braid/internal/auth"
	"braid/internal/db/sqltest"
	"braid/internal/snowflake"
)

// newTestServer 返回挂真实 store(t13,缺栈自动 skip)的路由与 token
// 签发器。每用例清空定义表(共享 schema)。
func newTestServer(t *testing.T, allowlist ...string) (*gin.Engine, *sql.DB, func(roles ...auth.Role) string) {
	t.Helper()
	dbh := sqltest.MustOpenAt(t, "braid_api_test")
	if _, err := dbh.Exec("DELETE FROM workflow_definition"); err != nil {
		t.Fatalf("clean workflow_definition: %v", err)
	}
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	cfg := Config{Addr: DefaultAddr, JWTSecret: testSecret, ToolAllowlist: allowlist, SnowflakeNode: 1}
	gin.SetMode(gin.TestMode)
	r := NewServer(cfg, dbh, ids).Router()
	sign := func(roles ...auth.Role) string {
		t.Helper()
		s, err := auth.SignToken(testSecret, time.Hour, "alice", roles)
		if err != nil {
			t.Fatalf("sign token: %v", err)
		}
		return s
	}
	return r, dbh, sign
}

// doJSON 发起带认证的 JSON 请求,返回状态码与解码后的 body。
func doJSON(t *testing.T, r *gin.Engine, method, path, token string, body []byte) (int, map[string]any) {
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

func createViaAPI(t *testing.T, r *gin.Engine, token string, doc []byte) map[string]any {
	t.Helper()
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/definitions", token, doc)
	if code != http.StatusCreated {
		t.Fatalf("create via API: code %d, body %v", code, body)
	}
	return body
}

func TestHTTPAuthAndRoles(t *testing.T) {
	r, _, sign := newTestServer(t)
	author := sign(auth.RoleAuthor)
	doc := dslDoc(uniqName("authz"))

	code, _ := doJSON(t, r, http.MethodPost, "/api/v1/definitions", "", doc)
	if code != 401 {
		t.Fatalf("unauthenticated create = %d, want 401", code)
	}
	code, _ = doJSON(t, r, http.MethodPost, "/api/v1/definitions", sign(auth.RoleApprover), doc)
	if code != 403 {
		t.Fatalf("approver create = %d, want 403", code)
	}
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/definitions", author, doc)
	if code != 201 {
		t.Fatalf("author create = %d, want 201", code)
	}
	if body["status"] != "DRAFT" || body["version"] != nil {
		t.Fatalf("created = %v, want DRAFT with null version", body)
	}
	// 读面:任意有效角色(approver)可读。
	code, _ = doJSON(t, r, http.MethodGet, "/api/v1/definitions?status=DRAFT", sign(auth.RoleApprover), nil)
	if code != 200 {
		t.Fatalf("approver list = %d, want 200", code)
	}
	// 健康检查无认证。
	code, _ = doJSON(t, r, http.MethodGet, "/healthz", "", nil)
	if code != 200 {
		t.Fatalf("healthz = %d, want 200", code)
	}
}

func TestHTTPCreateInvalidDSL(t *testing.T) {
	r, _, sign := newTestServer(t)
	author := sign(auth.RoleAuthor)
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/definitions", author,
		[]byte(`{"dsl":"1.0","name":"Bad_Name","nodes":[],"outputs":{}}`))
	if code != 400 {
		t.Fatalf("invalid DSL create = %d, want 400", code)
	}
	if body["error"] != "invalid_dsl" {
		t.Fatalf("error code = %v, want invalid_dsl", body["error"])
	}
	if _, ok := body["issues"].([]any); !ok {
		t.Fatalf("issues missing: %v", body)
	}
}

func TestHTTPDraftLifecycle(t *testing.T) {
	r, _, sign := newTestServer(t)
	author := sign(auth.RoleAuthor)
	name := uniqName("life")
	created := createViaAPI(t, r, author, dslDoc(name))
	id := created["id"].(string) // 雪碧 ID 线形态 = 字符串(2^53 精度契约)

	updated := dslDoc(name)
	code, body := doJSON(t, r, http.MethodPut, "/api/v1/drafts/"+id, author, updated)
	if code != 200 || body["status"] != "DRAFT" {
		t.Fatalf("update draft = %d %v", code, body)
	}
	code, body = doJSON(t, r, http.MethodPut, "/api/v1/drafts/"+id, author, dslDoc(uniqName("renamed")))
	if code != 400 || body["error"] != "name_mismatch" {
		t.Fatalf("rename = %d %v, want 400 name_mismatch", code, body)
	}
	code, _ = doJSON(t, r, http.MethodPut, "/api/v1/drafts/abc", author, updated)
	if code != 400 {
		t.Fatalf("bad id = %d, want 400", code)
	}
	code, _ = doJSON(t, r, http.MethodPut, "/api/v1/drafts/999999", author, updated)
	if code != 404 {
		t.Fatalf("unknown id = %d, want 404", code)
	}

	code, pub := doJSON(t, r, http.MethodPost, "/api/v1/drafts/"+id+"/publish", author, nil)
	if code != 200 || pub["version"] != float64(1) || pub["status"] != "PUBLISHED" {
		t.Fatalf("publish = %d %v, want 200 v1 PUBLISHED", code, pub)
	}
	code, _ = doJSON(t, r, http.MethodPost, "/api/v1/drafts/"+id+"/publish", author, nil)
	if code != 409 {
		t.Fatalf("republish = %d, want 409", code)
	}
	code, _ = doJSON(t, r, http.MethodPut, "/api/v1/drafts/"+id, author, updated)
	if code != 409 {
		t.Fatalf("update published = %d, want 409", code)
	}

	// 详情与版本面。
	code, latest := doJSON(t, r, http.MethodGet, "/api/v1/definitions/"+name, author, nil)
	if code != 200 || latest["version"] != float64(1) {
		t.Fatalf("latest = %d %v", code, latest)
	}
	if dslBody, ok := latest["dsl"].(map[string]any); !ok || dslBody["name"] != name {
		t.Fatalf("detail missing dsl (inline JSON): %v", latest)
	}
	code, _ = doJSON(t, r, http.MethodGet, "/api/v1/definitions/ghost-"+name, author, nil)
	if code != 404 {
		t.Fatalf("unknown name = %d, want 404", code)
	}
	code, ver := doJSON(t, r, http.MethodGet, "/api/v1/definitions/"+name+"/versions/1", author, nil)
	if code != 200 || ver["status"] != "PUBLISHED" {
		t.Fatalf("get version = %d %v", code, ver)
	}
	code, _ = doJSON(t, r, http.MethodGet, "/api/v1/definitions/"+name+"/versions/9", author, nil)
	if code != 404 {
		t.Fatalf("unknown version = %d, want 404", code)
	}
	code, _ = doJSON(t, r, http.MethodGet, "/api/v1/definitions/"+name+"/versions/x", author, nil)
	if code != 400 {
		t.Fatalf("bad version = %d, want 400", code)
	}
	code, vs := doJSON(t, r, http.MethodGet, "/api/v1/definitions/"+name+"/versions", author, nil)
	if code != 200 || len(vs["items"].([]any)) != 1 {
		t.Fatalf("versions = %d %v", code, vs)
	}

	// 归档 + 触发解析回退。
	code, _ = doJSON(t, r, http.MethodPost, "/api/v1/definitions/"+name+"/versions/1/archive", author, nil)
	if code != 200 {
		t.Fatalf("archive = %d, want 200", code)
	}
	code, _ = doJSON(t, r, http.MethodGet, "/api/v1/definitions/"+name, author, nil)
	if code != 404 {
		t.Fatalf("all-archived latest = %d, want 404 (禁新触发)", code)
	}
}

// TestHTTPPublishAllowlist 验收第二条的 HTTP 面:引用白名单外工具的
// 发布返回 4xx(400 + tool_not_allowed issue)。
func TestHTTPPublishAllowlist(t *testing.T) {
	r, _, sign := newTestServer(t, "file_read")
	author := sign(auth.RoleAuthor)
	name := uniqName("wl")
	created := createViaAPI(t, r, author, toolDSL(name)) // 草稿创建不校验白名单
	id := created["id"].(string)

	code, body := doJSON(t, r, http.MethodPost, "/api/v1/drafts/"+id+"/publish", author, nil)
	if code != 400 {
		t.Fatalf("publish blocked tool = %d, want 400", code)
	}
	if body["error"] != "invalid_dsl" {
		t.Fatalf("error = %v, want invalid_dsl", body["error"])
	}
	issues := body["issues"].([]any)
	first := issues[0].(map[string]any)
	if first["code"] != "tool_not_allowed" {
		t.Fatalf("issue = %v, want tool_not_allowed", first)
	}
}

func TestHTTPListPagingAndFilters(t *testing.T) {
	r, _, sign := newTestServer(t)
	author := sign(auth.RoleAuthor)
	name := uniqName("pg")
	for i := 0; i < 3; i++ {
		createViaAPI(t, r, author, dslDoc(fmt.Sprintf("%s-%d", name, i)))
	}
	code, body := doJSON(t, r, http.MethodGet, "/api/v1/definitions?status=DRAFT&limit=2&offset=0", author, nil)
	if code != 200 {
		t.Fatalf("list = %d", code)
	}
	page := body["page"].(map[string]any)
	if page["total"] != float64(3) || len(body["items"].([]any)) != 2 {
		t.Fatalf("page = %v items %v", page, body["items"])
	}
	code, _ = doJSON(t, r, http.MethodGet, "/api/v1/definitions?status=WEIRD", author, nil)
	if code != 400 {
		t.Fatalf("bad status = %d, want 400", code)
	}
	code, _ = doJSON(t, r, http.MethodGet, "/api/v1/definitions?limit=0", author, nil)
	if code != 400 {
		t.Fatalf("limit 0 = %d, want 400", code)
	}
	code, _ = doJSON(t, r, http.MethodGet, "/api/v1/definitions?limit=101", author, nil)
	if code != 400 {
		t.Fatalf("limit 101 = %d, want 400", code)
	}
	code, _ = doJSON(t, r, http.MethodGet, "/api/v1/definitions?offset=-1", author, nil)
	if code != 400 {
		t.Fatalf("offset -1 = %d, want 400", code)
	}
	code, _ = doJSON(t, r, http.MethodGet, "/api/v1/definitions?offset=10001", author, nil)
	if code != 400 {
		t.Fatalf("offset 10001 = %d, want 400 (deep-page guard)", code)
	}
}

func TestHTTPBodyTooLarge(t *testing.T) {
	r, _, sign := newTestServer(t)
	author := sign(auth.RoleAuthor)
	huge := append(dslDoc(uniqName("huge")), make([]byte, maxDSLBody)...)
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/definitions", author, huge)
	if code != 413 {
		t.Fatalf("oversized body = %d %v, want 413", code, body)
	}
}

// errReader 注入传输层读体失败(非超限:断连/坏 chunked 的模型)。
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("transport gone") }

// TestHTTPBodyReadError 钉死 413/400 分界:仅真超限(*http.MaxBytesError)
// 是 413;传输层读体失败 400 bad_request,不得谎报超限误导重试与告警
// (审查发现 #3 的回归锚)。
func TestHTTPBodyReadError(t *testing.T) {
	r, token := newNoDBServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/definitions", errReader{})
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("read error = %d %s, want 400", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "bad_request") {
		t.Fatalf("body = %s, want bad_request", w.Body.String())
	}
}

// newNoDBServer 构造不触 DB 的路由(readDSLBody/错误映射面在 store 之前
// 失败,无需 t13 栈——本组测试在无栈环境也必须跑)。
func newNoDBServer(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	dead, err := sql.Open("mysql", "root:x@tcp(127.0.0.1:1)/braid_none")
	if err != nil {
		t.Fatalf("open dead pool: %v", err)
	}
	t.Cleanup(func() { _ = dead.Close() })
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	cfg := Config{Addr: DefaultAddr, JWTSecret: testSecret, SnowflakeNode: 1}
	gin.SetMode(gin.TestMode)
	token, err := auth.SignToken(testSecret, time.Hour, "alice", []auth.Role{auth.RoleAuthor})
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return NewServer(cfg, dead, ids).Router(), token
}

// TestHTTPInternalErrorMapping 钉死 500 契约:存储故障 → 500 {"error":
// "internal"},且响应体不携带任何底层错误细节(DSN/驱动文本只进日志;
// 审查发现 #8 的回归锚)。死连接池令 BeginTx 必败,无需 t13 栈。
func TestHTTPInternalErrorMapping(t *testing.T) {
	r, token := newNoDBServer(t)
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/definitions", token, dslDoc(uniqName("fiveh")))
	if code != 500 {
		t.Fatalf("storage failure = %d %v, want 500", code, body)
	}
	if body["error"] != "internal" {
		t.Fatalf("error = %v, want internal", body["error"])
	}
	if len(body) != 1 {
		t.Fatalf("response leaks internals: %v", body)
	}
}

// TestHTTPGetDraftRoute 补文档路由 GET /drafts/{id} 的覆盖(审查发现 #7
// 的回归锚),并钉死「按 id 读任意状态行」的语义(发布后行仍可按 id 读)。
func TestHTTPGetDraftRoute(t *testing.T) {
	r, _, sign := newTestServer(t)
	author := sign(auth.RoleAuthor)
	name := uniqName("getd")
	created := createViaAPI(t, r, author, dslDoc(name))
	id := created["id"].(string)

	code, body := doJSON(t, r, http.MethodGet, "/api/v1/drafts/"+id, author, nil)
	if code != 200 || body["status"] != "DRAFT" || body["version"] != nil {
		t.Fatalf("get draft = %d %v", code, body)
	}
	if _, ok := body["dsl"].(map[string]any); !ok {
		t.Fatalf("draft detail missing dsl: %v", body)
	}
	code, _ = doJSON(t, r, http.MethodGet, "/api/v1/drafts/abc", author, nil)
	if code != 400 {
		t.Fatalf("bad id = %d, want 400", code)
	}
	code, _ = doJSON(t, r, http.MethodGet, "/api/v1/drafts/999999", author, nil)
	if code != 404 {
		t.Fatalf("unknown id = %d, want 404", code)
	}
	pcode, _ := doJSON(t, r, http.MethodPost, "/api/v1/drafts/"+id+"/publish", author, nil)
	if pcode != 200 {
		t.Fatalf("publish = %d, want 200", pcode)
	}
	code, body = doJSON(t, r, http.MethodGet, "/api/v1/drafts/"+id, author, nil)
	if code != 200 || body["status"] != "PUBLISHED" {
		t.Fatalf("published row by id = %d %v, want 200 PUBLISHED", code, body)
	}
}
