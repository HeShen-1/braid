package tools

// egress 默认拒绝代理单测(T10.2):CONNECT-only 代理的核心策略面。
// 口径(issue #92 验收 + GATE 1 设计):
//   - 默认拒绝:host(:port) 白名单(单源派生自 BRAID_HTTP_ENDPOINTS,
//     EgressAllowFromEndpoints),白名外一律 403 + host 标记;
//   - DNS/IP 白名单:解析全部落代理侧,任一解析 IP 落禁段整体拒
//     (fail-closed,防混合解析侧门),403 + ssrf 标记;
//   - 拒答经状态行 reason-phrase 标记跨隧道边界,helper 侧
//     egressDenialError 映射回真哨兵(ErrHostBlocked/ErrSSRFBlocked),
//     runner 既有 errors.Is 分类零改动(D-t101 #6 同哲学);
//   - IP 字面量端点不做 DNS(与 http.go allowlistKey「IP 字面量原样」
//     同判),198.18.0.0/15 段放行口径继承 ipForbidden
//     (D-2026-09-23-ssrf-extra-ranges)。

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// startEgressProxy 拉起被测代理:127.0.0.1:0 监听 + Serve goroutine,
// 返回代理拨号地址。生命周期挂 t.Cleanup。
func startEgressProxy(t *testing.T, cfg EgressConfig) string {
	t.Helper()
	p := NewEgressProxy(cfg)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- p.Serve(ln) }()
	t.Cleanup(func() {
		_ = ln.Close()
		select {
		case err := <-serveErr:
			if err != nil {
				t.Logf("proxy serve exit: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Logf("proxy serve did not exit in 5s")
		}
	})
	return ln.Addr().String()
}

// startEchoTarget 本地回显目标(TCP):读到什么写回什么,隧道双向泵的
// 断言原语。返回拨号地址。
func startEchoTarget(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// targetAddrHostPort 拆 127.0.0.1:port 形态地址为 allow 集条目。
func targetAddrHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	h, ps, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	port, err := strconv.Atoi(ps)
	if err != nil {
		t.Fatalf("port %q: %v", ps, err)
	}
	return h, port
}

// egressProbe 原始 CONNECT 探测:拨代理地址,写单条 CONNECT 请求,
// 读回首行,解出 (code, reason)。
func egressProbe(t *testing.T, proxyAddr, authority string) (int, string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	fields := strings.Fields(strings.TrimRight(line, "\r\n"))
	if len(fields) < 2 || fields[0] != "HTTP/1.1" {
		t.Fatalf("status line = %q, want HTTP/1.1 <code> <reason>", line)
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("status code in %q: %v", line, err)
	}
	return code, strings.Join(fields[2:], " ")
}

// allowAllForbiddenOff 是单测的开放禁段缝(GuardConfig.IPForbidden 同款
// 纪律:仅测试注入;生产恒零值 = ipForbidden)。
func allowAllForbiddenOff(net.IP) bool { return false }

func TestEgressProxyPolicy(t *testing.T) {
	echoAddr := startEchoTarget(t)
	echoHost, echoPort := targetAddrHostPort(t, echoAddr)

	cases := []struct {
		name         string
		allow        map[string]map[int]struct{}
		resolver     *net.Resolver
		ipForbidden  func(net.IP) bool
		authority    string
		wantCode     int
		wantReason   string
		wantSentinel error
	}{
		{
			name:         "allowlisted host and port tunnels",
			allow:        map[string]map[int]struct{}{echoHost: {echoPort: {}}},
			ipForbidden:  allowAllForbiddenOff,
			authority:    echoAddr,
			wantCode:     200,
			wantReason:   "Connection Established",
			wantSentinel: nil,
		},
		{
			name:       "default deny host not allowlisted",
			allow:      map[string]map[int]struct{}{"ok.example": {443: {}}},
			authority:  "evil.example:443",
			wantCode:   403,
			wantReason: egressReasonHostBlocked,
			// 非白名 host:哨兵归 host_blocked(策略拒连分桶)。
			wantSentinel: ErrHostBlocked,
		},
		{
			name: "port outside allowlist set denied",
			allow: map[string]map[int]struct{}{
				echoHost: {echoPort: {}},
			},
			ipForbidden: allowAllForbiddenOff,
			// 白名 host 的另一端口:端口级管控(GATE 1 #3,http.go
			// 「端口级管控归 P1 网络层 egress」在此回收)。
			authority:    fmt.Sprintf("%s:%d", echoHost, echoPort+1),
			wantCode:     403,
			wantReason:   egressReasonHostBlocked,
			wantSentinel: ErrHostBlocked,
		},
		{
			name: "forbidden ip literal denied",
			allow: map[string]map[int]struct{}{
				"127.0.0.1": {echoPort: {}},
			},
			// 生产禁段(环回):IP 字面量零 DNS 直查禁段。
			authority:    fmt.Sprintf("127.0.0.1:%d", echoPort),
			wantCode:     403,
			wantReason:   egressReasonSSRFBlocked,
			wantSentinel: ErrSSRFBlocked,
		},
		{
			name: "name resolving to forbidden ip denied",
			allow: map[string]map[int]struct{}{
				"rebind.test": {443: {}},
			},
			resolver:     fakeResolver(t, map[string][]string{"rebind.test": {"10.0.0.5"}}),
			authority:    "rebind.test:443",
			wantCode:     403,
			wantReason:   egressReasonSSRFBlocked,
			wantSentinel: ErrSSRFBlocked,
		},
		{
			name: "mixed resolution fail closed",
			allow: map[string]map[int]struct{}{
				"mixed.test": {443: {}},
			},
			// 一条放行 + 一条禁段:整体拒(单挑放行 IP 拨号 = 把「拨哪个」
			// 的裁决权留给解析序,fail-closed 不留侧门)。
			resolver:     fakeResolver(t, map[string][]string{"mixed.test": {"203.0.113.7", "169.254.169.254"}}),
			authority:    "mixed.test:443",
			wantCode:     403,
			wantReason:   egressReasonSSRFBlocked,
			wantSentinel: ErrSSRFBlocked,
		},
		{
			name: "resolve failure denied non sentinel",
			allow: map[string]map[int]struct{}{
				"nx.test": {443: {}},
			},
			// 假 resolver 无该 host 记录 → 解析失败:fail-closed 拒,
			// 但哨兵两分桶都不是 → 非 sentinel(宿主侧落 tool_failed)。
			resolver:     fakeResolver(t, map[string][]string{"other.test": {"203.0.113.7"}}),
			authority:    "nx.test:443",
			wantCode:     502,
			wantReason:   egressReasonResolveFailed,
			wantSentinel: nil,
		},
		{
			name: "dial failure denied non sentinel",
			allow: map[string]map[int]struct{}{
				echoHost: {1: {}}, // 1 端口无监听:解析(字面量)过、拨号必败。
			},
			ipForbidden:  allowAllForbiddenOff,
			authority:    fmt.Sprintf("%s:1", echoHost),
			wantCode:     502,
			wantReason:   egressReasonDialFailed,
			wantSentinel: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proxyAddr := startEgressProxy(t, EgressConfig{
				Allow:       tc.allow,
				Resolver:    tc.resolver,
				IPForbidden: tc.ipForbidden,
			})
			code, reason := egressProbe(t, proxyAddr, tc.authority)
			if code != tc.wantCode || reason != tc.wantReason {
				t.Fatalf("status = %d %q, want %d %q", code, reason, tc.wantCode, tc.wantReason)
			}
			// 映射面:helper 侧把拒答 reason 映射回哨兵错误的同一函数,
			// 在此与策略表同表锚定(标记词与哨兵的配对契约)。200 是
			// 成功形态,映射必须 nil。
			if code == 200 {
				if err := egressDenialError(code, reason); err != nil {
					t.Fatalf("egressDenialError(200) = %v, want nil", err)
				}
				return
			}
			err := egressDenialError(code, reason)
			switch tc.wantSentinel {
			case nil:
				if errors.Is(err, ErrHostBlocked) || errors.Is(err, ErrSSRFBlocked) {
					t.Fatalf("err = %v, want non-sentinel denial", err)
				}
				if err == nil {
					t.Fatalf("err = nil, want non-nil denial error for %d %q", code, reason)
				}
			default:
				if !errors.Is(err, tc.wantSentinel) {
					t.Fatalf("err = %v, want %v", err, tc.wantSentinel)
				}
			}
		})
	}
}

// TestEgressProxyNonConnect 非 CONNECT 一律拒(绝对形 GET 走私/常规请求
// 不转发——代理是 CONNECT-only,明文 http 绝不经此出网)。
func TestEgressProxyNonConnect(t *testing.T) {
	proxyAddr := startEgressProxy(t, EgressConfig{
		Allow:       map[string]map[int]struct{}{"ok.example": {443: {}}},
		IPForbidden: allowAllForbiddenOff,
	})
	c, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, "GET http://ok.example/ HTTP/1.1\r\nHost: ok.example\r\n\r\n"); err != nil {
		t.Fatalf("write GET: %v", err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.HasPrefix(line, "HTTP/1.1 405") || !strings.Contains(line, egressReasonConnectOnly) {
		t.Fatalf("status line = %q, want 405 %s", line, egressReasonConnectOnly)
	}
}

// TestEgressProxyMalformed 畸形请求行/缺端口的 authority 拒 400。
func TestEgressProxyMalformed(t *testing.T) {
	proxyAddr := startEgressProxy(t, EgressConfig{
		Allow:       map[string]map[int]struct{}{"ok.example": {443: {}}},
		IPForbidden: allowAllForbiddenOff,
	})
	for _, raw := range []string{
		"CONNECT ok.example HTTP/1.1\r\nHost: ok.example\r\n\r\n", // 缺端口
		"CONNECT ok.example:notaport HTTP/1.1\r\nHost: x\r\n\r\n", // 非数字端口
		"garbage\r\n\r\n", // 非请求行
	} {
		c, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
		if err != nil {
			t.Fatalf("dial proxy: %v", err)
		}
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(c, raw); err != nil {
			t.Fatalf("write %q: %v", raw, err)
		}
		line, err := bufio.NewReader(c).ReadString('\n')
		if err != nil {
			// 连接被关(畸形请求直接断)也是合法拒绝形态;不能既无 400 又可续用。
			if strings.Contains(raw, "garbage") {
				continue
			}
			t.Fatalf("write %q: read status line: %v", raw, err)
		}
		if !strings.HasPrefix(line, "HTTP/1.1 400") {
			t.Fatalf("write %q: status line = %q, want 400", raw, line)
		}
		_ = c.Close()
	}
}

// TestEgressProxyTunnelPump 隧道双向泵:CONNECT 建立后字节流原样双向
// 透传(端到端 TLS 由 helper 侧在隧道之上握手,代理零明文可见)。
func TestEgressProxyTunnelPump(t *testing.T) {
	echoAddr := startEchoTarget(t)
	echoHost, echoPort := targetAddrHostPort(t, echoAddr)
	proxyAddr := startEgressProxy(t, EgressConfig{
		Allow:       map[string]map[int]struct{}{echoHost: {echoPort: {}}},
		IPForbidden: allowAllForbiddenOff,
	})

	c, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.HasPrefix(line, "HTTP/1.1 200") {
		t.Fatalf("status = %q, want 200", line)
	}
	// 消化剩余响应头(空行收尾)。
	for {
		hl, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read resp headers: %v", err)
		}
		if strings.TrimSpace(hl) == "" {
			break
		}
	}
	// 隧道期:写标记字节,读回显(经代理双向泵)。
	const marker = "braid-egress-tunnel-probe"
	if _, err := io.WriteString(c, marker); err != nil {
		t.Fatalf("write tunnel payload: %v", err)
	}
	got := make([]byte, len(marker))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("read tunnel echo: %v", err)
	}
	if string(got) != marker {
		t.Fatalf("echo = %q, want %q", got, marker)
	}
}

// TestEgressProxyTunnelCap 并发隧道上界:超限 CONNECT 拒 503,不无限
// 堆 fd(被攻破容器可开原始隧道,worker 侧资源耗尽面须有界)。
func TestEgressProxyTunnelCap(t *testing.T) {
	old := egressMaxTunnels
	egressMaxTunnels = 1
	t.Cleanup(func() { egressMaxTunnels = old })

	echoAddr := startEchoTarget(t)
	echoHost, echoPort := targetAddrHostPort(t, echoAddr)
	proxyAddr := startEgressProxy(t, EgressConfig{
		Allow:       map[string]map[int]struct{}{echoHost: {echoPort: {}}},
		IPForbidden: allowAllForbiddenOff,
	})

	// 第一条:占住唯一名额,保持在场。
	hold, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = hold.Close() }()
	_ = hold.SetDeadline(time.Time{})
	if _, err := fmt.Fprintf(hold, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	hline, err := bufio.NewReader(hold).ReadString('\n')
	if err != nil || !strings.HasPrefix(hline, "HTTP/1.1 200") {
		t.Fatalf("first tunnel status = %q err=%v, want 200", hline, err)
	}

	code, reason := egressProbe(t, proxyAddr, echoAddr)
	if code != 503 || reason != egressReasonOverloaded {
		t.Fatalf("second CONNECT = %d %q, want 503 %s", code, reason, egressReasonOverloaded)
	}
}

// TestEgressDenialErrorMapping 拒答标记 → 哨兵映射的纯函数表:
// 三类标记外的组合(如 200 / 未知 reason)必须 nil 或非哨兵——映射只认
// (code, reason) 精确配对,防未来新增 reason 静默错配。
func TestEgressDenialErrorMapping(t *testing.T) {
	cases := []struct {
		code         int
		reason       string
		wantSentinel error
		wantNil      bool
	}{
		{403, egressReasonHostBlocked, ErrHostBlocked, false},
		{403, egressReasonSSRFBlocked, ErrSSRFBlocked, false},
		{502, egressReasonResolveFailed, nil, false},
		{502, egressReasonDialFailed, nil, false},
		{503, egressReasonOverloaded, nil, false},
		{405, egressReasonConnectOnly, nil, false},
		{200, "Connection Established", nil, true},
		{403, "Some Other Reason", nil, true},
	}
	for _, tc := range cases {
		err := egressDenialError(tc.code, tc.reason)
		if tc.wantNil {
			if err != nil {
				t.Fatalf("(%d, %q): err = %v, want nil", tc.code, tc.reason, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("(%d, %q): err = nil, want non-nil", tc.code, tc.reason)
		}
		if tc.wantSentinel != nil && !errors.Is(err, tc.wantSentinel) {
			t.Fatalf("(%d, %q): err = %v, want %v", tc.code, tc.reason, err, tc.wantSentinel)
		}
		if tc.wantSentinel == nil && (errors.Is(err, ErrHostBlocked) || errors.Is(err, ErrSSRFBlocked)) {
			t.Fatalf("(%d, %q): err = %v, want non-sentinel", tc.code, tc.reason, err)
		}
	}
}

// TestEgressAllowFromEndpoints 白名单单源派生:同 BRAID_HTTP_ENDPOINTS
// 原料产出 host→端口集(基线 §4c 防双真源)。端口推导:显式端口或 https
// 缺省 443;host 归一 = 小写(IP 字面量原样),与 http.go allowlistKey
// 同判。
func TestEgressAllowFromEndpoints(t *testing.T) {
	got, err := EgressAllowFromEndpoints(map[string]string{
		"a":        "https://Example.COM",
		"b":        "https://example.com:8443",
		"c":        "https://example.com:443",
		"hermetic": "https://198.19.0.1:32768",
	})
	if err != nil {
		t.Fatalf("EgressAllowFromEndpoints: %v", err)
	}
	want := map[string]map[int]struct{}{
		"example.com": {443: {}, 8443: {}},
		"198.19.0.1":  {32768: {}},
	}
	if len(got) != len(want) {
		t.Fatalf("allow = %v, want %v", got, want)
	}
	for host, ports := range want {
		gotPorts, ok := got[host]
		if !ok {
			t.Fatalf("host %q missing in %v", host, got)
		}
		if len(gotPorts) != len(ports) {
			t.Fatalf("ports for %q = %v, want %v", host, gotPorts, ports)
		}
		for p := range ports {
			if _, ok := gotPorts[p]; !ok {
				t.Fatalf("port %d for %q missing in %v", p, host, gotPorts)
			}
		}
	}
}

// TestEgressAllowFromEndpointsRejectsBad 坏值响亮败(装配缺陷启动即现
// 纪律;构造器上游 NewHTTPTool 已校验,此处是同门二道防御)。
func TestEgressAllowFromEndpointsRejectsBad(t *testing.T) {
	for name, raw := range map[string]string{
		"not url":     ":::",
		"http scheme": "http://example.com",
		"with path":   "https://example.com/p",
	} {
		if _, err := EgressAllowFromEndpoints(map[string]string{name: raw}); err == nil {
			t.Fatalf("EgressAllowFromEndpoints(%q) = nil err, want error", raw)
		}
	}
}

// TestEgressProxyActiveTunnels 计数器进出平衡(容量闸的实现原语;泄漏
// 计数会把容量闸耗成永久 503)。
func TestEgressProxyActiveTunnels(t *testing.T) {
	p := NewEgressProxy(EgressConfig{Allow: map[string]map[int]struct{}{}})
	if n := atomic.LoadInt64(&p.activeTunnels); n != 0 {
		t.Fatalf("activeTunnels = %d, want 0 at construction", n)
	}
}

// --- 隧道拨号器(NewGuardedClient.ProxyURL → DialTLSContext 手工 CONNECT)---

// tunnelGET 经受防客户端发一次 GET,返回错误(成功时返回 nil 并校验状态)。
func tunnelGET(t *testing.T, client *http.Client, rawurl string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %q", resp.StatusCode, body)
	}
	return nil
}

// tunnelPool 把 httptest TLS server 证书装进客户端根池(信任注入仅测试)。
func tunnelPool(t *testing.T, srv *httptest.Server) *tls.Config {
	t.Helper()
	cert, err := x509.ParseCertificate(srv.Certificate().Raw)
	if err != nil {
		t.Fatalf("parse httptest cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{RootCAs: pool}
}

func TestGuardedClientEgressTunnel(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "tunnel-ok "+r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	srvHost, srvPort := targetAddrHostPort(t, srv.Listener.Addr().String())
	proxyURLFor := func(t *testing.T, allow map[string]map[int]struct{}, forbidden func(net.IP) bool) *url.URL {
		t.Helper()
		pu, err := url.Parse("http://" + startEgressProxy(t, EgressConfig{Allow: allow, IPForbidden: forbidden}))
		if err != nil {
			t.Fatalf("parse proxy url: %v", err)
		}
		return pu
	}

	t.Run("https via tunnel reaches allowlisted target", func(t *testing.T) {
		proxy := proxyURLFor(t,
			map[string]map[int]struct{}{srvHost: {srvPort: {}}},
			allowAllForbiddenOff)
		client := NewGuardedClient(GuardConfig{ProxyURL: proxy, TLS: tunnelPool(t, srv)})
		defer client.CloseIdleConnections()
		if err := tunnelGET(t, client, srv.URL+"/probe"); err != nil {
			t.Fatalf("tunnel GET: %v", err)
		}
		// 连接池复用走同一隧道路径(第二次请求不应重建/不应失败)。
		if err := tunnelGET(t, client, srv.URL+"/again"); err != nil {
			t.Fatalf("tunnel GET (pooled): %v", err)
		}
	})

	t.Run("host denial sentinel crosses tunnel", func(t *testing.T) {
		// 白名单不含目标 host:代理 403 host 标记 → DialTLSContext 映射
		// ErrHostBlocked → url.Error %w 链 → errors.Is 命中(宿主 runner
		// 分类零改动的跨边界保真证明)。
		proxy := proxyURLFor(t,
			map[string]map[int]struct{}{"other.example": {443: {}}},
			allowAllForbiddenOff)
		client := NewGuardedClient(GuardConfig{ProxyURL: proxy, TLS: tunnelPool(t, srv)})
		defer client.CloseIdleConnections()
		// 目标 host 伪装:直连 srv 地址,host 不在白名单。
		err := tunnelGET(t, client, fmt.Sprintf("https://%s:%d/x", srvHost, srvPort))
		if !errors.Is(err, ErrHostBlocked) {
			t.Fatalf("err = %v, want ErrHostBlocked", err)
		}
	})

	t.Run("ssrf denial sentinel crosses tunnel", func(t *testing.T) {
		// host 白名但目标 IP 落禁段(生产禁段:环回)→ 403 ssrf 标记。
		proxy := proxyURLFor(t,
			map[string]map[int]struct{}{srvHost: {srvPort: {}}},
			nil) // 默认 ipForbidden:127.0.0.1 环回必拒
		client := NewGuardedClient(GuardConfig{ProxyURL: proxy, TLS: tunnelPool(t, srv)})
		defer client.CloseIdleConnections()
		err := tunnelGET(t, client, fmt.Sprintf("https://%s:%d/x", srvHost, srvPort))
		if !errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("err = %v, want ErrSSRFBlocked", err)
		}
	})

	t.Run("proxy unreachable is plain transport error", func(t *testing.T) {
		// 无监听端口:基础设施性失败,不产哨兵(宿主侧 tool_failed 家族)。
		dead, err := url.Parse("http://127.0.0.1:1")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		client := NewGuardedClient(GuardConfig{ProxyURL: dead, TLS: tunnelPool(t, srv)})
		defer client.CloseIdleConnections()
		err = tunnelGET(t, client, srv.URL+"/x")
		if err == nil || errors.Is(err, ErrHostBlocked) || errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("err = %v, want plain transport error", err)
		}
	})
}

func TestGuardedClientProxyURLPanicTable(t *testing.T) {
	for name, raw := range map[string]string{
		"https scheme": "https://127.0.0.1:9210", // 只接受明文 http 说代理
		"no port":      "http://127.0.0.1",
		"no host":      "http://:9210",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("NewGuardedClient(ProxyURL=%q) did not panic", raw)
				}
			}()
			_ = NewGuardedClient(GuardConfig{ProxyURL: u})
		}()
		_ = name
	}
}

// --- SandboxEgressStartup(启动段:建网/网关/绑听/serve)---

// egressStartupShimBody 有状态 shim 脚本体:网络不存在时 inspect 败,
// create 后成功;internal 恒 true、gateway 恒 127.0.0.1(单测可绑段)。
// 纪律:测试把 PATH 整体换成 shim 目录,脚本体只可用 shell 内建
// (echo/[/case;标记文件用 `: >` 重定向创建,不触 touch)。
func egressStartupShimBody(internal string) string {
	return `
rec "$@"
if [ "$1" = "info" ]; then echo 29.1.3; exit 0; fi
if [ "$1" = "network" ] && [ "$2" = "inspect" ]; then
  [ -f "$d/net" ] || exit 1
  case "$4" in
  *Internal*) echo ` + internal + ` ;;
  *Gateway*) echo 127.0.0.1 ;;
  *) exit 9 ;;
  esac
  exit 0
fi
if [ "$1" = "network" ] && [ "$2" = "create" ]; then : > "$d/net"; exit 0; fi
exit 9
`
}

func TestSandboxEgressStartup(t *testing.T) {
	t.Run("off mode zero docker contact", func(t *testing.T) {
		bin, dir := fakeNamedDockerShim(t, "docker", egressStartupShimBody("true"))
		t.Setenv("PATH", filepath.Dir(bin))
		u, err := SandboxEgressStartup(context.Background(), envOf(map[string]string{"BRAID_TOOL_SANDBOX": "off"}))
		if err != nil || u != nil {
			t.Fatalf("(%v, %v), want (nil, nil)", u, err)
		}
		if lines := shimArgv(t, dir); len(lines) != 0 {
			t.Fatalf("invocations = %q, want zero", lines)
		}
	})

	t.Run("docker mode ensures network and serves proxy", func(t *testing.T) {
		bin, dir := fakeNamedDockerShim(t, "docker", egressStartupShimBody("true"))
		t.Setenv("PATH", filepath.Dir(bin))
		u, err := SandboxEgressStartup(context.Background(), envOf(map[string]string{
			"BRAID_TOOL_SANDBOX":             "docker",
			"BRAID_TOOL_SANDBOX_EGRESS_PORT": "0", // 内核指派:测试零碰撞
			"BRAID_HTTP_ENDPOINTS":           `{"t":"https://127.0.0.1:1"}`,
		}))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" {
			t.Fatalf("addr = %v, want http://127.0.0.1:<port>", u)
		}
		want := []string{
			"info --format {{.ServerVersion}}",
			"network inspect --format {{.Internal}} braid-sbx-egress",
			"network create --internal braid-sbx-egress",
			"network inspect --format {{.Internal}} braid-sbx-egress",
			"network inspect --format {{(index .IPAM.Config 0).Gateway}} braid-sbx-egress",
		}
		lines := shimArgv(t, dir)
		if len(lines) != len(want) {
			t.Fatalf("invocations = %q, want %q", lines, want)
		}
		for i := range want {
			if lines[i] != want[i] {
				t.Fatalf("invocation[%d] = %q, want %q", i, lines[i], want[i])
			}
		}
		// 代理真在服务且 allow 集来自 endpoints(127.0.0.1:1 在白名,
		// 但生产禁段拒环回 → 403 ssrf 标记:allow 派生 + 默认禁段双证)。
		code, reason := egressProbe(t, u.Host, "127.0.0.1:1")
		if code != 403 || reason != egressReasonSSRFBlocked {
			t.Fatalf("probe = %d %q, want 403 %s", code, reason, egressReasonSSRFBlocked)
		}
		// 非白名 host:默认拒绝(host 白名单来自 endpoints 单源)。
		code, reason = egressProbe(t, u.Host, "evil.example:443")
		if code != 403 || reason != egressReasonHostBlocked {
			t.Fatalf("probe = %d %q, want 403 %s", code, reason, egressReasonHostBlocked)
		}
	})

	t.Run("non internal network reuse refused", func(t *testing.T) {
		// 复用同名但非 --internal 的既有网络 = 默认拒绝姿态被拆毁,拒启。
		bin, _ := fakeNamedDockerShim(t, "docker", egressStartupShimBody("false"))
		t.Setenv("PATH", filepath.Dir(bin))
		_, err := SandboxEgressStartup(context.Background(), envOf(map[string]string{
			"BRAID_TOOL_SANDBOX":             "docker",
			"BRAID_TOOL_SANDBOX_EGRESS_PORT": "0",
		}))
		if err == nil || !strings.Contains(err.Error(), "not --internal") {
			t.Fatalf("err = %v, want non-internal refusal", err)
		}
	})

	t.Run("occupied port refuses startup", func(t *testing.T) {
		bin, dir := fakeNamedDockerShim(t, "docker", egressStartupShimBody("true"))
		t.Setenv("PATH", filepath.Dir(bin))
		hold, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("hold port: %v", err)
		}
		defer func() { _ = hold.Close() }()
		_, err = SandboxEgressStartup(context.Background(), envOf(map[string]string{
			"BRAID_TOOL_SANDBOX":             "docker",
			"BRAID_TOOL_SANDBOX_EGRESS_PORT": strconv.Itoa(hold.Addr().(*net.TCPAddr).Port),
		}))
		if err == nil || !strings.Contains(err.Error(), "listen") {
			t.Fatalf("err = %v, want listen failure (shim argv: %q)", err, shimArgv(t, dir))
		}
	})

	t.Run("bad port key refused", func(t *testing.T) {
		// 端口键校验先于 docker 接触(快败;shim 被触到 = 顺序回归)。
		bin, dir := fakeNamedDockerShim(t, "docker", egressStartupShimBody("true"))
		t.Setenv("PATH", filepath.Dir(bin))
		for _, v := range []string{"-1", "65536", "x"} {
			_, err := SandboxEgressStartup(context.Background(), envOf(map[string]string{
				"BRAID_TOOL_SANDBOX":             "docker",
				"BRAID_TOOL_SANDBOX_EGRESS_PORT": v,
			}))
			if err == nil || !strings.Contains(err.Error(), "BRAID_TOOL_SANDBOX_EGRESS_PORT") {
				t.Fatalf("port %q: err = %v, want named rejection (shim argv: %q)", v, err, shimArgv(t, dir))
			}
		}
		if lines := shimArgv(t, dir); len(lines) != 0 {
			t.Fatalf("invocations = %q, want zero (port validated before docker contact)", lines)
		}
	})
}

// TestSandboxCarrierHTTPEnvelopeProxy 信封 proxy 字段:worker 装配(非
// nil EgressAddr)→ 信封带 proxy;api 装配(nil)→ omitted。
func TestSandboxCarrierHTTPEnvelopeProxy(t *testing.T) {
	limiter := newSandboxLimiter(1)
	cfg := sandboxConfig{
		Docker: true, Image: "i:1", Memory: "256m", CPUs: "0.5",
		PidsLimit: 64, MaxConcurrency: 1, EgressNetwork: sandboxEgressNetworkDefault,
	}
	c, err := newSandboxCarrierHTTP(cfg, map[string]string{"a": "https://a.test"}, limiter)
	if err != nil {
		t.Fatalf("carrier (nil proxy): %v", err)
	}
	var plain struct {
		Proxy *string `json:"proxy"`
	}
	if err := json.Unmarshal(c.toolConfig, &plain); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if plain.Proxy != nil {
		t.Fatalf("envelope proxy = %v, want omitted (api path)", *plain.Proxy)
	}

	pu, _ := url.Parse("http://172.18.0.1:9210")
	cfg.EgressAddr = pu
	c2, err := newSandboxCarrierHTTP(cfg, map[string]string{"a": "https://a.test"}, limiter)
	if err != nil {
		t.Fatalf("carrier (proxy set): %v", err)
	}
	var with struct {
		Proxy     string            `json:"proxy"`
		Endpoints map[string]string `json:"endpoints"`
	}
	if err := json.Unmarshal(c2.toolConfig, &with); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if with.Proxy != pu.String() || with.Endpoints["a"] != "https://a.test" {
		t.Fatalf("envelope = %+v, want proxy %v + endpoints preserved", with, pu)
	}
}

// TestRebuildSandboxToolHTTPProxy helper 重建全链路(零 docker):信封
// proxy + endpoints → 隧道客户端;经真代理 + httptest TLS 目标 200。
// 坏 proxy 词形 → 响亮 tool_failed 信封原料(rebuild 错误)。
func TestRebuildSandboxToolHTTPProxy(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "rebuild-ok")
	}))
	t.Cleanup(srv.Close)
	srvHost, srvPort := targetAddrHostPort(t, srv.Listener.Addr().String())

	t.Run("proxied rebuild reaches target via tunnel", func(t *testing.T) {
		// 容器内信任面 = 镜像 CA(e2e 路径);rebuild 路径无 TLS 缝(信封
		// 只带 endpoints+proxy),单测经 SSL_CERT_FILE 钩入系统根(x509
		// Linux 系统根的既定加载点——测试进程侧等价物)。
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
		certPath := filepath.Join(t.TempDir(), "root.pem")
		if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
			t.Fatalf("write root pem: %v", err)
		}
		t.Setenv("SSL_CERT_FILE", certPath)

		proxyAddr := startEgressProxy(t, EgressConfig{
			Allow:       map[string]map[int]struct{}{srvHost: {srvPort: {}}},
			IPForbidden: allowAllForbiddenOff,
		})
		cfgJSON := fmt.Sprintf(`{"endpoints":{"t":"https://%s"},"proxy":"http://%s"}`, srv.Listener.Addr(), proxyAddr)
		tool, err := rebuildSandboxTool("http", json.RawMessage(cfgJSON))
		if err != nil {
			t.Fatalf("rebuild: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res, err := tool.Run(ctx, Call{Inputs: map[string]any{"name": "t", "path": "/x"}})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		var out struct {
			Status json.Number `json:"status"`
			Body   string      `json:"body_text"`
		}
		if err := json.Unmarshal(res.Output, &out); err != nil {
			t.Fatalf("unmarshal %s: %v", res.Output, err)
		}
		if out.Status.String() != "200" || out.Body != "rebuild-ok" {
			t.Fatalf("out = %s, want 200 rebuild-ok", res.Output)
		}
	})

	t.Run("bad proxy word fails loud", func(t *testing.T) {
		_, err := rebuildSandboxTool("http", json.RawMessage(`{"endpoints":{"t":"https://a.test"},"proxy":"https://bad:1"}`))
		if err == nil || !strings.Contains(err.Error(), "egress proxy address") {
			t.Fatalf("err = %v, want loud proxy word rejection", err)
		}
	})
}

// TestEgressProxyTunnelLargePayload 大载荷回归(对抗审计 BLOCKER:请求头
// 阶段的 LimitReader 曾被带进泵,client→target 被钉死 16 KiB):64 KiB
// 载荷双向过泵逐字节回收。
func TestEgressProxyTunnelLargePayload(t *testing.T) {
	echoAddr := startEchoTarget(t)
	echoHost, echoPort := targetAddrHostPort(t, echoAddr)
	proxyAddr := startEgressProxy(t, EgressConfig{
		Allow:       map[string]map[int]struct{}{echoHost: {echoPort: {}}},
		IPForbidden: allowAllForbiddenOff,
	})
	c, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(c)
	if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "HTTP/1.1 200") {
		t.Fatalf("status = %q err=%v, want 200", line, err)
	}
	for {
		hl, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read resp headers: %v", err)
		}
		if strings.TrimSpace(hl) == "" {
			break
		}
	}
	payload := make([]byte, 64<<10)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	go func() {
		_, _ = c.Write(payload)
	}()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	for i := range payload {
		if got[i] != payload[i] {
			t.Fatalf("echo diverged at %d: %d != %d", i, got[i], payload[i])
		}
	}
}

// TestEgressProxyPreTunnelConnCap 隧道前连接闸(orch-review advisory:隧道
// 闸只覆盖已建隧道,建连不发 CONNECT 的慢速洪泛曾可无界占 fd)。闸 =
// 2×隧道上界;占满后新连接立即 503 断。
func TestEgressProxyPreTunnelConnCap(t *testing.T) {
	old := egressMaxTunnels
	egressMaxTunnels = 1 // 连接闸 = 2
	t.Cleanup(func() { egressMaxTunnels = old })

	echoAddr := startEchoTarget(t)
	echoHost, echoPort := targetAddrHostPort(t, echoAddr)
	proxyAddr := startEgressProxy(t, EgressConfig{
		Allow:       map[string]map[int]struct{}{echoHost: {echoPort: {}}},
		IPForbidden: allowAllForbiddenOff,
	})

	// 两条停泊连接(建连不发言):占满连接闸(各留 100ms 让 handler 起跑
	// ——原子计数在 handleConn 入口,goroutine 调度不可假设即时)。
	var parked []net.Conn
	defer func() {
		for _, c := range parked {
			_ = c.Close()
		}
	}()
	for i := 0; i < 2; i++ {
		c, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
		if err != nil {
			t.Fatalf("park #%d: %v", i, err)
		}
		parked = append(parked, c)
		_ = c.SetDeadline(time.Time{})
		time.Sleep(100 * time.Millisecond)
	}

	// 第三条:立即 503(连接闸拒,不进头读窗)。
	c3, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial over cap: %v", err)
	}
	defer func() { _ = c3.Close() }()
	_ = c3.SetDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(c3).ReadString('\n')
	if err != nil {
		t.Fatalf("read over-cap response: %v", err)
	}
	if !strings.HasPrefix(line, "HTTP/1.1 503 "+egressReasonOverloaded) {
		t.Fatalf("status = %q, want 503 %s", line, egressReasonOverloaded)
	}
}

// TestEgressTunnelALPNPin 恒 HTTP/1.1 的结构锚(设计对抗审查 advisory):
// 隧道 TLS 配置把 ALPN 钉死 http/1.1 且 ServerName 取目标 host——任何
// 来源的 NextProtos(含注入配置带的 h2)都被覆写,「h2 协商连接上跑
// HTTP/1.1 persistConn」的协议错乱结构性不可能。
func TestEgressTunnelALPNPin(t *testing.T) {
	cfg := egressTunnelTLSConfig(nil, "example.com")
	if len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != "http/1.1" {
		t.Fatalf("NextProtos = %v, want [http/1.1]", cfg.NextProtos)
	}
	if cfg.ServerName != "example.com" {
		t.Fatalf("ServerName = %q, want example.com", cfg.ServerName)
	}
	// 注入配置(测试根/h2 意外在场)也被钉:Clone 后覆写,不继承。
	injected := &tls.Config{RootCAs: x509.NewCertPool(), NextProtos: []string{"h2"}}
	pinned := egressTunnelTLSConfig(injected, "example.com")
	if len(pinned.NextProtos) != 1 || pinned.NextProtos[0] != "http/1.1" {
		t.Fatalf("pinned NextProtos = %v, want [http/1.1] (injected h2 must be overridden)", pinned.NextProtos)
	}
	if pinned.RootCAs == nil {
		t.Fatal("injected RootCAs must survive the Clone")
	}
}
