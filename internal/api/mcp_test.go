package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"braid/internal/auth"
	"braid/internal/db/sqltest"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
	"braid/internal/tools"
)

// newMCPServer 构造直调测试用 Server(工具 handler 单元面:ctx 直注
// claims,不经 HTTP/SDK 编解码——协议级对拍归 T4 e2e)。trig 是闭包:
// 真 scheduler 需先拿 dbh 才能装配(NewServer 对 nil trigger fail-loud)。
func newMCPServer(t *testing.T, trig func(dbh *sql.DB) ExecutionTrigger) (*Server, *sql.DB) {
	t.Helper()
	dbh := sqltest.MustOpenAt(t, "braid_api_test")
	cleanExecutionChain(t, dbh)
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	cfg := Config{Addr: DefaultAddr, JWTSecret: testSecret, SnowflakeNode: 1,
		LoginRateLimit: 100000, LoginRateWindow: time.Minute,
		LoginMaxFailures: 100000, LoginLockout: time.Minute}
	reg, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	return NewServer(cfg, dbh, ids, reg, trig(dbh), auth.BuiltinProvider{DB: dbh}), dbh
}

// fakeTrig 是 fakeTrigger 的闭包包装(与真 scheduler 构造同形)。
func fakeTrig(id uint64, err error) func(*sql.DB) ExecutionTrigger {
	return func(*sql.DB) ExecutionTrigger { return fakeTrigger{id: id, err: err} }
}

// realTrig 装配真 scheduler(realChainServer 同款 Deps)。
func realTrig(t *testing.T) func(*sql.DB) ExecutionTrigger {
	t.Helper()
	return func(dbh *sql.DB) ExecutionTrigger {
		schedIDs, err := snowflake.New(1)
		if err != nil {
			t.Fatalf("snowflake: %v", err)
		}
		sched, err := scheduler.New(scheduler.Deps{DB: dbh, IDs: schedIDs})
		if err != nil {
			t.Fatalf("scheduler: %v", err)
		}
		return sched
	}
}

// mcpClaimsCtx 是工具 handler 的主体注入面(镜像 mcpEntry 的注入形态)。
func mcpClaimsCtx(claims auth.Claims) context.Context {
	return context.WithValue(context.Background(), mcpClaimsCtxKey{}, claims)
}

// testClaims 构造直调 claims(工具面读 Subject/Tenant/Scopes)。
func testClaims(subject string, tenant uint64, scopes ...auth.Scope) auth.Claims {
	return auth.Claims{
		Roles:            []auth.Role{auth.RoleAuthor},
		Tenant:           tenant,
		Scopes:           scopes,
		RegisteredClaims: jwt.RegisteredClaims{Subject: subject},
	}
}

// TestMCPPagageResolve:mcpPageIn 分页值域(REST parsePageParams 同判:
// 缺省 defaultPageLimit、上界 maxPageLimit/maxPageOffset,值域外响亮)。
func TestMCPPagageResolve(t *testing.T) {
	cases := []struct {
		name      string
		in        mcpPageIn
		wantLimit int
		wantOff   int
		wantErr   bool
	}{
		{"zero defaults", mcpPageIn{}, defaultPageLimit, 0, false},
		{"explicit", mcpPageIn{Limit: 100, Offset: 10000}, 100, 10000, false},
		{"limit over", mcpPageIn{Limit: maxPageLimit + 1}, 0, 0, true},
		{"limit zero-ish negative", mcpPageIn{Limit: -1}, 0, 0, true},
		{"offset over", mcpPageIn{Offset: maxPageOffset + 1}, 0, 0, true},
		{"offset negative", mcpPageIn{Offset: -1}, 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limit, offset, err := tc.in.resolve()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolve(%v) = nil error, want error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve(%v) = %v", tc.in, err)
			}
			if limit != tc.wantLimit || offset != tc.wantOff {
				t.Fatalf("resolve(%v) = %d/%d, want %d/%d", tc.in, limit, offset, tc.wantLimit, tc.wantOff)
			}
		})
	}
}

// TestMCPToolScopeGate:作用域闸是工具面第一判定——无所需 scope 一律拒
// (错误文本携所需 scope 名,D-t131 决策 5 语义面);未注入 claims 的 ctx
// (接线 bug)fail-closed。持所需 scope 时错误(若有)不再含 scope 面。
func TestMCPToolScopeGate(t *testing.T) {
	s, _ := newMCPServer(t, fakeTrig(1, nil))
	ctx := mcpClaimsCtx(testClaims("alice", 0)) // 零作用域

	cases := []struct {
		name  string
		call  func() error
		scope string
	}{
		{"list workflows", func() error {
			_, _, err := s.mcpListWorkflows(ctx, nil, listWorkflowsIn{})
			return err
		}, "workflows:read"},
		{"get workflow", func() error {
			_, _, err := s.mcpGetWorkflow(ctx, nil, getWorkflowIn{Name: "x"})
			return err
		}, "workflows:read"},
		{"trigger workflow", func() error {
			_, _, err := s.mcpTriggerWorkflow(ctx, nil, triggerWorkflowIn{DefinitionName: "x"})
			return err
		}, "executions:write"},
		{"get execution", func() error {
			_, _, err := s.mcpGetExecution(ctx, nil, getExecutionIn{ID: "1"})
			return err
		}, "executions:read"},
		{"list executions", func() error {
			_, _, err := s.mcpListExecutions(ctx, nil, listExecutionsIn{})
			return err
		}, "executions:read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil || !strings.Contains(err.Error(), tc.scope) {
				t.Fatalf("err = %v, want scope denial mentioning %q", err, tc.scope)
			}
		})
	}

	// 接线 bug 面:无 claims 注入的 ctx 一律 fail-closed。
	bare := context.Background()
	if _, _, err := s.mcpListWorkflows(bare, nil, listWorkflowsIn{}); err == nil ||
		!strings.Contains(err.Error(), "unauthenticated") {
		t.Fatalf("bare ctx err = %v, want unauthenticated", err)
	}

	// 持作用域:闸放行(空 DB 上 list 成功;get 的 not_found 不是 scope 面)。
	held := mcpClaimsCtx(testClaims("alice", 0, auth.AllScopes()...))
	if _, _, err := s.mcpListWorkflows(held, nil, listWorkflowsIn{}); err != nil {
		t.Fatalf("held scope list: %v", err)
	}
	if _, _, err := s.mcpGetWorkflow(held, nil, getWorkflowIn{Name: "nope"}); err == nil ||
		strings.Contains(err.Error(), "scope") {
		t.Fatalf("held scope get: %v, want not_found (not scope denial)", err)
	}
}

// TestMCPListWorkflowsTenantIsolation:列表 = 发现面——租户谓词经
// claims.Tenant 直达 Store(t11.2 编译期不变式自动覆盖);跨租户 = 空页
// 而非对方行。
func TestMCPListWorkflowsTenantIsolation(t *testing.T) {
	s, _ := newMCPServer(t, fakeTrig(1, nil))
	r := s.Router()
	author, err := auth.SignToken(testSecret, time.Hour, "alice", []auth.Role{auth.RoleAuthor}, 0)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	name := uniqName("mcp-wf")
	publishViaAPI(t, r, author, name)

	_, out, err := s.mcpListWorkflows(mcpClaimsCtx(testClaims("alice", 0, auth.ScopeWorkflowsRead)), nil, listWorkflowsIn{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if out.Total != 1 || len(out.Workflows) != 1 || out.Workflows[0].Name != name {
		t.Fatalf("out = %+v, want single %q (tenant 0)", out, name)
	}
	if out.Workflows[0].Status != statusPublished || out.Workflows[0].Version == nil {
		t.Fatalf("workflow = %+v, want PUBLISHED with version", out.Workflows[0])
	}

	// 跨租户:空页(同面 = 不泄露存在性)。
	_, out7, err := s.mcpListWorkflows(mcpClaimsCtx(testClaims("bob", 7, auth.ScopeWorkflowsRead)), nil, listWorkflowsIn{})
	if err != nil {
		t.Fatalf("list tenant 7: %v", err)
	}
	if out7.Total != 0 || len(out7.Workflows) != 0 {
		t.Fatalf("tenant 7 out = %+v, want empty", out7)
	}

	// 参数面。
	for name, in := range map[string]listWorkflowsIn{
		"bad status": {Status: "BOGUS"},
		"bad limit":  {Page: mcpPageIn{Limit: 999}},
	} {
		if _, _, err := s.mcpListWorkflows(mcpClaimsCtx(testClaims("alice", 0, auth.ScopeWorkflowsRead)), nil, in); err == nil {
			t.Fatalf("%s: nil error, want rejection", name)
		}
	}
}

// TestMCPGetWorkflow:发现面 detail——DSL 正文回显(agent 决策输入面)、
// 钉版、跨租户 404 面、version=0 形态拒。
func TestMCPGetWorkflow(t *testing.T) {
	s, _ := newMCPServer(t, fakeTrig(1, nil))
	r := s.Router()
	author, err := auth.SignToken(testSecret, time.Hour, "alice", []auth.Role{auth.RoleAuthor}, 0)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	name := uniqName("mcp-get")
	version := publishViaAPI(t, r, author, name)
	claims := mcpClaimsCtx(testClaims("alice", 0, auth.ScopeWorkflowsRead))

	_, out, err := s.mcpGetWorkflow(claims, nil, getWorkflowIn{Name: name})
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	if out.Name != name || out.Status != statusPublished || out.Version == nil || *out.Version != version {
		t.Fatalf("out = %+v, want %q v%d PUBLISHED", out, name, version)
	}
	if out.DSL == "" || out.DSLHash == "" || !strings.Contains(out.DSL, name) {
		t.Fatalf("dsl/hash = %q/%q, want DSL body with name", out.DSL, out.DSLHash)
	}

	// 钉版。
	_, pinned, err := s.mcpGetWorkflow(claims, nil, getWorkflowIn{Name: name, Version: &version})
	if err != nil || pinned.Version == nil || *pinned.Version != version {
		t.Fatalf("pinned = %+v err=%v, want v%d", pinned, err, version)
	}

	// 形态/存在性/租户面。
	if _, _, err := s.mcpGetWorkflow(claims, nil, getWorkflowIn{Name: name, Version: ptrUint32(0)}); err == nil ||
		!strings.Contains(err.Error(), "bad_version") {
		t.Fatalf("version 0 err = %v, want bad_version", err)
	}
	if _, _, err := s.mcpGetWorkflow(claims, nil, getWorkflowIn{Name: "no-such"}); err == nil ||
		!strings.Contains(err.Error(), "not_found") {
		t.Fatalf("unknown err = %v, want not_found", err)
	}
	ctx7 := mcpClaimsCtx(testClaims("bob", 7, auth.ScopeWorkflowsRead))
	if _, _, err := s.mcpGetWorkflow(ctx7, nil, getWorkflowIn{Name: name}); err == nil ||
		!strings.Contains(err.Error(), "not_found") {
		t.Fatalf("cross-tenant err = %v, want not_found", err)
	}
}

func ptrUint32(v uint32) *uint32 { return &v }

// TestMCPTriggerWorkflowRealChain:真链物化(真 scheduler)——ack 契约、
// 幂等键二维作用域(主体+定义)在 MCP 面与 REST 面同键空间:同主体同键
// 重放(replayed=true 同 id)、异主体同键不重放(新执行)、REST 触发后
// MCP 携同键重放。
func TestMCPTriggerWorkflowRealChain(t *testing.T) {
	s, dbh := newMCPServer(t, realTrig(t))
	r := s.Router()
	author, err := auth.SignToken(testSecret, time.Hour, "alice", []auth.Role{auth.RoleAuthor}, 0)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	name := uniqName("mcp-trig")
	publishViaAPI(t, r, author, name)

	full := testClaims("alice", 0, auth.AllScopes()...)
	in := triggerWorkflowIn{DefinitionName: name,
		Input: map[string]json.RawMessage{
			"q":   []byte(`"mcp"`),
			"big": []byte("9007199254740993"), // 2^53+1:输入保真锚(评审 MEDIUM)
		}, IdempotencyKey: "k1"}

	_, out1, err := s.mcpTriggerWorkflow(mcpClaimsCtx(full), nil, in)
	if err != nil {
		t.Fatalf("trigger #1: %v", err)
	}
	// 行序:API 词面比较置行尾(gitleaks generic-api-key 对「api 关键词 +
	// || 算子 + 长标识符」形态误报,PR #109 首跑实证;常量单源同 DDL 词表)。
	if out1.Replayed || out1.Status != "PENDING" || out1.TriggeredBy != "alice" || out1.DefinitionName != name ||
		out1.TriggerType != triggerTypeAPI {
		t.Fatalf("ack = %+v, want PENDING/API/alice fresh", out1)
	}
	if out1.ID == "" {
		t.Fatalf("ack id empty")
	}
	// 输入保真(评审 MEDIUM):>2^53 整数经 MCP 面原样落库,不经 float64
	// 折叠(REST 原始 JSON 路径同判;any 解码路径在此锚必红)。
	execID1, _ := strconv.ParseUint(out1.ID, 10, 64)
	exec1, err := s.st.GetExecution(context.Background(), dbh, 0, execID1)
	if err != nil {
		t.Fatalf("read back execution: %v", err)
	}
	if !strings.Contains(string(exec1.Input), "9007199254740993") {
		t.Fatalf("input = %s, want >2^53 integer preserved", exec1.Input)
	}
	// 读面同判(评审 MEDIUM 二轮):MCP get_execution 的 Out 侧 RawMessage
	// 透传——服务端字节原样,不经 any 解码 float64 折叠(any 路径在此锚
	// 必红;client SDK 侧的解码折叠是消费端 artifact,非本面语义)。
	_, rd, err := s.mcpGetExecution(mcpClaimsCtx(testClaims("alice", 0, auth.ScopeExecutionsRead)),
		nil, getExecutionIn{ID: out1.ID})
	if err != nil {
		t.Fatalf("mcp read back: %v", err)
	}
	if !strings.Contains(string(rd.Input), "9007199254740993") {
		t.Fatalf("mcp read back input = %s, want >2^53 preserved", rd.Input)
	}

	// 同主体同键:重放。
	_, out2, err := s.mcpTriggerWorkflow(mcpClaimsCtx(full), nil, in)
	if err != nil {
		t.Fatalf("trigger #2: %v", err)
	}
	if !out2.Replayed || out2.ID != out1.ID {
		t.Fatalf("replay = %+v, want replayed=true id=%s", out2, out1.ID)
	}

	// 异主体同键:不重放(作用域 = 主体+定义二维)。
	bob := testClaims("bob", 0, auth.AllScopes()...)
	_, out3, err := s.mcpTriggerWorkflow(mcpClaimsCtx(bob), nil, in)
	if err != nil {
		t.Fatalf("trigger bob: %v", err)
	}
	if out3.Replayed || out3.ID == out1.ID {
		t.Fatalf("bob = %+v, want fresh execution (subject dimension of idem scope)", out3)
	}

	// REST 与 MCP 同键空间:REST 头键 = MCP 参数键(同主体同定义同哈希)。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions",
		strings.NewReader(fmt.Sprintf(`{"definition_name":%q}`, name)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+author)
	req.Header.Set(IdempotencyKeyHeader, "rest-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("REST trigger: code %d body %s", w.Code, w.Body.String())
	}
	var restBody map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &restBody); err != nil {
		t.Fatalf("REST body: %v", err)
	}
	restID := restBody["id"].(string)
	_, out4, err := s.mcpTriggerWorkflow(mcpClaimsCtx(full), nil,
		triggerWorkflowIn{DefinitionName: name, IdempotencyKey: "rest-key"})
	if err != nil {
		t.Fatalf("mcp after rest: %v", err)
	}
	if !out4.Replayed || out4.ID != restID {
		t.Fatalf("cross-face replay = %+v, want replayed=true id=%s", out4, restID)
	}

	// 零输入语义 + 形态面。
	_, out5, err := s.mcpTriggerWorkflow(mcpClaimsCtx(full), nil, triggerWorkflowIn{DefinitionName: name})
	if err != nil || out5.Replayed {
		t.Fatalf("zero input = %+v err=%v, want fresh ok", out5, err)
	}
	if _, _, err := s.mcpTriggerWorkflow(mcpClaimsCtx(full), nil, triggerWorkflowIn{DefinitionName: "NOPE"}); err == nil ||
		!strings.Contains(err.Error(), "bad_definition_name") {
		t.Fatalf("bad name err = %v, want bad_definition_name", err)
	}
}

// TestMCPGetExecutionRealChain:查询面——detail(节点/审计指针)、跨租户
// 404 面、bad_id。
func TestMCPGetExecutionRealChain(t *testing.T) {
	s, _ := newMCPServer(t, realTrig(t))
	r := s.Router()
	author, err := auth.SignToken(testSecret, time.Hour, "alice", []auth.Role{auth.RoleAuthor}, 0)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	name := uniqName("mcp-exec")
	publishViaAPI(t, r, author, name)
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/executions", author,
		[]byte(fmt.Sprintf(`{"definition_name":%q,"input":{"q":"x"}}`, name)))
	if code != http.StatusCreated {
		t.Fatalf("REST trigger: code %d body %v", code, body)
	}
	execID := body["id"].(string)

	full := mcpClaimsCtx(testClaims("alice", 0, auth.ScopeExecutionsRead))
	_, out, err := s.mcpGetExecution(full, nil, getExecutionIn{ID: execID})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if out.ID != execID || out.Status != "PENDING" || out.TriggeredBy != "alice" {
		t.Fatalf("detail = %+v", out)
	}
	if len(out.Nodes) != 1 || out.Nodes[0].Status != "PENDING" {
		t.Fatalf("nodes = %+v, want single PENDING", out.Nodes)
	}

	if _, _, err := s.mcpGetExecution(full, nil, getExecutionIn{ID: "abc"}); err == nil ||
		!strings.Contains(err.Error(), "bad_id") {
		t.Fatalf("bad id err = %v", err)
	}
	if _, _, err := s.mcpGetExecution(full, nil, getExecutionIn{ID: "999999"}); err == nil ||
		!strings.Contains(err.Error(), "not_found") {
		t.Fatalf("unknown id err = %v, want not_found", err)
	}
	ctx7 := mcpClaimsCtx(testClaims("bob", 7, auth.ScopeExecutionsRead))
	if _, _, err := s.mcpGetExecution(ctx7, nil, getExecutionIn{ID: execID}); err == nil ||
		!strings.Contains(err.Error(), "not_found") {
		t.Fatalf("cross-tenant err = %v, want not_found(不泄露存在性)", err)
	}
}

// TestMCPListExecutionsRealChain:列表过滤(name)、status 词表、分页值域。
func TestMCPListExecutionsRealChain(t *testing.T) {
	s, _ := newMCPServer(t, realTrig(t))
	r := s.Router()
	author, err := auth.SignToken(testSecret, time.Hour, "alice", []auth.Role{auth.RoleAuthor}, 0)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	name := uniqName("mcp-list")
	publishViaAPI(t, r, author, name)
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/executions", author,
		[]byte(fmt.Sprintf(`{"definition_name":%q}`, name)))
	if code != http.StatusCreated {
		t.Fatalf("REST trigger: code %d body %v", code, body)
	}
	execID := body["id"].(string)

	ctx := mcpClaimsCtx(testClaims("alice", 0, auth.ScopeExecutionsRead))
	_, out, err := s.mcpListExecutions(ctx, nil, listExecutionsIn{Name: name})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if out.Total < 1 || len(out.Executions) < 1 || out.Executions[0].ID != execID {
		t.Fatalf("out = %+v, want %q first", out, execID)
	}

	for label, in := range map[string]listExecutionsIn{
		"bad status": {Status: "WOKE"},
		"bad limit":  {Page: mcpPageIn{Limit: 101}},
	} {
		if _, _, err := s.mcpListExecutions(ctx, nil, in); err == nil {
			t.Fatalf("%s: nil error, want rejection", label)
		}
	}
}

// TestMCPUnauthInternalErrorFace:internal 错误不带底层细节(terr.err 只
// 进日志)——非 internal 码 = 干净码文本。
func TestMCPUnauthInternalErrorFace(t *testing.T) {
	s, _ := newMCPServer(t, fakeTrig(1, errors.New("boom-internal-detail")))
	r := s.Router()
	author, err := auth.SignToken(testSecret, time.Hour, "alice", []auth.Role{auth.RoleAuthor}, 0)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	name := uniqName("mcp-err")
	publishViaAPI(t, r, author, name)
	ctx := mcpClaimsCtx(testClaims("alice", 0, auth.AllScopes()...))
	_, _, err = s.mcpTriggerWorkflow(ctx, nil, triggerWorkflowIn{DefinitionName: name})
	if err == nil || !strings.Contains(err.Error(), "internal") {
		t.Fatalf("err = %v, want internal", err)
	}
	if strings.Contains(err.Error(), "boom-internal-detail") {
		t.Fatalf("internal err leaks detail: %v", err)
	}
}
