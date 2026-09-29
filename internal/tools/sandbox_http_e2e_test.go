package tools

// hermetic http e2e(T10.1 T10,真 docker):http 经生产装配构造器全链路
// 跑通。验收面 = 基线 §2 #11(网络姿态按载体拆分)+ #12(env 从零构造)
// 的「失控即红」证明:
//   - 正向:容器内守卫客户端拨段内地址,TLS 信任仅来自 e2e 镜像构建期
//     CA append(私 CA 单通道,基线 §3);
//   - 负向:SSRF 禁段(环回/metadata/bridge 网关)在容器内拨号层被拒,
//     哨兵跨信封保真——host-only 检查(DNS rebinding TOCTOU)在此红;
//   - file_read 姿态(network none)对可达地址必败:none 漂移为 bridge
//     时同信封同 CA 会成功到达 serve,失败断言即翻红;
//   - dump-env:容器 env 键集合恰 {PATH,LANG,HOSTNAME,HOME},worker
//     ambient 凭证键值双缺席(白名单构造 = 从未放入)。
//
// 拓扑(T10 裁决):TLS server = 宿主侧 selftest serve 子进程,绑槽位
// 独占的 198.19.0.x/32 lo alias(alias 装配经 net=host + NET_ADMIN 容器
// 执行 busybox ip——唯一前提 docker,本地/CI 同路径免 root;槽位分 IP
// 消并行 lane 竞态,198.18.0.0/15 是守卫禁段外放行段,D-2026-09-23-
// ssrf-extra-ranges 维持放行)。镜像 sandboxBuildE2EImage 恒重建(CA
// ephemeral),tag = e2e-t<slot>(不覆写 prod 缺省引用;收尾 rmi + 删自加
// alias + serve stdin 关闭)。watch:fake-ip 型 TUN 代理若自配 198.18.0.0/15
// 档(本机现为 198.18.0.0/30,与 198.19.0.x 不相交)会覆盖本段选段——
// present 探测只查 lo 检不出双接口同址,届时需要重选段(与「守卫扩禁
// 同步重选段」同一联动条款)。

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

	// 载体:多别名 endpoints(hermetic 正向 + 三条禁段负向)。
	carrier, err := newSandboxCarrierHTTP(sandboxConfig{
		Docker:         true,
		Image:          e2eTag,
		Memory:         "256m",
		CPUs:           "0.5",
		PidsLimit:      64,
		MaxConcurrency: 8,
	}, map[string]string{
		"hermetic": epBase,
		"loopback": "https://127.0.0.1:1",
		"metadata": "https://169.254.169.254",
		"gateway":  "https://172.17.0.1",
	}, newSandboxLimiter(8))
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

	// SSRF 禁段三连:环回(容器自身)、metadata 链路本地、bridge 网关
	// (= 宿主网络面)。守卫若只在宿主侧检查(或被容器内剥除),拨号会
	// 以 connection refused / no route 落 tool_failed 而非哨兵——分类
	// 漂移即红;gateway 别名另证「拨号确实发生在容器内」(宿主进程拨
	// 172.17.0.1 恒 refused,与哨兵可区分)。
	for _, c := range []struct{ alias, why string }{
		{"loopback", "container loopback"},
		{"metadata", "link-local metadata 169.254.169.254"},
		{"gateway", "bridge gateway (host netns surface)"},
	} {
		t.Run("ssrf blocked: "+c.why, func(t *testing.T) {
			if _, err := call(t, c.alias, "/probe"); !errors.Is(err, ErrSSRFBlocked) {
				t.Fatalf("err = %v, want ErrSSRFBlocked (in-container guard enforcement)", err)
			}
		})
	}

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

// sandboxHermeticAliasIP 槽位独占的 hermetic 地址 198.19.0.<slot+1>(段内、
// 守卫放行侧):每 lane 各拥各地址,alias 增删生命周期互不干扰。
func sandboxHermeticAliasIP(t *testing.T) net.IP {
	t.Helper()
	n := sandboxSlotNumber(t)
	if n+1 > 254 {
		t.Fatalf("slot %d cannot map into 198.19.0.x last octet", n)
	}
	return net.IPv4(198, 19, 0, byte(n+1)).To4()
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
