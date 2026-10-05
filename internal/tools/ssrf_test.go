package tools

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestIPForbidden 锁定默认禁段口径(ARCHITECTURE §3.4):私网/环回/链路
// 本地(169.254/16 内含 metadata 169.254.169.254)/CGNAT,另收未指定/
// 组播/广播;扩禁五段(D-2026-10-05-ssrf-five-ranges,关闭 D-2026-09-23-
// ssrf-extra-ranges 待议项):NAT64 well-known 64:ff9b::/96、198.18.0.0/15、
// 192.0.0.0/24、0.0.0.0/8、240.0.0.0/4——永不合法的拨号目标,fail-closed。
// 边界值钉死相邻段。
func TestIPForbidden(t *testing.T) {
	cases := []struct {
		name string
		ip   string
		want bool
	}{
		// 环回 127/8(全段,不止 127.0.0.1)
		{"loopback v4", "127.0.0.1", true},
		{"loopback v4 high", "127.255.255.254", true},
		{"loopback v6", "::1", true},
		// 私网 RFC1918
		{"private 10/8 low", "10.0.0.1", true},
		{"private 10/8 high", "10.255.255.254", true},
		{"private 172.16/12 in-low", "172.16.0.1", true},
		{"private 172.16/12 in-high", "172.31.255.254", true},
		{"private 192.168/16", "192.168.1.1", true},
		{"private v6 ULA fc00", "fc00::1", true},
		{"private v6 ULA fd00", "fd12:3456::1", true},
		// 链路本地(含云 metadata 169.254.169.254)
		{"link-local v4", "169.254.0.1", true},
		{"metadata aws/gcp", "169.254.169.254", true},
		{"link-local v6", "fe80::1", true},
		{"link-local v6 high", "febf::1", true},
		// CGNAT 100.64/10(RFC 6598,stdlib 无助手)
		{"cgnat in-low", "100.64.0.1", true},
		{"cgnat in-high", "100.127.255.254", true},
		// 未指定/组播/广播
		{"unspecified v4", "0.0.0.0", true},
		{"unspecified v6", "::", true},
		{"multicast v4", "224.0.0.1", true},
		{"multicast v4 high", "239.255.255.254", true},
		{"multicast v6", "ff02::1", true},
		{"broadcast", "255.255.255.255", true},
		// 扩禁五段(D-2026-10-05-ssrf-five-ranges,2026-10-05 用户拍板):
		// 环境依赖的可拨保留段一律拒。0/8 全零(unspecified)与 240/4
		// 广播原已拒,此处钉「其余部分」与段内全谱。
		// 0.0.0.0/8 非全零(this-network 段)
		{"this-network 0/8 non-zero", "0.1.2.3", true},
		{"this-network 0/8 high", "0.255.255.254", true},
		// 240.0.0.0/4 剩余(class E 保留)
		{"reserved class E low", "240.0.0.1", true},
		{"reserved class E mid", "250.100.1.1", true},
		{"reserved class E high non-bcast", "255.255.255.254", true},
		// 198.18.0.0/15(RFC 2544 基准段,部分云 LB 内部使用;198.19.x
		// 为 t10.1 旧 hermetic 段,已随扩禁重选为 198.51.100.0/24)
		{"benchmark 198.18/15 low", "198.18.0.1", true},
		{"benchmark 198.18/15 mid-high", "198.18.255.254", true},
		{"benchmark 198.18/15 old-hermetic", "198.19.0.1", true},
		{"benchmark 198.18/15 high", "198.19.255.254", true},
		// 192.0.0.0/24(IETF 协议分配段)
		{"ietf protocol in", "192.0.0.1", true},
		{"ietf protocol in-high", "192.0.0.254", true},
		// NAT64 well-known 64:ff9b::/96:内嵌 v4 可经 NAT64 出口触达内网,
		// To4()==nil 逃 v4 分支,由 v6 分支收口
		{"nat64 prefix addr", "64:ff9b::", true},
		{"nat64 embedded v4", "64:ff9b::1.2.3.4", true},
		{"nat64 high", "64:ff9b::ffff:ffff", true},
		// IPv4-mapped IPv6 归一后同口径(四个新 v4 段各一锚)
		{"mapped private", "::ffff:10.0.0.1", true},
		{"mapped loopback", "::ffff:127.0.0.1", true},
		{"mapped benchmark", "::ffff:198.18.0.1", true},
		{"mapped this-network", "::ffff:0.1.2.3", true},
		{"mapped ietf protocol", "::ffff:192.0.0.1", true},
		{"mapped class E", "::ffff:240.0.0.1", true},
		// 公网放行(含各禁段紧邻边界,防 off-by-one)
		{"public", "1.1.1.1", false},
		{"public doc", "203.0.113.10", false},
		{"public doc testnet2 (new hermetic)", "198.51.100.10", false},
		{"this-network above", "1.0.0.1", false},
		{"benchmark below", "198.17.255.254", false},
		{"benchmark above", "198.20.0.1", false},
		{"ietf protocol below", "191.255.255.254", false},
		{"ietf protocol above", "192.0.1.1", false},
		{"nat64 above /96", "64:ff9b:0:0:0:1::", false},
		{"cgnat below", "100.63.255.254", false},
		{"cgnat above", "100.128.0.1", false},
		{"private 172 below", "172.15.255.254", false},
		{"private 172 above", "172.32.0.1", false},
		{"private 192 above", "192.169.0.1", false},
		{"public v6", "2606:4700:4700::1111", false},
		{"public v6 2001", "2001:4860:4860::8888", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("bad test IP %q", tc.ip)
			}
			if got := ipForbidden(ip); got != tc.want {
				t.Errorf("ipForbidden(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}

	// nil IP(解析失败路径)必须 fail-closed:拒。
	if !ipForbidden(nil) {
		t.Error("ipForbidden(nil) = false, want true (fail-closed)")
	}
}

// fakeResolver 是可控 DNS 解析缝(issue #26 要求):PreferGo 纯解析器 +
// Dial 挂 net.Pipe,逐查询手写最小 DNS 应答(A/AAAA 各按 hostToIPs 里
// 对应家族作答,另一族空答)。DNS rebinding 单测借此构造"URL 用域名、
// 解析结果可控"的现场,拨号复核各看各的假象即可制造。
//
// 帧口径(Go 1.27 dnsclient_unix.go exchange 实证):解析器按返回连接
// 是否实现 net.PacketConn 决定帧式,不看 Dial 的 network 参数——net.Pipe
// 是流式连接 → 一律 dnsStreamRoundTrip,即 2 字节长度前缀的 TCP 帧。
//
// goroutine 退出契约:serveDNS 不带独立退出信号,依赖解析器在单次查询
// 结束(成功/错误/ctx 取消)后关闭其持有的另一端 pipe,ReadFull 报错即
// 返回——当前无泄漏(已逐路径核实);若未来 Go 解析器改为长连复用,
// 此处需补显式关闭缝。
func fakeResolver(t *testing.T, hostToIPs map[string][]string) *net.Resolver {
	t.Helper()
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			c1, c2 := net.Pipe()
			go serveDNS(c2, hostToIPs)
			return c1, nil
		},
	}
}

// serveDNS 在单连接上循环应答 DNS 查询,一律 TCP 帧(见 fakeResolver
// 注释)。应答:头(ID 回显 + QR/RD/RA)+ 原问题 + 按家族作答。
func serveDNS(c net.Conn, hostToIPs map[string][]string) {
	defer func() { _ = c.Close() }()
	for {
		var l [2]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return
		}
		q := make([]byte, binary.BigEndian.Uint16(l[:]))
		if _, err := io.ReadFull(c, q); err != nil {
			return
		}
		resp, ok := answerDNS(q, hostToIPs)
		if !ok {
			return
		}
		l[0], l[1] = byte(len(resp)>>8), byte(len(resp))
		if _, err := c.Write(append(l[:], resp...)); err != nil {
			return
		}
	}
}

// answerDNS 构造单条查询的应答字节。问题段按偏移切出(QNAME 标签 +
// QTYPE/QCLASS);应答段 NAME 用 0xC00C 压缩指针,A/AAAA 按预设 IP 的
// 家族过滤,TTL 0(不缓存)。
func answerDNS(q []byte, hostToIPs map[string][]string) ([]byte, bool) {
	if len(q) < 12 {
		return nil, false
	}
	name, off := "", 12
	for {
		if off >= len(q) {
			return nil, false
		}
		l := int(q[off])
		if l == 0 {
			off++
			break
		}
		if off+1+l > len(q) {
			return nil, false
		}
		name += string(q[off+1:off+1+l]) + "."
		off += 1 + l
	}
	if off+4 > len(q) {
		return nil, false
	}
	qType := binary.BigEndian.Uint16(q[off : off+2])
	qClass := binary.BigEndian.Uint16(q[off+2 : off+4])
	ips := hostToIPs[strings.ToLower(strings.TrimSuffix(name, "."))]

	var answers []byte
	count := 0
	for _, s := range ips {
		ip := net.ParseIP(s)
		var rdata []byte
		switch {
		case qType == 1 && ip != nil && ip.To4() != nil: // A
			rdata = ip.To4()
		case qType == 28 && ip != nil && ip.To16() != nil && ip.To4() == nil: // AAAA
			rdata = ip.To16()
		default:
			continue
		}
		answers = append(answers, 0xC0, 0x0C)
		answers = binary.BigEndian.AppendUint16(answers, qType)
		answers = binary.BigEndian.AppendUint16(answers, qClass)
		answers = binary.BigEndian.AppendUint32(answers, 0) // TTL 0
		answers = binary.BigEndian.AppendUint16(answers, uint16(len(rdata)))
		answers = append(answers, rdata...)
		count++
	}

	resp := make([]byte, 0, 12+(off+4)+len(answers))
	resp = append(resp, q[0], q[1])                           // ID 回显
	resp = append(resp, 0x81, 0x80)                           // QR=1 RD=1 RA=1, RCODE=0
	resp = binary.BigEndian.AppendUint16(resp, 1)             // QDCOUNT
	resp = binary.BigEndian.AppendUint16(resp, uint16(count)) // ANCOUNT
	resp = append(resp, 0, 0, 0, 0)                           // NSCOUNT / ARCOUNT
	resp = append(resp, q[12:off+4]...)                       // 问题段原样(QNAME+QTYPE+QCLASS)
	resp = append(resp, answers...)
	return resp, true
}

// tlsSeam 返回信任 httptest TLS 桩证书的 tls.Config;ServerName 覆写用于
// URL 走假域名、证书只签 127.0.0.1 的组合(解析器/重定向用例)。
func tlsSeam(srv *httptest.Server, serverName string) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return &tls.Config{RootCAs: pool, ServerName: serverName}
}

// forbidOnly 生成测试策略:仅拒给定 IP(其余放行)。
func forbidOnly(ips ...string) func(net.IP) bool {
	var banned []net.IP
	for _, s := range ips {
		banned = append(banned, net.ParseIP(s))
	}
	return func(ip net.IP) bool {
		for _, b := range banned {
			if ip.Equal(b) {
				return true
			}
		}
		return false
	}
}

// doGetWithTimeout 给 blocked 类用例的请求加短 ctx:守卫失守(回归)时
// 对不可达禁段的拨号由 ctx 而非 TCP 超时收口,变异击杀保持秒级而不是
// 拖到 go test 全局超时墙。
func doGetWithTimeout(t *testing.T, c *http.Client, rawURL string, d time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	_, err = c.Do(req)
	return err
}

// allowOnly 生成测试策略:仅放行给定 IP(其余全拒)——模拟"环回即公网
// 目标"的最小放行现场。
func allowOnly(ips ...string) func(net.IP) bool {
	var ok []net.IP
	for _, s := range ips {
		ok = append(ok, net.ParseIP(s))
	}
	return func(ip net.IP) bool {
		for _, b := range ok {
			if ip.Equal(b) {
				return false
			}
		}
		return true
	}
}

// TestGuardedClientAllowPath 锁定放行链路:https 全程(拨号复核过、
// TLS 验证走系统/注入根、无凭证泄漏面)完整 200 往返——防的是"全拒
// 假绿"。另钉失败码词表与 DDL(0001_init.up.sql error_code 注释)对齐。
func TestGuardedClientAllowPath(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":200}`)
	}))
	defer srv.Close()
	client := NewGuardedClient(GuardConfig{
		IPForbidden: allowOnly("127.0.0.1"),
		TLS:         tlsSeam(srv, ""),
	})
	resp, err := client.Get(srv.URL + "/api/v1/documents/42?verbose=true")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != `{"status":200}` {
		t.Fatalf("status = %d body = %q, want 200 with body", resp.StatusCode, body)
	}
	if CodeSSRFBlocked != "tool_ssrf_blocked" {
		t.Errorf("CodeSSRFBlocked = %q, want tool_ssrf_blocked (DDL 词表)", CodeSSRFBlocked)
	}
}

// TestGuardedClientDialRecheck 锁定第二层(拨号瞬间复核):Control 在
// connect 之前拒禁段;DNS rebinding(TOCTOU)在"检查即拨号"下无窗口——
// 解析返回禁段 IP,拨号即拒;混合 A 记录回退到放行 IP 完成请求。
func TestGuardedClientDialRecheck(t *testing.T) {
	t.Run("rebinding to private ip blocked", func(t *testing.T) {
		res := fakeResolver(t, map[string][]string{"rebind.test": {"10.0.0.5"}})
		client := NewGuardedClient(GuardConfig{Resolver: res})
		err := doGetWithTimeout(t, client, "https://rebind.test:9/x", 3*time.Second)
		if !errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("err = %v, want ErrSSRFBlocked", err)
		}
	})
	t.Run("rebinding to metadata ip blocked", func(t *testing.T) {
		res := fakeResolver(t, map[string][]string{"rebind.test": {"169.254.169.254"}})
		client := NewGuardedClient(GuardConfig{Resolver: res})
		err := doGetWithTimeout(t, client, "https://rebind.test:9/x", 3*time.Second)
		if !errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("err = %v, want ErrSSRFBlocked", err)
		}
	})
	t.Run("rebinding to loopback v6 blocked", func(t *testing.T) {
		res := fakeResolver(t, map[string][]string{"rebind6.test": {"::1"}})
		client := NewGuardedClient(GuardConfig{Resolver: res})
		err := doGetWithTimeout(t, client, "https://rebind6.test:9/x", 3*time.Second)
		if !errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("err = %v, want ErrSSRFBlocked", err)
		}
	})
	t.Run("ip literal blocked by default policy", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
		}))
		defer srv.Close()
		client := NewGuardedClient(GuardConfig{}) // 默认策略 + 默认解析器
		_, err := client.Get(srv.URL)             // https://127.0.0.1:port
		if !errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("err = %v, want ErrSSRFBlocked", err)
		}
		if n := hits.Load(); n != 0 {
			t.Errorf("server hits = %d, want 0 (blocked before connect)", n)
		}
	})
	t.Run("mixed a records fall back to allowed ip", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			_, _ = io.WriteString(w, "ok")
		}))
		defer srv.Close()
		var rejections atomic.Int32
		forbidden := func(ip net.IP) bool {
			if ip.Equal(net.ParseIP("127.0.0.2")) {
				rejections.Add(1)
				return true
			}
			return false
		}
		u, _ := url.Parse(srv.URL)
		u.Host = "mix.test:" + u.Port()
		res := fakeResolver(t, map[string][]string{"mix.test": {"127.0.0.2", "127.0.0.1"}})
		client := NewGuardedClient(GuardConfig{
			Resolver:    res,
			IPForbidden: forbidden,
			TLS:         tlsSeam(srv, "127.0.0.1"),
		})
		resp, err := client.Get(u.String())
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || string(body) != "ok" {
			t.Fatalf("status = %d body = %q, want 200 ok", resp.StatusCode, body)
		}
		if n := rejections.Load(); n == 0 {
			t.Error("forbidden address was never tried/rejected; fallback path not exercised")
		}
		if n := hits.Load(); n != 1 {
			t.Errorf("server hits = %d, want 1", n)
		}
	})
}

// TestGuardedClientRedirects 锁定第三层(CheckRedirect 每跳重校验)与
// 仅 https 门:重定向到 http 拒;同宿重定向剥 Authorization/Cookie(stdlib
// 对同宿默认保留——防护语义要求逐跳剥,不依赖其跨宿启发式);重定向目标
// 解析到禁段由拨号复核收口;跳数上限(设 CheckRedirect 后 stdlib 默认
// 10 跳上限失效,必须自管)。
func TestGuardedClientRedirects(t *testing.T) {
	t.Run("http url rejected", func(t *testing.T) {
		client := NewGuardedClient(GuardConfig{})
		_, err := client.Get("http://example.invalid/x") // scheme 门在拨号前拒
		if !errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("err = %v, want ErrSSRFBlocked", err)
		}
	})
	t.Run("redirect to http rejected", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/hop" {
				http.Redirect(w, r, "http://127.0.0.1:9/land", http.StatusFound)
				return
			}
		}))
		defer srv.Close()
		client := NewGuardedClient(GuardConfig{IPForbidden: forbidOnly(), TLS: tlsSeam(srv, "")})
		_, err := client.Get(srv.URL + "/hop")
		if !errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("err = %v, want ErrSSRFBlocked", err)
		}
		// 钉住「重定向层先于 transport 层拦」的纵深事实:两层同哨兵,
		// 单断 errors.Is 分不清拦截层,消息前缀把层固定住。
		if !strings.Contains(err.Error(), "redirect to non-https") {
			t.Errorf("err = %v, want CheckRedirect-layer message", err)
		}
	})
	t.Run("redirect strips credentials same host", func(t *testing.T) {
		var gotAuth, gotCookie atomic.Bool
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/hop":
				http.Redirect(w, r, "/land", http.StatusFound) // 同宿相对跳
			case "/land":
				gotAuth.Store(r.Header.Get("Authorization") != "")
				gotCookie.Store(r.Header.Get("Cookie") != "")
				_, _ = io.WriteString(w, "landed")
			}
		}))
		defer srv.Close()
		client := NewGuardedClient(GuardConfig{IPForbidden: forbidOnly(), TLS: tlsSeam(srv, "")})
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/hop", nil)
		req.Header.Set("Authorization", "Bearer scoped-token")
		req.Header.Set("Cookie", "sid=secret")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if gotAuth.Load() || gotCookie.Load() {
			t.Errorf("redirect hop carried credentials: authorization=%v cookie=%v, want both stripped",
				gotAuth.Load(), gotCookie.Load())
		}
	})
	t.Run("redirect to internal host blocked", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/hop" {
				http.Redirect(w, r, "https://internal.test:9/land", http.StatusFound)
			}
		}))
		defer srv.Close()
		res := fakeResolver(t, map[string][]string{"internal.test": {"10.0.0.5"}})
		client := NewGuardedClient(GuardConfig{
			Resolver:    res,
			IPForbidden: forbidOnly("10.0.0.5"), // 首跳环回放行,禁段只留内网目标
			TLS:         tlsSeam(srv, ""),
		})
		_, err := client.Get(srv.URL + "/hop")
		if !errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("err = %v, want ErrSSRFBlocked", err)
		}
	})
	t.Run("redirect cap at ten hops", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			http.Redirect(w, r, "/loop", http.StatusFound) // 自跳,无限重定向
		}))
		defer srv.Close()
		client := NewGuardedClient(GuardConfig{IPForbidden: forbidOnly(), TLS: tlsSeam(srv, "")})
		_, err := client.Get(srv.URL + "/loop")
		if err == nil {
			t.Fatal("err = nil, want redirect cap error")
		}
		if !strings.Contains(err.Error(), "stopped after 10 redirects") {
			t.Fatalf("err = %v, want cap message", err)
		}
		// 越限重定向属 SSRF 拒连家族:哨兵必须在链上(runner 以
		// errors.Is 映射 CodeSSRFBlocked,丢失即错分 tool_failed)。
		if !errors.Is(err, ErrSSRFBlocked) {
			t.Errorf("err = %v, want ErrSSRFBlocked on chain", err)
		}
		if n := hits.Load(); n != 10 {
			t.Errorf("server hits = %d, want 10", n)
		}
	})
	t.Run("redirect userinfo derived credentials stripped", func(t *testing.T) {
		// stdlib send() 对每跳用 req.URL.User 派生 Basic 凭证(时点晚于
		// CheckRedirect):Location 携带 userinfo 时,单靠 Del 头会被重新
		// 注入——CheckRedirect 必须同时清 URL.User。
		var gotAuth atomic.Bool
		var hitLand atomic.Bool
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/hop":
				// r.Host 携 host:port(服务端 r.URL 只有 path/query)。
				http.Redirect(w, r, "https://tok:pw@"+r.Host+"/land", http.StatusFound)
			case "/land":
				hitLand.Store(true)
				gotAuth.Store(r.Header.Get("Authorization") != "")
				_, _ = io.WriteString(w, "landed")
			}
		}))
		defer srv.Close()
		client := NewGuardedClient(GuardConfig{IPForbidden: forbidOnly(), TLS: tlsSeam(srv, "")})
		resp, err := client.Get(srv.URL + "/hop")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if !hitLand.Load() {
			t.Fatal("never landed on /land")
		}
		if gotAuth.Load() {
			t.Error("redirect hop carried userinfo-derived Authorization, want stripped")
		}
	})
	t.Run("env proxy ignored", func(t *testing.T) {
		// Transport.Proxy 恒 nil 是 SSRF 旁路封堵项(经代理绕过本地拨号
		// 复核):env 代理存在时也必须直达源站。目标用**非环回主机名**
		// (假解析器指回 127.0.0.1)——ProxyFromEnvironment 对环回目标
		// 恒 bypass,环回直连 URL 杀不死「捡回代理」变异。
		var proxyHits atomic.Int32
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			proxyHits.Add(1)
		}))
		defer proxy.Close()
		t.Setenv("HTTP_PROXY", proxy.URL)
		t.Setenv("HTTPS_PROXY", proxy.URL)
		t.Setenv("http_proxy", proxy.URL)
		t.Setenv("https_proxy", proxy.URL)

		var hits atomic.Int32
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			_, _ = io.WriteString(w, "direct")
		}))
		defer srv.Close()
		res := fakeResolver(t, map[string][]string{"direct.test": {"127.0.0.1"}})
		client := NewGuardedClient(GuardConfig{
			Resolver:    res,
			IPForbidden: allowOnly("127.0.0.1"),
			TLS:         tlsSeam(srv, "127.0.0.1"),
		})
		resp, err := client.Get("https://direct.test:" + portOf(t, srv.URL) + "/x")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if n := proxyHits.Load(); n != 0 {
			t.Errorf("proxy hits = %d, want 0 (env proxy must be ignored)", n)
		}
		if n := hits.Load(); n != 1 {
			t.Errorf("origin hits = %d, want 1", n)
		}
	})
}

// portOf 取 httptest URL 的端口(主机名 URL 拼接用)。
func portOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	return u.Port()
}
