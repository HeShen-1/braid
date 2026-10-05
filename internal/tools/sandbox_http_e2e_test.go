package tools

// hermetic http e2e(T10.1 T10 起,T10.2 换装到 egress 拓扑;真 docker):
// http 经生产装配构造器全链路跑通。验收面 = 基线 §2 #11(网络姿态按载体
// 拆分)+ #12(env 从零构造)+ T10.2 egress 默认拒绝面的「失控即红」证明:
//   - 正向:容器 → 网关代理 CONNECT 隧道 → 段内目标,TLS 信任仅来自 e2e
//     镜像构建期 CA append(私 CA 单通道,基线 §3;代理零明文可见);
//   - 负向(SSRF 三连):禁段端点(环回/metadata/私网 IP)在**代理侧**
//     IP 门被拒,哨兵经 reason-phrase 标记 → 隧道拨号器 → 信封跨进程
//     保真(errors.Is 命中 runner 既有映射)——检查只在容器内(或只在
//     宿主)在此红;
//   - egress 默认拒绝(网络层结构性):内网直连非网关地址必败;网关上
//     的代理是唯一可达面(对照组 connected);
//   - 非白名 CONNECT:代理 403 host 标记(egress-dial 探针原始断言);
//   - file_read 姿态(network none)对可达地址必败:none 漂移时同信封
//     同 CA 会成功到达 serve,失败断言即翻红;
//   - dump-env:容器 env 键集合恰 {PATH,LANG,HOSTNAME,HOME},worker
//     ambient 凭证键值双缺席(白名单构造 = 从未放入;T10.2 代理地址走
//     stdin 信封不走 env——BRAID_* 家族结构性缺席含新键)。
//
// 拓扑(T10 裁决 + T10.2 换装):TLS server = 宿主侧 selftest serve 子进程,
// 绑槽位独占的 198.51.100.x/32 lo alias(alias 装配经 net=host + NET_ADMIN
// 容器执行 busybox ip——唯一前提 docker,本地/CI 同路径免 root;槽位分
// IP 消并行 lane 竞态,198.51.100.0/24 RFC 5737 TEST-NET-2 是守卫禁段外
// 放行段——旧段 198.18.0.0/15(198.19.x)已随 D-2026-10-05-ssrf-five-ranges
// 五段扩禁入禁,T18.1 重选本段)。T10.2:egress 内网 = 槽位独占 --internal
// 网络 braid-sbx-egress-t<slot>,代理 = 测试进程内真 EgressProxy 绑网关
// (allow 集经 EgressAllowFromEndpoints 从下方 endpoints 单源派生),
// http 容器唯一可达面 = 网关代理(本机 docker 29.1.3 实证:internal 网
// 容器→网关可达、→他宿主 IP/外网结构性不可达)。镜像 sandboxBuildE2EImage
// 恒重建(CA ephemeral),tag = e2e-t<slot>(不覆写 prod 缺省引用;收尾
// rmi + 删自加 alias + rm 自建网络 + serve stdin 关闭)。watch:fake-ip 型
// TUN 池多选 198.18.0.0/15 档(本机 198.18.0.0/30 在档),与 TEST-NET-2
// 零交集——原「TUN 覆盖选段」顾虑随重选结构性消除;present 探测仍保留
// (双接口同址它检不出,极端撞段时 add 响亮败)。将来守卫再扩禁触本段,
// TestSandboxHermeticCIDRAllowed 先红,重选段同条款跟进。

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// serveErrSink 互斥包裹的 stderr 缓冲:exec 的 stderr 拷贝 goroutine 与
// 测试 Fatal 路径的读取并发,bytes.Buffer 非并发安全(PR #85 review,
// 竞态面仅此诊断读取)。
type serveErrSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *serveErrSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *serveErrSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestSandboxE2EHTTP 真 docker 全链路 hermetic http e2e(子用例共享同一
// 套装配:镜像/alias/serve/载体)。
func TestSandboxE2EHTTP(t *testing.T) {
	sandboxDockerGate(t)
	slot := sandboxSlotNumber(t)
	if os.Getenv("BRAID_TEST_REDIS_DB") == "" {
		t.Logf("BRAID_TEST_REDIS_DB unset → slot 0 = main-repo lane; worktrees must export their slot (parallel convention)")
	}
	e2eTag := "braid-tool-sandbox:e2e-t" + strconv.Itoa(slot)
	aliasIP := sandboxHermeticAliasIP(t)

	// 零残留基线快照(T9 同款差分法与已知残余声明)。
	before := sandboxLabeledContainerIDs(t)

	// e2e 镜像:恒重建(CA ephemeral,复用缓存镜像携带旧 CA 会静默断
	// TLS 配对);tag 守卫拒 prod 缺省引用。构建成功即注册清理(零间隙:
	// 装配期 Fatal 不滞留镜像,滞留 tag 下轮恒重建覆盖亦可自愈)。
	m := generateSandboxCA(t, []net.IP{aliasIP})
	sandboxBuildE2EImage(t, e2eTag, m.CACertPEM)
	t.Cleanup(func() {
		_ = exec.Command(sandboxDockerBin, "rmi", e2eTag).Run()
	})

	// lo alias:幂等装配,仅自加者清理(所有权 = 本进程 add 成功;复用
	// 非自加残留 = 前轮崩溃泄漏或同槽位公约违例,登记观测且不删——删
	// 别人的 alias 会打断其在飞测试;泄漏残留由宿主重启回收,接受)。
	// 同槽位并发双跑 = add 撞 File exists 响亮红(公约违例的正确形态)。
	if sandboxLoAliasPresent(t, e2eTag, aliasIP) {
		t.Logf("lo alias %s already present (leaked from a crashed run or a same-slot sibling); reusing without cleanup", aliasIP)
	} else {
		if err := sandboxLoAlias(e2eTag, "add", aliasIP); err != nil {
			t.Fatalf("add lo alias %s: %v", aliasIP, err)
		}
		if !sandboxLoAliasPresent(t, e2eTag, aliasIP) {
			t.Fatalf("lo alias %s not present after add", aliasIP)
		}
		// 删除清理后注册(rmi 先注册 ⇒ LIFO 下 del 先于 rmi 执行——del
		// 经 e2e 镜像跑,镜像必须还在)。
		t.Cleanup(func() {
			if err := sandboxLoAlias(e2eTag, "del", aliasIP); err != nil {
				t.Logf("cleanup: del lo alias %s: %v", aliasIP, err)
			}
		})
	}

	// serve 子进程(宿主侧 helper 二进制):port 0 内核指派,listening
	// 行读回实际端口;生命周期与测试绑定(stdin 关闭即优雅退)。
	helperDir := t.TempDir()
	sandboxBuildHelperBinary(t, helperDir)
	certPath := filepath.Join(helperDir, "leaf.crt")
	keyPath := filepath.Join(helperDir, "leaf.key")
	if err := os.WriteFile(certPath, m.LeafCertPEM, 0o600); err != nil {
		t.Fatalf("write leaf cert: %v", err)
	}
	if err := os.WriteFile(keyPath, m.LeafKeyPEM, 0o600); err != nil {
		t.Fatalf("write leaf key: %v", err)
	}
	serveCmd := exec.Command(filepath.Join(helperDir, "braid-sandbox-exec"),
		"selftest", "serve", net.JoinHostPort(aliasIP.String(), "0"), certPath, keyPath)
	serveIn, err := serveCmd.StdinPipe()
	if err != nil {
		t.Fatalf("serve stdin pipe: %v", err)
	}
	serveOut, err := serveCmd.StdoutPipe()
	if err != nil {
		t.Fatalf("serve stdout pipe: %v", err)
	}
	// serve stderr 收进缓冲:listen/证书装载失败时 listening 行读不到,
	// 缓冲里的病因随 Fatal 一并呈现(诊断面,审计处置)。
	serveErrBuf := &serveErrSink{}
	serveCmd.Stderr = serveErrBuf
	if err := serveCmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	t.Cleanup(func() {
		_ = serveIn.Close()
		done := make(chan struct{})
		go func() {
			_ = serveCmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = serveCmd.Process.Kill()
		}
	})
	line, err := bufio.NewReader(serveOut).ReadString('\n')
	if err != nil {
		t.Fatalf("read serve listening line: %v\nserve stderr: %s", err, serveErrBuf.String())
	}
	listenHost, port, err := net.SplitHostPort(strings.TrimSpace(strings.TrimPrefix(line, "listening ")))
	if err != nil || listenHost != aliasIP.String() {
		t.Fatalf("serve listening line = %q, want host %s (err=%v)", line, aliasIP, err)
	}
	epBase := "https://" + net.JoinHostPort(aliasIP.String(), port)

	// 宿主侧装配自检:CA 池客户端直拨段内地址。此步绿 = server/CA/alias
	// 三件套配对正确——后续子用例若红,病灶收窄到容器路径。
	hostPool := x509.NewCertPool()
	if !hostPool.AppendCertsFromPEM(m.CACertPEM) {
		t.Fatal("e2e CA not loadable into pool")
	}
	hostClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: hostPool},
	}}
	t.Cleanup(hostClient.CloseIdleConnections)
	sanityURL := epBase + "/assembly-sanity"
	resp, err := hostClient.Get(sanityURL)
	if err != nil {
		t.Fatalf("host-side assembly sanity GET %s: %v", sanityURL, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read sanity body: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != sandboxServeMarker+" /assembly-sanity" {
		t.Fatalf("sanity resp = %d %q, want 200 %q", resp.StatusCode, body, sandboxServeMarker+" /assembly-sanity")
	}

	// T10.2 egress 拓扑:槽位独占 --internal 网络 + 测试进程内真代理绑
	// 网关(allow 集 = EgressAllowFromEndpoints 对下方 endpoints 单源派生,
	// 生产禁段 ipForbidden 不注入——禁段端点必须在代理侧被拒)。
	netName := "braid-sbx-egress-t" + strconv.Itoa(slot)
	gwIP := sandboxEgressE2ENetwork(t, netName)
	eps := map[string]string{
		"hermetic": epBase,
		"loopback": "https://127.0.0.1:1",
		"metadata": "https://169.254.169.254",
		"gateway":  "https://172.17.0.1",
	}
	allow, err := EgressAllowFromEndpoints(eps)
	if err != nil {
		t.Fatalf("EgressAllowFromEndpoints: %v", err)
	}
	proxyLn, err := net.Listen("tcp", net.JoinHostPort(gwIP, "0"))
	if err != nil {
		t.Fatalf("bind egress proxy on gateway %s: %v", gwIP, err)
	}
	t.Cleanup(func() { _ = proxyLn.Close() })
	proxy := NewEgressProxy(EgressConfig{Allow: allow})
	serveErr := make(chan error, 1)
	go func() { serveErr <- proxy.Serve(proxyLn) }()
	proxyURL := &url.URL{Scheme: "http", Host: proxyLn.Addr().String()}

	// 载体:多别名 endpoints(hermetic 正向 + 三条禁段负向)+ egress
	// 网络位与代理地址(worker 生产装配同构:addr 进信封,不进容器 env)。
	carrier, err := newSandboxCarrierHTTP(sandboxConfig{
		Docker:         true,
		Image:          e2eTag,
		Memory:         "256m",
		CPUs:           "0.5",
		PidsLimit:      64,
		MaxConcurrency: 8,
		EgressNetwork:  netName,
		EgressAddr:     proxyURL,
		Owner:          sandboxTestOwner(),
	}, eps, newSandboxLimiter(8))
	if err != nil {
		t.Fatalf("newSandboxCarrierHTTP: %v", err)
	}

	call := func(t *testing.T, name, path string) (Result, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return carrier.Run(ctx, Call{
			Inputs:         map[string]any{"name": name, "path": path},
			Token:          "scoped-token",
			IdempotencyKey: "att:t10:1",
		})
	}
	cfg := sandboxConfig{
		Docker:    true,
		Image:     e2eTag,
		Memory:    "256m",
		CPUs:      "0.5",
		PidsLimit: 64, MaxConcurrency: 8,
		EgressNetwork: netName,
		Owner:         sandboxTestOwner(),
	}

	t.Run("hermetic https via build-time CA trust", func(t *testing.T) {
		// 红条件:CA 未注入镜像 → TLS 验证失败(tool_failed);路由断/
		// alias 缺 → 拨号失败;信封漂移 → 输出词形漂移。body 非 JSON →
		// 工具输出落 body_text;path 回显证明 base_url 与 DSL path 拼接。
		res, err := call(t, "hermetic", "/hermetic/probe")
		if err != nil {
			t.Fatalf("hermetic call: %v", err)
		}
		var out struct {
			Status   json.Number `json:"status"`
			BodyText string      `json:"body_text"`
		}
		if err := json.Unmarshal(res.Output, &out); err != nil {
			t.Fatalf("unmarshal http output %s: %v", res.Output, err)
		}
		if out.Status.String() != "200" {
			t.Fatalf("status = %s, want 200 (full output %s)", out.Status, res.Output)
		}
		if out.BodyText != sandboxServeMarker+" /hermetic/probe" {
			t.Fatalf("body_text = %q, want %q", out.BodyText, sandboxServeMarker+" /hermetic/probe")
		}
	})

	// SSRF 禁段三连:环回、metadata 链路本地、docker0 网关(宿主网络面
	// 私网 IP)。T10.2 换装后执法点 = 代理侧 IP 门(容器不再直拨目标):
	// 拒答 reason-phrase 标记 → 隧道拨号器 egressDenialError → 哨兵跨
	// 信封保真——标记映射/信封码/宿主重抛任一环漂移,errors.Is 即不命中
	// (分类漂移即红,基线 §4d 回归锚)。
	for _, c := range []struct{ alias, why string }{
		{"loopback", "loopback 127.0.0.1"},
		{"metadata", "link-local metadata 169.254.169.254"},
		{"gateway", "docker0 gateway 172.17.0.1 (private)"},
	} {
		t.Run("ssrf blocked: "+c.why, func(t *testing.T) {
			if _, err := call(t, c.alias, "/probe"); !errors.Is(err, ErrSSRFBlocked) {
				t.Fatalf("err = %v, want ErrSSRFBlocked (proxy-side IP gate, sentinel crossing envelope)", err)
			}
		})
	}

	// T10.2 验收:非白名 CONNECT 被拒(代理默认拒绝面;egress-dial 探针
	// 原始断言,不经工具策略层——证明的是网络层策略面本身)。
	t.Run("non-allowlisted CONNECT denied by proxy", func(t *testing.T) {
		argv, err := buildSandboxRunArgv(cfg, "t102-connect-deny-t"+strconv.Itoa(slot), "http", nil)
		if err != nil {
			t.Fatalf("build argv: %v", err)
		}
		if argv[len(argv)-1] != "http" {
			t.Fatalf("builder tail contract drifted: %q", argv[len(argv)-1])
		}
		argv = append(argv[:len(argv)-1], "selftest", "egress-dial", proxyURL.Host, "evil.example:443")
		out, err := exec.Command(sandboxDockerBin, argv...).Output()
		if err != nil {
			t.Fatalf("docker run (egress-dial): %v\n%s", err, out)
		}
		if line, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "HTTP/1.1 "); !ok || !strings.HasPrefix(line, "403 "+egressReasonHostBlocked) {
			t.Fatalf("egress-dial out = %q, want 403 %s", strings.TrimSpace(string(out)), egressReasonHostBlocked)
		}
	})

	// T10.2 验收:egress 内网直连结构性拒绝(默认拒绝的网络层证明)——
	// 容器拨段内目标(hermetic alias,宿主持有)必败;对照:同容器拨
	// 网关上的代理端口连通(网关 = 唯一可达面)。
	t.Run("direct egress blocked, gateway proxy is the only face", func(t *testing.T) {
		probe := func(t *testing.T, name, addr, wantPrefix string) {
			t.Helper()
			argv, err := buildSandboxRunArgv(cfg, "t102-"+name+"-t"+strconv.Itoa(slot), "http", nil)
			if err != nil {
				t.Fatalf("build argv: %v", err)
			}
			argv = append(argv[:len(argv)-1], "selftest", "raw-dial", addr)
			out, err := exec.Command(sandboxDockerBin, argv...).Output()
			if err != nil {
				t.Fatalf("docker run (%s): %v\n%s", name, err, out)
			}
			if !strings.HasPrefix(strings.TrimSpace(string(out)), wantPrefix) {
				t.Fatalf("%s out = %q, want prefix %q", name, out, wantPrefix)
			}
		}
		probe(t, "direct", net.JoinHostPort(aliasIP.String(), port), "failed ")
		probe(t, "gateway", proxyURL.Host, "connected ")
	})

	t.Run("file_read posture is network none", func(t *testing.T) {
		// argv 手术:builder 出 file_read 形态(--network none + 全硬化
		// 旗标,golden 锚不破),尾参换 http 跑同一信封会话——载体永不
		// 发出该组合(工具↔网络位钉死),但 none 网容器内拨可达段内地址
		// 必败正是被测面:none 漂移为 bridge 时,同信封 + 同 CA 镜像会
		// 成功到达 serve(失控即红:失败断言翻绿路径)。红路径的环境前置
		// = 宿主对 docker0 入向不 DROP(bridge 漂移下 200 才可达);CI
		// runner 无 INPUT 防火墙恒成立,本机开 ufw 者漂移不红——已知
		// 限制,声明于此。
		argv, err := buildSandboxRunArgv(cfg, "t10-none-posture-t"+strconv.Itoa(slot), "file_read", nil)
		if err != nil {
			t.Fatalf("build argv: %v", err)
		}
		// 手术防漂移:builder 尾部契约 = [entrypoint, image, tool],漂移
		// (尾部再 append 旗标)下替换的不再是 tool——helper 收 file_read
		// + http 信封会以「tool mismatch」tool_failed 信封静默过本用例
		// 的弱断言,尾参检查在此先行响亮红(审计处置)。
		if argv[len(argv)-1] != "file_read" {
			t.Fatalf("builder tail contract drifted: argv tail = %q, want %q (surgery target moved)", argv[len(argv)-1], "file_read")
		}
		argv[len(argv)-1] = "http"
		envelope, err := EncodeSandboxRequest(SandboxRequest{
			Tool:           "http",
			Config:         json.RawMessage(mustJSON(t, map[string]any{"endpoints": map[string]string{"hermetic": epBase}})),
			Token:          "scoped-token",
			IdempotencyKey: "att:t10:none",
			Inputs:         json.RawMessage(mustJSON(t, map[string]any{"name": "hermetic", "path": "/none-probe"})),
		})
		if err != nil {
			t.Fatalf("encode envelope: %v", err)
		}
		cmd := exec.Command(sandboxDockerBin, argv...)
		cmd.Stdin = strings.NewReader(string(envelope) + "\n")
		var runStderr bytes.Buffer
		cmd.Stderr = &runStderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("docker run (none posture): %v\n%s\ndocker stderr: %s", err, out, runStderr.String())
		}
		_, derr := DecodeSandboxResponse(out)
		if derr == nil {
			t.Fatal("dial from network=none container succeeded (posture drift none → bridge?)")
		}
		// 失败形态钉死 = tool_failed 信封(容器内拨号错误的业务分类):
		// 信封层失败(空 stdout/畸形 JSON/未知码)各自携带不同错误词形,
		// 前缀断言使它们无法伪装成「合法的 none 网失败」(审计处置);
		// 三哨兵全排除——段内地址是放行侧,任何哨兵 = 策略层错位。
		if !strings.HasPrefix(derr.Error(), "tools: sandbox tool_failed:") {
			t.Fatalf("err = %v, want tool_failed envelope (raw in-container dial failure)", derr)
		}
		for _, sentinel := range []error{ErrSSRFBlocked, ErrHostBlocked, ErrPathBlocked} {
			if errors.Is(derr, sentinel) {
				t.Fatalf("err = %v, want non-sentinel dial failure (segment address is allowlisted)", derr)
			}
		}
	})

	t.Run("container env constructed from zero", func(t *testing.T) {
		// argv 手术尾参 → selftest dump-env:载体永不发 selftest,但 env
		// 从零构造是 argv/镜像的属性,沿 builder 全硬化旗标测得才有效。
		// 假主凭证注入本测试进程 env——容器内键值双缺席是「结构性不可
		// 达 = 从未放入」的负向证明(基线 §5)。
		fakes := map[string]string{
			"BRAID_TOOL_TOKEN_SECRET": "fake-master-secret",
			"BRAID_LLM_API_KEY":       "fake-llm-key",
			"BRAID_TEST_MYSQL_DSN":    "fake:dsn@tcp(10.9.9.9)/db",
			"HTTP_PROXY":              "http://fake-proxy.example:3128",
		}
		for k, v := range fakes {
			t.Setenv(k, v)
		}
		argv, err := buildSandboxRunArgv(cfg, "t10-dump-env-t"+strconv.Itoa(slot), "file_read", nil)
		if err != nil {
			t.Fatalf("build argv: %v", err)
		}
		// 手术防漂移(同 none-posture 子用例):尾参必须恰是 tool 位。
		if argv[len(argv)-1] != "file_read" {
			t.Fatalf("builder tail contract drifted: argv tail = %q, want %q (surgery target moved)", argv[len(argv)-1], "file_read")
		}
		argv = append(argv[:len(argv)-1], "selftest", "dump-env")
		var stderrBuf bytes.Buffer
		docker := exec.Command(sandboxDockerBin, argv...)
		docker.Stderr = &stderrBuf
		out, err := docker.Output()
		if err != nil {
			t.Fatalf("docker run (dump-env): %v\n%s\ndocker stderr: %s", err, out, stderrBuf.String())
		}
		var env map[string]string
		if err := json.Unmarshal(out, &env); err != nil {
			t.Fatalf("dump-env not a JSON object: %v (%s)", err, out)
		}
		// 正半边:两成员在场(PATH 基座提供,LANG 镜像 ENV——T8 漏、
		// 本实证红后补)。
		if !strings.Contains(env["PATH"], "/usr/local/bin") {
			t.Fatalf("PATH = %q, want container-standard PATH", env["PATH"])
		}
		if env["LANG"] != "C.UTF-8" {
			t.Fatalf("LANG = %q, want C.UTF-8 (image ENV, baseline §5)", env["LANG"])
		}
		// 键集合恰四:PATH/LANG + daemon 自管 HOSTNAME/HOME(平台成员,
		// 非 worker ambient;docker run 无法 unset HOSTNAME)。多任何
		// 成员即红——白名单构造的强度就在「恰好」。
		allowed := map[string]bool{"PATH": true, "LANG": true, "HOSTNAME": true, "HOME": true}
		if len(env) != len(allowed) {
			t.Fatalf("env members = %v, want exactly %v", env, allowed)
		}
		for k := range env {
			if !allowed[k] {
				t.Fatalf("unexpected env member %q (allowlist construct broken)", k)
			}
		}
		// 负半边:假凭证键与值双缺席;结构性:零 BRAID_* 键。
		for k, v := range fakes {
			if _, ok := env[k]; ok {
				t.Fatalf("worker ambient key %q leaked into container env", k)
			}
			for ek, ev := range env {
				if strings.Contains(ev, v) {
					t.Fatalf("credential value of %q present in env[%q]", k, ek)
				}
			}
		}
		for k := range env {
			if strings.HasPrefix(k, "BRAID_") {
				t.Fatalf("BRAID_* key %q in container env (structural invariant: family never enters)", k)
			}
		}
	})

	t.Run("zero residue", func(t *testing.T) {
		after := sandboxLabeledContainerIDs(t)
		var leaked []string
		for id := range after {
			if !before[id] {
				leaked = append(leaked, id)
			}
		}
		if len(leaked) > 0 {
			t.Fatalf("sandbox containers leaked past --rm: %v", leaked)
		}
	})
}

// sandboxSlotNumber Redis 测试槽位号(BRAID_TEST_REDIS_DB 语义即槽位,
// 组合发生在消费包 TestMain;此处只消费)。
func sandboxSlotNumber(t *testing.T) int {
	t.Helper()
	raw := os.Getenv("BRAID_TEST_REDIS_DB")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > 15 {
		t.Fatalf("BRAID_TEST_REDIS_DB %q invalid as slot number (want 0..15)", raw)
	}
	return n
}

// sandboxEgressE2ENetwork 幂等确保槽位独占的 --internal egress 测试网并
// 返回网关 IP。所有权 = 本进程 create 成功才注册 rm 清理(复用非自建
// 残留 = 前轮崩溃泄漏或同槽位公约违例,登记且不动——lo alias 同款纪律);
// 复用网络必须验 Internal=true(非 internal 复用 = 拒绝姿态被拆毁)。
func sandboxEgressE2ENetwork(t *testing.T, name string) string {
	t.Helper()
	created := false
	if err := exec.Command(sandboxDockerBin, "network", "inspect", name).Run(); err != nil {
		if out, cerr := exec.Command(sandboxDockerBin, "network", "create", "--internal", name).CombinedOutput(); cerr != nil {
			// 竞态下他者已建:复检在场即续行(存在性由 inspect 裁决)。
			if ierr := exec.Command(sandboxDockerBin, "network", "inspect", name).Run(); ierr != nil {
				t.Fatalf("create egress network %s: %v\n%s", name, cerr, out)
			}
		} else {
			created = true
		}
	}
	if created {
		t.Cleanup(func() {
			if err := exec.Command(sandboxDockerBin, "network", "rm", name).Run(); err != nil {
				t.Logf("cleanup: rm egress network %s: %v", name, err)
			}
		})
	} else {
		t.Logf("egress network %s already present (leaked from a crashed run or same-slot sibling); reusing without cleanup", name)
	}
	internal, err := exec.Command(sandboxDockerBin, "network", "inspect", "--format", "{{.Internal}}", name).Output()
	if err != nil || strings.TrimSpace(string(internal)) != "true" {
		t.Fatalf("egress network %s not --internal (inspect: %v %q)", name, err, internal)
	}
	gwOut, err := exec.Command(sandboxDockerBin, "network", "inspect", "--format", "{{(index .IPAM.Config 0).Gateway}}", name).Output()
	if err != nil {
		t.Fatalf("read egress network gateway: %v", err)
	}
	gw := strings.TrimSpace(string(gwOut))
	if net.ParseIP(gw) == nil {
		t.Fatalf("egress network gateway %q not an IP", gw)
	}
	return gw
}

// sandboxHermeticAliasIP 槽位独占的 hermetic 地址(段内主机位 <slot+1>,
// 守卫放行侧):从 sandboxHermeticCIDR 单源派生——常量改段时取址随之走,
// 不留第二真源(联动锚 TestSandboxHermeticCIDRAllowed 同源钉住放行侧)。
// 每 lane 各拥各地址,alias 增删生命周期互不干扰。
func sandboxHermeticAliasIP(t *testing.T) net.IP {
	t.Helper()
	_, hermeticNet, err := net.ParseCIDR(sandboxHermeticCIDR)
	if err != nil {
		t.Fatalf("parse %s: %v", sandboxHermeticCIDR, err)
	}
	base := hermeticNet.IP.To4()
	if base == nil {
		t.Fatalf("hermetic CIDR %s must be IPv4", sandboxHermeticCIDR)
	}
	n := sandboxSlotNumber(t)
	if n+1 > 254 {
		t.Fatalf("slot %d cannot map into %s host octet", n, sandboxHermeticCIDR)
	}
	host := make(net.IP, 4)
	copy(host, base)
	host[3] = byte(n + 1) // /24 段内主机位取址(空间 .1..254,与全段锚同源)
	return host
}

// sandboxLoAlias 在宿主 netns 对 lo 执行别名增删:net=host + NET_ADMIN +
// uid 0 容器跑 busybox ip(免宿主 root;唯一前提 = docker,恰是本 e2e
// 既有门控,本地/CI 同路径)。镜像 = e2e tag(本测试内恒在场)。
func sandboxLoAlias(image, verb string, ip net.IP) error {
	out, err := exec.Command(sandboxDockerBin, "run", "--rm",
		"--network", "host", "--cap-add", "NET_ADMIN", "--user", "0:0",
		"--entrypoint", "/bin/sh", image, "-c",
		fmt.Sprintf("ip addr %s %s/32 dev lo", verb, ip)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip addr %s %s: %w\n%s", verb, ip, err, out)
	}
	return nil
}

// sandboxLoAliasPresent 报告宿主 lo 是否已持该地址(幂等装配的在场探测;
// CombinedOutput——探测容器自身失败时 stderr 随 Fatal 呈现,与 add/del
// 同款诊断面)。
func sandboxLoAliasPresent(t *testing.T, image string, ip net.IP) bool {
	t.Helper()
	out, err := exec.Command(sandboxDockerBin, "run", "--rm",
		"--network", "host", "--entrypoint", "/bin/sh", image, "-c",
		"ip -4 addr show dev lo").CombinedOutput()
	if err != nil {
		t.Fatalf("ip addr show dev lo: %v\n%s", err, out)
	}
	return strings.Contains(string(out), ip.String()+"/")
}
