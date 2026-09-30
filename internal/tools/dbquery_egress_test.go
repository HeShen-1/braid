package tools

// db_query 衍生的 egress 面测试(T10.4,D-2026-09-30-t104-dbquery-egress):
// AllowInternal 钉死条目 = 运维显式授权的内部 DB 位置,豁免 ipForbidden
// 的私网/环回/CGNAT 禁段(且仅此三类);link-local(含 metadata)/未指定/
// 组播仍拒——豁免面最小化。http 派生条目(Allow)的公网-only 姿态不受
// 影响(egress_test.go TestEgressProxyPolicy 既有覆盖)。

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestIPForbiddenPinned 单元表:钉死条目的禁段口径。
func TestIPForbiddenPinned(t *testing.T) {
	cases := []struct {
		name string
		ip   string
		want bool // true = 拒
	}{
		{"private v4 allowed", "10.1.2.3", false},
		{"loopback allowed", "127.0.0.1", false},
		{"cgnat allowed", "100.64.0.9", false},
		{"public allowed", "8.8.8.8", false},
		{"private v6 allowed", "fd00::1", false},
		{"loopback v6 allowed", "::1", false},
		{"link local metadata still forbidden", "169.254.169.254", true},
		{"link local still forbidden", "169.254.1.1", true},
		{"multicast still forbidden", "224.0.0.1", true},
		{"unspecified still forbidden", "0.0.0.0", true},
		{"nil forbidden", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ip net.IP
			if tc.ip != "" {
				ip = net.ParseIP(tc.ip)
				if ip == nil {
					t.Fatalf("unparsable %q", tc.ip)
				}
			}
			if got := ipForbiddenPinned(ip); got != tc.want {
				t.Fatalf("ipForbiddenPinned(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

// TestEgressProxyPinnedEntry 钉死条目经真代理:环回目标在默认禁段下仍可
// 隧道(豁免生效);端口不匹配/条目缺席照旧 403;普通 Allow 条目在默认
// 禁段下对环回目标仍拒(姿态不变锚)。
func TestEgressProxyPinnedEntry(t *testing.T) {
	echoAddr := startEchoTarget(t)
	echoHost, echoPort := targetAddrHostPort(t, echoAddr)

	pinned := startEgressProxy(t, EgressConfig{
		AllowInternal: map[string]map[int]struct{}{echoHost: {echoPort: {}}},
	})
	if code, reason := egressProbe(t, pinned, echoAddr); code != 200 {
		t.Fatalf("pinned loopback target: got %d %s, want 200 (internal exemption)", code, reason)
	}

	miss := startEgressProxy(t, EgressConfig{
		Allow:         map[string]map[int]struct{}{echoHost: {echoPort: {}}},
		AllowInternal: map[string]map[int]struct{}{"other.example": {3306: {}}},
	})
	// 姿态不变锚:Allow 条目(http 派生)对环回目标仍拒——分桶是
	// egressReasonSSRFBlocked(白名 host 落禁段 IP),与钉死条目的 200
	// 构成豁免面的对照断言。
	if code, reason := egressProbe(t, miss, echoAddr); code != 403 || reason != egressReasonSSRFBlocked {
		t.Fatalf("allow-only entry for loopback: got %d %s, want 403 ssrf blocked", code, reason)
	}

	wrongPort := startEgressProxy(t, EgressConfig{
		AllowInternal: map[string]map[int]struct{}{echoHost: {echoPort + 1: {}}},
	})
	if code, reason := egressProbe(t, wrongPort, echoAddr); code != 403 || reason != egressReasonHostBlocked {
		t.Fatalf("pinned host wrong port: got %d %s, want 403 host blocked", code, reason)
	}

	// 同宿主双集按端口组合(orch-review LOW 收口锚):Allow 含同宿主但只
	// 管 443,DSN 端口走钉死条目——豁免可达,不再是 Allow 优先级脚枪。
	mixed := startEgressProxy(t, EgressConfig{
		Allow:         map[string]map[int]struct{}{echoHost: {443: {}}},
		AllowInternal: map[string]map[int]struct{}{echoHost: {echoPort: {}}},
	})
	if code, reason := egressProbe(t, mixed, echoAddr); code != 200 {
		t.Fatalf("allow+internal same host: got %d %s, want 200 (per-(host,port) composition)", code, reason)
	}
	if code, reason := egressProbe(t, mixed, net.JoinHostPort(echoHost, "443")); code != 403 || reason != egressReasonSSRFBlocked {
		// 443 在 Allow 集但环回 IP 落禁段:钉死豁免不随 Allow 端口走——
		// 断言「Allow 端口不因钉死条目同宿主而获得豁免」,落 SSRF 分桶。
		t.Fatalf("allow port on dual-entry host: got %d %s, want 403 ssrf blocked (exemption stays pinned-port-only)", code, reason)
	}
}

// TestEgressAllowFromDSN DSN → 允许集条目派生(单源:与工具构造共用
// dbqDSNConfig,漂移在启动即红)。
func TestEgressAllowFromDSN(t *testing.T) {
	host, port, err := EgressAllowFromDSN("ro:pw@tcp(DB.Example:13306)/db")
	if err != nil {
		t.Fatalf("EgressAllowFromDSN: %v", err)
	}
	if host != "db.example" || port != 13306 {
		t.Fatalf("got %s:%d, want db.example:13306 (host lowercased)", host, port)
	}
	host, port, err = EgressAllowFromDSN("ro:pw@tcp(10.0.0.5)/db")
	if err != nil {
		t.Fatalf("EgressAllowFromDSN: %v", err)
	}
	if host != "10.0.0.5" || port != 3306 {
		t.Fatalf("got %s:%d, want 10.0.0.5:3306 (default port)", host, port)
	}
	// 空地址 = 驱动文档化缺省 127.0.0.1:3306(合法 DSN,派生口径锚)。
	host, port, err = EgressAllowFromDSN("ro:pw@tcp()/db")
	if err != nil {
		t.Fatalf("EgressAllowFromDSN default addr: %v", err)
	}
	if host != "127.0.0.1" || port != 3306 {
		t.Fatalf("got %s:%d, want 127.0.0.1:3306 (driver default addr)", host, port)
	}
	for _, bad := range []string{
		"://bad",
		"ro:pw@unix(/tmp/x.sock)/db",
	} {
		if _, _, err := EgressAllowFromDSN(bad); err == nil {
			t.Fatalf("EgressAllowFromDSN(%q) accepted", bad)
		}
	}
}

// TestEgressTunnelDialPlain 明文隧道拨号器(helper 侧 MySQL DialFunc):
// 经真代理 + 回显目标双向通;条目缺席 = 拒答映射为错误。
func TestEgressTunnelDialPlain(t *testing.T) {
	echoAddr := startEchoTarget(t)
	echoHost, echoPort := targetAddrHostPort(t, echoAddr)
	proxyURL := &url.URL{Scheme: "http", Host: startEgressProxy(t, EgressConfig{
		AllowInternal: map[string]map[int]struct{}{echoHost: {echoPort: {}}},
	})}

	dial := egressTunnelDialPlain(proxyURL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", echoAddr)
	if err != nil {
		t.Fatalf("plain tunnel dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatalf("write over tunnel: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if strings.TrimSpace(line) != "ping" {
		t.Fatalf("echo = %q, want ping", line)
	}

	// 条目缺席(默认拒绝):拨号器收 403,错误可观测(非挂死)。
	deniedURL := &url.URL{Scheme: "http", Host: startEgressProxy(t, EgressConfig{})}
	dialDenied := egressTunnelDialPlain(deniedURL)
	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dcancel()
	if _, err := dialDenied(dctx, "tcp", echoAddr); err == nil {
		t.Fatalf("denied target: dial succeeded, want error")
	} else if !strings.Contains(err.Error(), "egress proxy denied") {
		t.Fatalf("denied dial error = %v, want egress proxy denied marker", err)
	}
}

// TestEgressTunnelDialPlainLargePayload 隧道期无 LimitReader 残留(共享
// CONNECT 提取段的重构锚:泵字节流不受请求头阶段读取上界影响)。
func TestEgressTunnelDialPlainLargePayload(t *testing.T) {
	echoAddr := startEchoTarget(t)
	echoHost, echoPort := targetAddrHostPort(t, echoAddr)
	proxyURL := &url.URL{Scheme: "http", Host: startEgressProxy(t, EgressConfig{
		AllowInternal: map[string]map[int]struct{}{echoHost: {echoPort: {}}},
	})}
	conn, err := egressTunnelDialPlain(proxyURL)(context.Background(), "tcp", echoAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	payload := strings.Repeat("x", 64<<10)
	go func() {
		_, _ = conn.Write([]byte(payload))
	}()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadFull(conn, make([]byte, len(payload)))
	if err != nil {
		t.Fatalf("read full payload: %v", err)
	}
	if got != len(payload) {
		t.Fatalf("read %d bytes, want %d", got, len(payload))
	}
}
