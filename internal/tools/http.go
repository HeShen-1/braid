package tools

// http 内置工具(T3.5,issue #32)。地基 = T3.3 的 NewGuardedClient
// (httpsOnly / Dialer.Control 拨号复核 / CheckRedirect 每跳剥凭证),
// 本文件加策略层:host 白名单逐跳校验。
//
// 输入契约(DSL 只声明位置无关参数;host 词表被 T1.2 notNetworkKey
// 结构性挡在 DSL 外,运行时白名单是权威门禁):
//
//	name   必填,端点别名(BRAID_HTTP_ENDPOINTS 键)——host 的唯一来路
//	method 可选,GET(默认)|POST|PUT|PATCH|DELETE(大小写不敏感)
//	path   必填,以 / 开头的路径(前缀校验阻断 scheme 与 //host 走私;
//	       不含 ? 与 #,查询参数走 query)
//	query  可选,对象;值 = 标量(string/number/boolean)或标量数组
//	       (数组 → 同键重复参数),其余类型响亮拒
//	body   可选,任意 JSON 值(application/json 发出;GET 拒 body)
//
// 凭证红线(T3.3 装配约束):P0 不支持任何凭证输入——Authorization/
// Cookie/自定义头均不可经 DSL 注入,守卫的逐跳剥凭证因此平凡成立;端点级
// 静态凭证(放 Authorization)属 P1。幂等键按 D-t32 #6 透传为
// Idempotency-Key 头(非凭证类,跨跳保留——T3.3 CheckRedirect 注释锚定)。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DefaultMaxHTTPBody 是响应体读取上限(与 CommandTool DefaultMaxStdout
// 同量级:远低于 max_allowed_packet 的防 OOM 界;attempt 内公网端点可灌
// 任意字节——ssrf.go NewGuardedClient「响应体限额归工具实现」边界注释)。
const DefaultMaxHTTPBody = 8 << 20 // 8 MiB

// maxHTTPPathBytes 是 path 输入长度上限(DSL 字符串上界 64 KiB 的收窄:
// 超长路径对真实端点只会 4xx,提前响亮拒)。
const maxHTTPPathBytes = 8 << 10

// ErrHostBlocked 是 host 白名单拒绝的哨兵错误:首请求复核与重定向逐跳
// Location 校验共用。与 ErrSSRFBlocked 分立——策略拒连 vs 网络位置守卫,
// 失败码分桶(tool_host_blocked vs tool_ssrf_blocked),审计可区分。映射
// 契约同款:工具返回携带本哨兵的裸错误,LocalRunner errors.Is 分类。
var ErrHostBlocked = errors.New("tools: host not allowlisted")

// endpointNameRe 端点别名词表:与 DSL 工具名(toolNode.tool pattern)同款,
// 两处词表对齐使「inputs.name 引用端点」与「tool 名引用工具」同一命名域。
var endpointNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// httpMethodWords 是 method 输入词表(大写比对;小写输入归一化后匹配)。
var httpMethodWords = map[string]struct{}{
	http.MethodGet:    {},
	http.MethodPost:   {},
	http.MethodPut:    {},
	http.MethodPatch:  {},
	http.MethodDelete: {},
}

// HTTPConfig 是 http 工具的构造配置。Endpoints:别名→base URL(仅
// scheme+host[:port];path/query/fragment/userinfo 构造期拒——完整路径每次
// 调用经 DSL 声明,base 不做隐式前缀拼接)。Guard 直通 NewGuardedClient:
// **生产装配恒零值**(ssrf.go GuardConfig.IPForbidden 红线:非 nil 整体
// 替换默认禁段且静默无报错);仅测试注入 IPForbidden/TLS/Resolver。
// MaxBodyBytes ≤0 取 DefaultMaxHTTPBody。
type HTTPConfig struct {
	Endpoints    map[string]string
	Guard        GuardConfig
	MaxBodyBytes int64
}

// httpEndpoint 是构造期拆解后的端点(base URL 规范形)。
type httpEndpoint struct {
	base *url.URL
}

// HTTPTool 是内置 http 工具(Tool 接口进程内载体,T3.2 D-t32 #1)。
type HTTPTool struct {
	client    *http.Client
	endpoints map[string]httpEndpoint
	allow     map[string]struct{} // 小写 hostname → 存在(host 白名单)
	maxBody   int64
}

// NewHTTPTool 构造 http 工具。host 白名单 = Endpoints 宿主集;逐跳校验
// 通过二次包装 NewGuardedClient 的 CheckRedirect 实现(外层 host 门 →
// 内层守卫:剥凭证/https 门/跳数上限)——**禁止只查首请求**,重定向跳
// 公网非白名 host 必须被拒(issue #32 硬约束)。首请求宿主来自构造期
// 拆解的 base URL(白名单即其来源),Run 内对最终 URL 再复核一次。
func NewHTTPTool(cfg HTTPConfig) (*HTTPTool, error) {
	if len(cfg.Endpoints) == 0 {
		return nil, fmt.Errorf("tools: http tool with empty endpoints")
	}
	eps := make(map[string]httpEndpoint, len(cfg.Endpoints))
	allow := make(map[string]struct{}, len(cfg.Endpoints))
	for name, raw := range cfg.Endpoints {
		if !endpointNameRe.MatchString(name) {
			return nil, fmt.Errorf("tools: http endpoint name %q invalid (want ^[a-z][a-z0-9_]{0,63}$)", name)
		}
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("tools: http endpoint %q: parse base url: %w", name, err)
		}
		switch {
		case u.Scheme != "https":
			return nil, fmt.Errorf("tools: http endpoint %q: base url must be https, got %q", name, raw)
		case u.User != nil:
			return nil, fmt.Errorf("tools: http endpoint %q: userinfo in base url forbidden", name)
		case u.Hostname() == "":
			return nil, fmt.Errorf("tools: http endpoint %q: base url without host", name)
		case !isASCIIHost(u.Hostname()):
			return nil, fmt.Errorf("tools: http endpoint %q: non-ascii host %q forbidden", name, u.Hostname())
		case u.EscapedPath() != "" && u.EscapedPath() != "/":
			return nil, fmt.Errorf("tools: http endpoint %q: base url must not carry path (declare full path per call), got %q", name, u.EscapedPath())
		case u.RawQuery != "" || u.Fragment != "":
			return nil, fmt.Errorf("tools: http endpoint %q: base url must not carry query/fragment", name)
		}
		eps[name] = httpEndpoint{base: u}
		allow[allowlistKey(u)] = struct{}{}
	}
	maxBody := cfg.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = DefaultMaxHTTPBody
	}
	client := NewGuardedClient(cfg.Guard)
	inner := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		// host 门在前(策略先于传输):非白名 host 即使是合法 https 公网
		// 目标也拒;通过后再走守卫(剥凭证/https/跳数)。
		if err := checkHostAllowed(allow, req.URL); err != nil {
			return err
		}
		return inner(req, via)
	}
	return &HTTPTool{client: client, endpoints: eps, allow: allow, maxBody: maxBody}, nil
}

// Name 满足 Tool 接口(与 DSL tool 词表对齐:testdata http_tool.json)。
func (h *HTTPTool) Name() string { return "http" }

// Run 执行一次 HTTP 调用。错误语义(映射契约单点在 runner):携带
// ErrSSRFBlocked/ErrHostBlocked 的裸错误上抛(runner 分类);其余输入/
// 传输/协议错误 = 业务失败(tools: 前缀裸错误 → CodeToolFailed);ctx
// 取消由 client.Do 链回原样上抛(基础设施失败)。
//
// 硬契约(ssrf.go NewGuardedClient 注释):整体耗时由 ctx(attempt
// deadline)强制约束,本层不另设超时;响应体 io.LimitReader 封顶。
func (h *HTTPTool) Run(ctx context.Context, call Call) (Result, error) {
	name, err := requiredString(call.Inputs, "name")
	if err != nil {
		return Result{}, fmt.Errorf("tools: http: %w", err)
	}
	ep, ok := h.endpoints[name]
	if !ok {
		return Result{}, fmt.Errorf("tools: http: endpoint %q not configured", name)
	}
	method := http.MethodGet
	if raw, ok := call.Inputs["method"]; ok && raw != nil {
		s, ok := raw.(string)
		if !ok {
			return Result{}, fmt.Errorf("tools: http: input \"method\" must be string, got %T", raw)
		}
		method = strings.ToUpper(s)
		if _, ok := httpMethodWords[method]; !ok {
			return Result{}, fmt.Errorf("tools: http: unsupported method %q (want GET|POST|PUT|PATCH|DELETE)", s)
		}
	}
	path, err := requiredString(call.Inputs, "path")
	if err != nil {
		return Result{}, fmt.Errorf("tools: http: %w", err)
	}
	if err := validateToolPath(path); err != nil {
		return Result{}, err
	}
	u := *ep.base // 拷贝:请求特有部分(path/query)不回写端点
	// path 经 url.Parse 解码 %XX(直接赋值 u.Path 会把已转义路径二次转义)
	parsed, err := url.Parse(path)
	if err != nil {
		return Result{}, fmt.Errorf("tools: http: invalid path %q: %w", path, err)
	}
	u.Path, u.RawPath = parsed.Path, parsed.RawPath
	if q, ok := call.Inputs["query"]; ok && q != nil {
		qm, ok := q.(map[string]any)
		if !ok {
			return Result{}, fmt.Errorf("tools: http: input \"query\" must be object, got %T", q)
		}
		enc, err := encodeQuery(qm)
		if err != nil {
			return Result{}, err
		}
		u.RawQuery = enc
	}
	// 首请求复核:宿主按构造来自白名单,此处是防将来重构走样的带(3 行)。
	if err := checkHostAllowed(h.allow, &u); err != nil {
		return Result{}, err
	}
	var body []byte
	if raw, ok := call.Inputs["body"]; ok && raw != nil {
		if method == http.MethodGet {
			return Result{}, fmt.Errorf("tools: http: body with GET forbidden (use POST/PUT/PATCH/DELETE)")
		}
		body, err = json.Marshal(raw)
		if err != nil {
			return Result{}, fmt.Errorf("tools: http: marshal body: %w", err)
		}
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return Result{}, fmt.Errorf("tools: http: new request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if call.IdempotencyKey != "" { // AGENTS 铁律:外部调用幂等键传递面
		req.Header.Set("Idempotency-Key", call.IdempotencyKey)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return Result{}, err // url.Error 包装;哨兵经 %w 链可 errors.Is
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, h.maxBody+1))
	if err != nil {
		return Result{}, fmt.Errorf("tools: http: read response (status %d): %w", resp.StatusCode, err)
	}
	if int64(len(data)) > h.maxBody {
		return Result{}, fmt.Errorf("tools: http: response body exceeds %d bytes (status %d)", h.maxBody, resp.StatusCode)
	}
	out := map[string]any{"status": json.Number(strconv.Itoa(resp.StatusCode))}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 {
		// 任意合法 JSON(对象/数组/标量)→ body(UseNumber 数值保真,
		// 与 extractOutputs 同解码口径);非法 JSON → body_text(UTF-8
		// 规整:非 UTF-8 字节经序列化替换为 U+FFFD,与 file_read 契约
		// 同族——文本通道,二进制不属契约)。DSL 声明什么类型由 outputs
		// 契约约束,缺字段/类型不符在 extractOutputs 响亮失败——本层
		// 不做类型预判。
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.UseNumber()
		var parsedBody any
		if err := dec.Decode(&parsedBody); err == nil {
			var extra any
			if err := dec.Decode(&extra); errors.Is(err, io.EOF) { // 尾随垃圾 = 非 JSON
				out["body"] = parsedBody
			} else {
				out["body_text"] = string(trimmed)
			}
		} else {
			out["body_text"] = string(trimmed)
		}
	}
	raw, err := marshalToolOutput(out)
	if err != nil {
		return Result{}, fmt.Errorf("tools: http: %w", err)
	}
	return Result{Output: raw}, nil
}

// allowlistKey 归一化 URL 宿主为白名单比对键:小写 hostname(DNS 大小写
// 不敏感,IP 字面量原样)。端口不参与——白名单策略单位是宿主,端口级
// 管控归 P1 网络层 egress;https 默认 443 与显式 :443 由此自然等价。
func allowlistKey(u *url.URL) string {
	return strings.ToLower(u.Hostname())
}

// checkHostAllowed 校验 URL 宿主 ∈ 白名单,否则返回携带 ErrHostBlocked
// 的错误(诊断信息含 host:port 原文)。非 ASCII 宿主一律拒(IDNA 差分
// 防线,T3.5 审计 MEDIUM):门的归一(strings.ToLower 简单折叠)与拨号
// 面归一(stdlib UTS#46 ToASCII)是两套映射,存在折形差分码位(如
// U+0130 折 'i' 而 IDNA 映射不同);白名单是 ASCII 域,fail-closed——
// 非 ASCII 宿主无合法席位,不给差分留面。
func checkHostAllowed(allow map[string]struct{}, u *url.URL) error {
	h := u.Hostname()
	if !isASCIIHost(h) {
		return fmt.Errorf("%w: non-ascii host %q", ErrHostBlocked, h)
	}
	if _, ok := allow[strings.ToLower(h)]; !ok {
		return fmt.Errorf("%w: %q", ErrHostBlocked, u.Host)
	}
	return nil
}

// isASCIIHost 判定宿主是否纯 ASCII(字节级,无 UTF-8 多字节序列)。
func isASCIIHost(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// validateToolPath 校验 path 输入:绝对路径形态(以 / 开头且非 //——
// "//host" 会被 URL 解析为宿主,是 scheme 走私的同族变体)、无查询/锚点
// (查询参数走 query 输入,契约单通道)、无控制字节、长度上限。
func validateToolPath(path string) error {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return fmt.Errorf("tools: http: path must start with a single slash, got %q", path)
	}
	if strings.ContainsAny(path, "?#\x00\r\n") {
		return fmt.Errorf("tools: http: path must not contain ?, # or control bytes, got %q", path)
	}
	if len(path) > maxHTTPPathBytes {
		return fmt.Errorf("tools: http: path exceeds %d bytes", maxHTTPPathBytes)
	}
	return nil
}

// encodeQuery 把 query 输入对象编码为 URL 查询串。值 = 标量或标量数组
// (数组 → 同键重复参数);对象/null/嵌套数组响亮拒(T3.5 审计 MEDIUM:
// 递归下钻曾把 [[1,2],[3]] 静默展平成三个重复参数,违背「数组内非标量
// 响亮拒」——词表外的强制转换只会静默丢数据)。
func encodeQuery(q map[string]any) (string, error) {
	v := make(url.Values, len(q))
	for k, val := range q {
		if k == "" {
			return "", fmt.Errorf("tools: http: empty query key")
		}
		if arr, ok := val.([]any); ok {
			for _, el := range arr {
				s, err := queryScalar(k, el)
				if err != nil {
					return "", err
				}
				v[k] = append(v[k], s)
			}
			if len(v[k]) == 0 {
				return "", fmt.Errorf("tools: http: query %q: empty array", k)
			}
			continue
		}
		s, err := queryScalar(k, val)
		if err != nil {
			return "", err
		}
		v[k] = []string{s}
	}
	return v.Encode(), nil
}

// queryScalar 把标量值转为查询参数字符串;数组元素必须是标量(嵌套
// 数组在此响亮拒,不递归下钻)。
func queryScalar(key string, val any) (string, error) {
	switch t := val.(type) {
	case string:
		return t, nil
	case json.Number:
		return t.String(), nil
	case bool:
		return strconv.FormatBool(t), nil
	case float64: // 非 UseNumber 解码路径的兼容(契约形态是 json.Number)
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case int:
		return strconv.Itoa(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	default:
		return "", fmt.Errorf("tools: http: query %q: unsupported value type %T (want scalar or scalar array)", key, val)
	}
}

// requiredString 抽取必填字符串输入(缺失/类型不符响亮拒;错误不带包
// 前缀,调用方按工具名包装)。
func requiredString(inputs map[string]any, key string) (string, error) {
	raw, ok := inputs[key]
	if !ok || raw == nil {
		return "", fmt.Errorf("missing required input %q", key)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("input %q must be string, got %T", key, raw)
	}
	return s, nil
}
