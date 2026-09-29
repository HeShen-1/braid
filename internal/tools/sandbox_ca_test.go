package tools

// 私 CA 材料生成器与构建期注入契约测试(T10.1 T7):hermetic http e2e
// (T10,私 CA + 198.19.x 网段)的信任材料单源。
//
// 契约(docs/designs/t101-sandbox-baseline.md §3「私 CA 注入契约」):
//   - 信任集扩张只经镜像构建:context 文件 sandbox-e2e-ca.crt(仅 CA 证书
//     PEM)在构建期 append 进 /etc/ssl/certs/ca-certificates.crt;
//   - hermetic e2e 的 TLS server 落宿主侧(§3 钉死:bridge 网容器 IP 全
//     落禁段,可达的 198.19.x 地址必为宿主持有;叶对 = 宿主侧 serve 的
//     服务身份);叶证书与全部私钥不进 build context、不入镜像、不经任何
//     运行期通道进容器,材料每次测试运行临时生成,零落仓零落库;
//   - 运行期零通道:容器 env(§5 白名单恰 PATH/LANG)/ stdin 信封(装的是
//     工具原料,非信任锚)/ 挂载均不承载 CA——§9-7「束内容随镜像构建
//     固定,运行期不改」的落地。
//
// 本文件全成员测试专用(零生产代码面;e2e 密钥材料永不进生产二进制):
// T8 镜像构建(deploy/sandbox.Dockerfile 的 sandbox-e2e target)与 T10
// e2e 经本生成器消费,材料形状零漂移。

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// sandboxCACertFile 私 CA 证书在镜像 build context 的成员名(§3 契约;
// T8 Dockerfile sandbox-e2e target COPY 本名,T10 e2e 组装 context 同名,
// 两侧零漂移)。
const sandboxCACertFile = "sandbox-e2e-ca.crt"

// sandboxHermeticCIDR hermetic 段(T10 裁决:NameConstraints 钉段):SSRF
// 守卫禁段外的放行段(D-2026-09-23-ssrf-extra-ranges 维持 198.18.0.0/15
// 放行),e2e CA 的可签 IP 被结构性限制在该段内——ephemeral 材料即便泄漏
// 也无法为段外身份(公网域名/IP、内网其它段)背书。守卫扩禁同步重选段
// 时,本常量与 DECISIONS 待议项联动。
const sandboxHermeticCIDR = "198.18.0.0/15"

// sandboxCASentinelDNS DNS 名的哨兵域(T10 审计补全:RFC 5280 语义下
// permitted 集只含 IP 条目 = DNS 名 unconstrained fail-open,「钉段」只
// 落地一半——段内 IP + 恶意 DNS SAN 的叶曾可过验)。设非空 permitted
// DNS 集(只许本哨兵 .invalid 保留域)把 DNS 面翻转为「只许哨兵」:真实
// 叶恒零 DNS SAN(生成器模板钉死 + shape 锚),任何 DNS 名一律拒。
// URI/Email 同为 fail-open 类型,但生成器从不设置且无 TLS 消费面,模板
// 形状即边界(不为不可消费的名字类型追加深约束)。
const sandboxCASentinelDNS = "braid-e2e-sentinel.invalid"

// 材料有效期:ephemeral e2e 用,24h 封顶;NotBefore 回拨 1h 容宿主/容器
// 时钟步进(CA/叶同窗)。
const (
	sandboxCABackdate = time.Hour
	sandboxCATTL      = 24 * time.Hour
)

// sandboxCAMaterials 一套 e2e 私 CA 材料。仅 CACertPEM 允许进镜像 build
// context;CAKeyPEM(防再签)与叶对(serve 的服务身份)恒留宿主。
type sandboxCAMaterials struct {
	CACertPEM   []byte
	CAKeyPEM    []byte
	LeafCertPEM []byte
	LeafKeyPEM  []byte
	CA          *x509.Certificate
	Leaf        *x509.Certificate
}

// generateSandboxCA 生成材料:自签 CA(ECDSA P-256,仅签叶、禁中间 CA)+
// 由其签发的叶证书(IP SAN = ips,ServerAuth EKU)。失败 t.Fatal——材料
// 生成是装配缺陷,启动即现惯例。
func generateSandboxCA(t *testing.T, ips []net.IP) sandboxCAMaterials {
	t.Helper()
	if len(ips) == 0 {
		t.Fatal("generateSandboxCA: at least one IP SAN required (hermetic e2e server address)")
	}
	now := time.Now()

	// NameConstraints 钉段(T10 裁决):只允许签 198.18.0.0/15 段内 IP 的叶。
	_, hermeticNet, err := net.ParseCIDR(sandboxHermeticCIDR)
	if err != nil {
		t.Fatalf("parse %s: %v", sandboxHermeticCIDR, err)
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          sandboxCASerial(t),
		Subject:               pkix.Name{Organization: []string{"braid"}, CommonName: "braid-sandbox-e2e-ca"},
		NotBefore:             now.Add(-sandboxCABackdate),
		NotAfter:              now.Add(sandboxCATTL),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,                           // 用途单一收窄:只签叶,禁链出中间 CA
		PermittedIPRanges:     []*net.IPNet{hermeticNet},      // 钉段:段外叶必拒(e2e 材料泄漏半径封顶)
		PermittedDNSDomains:   []string{sandboxCASentinelDNS}, // DNS 面:只许哨兵域(零 DNS SAN 的真实叶不受影响),恶意 DNS 名必拒
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber:          sandboxCASerial(t),
		Subject:               pkix.Name{Organization: []string{"braid"}, CommonName: "braid-sandbox-e2e-leaf"},
		NotBefore:             now.Add(-sandboxCABackdate),
		NotAfter:              now.Add(sandboxCATTL),
		KeyUsage:              x509.KeyUsageDigitalSignature, // ECDSA 协商用;KeyEncipherment 是 RSA 专用位
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           ips,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf certificate: %v", err)
	}
	leafCert, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatalf("parse leaf certificate: %v", err)
	}

	return sandboxCAMaterials{
		CACertPEM:   sandboxCAPemCert(t, caDER),
		CAKeyPEM:    sandboxCAPemKey(t, caKey),
		LeafCertPEM: sandboxCAPemCert(t, leafDER),
		LeafKeyPEM:  sandboxCAPemKey(t, leafKey),
		CA:          caCert,
		Leaf:        leafCert,
	}
}

// writeCACertToContext 把 CA 证书写成 build context 成员(§3 契约唯一允许
// 进 context 的材料)。函数体只触碰 CACertPEM——receiver 虽携全量材料,
// 「恰一成员 + 零私钥」约束由 TestSandboxCACertBuildContext 机械钉死。返回
// 成员路径。
func (m sandboxCAMaterials) writeCACertToContext(t *testing.T, ctxDir string) string {
	t.Helper()
	p := filepath.Join(ctxDir, sandboxCACertFile)
	if err := os.WriteFile(p, m.CACertPEM, 0o644); err != nil { // 公开材料,无保密位需求
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// sandboxCASerial 128-bit 随机序列号(防相邻材料碰撞;单调序列是生产
// 侧约束,e2e 材料无此需求)。
func sandboxCASerial(t *testing.T) *big.Int {
	t.Helper()
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}
	return serial
}

// sandboxCAPemCert DER → PEM CERTIFICATE 块(信任束成员词形)。
func sandboxCAPemCert(t *testing.T, der []byte) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// sandboxCAPemKey 私钥 → PEM PKCS#8 块(tls.X509KeyPair 可消费的现代
// 算法无关词形;SEC1 EC 块是遗留兼容面,不产出)。
func sandboxCAPemKey(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal PKCS8 key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// sandboxCAParseKey PEM PKCS#8 → ECDSA 私钥(配对断言的消费口)。
func sandboxCAParseKey(t *testing.T, pemBytes []byte) *ecdsa.PrivateKey {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatal("key PEM undecodable")
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse PKCS8 key: %v", err)
	}
	key, ok := keyAny.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("key type = %T, want *ecdsa.PrivateKey", keyAny)
	}
	return key
}

// TestSandboxCAMaterialsShape 材料形状契约:CA/叶证书字段(CA:true、
// pathlen 0、ServerAuth EKU、IP SAN 回显)、ephemeral 有效窗、PEM 块词形、
// 密钥与证书配对、叶经 CA 池可验、CA 自签自洽。
func TestSandboxCAMaterialsShape(t *testing.T) {
	now := time.Now()
	ips := []net.IP{net.ParseIP("198.19.0.1"), net.ParseIP("198.19.0.2")}
	m := generateSandboxCA(t, ips)

	if !m.CA.IsCA || m.CA.KeyUsage&x509.KeyUsageCertSign == 0 || !m.CA.MaxPathLenZero {
		t.Fatalf("CA constraints wrong: IsCA=%v certSign=%v maxPathLenZero=%v",
			m.CA.IsCA, m.CA.KeyUsage&x509.KeyUsageCertSign != 0, m.CA.MaxPathLenZero)
	}
	// 钉段在场(T10 裁决):CA 携带 hermetic 段的 permitted IP 约束——
	// 丢字段的回归在此红;段外叶必拒的行为面由 TestSandboxCANameConstraints
	// 对抗证明。
	if len(m.CA.PermittedIPRanges) != 1 || m.CA.PermittedIPRanges[0].String() != sandboxHermeticCIDR {
		t.Fatalf("CA permitted IP ranges = %v, want exactly [%s]", m.CA.PermittedIPRanges, sandboxHermeticCIDR)
	}
	// DNS 哨兵域在场 + 真实叶恒零 DNS SAN(钉段的另一半:IP 段 + DNS
	// 哨兵;叶带 DNS 名的漂移在此红,恶意 DNS 行为面由负测锚)。
	if len(m.CA.PermittedDNSDomains) != 1 || m.CA.PermittedDNSDomains[0] != sandboxCASentinelDNS {
		t.Fatalf("CA permitted DNS domains = %v, want exactly [%s]", m.CA.PermittedDNSDomains, sandboxCASentinelDNS)
	}
	if len(m.Leaf.DNSNames) != 0 {
		t.Fatalf("leaf DNS SAN = %v, want none (hermetic e2e is IP-addressed)", m.Leaf.DNSNames)
	}
	if m.Leaf.IsCA {
		t.Fatal("leaf must not be a CA")
	}
	serverAuth := false
	for _, eku := range m.Leaf.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth {
			serverAuth = true
		}
	}
	if !serverAuth {
		t.Fatalf("leaf ExtKeyUsage = %v, want ServerAuth", m.Leaf.ExtKeyUsage)
	}
	if len(m.Leaf.IPAddresses) != len(ips) {
		t.Fatalf("leaf IP SAN count = %d, want %d (%v)", len(m.Leaf.IPAddresses), len(ips), ips)
	}
	got := make(map[string]bool, len(m.Leaf.IPAddresses))
	for _, ip := range m.Leaf.IPAddresses {
		got[ip.String()] = true
	}
	for _, ip := range ips {
		if !got[ip.String()] {
			t.Fatalf("leaf IP SAN missing %s (got %v)", ip, m.Leaf.IPAddresses)
		}
	}
	// ephemeral 窗:NotBefore 回拨生效;CA/叶同窗逐字落地(回归只动 CA
	// 模板——如丢回拨——同样变红);叶 NotAfter 有下界(防 TTL 单向缩小
	// 漂移,慢镜像构建下材料在消费前过期)且不超 24h+裕量。
	if !m.Leaf.NotBefore.Before(now) {
		t.Fatalf("leaf NotBefore = %v, want backdated before %v", m.Leaf.NotBefore, now)
	}
	if !m.CA.NotBefore.Equal(m.Leaf.NotBefore) || !m.CA.NotAfter.Equal(m.Leaf.NotAfter) {
		t.Fatalf("CA/leaf validity windows diverged: CA=[%v,%v] leaf=[%v,%v]",
			m.CA.NotBefore, m.CA.NotAfter, m.Leaf.NotBefore, m.Leaf.NotAfter)
	}
	if m.Leaf.NotAfter.Before(now.Add(sandboxCATTL - time.Hour)) {
		t.Fatalf("leaf NotAfter = %v, want >= now+%s (TTL shrink regression?)", m.Leaf.NotAfter, sandboxCATTL-time.Hour)
	}
	if m.Leaf.NotAfter.After(now.Add(sandboxCATTL + time.Hour)) {
		t.Fatalf("leaf NotAfter = %v, want <= now+%s", m.Leaf.NotAfter, sandboxCATTL+time.Hour)
	}
	// PEM 块词形:cert = CERTIFICATE,key = PRIVATE KEY(PKCS#8)。
	for name, pemBytes := range map[string][]byte{
		"CACertPEM":   m.CACertPEM,
		"LeafCertPEM": m.LeafCertPEM,
	} {
		block, _ := pem.Decode(pemBytes)
		if block == nil || block.Type != "CERTIFICATE" {
			t.Fatalf("%s is not a PEM CERTIFICATE block", name)
		}
	}
	for name, pemBytes := range map[string][]byte{
		"CAKeyPEM":   m.CAKeyPEM,
		"LeafKeyPEM": m.LeafKeyPEM,
	} {
		block, _ := pem.Decode(pemBytes)
		if block == nil || block.Type != "PRIVATE KEY" {
			t.Fatalf("%s is not a PEM PKCS#8 block", name)
		}
	}
	// 密钥与证书配对,CA 与叶两侧(叶 = serve 服务身份,CA = 防再签面;
	// 若 T10 将来以 CAKey 再签新叶,配对缺失即静默漂移点,两侧同钉)。
	for name, tc := range map[string]struct {
		keyPEM []byte
		cert   *x509.Certificate
	}{
		"leaf": {m.LeafKeyPEM, m.Leaf},
		"CA":   {m.CAKeyPEM, m.CA},
	} {
		key := sandboxCAParseKey(t, tc.keyPEM)
		certPub, ok := tc.cert.PublicKey.(*ecdsa.PublicKey)
		if !ok {
			t.Fatalf("%s cert public key type = %T, want *ecdsa.PublicKey", name, tc.cert.PublicKey)
		}
		if !key.PublicKey.Equal(certPub) {
			t.Fatalf("%s key does not pair with its certificate", name)
		}
	}
	// 链:叶经 CA 池验证(ServerAuth 用途);CA 自签自洽。
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(m.CACertPEM) {
		t.Fatal("CA PEM not loadable into a cert pool")
	}
	if _, err := m.Leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("leaf does not verify against its CA: %v", err)
	}
	if err := m.CA.CheckSignatureFrom(m.CA); err != nil {
		t.Fatalf("CA not self-signed: %v", err)
	}
}

// TestSandboxCABundleAppendTrust 核心主张:构建期 append 语义 ⇒ 容器内
// Go 系统池信任。链路同构面:crypto/x509 在 Linux 读 /etc/ssl/certs/
// ca-certificates.crt 的实现即「文件字节 → CertPool.AppendCertsFromPEM」
// (root.go loadOnDiskRoots;root_linux.go 仅声明 certFiles 路径表,该
// 文件在 alpine 上是探测首项)——本测试以「既有束 + append 的 CA」字节
// 构池,经 x509.Verify 与真 TLS 握手双面钉死:
//   - 正向:append 后的束池下,叶证书通过验证、握手成功、body 到达;
//   - 负向:同束去掉本 CA(仅既有束)→ 验证拒绝 + 握手呈
//     tls.CertificateVerificationError——信任增量确来自 append,而非
//     系统池或客户端旁路。
func TestSandboxCABundleAppendTrust(t *testing.T) {
	// 材料全落在 hermetic 段内(CA 已被 NameConstraints 钉段,段外叶在此
	// 会因约束拒绝——那不是本测试的主张面,别让钉段污染 append 语义)。
	m := generateSandboxCA(t, []net.IP{net.ParseIP("198.19.0.1")})
	// 既有束占位:另一套材料的 CA(多 PEM 成员的真束形态,append 落尾部)。
	base := generateSandboxCA(t, []net.IP{net.ParseIP("198.19.0.2")})
	bundle := append(append([]byte{}, base.CACertPEM...), m.CACertPEM...)

	appended := x509.NewCertPool()
	if !appended.AppendCertsFromPEM(bundle) {
		t.Fatal("appended bundle not loadable into a cert pool")
	}
	baseOnly := x509.NewCertPool()
	if !baseOnly.AppendCertsFromPEM(base.CACertPEM) {
		t.Fatal("base bundle not loadable into a cert pool")
	}
	if _, err := m.Leaf.Verify(x509.VerifyOptions{Roots: appended, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("leaf does not verify against appended bundle: %v", err)
	}
	if _, err := m.Leaf.Verify(x509.VerifyOptions{Roots: baseOnly, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Fatal("leaf verified against base bundle without its CA (append is not the trust delta)")
	}

	// 真 TLS 握手:服务端持叶对(宿主侧 serve 形态);客户端 RootCAs
	// = 束池。刻意不用 httptest.Server.Client()——它注入 srv 自签证书
	// 信任,恰好绕过被测主张。httptest 绑 127.0.0.1 而 CA 已钉段(SAN
	// 只能是 198.18/15 段内 IP):URL 主机写叶 SAN 的段内地址、拨号重定向
	// 到 srv 实际监听地址(hosts 钉扎同效)——证书验证只消费 URL 主机,
	// 验证链完整性不受拨号目标影响。
	pair, err := tls.X509KeyPair(m.LeafCertPEM, m.LeafKeyPEM)
	if err != nil {
		t.Fatalf("leaf pair: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("braid-e2e-tls-ok"))
	}))
	// 负向半边按预期触发握手失败;静默 http.Server 默认 ErrorLog,免得
	// -v 输出把预期内的 "remote error: tls: bad certificate" 呈成异常。
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	sanHost := "198.19.0.1"
	sanURL := func() string {
		_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
		if err != nil {
			t.Fatalf("split srv addr: %v", err)
		}
		return "https://" + net.JoinHostPort(sanHost, port)
	}
	dialPinned := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", srv.Listener.Addr().String())
	}

	trGood := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: appended},
		DialContext:     dialPinned,
	}
	t.Cleanup(trGood.CloseIdleConnections)
	resp, err := (&http.Client{Transport: trGood}).Get(sanURL())
	if err != nil {
		t.Fatalf("handshake with appended-bundle pool: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "braid-e2e-tls-ok" {
		t.Fatalf("response = %d %q, want 200 %q", resp.StatusCode, body, "braid-e2e-tls-ok")
	}

	trBad := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: baseOnly},
		DialContext:     dialPinned,
	}
	t.Cleanup(trBad.CloseIdleConnections)
	_, err = (&http.Client{Transport: trBad}).Get(sanURL())
	var verifyErr *tls.CertificateVerificationError
	if !errors.As(err, &verifyErr) {
		t.Fatalf("want TLS certificate verification failure without CA in pool, got %v", err)
	}
	// 收窄到「根缺失」指征:过期/hostname 类失败不放行——「缺根」才是
	// 「信任增量来自 append」的唯一失败模式(tls 错误链可 Unwrap 至 x509;
	// UnknownAuthorityError 按值落链,target 须取值类型)。
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Fatalf("want x509.UnknownAuthorityError (missing root), got %v", err)
	}
}

// TestSandboxCANameConstraints 钉段对抗证明:以 CAKey 再签段外叶
// (192.0.2.1,TEST-NET-1)——CA 在信任池内也必须拒,且失败模式 = 约束
// 违反(不是过期/未知 CA 等旁因);段内叶(生成器产物)放行作对照。
// NameConstraints 被删的变异下,段外叶验证通过即红。
func TestSandboxCANameConstraints(t *testing.T) {
	m := generateSandboxCA(t, []net.IP{net.ParseIP("198.19.0.3")})
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(m.CACertPEM) {
		t.Fatal("CA PEM not loadable into a cert pool")
	}
	// 段内对照:约束在场且不误伤 hermetic 段自身。
	if _, err := m.Leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("in-segment leaf rejected: %v", err)
	}

	caKey := sandboxCAParseKey(t, m.CAKeyPEM)
	// 再签探针:以 CAKey 签任意形状叶(材料单源,配对由 shape 测试锚)。
	signLeaf := func(cn string, ips []net.IP, dns []string) *x509.Certificate {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("generate probe leaf key: %v", err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: sandboxCASerial(t),
			Subject:      pkix.Name{Organization: []string{"braid"}, CommonName: cn},
			NotBefore:    m.Leaf.NotBefore,
			NotAfter:     m.Leaf.NotAfter,
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			IPAddresses:  ips,
			DNSNames:     dns,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, m.CA, &key.PublicKey, caKey)
		if err != nil {
			t.Fatalf("sign probe leaf %q: %v", cn, err)
		}
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("parse probe leaf %q: %v", cn, err)
		}
		return leaf
	}
	mustReject := func(name string, leaf *x509.Certificate) {
		t.Helper()
		_, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
		if err == nil {
			t.Fatalf("%s verified (constraint dropped?)", name)
		}
		// 失败模式收窄到「约束拒绝」(Go 1.27 实测词形:值类型
		// CertificateInvalidError + CANotAuthorizedForThisName——errors.As
		// 值类型目标,T7 同款纪律):过期/未知 CA/用途不符等旁因不放行,
		// 「被约束拒」才是钉段的唯一失败模式。
		var invalid x509.CertificateInvalidError
		if !errors.As(err, &invalid) || invalid.Reason != x509.CANotAuthorizedForThisName {
			t.Fatalf("%s: err = %v, want CertificateInvalidError(CANotAuthorizedForThisName)", name, err)
		}
	}

	// 段外 IP(192.0.2.1,TEST-NET-1):IP 约束执法面。
	mustReject("out-of-segment IP leaf", signLeaf("braid-leaf-outofseg",
		[]net.IP{net.ParseIP("192.0.2.1")}, nil))
	// 段内 IP + 恶意 DNS SAN:DNS 面执法(T10 审计确认的 fail-open 半边
	// ——IP 过验 + DNS 旁路曾是 ACCEPT;DNS 哨兵域后必拒)。
	mustReject("in-segment IP with hostile DNS SAN", signLeaf("braid-leaf-dns-hijack",
		[]net.IP{net.ParseIP("198.19.0.5")}, []string{"evil.example"}))
	// 纯 DNS SAN(无 IP):同面纯形态,必拒。
	mustReject("pure DNS SAN leaf", signLeaf("braid-leaf-dns-only",
		nil, []string{"metadata.google.internal"}))
}

// TestSandboxCACertBuildContext context 成员契约:写出目录恰一个成员、
// 名字与词形零漂移、无私钥材料、可被系统池加载路径消费。
func TestSandboxCACertBuildContext(t *testing.T) {
	dir := t.TempDir()
	m := generateSandboxCA(t, []net.IP{net.ParseIP("198.19.0.1")})
	p := m.writeCACertToContext(t, dir)

	if filepath.Base(p) != sandboxCACertFile {
		t.Fatalf("context member name = %q, want %q", filepath.Base(p), sandboxCACertFile)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read context dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != sandboxCACertFile {
		t.Fatalf("build context members = %d entries, want exactly [%s]", len(entries), sandboxCACertFile)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	if !bytes.Equal(raw, m.CACertPEM) {
		t.Fatal("context file is not the CA cert PEM verbatim")
	}
	if bytes.Contains(raw, []byte("PRIVATE KEY")) {
		t.Fatal("private key material leaked into build context (§3: CA cert only)")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		t.Fatal("context file not loadable via AppendCertsFromPEM (system-pool loader path)")
	}
}
