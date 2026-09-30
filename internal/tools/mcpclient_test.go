package tools

// MCP 消费面测试(T13.2,D-t132):真 SDK server × httptest TLS × 真
// streamable client transport 对拍(零 docker、零 Redis)。覆盖:构造
// 校验表、产出契约三通道(structured/content/content_text)、IsError、
// 凭证/幂等键头注入(与 scoped token 不外发)、host 门重定向、egress
// 代理过/拒两态(真 EgressProxy in-process)。

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpHeaderCapture 捕获 MCP 面收到的请求头(凭证/幂等键注入与
// scoped-token 不外发的断言面)。
type mcpHeaderCapture struct {
	mu      sync.Mutex
	auth    []string
	idem    []string
	rawAuth []string // 原始 Authorization 头(含非 Bearer 形态)
}

func (c *mcpHeaderCapture) record(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.auth = append(c.auth, r.Header.Get("Authorization"))
	c.idem = append(c.idem, r.Header.Get("Idempotency-Key"))
	c.rawAuth = append(c.rawAuth, r.Header["Authorization"]...)
}

func (c *mcpHeaderCapture) snapshot() (auth, idem []string, rawAuth []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.auth...), append([]string{}, c.idem...), append([]string{}, c.rawAuth...)
}

// mcpTestServer 起 in-process 真 SDK server(stateless streamable HTTP,
// t13.1 同形态)挂 httptest TLS,返回可注入 GuardConfig.TLS 的根。
func mcpTestServer(t *testing.T, add func(*mcp.Server)) (*httptest.Server, *mcpHeaderCapture, *tls.Config) {
	return mcpTestServerOpts(t, []string{MCPProtocolVersion}, true, add)
}

// mcpTestServerOpts 是可配置变体缝(对抗审计补):versions = server 支持的
// 协议版本(钉死值之外另测 legacy 协商拒);jsonResp = JSONResponse 关闭
// 时 POST 响应走 SSE 帧(生产常态之一,DefaultMaxEventSize 路径)。
func mcpTestServerOpts(t *testing.T, versions []string, jsonResp bool, add func(*mcp.Server)) (*httptest.Server, *mcpHeaderCapture, *tls.Config) {
	t.Helper()
	ms := mcp.NewServer(
		&mcp.Implementation{Name: "test-mcp", Version: "v1"},
		&mcp.ServerOptions{SupportedProtocolVersions: versions},
	)
	add(ms)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return ms },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: jsonResp})
	cap := &mcpHeaderCapture{}
	hs := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(hs.Close)
	pool := x509.NewCertPool()
	pool.AddCert(hs.Certificate())
	return hs, cap, &tls.Config{RootCAs: pool}
}

// mcpTestIn/Out 是 echo_struct 的 typed In/ Out(驱动 SDK 填
// structuredContent + 文本 Content 双通道)。
type mcpTestIn struct {
	City string `json:"city"`
}

type mcpTestOut struct {
	City string `json:"city"`
	N    int64  `json:"n"`
}

// mcpTestTools 注册一族测试工具,驱动产出契约三通道与 IsError:
//
//	echo_struct   typed Out(structuredContent + 文本 Content 双在,SDK 自动)
//	echo_text     raw handler 只回文本 Content(JSON 数值——UseNumber 保真通道)
//	echo_plaintext raw handler 只回非 JSON 文本
//	boom          raw handler IsError=true + 错误文本
func mcpTestTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "echo_struct"}, func(_ context.Context, _ *mcp.CallToolRequest, in mcpTestIn) (*mcp.CallToolResult, mcpTestOut, error) {
		return nil, mcpTestOut{City: in.City, N: 42}, nil
	})
	srv.AddTool(&mcp.Tool{Name: "echo_text", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			text := `{"big":12345678901234567890,"echo":` + string(req.Params.Arguments) + "}"
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
		})
	srv.AddTool(&mcp.Tool{Name: "echo_plaintext", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "not json at all"}}}, nil
		})
	srv.AddTool(&mcp.Tool{Name: "boom", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "city not found"}}}, nil
		})
}

// newTestMCPTool 单工具装配捷径(默认 Guard 注入测试 TLS 根)。
func newTestMCPTool(t *testing.T, srvURL string, tlsCfg *tls.Config, name, remote, token string) Tool {
	t.Helper()
	tools_, err := NewMCPTools(MCPConfig{
		Servers: map[string]string{"srv": srvURL},
		Tools:   map[string]MCPToolRef{name: {Server: "srv", Tool: remote}},
		Tokens:  map[string]string{"srv": token},
		Guard:   GuardConfig{TLS: tlsCfg, IPForbidden: allowAllForbiddenOff}, // httptest 127.0.0.1 落生产禁段;测试缝放行(生产该 IP 本就拒)
	})
	if err != nil {
		t.Fatalf("NewMCPTools: %v", err)
	}
	if len(tools_) != 1 {
		t.Fatalf("want 1 tool, got %d", len(tools_))
	}
	return tools_[0]
}

// TestMCPToolRunHappyPaths 锁定产出契约三通道 + 输入透传 + 头注入。
func TestMCPToolRunHappyPaths(t *testing.T) {
	hs, cap, tlsCfg := mcpTestServer(t, mcpTestTools)
	tool := newTestMCPTool(t, hs.URL, tlsCfg, "mcp_echo", "echo_struct", "sekrit-token")

	res, err := tool.Run(context.Background(), Call{
		Inputs:         map[string]any{"city": "berlin"},
		Token:          "internal-scoped-token",
		IdempotencyKey: "att:1:2",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// structured 通道胜出(typed Out → SDK 填 structuredContent + 文本)。
	var out map[string]json.RawMessage
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("output not object: %v", err)
	}
	var sc map[string]json.RawMessage
	if err := json.Unmarshal(out["structured"], &sc); err != nil {
		t.Fatalf("structured not object: %v", err)
	}
	if string(sc["city"]) != `"berlin"` || string(sc["n"]) != "42" {
		t.Fatalf("structured = %s, want city=berlin n=42", out["structured"])
	}
	// 头注入对拍:静态 bearer + 幂等键到位;scoped token 绝不外发(凭证
	// 红线——平台内部凭证只存在于 worker 进程内)。
	auth, idem, _ := cap.snapshot()
	if len(auth) == 0 || auth[0] != "Bearer sekrit-token" {
		t.Fatalf("Authorization = %v, want Bearer sekrit-token", auth)
	}
	if len(idem) == 0 || idem[0] != "att:1:2" {
		t.Fatalf("Idempotency-Key = %v, want att:1:2", idem)
	}
	for _, a := range auth {
		if strings.Contains(a, "internal-scoped-token") {
			t.Fatalf("scoped token leaked to external server: %q", a)
		}
	}
}

// TestMCPToolRunTextChannels 锁定文本通道:合法 JSON(UseNumber 数值
// 保真,>2^53 整数经文本面不折叠)与非 JSON 文本分桶。
func TestMCPToolRunTextChannels(t *testing.T) {
	hs, _, tlsCfg := mcpTestServer(t, mcpTestTools)

	textTool := newTestMCPTool(t, hs.URL, tlsCfg, "mcp_text", "echo_text", "")
	res, err := textTool.Run(context.Background(), Call{Inputs: map[string]any{"q": 1}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("output not object: %v", err)
	}
	var content map[string]json.RawMessage
	if err := json.Unmarshal(out["content"], &content); err != nil {
		t.Fatalf("content channel not object: %v (%s)", err, out["content"])
	}
	if string(content["big"]) != "12345678901234567890" {
		t.Fatalf("text-channel big int folded: %s, want 12345678901234567890", content["big"])
	}

	plainTool := newTestMCPTool(t, hs.URL, tlsCfg, "mcp_plain", "echo_plaintext", "")
	res, err = plainTool.Run(context.Background(), Call{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("output not object: %v", err)
	}
	if string(out["content_text"]) != `"not json at all"` {
		t.Fatalf("content_text = %s", out["content_text"])
	}
}

// TestMCPToolRunIsError 锁定 server 语义失败 = 裸错误(runner 归
// tool_failed;错误文本 LLM 可读)。
func TestMCPToolRunIsError(t *testing.T) {
	hs, _, tlsCfg := mcpTestServer(t, mcpTestTools)
	tool := newTestMCPTool(t, hs.URL, tlsCfg, "mcp_boom", "boom", "")
	_, err := tool.Run(context.Background(), Call{})
	if err == nil {
		t.Fatal("want error for IsError result")
	}
	if !strings.Contains(err.Error(), "city not found") || !strings.Contains(err.Error(), "mcp_boom") {
		t.Fatalf("error = %v, want remote tool failure text", err)
	}
	var ce *CallError
	if errors.As(err, &ce) {
		t.Fatalf("tool must not wrap CallError itself (runner single point): %v", err)
	}
}

// TestMCPToolUnreachable 锁定连接失败 = 裸错误(与 http 工具传输错误
// 同桶 → tool_failed;非 infra——asynq attempt 级重试语义面)。
func TestMCPToolUnreachable(t *testing.T) {
	hs, _, tlsCfg := mcpTestServer(t, mcpTestTools)
	url := hs.URL
	hs.Close() // 端口即刻消亡:连接拒收
	tool := newTestMCPTool(t, url, tlsCfg, "mcp_dead", "echo_text", "")
	_, err := tool.Run(context.Background(), Call{})
	if err == nil {
		t.Fatal("want error for unreachable server")
	}
	if errors.Is(err, ErrSSRFBlocked) || errors.Is(err, ErrHostBlocked) {
		t.Fatalf("unreachable misclassified as guard rejection: %v", err)
	}
}

// TestMCPToolHostGateRedirect 锁定 host 门逐跳:声明宿主重定向到其他
// 宿主 = ErrHostBlocked(策略先于传输,即使目标是合法 https 公网)。
func TestMCPToolHostGateRedirect(t *testing.T) {
	// 声明的 server 实为 302 跳板:一切请求 302 → 未声明宿主。
	decoy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example.com/mcp", http.StatusFound)
	}))
	t.Cleanup(decoy.Close)
	pool := x509.NewCertPool()
	pool.AddCert(decoy.Certificate())
	tool := newTestMCPTool(t, decoy.URL, &tls.Config{RootCAs: pool}, "mcp_decoy", "echo_text", "")
	_, err := tool.Run(context.Background(), Call{})
	if !errors.Is(err, ErrHostBlocked) {
		t.Fatalf("err = %v, want ErrHostBlocked", err)
	}
}

// TestMCPToolEgressProxy 锁定 docker 模式执法面:流量过真 egress 代理
// (Allow 含 server host:port → 通;缺 → CONNECT 403 → ErrHostBlocked)。
// 127.0.0.1 目标经 IPForbidden 测试缝放行(生产该 IP 本就禁段拒)。
func TestMCPToolEgressProxy(t *testing.T) {
	hs, cap, tlsCfg := mcpTestServer(t, mcpTestTools)
	allow, err := EgressAllowFromMCPServers(map[string]string{"srv": hs.URL})
	if err != nil {
		t.Fatalf("derive allow: %v", err)
	}
	proxyAddr := startEgressProxy(t, EgressConfig{Allow: allow, IPForbidden: allowAllForbiddenOff})
	pu, err := parseEgressProxyURL("http://" + proxyAddr)
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	tools_, err := NewMCPTools(MCPConfig{
		Servers: map[string]string{"srv": hs.URL},
		Tools:   map[string]MCPToolRef{"mcp_echo": {Server: "srv", Tool: "echo_text"}},
		Guard:   GuardConfig{TLS: tlsCfg, ProxyURL: pu, IPForbidden: allowAllForbiddenOff},
	})
	if err != nil {
		t.Fatalf("NewMCPTools: %v", err)
	}
	if _, err := tools_[0].Run(context.Background(), Call{IdempotencyKey: "att:9:1"}); err != nil {
		t.Fatalf("run via egress proxy: %v", err)
	}
	auth, idem, _ := cap.snapshot()
	if len(idem) == 0 || idem[0] != "att:9:1" {
		t.Fatalf("Idempotency-Key through proxy = %v", idem)
	}
	for _, a := range auth { // 匿名连接:零 Authorization 头
		if a != "" {
			t.Fatalf("anonymous connection must not carry Authorization, got %q", a)
		}
	}

	// 拒态:代理 Allow 不含 server host → CONNECT 403 → ErrHostBlocked
	//(egressDenialError (code,reason) 配对映射,哨兵跨层保真)。
	denyProxy := startEgressProxy(t, EgressConfig{Allow: map[string]map[int]struct{}{"other.example.com": {443: {}}}, IPForbidden: allowAllForbiddenOff})
	dpu, err := parseEgressProxyURL("http://" + denyProxy)
	if err != nil {
		t.Fatalf("parse deny proxy url: %v", err)
	}
	dtools, err := NewMCPTools(MCPConfig{
		Servers: map[string]string{"srv": hs.URL},
		Tools:   map[string]MCPToolRef{"mcp_echo": {Server: "srv", Tool: "echo_text"}},
		Guard:   GuardConfig{TLS: tlsCfg, ProxyURL: dpu, IPForbidden: allowAllForbiddenOff},
	})
	if err != nil {
		t.Fatalf("NewMCPTools: %v", err)
	}
	_, err = dtools[0].Run(context.Background(), Call{})
	if !errors.Is(err, ErrHostBlocked) {
		t.Fatalf("err = %v, want ErrHostBlocked via proxy 403", err)
	}
}

// TestNewMCPToolsValidation 锁定构造校验表(词形/URL 单门/引用闭合)。
func TestNewMCPToolsValidation(t *testing.T) {
	cases := []struct {
		name    string
		servers map[string]string
		tools   map[string]MCPToolRef
		wantErr string
	}{
		{"empty tools", map[string]string{"srv": "https://a.example.com"}, nil, "declared empty"},
		{"no servers", nil, map[string]MCPToolRef{"mcp_x": {Server: "srv", Tool: "t"}}, "without servers"},
		{"bad alias", map[string]string{"Srv": "https://a.example.com"},
			map[string]MCPToolRef{"mcp_x": {Server: "Srv", Tool: "t"}}, "alias"},
		{"bad tool name", map[string]string{"srv": "https://a.example.com"},
			map[string]MCPToolRef{"Mcp-X": {Server: "srv", Tool: "t"}}, "tool name"},
		{"unknown server ref", map[string]string{"srv": "https://a.example.com"},
			map[string]MCPToolRef{"mcp_x": {Server: "other", Tool: "t"}}, "unknown server"},
		{"bad remote name", map[string]string{"srv": "https://a.example.com"},
			map[string]MCPToolRef{"mcp_x": {Server: "srv", Tool: "not allowed!"}}, "remote tool name"},
		{"http scheme", map[string]string{"srv": "http://a.example.com"},
			map[string]MCPToolRef{"mcp_x": {Server: "srv", Tool: "t"}}, "must be https"},
		{"userinfo", map[string]string{"srv": "https://u:p@a.example.com"},
			map[string]MCPToolRef{"mcp_x": {Server: "srv", Tool: "t"}}, "userinfo"},
		{"query", map[string]string{"srv": "https://a.example.com/?x=1"},
			map[string]MCPToolRef{"mcp_x": {Server: "srv", Tool: "t"}}, "query/fragment"},
		{"no host", map[string]string{"srv": "https://"},
			map[string]MCPToolRef{"mcp_x": {Server: "srv", Tool: "t"}}, "without host"},
		{"port out of range", map[string]string{"srv": "https://a.example.com:99999"},
			map[string]MCPToolRef{"mcp_x": {Server: "srv", Tool: "t"}}, "port"},
		{"port zero", map[string]string{"srv": "https://a.example.com:0"},
			map[string]MCPToolRef{"mcp_x": {Server: "srv", Tool: "t"}}, "port"},
		{"builtin name collision", map[string]string{"srv": "https://a.example.com"},
			map[string]MCPToolRef{"http": {Server: "srv", Tool: "t"}}, "collides with builtin"},
		{"non-ascii host", map[string]string{"srv": "https://mcp.exämple.com"},
			map[string]MCPToolRef{"mcp_x": {Server: "srv", Tool: "t"}}, "non-ascii host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewMCPTools(MCPConfig{Servers: tc.servers, Tools: tc.tools})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
	// 合法形:路径前缀允许(MCP 端点语义,与 http 端点不同)。
	if _, err := NewMCPTools(MCPConfig{
		Servers: map[string]string{"srv": "https://a.example.com/mcp"},
		Tools:   map[string]MCPToolRef{"mcp_x": {Server: "srv", Tool: "t"}},
	}); err != nil {
		t.Fatalf("path-prefix url must be accepted: %v", err)
	}
}

// TestMCPToolResponseBodyLimit 锁定响应体上限(评审 MEDIUM:SDK 裸
// io.ReadAll 无界读在 runner 16MiB 收口之前——上限在 header transport
// 层施加,超限截断使 SDK 解码必败 → 裸错误,不放大内存)。真 server 面
// 只回巨型 application/json body。
func TestMCPToolResponseBodyLimit(t *testing.T) {
	var served int64
	hs := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		w.Header().Set("Content-Type", "application/json")
		junk := bytes.Repeat([]byte("a"), maxMCPResponseBytes+(1<<20))
		_, _ = w.Write(junk)
	}))
	t.Cleanup(hs.Close)
	pool := x509.NewCertPool()
	pool.AddCert(hs.Certificate())
	tool := newTestMCPTool(t, hs.URL, &tls.Config{RootCAs: pool}, "mcp_big", "echo_text", "")
	_, err := tool.Run(context.Background(), Call{})
	if err == nil {
		t.Fatal("want decode error for over-limit body")
	}
	if served == 0 {
		t.Fatal("server never reached")
	}
}

// TestMCPResultTextBounded 锁定 IsError 文本预算的流内截断(go 评审
// MEDIUM:大文本 Content 先整块拷贝再截会放大峰值内存;截断分支此前无
// 直接单测)。断言:错误文本有界 + rune 边界合法 + 前缀保留。
func TestMCPResultTextBounded(t *testing.T) {
	big := strings.Repeat("x", maxMCPResponseBytes) // 单块即达响应体量级
	tool, _ := mcpToolWithDial("mcp_big_err", &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: big}},
	}, nil)
	_, err := tool.Run(context.Background(), Call{})
	if err == nil {
		t.Fatal("want IsError error")
	}
	msg := err.Error()
	if len(msg) > len("tools: mcp_big_err: remote tool \"remote_tool\" failed: ")+maxMCPErrorText+64 {
		t.Fatalf("error message unbounded: %d bytes", len(msg))
	}
	if !strings.Contains(err.Error(), "xxxx") {
		t.Fatal("prefix of long text must survive truncation")
	}
	// 多字节文本:rune 边界截断不产生无效 UTF-8。
	multibyte := strings.Repeat("汉", 1<<20)
	tool2, _ := mcpToolWithDial("mcp_mb_err", &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: multibyte}},
	}, nil)
	_, err2 := tool2.Run(context.Background(), Call{})
	if err2 == nil {
		t.Fatal("want IsError error")
	}
	if !utf8.ValidString(err2.Error()) {
		t.Fatal("truncated message must stay valid UTF-8")
	}
}

// TestMCPToolProtocolPinRejectsLegacy 锁定协议钉死执法点(对抗审计
// MEDIUM:server 支持表换 legacy 版本时 SDK 会协商降级并成功建会话,
// 唯一防线 = Connect 后的版本比对——此前该分支零覆盖,删除不红)。
func TestMCPToolProtocolPinRejectsLegacy(t *testing.T) {
	hs, _, tlsCfg := mcpTestServerOpts(t, []string{"2025-06-18"}, true, mcpTestTools)
	tool := newTestMCPTool(t, hs.URL, tlsCfg, "mcp_legacy", "echo_text", "")
	_, err := tool.Run(context.Background(), Call{})
	if err == nil || !strings.Contains(err.Error(), "negotiated protocol") {
		t.Fatalf("err = %v, want negotiated protocol rejection (pinned %s)", err, MCPProtocolVersion)
	}
}

// TestMCPToolSSEResponsePath 行使 SSE 帧化响应路径(对抗审计 LOW:
// JSONResponse:true 钉死使 SDK 的 SSE 解码 + limitedBody 叠加从未跑过;
// DisableStandaloneSSE 只关 GET 推送流,POST 响应 SSE 帧是规范常态)。
func TestMCPToolSSEResponsePath(t *testing.T) {
	hs, _, tlsCfg := mcpTestServerOpts(t, []string{MCPProtocolVersion}, false, mcpTestTools)
	tool := newTestMCPTool(t, hs.URL, tlsCfg, "mcp_sse", "echo_text", "")
	res, err := tool.Run(context.Background(), Call{Inputs: map[string]any{"q": 1}})
	if err != nil {
		t.Fatalf("run over SSE-framed responses: %v", err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(res.Output, &out); err != nil || out["content"] == nil {
		t.Fatalf("output = %s (%v), want content channel", out, err)
	}
}

// rtFunc 是 RoundTripper 测试缝(transport 单测)。
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestMCPHeaderTransportBodyCap 直接锁定响应体上限的存在与值(对抗审计
// MEDIUM:端到端 junk 用例无判别力——解码失败在有无上限时同形;此用例
// 断言包裹后的 body 恰好读到 maxMCPResponseBytes,删除上限即红)。
// 同锚定头注入(Clone 不改原请求)。
func TestMCPHeaderTransportBodyCap(t *testing.T) {
	var gotAuth, gotIdem string
	inner := rtFunc(func(r *http.Request) (*http.Response, error) {
		gotAuth, gotIdem = r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key")
		return &http.Response{
			StatusCode: 200,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(make([]byte, maxMCPResponseBytes+(1<<20)))),
		}, nil
	})
	h := &mcpHeaderTransport{rt: inner, auth: "Bearer x", idem: "att:1:1"}
	req := httptest.NewRequest(http.MethodPost, "https://srv.example/mcp", nil)
	resp, err := h.RoundTrip(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if int64(len(data)) != maxMCPResponseBytes {
		t.Fatalf("body read = %d bytes, want capped at %d (cap deleted or miswired)", len(data), maxMCPResponseBytes)
	}
	if gotAuth != "Bearer x" || gotIdem != "att:1:1" {
		t.Fatalf("headers = %q/%q, want injected", gotAuth, gotIdem)
	}
	if req.Header.Get("Authorization") != "" || req.Header.Get("Idempotency-Key") != "" {
		t.Fatal("original request must not be mutated (Clone contract)")
	}
}
