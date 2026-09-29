package tools

// 沙箱镜像构建面(T10.1 T8):deploy/sandbox.Dockerfile 的机械锚(注入
// 拓扑不变量)、docker 实机构建验证,以及 T9/T10 e2e 消费的镜像装配
// helper。
//
// buildIfMissing 形态(2026-09-29 用户批准):独立测试入口,Probe 不内建
// ——基线 §3 离线口径已锁「daemon 永不 pull,缺镜像 = 响亮失败携带构建
// 命令」,启动期 auto-build 同属部署面副作用(worker 运行处未必有 build
// context);运维路径 = Probe 错误消息的两步命令 + deploy/README.md §7。
// 语义取「本测试进程内恒构建一次」而非跨运行 if-missing:daemon 里昨日
// 代码的滞留镜像会让 e2e 静默测到旧 helper 二进制——与 ephemeral CA 材料
// 零滞留同口径。测试侧 tag 按 Redis 槽位后缀隔离:daemon 是机器级共享
// 资源,并行 worktree 各自的全量测试若都覆写 braid-tool-sandbox:1 会互踩。
//
// 机械锚防的是意外漂移:锚红 = 有人动了注入拓扑;显式变更(如企业私 CA
// 落 prod stage,deploy/README.md §7 通道)随变更同步更新锚与 README,
// 不视为锚的失效。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	// sandboxE2ETarget hermetic e2e 构建目标(deploy/sandbox.Dockerfile 的
	// AS 名,锚测试断言 Dockerfile 侧同名;T10 e2e 消费)。
	sandboxE2ETarget = "sandbox-e2e"

	// sandboxGateTimeout 测试门探活预算:socket 激活的 daemon 冷启动可达
	// 数秒,worker 启动期的 5s 快败口径(sandboxProbeTimeout)不适用测试
	// 侧——冷启动误判 = 本地静默 skip 假绿。
	sandboxGateTimeout = 30 * time.Second

	// sandboxRuntimeBase 运行层基座逐字钉死值(§3「平行 FROM 基座」):
	// 两运行段 FROM 同此基座——digest 漂移与「e2e FROM prod 镜像引用」
	// 的继承形态都在此即红。与 deploy/sandbox.Dockerfile 单点镜像同源,
	// 改基座 = 同 commit 双改(锚红会点名)。
	sandboxRuntimeBase = "alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8"
)

// sandboxRepoRoot 测试视角的仓库根(go test CWD = internal/tools)。
func sandboxRepoRoot() string { return filepath.Join("..", "..") }

// sandboxE2EBuildcheckTag T8 构建验证用一次性 tag:按槽位后缀隔离
// (sandboxTestImageTag 同款公约——daemon 是机器级共享资源,固定 tag
// 在并行 wave 下构建覆盖/rmi 竞态产生假红,rmi 竞态分支还会意外触发
// Docker Hub pull;T10 审计确认后槽位化)。只证 target 可构建 + append
// 落地,收尾 rmi。
func sandboxE2EBuildcheckTag(t *testing.T) string {
	t.Helper()
	return "braid-tool-sandbox:e2e-buildcheck-t" + strconv.Itoa(sandboxSlotNumber(t))
}

// readSandboxDockerfile 读镜像定义(锚测试与构建 helper 共用真源)。
func readSandboxDockerfile(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sandboxRepoRoot(), "deploy", "sandbox.Dockerfile"))
	if err != nil {
		t.Fatalf("read deploy/sandbox.Dockerfile: %v", err)
	}
	return string(raw)
}

// sandboxDockerfileInstrs 把 Dockerfile 归约为指令行序列,镜像 BuildKit
// 解析顺序(T8 审计实证:先合并后剥注释会让「\ 结尾注释行吞掉下一指令」
// ——Docker 执行该指令、锚看不到,负向锚被静默绕过):
//   - 注释行(# 开头,允许前导空白)整行丢弃,含尾反斜杠同样丢弃——
//     不开启也不并入续行链;续行链中间的注释行不并入文本、链保持;
//   - 续行(\ 结尾)拼下一非注释物理行;空行终止链;
//   - 指令头大小写归一为大写:Dockerfile 指令不区分大小写而锚按大写
//     词形匹配,不归一即负向锚 fail-open(T10 审计实证:`env
//     SSL_CERT_FILE=...` 小写可整体绕过 ENV 白名单——信任源覆盖直击
//     §5 红线;归一在 parser 层做,全部锚同时受益,TestSandboxInstrs
//     CaseNormalization 钉死)。
func sandboxDockerfileInstrs(t *testing.T, raw string) []string {
	t.Helper()
	var instrs []string
	var buf []string
	flush := func() {
		if len(buf) > 0 {
			fields := strings.Fields(strings.Join(buf, " "))
			// 空白压缩 + 首 token 大写:Dockerfile 词形自由度(空格/tab/
			// 大小写)全部归一到锚词形——`env\tSSL_CERT_FILE=...` 与
			// `ENV  LANG=...` 等拼写不再可能从任一锚的匹配面外滑过。
			instrs = append(instrs, strings.ToUpper(fields[0])+" "+strings.Join(fields[1:], " "))
			buf = nil
		}
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if strings.HasPrefix(line, "#") {
			continue
		}
		if line == "" {
			flush()
			continue
		}
		if cont := strings.HasSuffix(line, "\\"); cont {
			line = strings.TrimSuffix(line, "\\")
			buf = append(buf, line)
			continue
		}
		buf = append(buf, line)
		flush()
	}
	flush()
	return instrs
}

// TestSandboxDockerfileInstrsCaseNormalization parser 归一锚(变异转正):
// 小写/混写指令头必须归一为大写——归一被删(锚退回大小写敏感)时,
// `env SSL_CERT_FILE=/attacker.crt` 这类小写攻击行将不再命中 ENV 白名单
// 锚,负向防线 fail-open(T10 审计三视角确认的绕道,本测试即其永久化)。
func TestSandboxDockerfileInstrsCaseNormalization(t *testing.T) {
	raw := "from alpine:3.22@sha256:dead\n\n# comment\tenv trick\nenv SSL_CERT_FILE=/attacker.crt\nENV LANG=C.UTF-8\ncopy\tbin /usr/local/bin/x\n"
	instrs := sandboxDockerfileInstrs(t, raw)
	want := []string{
		"FROM alpine:3.22@sha256:dead",
		"ENV SSL_CERT_FILE=/attacker.crt",
		"ENV LANG=C.UTF-8",
		"COPY bin /usr/local/bin/x", // tab 分隔同样归一首 token
	}
	if len(instrs) != len(want) {
		t.Fatalf("instrs = %q, want %q", instrs, want)
	}
	for i := range want {
		if instrs[i] != want[i] {
			t.Fatalf("instrs[%d] = %q, want %q (full %q)", i, instrs[i], want[i], instrs)
		}
	}
	// 白名单消费面复演:归一后小写攻击行必须命中「ENV 前缀 + 非白名单整行」
	// 的拒绝分支——即 TestSandboxDockerfileAnchors 的 ENV 锚能看见它。
	for _, l := range instrs {
		if strings.HasPrefix(l, "ENV ") && l != "ENV LANG=C.UTF-8" {
			if !strings.Contains(l, "SSL_CERT_FILE") {
				t.Fatalf("unexpected non-whitelisted ENV line shape: %q", l)
			}
		}
	}
}

// TestSandboxDockerfileAnchors 机械锚:钉死基线 §3 的 Dockerfile 拓扑
// 不变量。成员名出现在注释里不参与断言(注释先于指令分析剥除)。
func TestSandboxDockerfileAnchors(t *testing.T) {
	instrs := sandboxDockerfileInstrs(t, readSandboxDockerfile(t))

	// stage 切分:FROM 行开段;AS 名取段名,prod 缺省 target 无名。
	type stage struct {
		name string
		from int
	}
	var stages []stage
	for i, l := range instrs {
		if !strings.HasPrefix(l, "FROM ") {
			continue
		}
		if !strings.Contains(l, "@sha256:") {
			t.Fatalf("FROM line not digest-pinned: %q", l)
		}
		st := stage{from: i}
		if as := strings.Index(l, " AS "); as >= 0 {
			st.name = strings.TrimSpace(l[as+4:])
		}
		stages = append(stages, st)
	}
	if len(stages) != 3 {
		t.Fatalf("stage count = %d, want 3 (build / %s / prod): %v", len(stages), sandboxE2ETarget, stages)
	}
	if stages[0].name != "build" {
		t.Fatalf("first stage = %q, want build (from-source discipline holder)", stages[0].name)
	}
	if stages[1].name != sandboxE2ETarget {
		t.Fatalf("second stage = %q, want AS %s (hermetic e2e target)", stages[1].name, sandboxE2ETarget)
	}
	// prod = 最后一个 stage 且无名 = 缺省 target 零注入点(SandboxProbe
	// 提示命令不带 --target,缺省构建必须直接可用)。
	if last := stages[len(stages)-1]; last.name != "" {
		t.Fatalf("last stage = %q, want anonymous prod (default target must be the zero-injection stage)", last.name)
	}
	// 平行 FROM 基座:两运行段基座逐字同钉(继承形态/基座漂移在此即红,
	// 免把基座分歧误归因到 append 语义)。
	baseOf := func(l string) string {
		b := strings.TrimPrefix(l, "FROM ")
		if as := strings.Index(b, " AS "); as >= 0 {
			b = b[:as]
		}
		return strings.TrimSpace(b)
	}
	for _, idx := range []int{1, 2} {
		if got := baseOf(instrs[stages[idx].from]); got != sandboxRuntimeBase {
			t.Fatalf("runtime stage %d base = %q, want pinned %q (parallel-from-the-same-base)", idx, got, sandboxRuntimeBase)
		}
	}

	e2e := instrs[stages[1].from:stages[2].from]
	prod := instrs[stages[2].from:]
	build := instrs[stages[0].from:stages[1].from]
	line := func(prefix string, section []string, what string) int {
		for i, l := range section {
			if strings.HasPrefix(l, prefix) {
				return i
			}
		}
		t.Fatalf("no %s in section (want %q)", what, prefix)
		return -1
	}

	// CA 成员只存在于 sandbox-e2e 段:全指令扫描,区段外出现即红
	// (prod 零注入点)。
	for i, l := range instrs {
		if !strings.Contains(l, sandboxCACertFile) {
			continue
		}
		if i < stages[1].from || i >= stages[2].from {
			t.Fatalf("%s appears outside the %s section: %q", sandboxCACertFile, sandboxE2ETarget, l)
		}
	}
	caCopy := line("COPY "+sandboxCACertFile+" ", e2e, "CA cert COPY line")

	// append 语义:凡触碰系统信任束的指令必须是「>> 束路径」邻接形态
	// (只增不减;clobber 整体替换会静默断全部公网 https 出站;操作符与
	// 路径须同语句邻接,防无关 >> 凑数)。
	const bundlePath = "/etc/ssl/certs/ca-certificates.crt"
	const bundleAppend = ">> " + bundlePath
	bundleAt := -1
	for i, l := range e2e {
		if !strings.Contains(l, bundlePath) {
			continue
		}
		if !strings.Contains(l, bundleAppend) {
			t.Fatalf("bundle-touching instruction is not an adjacent append: %q", l)
		}
		bundleAt = i
	}
	if bundleAt < 0 {
		t.Fatalf("no %s append in %s section", bundlePath, sandboxE2ETarget)
	}
	// 次序:CA COPY → append → USER 65532(bundle 属 root,USER 后 append
	// 即 Permission denied)。
	userAt := line("USER 65532", e2e, "USER 65532 in e2e section")
	if caCopy >= bundleAt || bundleAt >= userAt {
		t.Fatalf("e2e section order wrong: CA COPY@%d, append@%d, USER@%d (want COPY < append < USER)", caCopy, bundleAt, userAt)
	}

	entrypoint := `"` + sandboxEntrypoint + `"`
	runtimeCommon := func(section []string, what string) {
		line("COPY braid-sandbox-exec ", section, "helper binary COPY in "+what)
		if !strings.Contains(strings.Join(section, "\n"), "ENTRYPOINT ["+entrypoint+"]") {
			t.Fatalf("%s ENTRYPOINT not pinned to argv constant %s", what, entrypoint)
		}
		// 运行段恰一条 USER 且 = 65532(后置 USER 降级非 root 姿态即红)。
		users := 0
		for _, l := range section {
			if strings.HasPrefix(l, "USER ") {
				users++
				if l != "USER 65532" {
					t.Fatalf("%s USER line = %q, want USER 65532", what, l)
				}
			}
			// 镜像内烘焙 ENV 是 argv 白名单之外的平行 env 通道,整行精确
			// 白名单(§5 两成员:PATH 基座自带无需声明,LANG 唯一放行词形
			// ——T10 dump-env 实证 T8 缺此行后补)。精确到整行:Dockerfile
			// ENV 支持单行多键,前缀匹配放不住 SSL_CERT_FILE/DIR 类信任源
			// 覆盖搭车进容器(旁路构建期 CA append 单通道)。
			if strings.HasPrefix(l, "ENV ") && l != "ENV LANG=C.UTF-8" {
				t.Fatalf("%s bakes ENV %q (only exact line \"ENV LANG=C.UTF-8\" is allowed; trust-source overrides like SSL_CERT_FILE/DIR must not enter via image)", what, l)
			}
		}
		if users != 1 {
			t.Fatalf("%s USER 65532 count = %d, want exactly 1", what, users)
		}
	}
	runtimeCommon(e2e, "e2e section")
	runtimeCommon(prod, "prod section")

	// prod 零注入:唯一 COPY = 宿主二进制(无 --from = builder 非 prod
	// 祖先,保 COPY-only 测试路径;多余 COPY = 注入面扩张即红)、零束
	// 操作、无源构建。
	copies := 0
	for _, l := range prod {
		if strings.HasPrefix(l, "COPY ") {
			copies++
			if !strings.HasPrefix(l, "COPY braid-sandbox-exec ") || strings.Contains(l, "--from") {
				t.Fatalf("prod COPY must be exactly the context binary copy: %q", l)
			}
		}
		if strings.Contains(l, "--from") || strings.Contains(l, bundlePath) {
			t.Fatalf("prod stage must be zero-injection and context-COPY only: %q", l)
		}
		if strings.Contains(l, "CGO_ENABLED") || strings.Contains(l, "go build") {
			t.Fatalf("prod stage must not build from source (host-built + COPY-only): %q", l)
		}
	}
	if copies != 1 {
		t.Fatalf("prod COPY count = %d, want exactly 1 (helper binary)", copies)
	}

	// build 段:与根 Dockerfile 同构建纪律三元组(锚防旗标漂移)。
	joined := strings.Join(build, "\n")
	for _, flag := range []string{"CGO_ENABLED=0", "-trimpath", "-ldflags='-s -w'"} {
		if !strings.Contains(joined, flag) {
			t.Fatalf("build stage lost root-Dockerfile build discipline (missing %q):\n%s", flag, joined)
		}
	}
}

// sandboxGateRequire 门控矩阵的纯决策核:BRAID_TEST_REQUIRE_SANDBOX 显式
// 优先,未设继承 BRAID_TEST_REQUIRE_STACK(GATE 1 决策 #5,零 ci.yml
// 触碰);返回 "1" = 必须真跑(不可达即 Fatalf),其余值 = 本地 skip。
// 纯函数化 = 矩阵可表驱动单测(基线 §7/D-t101 #5 承诺,T10 审计后兑现)。
func sandboxGateRequire(getenv func(string) string) string {
	require := getenv("BRAID_TEST_REQUIRE_SANDBOX")
	if require == "" {
		require = getenv("BRAID_TEST_REQUIRE_STACK")
	}
	return require
}

// TestSandboxGateRequireMatrix 门控矩阵四象限表驱动:显式要求 / 未设继承
// / 显式关闭优先于继承 / 双未设。矩阵漂移(如继承丢失 = CI 静默 skip
// 假绿)在此红。
func TestSandboxGateRequireMatrix(t *testing.T) {
	for _, c := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"explicit require", map[string]string{"BRAID_TEST_REQUIRE_SANDBOX": "1", "BRAID_TEST_REQUIRE_STACK": "1"}, "1"},
		{"inherits stack when sandbox unset", map[string]string{"BRAID_TEST_REQUIRE_STACK": "1"}, "1"},
		{"explicit off overrides stack require", map[string]string{"BRAID_TEST_REQUIRE_SANDBOX": "0", "BRAID_TEST_REQUIRE_STACK": "1"}, "0"},
		{"both unset (local default)", map[string]string{}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := sandboxGateRequire(envOf(c.env)); got != c.want {
				t.Fatalf("require = %q, want %q", got, c.want)
			}
		})
	}
}

// sandboxGateSkipOrFail 门控裁决:REQUIRE=1(CI)→ Fatalf(CI 绿 = 沙箱
// 构建真跑);否则 Skipf。
func sandboxGateSkipOrFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if sandboxGateRequire(os.Getenv) == "1" {
		t.Fatalf("sandbox image test gated off in CI (docker must be reachable): "+format, args...)
	}
	t.Skipf(format, args...)
}

// sandboxDockerGate docker bin/daemon 可达性门(不可达 = skip/CI Fatalf;
// 探活命令与 SandboxProbe 同款,预算取测试口径——冷启动 daemon 不误判)。
func sandboxDockerGate(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(sandboxDockerBin); err != nil {
		sandboxGateSkipOrFail(t, "docker binary not in PATH: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sandboxGateTimeout)
	defer cancel()
	if err := exec.CommandContext(ctx, sandboxDockerBin, "info", "--format", "{{.ServerVersion}}").Run(); err != nil {
		sandboxGateSkipOrFail(t, "docker daemon unreachable within %s (cold-start?): %v", sandboxGateTimeout, err)
	}
}

// sandboxBuildHelperBinary 宿主构建 braid-sandbox-exec(与 Dockerfile
// build stage 同旗标)落入 ctxDir;测试路径 = 宿主构建 + COPY-only
// docker build(免拉 golang 镜像,CI 20min 预算友好)。go 工具链 = go
// test 运行环境自带。
func sandboxBuildHelperBinary(t *testing.T, ctxDir string) {
	t.Helper()
	out := filepath.Join(ctxDir, "braid-sandbox-exec")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", out, "./cmd/braid-sandbox-exec")
	cmd.Dir = sandboxRepoRoot()
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build braid-sandbox-exec: %v\n%s", err, out)
	}
}

// sandboxImagePresent 镜像在场探测(命令同 SandboxProbe 第三步)。
func sandboxImagePresent(t *testing.T, image string) bool {
	t.Helper()
	return exec.Command(sandboxDockerBin, "image", "inspect", image).Run() == nil
}

// sandboxDockerBuild COPY-only 构建:context = ctxDir(宿主产物成员),
// Dockerfile 取仓库内真源;target 空 = 缺省(prod)。失败消息携带构建输出。
func sandboxDockerBuild(t *testing.T, tag, target, ctxDir string) {
	t.Helper()
	args := []string{"build", "-f", filepath.Join(sandboxRepoRoot(), "deploy", "sandbox.Dockerfile"), "-t", tag}
	if target != "" {
		args = append(args, "--target", target)
	}
	args = append(args, ctxDir)
	out, err := exec.Command(sandboxDockerBin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker build (tag=%s target=%q): %v\n%s", tag, target, err, out)
	}
}

// sandboxEnsuredImages 本测试进程内已构建的 tag(包内测试串行,无锁):
// 恒构建一次,跨 go test 进程不复用 daemon 滞留镜像(旧 helper 二进制
// 静默漂移,见文件头)。
var sandboxEnsuredImages = map[string]bool{}

// sandboxTestImageTag prod 形态的测试侧 tag:按 Redis 测试槽位后缀隔离
// (BRAID_TEST_REDIS_DB = worktree 槽位号,repo 并行公约的既有判别键,
// 缺省 0 = 主仓)。daemon 是机器级共享资源,不同 worktree 的全量测试
// 各自重建 braid-tool-sandbox:1 会互踩;槽位后缀使各 lane 独占各 tag。
func sandboxTestImageTag() string {
	slot := os.Getenv("BRAID_TEST_REDIS_DB")
	if slot == "" {
		slot = "0"
	}
	return "braid-tool-sandbox:t" + slot
}

// sandboxEnsureImage prod 形态镜像装配(T9/T10 e2e 消费):进程内首调用
// 构建缺省 target(prod = Dockerfile 最后 stage,零注入),之后在场即跳过。
func sandboxEnsureImage(t *testing.T, image string) {
	t.Helper()
	sandboxDockerGate(t)
	if sandboxEnsuredImages[image] && sandboxImagePresent(t, image) {
		return
	}
	ctxDir := t.TempDir()
	sandboxBuildHelperBinary(t, ctxDir)
	sandboxDockerBuild(t, image, "", ctxDir)
	sandboxEnsuredImages[image] = true
}

// sandboxNormalizeImageRef docker 引用规范化的近似(docker.io/library/
// 前缀剥离):守卫须按「解析后同引用」比较,familiar name 的别名拼写
// (docker.io/braid-tool-sandbox:1 等)与缺省引用落同一 daemon RepoTag。
func sandboxNormalizeImageRef(ref string) string {
	for _, p := range []string{"index.docker.io/", "docker.io/"} {
		ref = strings.TrimPrefix(ref, p)
	}
	return strings.TrimPrefix(ref, "library/")
}

// sandboxValidateE2ETag e2e 构建 tag 守卫(纯函数):拒空、拒 prod 缺省
// 引用(按规范化后比较)——ephemeral CA 镜像覆写缺省引用后会滞留 daemon
// 被 prod Probe 放行(基线 §3 点名失败形态)。
func sandboxValidateE2ETag(tag string) error {
	if tag == "" {
		return fmt.Errorf("tools: sandbox e2e image tag empty")
	}
	if sandboxNormalizeImageRef(tag) == sandboxNormalizeImageRef(sandboxImageDefault) {
		return fmt.Errorf("tools: sandbox e2e image must not overwrite prod default ref %q (ephemeral CA lingering in the daemon would pass the prod probe)", sandboxImageDefault)
	}
	return nil
}

// sandboxBuildE2EImage --target sandbox-e2e 恒重建(CA 是 ephemeral 材料,
// 复用缓存镜像携带旧 CA 会静默断 TLS 配对):context = 宿主二进制 + CA
// 证书(唯一允许进 context 的材料,基线 §3)。收尾口径(T10 已定):
// tag = e2e-t<slot>(槽位隔离,不覆写 prod 缺省引用),调用方测试尾
// rmi——正式消费面 = sandbox_http_e2e_test.go。
func sandboxBuildE2EImage(t *testing.T, tag string, caPEM []byte) {
	t.Helper()
	if err := sandboxValidateE2ETag(tag); err != nil {
		t.Fatal(err)
	}
	sandboxDockerGate(t)
	ctxDir := t.TempDir()
	sandboxBuildHelperBinary(t, ctxDir)
	if err := os.WriteFile(filepath.Join(ctxDir, sandboxCACertFile), caPEM, 0o644); err != nil {
		t.Fatalf("write %s into build context: %v", sandboxCACertFile, err)
	}
	sandboxDockerBuild(t, tag, sandboxE2ETarget, ctxDir)
}

// TestSandboxValidateE2ETag tag 守卫单测(纯函数,无需 docker):别名
// 拼写(docker 规范化后同引用)同样拒。
func TestSandboxValidateE2ETag(t *testing.T) {
	rejected := []string{
		"",
		sandboxImageDefault,
		"library/" + sandboxImageDefault,
		"docker.io/" + sandboxImageDefault,
		"docker.io/library/" + sandboxImageDefault,
		"index.docker.io/library/" + sandboxImageDefault,
	}
	for _, tag := range rejected {
		if err := sandboxValidateE2ETag(tag); err == nil {
			t.Fatalf("tag %q accepted, want rejection", tag)
		}
	}
	for _, tag := range []string{"braid-tool-sandbox:e2e", "braid-tool-sandbox:t2", "localhost:5000/braid-tool-sandbox:e2e"} {
		if err := sandboxValidateE2ETag(tag); err != nil {
			t.Fatalf("tag %q rejected: %v", tag, err)
		}
	}
}

// TestSandboxImageBuild prod 形态构建 + 最小冒烟:缺省 target 直接可用
// (拓扑约束「缺省构建必须直接可用」的实证半边);ENTRYPOINT 接线 +
// 非 root 用户可执行由 selftest dump-env 出 JSON 对象即证(env 内容
// 断言属 T10 dump-env e2e,此处只证可执行——冒烟即在场复查)。
func TestSandboxImageBuild(t *testing.T) {
	tag := sandboxTestImageTag()
	sandboxEnsureImage(t, tag)
	out, err := exec.Command(sandboxDockerBin, "run", "--rm", tag, "selftest", "dump-env").CombinedOutput()
	if err != nil {
		t.Fatalf("container smoke (selftest dump-env) on %s: %v\n%s", tag, err, out)
	}
	var env map[string]any
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("dump-env output is not a JSON object: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command(sandboxDockerBin, "rmi", tag).Run()
	})
}

// sandboxCatBundle 读镜像内系统信任束(Go 系统池探测首项,基线 §3)。
func sandboxCatBundle(t *testing.T, image string) []byte {
	t.Helper()
	out, err := exec.Command(sandboxDockerBin, "run", "--rm", "--entrypoint", "cat", image, "/etc/ssl/certs/ca-certificates.crt").CombinedOutput()
	if err != nil {
		t.Fatalf("cat system bundle from %s: %v\n%s", image, err, out)
	}
	return out
}

// TestSandboxImageE2EAppend e2e target 构建期注入实证:同一 digest 基座
// 确定性成立 ⇒ e2e 束 == prod 束 ++ CA PEM 逐字节(append 落地 + 只增
// 不减双半边,一次断言)。两 tag 均一次性且各自收尾 rmi 不滞留 daemon;
// prod 槽位 tag 由本测试自行清场(不假设同包其它测试的消费序)。
func TestSandboxImageE2EAppend(t *testing.T) {
	m := generateSandboxCA(t, []net.IP{net.ParseIP("198.19.0.1")})
	prodTag := sandboxTestImageTag()
	sandboxEnsureImage(t, prodTag)
	t.Cleanup(func() {
		_ = exec.Command(sandboxDockerBin, "rmi", prodTag).Run()
	})
	prodBundle := sandboxCatBundle(t, prodTag)

	checkTag := sandboxE2EBuildcheckTag(t)
	sandboxBuildE2EImage(t, checkTag, m.CACertPEM)
	t.Cleanup(func() {
		_ = exec.Command(sandboxDockerBin, "rmi", checkTag).Run()
	})
	e2eBundle := sandboxCatBundle(t, checkTag)

	want := append(append([]byte{}, prodBundle...), m.CACertPEM...)
	if !bytes.Equal(want, e2eBundle) {
		t.Fatalf("e2e bundle != prod bundle ++ CA PEM (append-only broken or base diverged?): prod=%d e2e=%d ca=%d bytes",
			len(prodBundle), len(e2eBundle), len(m.CACertPEM))
	}
}
