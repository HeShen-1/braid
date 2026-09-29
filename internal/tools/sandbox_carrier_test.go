package tools

// 沙箱载体运行时测试(T10.1 T4):fake docker shim 冒充 docker CLI——argv
// 逐调用单行落盘、stdin 信封落盘、stop/inspect/ps 行为内联,决策逻辑全
// 离线可测(真 docker 只进 e2e)。分类契约锚(基线 §6 / D-t101 #4/#6):
//   - exit 0 + 完整信封 → 信封决定成败:哨兵原样重抛(runner 既有映射)、
//     output 透传;信封畸形/缺失/溢出 = 普通错误(runner 默认 tool_failed);
//   - ctx 取消(含 run 途中被杀)→ ctx.Err() 链回 = 基础设施失败,随后
//     docker stop -t 3 <name> 有界清场(best-effort);
//   - exit≠0 无信封 → 普通错误,消息带 exit 码 + OOMKilled 归因单字段
//     (inspect 不可达时降级 unknown,exit 137 仍是信号)。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeDockerShim 落一个可执行脚本冒充 docker 二进制,返回 bin 路径与记录
// 目录。脚本内置 $d(记录目录)与 rec()(单行落盘一次调用——argv 白名单
// 保证参数无空白,单行还原无损);body 按 $1 分派子命令行为。
func fakeDockerShim(t *testing.T, body string) (bin, dir string) {
	t.Helper()
	return fakeNamedDockerShim(t, "fake-docker", body)
}

// fakeNamedDockerShim 同上,但二进制名可指定——SandboxStartupCheck 查
// sandboxDockerBin 常量("docker"),测试经 PATH 注入同名 shim。
func fakeNamedDockerShim(t *testing.T, name, body string) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	bin = filepath.Join(dir, name)
	script := "#!/bin/sh\nd='" + dir + "'\nrec() { echo \"$*\" >> \"$d/argv.log\"; }\n" + body
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker shim: %v", err)
	}
	return bin, dir
}

// shimArgv 读回调用记录(每次调用一行)。
func shimArgv(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "argv.log"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read shim argv log: %v", err)
	}
	text := strings.TrimSpace(string(b))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// carrierCfg 测试基线配置(与 argv golden 同值)。
var carrierCfg = sandboxConfig{
	Docker:         true,
	Image:          "braid-tool-sandbox:1",
	Memory:         "256m",
	CPUs:           "0.5",
	PidsLimit:      64,
	MaxConcurrency: 8,
}

var sandboxNameRe = regexp.MustCompile(`^braid-sbx-(http|file_read)-[0-9a-f]{16}$`)

// newTestCarrier 构造注入 fake bin 的载体(生产默认 "docker" 走 PATH,
// 仅测试置换——GATE 1 拍板:不留 docker bin 生产 env 面)。
func newTestCarrier(t *testing.T, name string, toolConfig json.RawMessage, dirs []string, bin string, cap int) *sandboxCarrier {
	t.Helper()
	c, err := newSandboxCarrier(name, carrierCfg, toolConfig, dirs, newSandboxLimiter(cap))
	if err != nil {
		t.Fatalf("newSandboxCarrier: %v", err)
	}
	c.docker = bin
	return c
}

// expectedRunArgv 期望 argv 单行(基线旗标全矩阵硬编码,与 argv golden 同锚)。
func expectedRunArgv(name, tool, mount string) string {
	base := "run --rm --interactive --name " + name +
		" --label braid.sandbox=1 --label braid.tool=" + tool +
		" --read-only --tmpfs /tmp:rw,noexec,nosuid,size=16m" +
		" --memory 256m --memory-swap 256m --cpus 0.5 --pids-limit 64" +
		" --ulimit nofile=1024:1024 --cap-drop ALL --security-opt no-new-privileges" +
		" --user 65532:65532 --init --log-driver json-file --log-opt max-size=5m --log-opt max-file=1"
	net := " --network bridge"
	if tool == "file_read" {
		net = " --network none"
		if mount != "" {
			net += " --mount type=bind,src=" + mount + ",dst=" + mount + ",readonly"
		}
	}
	return base + net + " --entrypoint /usr/local/bin/braid-sandbox-exec braid-tool-sandbox:1 " + tool
}

func TestNewSandboxCarrier(t *testing.T) {
	t.Run("defaults and name", func(t *testing.T) {
		c, err := newSandboxCarrier("http", carrierCfg, json.RawMessage(`{}`), nil, newSandboxLimiter(8))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if c.Name() != "http" || c.docker != "docker" {
			t.Fatalf("carrier = %+v, want name http and default docker bin", c)
		}
	})

	t.Run("rejects off mode", func(t *testing.T) {
		off := carrierCfg
		off.Docker = false
		_, err := newSandboxCarrier("http", off, nil, nil, newSandboxLimiter(8))
		if err == nil || !strings.Contains(err.Error(), "docker mode") {
			t.Fatalf("err = %v, want off-mode rejection", err)
		}
	})

	t.Run("rejects tool outside vocabulary", func(t *testing.T) {
		_, err := newSandboxCarrier("shell", carrierCfg, nil, nil, newSandboxLimiter(8))
		if err == nil || !strings.Contains(err.Error(), "vocabulary") {
			t.Fatalf("err = %v, want vocabulary rejection", err)
		}
	})

	t.Run("rejects dirs on non-file_read tool", func(t *testing.T) {
		_, err := newSandboxCarrier("http", carrierCfg, nil, []string{"/var/braid/docs"}, newSandboxLimiter(8))
		if err == nil || !strings.Contains(err.Error(), "dirs") {
			t.Fatalf("err = %v, want dirs rejection", err)
		}
	})

	t.Run("nil limiter panics", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("want panic on nil limiter")
			}
		}()
		_, _ = newSandboxCarrier("http", carrierCfg, nil, nil, nil)
	})
}

func TestSandboxLimiter(t *testing.T) {
	t.Run("cap blocks then ctx cancels wait", func(t *testing.T) {
		l := newSandboxLimiter(1)
		if err := l.acquire(context.Background()); err != nil {
			t.Fatalf("first acquire: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if err := l.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("second acquire err = %v, want deadline", err)
		}
		l.release()
		if err := l.acquire(context.Background()); err != nil {
			t.Fatalf("acquire after release: %v", err)
		}
	})

	t.Run("zero cap panics", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("want panic on zero cap")
			}
		}()
		newSandboxLimiter(0)
	})
}

func TestSandboxCarrierRun(t *testing.T) {
	t.Run("ok envelope over stdin with whitelisted argv", func(t *testing.T) {
		bin, dir := fakeDockerShim(t, `
rec "$@"
cat > "$d/stdin.log"
echo '{"ok":true,"output":{"path":"/etc/hosts","bytes":12}}'
`)
		c := newTestCarrier(t, "http", json.RawMessage(`{"endpoints":{"api":"https://a.test"}}`), nil, bin, 8)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, err := c.Run(ctx, Call{
			Inputs:         map[string]any{"endpoint": "api", "path": "/x"},
			Token:          "tok-abc",
			IdempotencyKey: "att:7:2",
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if string(res.Output) != `{"path":"/etc/hosts","bytes":12}` {
			t.Fatalf("output = %s, want envelope output verbatim", res.Output)
		}

		// argv:单次 run 调用、旗标全矩阵、一次性容器名。
		lines := shimArgv(t, dir)
		if len(lines) != 1 {
			t.Fatalf("docker invocations = %q, want exactly one run", lines)
		}
		args := strings.Split(lines[0], " ")
		if len(args) < 5 || args[0] != "run" || args[3] != "--name" {
			t.Fatalf("argv = %q, want run ... --name <name> ...", lines[0])
		}
		name := args[4]
		if !sandboxNameRe.MatchString(name) {
			t.Fatalf("container name %q outside braid-sbx pattern", name)
		}
		if want := expectedRunArgv(name, "http", ""); lines[0] != want {
			t.Fatalf("argv =\n%s\nwant\n%s", lines[0], want)
		}

		// stdin 信封:五载荷齐全,deadline 取自 ctx。
		raw, err := os.ReadFile(filepath.Join(dir, "stdin.log"))
		if err != nil {
			t.Fatalf("read stdin log: %v", err)
		}
		var env map[string]any
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("stdin not JSON envelope: %v (%s)", err, raw)
		}
		if env["tool"] != "http" || env["token"] != "tok-abc" || env["idempotency_key"] != "att:7:2" {
			t.Fatalf("envelope identity fields = %v", env)
		}
		cfgObj, _ := env["config"].(map[string]any)
		if cfgObj == nil || cfgObj["endpoints"] == nil {
			t.Fatalf("envelope config missing: %v", env)
		}
		inputs, _ := env["inputs"].(map[string]any)
		if inputs == nil || inputs["endpoint"] != "api" {
			t.Fatalf("envelope inputs missing: %v", env)
		}
		deadlineMs, _ := env["deadline_ms"].(float64)
		// 相对剩余毫秒:编码发生在 Run 内(≈ start),10s 预算减去启动
		// 开销后落在 (9.8s, 10s];绝对秒锚已废弃(双钟锚缝,T5 审计)。
		if deadlineMs < 9800 || deadlineMs > 10000 {
			t.Fatalf("deadline_ms = %v, want remaining budget in (9800, 10000]", deadlineMs)
		}
	})

	t.Run("sentinel envelopes reraise", func(t *testing.T) {
		cases := []struct {
			code     string
			sentinel error
		}{
			{SandboxCodeSSRFBlocked, ErrSSRFBlocked},
			{SandboxCodeHostBlocked, ErrHostBlocked},
			{SandboxCodePathBlocked, ErrPathBlocked},
		}
		for _, tc := range cases {
			t.Run("code "+tc.code, func(t *testing.T) {
				bin, _ := fakeDockerShim(t, `
cat > /dev/null
echo '{"ok":false,"code":"`+tc.code+`","message":"rejected"}'
`)
				carrier := newTestCarrier(t, "http", nil, nil, bin, 8)
				_, err := carrier.Run(context.Background(), Call{Inputs: map[string]any{}})
				if err == nil {
					t.Fatal("want error")
				}
				if !errors.Is(err, tc.sentinel) {
					t.Fatalf("err = %v, want sentinel %v reraised", err, tc.sentinel)
				}
			})
		}
	})

	t.Run("tool_failed envelope is plain error", func(t *testing.T) {
		bin, _ := fakeDockerShim(t, `
cat > /dev/null
echo '{"ok":false,"code":"tool_failed","message":"bad input"}'
`)
		c := newTestCarrier(t, "http", nil, nil, bin, 8)
		_, err := c.Run(context.Background(), Call{Inputs: map[string]any{}})
		if err == nil || !strings.Contains(err.Error(), "bad input") {
			t.Fatalf("err = %v, want message preserved", err)
		}
		for _, s := range []error{ErrSSRFBlocked, ErrHostBlocked, ErrPathBlocked} {
			if errors.Is(err, s) {
				t.Fatalf("err = %v, must not carry sentinel", err)
			}
		}
	})

	t.Run("exit 137 without envelope attributes oom when inspect answers", func(t *testing.T) {
		bin, _ := fakeDockerShim(t, `
rec "$@"
case "$1" in
run) echo oom-stderr-noise >&2; exit 137 ;;
inspect) echo true; exit 0 ;;
*) exit 1 ;;
esac
`)
		c := newTestCarrier(t, "http", nil, nil, bin, 8)
		_, err := c.Run(context.Background(), Call{Inputs: map[string]any{}})
		if err == nil {
			t.Fatal("want error for exit 137")
		}
		for _, want := range []string{"exit=137", "oom_killed=true", "oom-stderr-noise"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want %q in attribution", err, want)
			}
		}
		for _, s := range []error{ErrSSRFBlocked, ErrHostBlocked, ErrPathBlocked} {
			if errors.Is(err, s) {
				t.Fatalf("err = %v, must not carry sentinel", err)
			}
		}
	})

	t.Run("inspect unavailable degrades to unknown", func(t *testing.T) {
		bin, _ := fakeDockerShim(t, `
case "$1" in
run) echo noise >&2; exit 137 ;;
*) exit 1 ;;
esac
`)
		c := newTestCarrier(t, "http", nil, nil, bin, 8)
		_, err := c.Run(context.Background(), Call{Inputs: map[string]any{}})
		if err == nil || !strings.Contains(err.Error(), "oom_killed=unknown") || !strings.Contains(err.Error(), "exit=137") {
			t.Fatalf("err = %v, want exit=137 with oom_killed=unknown", err)
		}
	})

	t.Run("malformed envelope is plain error", func(t *testing.T) {
		bin, _ := fakeDockerShim(t, `
cat > /dev/null
echo not-json
`)
		c := newTestCarrier(t, "http", nil, nil, bin, 8)
		_, err := c.Run(context.Background(), Call{Inputs: map[string]any{}})
		if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
			t.Fatalf("err = %v, want malformed-envelope rejection", err)
		}
	})

	t.Run("empty stdout with exit 0 is plain error", func(t *testing.T) {
		bin, _ := fakeDockerShim(t, "cat > /dev/null\n")
		c := newTestCarrier(t, "http", nil, nil, bin, 8)
		_, err := c.Run(context.Background(), Call{Inputs: map[string]any{}})
		if err == nil || !strings.Contains(err.Error(), "empty") {
			t.Fatalf("err = %v, want empty-envelope rejection", err)
		}
	})

	t.Run("stdout overflow is plain error", func(t *testing.T) {
		bin, _ := fakeDockerShim(t, `
cat > /dev/null
head -c 17825792 /dev/zero | tr '\0' a
`)
		c := newTestCarrier(t, "http", nil, nil, bin, 8)
		_, err := c.Run(context.Background(), Call{Inputs: map[string]any{}})
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("err = %v, want overflow rejection", err)
		}
	})

	t.Run("cancel kills run and stops container bounded", func(t *testing.T) {
		bin, dir := fakeDockerShim(t, `
rec "$@"
case "$1" in
run) cat > /dev/null; exec sleep 30 ;;
stop) exit 0 ;;
*) exit 1 ;;
esac
`)
		c := newTestCarrier(t, "file_read", json.RawMessage(`{"dirs":["/var/braid/docs"]}`), []string{"/var/braid/docs"}, bin, 8)
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := c.Run(ctx, Call{Inputs: map[string]any{}})
		elapsed := time.Since(start)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want ctx deadline error (infra path)", err)
		}
		// stop -t 3 <name> 有界清场落盘;run 调用在案且与 stop 目标同一容器,
		// 无多余子命令(审计确认项:断言调用全集,非仅存在性)。
		lines := shimArgv(t, dir)
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "run ") || !strings.HasPrefix(lines[1], "stop ") {
			t.Fatalf("docker invocations = %q, want [run ..., stop ...]", lines)
		}
		runName := strings.Split(lines[0], " ")[4]
		stopLine := lines[1]
		if !regexp.MustCompile(`^stop -t 3 braid-sbx-file_read-[0-9a-f]{16}$`).MatchString(stopLine) {
			t.Fatalf("stop invocation = %q, want \"stop -t 3 <name>\"", stopLine)
		}
		if stopName := strings.Fields(stopLine)[3]; stopName != runName {
			t.Fatalf("stop target %q != run container %q", stopName, runName)
		}
		if elapsed > 10*time.Second {
			t.Fatalf("cancel path took %v, want bounded cleanup", elapsed)
		}
	})

	t.Run("tight deadline that the envelope beats keeps sentinel", func(t *testing.T) {
		bin, _ := fakeDockerShim(t, `
cat > /dev/null
sleep 0.2
echo '{"ok":false,"code":"ssrf_blocked","message":"late but complete"}'
`)
		c := newTestCarrier(t, "http", nil, nil, bin, 8)
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		_, err := c.Run(ctx, Call{Inputs: map[string]any{}})
		if !errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("err = %v, want completed sentinel envelope to outrank un-fired deadline", err)
		}
	})

	t.Run("deadline that fires first is infra error", func(t *testing.T) {
		bin, _ := fakeDockerShim(t, `
cat > /dev/null
sleep 0.4
echo '{"ok":false,"code":"ssrf_blocked","message":"never lands"}'
`)
		c := newTestCarrier(t, "http", nil, nil, bin, 8)
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		_, err := c.Run(ctx, Call{Inputs: map[string]any{}})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want deadline before completion to be infra", err)
		}
		if errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("err = %v, killed run must not fabricate envelope semantics", err)
		}
	})

	t.Run("pre-canceled ctx never spawns docker", func(t *testing.T) {
		bin, dir := fakeDockerShim(t, "exit 1\n")
		c := newTestCarrier(t, "http", nil, nil, bin, 8)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := c.Run(ctx, Call{Inputs: map[string]any{}})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if lines := shimArgv(t, dir); len(lines) != 0 {
			t.Fatalf("docker invocations = %q, want none", lines)
		}
	})

	t.Run("semaphore bounds concurrent runs", func(t *testing.T) {
		bin, dir := fakeDockerShim(t, `
rec "$@"
cat > /dev/null
sleep 0.3
echo '{"ok":true,"output":{"n":1}}'
`)
		c := newTestCarrier(t, "http", nil, nil, bin, 1)
		done := make(chan error, 1)
		go func() {
			_, err := c.Run(context.Background(), Call{Inputs: map[string]any{}})
			done <- err
		}()
		// 确定性同步(审计确认项:sleep 同步有窄时窗):轮询 argv.log 直到
		// A 的 run 调用落盘——此刻 A 必已持有唯一槽位,B 才会真正阻塞。
		for i := 0; i < 200; i++ {
			if lines := shimArgv(t, dir); len(lines) >= 1 && strings.HasPrefix(lines[0], "run ") {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		if lines := shimArgv(t, dir); len(lines) == 0 || !strings.HasPrefix(lines[0], "run ") {
			t.Fatal("first run never recorded; cannot establish semaphore occupancy")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, err := c.Run(ctx, Call{Inputs: map[string]any{}})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("second run err = %v, want deadline while waiting on semaphore", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("first run: %v", err)
		}
		if lines := shimArgv(t, dir); len(lines) != 1 {
			t.Fatalf("docker invocations = %q, want exactly one (B never spawned)", lines)
		}
	})
}

func TestSandboxCarrierMounts(t *testing.T) {
	t.Run("missing whitelist dirs are not mounted but stay in envelope config", func(t *testing.T) {
		base := t.TempDir()
		keep := filepath.Join(base, "keep")
		if err := os.Mkdir(keep, 0o755); err != nil {
			t.Fatal(err)
		}
		gone := filepath.Join(base, "gone") // 刻意不创建:docker 会 root 自动建目录,必须不挂

		bin, dir := fakeDockerShim(t, `
rec "$@"
cat > "$d/stdin.log"
echo '{"ok":true,"output":{"n":1}}'
`)
		// 尾斜杠配置形态:挂载走 Clean 后词形(与 helper 词形门同构),
		// 信封 config 保留原始词形(策略面不感知载体 normalize)。
		slashKeep := keep + "/"
		toolConfig := json.RawMessage(`{"dirs":["` + slashKeep + `","` + gone + `"]}`)
		c := newTestCarrier(t, "file_read", toolConfig, []string{slashKeep, gone}, bin, 8)
		res, err := c.Run(context.Background(), Call{Inputs: map[string]any{"path": keep + "/a.txt"}})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if string(res.Output) != `{"n":1}` {
			t.Fatalf("output = %s", res.Output)
		}

		lines := shimArgv(t, dir)
		if len(lines) != 1 {
			t.Fatalf("docker invocations = %q, want one", lines)
		}
		args := strings.Split(lines[0], " ")
		name := args[4]
		if !sandboxNameRe.MatchString(name) {
			t.Fatalf("container name %q outside pattern", name)
		}
		if want := expectedRunArgv(name, "file_read", keep); lines[0] != want {
			t.Fatalf("argv =\n%s\nwant\n%s", lines[0], want)
		}
		if strings.Contains(lines[0], gone) {
			t.Fatalf("argv must not mount missing dir %s", gone)
		}

		// 信封 config 保留原词形全量(策略与进程内一致;缺目录→容器内打开
		// 失败→tool_failed,与 EvalSymlinks 失败同语义——GATE 1 #9)。
		raw, err := os.ReadFile(filepath.Join(dir, "stdin.log"))
		if err != nil {
			t.Fatal(err)
		}
		var env struct {
			Config struct {
				Dirs []string `json:"dirs"`
			} `json:"config"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("envelope: %v (%s)", err, raw)
		}
		if len(env.Config.Dirs) != 2 || env.Config.Dirs[0] != slashKeep || env.Config.Dirs[1] != gone {
			t.Fatalf("envelope config dirs = %v, want unfiltered raw whitelist", env.Config.Dirs)
		}
	})
}

func TestSandboxProbe(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"healthy daemon and image", `
rec "$@"
case "$1" in
info) echo 24.0.7 ;;
image) exit 0 ;;
*) exit 9 ;;
esac
`, ""},
		{"floor version exactly ok", `
case "$1" in
info) echo 17.06.1-ce ;;
image) exit 0 ;;
*) exit 9 ;;
esac
`, ""},
		{"below floor rejected", `
case "$1" in
info) echo 17.05.0 ;;
image) exit 0 ;;
*) exit 9 ;;
esac
`, "17.06"},
		{"unparseable version rejected", `
case "$1" in
info) echo banana ;;
image) exit 0 ;;
*) exit 9 ;;
esac
`, "version"},
		{"daemon unreachable", `
case "$1" in
info) exit 1 ;;
*) exit 9 ;;
esac
`, "unreachable"},
		{"image missing carries build command", `
case "$1" in
info) echo 24.0.7 ;;
image) exit 1 ;;
*) exit 9 ;;
esac
`, "CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o braid-sandbox-exec ./cmd/braid-sandbox-exec && docker build -f deploy/sandbox.Dockerfile -t braid-tool-sandbox:1 ."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bin, dir := fakeDockerShim(t, c.body)
			err := SandboxProbe(context.Background(), bin, "braid-tool-sandbox:1")
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, c.wantErr)
			}
			if c.name == "healthy daemon and image" {
				lines := shimArgv(t, dir)
				if len(lines) != 2 ||
					lines[0] != "info --format {{.ServerVersion}}" ||
					lines[1] != "image inspect braid-tool-sandbox:1" {
					t.Fatalf("probe invocations = %q", lines)
				}
			}
		})
	}

	t.Run("missing docker binary", func(t *testing.T) {
		err := SandboxProbe(context.Background(), filepath.Join(t.TempDir(), "no-such-docker"), "braid-tool-sandbox:1")
		if err == nil || !strings.Contains(err.Error(), "not found in PATH") {
			t.Fatalf("err = %v, want LookPath rejection", err)
		}
	})
}

func TestSandboxSweep(t *testing.T) {
	t.Run("removes labeled leftovers", func(t *testing.T) {
		bin, dir := fakeDockerShim(t, `
rec "$@"
case "$1" in
ps) printf 'abc123\ndef456\n' ;;
rm) exit 0 ;;
*) exit 9 ;;
esac
`)
		removed, err := SandboxSweep(context.Background(), bin)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(removed) != 2 || removed[0] != "abc123" || removed[1] != "def456" {
			t.Fatalf("removed = %q", removed)
		}
		lines := shimArgv(t, dir)
		if len(lines) != 3 || lines[0] != "ps -aq --filter label=braid.sandbox=1" ||
			lines[1] != "rm -f abc123" || lines[2] != "rm -f def456" {
			t.Fatalf("sweep invocations = %q", lines)
		}
	})

	t.Run("nothing to sweep", func(t *testing.T) {
		bin, dir := fakeDockerShim(t, `
rec "$@"
case "$1" in
ps) exit 0 ;;
*) exit 9 ;;
esac
`)
		removed, err := SandboxSweep(context.Background(), bin)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(removed) != 0 {
			t.Fatalf("removed = %q, want none", removed)
		}
		if lines := shimArgv(t, dir); len(lines) != 1 {
			t.Fatalf("invocations = %q, want ps only", lines)
		}
	})

	t.Run("ps failure is loud", func(t *testing.T) {
		bin, _ := fakeDockerShim(t, `
case "$1" in
ps) exit 1 ;;
*) exit 9 ;;
esac
`)
		_, err := SandboxSweep(context.Background(), bin)
		if err == nil || !strings.Contains(err.Error(), "docker ps") {
			t.Fatalf("err = %v, want loud ps failure", err)
		}
	})

	t.Run("individual rm failure is skipped", func(t *testing.T) {
		bin, _ := fakeDockerShim(t, `
rec "$@"
case "$1" in
ps) printf 'x1\nx2\n' ;;
rm) [ "$3" = x1 ] && exit 1; exit 0 ;;
*) exit 9 ;;
esac
`)
		removed, err := SandboxSweep(context.Background(), bin)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(removed) != 1 || removed[0] != "x2" {
			t.Fatalf("removed = %q, want only x2", removed)
		}
	})
}

// TestSandboxStartupCheck(T6 worker 接线):off = 零 docker 接触;docker =
// probe 先于 sweep(部署性拒启,失败不清扫)、自定义镜像传播、词表坏值
// 键名响亮败。shim 以 "docker" 之名注入 PATH(sandboxDockerBin 常量查找)。
func TestSandboxStartupCheck(t *testing.T) {
	t.Run("off mode touches nothing", func(t *testing.T) {
		bin, dir := fakeNamedDockerShim(t, "docker", "rec \"$@\"\nexit 9")
		t.Setenv("PATH", filepath.Dir(bin))
		removed, err := SandboxStartupCheck(context.Background(), envOf(map[string]string{"BRAID_TOOL_SANDBOX": "off"}))
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if removed != nil {
			t.Fatalf("removed = %v, want nil", removed)
		}
		if lines := shimArgv(t, dir); len(lines) != 0 {
			t.Fatalf("invocations = %q, want zero docker contact", lines)
		}
	})

	t.Run("docker mode probes then sweeps", func(t *testing.T) {
		bin, dir := fakeNamedDockerShim(t, "docker", `
rec "$@"
case "$1" in
info) echo 24.0.7 ;;
image) exit 0 ;;
ps) printf 'abc123\n' ;;
rm) exit 0 ;;
*) exit 9 ;;
esac
`)
		t.Setenv("PATH", filepath.Dir(bin))
		removed, err := SandboxStartupCheck(context.Background(), envOf(map[string]string{"BRAID_TOOL_SANDBOX": "docker"}))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(removed) != 1 || removed[0] != "abc123" {
			t.Fatalf("removed = %v, want [abc123]", removed)
		}
		want := []string{
			"info --format {{.ServerVersion}}",
			"image inspect braid-tool-sandbox:1",
			"ps -aq --filter label=braid.sandbox=1",
			"rm -f abc123",
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
	})

	t.Run("probe failure refuses startup without sweeping", func(t *testing.T) {
		bin, dir := fakeNamedDockerShim(t, "docker", `
rec "$@"
case "$1" in
info) exit 1 ;;
ps) exit 0 ;;
*) exit 9 ;;
esac
`)
		t.Setenv("PATH", filepath.Dir(bin))
		_, err := SandboxStartupCheck(context.Background(), envOf(map[string]string{"BRAID_TOOL_SANDBOX": "docker"}))
		if err == nil || !strings.Contains(err.Error(), "unreachable") {
			t.Fatalf("err = %v, want probe rejection", err)
		}
		lines := shimArgv(t, dir)
		if len(lines) != 1 || lines[0] != "info --format {{.ServerVersion}}" {
			t.Fatalf("invocations = %q, want probe info only (no sweep on probe failure)", lines)
		}
	})

	t.Run("custom image propagates to probe", func(t *testing.T) {
		bin, dir := fakeNamedDockerShim(t, "docker", `
rec "$@"
case "$1" in
info) echo 24.0.7 ;;
image) exit 0 ;;
ps) exit 0 ;;
*) exit 9 ;;
esac
`)
		t.Setenv("PATH", filepath.Dir(bin))
		if _, err := SandboxStartupCheck(context.Background(), envOf(map[string]string{
			"BRAID_TOOL_SANDBOX":       "docker",
			"BRAID_TOOL_SANDBOX_IMAGE": "reg.local/braid/sandbox:2",
		})); err != nil {
			t.Fatalf("err = %v", err)
		}
		lines := shimArgv(t, dir)
		if len(lines) < 2 || lines[1] != "image inspect reg.local/braid/sandbox:2" {
			t.Fatalf("invocations = %q, want custom image inspect", lines)
		}
	})

	t.Run("invalid mode env is loud", func(t *testing.T) {
		bin, dir := fakeNamedDockerShim(t, "docker", "rec \"$@\"\nexit 9")
		t.Setenv("PATH", filepath.Dir(bin))
		_, err := SandboxStartupCheck(context.Background(), envOf(map[string]string{"BRAID_TOOL_SANDBOX": "auto"}))
		if err == nil || !strings.Contains(err.Error(), "BRAID_TOOL_SANDBOX") {
			t.Fatalf("err = %v, want named-key rejection", err)
		}
		if lines := shimArgv(t, dir); len(lines) != 0 {
			t.Fatalf("invocations = %q, want zero docker contact", lines)
		}
	})

	t.Run("shutdown signal in probe window attributes to ctx", func(t *testing.T) {
		// 停机信号落在探测窗:exec 错误呈 "signal: killed" 不含 Canceled,
		// 必须归因回 ctx.Err()——worker main 的正常停机出口(exit 0)依赖
		// errors.Is(err, context.Canceled/DeadlineExceeded) 命中。
		// busy-loop 挂住:纯 sh 内建(测试 PATH 只含 shim 目录,sleep 等
		// 外部命令不可用),由 CommandContext 在 ctx 到点 SIGKILL。
		bin, _ := fakeNamedDockerShim(t, "docker", "case \"$1\" in\ninfo) while :; do :; done ;;\n*) exit 9 ;;\nesac")
		t.Setenv("PATH", filepath.Dir(bin))
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := SandboxStartupCheck(ctx, envOf(map[string]string{"BRAID_TOOL_SANDBOX": "docker"}))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want ctx deadline attribution", err)
		}
	})

	t.Run("sweep budget bounds a wedged daemon", func(t *testing.T) {
		// probe 应答后 daemon 在 ps 上挂死:裸生命周期 ctx 会无限悬挂,
		// 预算把它退化为响亮拒启(消息带预算与归因提示)。
		old := sandboxSweepTimeout
		sandboxSweepTimeout = 100 * time.Millisecond
		t.Cleanup(func() { sandboxSweepTimeout = old })
		bin, _ := fakeNamedDockerShim(t, "docker", "case \"$1\" in\ninfo) echo 24.0.7 ;;\nimage) exit 0 ;;\nps) while :; do :; done ;;\n*) exit 9 ;;\nesac")
		t.Setenv("PATH", filepath.Dir(bin))
		_, err := SandboxStartupCheck(context.Background(), envOf(map[string]string{"BRAID_TOOL_SANDBOX": "docker"}))
		if err == nil || !strings.Contains(err.Error(), "budget") {
			t.Fatalf("err = %v, want sweep budget rejection", err)
		}
	})
}
