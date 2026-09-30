package tools

// MCP 消费面「白名单/沙箱约束不降级」逐面对抗套件(T13.2 验收核心,
// issue #112)。全部经 LocalRunner 四道门(RBAC→token→registry→产出
// 契约)驱动——与内置工具同链 = 同权限模型的结构性证明;每面锚定一个
// 「若 MCP 载体绕过既有约束会绿」的对抗用例:
//
//	面 1 RBAC:approver 级拒绝 → tool_rbac_denied,工具零触达
//	面 2 registry:未声明名 → tool_not_found(动态发现不可注入白名单)
//	面 3 host 门:声明宿主重定向他宿 → tool_host_blocked(runner 分桶)
//	面 4 https 门:重定向降级 http → tool_ssrf_blocked
//	面 5 egress:代理 Allow 缺席 → CONNECT 403 → tool_host_blocked
//	面 6 产出契约:畸形结果(无 structured 无文本)→ tool_failed;
//	       成功路径产出恒为顶层对象(runner validateObjectOutput 收口)
//	面 7 幂等键:attempt 键经头透传到达外部 server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// denyAllAuthz 是携带 ErrCallDenied 哨兵的拒绝授权门(语义 = 确定性
// 越权拒绝,runner 应收口 RBAC 业务失败而非 infra)。
type denyAllAuthz struct{}

func (denyAllAuthz) AuthorizeCall(context.Context, uint64, string) error {
	return fmt.Errorf("%w: role mismatch (test)", ErrCallDenied)
}

// fakeMCPCaller 是假 session(dial 缝注入):回放固定结果 + 触达计数。
type fakeMCPCaller struct {
	res *mcp.CallToolResult
	err error
}

func (f *fakeMCPCaller) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return f.res, f.err
}
func (f *fakeMCPCaller) Close() error { return nil }

// mcpToolWithDial 构造带 dial 缝 + 触达计数的载体(runner 链驱动用)。
func mcpToolWithDial(name string, res *mcp.CallToolResult, err error) (*mcpClientTool, *int) {
	calls := 0
	tool := &mcpClientTool{name: name, remote: "remote_tool", client: NewGuardedClient(GuardConfig{})}
	tool.dial = func(context.Context, *mcpClientTool, Call) (mcpCaller, error) {
		calls++
		return &fakeMCPCaller{res: res, err: err}, nil
	}
	return tool, &calls
}

// 面 1:执行时 RBAC 门先于一切能力签发(角色不足 → 零触达 token/工具)。
func TestMCPAdversarialRBACGate(t *testing.T) {
	tool, calls := mcpToolWithDial("mcp_x", &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"a":1}`}}}, nil)
	r := NewLocalRunner(registryOf(t, tool), NewTokenIssuer(nil, 0, nil), denyAllAuthz{})
	_, err := r.Run(context.Background(), RunRequest{Tool: "mcp_x", NodeExecID: 1, AttemptNo: 1})
	if got := callCodeOf(t, err); got != CodeRBACDenied {
		t.Fatalf("code = %q, want %q", got, CodeRBACDenied)
	}
	if *calls != 0 {
		t.Fatalf("denied call reached the tool %d times (authorization must precede dispatch)", *calls)
	}
}

// 面 2:registry 白名单——未声明名(动态发现注入面不存在)即拒。
func TestMCPAdversarialRegistryWhitelist(t *testing.T) {
	tool, calls := mcpToolWithDial("mcp_declared", &mcp.CallToolResult{}, nil)
	r := NewLocalRunner(registryOf(t, tool), NewTokenIssuer(nil, 0, nil), AllowAllAuthorizer{})
	_, err := r.Run(context.Background(), RunRequest{Tool: "mcp_undeclared", NodeExecID: 1, AttemptNo: 1})
	if got := callCodeOf(t, err); got != CodeToolNotFound {
		t.Fatalf("code = %q, want %q", got, CodeToolNotFound)
	}
	if *calls != 0 {
		t.Fatalf("undeclared call reached a tool %d times", *calls)
	}
}

// 面 3+4+5:守卫哨兵经 runner 分桶(重定向他宿 host 门 / 重定向降级
// http 的 https 门 / egress 代理 403)。三者共用「真 transport + 真
// server/代理」对拍——假 session 无法覆盖守卫层。
func TestMCPAdversarialGuardBuckets(t *testing.T) {
	// 面 3:host 门——声明宿主 302 跳未声明宿主。
	t.Run("host gate", func(t *testing.T) {
		decoy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://evil.example.com/mcp", http.StatusFound)
		}))
		t.Cleanup(decoy.Close)
		pool := x509.NewCertPool()
		pool.AddCert(decoy.Certificate())
		tool := newTestMCPTool(t, decoy.URL, &tls.Config{RootCAs: pool}, "mcp_decoy", "echo_text", "")
		r := NewLocalRunner(registryOf(t, tool), NewTokenIssuer(nil, 0, nil), AllowAllAuthorizer{})
		_, err := r.Run(context.Background(), RunRequest{Tool: "mcp_decoy", NodeExecID: 1, AttemptNo: 1})
		if got := callCodeOf(t, err); got != CodeHostBlocked {
			t.Fatalf("code = %q, want %q", got, CodeHostBlocked)
		}
	})

	// 面 4:https 门——302 跳明文 http(守卫 httpsOnly 每跳拒)。
	t.Run("https gate", func(t *testing.T) {
		decoy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://127.0.0.1:1/mcp", http.StatusFound)
		}))
		t.Cleanup(decoy.Close)
		pool := x509.NewCertPool()
		pool.AddCert(decoy.Certificate())
		tool := newTestMCPTool(t, decoy.URL, &tls.Config{RootCAs: pool}, "mcp_downgrade", "echo_text", "")
		r := NewLocalRunner(registryOf(t, tool), NewTokenIssuer(nil, 0, nil), AllowAllAuthorizer{})
		_, err := r.Run(context.Background(), RunRequest{Tool: "mcp_downgrade", NodeExecID: 1, AttemptNo: 1})
		if got := callCodeOf(t, err); got != CodeSSRFBlocked {
			t.Fatalf("code = %q, want %q", got, CodeSSRFBlocked)
		}
	})

	// 面 5:egress 代理——Allow 缺席 CONNECT 403,哨兵跨代理/隧道/SDK
	// 多层错误链保真,runner 分桶 host_blocked。
	t.Run("egress deny", func(t *testing.T) {
		hs, _, tlsCfg := mcpTestServer(t, mcpTestTools)
		deny := startEgressProxy(t, EgressConfig{Allow: map[string]map[int]struct{}{}, IPForbidden: allowAllForbiddenOff})
		pu, err := parseEgressProxyURL("http://" + deny)
		if err != nil {
			t.Fatalf("parse proxy: %v", err)
		}
		tools_, err := NewMCPTools(MCPConfig{
			Servers: map[string]string{"srv": hs.URL},
			Tools:   map[string]MCPToolRef{"mcp_via_proxy": {Server: "srv", Tool: "echo_text"}},
			Guard:   GuardConfig{TLS: tlsCfg, ProxyURL: pu, IPForbidden: allowAllForbiddenOff},
		})
		if err != nil {
			t.Fatalf("NewMCPTools: %v", err)
		}
		r := NewLocalRunner(registryOf(t, tools_...), NewTokenIssuer(nil, 0, nil), AllowAllAuthorizer{})
		_, err = r.Run(context.Background(), RunRequest{Tool: "mcp_via_proxy", NodeExecID: 1, AttemptNo: 1})
		if got := callCodeOf(t, err); got != CodeHostBlocked {
			t.Fatalf("code = %q, want %q (egress proxy 403 sentinel must survive SDK error chain)", got, CodeHostBlocked)
		}
	})
}

// 面 6+7:全链成功路径——产出恒为顶层对象(runner 契约收口),幂等键
// 经头到达外部 server(attempt 键传递面铁律)。
func TestMCPAdversarialFullChain(t *testing.T) {
	hs, cap, tlsCfg := mcpTestServer(t, mcpTestTools)
	tool := newTestMCPTool(t, hs.URL, tlsCfg, "mcp_echo", "echo_text", "")
	r := NewLocalRunner(registryOf(t, tool), NewTokenIssuer(nil, 0, nil), AllowAllAuthorizer{})
	res, err := r.Run(context.Background(), RunRequest{
		Tool: "mcp_echo", Inputs: map[string]any{"q": 1},
		NodeExecID: 7, AttemptNo: 3, IdempotencyKey: "att:7:3",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("output must be top-level object (runner contract): %v", err)
	}
	auth, idem, _ := cap.snapshot()
	found := false
	for _, v := range idem {
		if v == "att:7:3" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Idempotency-Key %v must reach external server", idem)
	}
	for _, a := range auth {
		if a != "" && !strings.HasPrefix(a, "Bearer") {
			t.Fatalf("unexpected Authorization header: %q", a)
		}
	}
}

// 面 6b:畸形产出(无 structured 无文本)→ tool_failed 业务失败
// (server 面畸形是可预期失败,LLM 可读错误文本重试;非 infra)。
func TestMCPAdversarialMalformedResult(t *testing.T) {
	tool, _ := mcpToolWithDial("mcp_empty", &mcp.CallToolResult{}, nil)
	r := NewLocalRunner(registryOf(t, tool), NewTokenIssuer(nil, 0, nil), AllowAllAuthorizer{})
	_, err := r.Run(context.Background(), RunRequest{Tool: "mcp_empty", NodeExecID: 1, AttemptNo: 1})
	if got := callCodeOf(t, err); got != CodeToolFailed {
		t.Fatalf("code = %q, want %q", got, CodeToolFailed)
	}
}

// 面 6c:连接成功后 CallTool 阶段传输失败(server 中途断连/调用被拒)
// → tool_failed 业务失败(对抗审计 LOW:该生产最常见失败路径此前零
// 覆盖,fakeMCPCaller.err 为死参;若此分支错误自行包 CallError 或改
// 哨兵语义,本用例红)。
func TestMCPAdversarialCallToolTransportError(t *testing.T) {
	tool, _ := mcpToolWithDial("mcp_reset", nil, fmt.Errorf("connection reset by peer"))
	r := NewLocalRunner(registryOf(t, tool), NewTokenIssuer(nil, 0, nil), AllowAllAuthorizer{})
	_, err := r.Run(context.Background(), RunRequest{Tool: "mcp_reset", NodeExecID: 1, AttemptNo: 1})
	if got := callCodeOf(t, err); got != CodeToolFailed {
		t.Fatalf("code = %q, want %q", got, CodeToolFailed)
	}
	if !strings.Contains(err.Error(), "call remote tool") {
		t.Fatalf("err = %v, want call-stage wrapping", err)
	}
}
