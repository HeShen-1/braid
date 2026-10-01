package tools

// MCP 装配面测试(T13.2,D-t132):BuiltinToolsFromEnv 的 MCP 分支
//(off/docker/api 三形态、凭证 env 单读、ProxyURL 消费)+ egress 派生表
//(EgressAllowFromMCPServers:与 http 端点同语义、路径前缀差异锚)。

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuiltinToolsFromEnvMCP:off 模式装配 + 凭证 env 单读 + Names() 白名单。
func TestBuiltinToolsFromEnvMCP(t *testing.T) {
	ts, err := BuiltinToolsFromEnv(envOf(map[string]string{
		"BRAID_MCP_SERVERS":   `{"weather":"https://mcp.example.com"}`,
		"BRAID_MCP_TOOLS":     `{"mcp_weather_lookup":{"server":"weather","tool":"lookup_weather"}}`,
		"BRAID_MCP_TOKEN_FOO": "ignored-unreferenced-alias",
		"BRAID_TOOL_SANDBOX":  "off",
	}))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(ts) != 1 {
		t.Fatalf("tools = %d, want 1", len(ts))
	}
	mt, ok := ts[0].(*mcpClientTool)
	if !ok {
		t.Fatalf("tool = %T, want *mcpClientTool (in-process carrier, no sandbox swap)", ts[0])
	}
	if mt.Name() != "mcp_weather_lookup" || mt.remote != "lookup_weather" {
		t.Fatalf("tool = %s/%s", mt.Name(), mt.remote)
	}
	// 凭证 env 单读:仅被引用 server 的 token 被装配(未被引用的 FOO 键
	// 无消费面)。装配值经 TrimSpace 归一。
	if mt.token != "" {
		t.Fatalf("unreferenced alias token leaked into assembly: %q", mt.token)
	}
	// 发布白名单真源:注册名进 Names()(api/worker 单源装配同集)。
	if got := strings.Join(registryOf(t, ts...).Names(), ","); got != "mcp_weather_lookup" {
		t.Fatalf("registry names = %q", got)
	}
}

// TestBuiltinToolsFromEnvMCPTokenRead:被引用 server 的凭证键被单读并注入
// (白盒字段断言,builtin_test 载体字段先例;Bearer 行为面在
// mcpclient_test 已锚)。值经 TrimSpace 归一;无键 = 匿名连接。
func TestBuiltinToolsFromEnvMCPTokenRead(t *testing.T) {
	ts, err := BuiltinToolsFromEnv(envOf(map[string]string{
		"BRAID_MCP_SERVERS": `{"weather":"https://mcp.example.com"}`,
		"BRAID_MCP_TOOLS":   `{"mcp_weather_lookup":{"server":"weather","tool":"lookup_weather"}}`,
	}))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if mt := ts[0].(*mcpClientTool); mt.token != "" {
		t.Fatalf("token = %q, want empty (anonymous)", mt.token)
	}

	ts2, err := BuiltinToolsFromEnv(envOf(map[string]string{
		"BRAID_MCP_SERVERS":       `{"weather":"https://mcp.example.com"}`,
		"BRAID_MCP_TOOLS":         `{"mcp_weather_lookup":{"server":"weather","tool":"lookup_weather"}}`,
		"BRAID_MCP_TOKEN_WEATHER": "  sekrit-token  ",
	}))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if mt2 := ts2[0].(*mcpClientTool); mt2.token != "sekrit-token" {
		t.Fatalf("token = %q, want trimmed sekrit-token", mt2.token)
	}
}

// TestBuiltinToolsFromEnvMCPDockerProxy:docker 模式 ProxyURL 消费锚——
// 装配读 sandboxConfig.EgressAddr 进守卫;指向全拒代理 = 一切出站 CONNECT
// 403 → ErrHostBlocked(行为证明代理路径真的被走到,非仅字段赋值)。
func TestBuiltinToolsFromEnvMCPDockerProxy(t *testing.T) {
	deny := startEgressProxy(t, EgressConfig{Allow: map[string]map[int]struct{}{}})
	env := map[string]string{
		"BRAID_TOOL_SANDBOX":             "docker",
		"BRAID_MCP_SERVERS":              `{"srv":"https://mcp.example.com"}`,
		"BRAID_MCP_TOOLS":                `{"mcp_echo":{"server":"srv","tool":"echo_text"}}`,
		"BRAID_TOOL_SANDBOX_EGRESS_ADDR": "http://" + deny,
	}
	ts, err := BuiltinToolsFromEnv(envOf(env))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	mt := ts[0].(*mcpClientTool)
	if _, err := mt.Run(t.Context(), Call{}); err == nil || !strings.Contains(err.Error(), "connect mcp server") {
		t.Fatalf("err = %v, want connect failure via deny-all proxy", err)
	}
}

// TestBuiltinToolsFromEnvMCPAPIForm:api 进程形态(docker 模式但无
// EGRESS_ADDR)装配成功零外联(Names() 可用;执行仅在 worker——结构
// 保证与 http 载体在 api 的惰性构造同判)。
func TestBuiltinToolsFromEnvMCPAPIForm(t *testing.T) {
	ts, err := BuiltinToolsFromEnv(envOf(map[string]string{
		"BRAID_TOOL_SANDBOX": "docker",
		"BRAID_MCP_SERVERS":  `{"srv":"https://mcp.example.com"}`,
		"BRAID_MCP_TOOLS":    `{"mcp_echo":{"server":"srv","tool":"echo_text"}}`,
	}))
	if err != nil {
		t.Fatalf("api-form assembly must succeed without egress addr: %v", err)
	}
	if len(ts) != 1 || ts[0].Name() != "mcp_echo" {
		t.Fatalf("tools = %v", ts)
	}
}

// TestEgressAllowFromMCPServers:派生表(与 EgressAllowFromEndpoints 同
// 语义;差异锚 = 路径前缀允许)。
func TestEgressAllowFromMCPServers(t *testing.T) {
	got, err := EgressAllowFromMCPServers(map[string]string{
		"a": "https://mcp.example.com",          // 缺省 443
		"b": "https://other.example.com:8443/x", // 显式端口 + 路径前缀
		"c": "https://MCP.example.com",          // host 归一小写
	})
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	want := map[string]map[int]struct{}{
		"mcp.example.com":   {443: {}},
		"other.example.com": {8443: {}},
	}
	if len(got) != len(want) {
		t.Fatalf("allow = %v, want %v", got, want)
	}
	for h, ports := range want {
		if got[h] == nil || len(got[h]) != len(ports) {
			t.Fatalf("allow[%s] = %v, want %v", h, got[h], ports)
		}
		for p := range ports {
			if _, ok := got[h][p]; !ok {
				t.Fatalf("allow[%s] missing port %d", h, p)
			}
		}
	}
	for _, bad := range []string{
		"http://mcp.example.com",       // 明文 scheme 拒
		"https://u:p@mcp.example.com",  // userinfo 拒
		"https://mcp.example.com/?q=1", // query 拒
		"https://",                     // 无宿主
	} {
		if _, err := EgressAllowFromMCPServers(map[string]string{"x": bad}); err == nil {
			t.Fatalf("bad url %q must reject", bad)
		}
	}
}

// TestSandboxEgressStartupMCPAllow 锁定 docker 模式生产接线:SandboxEgress
// Startup 从 MCP 声明单源派生 host:port 进代理 Allow 集并集(对抗审计
// MEDIUM:派生函数/代理 403/ProxyURL 消费各自有测,但这条装配缝——
// EgressAllowFromMCPServers + mergeEgressAllow 在 startup 的调用——此前
// 零覆盖,删除整个合并块全套件仍绿)。docker shim + 真代理 + 探针,
// 与 TestSandboxEgressStartup 同形:声明 host(环回 IP)在白名 → 禁段
// 403 ssrf(派生 + 默认禁段双证);非声明 host → 403 host_blocked。
func TestSandboxEgressStartupMCPAllow(t *testing.T) {
	bin, _ := fakeNamedDockerShim(t, "docker", egressStartupShimBody("true"))
	t.Setenv("PATH", filepath.Dir(bin))
	u, err := SandboxEgressStartup(context.Background(), envOf(map[string]string{
		"BRAID_TOOL_SANDBOX":             "docker",
		"BRAID_TOOL_SANDBOX_EGRESS_PORT": "0",
		"BRAID_MCP_SERVERS":              `{"m":"https://127.0.0.1:1"}`,
		"BRAID_MCP_TOOLS":                `{"mcp_x":{"server":"m","tool":"t"}}`,
	}), "w1")
	if err != nil {
		t.Fatalf("startup: %v", err)
	}
	code, reason := egressProbe(t, u.ProxyURL.Host, "127.0.0.1:1")
	if code != 403 || reason != egressReasonSSRFBlocked {
		t.Fatalf("probe declared host = %d %q, want 403 %s (host in allow, loopback IP forbidden)", code, reason, egressReasonSSRFBlocked)
	}
	code, reason = egressProbe(t, u.ProxyURL.Host, "evil.example:443")
	if code != 403 || reason != egressReasonHostBlocked {
		t.Fatalf("probe undeclared host = %d %q, want 403 %s", code, reason, egressReasonHostBlocked)
	}
}
