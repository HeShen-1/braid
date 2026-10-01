package tools

// egress 默认拒绝代理(T10.2,issue #92;ARCHITECTURE §3.4 SSRF 三层修的
// 网络层半边)。worker 进程内 CONNECT-only 代理,绑沙箱 egress 内网的
// 网关地址——http 沙箱容器(--network <内网>)唯一可达面即本代理,直连
// 在网络层结构性拒绝(默认拒绝由拓扑保证,非策略保证)。
//
// 策略面(GATE 1 拍板):
//   - host(:port) 白名单 = EgressAllowFromEndpoints 从 BRAID_HTTP_ENDPOINTS
//     单源派生(基线 §4c 防双真源);端口级管控在此回收(http.go
//     allowlistKey「端口不参与/端口级管控归 P1 网络层 egress」注记)。
//   - DNS 全在代理侧解析(容器零解析);全部解析 IP 须过 ipForbidden,
//     任一落禁段整体拒(fail-closed:单挑放行 IP 拨号 = 把「拨哪个」的
//     裁决权留给解析序);拨已解析 IP,无二次解析(DNS rebinding TOCTOU
//     在代理侧结构性消失)。
//   - 拒答经状态行 reason-phrase 标记(egressReason* 常量)跨隧道边界;
//     helper 侧 egressDenialError 按精确 (code, reason) 配对映射回真哨兵
//     (ErrHostBlocked/ErrSSRFBlocked),宿主 runner 既有 errors.Is 分类
//     零改动(D-t101 #6 失败分类跨边界保真同哲学)。
//   - CONNECT-only:明文 http 绝不经此出网(绝对形请求 405);端到端
//     TLS 在 helper 与目标之间于隧道之上握手,代理零明文可见。
//
// 资源面:并发隧道上界 egressMaxTunnels + 在场连接上界 2×egressMaxTunnels
// (被攻破容器可不经 helper 直开原始连接/隧道,worker fd 耗尽面须有界——
// 含隧道前的握手窗);每隧道硬期限 egressTunnelDeadline 兜底(helper ctx
// 取消关闭其半侧,此为双保险)。

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// 拒答 reason-phrase 标记:helper 侧映射的唯一契约词(改动即两侧同步,
// TestEgressProxyPolicy 的同表断言是回归锚)。状态码语义:403 = 策略拒
// (哨兵两分桶)、502 = 传输失败(非哨兵,宿主侧落 tool_failed)、
// 503 = 过载、405/400 = 协议形态错。
const (
	egressReasonHostBlocked   = "Braid Egress Host Blocked"   // → ErrHostBlocked
	egressReasonSSRFBlocked   = "Braid Egress SSRF Blocked"   // → ErrSSRFBlocked
	egressReasonResolveFailed = "Braid Egress Resolve Failed" // → 非 sentinel
	egressReasonDialFailed    = "Braid Egress Dial Failed"    // → 非 sentinel
	egressReasonOverloaded    = "Braid Egress Overloaded"     // → 非 sentinel
	egressReasonConnectOnly   = "Braid Egress Connect Only"   // → 非 sentinel
	egressReasonBadRequest    = "Braid Egress Bad Request"    // → 非 sentinel
	egressReasonEstablished   = "Connection Established"      // 200 成功行
)

// 测试可收缩的量(生产值固定;runner/http 同款 var 纪律)。
var (
	// egressReadHeaderTimeout:单连接 CONNECT 请求头读取时限——慢速攻击
	// (建连不发言)不得长期占隧道名额。
	egressReadHeaderTimeout = 10 * time.Second
	// egressDialTimeout:代理侧解析+拨号的目标半段预算(DefaultTransport
	// 拨号基线 30s 同款;整体耗时仍由 helper 侧 attempt deadline 约束)。
	egressDialTimeout = 30 * time.Second
	// egressTunnelDeadline:单隧道硬期限(helper ctx 取消是其半侧的正常
	// 关闭路径;此为挂死兜底,正常调用远早于此结束)。
	egressTunnelDeadline = 30 * time.Minute
	// egressMaxTunnels:并发隧道上界(fd 耗尽面;helper 侧沙箱并发闸
	// (BRAID_TOOL_SANDBOX_MAX_CONCURRENCY)约束正常负载,此界防绕过
	// helper 的原始隧道滥用)。
	egressMaxTunnels = 128
	// egressMaxRequestBytes:CONNECT 请求(请求行+头)读取上界;超限按
	// 畸形拒(400 后断连)。
	egressMaxRequestBytes = 16 << 10
)

// EgressConfig 是代理的构造配置(不可变值对象)。Allow 为 nil/空 = 全拒
// (合法姿态:无 endpoints 即无合法目的地)。Resolver/IPForbidden 仅测试
// 注入(GuardConfig 同款纪律:非 nil 整体替换默认,生产恒零值——Allow
// 是唯一生产字段)。
//
// AllowInternal(T10.4,D-2026-09-30-t104-dbquery-egress):db_query DSN
// 派生的**钉死内部条目**——运维显式授权的内部 DB 位置(host:port 精确
// 匹配),豁免 ipForbidden 的私网/环回/CGNAT 禁段(ipForbiddenPinned:
// link-local/metadata/未指定/组播仍拒,豁免面最小化)。Allow 条目的
// 公网-only 姿态逐字节不变;残余风险(DSN 宿主名 DNS 重绑定可在私网内
// 枢转)登记于 D- 条目,威胁前提 = 攻击者控制运维所选宿主的 DNS。
type EgressConfig struct {
	Allow         map[string]map[int]struct{} // 小写 host(IP 字面量原样) → 端口集
	AllowInternal map[string]map[int]struct{} // 同上;钉死条目,豁免私网禁段(db_query only)
	Resolver      *net.Resolver               // nil → net.DefaultResolver
	IPForbidden   func(net.IP) bool           // nil → ipForbidden
}

// EgressProxy 是默认拒绝 CONNECT 代理。零内部 goroutine——生命周期跟随
// Serve 的 listener(listener 关闭即退出)。
type EgressProxy struct {
	cfg           EgressConfig
	activeTunnels int64 // 已建隧道计数(隧道闸;泄漏计数 = 容量闸永久 503)
	activeConns   int64 // 在场连接计数(隧道前握手窗闸,orch-review advisory:
	// 隧道闸只覆盖已建隧道,建连不发 CONNECT 的慢速洪泛曾可无界占
	// fd——违背本文件头「fd 耗尽面须有界」承诺,故连接级先行有界)
}

// NewEgressProxy 构造代理(纯配置惰性:绑定/监听在调用方,worker 装配
// 见 SandboxEgressStartup——cmd/worker 启动段接线)。
func NewEgressProxy(cfg EgressConfig) *EgressProxy {
	if cfg.Allow == nil {
		cfg.Allow = map[string]map[int]struct{}{}
	}
	if cfg.Resolver == nil {
		cfg.Resolver = net.DefaultResolver
	}
	if cfg.IPForbidden == nil {
		cfg.IPForbidden = ipForbidden
	}
	return &EgressProxy{cfg: cfg}
}

// Serve 接受循环(listener 关闭即返回其错误;单连接失败不断循环——
// 恶意连接不得耗尽代理可用性)。
func (p *EgressProxy) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer func() { _ = conn.Close() }()
			p.handleConn(conn)
		}()
	}
}

// handleConn 单连接会话:有界读 CONNECT 请求 → 策略判定 → 拒答(写状态
// 行后断)或建隧道(200 + 双向泵至任侧关闭)。请求头阶段经 LimitReader
// 封顶(头部洪泛在 ReadRequest 内截断为解析错 → 400);隧道期换
// Peek+MultiReader 续流——LimitReader 不得带进泵(对抗审计 BLOCKER:
// 带进 = client→target 方向被钉死 16 KiB,大 POST 必断)。
func (p *EgressProxy) handleConn(conn net.Conn) {
	// 连接级闸(先于隧道闸、先于一切读):在场连接上界 = 2×隧道上界
	// (隧道 + 握手窗共享预算)。建连不发言的洪泛在最坏头读时限内只能
	// 占满本闸,不再无界耗 fd。
	if atomic.AddInt64(&p.activeConns, 1) > int64(2*egressMaxTunnels) {
		atomic.AddInt64(&p.activeConns, -1)
		p.deny(conn, 503, egressReasonOverloaded)
		return
	}
	defer atomic.AddInt64(&p.activeConns, -1)

	_ = conn.SetReadDeadline(time.Now().Add(egressReadHeaderTimeout))
	br := bufio.NewReader(io.LimitReader(conn, int64(egressMaxRequestBytes)))
	req, err := http.ReadRequest(br)
	if err != nil {
		p.deny(conn, 400, egressReasonBadRequest)
		return
	}
	if req.Method != http.MethodConnect {
		p.deny(conn, 405, egressReasonConnectOnly)
		return
	}
	// CONNECT authority-form → req.URL.Host(ReadRequest 源码口径:
	// justAuthority 前缀 "http://" 再剥 Scheme;百分号编码不解码——
	// 编码词形 ≠ 白名词形,天然 fail-closed)。
	authority := req.URL.Host
	if authority == "" {
		// net/rpc 形态(authority 以 / 开头落 Path)或空:非隧道语义。
		p.deny(conn, 400, egressReasonBadRequest)
		return
	}
	host, portStr, err := net.SplitHostPort(authority)
	if err != nil {
		p.deny(conn, 400, egressReasonBadRequest)
		return
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 || host == "" || len(host) > 253 {
		p.deny(conn, 400, egressReasonBadRequest)
		return
	}

	// 策略门一:host(:port) 白名单(默认拒绝——不在集内即拒,零例外)。
	// AllowInternal 是 db_query DSN 派生的钉死条目(T10.4):命中即豁免
	// 私网禁段(ipForbiddenPinned),Allow 条目照走 ipForbidden 全禁段。
	// 同宿主双集(https 端点与 DSN 同宿主)按端口组合解析(orch-review
	// LOW 收口):Allow 管自己的端口,钉死条目管 DSN 端口——豁免语义
	// 只随钉死端口走,Allow 侧端口维持全禁段。
	ports, pinned := lookupEgressPorts(p.cfg, host, port)
	if ports == nil {
		p.deny(conn, 403, egressReasonHostBlocked)
		return
	}

	// 容量闸:占名额在建隧道之后(策略拒不占名额——攻击者的 403 洪
	// 不消耗真实隧道容量)。
	if atomic.AddInt64(&p.activeTunnels, 1) > int64(egressMaxTunnels) {
		atomic.AddInt64(&p.activeTunnels, -1)
		p.deny(conn, 503, egressReasonOverloaded)
		return
	}
	defer atomic.AddInt64(&p.activeTunnels, -1)

	// 策略门二:目标 IP。IP 字面量零 DNS 直查;域名全量解析、全 IP 过
	// 禁段(fail-closed),拨已解析 IP(钉死,无二次解析)。钉死条目换用
	// ipForbiddenPinned(私网/环回/CGNAT 豁免);测试注入的 IPForbidden
	// 只辖 Allow 条目——钉死语义不随注入缝漂移。
	forbidden := p.cfg.IPForbidden
	if pinned {
		forbidden = ipForbiddenPinned
	}
	rctx, cancel := context.WithTimeout(context.Background(), egressDialTimeout)
	defer cancel()
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		resolved, err := p.cfg.Resolver.LookupIP(rctx, "ip", host)
		if err != nil || len(resolved) == 0 {
			// 解析失败 fail-closed 拒(哨兵两分桶都不是:基础设施性失败,
			// 宿主侧落 tool_failed)。
			p.deny(conn, 502, egressReasonResolveFailed)
			return
		}
		ips = resolved
	}
	var target net.Conn
	for _, ip := range ips {
		if forbidden(ip) {
			p.deny(conn, 403, egressReasonSSRFBlocked)
			return
		}
	}
	for _, ip := range ips {
		d := &net.Dialer{Timeout: egressDialTimeout}
		c, err := d.DialContext(rctx, "tcp", net.JoinHostPort(ip.String(), portStr))
		if err == nil {
			target = c
			break
		}
	}
	if target == nil {
		p.deny(conn, 502, egressReasonDialFailed)
		return
	}
	defer func() { _ = target.Close() }()

	// 建隧道:200 状态行(无响应头——CONNECT 应答的既定形态),清读
	// 期限,双向泵。硬期限兜底(timer 触发双断;正常路径任侧关闭即止)。
	// 泵的 client 侧源 = br 已缓冲残余(CONNECT 后立即发送的隧道首字节,
	// Peek 不丢)+ 原始 conn——LimitReader 止步于请求头阶段,不带进泵。
	_ = conn.SetReadDeadline(time.Time{})
	if _, err := conn.Write([]byte("HTTP/1.1 200 " + egressReasonEstablished + "\r\n\r\n")); err != nil {
		return
	}
	var src io.Reader = conn
	if n := br.Buffered(); n > 0 {
		buffered, _ := br.Peek(n)
		src = io.MultiReader(bytes.NewReader(buffered), conn)
	}
	p.pump(src, conn, target)
}

// pump 双向字节泵:client→target 从 src 读(可能含请求头阶段已缓冲的
// 隧道首字节),target→client 直写 conn。任方向结束即整体收场(defer
// 关闭两侧)。
func (p *EgressProxy) pump(src io.Reader, client, target net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(target, src)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, target)
		done <- struct{}{}
	}()
	timer := time.AfterFunc(egressTunnelDeadline, func() {
		_ = client.Close()
		_ = target.Close()
	})
	defer timer.Stop()
	<-done
	_ = client.Close() // 先到者触发对向 Copy 的 EOF,加速第二方向收场
	<-done
}

// deny 写单行拒答后断连(Content-Length: 0 防客户端等 body)。
func (p *EgressProxy) deny(conn net.Conn, code int, reason string) {
	_, _ = io.WriteString(conn, fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", code, reason))
}

// egressDenialError 把代理拒答的 (code, reason) 精确配对映射为哨兵错误
// (helper 侧 DialTLSContext 消费;映射只认常量配对——未知组合返回 nil,
// 由调用方按普通传输错误处理,防新增 reason 静默错配)。403 两标记外的一切
// 拒答形态对 helper 都是普通错误(宿主侧 tool_failed)。
func egressDenialError(code int, reason string) error {
	switch {
	case code == 403 && reason == egressReasonHostBlocked:
		return fmt.Errorf("%w: egress proxy denied %d %s", ErrHostBlocked, code, reason)
	case code == 403 && reason == egressReasonSSRFBlocked:
		return fmt.Errorf("%w: egress proxy denied %d %s", ErrSSRFBlocked, code, reason)
	case code == 502 && reason == egressReasonResolveFailed,
		code == 502 && reason == egressReasonDialFailed,
		code == 503 && reason == egressReasonOverloaded,
		code == 405 && reason == egressReasonConnectOnly,
		code == 400 && reason == egressReasonBadRequest:
		return fmt.Errorf("egress proxy denied: %d %s", code, reason)
	default:
		return nil
	}
}

// parseEgressProxyURL 解析并校验 egress 代理地址词形:http://host:port,
// 端口必显式、零 userinfo/path/query(代理是明文 http 内网单跳;https 或
// socks 非本设计)。env 键(BRAID_TOOL_SANDBOX_EGRESS_ADDR)与信封
// 字段("proxy")共用此单门——两侧词形口径漂移在此结构性不可能。
func parseEgressProxyURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("tools: egress proxy address %q: %w", raw, err)
	}
	if u.Scheme != "http" || u.Hostname() == "" || u.Port() == "" ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("tools: egress proxy address %q: want http://host:port", raw)
	}
	return u, nil
}

// EgressAllowFromEndpoints 从 BRAID_HTTP_ENDPOINTS 原料单源派生代理白
// 名单(host → 端口集):https 缺省 443、显式端口原样;host 归一 = 小写
// (IP 字面量原样),与 http.go allowlistKey 同判。坏值响亮败(上游
// NewHTTPTool 已同门校验,此处二道防御——两道校验口径漂移即红)。
func EgressAllowFromEndpoints(endpoints map[string]string) (map[string]map[int]struct{}, error) {
	allow := make(map[string]map[int]struct{}, len(endpoints))
	for name, raw := range endpoints {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("tools: egress allowlist: endpoint %q: parse: %w", name, err)
		}
		if u.Scheme != "https" || u.Hostname() == "" || (u.EscapedPath() != "" && u.EscapedPath() != "/") {
			return nil, fmt.Errorf("tools: egress allowlist: endpoint %q: want https base url with host and no path, got %q", name, raw)
		}
		port := 443
		if p := u.Port(); p != "" {
			v, err := strconv.Atoi(p)
			if err != nil || v < 1 || v > 65535 {
				return nil, fmt.Errorf("tools: egress allowlist: endpoint %q: port %q invalid", name, p)
			}
			port = v
		}
		host := strings.ToLower(u.Hostname())
		if allow[host] == nil {
			allow[host] = map[int]struct{}{}
		}
		allow[host][port] = struct{}{}
	}
	return allow, nil
}

// lookupEgressPorts 按 (host, port) 解析允许集:Allow 命中且含端口 →
// 常规条目;否则 AllowInternal 含该端口 → 钉死条目;两者皆不含 → nil
// (默认拒绝)。返回值二 = 是否钉死(豁免私网禁段的语义位)。
func lookupEgressPorts(cfg EgressConfig, host string, port int) (map[int]struct{}, bool) {
	if ports, ok := cfg.Allow[host]; ok {
		if _, pok := ports[port]; pok {
			return ports, false
		}
	}
	if ports, ok := cfg.AllowInternal[host]; ok {
		if _, pok := ports[port]; pok {
			return ports, true
		}
	}
	return nil, false
}

// ipForbiddenPinned 钉死条目(db_query DSN 派生)的禁段口径:私网/环回/
// CGNAT 豁免(运维显式授权的内部位置),其余禁段(link-local 含 metadata、
// 未指定/组播/广播)仍拒;公网放行(与 ipForbidden 对公网的判定一致)。
// 见 EgressConfig.AllowInternal 注释与 D-2026-09-30-t104-dbquery-egress。
func ipForbiddenPinned(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsPrivate() || ip.IsLoopback() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil && cgnat.Contains(ip4) {
		return false
	}
	return ipForbidden(ip)
}

// EgressAllowFromDSN 从 BRAID_DBQUERY_DSN 派生钉死条目(小写 host → 端口;
// 与工具构造共用 dbqDSNConfig/dbqDSNHostPort 单源,口径漂移在启动即红)。
func EgressAllowFromDSN(dsn string) (string, int, error) {
	cfg, err := dbqDSNConfig(dsn)
	if err != nil {
		return "", 0, fmt.Errorf("tools: egress pinned entry: %w", err)
	}
	host, port, err := dbqDSNHostPort(cfg.Addr)
	if err != nil {
		return "", 0, fmt.Errorf("tools: egress pinned entry: %w", err)
	}
	return strings.ToLower(host), port, nil
}

// EgressAllowFromMCPServers 从 MCP server 声明单源派生代理白名单条目
// (T13.2,D-t132)。**常规 Allow,非 AllowInternal**——MCP server 须公网
// https 端点,不豁免私网禁段(内部 MCP server 属未来 D- 议题;off 模式
// GuardConfig 拨号复核同判,两模式口径一致)。与 EgressAllowFromEndpoints
// 的差异仅在路径:MCP 端点允许路径前缀(mcpclient.go mcpServerURL 同门),
// host:port 派生语义(小写 host、https 缺省 443、显式端口原样)同款;
// 多工具共宿/共 server 的端口集自然并集。坏值响亮败(上游 NewMCPTools
// 已同门校验,此处二道防御——两道校验口径漂移即红)。
func EgressAllowFromMCPServers(servers map[string]string) (map[string]map[int]struct{}, error) {
	allow := make(map[string]map[int]struct{}, len(servers))
	for alias, raw := range servers {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("tools: egress allowlist: mcp server %q: parse: %w", alias, err)
		}
		if u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("tools: egress allowlist: mcp server %q: want https base url with host and no userinfo/query/fragment (path prefix allowed), got %q", alias, mcpRedactURL(u, raw))
		}
		port := 443
		if p := u.Port(); p != "" {
			v, perr := strconv.Atoi(p)
			if perr != nil || v < 1 || v > 65535 {
				return nil, fmt.Errorf("tools: egress allowlist: mcp server %q: port %q invalid", alias, p)
			}
			port = v
		}
		host := strings.ToLower(u.Hostname())
		if allow[host] == nil {
			allow[host] = map[int]struct{}{}
		}
		allow[host][port] = struct{}{}
	}
	return allow, nil
}

// mergeEgressAllow 把派生条目并进 allow(host 维度并集、端口集合并;
// 多个声明源(endpoints/MCP servers)汇入同一 allow 集的收口点)。
func mergeEgressAllow(allow, extra map[string]map[int]struct{}) {
	for host, ports := range extra {
		if allow[host] == nil {
			allow[host] = map[int]struct{}{}
		}
		for p := range ports {
			allow[host][p] = struct{}{}
		}
	}
}

// egressConnectTunnel 拨代理并完成 CONNECT 握手(明文/ TLS 两族共享段,
// T10.4 提取):写 CONNECT → 读状态行 → 拒答按 egressDenialError 映射 →
// 2xx 消化剩余响应头。返回已过 2xx 的连接与其 bufio 残余(隧道期续读,
// 不丢已缓冲字节);**调用方接管连接所有权**(返回即交接,无 defer 关闭)。
// CONNECT 段共用 egressDialTimeout 绝对 deadline(拨号段唯一执法点:
// 上游 ctx 对自定义拨号器是 WithoutCancel,attempt deadline 不达此段)。
func egressConnectTunnel(ctx context.Context, proxy *url.URL, addr string) (net.Conn, *bufio.Reader, error) {
	d := &net.Dialer{Timeout: egressDialTimeout}
	raw, err := d.DialContext(ctx, "tcp", net.JoinHostPort(proxy.Hostname(), proxy.Port()))
	if err != nil {
		return nil, nil, fmt.Errorf("egress proxy dial %s: %w", proxy.Host, err)
	}
	// 失败路径统一收场:established 只在「所有权随返回值移交」时置位;
	// 其余一切出口(含作用域遮蔽 err 的路径)都由 defer 关闭 raw。
	established := false
	defer func() {
		if !established {
			_ = raw.Close()
		}
	}()
	deadline := time.Now().Add(egressDialTimeout)
	_ = raw.SetDeadline(deadline)
	if _, err := fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr); err != nil {
		return nil, nil, fmt.Errorf("egress proxy write CONNECT: %w", err)
	}
	br := bufio.NewReader(raw)
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, nil, fmt.Errorf("egress proxy read CONNECT response: %w", err)
	}
	if len(line) > egressMaxRequestBytes {
		return nil, nil, fmt.Errorf("egress proxy response line exceeds %d bytes", egressMaxRequestBytes)
	}
	fields := strings.Fields(strings.TrimRight(line, "\r\n"))
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
		return nil, nil, fmt.Errorf("egress proxy malformed response %q", strings.TrimSpace(line))
	}
	code, cerr := strconv.Atoi(fields[1])
	if cerr != nil {
		return nil, nil, fmt.Errorf("egress proxy malformed status %q: %w", strings.TrimSpace(line), cerr)
	}
	reason := strings.Join(fields[2:], " ")
	if code < 200 || code > 299 {
		// 拒答映射:精确 (code, reason) 配对 → 哨兵或普通错误;
		// 未知组合按普通传输错误(宿主侧 tool_failed),绝不静默。
		if derr := egressDenialError(code, reason); derr != nil {
			return nil, nil, derr
		}
		return nil, nil, fmt.Errorf("egress proxy denied: unexpected %q", strings.TrimSpace(line))
	}
	// 2xx:消化剩余响应头至空行(隧道期从 br 续读,不丢已缓冲字节)。
	for {
		hl, herr := br.ReadString('\n')
		if herr != nil {
			return nil, nil, fmt.Errorf("egress proxy read CONNECT headers: %w", herr)
		}
		if strings.TrimSpace(hl) == "" {
			break
		}
	}
	established = true
	return raw, br, nil
}

// egressTunnelDialPlain 构造 helper 侧的**明文**隧道拨号器(T10.4,
// db_query 的 MySQL DialFunc 消费):CONNECT 隧道之上直接承载 MySQL 协议
// 字节——协议选择权在两端(MySQL 原生挑战响应认证;链路加密留运营方
// DSN tls= 参数,代理与 worker 同进程同信任域)。拒答映射/残余续读与
// TLS 族同源(egressConnectTunnel)。
func egressTunnelDialPlain(proxy *url.URL) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		raw, br, err := egressConnectTunnel(ctx, proxy, addr)
		if err != nil {
			return nil, err
		}
		_ = raw.SetDeadline(time.Time{})
		return &egressTunnelConn{Conn: raw, r: br}, nil
	}
}

// egressTunnelDialContext 构造 helper 侧的 https 隧道拨号器(T10.2,
// NewGuardedClient 消费):Transport.Proxy 恒 nil、DialTLSContext 指向本
// 函数——它是 https 请求的唯一拨号路径(Go 1.27 transport.go 实证:
// Proxy nil + hasCustomTLSDialer → customDialTLS(ctx, "tcp", cm.addr()) 恰
// 收目标地址,错误不经 net.OpError 包装原样穿透)。流程:egressConnectTunnel
// 共享段 → 隧道之上端到端 TLS(系统根或注入根;代理零明文可见),返回
// 已完成握手的 *tls.Conn(Transport 对 handshaker 幂等)。
//
// 重定向每跳新目标 = 新拨号 = 每跳过代理策略门(容器内 host 门是第一道,
// 代理是网络层第二道)。
func egressTunnelDialContext(proxy *url.URL, tlsCfg *tls.Config) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		raw, br, err := egressConnectTunnel(ctx, proxy, addr)
		if err != nil {
			return nil, err
		}
		// 失败路径收场:handoff 只在「raw 的所有权移交给返回的 tls.Conn」
		// 时置位;TLS 握手失败等一切出口都由 defer 关闭 raw。
		handoff := false
		defer func() {
			if !handoff {
				_ = raw.Close()
			}
		}()
		host, _, serr := net.SplitHostPort(addr)
		if serr != nil {
			return nil, fmt.Errorf("egress tunnel: bad target addr %q: %w", addr, serr)
		}
		tconn := tls.Client(&egressTunnelConn{Conn: raw, r: br}, egressTunnelTLSConfig(tlsCfg, host))
		if herr := tconn.HandshakeContext(ctx); herr != nil {
			return nil, fmt.Errorf("egress tunnel TLS handshake %s: %w", host, herr)
		}
		_ = raw.SetDeadline(time.Time{})
		handoff = true
		return tconn, nil
	}
}

// egressTunnelConn 隧道期连接:Read 走 bufio(状态行/头之后已缓冲的字节
// 不得丢),其余直通底层 conn(Write/Close/deadline)。
type egressTunnelConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *egressTunnelConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// egressTunnelTLSConfig 构造隧道上端到端 TLS 的配置:Clone 注入根(或
// 系统根)、ServerName 取目标 host、ALPN 钉死 http/1.1——「恒 HTTP/1.1」
// 从配置缺席的巧合升级为拨号器结构(任何来源的 NextProtos 混入 h2 都
// 不可能协商出「h2 连接上跑 Transport 的 HTTP/1.1 persistConn」的协议
// 错乱;Transport 对 custom dialer 不建 h2Transport。设计对抗审查
// advisory 收口,TestEgressTunnelALPNPin 是配置锚)。
func egressTunnelTLSConfig(tlsCfg *tls.Config, host string) *tls.Config {
	tlsConf := tlsCfg.Clone()
	if tlsConf == nil {
		tlsConf = &tls.Config{}
	}
	if tlsConf.ServerName == "" {
		tlsConf.ServerName = host
	}
	tlsConf.NextProtos = []string{"http/1.1"}
	return tlsConf
}

// EgressStartup 是 SandboxEgressStartup 的结果:代理地址 + 实例段网名。
// 调用方写 BRAID_TOOL_SANDBOX_EGRESS_ADDR 与 BRAID_TOOL_SANDBOX_EGRESS_NETWORK
// (实例名覆写基段)后再走 BuiltinToolsFromEnv——容器 --network 位、信封
// proxy 字段与代理 allow 集由此三面同源(ADDR handoff 同款纪律,worker
// 进程内传递,非手配键)。
type EgressStartup struct {
	ProxyURL *url.URL
	Network  string
}

// SandboxEgressStartup 是 worker 启动的 egress 段(T10.2,SandboxStartupCheck
// 同族):docker 模式下幂等确保 --internal egress 内网 → 读网关 IP → 以
// resolveToolSet 单源派生 allow 集(签名工具包/env 矩阵同过,基线 §4c)
// → 绑网关:port 起 CONNECT 代理并 serve → 返回代理地址与实例段网名
// (调用方 os.Setenv BRAID_TOOL_SANDBOX_EGRESS_ADDR / EGRESS_NETWORK 后再走
// BuiltinToolsFromEnv 装配,信封 proxy 字段与代理 allow 集由此同源)。
// off 模式零 docker 接触,返回 nil。零静默降级:网络不可确保/非 internal
// 复用/网关不可读/端口不可绑 = 响亮败(worker 拒启,绝不回落 bridge)。
//
// 多副本口径(D-t151 决策 5,t15.4):网名带实例段(egressNetworkInstance,
// 一 worker 一网)⇒ 独立 bridge 子网独立网关 IP,同端口 9210 不撞,顺带
// worker 间沙箱容器物理网络隔离。同 worker_id 二启 = 幂等复用同一实例网
// + 同网关同端口 bind 拒启(**显式端口**下 owner 唯一部署契约的执法形态;
// PORT=0 内核指派 = dev/test 语义,不执法——同族登记边界;活体同 owner
// 冲突面见 D-t151 决策 8-③);
// 地址池上界与 recreate 泄漏网回收 = runbook 面(deploy/RUNBOOK.md)。
func SandboxEgressStartup(ctx context.Context, getenv func(string) string, workerID string) (*EgressStartup, error) {
	cfg, err := sandboxConfigFromEnv(getenv)
	if err != nil {
		return nil, err
	}
	if !cfg.Docker {
		return nil, nil
	}

	// allow 集单源:与装配同门解析(env 或验签清单),漂移在启动即红。
	// 钉死条目(T10.4):db_query DSN 派生 host:port 进 AllowInternal——
	// 运维显式授权的内部 DB 位置豁免私网禁段(D-2026-09-30-t104
	// -dbquery-egress);DSN 缺席/坏值在启动即红(凭证通道与装配同源)。
	ts, err := resolveToolSet(getenv)
	if err != nil {
		return nil, err
	}
	allow, err := EgressAllowFromEndpoints(ts.httpEndpoints)
	if err != nil {
		return nil, err
	}
	// MCP server 条目(T13.2):常规 Allow(host:port 单源派生;toolSet
	// 半配置拒保证全量 server 被引用)。
	if len(ts.mcpServers) > 0 {
		mcpAllow, err := EgressAllowFromMCPServers(ts.mcpServers)
		if err != nil {
			return nil, err
		}
		mergeEgressAllow(allow, mcpAllow)
	}
	var pinned map[string]map[int]struct{}
	if ts.dbDSN != "" {
		h, p, derr := EgressAllowFromDSN(ts.dbDSN)
		if derr != nil {
			return nil, derr
		}
		pinned = map[string]map[int]struct{}{h: {p: {}}}
	}

	// 端口键独立解析(仅启动段消费,不进 sandboxConfig):空 = 缺省
	// 9210;0 = 内核指派(dev/测试避碰撞——addr 经返回值+setenv 全链
	// 传递,零硬编码消费面)。校验先于 docker 接触(坏值快败,零副作用)。
	port := sandboxEgressPortDefault
	if raw := strings.TrimSpace(getenv("BRAID_TOOL_SANDBOX_EGRESS_PORT")); raw != "" {
		v, perr := strconv.Atoi(raw)
		if perr != nil || v < 0 || v > 65535 {
			return nil, fmt.Errorf("tools: BRAID_TOOL_SANDBOX_EGRESS_PORT: want integer in [0, 65535] (0 = kernel-assigned)")
		}
		port = v
	}

	// 实例段网名(一 worker 一网,D-t151 决策 5):坏输入(空身份/基段
	// 超长)在此快败——与端口键同窗口,先于 docker 接触(零副作用)。
	network, nerr := egressNetworkInstance(cfg.EgressNetwork, workerID)
	if nerr != nil {
		return nil, nerr
	}

	// daemon 语义下限:--internal 旗标自 17.06 存在,但「隔离转发」语义
	// (容器仅达网关、外发结构性拒绝)按 20.10 验证——低于即拒启(基线
	// §9-8 daemon 版本漂移残留的 egress 半边,SandboxProbe 的 17.06 下限
	// 只覆盖 --init/--mount)。
	vctx, cancel := context.WithTimeout(ctx, sandboxProbeTimeout)
	vout, verr := exec.CommandContext(vctx, sandboxDockerBin, "info", "--format", "{{.ServerVersion}}").Output()
	cancel()
	if verr != nil {
		return nil, fmt.Errorf("tools: egress startup: docker info: %w", verr)
	}
	major, minor, perr := parseDaemonVersion(strings.TrimSpace(string(vout)))
	if perr != nil {
		return nil, fmt.Errorf("tools: egress startup: daemon version %q unparsable: %v", strings.TrimSpace(string(vout)), perr)
	}
	if major < 20 || (major == 20 && minor < 10) {
		return nil, fmt.Errorf("tools: egress startup: docker daemon %d.%d below 20.10 minimum for --internal isolation semantics (default-deny egress)", major, minor)
	}

	gwIP, err := ensureEgressNetwork(ctx, sandboxDockerBin, network)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(gwIP, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("tools: egress proxy listen %s:%d: %w (per-worker network %s; concurrent same-owner start must fail loud)", gwIP, port, err, network)
	}
	proxy := NewEgressProxy(EgressConfig{Allow: allow, AllowInternal: pinned})
	go func() { _ = proxy.Serve(ln) }() // listener 关闭即退(worker 进程生命周期)
	return &EgressStartup{
		ProxyURL: &url.URL{Scheme: "http", Host: ln.Addr().String()},
		Network:  network,
	}, nil
}

// sandboxEgressPortDefault 代理监听缺省端口(deploy/README §egress 同源
// 登记;容器 → 网关唯一可达面,无公网暴露)。
const sandboxEgressPortDefault = 9210

// sandboxEgressNetworkBaseMax 基段长度上界:实例名 = 基段 + "-" + 8 hex,
// 须落 docker 网络名值域(≤64,sandboxNetworkRe)⇒ 基段 ≤ 55。
const sandboxEgressNetworkBaseMax = 55

// egressNetworkInstance 从基段与 worker 身份派生实例段网名(D-t151 决策
// 5):<base>-<sha256(workerID) 前 4 字节 hex>。同 worker_id 跨重启同名
// (幂等复用网 + owner 唯一部署契约的前提,算法稳定性由 golden 测试钉死);
// 异 worker_id 各持独立 --internal bridge——独立子网独立网关 IP ⇒ 同端口
// 9210 不撞,顺带 worker 间沙箱容器物理网络隔离。8 hex = 2^32 空间,同
// daemon 副本数下碰撞后果 = 同名网(显式端口下 bind 冲突拒启,响亮;
// PORT=0 内核指派 = dev/test 语义,双活不执法——登记边界,与同 owner
// 二启同判),静默串扰结构性不可达。网名最终落 docker argv,值域白名单
// (sandboxNetworkRe)在此复检——镜像引用同款纪律。
func egressNetworkInstance(base, workerID string) (string, error) {
	if workerID == "" {
		return "", fmt.Errorf("tools: egress network instance segment requires worker identity (empty worker id; set BRAID_WORKER_ID or hostname)")
	}
	if len(base) > sandboxEgressNetworkBaseMax {
		return "", fmt.Errorf("tools: BRAID_TOOL_SANDBOX_EGRESS_NETWORK=%q longer than %d chars (no room for per-worker instance segment)", base, sandboxEgressNetworkBaseMax)
	}
	sum := sha256.Sum256([]byte(workerID))
	name := fmt.Sprintf("%s-%x", base, sum[:4])
	if !sandboxNetworkRe.MatchString(name) {
		return "", fmt.Errorf("tools: egress network instance name %q invalid (docker network name value domain)", name)
	}
	return name, nil
}

// ensureEgressNetwork 幂等确保 egress 内网在场且为 --internal:inspect 缺
// 失 → create --internal(「已存在」竞态 = 复检消化);复用既有网络但
// Internal=false → 拒(非 internal 网络 = 默认拒绝被静默拆毁的形态)。
// 返回网关 IP(docker network inspect IPAM.Gateway)。
func ensureEgressNetwork(ctx context.Context, dockerBin, network string) (string, error) {
	dockerOutput := func(args ...string) (string, error) {
		cctx, cancel := context.WithTimeout(ctx, sandboxProbeTimeout)
		defer cancel()
		out, err := exec.CommandContext(cctx, dockerBin, args...).Output()
		return strings.TrimSpace(string(out)), err
	}
	internalRaw, ierr := dockerOutput("network", "inspect", "--format", "{{.Internal}}", network)
	if ierr != nil {
		// 缺失 → 创建;create 退出码携带不了「竞态下已存在」语义,
		// 存在性一律由复检裁决。
		_, _ = dockerOutput("network", "create", "--internal", network)
		internalRaw, ierr = dockerOutput("network", "inspect", "--format", "{{.Internal}}", network)
		if ierr != nil {
			return "", fmt.Errorf("tools: egress network %q unavailable after create attempt (inspect: %v)", network, ierr)
		}
	}
	if internalRaw != "true" {
		return "", fmt.Errorf("tools: egress network %q exists but not --internal (default-deny posture broken; remove the network or pick another name via BRAID_TOOL_SANDBOX_EGRESS_NETWORK)", network)
	}
	gw, err := dockerOutput("network", "inspect", "--format", "{{(index .IPAM.Config 0).Gateway}}", network)
	if err != nil || gw == "" {
		return "", fmt.Errorf("tools: egress network %q gateway unreadable: %v (%q)", network, err, gw)
	}
	if ip := net.ParseIP(gw); ip == nil {
		return "", fmt.Errorf("tools: egress network %q gateway %q not an IP", network, gw)
	}
	return gw, nil
}
