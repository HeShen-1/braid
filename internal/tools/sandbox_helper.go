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

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
		return fmt.Errorf("usage: braid-sandbox-exec <http|file_read> | selftest <mode>")
	}
	switch argv[0] {
	case "http", "file_read":
		return runSandboxToolSession(ctx, argv[0], stdin, stdout, hardExit)
	case "selftest":
		return runSandboxSelftest(ctx, argv[1:], stdin, stdout)
	default:
		// argv 由载体白名单构造器发出;到这即错配/篡改——信封可观测报错。
		return writeSandboxEnvelope(stdout, SandboxResponse{
			OK: false, Code: SandboxCodeToolFailed,
			Message: fmt.Sprintf("unknown mode %q (want http|file_read|selftest)", argv[0]),
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
		}
		if len(config) > 0 {
			if err := json.Unmarshal(config, &cfg); err != nil {
				return nil, fmt.Errorf("http config not valid JSON: %w", err)
			}
		}
		return NewHTTPTool(HTTPConfig{Endpoints: cfg.Endpoints})
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
		return fmt.Errorf("selftest requires a mode: dump-env | alloc <MiB> | serve <ip:port> <cert> <key>")
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
	default:
		return fmt.Errorf("selftest unknown mode %q (want dump-env | alloc | serve)", argv[0])
	}
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
