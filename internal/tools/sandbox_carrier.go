package tools

// 沙箱载体运行时(T10.1,D-2026-09-28-t101):泛型容器载体实现 Tool,在
// BuiltinToolsFromEnv 装配缝内同名 wrap http/file_read(T6)——registry/
// LocalRunner/nodes 零改动。本文件是宿主侧执行力学:exec docker CLI(argv
// 只经 buildSandboxRunArgv 白名单构造)、stdin 信封下发、limitBuffer 收流、
// ctx 生命周期绑定与失败分类。容器内侧 = cmd/braid-sandbox-exec helper(T5)。
//
// 失败分类契约(基线 §6,runner.go 现行边界零改动):
//   - exit 0 + 完整信封 → 信封决定成败:三哨兵码重抛真哨兵(errors.Is 命中
//     runner 既有映射)、ok=true 透传 output;信封畸形/缺失/溢出 = 普通错误
//     → runner 默认 tool_failed。完整退出 0 是确定性证据:晚到的 ctx 取消
//     不改写信封分类(与 runner「哨兵先于 ctx 门」同序,结构性消除竞争)。
//   - run 途中 ctx 取消/超时 → ctx.Err() 链回裸 error = 基础设施失败,
//     attempt 留 RUNNING 由 asynq 重试;随后 docker stop -t 3 <name> 有界
//     清场(基线 #13,失败吞掉——残余窗 §9-6 已登记,由启动清扫兜底)。
//   - exit≠0 无信封 → 普通错误 → tool_failed 业务,不重试(D-t101 #4:
//     确定性超限重试同损);消息带 exit 码与 OOMKilled 归因单字段。
//   - 本载体不造 infra 哨兵通道:daemon 故障在运行时与工具失败同桶
//     (tool_failed),部署性故障由 SandboxProbe 启动前移为拒启(D-t101 #7)。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// sandboxStopGrace/sandboxStopBudget:cancel 窗有界清场(基线 #13)——
	// stop 给容器内 PID1 3s 宽限,总预算 5s 封顶(stop 自身挂起不拖垮调用方)。
	sandboxStopGrace  = 3 * time.Second
	sandboxStopBudget = 5 * time.Second
	// sandboxInspectTimeout:失败归因 inspect 的单往返预算(best-effort,
	// 失败只降级归因精度,不改分类)。
	sandboxInspectTimeout = 2 * time.Second
	// sandboxEnvelopeHeadroom:stdout 收流上界 = 产出契约 16MiB + 信封
	// 结构开销。响应编码器已钉死 SetEscapeHTML(false)
	// (EncodeSandboxResponse,防 RawMessage 6x HTML 转义放大),headroom
	// 只需覆盖键名/包装残差。
	sandboxEnvelopeHeadroom = 4 << 10
	// sandboxProbeTimeout:Probe 子命令单往返预算(部署性检查,快败快启)。
	sandboxProbeTimeout = 5 * time.Second
	// 沙箱基线旗标的 daemon 版本下限:--init 与 --mount type=bind 均需
	// 17.06+(残余 #8:版本漂移由 Probe 下限检查拦截)。
	sandboxMinDaemonMajor = 17
	sandboxMinDaemonMinor = 6
)

// sandboxDockerBin docker CLI 固定名:词表不设覆盖键(GATE 1 拍板,不留
// 生产 env 面);测试经 PATH 注入同名 shim(StartupCheck)或置换载体字段
// (newTestCarrier)。
const sandboxDockerBin = "docker"

// sandboxCarrier 是容器沙箱载体:每次 Run 一个一次性 docker run 容器
// (--rm,容器名 braid-sbx-<tool>-<rand>)。构造保持纯配置惰性——docker
// 二进制解析/daemon/镜像检查全部在 Probe/Run 期(api 进程构造零 docker
// 接触的结构保证)。
type sandboxCarrier struct {
	name       string // 同名 wrap 的 builtin 名(http|file_read)
	cfg        sandboxConfig
	toolConfig json.RawMessage // 信封 Config 原料(T6 装配期序列化,容器内重建同构工具)
	dirs       []string        // file_read 挂载候选(白名单原值);http 恒空
	docker     string          // docker 二进制(默认 "docker" 走 PATH;仅测试注入 fake shim)
	limiter    *sandboxLimiter // 并发信号量,装配期单例跨工具共享
}

// 编译期接口锚:载体在 Tool 接口之下同名替换(registry.go 注释契约)。
var _ Tool = (*sandboxCarrier)(nil)

// newSandboxCarrier 构造载体。limiter 为 nil panic(装配缺陷启动即现,
// NewLocalRunner 同款惯例);工具名必须在沙箱词表内、配置必须 docker 模式
// ——装配期响亮败,不留静默降级面。
func newSandboxCarrier(name string, cfg sandboxConfig, toolConfig json.RawMessage, dirs []string, limiter *sandboxLimiter) (*sandboxCarrier, error) {
	if limiter == nil {
		panic("tools: nil sandboxLimiter")
	}
	if !cfg.Docker {
		return nil, fmt.Errorf("tools: sandbox carrier %q requires docker mode (BRAID_TOOL_SANDBOX=docker)", name)
	}
	if _, ok := sandboxSandboxableTools[name]; !ok {
		return nil, fmt.Errorf("tools: sandbox carrier tool %q not in sandboxable vocabulary (want http|file_read)", name)
	}
	if name != "file_read" && len(dirs) > 0 {
		return nil, fmt.Errorf("tools: sandbox carrier %q does not take dirs (file_read only)", name)
	}
	return &sandboxCarrier{
		name:       name,
		cfg:        cfg,
		toolConfig: toolConfig,
		dirs:       dirs,
		docker:     sandboxDockerBin,
		limiter:    limiter,
	}, nil
}

// newSandboxCarrierHTTP / newSandboxCarrierFileRead 装配缝构造器(T6):
// 把装配期已解析的工具配置序列化为信封 Config 原料,词形与 helper
// rebuildSandboxTool 消费面同构({"endpoints":…,"proxy":…}/{"dirs":…})。
// 编码复用 marshalSandboxEnvelope(SetEscapeHTML(false),信封纪律内层同款):
// dirs 里合法的 &<> 保持字面量,不被默认 Marshal 落成 \uXXXX 词形。
// proxy 字段(T10.2)源自 cfg.EgressAddr:worker 装配恒在(api 侧 nil =
// omitted;api 不执行工具,信封永不发出)。
func newSandboxCarrierHTTP(cfg sandboxConfig, eps map[string]string, limiter *sandboxLimiter) (*sandboxCarrier, error) {
	proxy := ""
	if cfg.EgressAddr != nil {
		proxy = cfg.EgressAddr.String()
	}
	tc, err := marshalSandboxEnvelope(struct {
		Endpoints map[string]string `json:"endpoints"`
		Proxy     string            `json:"proxy,omitempty"`
	}{Endpoints: eps, Proxy: proxy})
	if err != nil {
		return nil, fmt.Errorf("marshal http endpoints: %w", err)
	}
	return newSandboxCarrier("http", cfg, tc, nil, limiter)
}

// newSandboxCarrierFileRead:toolConfig 携 dirs 原样词形(不筛不存在目录
// ——挂载筛选在 mountDirs 逐调用做,信封 config 全量原词形,容器内
// helper 按原配置重建;基线 #3「同路径同构」/GATE 1 #9);载体 dirs 与
// 信封同源(白名单原值),挂载与授权零漂移。装配期预检(T6 审计三项
// 收口):值域取 Clean 后形态(mountDirs 同款变换;尾斜杠等冗余形态不
// 误拒)对齐 argv 构造器的 sandboxMountDirRe;Clean 等价类去重(重复
// 词形 off 模式无害,docker 下逐调用产重复 dst → daemon 拒建容器);
// 条目在场且非目录即拒(单文件白名单本地可服务,docker 永不挂载 →
// 每调用 tool_failed;不存在 = 放行,挂载晚于启动语义保留)——一律
// 装配期响亮败,不留「每次调用 argv 构造响亮败」的运行期地雷。
func newSandboxCarrierFileRead(cfg sandboxConfig, dirs []string, limiter *sandboxLimiter) (*sandboxCarrier, error) {
	seen := make(map[string]struct{}, len(dirs))
	for _, d := range dirs {
		c := filepath.Clean(d)
		if !sandboxMountDirRe.MatchString(c) {
			return nil, fmt.Errorf("dir %q outside docker mount value domain (clean form %q: want absolute path, charset [A-Za-z0-9_./-], no comma/space)", d, c)
		}
		if _, dup := seen[c]; dup {
			return nil, fmt.Errorf("dir %q duplicates earlier entry (clean form %q: off mode tolerates duplicate roots, docker rejects duplicate mount dst per call)", d, c)
		}
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return nil, fmt.Errorf("dir %q exists but is not a directory (docker mode bind-mounts dirs only; a single-file entry local mode serves would fail every container call)", d)
		}
		seen[c] = struct{}{}
	}
	tc, err := marshalSandboxEnvelope(struct {
		Dirs []string `json:"dirs"`
	}{Dirs: dirs})
	if err != nil {
		return nil, fmt.Errorf("marshal file_read dirs: %w", err)
	}
	return newSandboxCarrier("file_read", cfg, tc, dirs, limiter)
}

// Name 满足 Tool 接口(与被 wrap 的 builtin 同名——registry 同名替换)。
func (c *sandboxCarrier) Name() string { return c.name }

// Run 执行一次容器化工具调用。错误语义见文件头注释。
func (c *sandboxCarrier) Run(ctx context.Context, call Call) (Result, error) {
	// 并发信号量先于一切 docker 接触(D-t101 #7:fan-out × 每调用一容器
	// 的 daemon 负载上界);等待期 ctx 取消 = infra 错误链回。
	if err := c.limiter.acquire(ctx); err != nil {
		return Result{}, fmt.Errorf("tools: %s: %w", c.name, err)
	}
	defer c.limiter.release()

	name, err := sandboxContainerName(c.name)
	if err != nil {
		return Result{}, fmt.Errorf("tools: %s: %w", c.name, err)
	}
	argv, err := buildSandboxRunArgv(c.cfg, name, c.name, c.mountDirs())
	if err != nil {
		return Result{}, fmt.Errorf("tools: %s: %w", c.name, err)
	}

	// 信封族编码器而非裸 json.Marshal(PR #85 review):裸 Marshal 对
	// <>& 做 \u00XX 转义(1B→6B),8MiB inputs 最坏放大 48MiB 打爆
	// helper 16MiB 请求上限,off 模式同载荷成功 = 跨模式分叉。
	inputs, err := marshalSandboxEnvelope(call.Inputs)
	if err != nil {
		return Result{}, fmt.Errorf("tools: %s: marshal inputs: %w", c.name, err)
	}
	req := SandboxRequest{
		Tool:           c.name,
		Config:         c.toolConfig,
		Token:          call.Token,
		IdempotencyKey: call.IdempotencyKey,
		Inputs:         inputs,
	}
	if dl, ok := ctx.Deadline(); ok {
		// 相对剩余毫秒(time.Until 单调钟):信封期限与宿主 ctx 同锚,
		// 时钟步进不解耦两侧;已到期钳到 1ms(0 经 omitempty = 无期限)。
		ms := time.Until(dl).Milliseconds()
		if ms < 1 {
			ms = 1
		}
		req.DeadlineMs = ms
	}
	in, err := EncodeSandboxRequest(req)
	if err != nil {
		return Result{}, fmt.Errorf("tools: %s: encode envelope: %w", c.name, err)
	}

	cmd := exec.CommandContext(ctx, c.docker, argv...)
	cmd.Stdin = bytes.NewReader(in)
	stdout := &limitBuffer{limit: DefaultMaxToolOutput + sandboxEnvelopeHeadroom}
	stderr := &limitBuffer{limit: maxStderr}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = DefaultWaitDelay
	runErr := cmd.Run()

	if runErr == nil {
		// 完整退出 0:信封是唯一事实源(哨兵/成功原样上抛;此分支不咨询
		// ctx——完整证据已存在,晚到的取消只是环境态)。
		if stdout.overflow {
			return Result{}, fmt.Errorf("tools: %s: sandbox envelope exceeds %d bytes", c.name, DefaultMaxToolOutput)
		}
		out := bytes.TrimSpace(stdout.Bytes())
		if len(out) == 0 {
			return Result{}, fmt.Errorf("tools: %s: sandbox envelope empty (exit 0 without stdout)", c.name)
		}
		res, err := DecodeSandboxResponse(out)
		if err != nil {
			return Result{}, err
		}
		return res, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// ctx 优先于 ExitError/ErrWaitDelay 呈现(照抄 command.go 语义:
		// kill 收尾噪声的返回值不是语义本体);有界 stop 清场后原样上抛。
		c.stopContainer(name)
		return Result{}, fmt.Errorf("tools: %s: %w", c.name, ctxErr)
	}
	// 容器级失败且无信封:daemon 故障/资源超限 kill/helper panic 一律普通
	// 错误 → runner 默认 tool_failed;exit 码 + OOMKilled 归因机器可读。
	return Result{}, fmt.Errorf("tools: %s: sandbox container %s: %v: exit=%d oom_killed=%s: %s",
		c.name, name, runErr, exitCode(runErr), c.inspectOOMKilled(name), clamp(stderr.String(), 512))
}

// mountDirs 逐调用筛除不存在的白名单目录(GATE 1 #9):docker 对缺失 bind
// source 会以 root 自动建目录,必须避免;目录缺失 → 不挂载,但信封 config
// 保留原词形全量——容器内 helper 按原配置打开失败 → tool_failed,与进程内
// EvalSymlinks 失败同语义。
func (c *sandboxCarrier) mountDirs() []string {
	if len(c.dirs) == 0 {
		return nil
	}
	mounted := make([]string, 0, len(c.dirs))
	for _, d := range c.dirs {
		// Clean 对齐 helper 词形门(NewFileReadTool 的 lex 根即 Clean 后
		// 形态,基线 #3「同路径同构」;尾斜杠等冗余形态不因双轨值域误拒)。
		d = filepath.Clean(d)
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			mounted = append(mounted, d)
		}
	}
	return mounted
}

// sandboxContainerName 每调用一次性容器名:braid-sbx-<tool>-<16 hex>
// (crypto/rand;碰撞由 docker 重名响亮失败兜底,值域经 argv 构造器再校验)。
func sandboxContainerName(tool string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("container name entropy: %w", err)
	}
	return "braid-sbx-" + tool + "-" + hex.EncodeToString(b[:]), nil
}

// stopContainer cancel 窗有界清场:docker stop -t 3 <name>,总预算 5s;
// 失败吞掉(孤儿容器残余窗 = 基线 §9-6 登记残余,由 helper watchdog 与
// worker 启动清扫 SandboxSweep 兜底)——ctx 分类是语义本体,stop 只是卫生。
func (c *sandboxCarrier) stopContainer(name string) {
	sctx, cancel := context.WithTimeout(context.Background(), sandboxStopBudget)
	defer cancel()
	_ = exec.CommandContext(sctx, c.docker, "stop", "-t", strconv.Itoa(int(sandboxStopGrace/time.Second)), name).Run()
}

// inspectOOMKilled 失败归因的机器可读单字段(D-t101 #4):docker inspect
// --format {{.State.OOMKilled}} <name>。--rm 下 daemon 已移除退出容器时
// 返回 "unknown"(exit=137 仍是可靠信号);best-effort——inspect 自身
// 失败不改分类,只降级归因精度。
func (c *sandboxCarrier) inspectOOMKilled(name string) string {
	ictx, cancel := context.WithTimeout(context.Background(), sandboxInspectTimeout)
	defer cancel()
	out, err := exec.CommandContext(ictx, c.docker, "inspect", "--format", "{{.State.OOMKilled}}", name).Output()
	if err != nil {
		return "unknown"
	}
	switch strings.TrimSpace(string(out)) {
	case "true":
		return "true"
	case "false":
		return "false"
	default:
		return "unknown"
	}
}

// exitCode 从 exec 错误提取退出码(信号终止/非 ExitError → -1,错误链文本
// 自带 "signal: killed" 细节)。
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// sandboxLimiter 并发容器上界(BRAID_TOOL_SANDBOX_MAX_CONCURRENCY,默认
// 8):T9.1 fan-out × 每调用一容器的 daemon 负载闸;装配期单例,跨工具
// 共享(同一 daemon 预算)。
type sandboxLimiter struct {
	slots chan struct{}
}

// newSandboxLimiter 构造信号量;n < 1 panic(装配缺陷启动即现)。
func newSandboxLimiter(n int) *sandboxLimiter {
	if n < 1 {
		panic("tools: sandboxLimiter capacity must be >= 1")
	}
	return &sandboxLimiter{slots: make(chan struct{}, n)}
}

// acquire 占一个槽;等待期 ctx 取消 → ctx.Err()(infra 链回,不占槽)。
// 占槽后补一道 ctx 终判:双就绪(槽空 + 已取消)时 select 随机分支,已
// 取消的 ctx 必须恒先拒绝——归还槽位、零 docker 接触,且保持「acquire
// 成功 ⇔ 调用方负责 release」不变式。
func (l *sandboxLimiter) acquire(ctx context.Context) error {
	select {
	case l.slots <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-l.slots
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release 归还一个槽(仅 acquire 成功后的 defer 路径调用)。
func (l *sandboxLimiter) release() { <-l.slots }

// SandboxProbe 是 worker 启动部署性前移检查(D-t101 #7:mode=docker 下
// docker bin 缺失/daemon 不可达/镜像缺失 = 拒启,绝不静默回落 local)。
// 三步:bin 解析 → daemon 探活(docker info 单往返,兼取 ServerVersion 做
// 版本下限检查)→ 镜像在场(image inspect;离线口径 D-t72 同纪律:daemon
// 永不 pull,缺镜像错误携带构建命令)。由 cmd/worker 启动装配后调用。
func SandboxProbe(ctx context.Context, dockerBin, image string) error {
	if _, err := exec.LookPath(dockerBin); err != nil {
		return fmt.Errorf("tools: sandbox probe: docker binary %q not found in PATH (BRAID_TOOL_SANDBOX=docker requires a docker CLI; unset to keep local execution)", dockerBin)
	}

	infoCtx, cancel := context.WithTimeout(ctx, sandboxProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(infoCtx, dockerBin, "info", "--format", "{{.ServerVersion}}").Output()
	if err != nil {
		return fmt.Errorf("tools: sandbox probe: docker daemon unreachable (docker info: %v)", err)
	}
	version := strings.TrimSpace(string(out))
	major, minor, perr := parseDaemonVersion(version)
	if perr != nil {
		return fmt.Errorf("tools: sandbox probe: docker daemon version %q unparsable: %v", version, perr)
	}
	if major < sandboxMinDaemonMajor || (major == sandboxMinDaemonMajor && minor < sandboxMinDaemonMinor) {
		return fmt.Errorf("tools: sandbox probe: docker daemon version %q below minimum 17.06 for baseline flags --init/--mount", version)
	}

	imgCtx, cancel := context.WithTimeout(ctx, sandboxProbeTimeout)
	defer cancel()
	if err := exec.CommandContext(imgCtx, dockerBin, "image", "inspect", image).Run(); err != nil {
		return fmt.Errorf("tools: sandbox probe: image %q not present locally (daemon never pulls); build from repo root, then restart worker: CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o braid-sandbox-exec ./cmd/braid-sandbox-exec && docker build -f deploy/sandbox.Dockerfile -t %s . (canonical command: deploy/README.md §7)", image, image)
	}
	return nil
}

// parseDaemonVersion 解析 ServerVersion("24.0.7"/"17.06.2-ce")为
// (major, minor):取前两个点分段,patch 与 -ce/-ee 后缀忽略;分段非数字
// 或段数不足 = 错误(版本不可读不猜,响亮拒启)。
func parseDaemonVersion(v string) (int, int, error) {
	fields := strings.Split(v, ".")
	if len(fields) < 2 {
		return 0, 0, fmt.Errorf("no major.minor in %q", v)
	}
	major, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("major in %q: %w", v, err)
	}
	minor, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("minor in %q: %w", v, err)
	}
	return major, minor, nil
}

// SandboxSweep 是 worker 启动按 label 清扫(braid.sandbox=1 注册面,基线
// #13:cancel 窗僵尸与 kill -9 孤儿的兜底)。ps 枚举失败 = 响亮错误;单个
// rm 失败跳过继续(best-effort,下次启动再扫)。返回成功移除的容器 id,
// 由调用方记录。在 SandboxProbe 之后调用(probe 已保证 daemon 可达)。
func SandboxSweep(ctx context.Context, dockerBin string) ([]string, error) {
	out, err := exec.CommandContext(ctx, dockerBin, "ps", "-aq", "--filter", "label=braid.sandbox=1").Output()
	if err != nil {
		return nil, fmt.Errorf("tools: sandbox sweep: docker ps: %v", err)
	}
	var removed []string
	for _, id := range strings.Fields(string(out)) {
		if err := exec.CommandContext(ctx, dockerBin, "rm", "-f", id).Run(); err == nil {
			removed = append(removed, id)
		}
	}
	return removed, nil
}

// sandboxSweepTimeout:Sweep 在接线函数内的总预算——probe 三往返各有
// 5s 快败预算,若 sweep 拿裸生命周期 ctx,daemon「probe 应答后挂死」
// 窗口会让 worker 启动无限期悬挂(无错误无日志,比拒启更糟);有界后
// 悬挂退化为响亮拒启,重启循环随 daemon 恢复自愈(基线 #7 部署性故障
// 前移口径)。var 供测试收缩(sandboxHelperReadTimeout 同款)。
var sandboxSweepTimeout = 30 * time.Second

// SandboxStartupCheck 是 worker 启动接线(T10.1 T6,基线 #7/#13):按
// BRAID_TOOL_SANDBOX 词表决策——off(含未设)= 无操作零 docker 接触
// (dev 栈照跑);docker = SandboxProbe 部署性前移(缺 bin/daemon 不可达/
// 镜像缺失/版本低于下限 = 拒启,绝不静默回落 local)+ SandboxSweep label
// 清扫(cancel 窗僵尸与 kill -9 孤儿的兜底;probe 已保证 daemon 可达,
// sweep 失败同样响亮败)。返回成功移除的容器 id 供调用方记录。api 进程
// 不调用本函数(不执行工具,BuiltinToolsFromEnv 构造已 docker-free)。
//
// 失败归因:探测/清扫窗内收到停机信号时,exec 错误呈 "signal: killed"
// (exec.Wait 优先 ExitError,错误链不含 Canceled)——先归因回 ctx.Err(),
// worker main 的正常停机出口(errors.Is(context.Canceled) → exit 0)才
// 不被误判为 fatal 部署失败(carrier Run 的 ctx 优先归因同款)。
func SandboxStartupCheck(ctx context.Context, getenv func(string) string) ([]string, error) {
	cfg, err := sandboxConfigFromEnv(getenv)
	if err != nil {
		return nil, err
	}
	if !cfg.Docker {
		return nil, nil
	}
	if err := SandboxProbe(ctx, sandboxDockerBin, cfg.Image); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	sctx, cancel := context.WithTimeout(ctx, sandboxSweepTimeout)
	defer cancel()
	removed, err := SandboxSweep(sctx, sandboxDockerBin)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr // 停机信号落在 sweep 窗,同上归因。
		}
		if sctx.Err() != nil {
			return nil, fmt.Errorf("tools: sandbox sweep: budget %s exceeded (daemon wedged after probe?): %w", sandboxSweepTimeout, err)
		}
		return nil, err
	}
	return removed, nil
}
