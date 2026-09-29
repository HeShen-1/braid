package tools

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// cgnat 是 RFC 6598 运营商级 NAT 段(100.64.0.0/10),stdlib 无对应
// 分类助手(IsPrivate 不含它),显式登记。
var cgnat = net.IPNet{IP: net.IP{100, 64, 0, 0}.To4(), Mask: net.CIDRMask(10, 32)}

// ErrSSRFBlocked 是 SSRF 防护拒连的哨兵错误:拨号复核(禁段 IP)、仅
// https(scheme 门)、重定向每跳重校验、越限重定向共用。映射契约:内置
// http 工具(T3.5)返回携带本哨兵的**裸错误**(不自行包 CallError),
// LocalRunner 以 errors.Is 映射到 CallError{Code: CodeSSRFBlocked}——
// 失败分类单点保持在 runner(见 runner.go Run 的映射分支)。
var ErrSSRFBlocked = errors.New("tools: ssrf blocked")

// maxRedirects 是重定向跳数上限。CheckRedirect 一旦被设置,http.Client
// 的 stdlib 默认上限(10)即失效,上限必须在此自管;越限错误携带
// ErrSSRFBlocked 哨兵(归 SSRF 拒连家族,审计口径见 CodeSSRFBlocked)。
const maxRedirects = 10

// ipForbidden 判定拨号目标 IP 是否落在禁段(ARCHITECTURE §3.4 SSRF 三层修
// 锁定口径):私网/环回/链路本地(169.254/16 内含 metadata 169.254.169.254)/
// CGNAT;另收未指定/组播/广播——永不合法的拨号目标,fail-closed。nil(上游
// 解析失败)同样拒。IPv4-mapped IPv6 由各分类助手归一处理。口径外的保留段
// (NAT64 64:ff9b::/96、198.18.0.0/15 等)登记在 docs/DECISIONS.md 待议,
// 扩禁段须走 D-entry 流程,不在此顺手改。
func ipForbidden(ip net.IP) bool {
	if ip == nil {
		return true
	}
	switch {
	case ip.IsUnspecified(),
		ip.IsLoopback(),
		ip.IsPrivate(),
		ip.IsLinkLocalUnicast(),
		ip.IsMulticast():
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		if ip4.Equal(net.IPv4bcast) || cgnat.Contains(ip4) {
			return true
		}
	}
	return false
}

// GuardConfig 是受防 http.Client 的可测缝集合,零值即生产装配:默认解析
// 器 + 默认禁段 + 系统 TLS 根。
type GuardConfig struct {
	Resolver    *net.Resolver     // nil → net.DefaultResolver;测试注入假解析器(可控 DNS)
	IPForbidden func(net.IP) bool // 仅测试注入:nil → ipForbidden。非 nil 即**整体替换**默认禁段(含放行环回/metadata),生产装配误带此缝将静默放行全部禁段且无报错——恒零值
	TLS         *tls.Config       // 测试/T3.5 注入私有根证书;不放宽校验
}

// NewGuardedClient 构造 http 工具专用 http.Client(ARCHITECTURE §3.4
// SSRF 三层修的运行时半边)。防三条,纵深即注释:
//   - 仅 https:httpsOnlyTransport 在每一跳(直连首跳与每跳重定向)拒绝
//     非 https URL——不依赖远端行为;
//   - 拨号复核:net.Dialer.Control 在 connect 之前核对**实际拨打的 IP**,
//     DNS rebinding 的 TOCTOU 窗口消失(检查即拨号,无第二次解析);
//   - CheckRedirect:每跳剥 Authorization/Cookie 并清 URL userinfo(见
//     CheckRedirect 注释),并自管跳数上限。
//
// 边界归属(供 T3.5 装配对齐):
//   - Proxy 恒 nil(不拾取 HTTP(S)_PROXY——经代理的请求绕过本地拨号
//     复核,env 代理是默认可用的 SSRF 旁路);egress 默认拒绝代理的网络层
//     方案属 P1。
//   - 超时:本层只带 DefaultTransport 基线旋钮(拨号/TLS 握手/空闲连接),
//     整体耗时由 **request ctx 强制带 attempt deadline** 约束——硬契约。
//   - 响应体限额归工具实现:本层不读 body,T3.5 须 io.LimitReader + 声明
//     上限(attempt 内公网端点可灌任意字节)。
//   - 恒 HTTP/1.1:自定义 Dialer 下不设 ForceAttemptHTTP2(不升级 h2);
//     单次工具调用连接复用收益有限,建连拨号仍必经 Control。
func NewGuardedClient(cfg GuardConfig) *http.Client {
	forbidden := cfg.IPForbidden
	if forbidden == nil {
		forbidden = ipForbidden
	}
	dialer := &net.Dialer{
		Resolver: cfg.Resolver,
		Timeout:  30 * time.Second, // DefaultTransport 基线
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("%w: bad dial address %q: %w", ErrSSRFBlocked, address, err)
			}
			ip := net.ParseIP(host)
			if ip == nil || forbidden(ip) {
				return fmt.Errorf("%w: dial %s %s forbidden", ErrSSRFBlocked, network, host)
			}
			return nil
		},
	}
	tr := &http.Transport{
		DialContext:         dialer.DialContext,
		TLSClientConfig:     cfg.TLS,
		TLSHandshakeTimeout: 10 * time.Second, // DefaultTransport 基线
		IdleConnTimeout:     90 * time.Second, // 同上
		MaxIdleConns:        100,              // 同上
	}
	return &http.Client{
		Transport: &httpsOnlyTransport{rt: tr},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("tools: stopped after %d redirects: %w", maxRedirects, ErrSSRFBlocked)
			}
			if req.URL.Scheme != "https" {
				return fmt.Errorf("%w: redirect to non-https %q", ErrSSRFBlocked, req.URL.String())
			}
			// 每跳剥凭证:同宿重定向 stdlib 会保留 Authorization/Cookie,
			// 防护语义逐跳剥离,不依赖其跨宿启发式;仅剥这两项,其余头
			// (含 T3.5 幂等键头、D-t121 #2 的 W3C traceparent 头)跨跳保留
			// ——凭证只放 Authorization/Cookie 是 T3.5 装配红线,自定义凭证
			// 头不在此列(traceparent 非凭证类,与幂等键同判)。
			// URL userinfo 同步清空:stdlib send() 对每跳用 req.URL.User
			// 派生 Basic 凭证,时点晚于本函数,不清则剥头被绕过
			// (Location 携带 userinfo 是攻击者可自择字段)。
			// 目标 IP 的每跳重校验由重定向新拨号走 Control 完成。
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			req.URL.User = nil
			return nil
		},
	}
}

// httpsOnlyTransport 是仅 https 的 RoundTripper 门(每一跳必经)。
type httpsOnlyTransport struct{ rt http.RoundTripper }

func (g *httpsOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return nil, fmt.Errorf("%w: non-https URL %q", ErrSSRFBlocked, req.URL.String())
	}
	return g.rt.RoundTrip(req)
}
