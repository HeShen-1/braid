package tools

// MCP 消费面客户端工具(T13.2,issue #112;D-2026-09-30-t132)。外部
// MCP server 的工具经签名清单/静态 env 声明后,以独立 Tool 载体注册进
// Registry——与内置工具同权限模型:RBAC 门、scoped token、registry
// 分派、产出契约、审计全部走 LocalRunner 既有链,本文件零旁路。
//
// 白名单/沙箱约束不降级的执法面(D-t132 决策 3):
//
//   - 工具集静态声明(toolpackage.go),Names() 不受外部 server 状态影响;
//   - transport 仅 streamable http 且强制 https(stdio 不支持——本地
//     子进程执行域无网络守卫面,支持即降级);
//   - 出网守卫与内置 http 工具同源:NewGuardedClient(httpsOnly/拨号
//     复核/重定向逐跳)+ 本工具级 host 白名单(= 所声明 server 宿主,
//     逐跳);docker 模式叠加 egress 代理 CONNECT 隧道(ProxyURL →
//     DialTLSContext,server host:port 经 EgressAllowFromMCPServers 单源
//     派生进代理 Allow 集,见 egress.go);
//   - per-call session:每次 Run 新建 Connect→CallTool→Close(无进程级
//     连接状态,规避 30min 隧道期限;握手开销在节点预算内)。
//
// 凭证红线:外部 server 静态 bearer 恒 env 通道(BRAID_MCP_TOKEN_
// <ALIAS>,MCPTokenEnvKey 派生),清单/签名工件零凭证,错误零回显;
// LocalRunner 注入 Call.Token 的 per-call scoped token 是平台内部凭证,
// **绝不外发**(不进任何出站请求头)。幂等键透传 Idempotency-Key 头
// (http 工具同款,AGENTS 铁律)。
//
// 失败分类(与内置工具同桶,runner 零改动零新码):server 语义失败
// (IsError)与传输错误 = tools: 前缀裸错误 → tool_failed;ctx deadline
// 由错误链承载 → infra 留 RUNNING;ErrHostBlocked/ErrSSRFBlocked 哨兵
// 经 errors.Is → tool_host_blocked/tool_ssrf_blocked。
//
// 已知边界(D-t132 登记):go-sdk v1.8 client 把 structuredContent 解码
// 为 any(数值 → float64),>2^53 整数经 structured 通道精度受限;保真
// 通道 = 文本 Content 面(UseNumber 解码,与 http 工具 body 同口径)。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPProtocolVersion 是唯一支持的 MCP 协议版本(D-t131 决策 2 钉死;
// 消费面对称钉死)。单一真源:server 面(internal/api/mcp.go)引用本
// 常量。Connect 后校验实际协商结果,不符即败——明拒优于暗降。
const MCPProtocolVersion = "2026-07-28"

// mcpClientImpl 是 client 侧 Implementation 标识(server 日志面可见的
// 调用方身份;与 server 面 Name "braid" 区分,worker 是发起方)。
var mcpClientImpl = &mcp.Implementation{Name: "braid-worker", Version: "v1"}

// mcpRemoteToolRe server 侧工具名词形(MCP 规范 ^[a-zA-Z0-9_-]{1,64}$,
// fail-closed:规范外名字不支持——透传面拒绝优于运行期 server 报错)。
var mcpRemoteToolRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// MCPToolRef 是单个外部 MCP 工具的声明(签名清单与非签名 env 同形):
// Server = server 别名(必须 ∈ MCPConfig.Servers);Tool = server 侧
// 工具名(透传 CallTool,不经归一化)。
type MCPToolRef struct {
	Server string `json:"server"`
	Tool   string `json:"tool"`
}

// MCPConfig 是 MCP 工具族的构造配置。Servers:别名 → https base URL
// (路径前缀允许——MCP 端点常带 /mcp 路径,与 http 工具端点「无路径」
// 语义不同,单点校验见 mcpServerURL);Tools:注册名 → 引用;Tokens:
// 别名 → 静态 bearer(env 通道装配,缺席 = 匿名连接);Guard 与 http
// 工具同判:生产恒零值 + docker 模式 ProxyURL,测试注入 TLS/IPForbidden。
type MCPConfig struct {
	Servers map[string]string
	Tools   map[string]MCPToolRef
	Tokens  map[string]string
	Guard   GuardConfig
}

// MCPTokenEnvKey 派生 server 别名的凭证 env 键。别名词形
// ^[a-z][a-z0-9_]{0,63}$(endpointNameRe,小写域)保证大写折叠无歧义;
// 值经 strings.TrimSpace 消费,零回显(错误消息不含 token)。
func MCPTokenEnvKey(alias string) string {
	return "BRAID_MCP_TOKEN_" + strings.ToUpper(alias)
}

// mcpServerURL 校验并解析 server base URL(构造期单门;egress 派生侧
// 二道防御)。与 http 端点校验的差异仅在路径:MCP 端点允许路径前缀
// (SDK 把 Endpoint 当 POST 目标原样使用),其余同门——https 强制、
// userinfo 拒(凭证不进 URL)、非 ASCII 宿主拒(IDNA 差分防线)、
// query/fragment 拒(声明面只定位置,调用期无参数拼接)。
func mcpServerURL(alias, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("tools: mcp server %q: parse url: %w", alias, err)
	}
	switch {
	case u.Scheme != "https":
		return nil, fmt.Errorf("tools: mcp server %q: url must be https, got %q", alias, mcpRedactURL(u, raw))
	case u.User != nil:
		return nil, fmt.Errorf("tools: mcp server %q: userinfo in url forbidden (token goes in %s)", alias, MCPTokenEnvKey(alias))
	case u.Hostname() == "":
		return nil, fmt.Errorf("tools: mcp server %q: url without host", alias)
	case !isASCIIHost(u.Hostname()):
		return nil, fmt.Errorf("tools: mcp server %q: non-ascii host %q forbidden", alias, u.Hostname())
	case u.RawQuery != "" || u.Fragment != "":
		return nil, fmt.Errorf("tools: mcp server %q: url must not carry query/fragment", alias)
	}
	// 端口范围与 egress 派生同门(评审 LOW:url.Parse 接受 :99999/:0,
	// 两道门口径漂移 = off 模式构造通过、docker 模式拒启的不对称)。
	if p := u.Port(); p != "" {
		if v, perr := strconv.Atoi(p); perr != nil || v < 1 || v > 65535 {
			return nil, fmt.Errorf("tools: mcp server %q: port %q invalid", alias, p)
		}
	}
	return u, nil
}

// NewMCPTools 构造声明的全部 MCP 工具。逐工具独立载体:host 白名单 =
// 单 server 宿主(最小授权,http 工具「Endpoints 宿主集」的单元素形)。
// 构造期零出网(per-call session);值校验启动即现。遍历序确定化
// (toolpackage.go 同款纪律:多缺陷报错可复现)。
func NewMCPTools(cfg MCPConfig) ([]Tool, error) {
	if len(cfg.Tools) == 0 {
		return nil, fmt.Errorf("tools: mcp tools declared empty")
	}
	if len(cfg.Servers) == 0 {
		return nil, fmt.Errorf("tools: mcp tools declared without servers")
	}
	servers := make(map[string]*url.URL, len(cfg.Servers))
	for _, alias := range slices.Sorted(maps.Keys(cfg.Servers)) {
		if !endpointNameRe.MatchString(alias) {
			return nil, fmt.Errorf("tools: mcp server alias %q invalid (want ^[a-z][a-z0-9_]{0,63}$)", alias)
		}
		u, err := mcpServerURL(alias, cfg.Servers[alias])
		if err != nil {
			return nil, err
		}
		servers[alias] = u
	}
	out := make([]Tool, 0, len(cfg.Tools))
	for _, name := range slices.Sorted(maps.Keys(cfg.Tools)) {
		ref := cfg.Tools[name]
		if _, builtin := knownPackageTools[name]; builtin {
			// 内置名撞名拒(评审 LOW:签名面在解析期拒,构造器兜住 env 路径
			// ——否则未装配同名内置工具时 "http" 名下实为外部调用,语义
			// 劫持通道;D-t132 决策 1「撞名解析期拒」两路径对称)。
			return nil, fmt.Errorf("tools: mcp tool name %q collides with builtin tool", name)
		}
		if !endpointNameRe.MatchString(name) {
			return nil, fmt.Errorf("tools: mcp tool name %q invalid (want ^[a-z][a-z0-9_]{0,63}$)", name)
		}
		base, ok := servers[ref.Server]
		if !ok {
			return nil, fmt.Errorf("tools: mcp tool %q references unknown server %q", name, ref.Server)
		}
		if !mcpRemoteToolRe.MatchString(ref.Tool) {
			return nil, fmt.Errorf("tools: mcp tool %q: remote tool name %q invalid (want ^[a-zA-Z0-9_-]{1,64}$)", name, ref.Tool)
		}
		out = append(out, newMCPClientTool(name, ref, base, cfg.Tokens[ref.Server], cfg.Guard))
	}
	return out, nil
}

// newMCPClientTool 构造单工具载体:守卫客户端(httpsOnly + 拨号复核)
// 外包逐跳 host 门(NewHTTPTool 同款双层 CheckRedirect:策略门先于
// 传输门——非声明宿主即使是合法 https 公网目标也拒)。
func newMCPClientTool(name string, ref MCPToolRef, base *url.URL, token string, guard GuardConfig) *mcpClientTool {
	client := NewGuardedClient(guard)
	allow := map[string]struct{}{allowlistKey(base): {}}
	inner := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := checkHostAllowed(allow, req.URL); err != nil {
			return err
		}
		return inner(req, via)
	}
	return &mcpClientTool{
		name:   name,
		remote: ref.Tool,
		base:   base,
		token:  strings.TrimSpace(token),
		client: client,
	}
}

// mcpCaller 是 *mcp.ClientSession 的最小消费面(测试缝:同包测试注入
// 假 session,免起真 HTTP 即可驱动协议逻辑分支)。
type mcpCaller interface {
	CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error)
	Close() error
}

// mcpClientTool 是外部 MCP 工具的进程内载体(Tool 接口;两模式均不进
// 沙箱容器——外部 server 本身就是外部执行域,执法面在出网守卫)。
type mcpClientTool struct {
	name   string
	remote string
	base   *url.URL
	token  string
	client *http.Client
	// dial 缺省 = mcpStreamableDial(真 transport);nil 判定在 Run 内
	// (零值载体直接可用,装配路径无需感知缝)。
	dial func(ctx context.Context, t *mcpClientTool, call Call) (mcpCaller, error)
}

// Name 满足 Tool 接口(词表与 DSL 工具名同域,装配期已校验)。
func (t *mcpClientTool) Name() string { return t.name }

// maxMCPResponseBytes 是 MCP 响应体读取上限(评审 MEDIUM:SDK v1.8 对
// application/json 响应用裸 io.ReadAll——streamable.go handleJSON,无界
// 读发生在 runner 16MiB 产出收口之前)。界 = DefaultMaxHTTPBody(8MiB,
// 与内置 http 工具**同威胁模型同界**):除无界读外,SDK 随后对全量 body
// 做 any 解码,稠密 JSON 的对象图放大约 24-48x(对抗审计:32MiB 界下
// 单次恶意响应可达 ~GB 级堆;8MiB 界把最坏峰值压回 ~200MB 量级)。
// 超限截断使 SDK JSON 解码必然失败 → tool_failed(响亮,不静默)。
// 代价:线上大于 8MiB 的合法 MCP 结果会失败——与 http 工具 body 同判,
// 属资源界而非契约界(SSE 路径 SDK 侧另有 DefaultMaxEventSize 单事件界)。
const maxMCPResponseBytes = DefaultMaxHTTPBody

// mcpRedactURL 剥除 URL userinfo 后回显(凭证红线:错误零回显——运维
// 把凭证误写进 URL 时,报错原文不得把它带进启动日志;对抗审计 MEDIUM:
// scheme 门先于 userinfo 门命中时 got %q 会整串回显原始 URL)。
func mcpRedactURL(u *url.URL, raw string) string {
	if u != nil && u.User != nil {
		c := *u
		c.User = nil
		return c.String()
	}
	return raw
}

// mcpHeaderTransport 注入外部 server 静态 bearer 与 attempt 幂等键头,
// 并对响应体施加 maxMCPResponseBytes 上限(见常量注释)。注入层在守卫
// Transport 之上、Client 之下:重定向逐跳由 CheckRedirect 的 host 门先
// 拦(换宿请求到不了本层),凭证因此不会发往声明宿主之外(同宿重定向
// 重新注入 = 同一 server,无害)。请求经 Clone 再改头(RoundTripper 不
// 改原请求的 stdlib 契约)。零回显:错误链不含 token。
type mcpHeaderTransport struct {
	rt   http.RoundTripper
	auth string // 完整 Authorization 头值("" = 匿名)
	idem string
}

// limitedBody 把限读视图叠在原始 body 上(Close 归原始 body)。
type limitedBody struct {
	io.Reader
	io.Closer
}

func (h *mcpHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if h.auth != "" || h.idem != "" {
		req = req.Clone(req.Context())
		if h.auth != "" {
			req.Header.Set("Authorization", h.auth)
		}
		if h.idem != "" {
			req.Header.Set("Idempotency-Key", h.idem)
		}
	}
	resp, err := h.rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &limitedBody{Reader: io.LimitReader(resp.Body, maxMCPResponseBytes), Closer: resp.Body}
	return resp, nil
}

// mcpStreamableDial 是生产 dial:per-call http.Client(守卫 Transport +
// 头注入包装)+ StreamableClientTransport。MaxRetries=-1 禁 SDK 层重连
// 重试(单次 attempt = 单次调用;attempt 级重试归 asynq,幂等键同键);
// DisableStandaloneSSE=true 不开长连接推送流(纯 POST 请求-响应,规避
// 30min 隧道期限)。协议版本硬钉,Connect 后校验协商结果——server 暗降
// 即败(明拒优于暗降,D-t131 同判)。
func mcpStreamableDial(ctx context.Context, t *mcpClientTool, call Call) (mcpCaller, error) {
	auth := ""
	if t.token != "" {
		auth = "Bearer " + t.token
	}
	hc := &http.Client{
		Transport:     &mcpHeaderTransport{rt: t.client.Transport, auth: auth, idem: call.IdempotencyKey},
		CheckRedirect: t.client.CheckRedirect,
	}
	tr := &mcp.StreamableClientTransport{
		Endpoint:             t.base.String(),
		HTTPClient:           hc,
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}
	cl := mcp.NewClient(mcpClientImpl, nil)
	sess, err := cl.Connect(ctx, tr, &mcp.ClientSessionOptions{ProtocolVersion: MCPProtocolVersion})
	if err != nil {
		return nil, err
	}
	if ir := sess.InitializeResult(); ir != nil && ir.ProtocolVersion != MCPProtocolVersion {
		_ = sess.Close()
		return nil, fmt.Errorf("tools: mcp server negotiated protocol %q, want pinned %q", ir.ProtocolVersion, MCPProtocolVersion)
	}
	return sess, nil
}

// Run 执行一次外部工具调用。错误语义(映射契约单点在 runner):连接/
// 协议/传输错误与 server 语义失败(IsError)= tools: 前缀裸错误 →
// tool_failed(与 http 工具同桶;LLM 可读错误文本并改写输入重试);
// 守卫哨兵经 errors.Is 上抛分类;ctx 取消由 SDK 错误链承载(infra)。
// 整体耗时由 ctx(节点 attempt deadline)强制约束,本层不另设超时
// (http/db_query 工具同款硬契约)。
func (t *mcpClientTool) Run(ctx context.Context, call Call) (Result, error) {
	dial := t.dial
	if dial == nil {
		dial = mcpStreamableDial
	}
	sess, err := dial(ctx, t, call)
	if err != nil {
		return Result{}, fmt.Errorf("tools: %s: connect mcp server: %w", t.name, err)
	}
	defer func() { _ = sess.Close() }()
	args := call.Inputs
	if args == nil {
		args = map[string]any{}
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: t.remote, Arguments: args})
	if err != nil {
		return Result{}, fmt.Errorf("tools: %s: call remote tool %q: %w", t.name, t.remote, err)
	}
	if res.IsError {
		return Result{}, fmt.Errorf("tools: %s: remote tool %q failed: %s", t.name, t.remote, mcpResultText(res))
	}
	out, err := mcpToolOutput(res)
	if err != nil {
		return Result{}, fmt.Errorf("tools: %s: %w", t.name, err)
	}
	raw, err := marshalToolOutput(out)
	if err != nil {
		return Result{}, fmt.Errorf("tools: %s: %w", t.name, err)
	}
	return Result{Output: raw}, nil
}

// maxMCPErrorText 是 IsError 文本拼装上限(node error_message 列防御界;
// 截断优于丢弃——server 侧错误文本可能任意长)。
const maxMCPErrorText = 1024

// mcpResultText 拼装 IsError 结果的可读文本(文本 Content 依次拼接;
// 无文本面时给确定性占位)。预算 = maxMCPErrorText 字节,**流内截断**:
// 单个 Content 文本可达响应体上限(~32MiB),先整块拷贝再截会放大峰值
// 内存(go 评审 MEDIUM:[]rune 化更是一次 4× 再分配)——每块按剩余字节
// 预算截(rune 边界,尾部不完整序列最多回退 3 字节),Builder 总量恒
// 有界;ASCII 文本享受完整字节预算。
func mcpResultText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok && tc.Text != "" {
			if sb.Len() > 0 {
				sb.WriteString("; ")
			}
			remaining := maxMCPErrorText - sb.Len()
			if remaining <= 0 {
				break
			}
			sb.WriteString(truncateRunes(tc.Text, remaining))
			if sb.Len() >= maxMCPErrorText {
				break
			}
		}
	}
	s := sb.String()
	if s == "" {
		s = "(no text content)"
	}
	return s
}

// truncateRunes 在 maxBytes 内截断字符串,rune 边界对齐(切点落在
// UTF-8 序列中间时回退到序列头,至多 3 字节;不产生无效字节序列)。
func truncateRunes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

// mcpToolOutput 构造产出对象(runner validateObjectOutput 统一收口):
//
//	structuredContent 在场 → {"structured": <原值>}(数值精度边界见文件头,
//	any 解码已在 SDK 侧发生,本层不二次抢救);
//	缺席 → 首个文本 Content 按 http 工具 body 同口径:合法 JSON(UseNumber
//	解码 + 尾随垃圾拒)→ {"content": <原值>}(数值保真通道),否则
//	{"content_text": <文本>};两者全缺 = 畸形产出 → 错误。
//
// 单通道选择(structured 优先):extractOutputs 的声明抽取以键定界,
// 双通道并存只会制造歧义。
func mcpToolOutput(res *mcp.CallToolResult) (map[string]any, error) {
	if res.StructuredContent != nil {
		return map[string]any{"structured": res.StructuredContent}, nil
	}
	for _, c := range res.Content {
		tc, ok := c.(*mcp.TextContent)
		if !ok || strings.TrimSpace(tc.Text) == "" {
			continue
		}
		trimmed := strings.TrimSpace(tc.Text)
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.UseNumber()
		var parsed any
		if err := dec.Decode(&parsed); err == nil {
			var extra any
			if err := dec.Decode(&extra); errors.Is(err, io.EOF) {
				return map[string]any{"content": parsed}, nil
			}
		}
		return map[string]any{"content_text": trimmed}, nil
	}
	return nil, fmt.Errorf("mcp result carries neither structured content nor text content")
}
