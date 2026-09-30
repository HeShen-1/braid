package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"braid/internal/auth"
)

// bearerTransport 给 SDK client 的出站请求注入 Authorization 头(外部
// MCP agent 的最小形态:协议客户端 + Bearer token,无自定义传输语义)。
type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// e2eSession 用真 SDK client 连 httptest 真路由的 /mcp(协议级对拍:
// 编解码/分帧/版本钉死全部过 SDK 真实现,不经 handler 直调)。
func e2eSession(t *testing.T, url, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "braid-e2e", Version: "v1"}, nil)
	sess, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   url + "/mcp",
		HTTPClient: &http.Client{Transport: bearerTransport{token}, Timeout: 15 * time.Second},
	}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// e2eSetup 起 httptest 服务 + 发布一个定义;token 由各测试经 e2eToken
// 按需构造(全量/窄/存量/跨租户)。
func e2eSetup(t *testing.T) (baseURL, name string) {
	t.Helper()
	s, _ := newMCPServer(t, realTrig(t))
	r := s.Router()
	author, err := auth.SignToken(testSecret, time.Hour, "alice", []auth.Role{auth.RoleAuthor}, 0)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	name = uniqName("mcp-e2e")
	publishViaAPI(t, r, author, name)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts.URL, name
}

func e2eToken(t *testing.T, subject string, tenant uint64, scopes ...auth.Scope) string {
	t.Helper()
	tok, err := auth.SignTokenScoped(testSecret, time.Hour, subject, []auth.Role{auth.RoleAuthor}, tenant, "", scopes)
	if err != nil {
		t.Fatalf("sign scoped: %v", err)
	}
	return tok
}

// structured 从工具结果取 structured content(map 形态;typed Out 的
// StructuredContent 编码面)。
func structured(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res == nil {
		t.Fatalf("nil result")
	}
	if res.IsError {
		texts := make([]string, 0, len(res.Content))
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				texts = append(texts, tc.Text)
			}
		}
		t.Fatalf("unexpected IsError result: %v", texts)
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	return out
}

// errText 取 IsError 结果的文本面。
func errText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || !res.IsError {
		t.Fatalf("result = %+v, want IsError", res)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// TestMCPE2EDiscoverTriggerQuery:验收主链路对拍——真 MCP client 发现五枚
// 工具、触发真执行(真 scheduler 物化)、查询 detail/list;协议版本与
// 编解码全程走 SDK(钉死 2026-07-28 stateless)。
func TestMCPE2EDiscoverTriggerQuery(t *testing.T) {
	url, name := e2eSetup(t)
	sess := e2eSession(t, url, e2eToken(t, "alice", 0, auth.AllScopes()...))
	ctx := context.Background()

	// 发现:五枚工具,名字钉死。
	lt, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range lt.Tools {
		got[tool.Name] = true
	}
	for _, want := range []string{
		"braid_list_workflows", "braid_get_workflow", "braid_trigger_workflow",
		"braid_get_execution", "braid_list_executions",
	} {
		if !got[want] {
			t.Fatalf("tools/list = %v, missing %q", got, want)
		}
	}
	if len(lt.Tools) != 5 {
		t.Fatalf("tools = %d, want exactly 5", len(lt.Tools))
	}

	// 发现 detail:DSL 正文回显。
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "braid_get_workflow",
		Arguments: map[string]any{"name": name},
	})
	if err != nil {
		t.Fatalf("get workflow: %v", err)
	}
	wf := structured(t, res)
	if wf["name"] != name || wf["status"] != "PUBLISHED" || !strings.Contains(fmt.Sprint(wf["dsl"]), name) {
		t.Fatalf("workflow = %v", wf)
	}

	// 触发:真执行物化(ack id + PENDING + replayed=false)。
	res, err = sess.CallTool(ctx, &mcp.CallToolParams{
		Name: "braid_trigger_workflow",
		Arguments: map[string]any{
			"definition_name": name,
			"input":           map[string]any{"q": "e2e"},
			"idempotency_key": "e2e-1",
		},
	})
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	ack := structured(t, res)
	execID, _ := ack["id"].(string)
	if execID == "" || ack["status"] != "PENDING" || ack["trigger_type"] != "API" ||
		ack["triggered_by"] != "alice" || ack["replayed"] != false {
		t.Fatalf("ack = %v", ack)
	}

	// 重放:同键同主体。
	res, err = sess.CallTool(ctx, &mcp.CallToolParams{
		Name: "braid_trigger_workflow",
		Arguments: map[string]any{
			"definition_name": name,
			"idempotency_key": "e2e-1",
		},
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay := structured(t, res); replay["replayed"] != true || replay["id"] != execID {
		t.Fatalf("replay = %v", replay)
	}

	// 查询:detail(节点状态)+ list。
	res, err = sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "braid_get_execution",
		Arguments: map[string]any{"id": execID},
	})
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}
	detail := structured(t, res)
	if detail["id"] != execID || detail["status"] != "PENDING" {
		t.Fatalf("detail = %v", detail)
	}
	if nodes, ok := detail["nodes"].([]any); !ok || len(nodes) != 1 {
		t.Fatalf("nodes = %v, want single", detail["nodes"])
	}

	res, err = sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "braid_list_executions",
		Arguments: map[string]any{"name": name},
	})
	if err != nil {
		t.Fatalf("list executions: %v", err)
	}
	le := structured(t, res)
	if total, _ := le["total"].(float64); total < 1 {
		t.Fatalf("list = %v, want total >= 1", le)
	}
}

// TestMCPE2EScopeAndTenant:作用域语义面在协议层的落点——窄 token 触发
// = IsError + scope 名;存量(无 scp)token = MCP 面零权限;跨租户 =
// 不泄露(列表空页 / detail not_found)。
func TestMCPE2EScopeAndTenant(t *testing.T) {
	url, name := e2eSetup(t)
	ctx := context.Background()

	// 窄 token(只读):触发拒绝在工具结果面(HTTP 200,IsError)。
	narrow := e2eSession(t, url, e2eToken(t, "alice", 0, auth.ScopeWorkflowsRead, auth.ScopeExecutionsRead))
	res, err := narrow.CallTool(ctx, &mcp.CallToolParams{
		Name:      "braid_trigger_workflow",
		Arguments: map[string]any{"definition_name": name},
	})
	if err != nil {
		t.Fatalf("narrow trigger transport err = %v (语义拒绝应在结果面,不在传输面)", err)
	}
	if text := errText(t, res); !strings.Contains(text, "executions:write") {
		t.Fatalf("narrow trigger err text = %q, want executions:write", text)
	}

	// 存量 token(无 scp):MCP 面 fail-closed 零权限。
	legacyTok, err := auth.SignToken(testSecret, time.Hour, "alice", []auth.Role{auth.RoleAuthor}, 0)
	if err != nil {
		t.Fatalf("sign legacy: %v", err)
	}
	legacy := e2eSession(t, url, legacyTok)
	res, err = legacy.CallTool(ctx, &mcp.CallToolParams{
		Name:      "braid_list_workflows",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("legacy list transport err = %v", err)
	}
	if text := errText(t, res); !strings.Contains(text, "workflows:read") {
		t.Fatalf("legacy err text = %q, want workflows:read denial", text)
	}

	// 跨租户:列表空页(非对方行);detail not_found。
	other := e2eSession(t, url, e2eToken(t, "bob", 7, auth.AllScopes()...))
	res, err = other.CallTool(ctx, &mcp.CallToolParams{
		Name:      "braid_list_workflows",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("tenant list: %v", err)
	}
	lw := structured(t, res)
	if wfs, ok := lw["workflows"].([]any); !ok || len(wfs) != 0 {
		t.Fatalf("tenant 7 workflows = %v, want empty (不泄露存在性)", lw["workflows"])
	}
	res, err = other.CallTool(ctx, &mcp.CallToolParams{
		Name:      "braid_get_workflow",
		Arguments: map[string]any{"name": name},
	})
	if err != nil {
		t.Fatalf("tenant get: %v", err)
	}
	if text := errText(t, res); !strings.Contains(text, "not_found") {
		t.Fatalf("tenant get text = %q, want not_found", text)
	}
}

// TestMCPE2EHTTPFace:协议层外的 HTTP 面——未认证 401、非 POST 405
// (stateless 语义)、错协议版本拒(版本钉死执法)。
func TestMCPE2EHTTPFace(t *testing.T) {
	url, name := e2eSetup(t)
	token := e2eToken(t, "alice", 0, auth.AllScopes()...)
	hc := &http.Client{Transport: bearerTransport{token}}

	// 未认证 POST:401(HTTP 面,认证语义先于协议语义)。裸 client
	//(零 Authorization 头)——不经 bearerTransport。
	resp, err := http.Post(url+"/mcp", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	if err != nil {
		t.Fatalf("unauth post: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // 只读探测路径,关闭错误无处置义
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth = %d, want 401", resp.StatusCode)
	}

	// GET / DELETE:405(stateless 2026-07-28:无独立 SSE 流/无会话删除)。
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req, _ := http.NewRequest(method, url+"/mcp", nil)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		defer resp.Body.Close() //nolint:errcheck // 只读探测路径,关闭错误无处置义
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s = %d, want 405", method, resp.StatusCode)
		}
	}

	// 错协议版本:Mcp-Protocol-Version 钉 2026-07-28,2025-06-18 拒。
	req, _ := http.NewRequest(http.MethodPost, url+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	resp, err = hc.Do(req)
	if err != nil {
		t.Fatalf("wrong version post: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // 只读探测路径,关闭错误无处置义
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("wrong version = 200, want rejection(版本钉死执法)")
	}
	_ = name
}
