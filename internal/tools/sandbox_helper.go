package tools

// 沙箱 helper 会话(T10.1 T5,D-2026-09-28-t101 #2/#3):容器内 braid-sandbox-exec
// 的可测核心(命令行薄壳在 cmd/braid-sandbox-exec,只做 signal ctx 与退出码
// 映射)。会话流:有界读 stdin 请求信封 → 按 Config 原料重建同名真工具
// (同编译单元策略代码零复制,SSRF 守卫随行在容器内拨号层执法)→ 执行 →
// EncodeSandboxResponse 单行信封回 stdout。
//
// 失败分类(基线 §6 的容器内侧,镜像 runner.go 既有排序):
//   - 哨兵先于 ctx 门:工具错误携带三哨兵 → 信封回传对应码(宿主
//     DecodeSandboxResponse 重抛真哨兵,runner 映射原样命中);
//   - ctx 取消/deadline(宿主 kill、docker stop 的 SIGTERM、信封
//     DeadlineUnix 到点)→ 无信封、返回 ctx 错误由 cmd 非零退出——deadline
//     的 infra 分类归宿在宿主载体,helper 不得以 tool_failed 信封改写;
//   - 其余工具错误 → tool_failed 信封;请求层失败(空/畸形/超限/工具名
//     不符/配置重建失败/inputs 非对象)→ tool_failed 信封(诊断可观测,
//     宿主侧落 error_message)。
//
// env 白名单(基线 §5):构造面 = 镜像 ENV 的 PATH/LANG 两项;docker
// daemon 另自管注入 HOSTNAME/HOME(平台成员,docker run 无 unset 通道,
// 非 worker ambient——恰四键契约由 T10 dump-env e2e 钉死,D-t101 #2 注记)。
// helper 结构性不读 ambient env(工具重建只消费信封 Config;唯一例外 =
// selftest dump-env 探针,它就是把 env 序列化给 e2e 断言的)。
//
// selftest 子命令(基线 §3:argv 词表可达但 carrier 永不发出,容器内
// 攻击者无新能力面;argv 构造器单测是回归锚):
//
//	braid-sandbox-exec selftest dump-env          全量 env → JSON 对象(凭证不可见 e2e)
//	braid-sandbox-exec selftest alloc <MiB>       8MiB 步长写满分配至目标/被杀(资源 e2e)
//	braid-sandbox-exec selftest serve <ip:port> <cert> <key>
//	                                              宿主侧 TLS server(T10 hermetic http e2e:
//	                                              显式 IP 绑定钉 hermetic 段,body = marker
//	                                              + path 回显,stdin EOF/SIGTERM 优雅退)
//	braid-sandbox-exec selftest spin <s>          忙等自旋至期限(#5 CPU 配额 e2e 的载荷)
//	braid-sandbox-exec selftest forkchain <d> <max>
//	                                              /proc/self/exe 自再执行链(#6 pids e2e:
//	                                              每代 fork+exec 子代并 wait;budget
//	                                              (cgroup 预算内收口)/max/EAGAIN 终止
//	                                              并上报 depth——非真炸弹,max 有界)
//	braid-sandbox-exec selftest fds <max>         open /dev/null 至 EMFILE/max(#7 fd e2e)
//	braid-sandbox-exec selftest spam-stderr <MiB> 定长块写 stderr 至目标(#14 日志封顶 e2e)
//	braid-sandbox-exec selftest zombies <n>       派生 n 个「孤儿子进程」后扫 /proc Z 状态
//	                                              (#15 tini 收割 e2e;zombie-child/exit0
//	                                              是其内部子模式)
//	braid-sandbox-exec selftest egress-dial <proxy> <authority>
//	                                              原始 CONNECT 探测(T10.2 e2e):拨代理、
//	                                              写 CONNECT、回读状态行原样打印(成功
//	                                              与拒答都 exit 0——探针报告形态,e2e
//	                                              断言状态行)
//	braid-sandbox-exec selftest raw-dial <ip:port>
//	                                              裸 TCP 拨号探测(T10.2 e2e):连通打
//	                                              "connected <addr>",失败打 "failed <err>"
//	                                              (两者都 exit 0;e2e 断言拒绝形态)

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// sandboxRequestLimit:stdin 请求信封收流上界(与载体 stdout 收流对称:
	// 产出契约 16MiB + 信封结构开销)。inputs ≤8MiB 家族在这里天然封顶,
	// 无界读入的 OOM 面收窄为常量。
	sandboxRequestLimit = DefaultMaxToolOutput + sandboxEnvelopeHeadroom
	// sandboxHelperSelftestAllocStep:alloc 探针的分配步长(基线 #4
	// 「按 8MiB 步长分配」)。
	sandboxHelperSelftestAllocStep = 8
	// sandboxServeMarker:serve 响应 body 的固定前缀(e2e 断言锚;后缀
	// 回显请求 path,证明 base_url 与 DSL path 的拼接)。
	sandboxServeMarker = "braid-e2e-serve-ok"
)

// 测试可注入的时间量(read timeout 与 watchdog grace;生产值固定)。
var (
	// sandboxHelperReadTimeout:stdin 首读时限——carrier 在容器启动后即刻
	// 写信封,读阻塞超此值 = 会话对端已死/被劫持,快败非零退出。
	sandboxHelperReadTimeout = 5 * time.Second
	// sandboxHelperExitGrace:信封 deadline 之后的硬退裕量(watchdog,
	// 基线 #13/§9-6):工具无视 ctx 挂死、或宿主 kill -9 后无 stop 信号时,
	// 进程在 deadline+grace 有界自灭,--rm 容器随之被 daemon 移除。正常
	// 路径(宿主存活)stop -t 3 的 SIGTERM/SIGKILL 先到,永不触及。
	sandboxHelperExitGrace = 5 * time.Second
)

// RunSandboxHelper 执行一次 helper 会话。argv = 程序名之后的参数;stdin/
// stdout 由调用方注入(生产 = os.Stdin/os.Stdout);hardExit 非 nil 时武装
// deadline+grace watchdog(会话干净返回即解除;cmd 传入 os.Exit 包装)。
// 返回 nil = 信封已写(含 ok=false 业务信封);返回错误 = 无信封失败,
// cmd 以非零码退出(宿主按 §6 走「exit≠0 无信封」分支)。
func RunSandboxHelper(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer, hardExit func()) error {
	if len(argv) < 1 {
		return fmt.Errorf("usage: braid-sandbox-exec <http|file_read|db_query> | selftest <mode>")
	}
	switch argv[0] {
	case "http", "file_read", "db_query":
		return runSandboxToolSession(ctx, argv[0], stdin, stdout, hardExit)
	case "selftest":
		return runSandboxSelftest(ctx, argv[1:], stdin, stdout)
	default:
		// argv 由载体白名单构造器发出;到这即错配/篡改——信封可观测报错。
		return writeSandboxEnvelope(stdout, SandboxResponse{
			OK: false, Code: SandboxCodeToolFailed,
			Message: fmt.Sprintf("unknown mode %q (want http|file_read|db_query|selftest)", argv[0]),
		})
	}
}

// runSandboxToolSession 是正常工具会话(文件头注释的完整流程)。
func runSandboxToolSession(ctx context.Context, tool string, stdin io.Reader, stdout io.Writer, hardExit func()) error {
	// 读入层:ctx 取消/首读超时/IO 错误 = 会话环境死,无信封非零退出;
	// 载荷层(空/超限/畸形 JSON)是数据事实,落 tool_failed 信封可观测。
	raw, err := readSandboxStdin(ctx, stdin)
	if err != nil {
		return fmt.Errorf("tools: sandbox helper: %w", err)
	}
	if len(raw) > sandboxRequestLimit {
		return writeSandboxEnvelope(stdout, SandboxResponse{
			OK: false, Code: SandboxCodeToolFailed,
			Message: fmt.Sprintf("request envelope exceeds %d bytes", sandboxRequestLimit),
		})
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return writeSandboxEnvelope(stdout, SandboxResponse{
			OK: false, Code: SandboxCodeToolFailed,
			Message: "request envelope empty (stdin closed without payload)",
		})
	}
	var req SandboxRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return writeSandboxEnvelope(stdout, SandboxResponse{
			OK: false, Code: SandboxCodeToolFailed,
			Message: fmt.Sprintf("request envelope not valid JSON: %v", err),
		})
	}
	if req.Tool != tool {
		return writeSandboxEnvelope(stdout, SandboxResponse{
			OK: false, Code: SandboxCodeToolFailed,
			Message: fmt.Sprintf("envelope tool %q mismatches argv %q", req.Tool, tool),
		})
	}
	built, err := rebuildSandboxTool(tool, req.Config)
	if err != nil {
		return writeSandboxEnvelope(stdout, SandboxResponse{OK: false, Code: SandboxCodeToolFailed, Message: err.Error()})
	}

	// 生命周期:信封携相对剩余毫秒(DeadlineMs 单调钟语义,与宿主 ctx 同
	// 锚——绝对秒锚会在 CLOCK_REALTIME 前跳窗内让 helper 先于宿主自灭,
	// 宿主误归 tool_failed);watchdog 兜底挂死与孤儿(§9-6),干净返回
	// 即解除。
	dctx := ctx
	if req.DeadlineMs != 0 { // ≤0 = 已到期(WithTimeout 负预算立即超时);0 = 无期限
		budget := time.Duration(req.DeadlineMs) * time.Millisecond
		var cancel context.CancelFunc
		dctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
		if hardExit != nil {
			timer := time.AfterFunc(budget+sandboxHelperExitGrace, hardExit)
			defer timer.Stop()
		}
	}
	if err := dctx.Err(); err != nil {
		return fmt.Errorf("tools: sandbox helper: %w", err)
	}

	var inputs map[string]any
	if len(req.Inputs) > 0 {
		var err error
		inputs, err = decodeSandboxInputs(req.Inputs)
		if err != nil {
			return writeSandboxEnvelope(stdout, SandboxResponse{
				OK: false, Code: SandboxCodeToolFailed,
				Message: err.Error(),
			})
		}
	}

	res, err := built.Run(dctx, Call{Inputs: inputs, Token: req.Token, IdempotencyKey: req.IdempotencyKey})
	if err != nil {
		resp, abort := sandboxClassifyToolError(err, dctx.Err())
		if abort != nil {
			return fmt.Errorf("tools: sandbox helper: %w", abort)
		}
		return writeSandboxEnvelope(stdout, *resp)
	}
	return writeSandboxEnvelope(stdout, SandboxResponse{OK: true, Output: res.Output})
}

// sandboxClassifyToolError 工具错误的信封/中止二分,镜像 runner.go 既有
// 排序:三哨兵先于 ctx 门——拒连证据是确定性的,晚到的 deadline 不得吞
// 掉 blocked 分类(跨进程边界三分桶保真);非哨兵且 ctx 已死 → 无信封
// 中止(deadline 的 infra 分类归宿在宿主载体,§6);其余 → tool_failed
// 信封。
func sandboxClassifyToolError(toolErr, ctxErr error) (resp *SandboxResponse, abort error) {
	if toolErr == nil {
		return nil, nil
	}
	sentinel := errors.Is(toolErr, ErrSSRFBlocked) ||
		errors.Is(toolErr, ErrHostBlocked) ||
		errors.Is(toolErr, ErrPathBlocked)
	if !sentinel && ctxErr != nil {
		return nil, ctxErr
	}
	return &SandboxResponse{
		OK:      false,
		Code:    SandboxCodeForError(toolErr),
		Message: toolErr.Error(),
	}, nil
}

// readSandboxStdin 有界读入原始载荷(LimitReader 封顶 limit+1,超限由
// 调用方按载荷层判定);读阻塞受 ctx 与首读时限双护——对端死/劫持快败,
// 会话级错误(无信封非零退出)。
func readSandboxStdin(ctx context.Context, stdin io.Reader) ([]byte, error) {
	type readResult struct {
		b   []byte
		err error
	}
	ch := make(chan readResult, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(stdin, sandboxRequestLimit+1))
		ch <- readResult{b, err}
	}()
	var r readResult
	select {
	case r = <-ch:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(sandboxHelperReadTimeout):
		return nil, fmt.Errorf("stdin read timeout after %s", sandboxHelperReadTimeout)
	}
	if r.err != nil {
		return nil, fmt.Errorf("read stdin: %w", r.err)
	}
	return r.b, nil
}

// decodeSandboxInputs 解码 inputs,数值一律 UseNumber——全仓数值保真纪律
// 单点(internal/scheduler/eval.go「解码一律 UseNumber,json.Number 原文
// 往返」;本地链 nodes/tool.go 同口径):本地模式工具恒见 json.Number,
// 沙箱模式裸 Unmarshal 落 float64 会让同一 DSL 在两模式下出网字节静默
// 分叉(>2^53 整数失真、小数格式漂移,query/body 均受影响——T5 对抗
// 审计确认 HIGH)。信封 wire 对数字词素逐字节保真,本函数是唯一解码口。
func decodeSandboxInputs(raw json.RawMessage) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var inputs map[string]any
	if err := dec.Decode(&inputs); err != nil {
		return nil, fmt.Errorf("inputs not a JSON object: %w", err)
	}
	return inputs, nil
}

// rebuildSandboxTool 按信封 Config 原料重建同名真工具(构造器即进程内装配
// 用的同一批——策略零复制;构造失败 = 配置坏值,调用方落 tool_failed 信封)。
func rebuildSandboxTool(tool string, config json.RawMessage) (Tool, error) {
	switch tool {
	case "http":
		var cfg struct {
			Endpoints map[string]string `json:"endpoints"`
			Proxy     string            `json:"proxy"`
		}
		if len(config) > 0 {
			if err := json.Unmarshal(config, &cfg); err != nil {
				return nil, fmt.Errorf("http config not valid JSON: %w", err)
			}
		}
		// T10.2 egress 代理跳接:信封 proxy 字段 → Guard.ProxyURL(词形
		// 与宿主 env 键共用 parseEgressProxyURL 单门);坏词形落
		// tool_failed 信封可观测,不带容器内 panic(no-envelope 形态)。
		var guard GuardConfig
		if cfg.Proxy != "" {
			u, err := parseEgressProxyURL(cfg.Proxy)
			if err != nil {
				return nil, fmt.Errorf("http config: %w", err)
			}
			guard.ProxyURL = u
		}
		return NewHTTPTool(HTTPConfig{Endpoints: cfg.Endpoints, Guard: guard})
	case "file_read":
		var cfg struct {
			Dirs []string `json:"dirs"`
		}
		if len(config) > 0 {
			if err := json.Unmarshal(config, &cfg); err != nil {
				return nil, fmt.Errorf("file_read config not valid JSON: %w", err)
			}
		}
		return NewFileReadTool(FileReadConfig{Dirs: cfg.Dirs})
	case "db_query":
		var cfg struct {
			DSN   string `json:"dsn"`
			Proxy string `json:"proxy,omitempty"`
		}
		if len(config) > 0 {
			if err := json.Unmarshal(config, &cfg); err != nil {
				return nil, fmt.Errorf("db_query config not valid JSON: %w", err)
			}
		}
		// proxy 词形与宿主 env 键共用 parseEgressProxyURL 单门(http 同款);
		// 在场 = MySQL 拨号经明文隧道(egressTunnelDialPlain),缺席 = 容器
		// 内直连(off 形态不应到达:载体装配恒带 proxy,坏词形响亮拒)。
		var proxyURL *url.URL
		if cfg.Proxy != "" {
			u, err := parseEgressProxyURL(cfg.Proxy)
			if err != nil {
				return nil, fmt.Errorf("db_query config: %w", err)
			}
			proxyURL = u
		}
		return NewDBQueryTool(DBQueryConfig{DSN: cfg.DSN, Proxy: proxyURL})
	default:
		return nil, fmt.Errorf("tool %q not sandboxable", tool)
	}
}

// writeSandboxEnvelope 单次写全(单行信封;错误上抛 = 无信封非零退出)。
func writeSandboxEnvelope(w io.Writer, resp SandboxResponse) error {
	b, err := EncodeSandboxResponse(resp)
	if err != nil {
		return fmt.Errorf("encode response envelope: %w", err)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write response envelope: %w", err)
	}
	return nil
}

// runSandboxSelftest 分派 selftest 探针模式(仅 e2e 消费;错误 = 无信封
// 非零退出,探针没有信封契约)。
func runSandboxSelftest(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer) error {
	if len(argv) < 1 {
		return fmt.Errorf("selftest requires a mode: dump-env | alloc <MiB> | serve <ip:port> <cert> <key> | spin <s> | forkchain <d> <max> | fds <max> | spam-stderr <MiB> | zombies <n> | egress-dial <proxy> <authority> | raw-dial <ip:port>")
	}
	switch argv[0] {
	case "dump-env":
		env := make(map[string]string)
		for _, kv := range os.Environ() {
			if i := strings.IndexByte(kv, '='); i > 0 {
				env[kv[:i]] = kv[i+1:]
			}
		}
		b, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("dump-env: %w", err)
		}
		_, err = stdout.Write(append(b, '\n'))
		return err
	case "alloc":
		if len(argv) < 2 {
			return fmt.Errorf("selftest alloc requires a MiB argument")
		}
		mib, err := strconv.Atoi(argv[1])
		if err != nil || mib < 1 || mib > 4096 {
			return fmt.Errorf("selftest alloc: want integer MiB in [1, 4096], got %q", argv[1])
		}
		return sandboxSelftestAlloc(mib, stdout)
	case "serve":
		// hermetic http e2e(T10)的宿主侧 TLS server(T7 契约:bridge 网
		// 容器 IP 全落禁段,可达的 198.19.x 必为宿主持有;叶证书与私钥
		// 永不入容器,容器内 serve 被运行期零通道封死——本模式只在宿主
		// 侧被 e2e 当子进程拉起,材料经 T7 生成器出)。
		if len(argv) != 4 {
			return fmt.Errorf("selftest serve requires <ip:port> <leaf-cert.pem> <leaf-key.pem> (host-side hermetic e2e server)")
		}
		return sandboxSelftestServe(ctx, argv[1], argv[2], argv[3], stdin, stdout)
	case "spin":
		if len(argv) != 2 {
			return fmt.Errorf("selftest spin requires <seconds>")
		}
		secs, err := strconv.Atoi(argv[1])
		if err != nil || secs < 1 || secs > 300 {
			return fmt.Errorf("selftest spin: want integer seconds in [1, 300], got %q", argv[1])
		}
		return sandboxSelftestSpin(ctx, secs, stdout)
	case "forkchain":
		if len(argv) != 3 {
			return fmt.Errorf("selftest forkchain requires <depth> <max>")
		}
		depth, err1 := strconv.Atoi(argv[1])
		max, err2 := strconv.Atoi(argv[2])
		if err1 != nil || err2 != nil || depth < 0 || max < 1 || depth > max || max > 1000 {
			return fmt.Errorf("selftest forkchain: want 0 <= depth <= max <= 1000 integers, got %q %q", argv[1], argv[2])
		}
		return sandboxSelftestForkChain(depth, max, stdout)
	case "fds":
		if len(argv) != 2 {
			return fmt.Errorf("selftest fds requires <max>")
		}
		max, err := strconv.Atoi(argv[1])
		if err != nil || max < 1 || max > 1000000 {
			return fmt.Errorf("selftest fds: want integer max in [1, 1000000], got %q", argv[1])
		}
		return sandboxSelftestFds(max, stdout)
	case "spam-stderr":
		if len(argv) != 2 {
			return fmt.Errorf("selftest spam-stderr requires <MiB>")
		}
		mib, err := strconv.Atoi(argv[1])
		if err != nil || mib < 1 || mib > 512 {
			return fmt.Errorf("selftest spam-stderr: want integer MiB in [1, 512], got %q", argv[1])
		}
		return sandboxSelftestSpamStderr(mib, stdout)
	case "zombies":
		if len(argv) != 2 {
			return fmt.Errorf("selftest zombies requires <n>")
		}
		n, err := strconv.Atoi(argv[1])
		if err != nil || n < 1 || n > 64 {
			return fmt.Errorf("selftest zombies: want integer n in [1, 64], got %q", argv[1])
		}
		return sandboxSelftestZombies(n, stdout)
	case "egress-dial":
		// 原始 CONNECT 探测(T10.2 e2e):不做 TLS、不带工具策略——只把
		// 代理拒答的状态行原样报给 e2e 断言(非白名 CONNECT 拒绝面)。
		// 成功(2xx)也立即断连:探针不消费隧道。
		if len(argv) != 3 {
			return fmt.Errorf("selftest egress-dial requires <proxy host:port> <authority host:port>")
		}
		return sandboxSelftestEgressDial(ctx, argv[1], argv[2], stdout)
	case "raw-dial":
		// 裸 TCP 拨号探测(T10.2 e2e):egress 内网内直连非网关地址必须
		// 失败——「直连被拒」的网络层结构性断言原语。
		if len(argv) != 2 {
			return fmt.Errorf("selftest raw-dial requires <ip:port>")
		}
		return sandboxSelftestRawDial(ctx, argv[1], stdout)
	case "zombie-child", "exit0":
		// zombies 探针的内部子模式:zombie-child 派生一个 exit0 孙进程后
		// 立即自退(孙进程成为孤儿 → 转交给 PID 1);exit0 立即退出。
		// 直连误用时同样安全有界(只派生一个子进程)。
		if argv[0] == "zombie-child" {
			return sandboxSelftestZombieChild(stdout)
		}
		return nil
	default:
		return fmt.Errorf("selftest unknown mode %q (want dump-env | alloc | serve | spin | forkchain | fds | spam-stderr | zombies | egress-dial | raw-dial)", argv[0])
	}
}

// sandboxSelftestEgressDial 原始 CONNECT 探针(文件头 egress-dial 条目):
// 拨代理 → 单条 CONNECT → 回读状态行打印即断。10s 硬期限(探针无信封
// 契约,e2e 侧另有时限)。
func sandboxSelftestEgressDial(ctx context.Context, proxy, authority string, stdout io.Writer) error {
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", proxy)
	if err != nil {
		return fmt.Errorf("selftest egress-dial: dial %s: %w", proxy, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority); err != nil {
		return fmt.Errorf("selftest egress-dial: write CONNECT: %w", err)
	}
	line, err := bufio.NewReader(io.LimitReader(conn, 4096)).ReadString('\n')
	if err != nil {
		return fmt.Errorf("selftest egress-dial: read status: %w", err)
	}
	_, werr := fmt.Fprintf(stdout, "%s", line)
	return werr
}

// sandboxSelftestRawDial 裸 TCP 探针(文件头 raw-dial 条目):连通/失败
// 都如实打印后正常退出——探针报告形态,e2e 断言期望侧(内网直连必须
// 落 failed)。
func sandboxSelftestRawDial(ctx context.Context, addr string, stdout io.Writer) error {
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		_, werr := fmt.Fprintf(stdout, "failed %s: %v\n", addr, err)
		return werr
	}
	_ = conn.Close()
	_, werr := fmt.Fprintf(stdout, "connected %s\n", addr)
	return werr
}

// sandboxSelftestServe 宿主侧 TLS server(T10 裁决:监听形态 = 显式 IP
// 绑定,wildcard 拒——hermetic 段由地址本身钉死;port 允许 0 = 内核
// 指派)。段约束归属:serve 只拒 wildcard,不校验 IP 是否段内——段钉扎
// 在 CA NameConstraints 与 e2e 选址,不在监听层(单测绑 127.0.0.1 即此
// 正交性的体现,审计登记)。协议行:绑成后向 stdout 打 "listening <ip>:<port>" 单行,测试
// 据此组装 endpoints(port 0 时的唯一知情面);之后 stdout 不再有输出。
// body = marker + 请求 path 回显。生命周期:stdin EOF(测试关管道)/
// SIGTERM/ctx.Done → 2s 有界优雅退——serve 的存在与测试进程绑定,不产
// 生孤儿。
func sandboxSelftestServe(ctx context.Context, addr, certPath, keyPath string, stdin io.Reader, stdout io.Writer) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("selftest serve: addr %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		return fmt.Errorf("selftest serve: addr host %q must be an explicit (non-wildcard) IP — the hermetic segment pin", host)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return fmt.Errorf("selftest serve: port %q invalid (want 0..65535)", portStr)
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("selftest serve: leaf pair: %w", err)
	}
	ln, err := tls.Listen("tcp", addr, &tls.Config{
		Certificates: []tls.Certificate{pair},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		return fmt.Errorf("selftest serve: listen %s: %w", addr, err)
	}

	if _, err := fmt.Fprintf(stdout, "listening %s\n", ln.Addr().String()); err != nil {
		_ = ln.Close()
		return fmt.Errorf("selftest serve: write protocol line: %w", err)
	}

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, sandboxServeMarker+" "+r.URL.Path)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	// stdin EOF = 测试侧关管道(主停机信号);SIGTERM = 外部 kill 兜底。
	stdinDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, stdin)
		close(stdinDone)
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	select {
	case <-stdinDone:
	case <-sig:
	case <-ctx.Done():
	case err := <-serveErr:
		return fmt.Errorf("selftest serve: %w", err)
	}
	shCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(shCtx)
	return nil
}

// sandboxSelftestAlloc 按 8MiB 步长分配并逐字节写满(内核真提交,防零页
// 惰性合并假绿),至目标或被 OOM 杀——e2e 用 2×memory 档位演示
// kill-and-fail-closed(基线 #4)。达标则报告后返回(小目标单测路径)。
func sandboxSelftestAlloc(mib int, stdout io.Writer) error {
	var held [][]byte
	for allocated := 0; allocated < mib; allocated += sandboxHelperSelftestAllocStep {
		step := sandboxHelperSelftestAllocStep
		if remain := mib - allocated; remain < step {
			step = remain
		}
		chunk := make([]byte, step<<20)
		for i := range chunk {
			chunk[i] = 0xAA
		}
		held = append(held, chunk)
	}
	_, err := fmt.Fprintf(stdout, "allocated %d MiB in %d MiB steps\n", mib, sandboxHelperSelftestAllocStep)
	_ = held // 引用保活到函数尾:防编译器把纯写入优化掉
	return err
}

// sandboxSelftestSpin 忙等自旋至期限(#5 CPU 配额 e2e 的载荷):不做任何
// 工作,只烧 CPU——配额执法(cgroup CFS)把实际用量压到 quota,宿主经
// 容器 cgroup cpu.stat 计量断言。期限用单调钟读数;每圈非阻塞探
// ctx.Done()——docker stop 的 SIGTERM 经 cmd 层 signal ctx 传入,取消
// 即刻有界退(否则 -t 3 宽限永远无效,必然 SIGKILL 升级收场)。
func sandboxSelftestSpin(ctx context.Context, secs int, stdout io.Writer) error {
	start := time.Now()
	deadline := start.Add(time.Duration(secs) * time.Second)
	x := 1.0
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			// 环境取消非载荷失败:报告后正常退出(stop 宽限被尊重)。
			_, err := fmt.Fprintf(stdout, "spun interrupted after %s: %v\n", time.Since(start).Round(time.Millisecond), ctx.Err())
			return err
		default:
		}
		// 浮点自反链防编译器把空循环折叠成一次判定。
		x = x*0.999999999 + 1e-9
	}
	_, err := fmt.Fprintf(stdout, "spun %ds (x=%g)\n", secs, x)
	return err
}

// sandboxSelftestForkChain /proc/self/exe 自再执行链(基线 #6):depth 代
// fork+exec 子代(depth+1)并 wait;全链同时在场 → pids 计数随深度增长。
// 收口三形态:cap(max 硬上界,非真炸弹)/ budget(cgroup pids 预算内
// 收口——每代 Go runtime 启动要建 ~4-6 线程,「fork 抢到最后槽位后子代
// 建线程 EAGAIN」是 runtime fatal throw 而非可捕获错误,预算 margin 让
// 链在触顶前可控收口)/ eagain(真触顶兜底,fork 直接 EAGAIN 时如实
// 上报)。子代被信号杀(OOM 竞态诊断)→ 响亮败,不伪装成链正常收口。
func sandboxSelftestForkChain(depth, max int, stdout io.Writer) error {
	if depth >= max {
		_, err := fmt.Fprintf(stdout, "forkchain depth=%d stop=cap\n", depth)
		return err
	}
	if cur, lim, ok := sandboxPidsBudget(); ok && lim > 0 && cur+sandboxHelperPidsMargin >= lim {
		_, err := fmt.Fprintf(stdout, "forkchain depth=%d stop=budget pids=%d/%d\n", depth, cur, lim)
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("selftest forkchain: resolve self exe: %w", err)
	}
	childArgv := []string{self, "selftest", "forkchain", strconv.Itoa(depth + 1), strconv.Itoa(max)}
	pid, err := syscall.ForkExec(self, childArgv, &syscall.ProcAttr{Files: []uintptr{0, 1, 2}})
	if err != nil {
		if errors.Is(err, syscall.EAGAIN) {
			// pids(或它处)资源触顶:链的兜底终点,如实上报。
			_, werr := fmt.Fprintf(stdout, "forkchain depth=%d stop=eagain\n", depth)
			if werr != nil {
				return werr
			}
			return nil
		}
		return fmt.Errorf("selftest forkchain: fork exec depth %d: %w", depth+1, err)
	}
	var ws syscall.WaitStatus
	if _, werr := syscall.Wait4(pid, &ws, 0, nil); werr != nil {
		return fmt.Errorf("selftest forkchain: wait depth %d: %w", depth+1, werr)
	}
	if ws.Signaled() {
		// budget margin 击穿或 OOM 竞态:附 pids 计数供判别真触顶与
		// margin 不足(后者应上调 sandboxHelperPidsMargin)。
		cur, lim, _ := sandboxPidsBudget()
		return fmt.Errorf("selftest forkchain: child at depth %d killed by signal %d (thread-creation EAGAIN past margin, or oom? pids=%d/%d margin=%d)", depth+1, ws.Signal(), cur, lim, sandboxHelperPidsMargin)
	}
	if code := ws.ExitStatus(); code != 0 {
		return fmt.Errorf("selftest forkchain: child at depth %d exited %d", depth+1, code)
	}
	return nil
}

// sandboxHelperPidsMargin budget 收口的预留任务数:覆盖两代子进程的
// Go runtime 启动线程上浮(实测每代 ~4-6,GC worker 池/阻塞 syscall 撑
// 新 M/Go 版本演进都会上浮;fork 抢到最后槽位后子代建线程 EAGAIN 是
// runtime fatal throw,不可捕获,只能预算避开)。
const sandboxHelperPidsMargin = 16

// sandboxPidsBudget 读本 cgroup 的 pids 计数(v2 = /sys/fs/cgroup/pids.*,
// v1 = /sys/fs/cgroup/pids/ 下同名;容器内路径即自身 cgroup)。max 文件
// "max"(无限制)时 lim 返回 -1——预算分支不触发,链靠 cap 硬上界。
// 两代路径都不可读(cgroup 不可观测的环境)→ ok=false,同样退化为
// cap/eagain 决定。
func sandboxPidsBudget() (cur, max int64, ok bool) {
	readInt := func(path string) (int64, bool) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return 0, false
		}
		v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	for _, prefix := range []string{"/sys/fs/cgroup/", "/sys/fs/cgroup/pids/"} {
		c, okC := readInt(prefix + "pids.current")
		mRaw, errM := os.ReadFile(prefix + "pids.max")
		if !okC || errM != nil {
			continue
		}
		mStr := strings.TrimSpace(string(mRaw))
		if mStr == "max" {
			return c, -1, true
		}
		m, perr := strconv.ParseInt(mStr, 10, 64)
		if perr != nil {
			continue
		}
		return c, m, true
	}
	return 0, 0, false
}

// sandboxSelftestFds 循环 open /dev/null 至失败(基线 #7):EMFILE 是
// ulimit 执法的预期失败形态,如实上报计数后正常退出(软失败,exit 0)。
// 打开的 fd 全部持有到函数尾再统一关闭(计数即真实 fd 耗尽点)。
func sandboxSelftestFds(max int, stdout io.Writer) error {
	var held []*os.File
	stop := "cap"
	for len(held) < max {
		fh, err := os.Open(os.DevNull)
		if err != nil {
			if errors.Is(err, syscall.EMFILE) {
				stop = "emfile"
				break
			}
			for _, f := range held {
				_ = f.Close()
			}
			return fmt.Errorf("selftest fds: open #%d: %w", len(held), err)
		}
		held = append(held, fh)
	}
	n := len(held)
	for _, f := range held {
		_ = f.Close()
	}
	_, err := fmt.Fprintf(stdout, "fds=%d stop=%s\n", n, stop)
	return err
}

// sandboxSelftestSpamStderr 定长块写 stderr 至目标(基线 #14:daemon 日志
// 通道封顶的载荷)。块内容是确定性重复行,无新信息;写错误(如对端关
// 闭)响亮败——载荷失真会让封顶断言失真。收尾哨兵行:last-write 必落
// 轮转后驻留文件的尾部(轮转删头留尾),e2e 据此证明 stderr→json-file→
// docker logs 通道确实承载了载荷(驻留总量非确定,不能当内容证据)。
func sandboxSelftestSpamStderr(mib int, stdout io.Writer) error {
	const spamLine = "braid-e2e-stderr-spam-line\n"
	chunk := bytes.Repeat([]byte(spamLine), 4096/len(spamLine)+1)
	chunk = chunk[:4096]
	remaining := int64(mib) << 20
	for remaining > 0 {
		n := int64(len(chunk))
		if remaining < n {
			n = remaining
		}
		if _, err := os.Stderr.Write(chunk[:n]); err != nil {
			return fmt.Errorf("selftest spam-stderr: write: %w", err)
		}
		remaining -= n
	}
	if _, err := fmt.Fprintf(os.Stderr, "braid-e2e-stderr-spam-done %d\n", mib); err != nil {
		return fmt.Errorf("selftest spam-stderr: sentinel: %w", err)
	}
	_, err := fmt.Fprintf(stdout, "spammed %d MiB to stderr\n", mib)
	return err
}

// sandboxSelftestZombies 派生 n 个孤儿子进程后扫 /proc Z 状态(基线
// #15):每个孤儿经 zombie-child 双 fork 产生——child 派生 exit0 孙进程
// 后立即自退,孙进程转交 PID 1;--init(tini)收割 → Z 归零,PID 1 不
// 收割(如 --init 漂移)→ 孤儿僵尸永续滞留。子进程退出状态必须核验
// (forkchain 同款纪律):zombie-child 非零退出 = 孤儿从未产生,静默吞
// 掉会把探针失效退化成 zombies=0 = e2e 通过值(错绿),必须响亮败。
// 聚合用沉降判据而非取最大:正确配置下孙进程「退出→转交→tini 收割」
// 存在亚毫秒瞬态 Z 窗,任一轮观测即判会把收割延迟错当漂移(错红);
// 轮询至归零(2s 预算)报 0,预算尽报末次计数——漂移形态僵尸永不归零,
// 检测力不因瞬态容忍受损。
func sandboxSelftestZombies(n int, stdout io.Writer) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("selftest zombies: resolve self exe: %w", err)
	}
	for i := 0; i < n; i++ {
		argv := []string{self, "selftest", "zombie-child"}
		pid, err := syscall.ForkExec(self, argv, &syscall.ProcAttr{Files: []uintptr{0, 1, 2}})
		if err != nil {
			return fmt.Errorf("selftest zombies: spawn child #%d: %w", i, err)
		}
		var ws syscall.WaitStatus
		if _, werr := syscall.Wait4(pid, &ws, 0, nil); werr != nil {
			return fmt.Errorf("selftest zombies: wait child #%d: %w", i, werr)
		}
		if ws.Signaled() {
			return fmt.Errorf("selftest zombies: child #%d killed by signal %d (orphan spawn broken)", i, ws.Signal())
		}
		if code := ws.ExitStatus(); code != 0 {
			return fmt.Errorf("selftest zombies: child #%d exited %d (orphan never created — must fail loud, not read as zombies=0)", i, code)
		}
	}
	zombies := 0
	for round := 0; round < 10; round++ {
		if round > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		z, err := sandboxCountZombieProcesses()
		if err != nil {
			return fmt.Errorf("selftest zombies: scan: %w", err)
		}
		zombies = z
		if z == 0 {
			break // 沉降归零:正确配置的稳态。
		}
	}
	_, werr := fmt.Fprintf(stdout, "zombies=%d\n", zombies)
	return werr
}

// sandboxSelftestZombieChild 派生一个 exit0 孙进程后立即退出:孙进程先于
// 本进程死亡则成为本进程的僵尸并随后者转交;晚于则直接转交——两种次序
// 终态一致(孤儿 → PID 1)。stdout 形参与 selftest 处理器族同构,本模式
// 无输出。
func sandboxSelftestZombieChild(_ io.Writer) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("selftest zombie-child: resolve self exe: %w", err)
	}
	argv := []string{self, "selftest", "exit0"}
	pid, err := syscall.ForkExec(self, argv, &syscall.ProcAttr{Files: []uintptr{0, 1, 2}})
	if err != nil {
		return fmt.Errorf("selftest zombie-child: spawn: %w", err)
	}
	_ = pid // 不 wait:本进程立即退出,孙进程转交 PID 1 是被测面本身
	return nil
}

// sandboxCountZombieProcesses 扫 /proc/<pid>/stat 统计 Z 状态进程数
// (#15 的断言原语)。stat 第二字段(Comm)可含空格与括号——状态字段
// 取「最后一个 ')' 之后的首个非空 token」(proc(5) 同款解析法)。
func sandboxCountZombieProcesses() (int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	zombies := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid := e.Name()
		if _, err := strconv.Atoi(pid); err != nil {
			continue // 非 PID 目录
		}
		raw, err := os.ReadFile("/proc/" + pid + "/stat")
		if err != nil {
			continue // 进程退场竞态,跳过
		}
		if i := bytes.LastIndexByte(raw, ')'); i >= 0 && i+2 < len(raw) && raw[i+2] == 'Z' {
			zombies++
		}
	}
	return zombies, nil
}
