package tools

// 沙箱 helper 测试(T10.1 T5):容器内会话决策逻辑全离线可测(真 stdin/
// stdout 注入,信封往返真编解码)。分类契约锚(基线 §6 镜像 runner 序):
//   - 哨兵先于 ctx 门:工具携带三哨兵 → 信封回传对应码(宿主重抛真哨兵);
//   - ctx 取消/deadline(SIGTERM 经 cmd 层 signal ctx 传入)→ 无信封、
//     非零退出——deadline 的 infra 分类归宿在宿主,helper 不得以
//     tool_failed 信封改写;
//   - 其余工具错误 → tool_failed 信封;请求信封层失败(畸形/超限/空/
//     工具名不符/配置重建失败)→ tool_failed 信封(可观测诊断)。
// selftest:dump-env(凭证不可见 e2e 探针)+ alloc(8MiB 步长资源探针)
//   + serve(宿主侧 TLS server)+ T11 资源探针家族(spin/forkchain/fds/
//   spam-stderr/zombies——自再执行类经真 helper 二进制子进程,不进程内
//   重入测试二进制)。

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// helperEnvelope 用真编码器构造请求信封(单行 JSON + 换行,模拟 carrier 写入)。
func helperEnvelope(t *testing.T, req SandboxRequest) string {
	t.Helper()
	raw, err := EncodeSandboxRequest(req)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	return string(raw) + "\n"
}

// decodeHelperEnvelope 直接解 SandboxResponse(不经哨兵重抛,便于断言原样码)。
func decodeHelperEnvelope(t *testing.T, out string) SandboxResponse {
	t.Helper()
	var resp SandboxResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); err != nil {
		t.Fatalf("helper stdout not an envelope: %v (%q)", err, out)
	}
	return resp
}

func TestSandboxHelperFileReadSession(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(file, []byte("hello-sandbox"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := json.RawMessage(`{"dirs":["` + dir + `"]}`)
	inputs := json.RawMessage(`{"path":` + mustJSON(t, file) + `}`)

	t.Run("ok session echoes tool output", func(t *testing.T) {
		var out strings.Builder
		err := RunSandboxHelper(context.Background(),
			[]string{"file_read"},
			strings.NewReader(helperEnvelope(t, SandboxRequest{
				Tool: "file_read", Config: cfg, Token: "tok-1", IdempotencyKey: "att:9:1",
				Inputs: inputs, DeadlineMs: 30000,
			})), &out, nil)
		if err != nil {
			t.Fatalf("helper: %v", err)
		}
		resp := decodeHelperEnvelope(t, out.String())
		if !resp.OK || resp.Code != "" {
			t.Fatalf("resp = %+v, want ok session", resp)
		}
		var output map[string]any
		if err := json.Unmarshal(resp.Output, &output); err != nil {
			t.Fatalf("output not object: %v (%s)", err, resp.Output)
		}
		if !strings.Contains(string(resp.Output), "hello-sandbox") {
			t.Fatalf("output missing file content: %s", resp.Output)
		}
	})

	t.Run("path outside whitelist maps to path_blocked envelope", func(t *testing.T) {
		var out strings.Builder
		err := RunSandboxHelper(context.Background(),
			[]string{"file_read"},
			strings.NewReader(helperEnvelope(t, SandboxRequest{
				Tool: "file_read", Config: cfg,
				Inputs: json.RawMessage(`{"path":"/etc/passwd"}`),
			})), &out, nil)
		if err != nil {
			t.Fatalf("helper: %v", err)
		}
		resp := decodeHelperEnvelope(t, out.String())
		if resp.OK || resp.Code != SandboxCodePathBlocked {
			t.Fatalf("resp = %+v, want ok=false path_blocked", resp)
		}
	})
}

func TestSandboxHelperRequestLayer(t *testing.T) {
	envelopeFail := func(t *testing.T, out string, wantIn string) {
		t.Helper()
		resp := decodeHelperEnvelope(t, out)
		if resp.OK || resp.Code != SandboxCodeToolFailed {
			t.Fatalf("resp = %+v, want ok=false tool_failed", resp)
		}
		if !strings.Contains(resp.Message, wantIn) {
			t.Fatalf("message = %q, want containing %q", resp.Message, wantIn)
		}
	}

	t.Run("empty stdin", func(t *testing.T) {
		var out strings.Builder
		if err := RunSandboxHelper(context.Background(), []string{"file_read"}, strings.NewReader(""), &out, nil); err != nil {
			t.Fatalf("helper: %v", err)
		}
		envelopeFail(t, out.String(), "empty")
	})

	t.Run("malformed json", func(t *testing.T) {
		var out strings.Builder
		if err := RunSandboxHelper(context.Background(), []string{"http"}, strings.NewReader("not-json\n"), &out, nil); err != nil {
			t.Fatalf("helper: %v", err)
		}
		envelopeFail(t, out.String(), "not valid JSON")
	})

	t.Run("oversize request envelope", func(t *testing.T) {
		var out strings.Builder
		big := strings.NewReader(`{"tool":"http","inputs":"` + strings.Repeat("x", sandboxRequestLimit+1) + `"}` + "\n")
		if err := RunSandboxHelper(context.Background(), []string{"http"}, big, &out, nil); err != nil {
			t.Fatalf("helper: %v", err)
		}
		envelopeFail(t, out.String(), "exceeds")
	})

	t.Run("envelope tool name mismatches argv", func(t *testing.T) {
		var out strings.Builder
		err := RunSandboxHelper(context.Background(),
			[]string{"file_read"},
			strings.NewReader(helperEnvelope(t, SandboxRequest{Tool: "http"})), &out, nil)
		if err != nil {
			t.Fatalf("helper: %v", err)
		}
		envelopeFail(t, out.String(), "mismatch")
	})

	t.Run("config rebuild failure is observable", func(t *testing.T) {
		var out strings.Builder
		err := RunSandboxHelper(context.Background(),
			[]string{"file_read"},
			strings.NewReader(helperEnvelope(t, SandboxRequest{Tool: "file_read", Config: json.RawMessage(`{"dirs":[]}`)})), &out, nil)
		if err != nil {
			t.Fatalf("helper: %v", err)
		}
		envelopeFail(t, out.String(), "empty dir")
	})

	t.Run("inputs not an object", func(t *testing.T) {
		var out strings.Builder
		err := RunSandboxHelper(context.Background(),
			[]string{"file_read"},
			strings.NewReader(helperEnvelope(t, SandboxRequest{
				Tool: "file_read", Config: json.RawMessage(`{"dirs":["/var/braid/docs"]}`),
				Inputs: json.RawMessage(`"just a string"`),
			})), &out, nil)
		if err != nil {
			t.Fatalf("helper: %v", err)
		}
		envelopeFail(t, out.String(), "inputs")
	})

	t.Run("unknown argv mode reports tool_failed envelope", func(t *testing.T) {
		var out strings.Builder
		if err := RunSandboxHelper(context.Background(), []string{"wget"}, strings.NewReader(helperEnvelope(t, SandboxRequest{Tool: "wget"})), &out, nil); err != nil {
			t.Fatalf("helper: %v", err)
		}
		envelopeFail(t, out.String(), "unknown")
	})
}

func TestSandboxHelperContextPaths(t *testing.T) {
	dir := t.TempDir()
	cfg := json.RawMessage(`{"dirs":["` + dir + `"]}`)
	inputs := json.RawMessage(`{"path":"` + filepath.Join(dir, "a.txt") + `"}`)

	t.Run("pre-canceled ctx aborts without envelope", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var out strings.Builder
		err := RunSandboxHelper(ctx, []string{"file_read"},
			strings.NewReader(helperEnvelope(t, SandboxRequest{Tool: "file_read", Config: cfg, Inputs: inputs})), &out, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if out.Len() != 0 {
			t.Fatalf("stdout = %q, want no envelope on abort", out.String())
		}
	})

	t.Run("past deadline aborts without envelope", func(t *testing.T) {
		var out strings.Builder
		err := RunSandboxHelper(context.Background(), []string{"file_read"},
			strings.NewReader(helperEnvelope(t, SandboxRequest{
				Tool: "file_read", Config: cfg, Inputs: inputs,
				DeadlineMs: -1000, // 已到期(负值 → 立即超时)
			})), &out, nil)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want DeadlineExceeded (no envelope; host owns infra class)", err)
		}
		if out.Len() != 0 {
			t.Fatalf("stdout = %q, want no envelope on deadline abort", out.String())
		}
	})

	t.Run("stdin read honors ctx cancellation", func(t *testing.T) {
		pr, pw := io.Pipe()
		defer func() { _ = pw.Close() }()
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()
		var out strings.Builder
		err := RunSandboxHelper(ctx, []string{"http"}, pr, &out, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want ctx error from blocked read", err)
		}
	})

	t.Run("stdin read has its own timeout", func(t *testing.T) {
		old := sandboxHelperReadTimeout
		sandboxHelperReadTimeout = 100 * time.Millisecond
		defer func() { sandboxHelperReadTimeout = old }()
		pr, pw := io.Pipe()
		defer func() { _ = pw.Close() }()
		var out strings.Builder
		err := RunSandboxHelper(context.Background(), []string{"http"}, pr, &out, nil)
		if err == nil || !strings.Contains(err.Error(), "stdin") {
			t.Fatalf("err = %v, want stdin read timeout", err)
		}
	})

	t.Run("sentinel envelope survives live deadline", func(t *testing.T) {
		// 信封 deadline 在场(远未到):path_blocked 哨兵正常跨进程回传。
		// 哨兵-vs-deadline 的同窗竞争序由 TestSandboxHelperClassifyToolError
		// 纯函数表测钉死(会话级无法确定性构造 µs 竞争窗)。
		var out strings.Builder
		err := RunSandboxHelper(context.Background(), []string{"file_read"},
			strings.NewReader(helperEnvelope(t, SandboxRequest{
				Tool: "file_read", Config: cfg,
				Inputs:     json.RawMessage(`{"path":"/etc/passwd"}`),
				DeadlineMs: 2000,
			})), &out, nil)
		if err != nil {
			t.Fatalf("helper: %v", err)
		}
		resp := decodeHelperEnvelope(t, out.String())
		if resp.OK || resp.Code != SandboxCodePathBlocked {
			t.Fatalf("resp = %+v, want path_blocked envelope under live deadline", resp)
		}
	})
}

// TestSandboxHelperInputsNumberFidelity:inputs 数值保真(全仓 UseNumber
// 纪律,eval.go/nodes-tool.go 同口径)——>2^53 整数与小数词素原文往返,
// 本地/沙箱两模式工具所见逐字节一致(T5 对抗审计确认 HIGH 的回归锚)。
func TestSandboxHelperInputsNumberFidelity(t *testing.T) {
	inputs, err := decodeSandboxInputs(json.RawMessage(`{"since_id":9007199254740993,"price":1.50}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	big, ok := inputs["since_id"].(json.Number)
	if !ok || big.String() != "9007199254740993" {
		t.Fatalf("since_id = %#v, want json.Number 9007199254740993 verbatim (>2^53)", inputs["since_id"])
	}
	price, ok := inputs["price"].(json.Number)
	if !ok || price.String() != "1.50" {
		t.Fatalf("price = %#v, want json.Number 1.50 verbatim (trailing zero)", inputs["price"])
	}
}

// TestSandboxHelperClassifyToolError 钉死工具错误二分序(镜像 runner:
// 哨兵先于 ctx 门)——µs 级同窗竞争无法在会话层确定性构造,以纯函数表
// 测锚定语义。
func TestSandboxHelperClassifyToolError(t *testing.T) {
	cases := []struct {
		name      string
		toolErr   error
		ctxErr    error
		wantAbort error
		wantCode  string
	}{
		{"sentinel outranks fired deadline", fmt.Errorf("dial: %w", ErrSSRFBlocked), context.DeadlineExceeded, nil, SandboxCodeSSRFBlocked},
		{"sentinel outranks cancel", fmt.Errorf("read: %w", ErrPathBlocked), context.Canceled, nil, SandboxCodePathBlocked},
		{"plain error with live ctx envelopes tool_failed", errors.New("boom"), nil, nil, SandboxCodeToolFailed},
		{"plain error under fired deadline aborts without envelope", errors.New("boom"), context.DeadlineExceeded, context.DeadlineExceeded, ""},
		{"plain error under cancel aborts without envelope", errors.New("boom"), context.Canceled, context.Canceled, ""},
		{"nil tool error is nil", nil, context.DeadlineExceeded, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, abort := sandboxClassifyToolError(c.toolErr, c.ctxErr)
			if c.wantAbort != nil {
				if !errors.Is(abort, c.wantAbort) {
					t.Fatalf("abort = %v, want %v", abort, c.wantAbort)
				}
				if resp != nil {
					t.Fatalf("resp = %+v, want no envelope on abort", resp)
				}
				return
			}
			if abort != nil {
				t.Fatalf("abort = %v, want nil", abort)
			}
			if c.wantCode == "" {
				if resp != nil {
					t.Fatalf("resp = %+v, want nil", resp)
				}
				return
			}
			if resp == nil || resp.OK || resp.Code != c.wantCode {
				t.Fatalf("resp = %+v, want code %q", resp, c.wantCode)
			}
		})
	}
}

func TestSandboxHelperWatchdog(t *testing.T) {
	t.Run("clean session does not trip hard exit", func(t *testing.T) {
		old := sandboxHelperExitGrace
		sandboxHelperExitGrace = 200 * time.Millisecond
		defer func() { sandboxHelperExitGrace = old }()

		fired := make(chan struct{}, 1)
		dir := t.TempDir()
		var out strings.Builder
		// deadline 远未到(≥2s,避开 Unix() 秒截断相位),快会话干净返回,
		// watchdog 必须解除武装。
		err := RunSandboxHelper(context.Background(), []string{"file_read"},
			strings.NewReader(helperEnvelope(t, SandboxRequest{
				Tool:       "file_read",
				Config:     json.RawMessage(`{"dirs":["` + dir + `"]}`),
				Inputs:     json.RawMessage(`{"path":"` + filepath.Join(dir, "x") + `"}`),
				DeadlineMs: 2000,
			})), &out, func() { close(fired) })
		if err != nil {
			t.Fatalf("helper: %v", err)
		}
		select {
		case <-fired:
			t.Fatal("hard exit fired on a clean session; watchdog must be disarmed on return")
		case <-time.After(500 * time.Millisecond):
		}
	})
}

func TestSandboxHelperSelftest(t *testing.T) {
	t.Run("dump-env serializes full env as json object", func(t *testing.T) {
		t.Setenv("BRAID_FAKE_MASTER_KEY", "super-secret-value")
		var out strings.Builder
		if err := RunSandboxHelper(context.Background(), []string{"selftest", "dump-env"}, strings.NewReader(""), &out, nil); err != nil {
			t.Fatalf("helper: %v", err)
		}
		var env map[string]string
		if err := json.Unmarshal([]byte(out.String()), &env); err != nil {
			t.Fatalf("dump-env not a JSON object: %v (%s)", err, out.String())
		}
		if env["BRAID_FAKE_MASTER_KEY"] != "super-secret-value" {
			t.Fatalf("injected env missing: %v", env)
		}
	})

	t.Run("alloc commits requested memory in 8 MiB steps and reports", func(t *testing.T) {
		var out strings.Builder
		if err := RunSandboxHelper(context.Background(), []string{"selftest", "alloc", "16"}, strings.NewReader(""), &out, nil); err != nil {
			t.Fatalf("helper: %v", err)
		}
		if !strings.Contains(out.String(), "allocated 16 MiB") {
			t.Fatalf("out = %q, want allocation report", out.String())
		}
	})

	allocBad := []string{"0", "-4", "banana", "999999"}
	for _, a := range allocBad {
		t.Run("alloc rejects "+a, func(t *testing.T) {
			err := RunSandboxHelper(context.Background(), []string{"selftest", "alloc", a}, strings.NewReader(""), &strings.Builder{}, nil)
			if err == nil || !strings.Contains(err.Error(), "alloc") {
				t.Fatalf("err = %v, want alloc bound rejection", err)
			}
		})
	}

	t.Run("serve hosts TLS with marker+path echo and exits on stdin EOF", func(t *testing.T) {
		// 单测绑 127.0.0.1(免 alias 免特权);叶对不走 generateSandboxCA
		// ——该 CA 已被 NameConstraints 钉死 198.18/15 段(T10 裁决),
		// 段外 IP 的叶会拒。serve 机制与 CA 无绑定:任何合法叶对可服务,
		// 分离点即在此。hermetic 段上的真 CA 链路由 e2e 证明。
		certPEM, keyPEM := sandboxServeUnitTestLeaf(t)
		dir := t.TempDir()
		certPath := filepath.Join(dir, "leaf.crt")
		keyPath := filepath.Join(dir, "leaf.key")
		if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
			t.Fatalf("write leaf cert: %v", err)
		}
		if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
			t.Fatalf("write leaf key: %v", err)
		}

		stdinR, stdinW := io.Pipe()
		outR, outW, err := os.Pipe()
		if err != nil {
			t.Fatalf("pipe: %v", err)
		}
		t.Cleanup(func() { // os.Pipe 是真实 fd,不关则每 run 泄 2 个
			_ = outR.Close()
			_ = outW.Close()
		})
		done := make(chan error, 1)
		go func() {
			done <- RunSandboxHelper(context.Background(),
				[]string{"selftest", "serve", "127.0.0.1:0", certPath, keyPath},
				stdinR, outW, nil)
		}()

		line, err := bufio.NewReader(outR).ReadString('\n')
		if err != nil {
			t.Fatalf("read listening line: %v", err)
		}
		addr := strings.TrimSpace(strings.TrimPrefix(line, "listening "))
		if host, _, err := net.SplitHostPort(addr); err != nil || host != "127.0.0.1" {
			t.Fatalf("listening line = %q, want host 127.0.0.1 (err=%v)", line, err)
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(certPEM) {
			t.Fatal("unit leaf not loadable into pool")
		}
		client := &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		}}
		t.Cleanup(client.CloseIdleConnections)
		resp, err := client.Get("https://" + addr + "/unit/probe")
		if err != nil {
			t.Fatalf("GET hermetic server: %v", err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if resp.StatusCode != http.StatusOK || string(body) != sandboxServeMarker+" /unit/probe" {
			t.Fatalf("resp = %d %q, want 200 %q", resp.StatusCode, body, sandboxServeMarker+" /unit/probe")
		}

		// stdin EOF = 主停机信号:helper 必须有界返回(零孤儿)。
		if err := stdinW.Close(); err != nil {
			t.Fatalf("close stdin: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("serve session: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("serve did not exit on stdin EOF within 5s")
		}
	})

	t.Run("serve rejects wildcard and non-IP hosts", func(t *testing.T) {
		for _, host := range []string{"0.0.0.0:0", "[::]:0", "localhost:0"} {
			err := RunSandboxHelper(context.Background(),
				[]string{"selftest", "serve", host, "cert.pem", "key.pem"},
				strings.NewReader(""), &strings.Builder{}, nil)
			if err == nil || !strings.Contains(err.Error(), "explicit") {
				t.Fatalf("host %q: err = %v, want explicit-IP rejection", host, err)
			}
		}
	})

	t.Run("serve rejects wrong argv shape", func(t *testing.T) {
		err := RunSandboxHelper(context.Background(), []string{"selftest", "serve"},
			strings.NewReader(""), &strings.Builder{}, nil)
		if err == nil || !strings.Contains(err.Error(), "requires") {
			t.Fatalf("err = %v, want argv shape rejection", err)
		}
	})

	t.Run("unknown selftest mode", func(t *testing.T) {
		err := RunSandboxHelper(context.Background(), []string{"selftest", "explode"}, strings.NewReader(""), &strings.Builder{}, nil)
		if err == nil || !strings.Contains(err.Error(), "selftest") {
			t.Fatalf("err = %v, want mode rejection", err)
		}
	})

	// T11 资源探针:spin 可进程内测;forkchain/zombies 会 /proc/self/exe
	// 自再执行(进程内跑 = 测试二进制重入),spam-stderr 要真实 stderr
	// 通道——三者一律经真 helper 二进制子进程(与 e2e 镜像内同一二进制)。
	t.Run("spin burns at least the requested wall seconds", func(t *testing.T) {
		var out strings.Builder
		start := time.Now()
		if err := RunSandboxHelper(context.Background(), []string{"selftest", "spin", "1"}, strings.NewReader(""), &out, nil); err != nil {
			t.Fatalf("helper: %v", err)
		}
		if elapsed := time.Since(start); elapsed < time.Second {
			t.Fatalf("spin returned after %v, want >= 1s", elapsed)
		}
		if !strings.Contains(out.String(), "spun 1s") {
			t.Fatalf("out = %q, want spin report", out.String())
		}
	})

	spinBad := []string{"0", "-1", "banana", "301"}
	for _, s := range spinBad {
		t.Run("spin rejects "+s, func(t *testing.T) {
			err := RunSandboxHelper(context.Background(), []string{"selftest", "spin", s}, strings.NewReader(""), &strings.Builder{}, nil)
			if err == nil || !strings.Contains(err.Error(), "spin") {
				t.Fatalf("err = %v, want spin bound rejection", err)
			}
		})
	}

	t.Run("fds stops at a lowered nofile rlimit with emfile", func(t *testing.T) {
		// 进程内安全(纯 open 循环,无自再执行):把软限降到 64,循环必须
		// 在 ≤64 处 EMFILE 停止并正常退出(软失败语义)。计数下限不设:
		// 测试进程预持 fd 数(早前用例的 listener/pipe)≥ 限时首开即
		// EMFILE、n=0,执法面由 stop=emfile 锚定。
		var rl syscall.Rlimit
		if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
			t.Fatalf("getrlimit: %v", err)
		}
		saved := rl
		if rl.Cur > 64 {
			rl.Cur = 64
			if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
				t.Fatalf("setrlimit: %v", err)
			}
		}
		defer func() {
			_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &saved)
		}()
		var out strings.Builder
		if err := RunSandboxHelper(context.Background(), []string{"selftest", "fds", "4096"}, strings.NewReader(""), &out, nil); err != nil {
			t.Fatalf("helper: %v", err)
		}
		report := out.String()
		if !strings.Contains(report, "stop=emfile") {
			t.Fatalf("report = %q, want stop=emfile under rlimit 64", report)
		}
		var n int
		if _, err := fmt.Sscanf(report, "fds=%d", &n); err != nil || n < 0 || n > 64 {
			t.Fatalf("report = %q, want 0 <= fds count <= 64 (err=%v)", report, err)
		}
	})

	fdsBad := []string{"0", "banana", "1000001"}
	for _, a := range fdsBad {
		t.Run("fds rejects "+a, func(t *testing.T) {
			err := RunSandboxHelper(context.Background(), []string{"selftest", "fds", a}, strings.NewReader(""), &strings.Builder{}, nil)
			if err == nil || !strings.Contains(err.Error(), "fds") {
				t.Fatalf("err = %v, want fds bound rejection", err)
			}
		})
	}

	// helperBin 构建一次、三处子用例复用(全仓「每进程构建一次」口径:
	// 镜像侧 sandboxEnsuredImages 同款 memo 理由,CI 20min 预算)。目录由
	// 父测试持有——子用例的 t.TempDir() 在其结束时删除,跨子用例 memo
	// 会悬空路径。
	helperDir := t.TempDir()
	var helperBinOnce string
	helperBin := func(t *testing.T) string {
		t.Helper()
		if helperBinOnce == "" {
			sandboxBuildHelperBinary(t, helperDir)
			helperBinOnce = filepath.Join(helperDir, "braid-sandbox-exec")
		}
		return helperBinOnce
	}

	t.Run("forkchain reaches depth cap without eagain on host", func(t *testing.T) {
		// 宿主(挂载层级根 pids.max = max/不可读)→ 链走到 max 有界收口
		// stop=cap(非真炸弹的设计半边);EAGAIN/budget 终止半边只在容器
		// e2e 可证(pids-limit 执法)。期望从 sandboxPidsBudget 自身推导,
		// 消除环境耦合:套件跑在带数值 pids 限额的外层容器内时,budget
		// 收口(depth < 3)是正确行为,不是失败。
		out, err := exec.Command(helperBin(t), "selftest", "forkchain", "0", "3").CombinedOutput()
		if err != nil {
			t.Fatalf("forkchain subprocess: %v\n%s", err, out)
		}
		var depth int
		var stop string
		if _, perr := fmt.Sscanf(string(out), "forkchain depth=%d stop=%s", &depth, &stop); perr != nil {
			t.Fatalf("forkchain report = %q, want 'forkchain depth=N stop=…'", out)
		}
		_, lim, budgetOK := sandboxPidsBudget()
		tightBudget := budgetOK && lim > 0 && lim <= 3+sandboxHelperPidsMargin
		switch stop {
		case "cap":
			if depth != 3 {
				t.Fatalf("stop=cap at depth %d, want 3", depth)
			}
		case "budget", "eagain":
			if !tightBudget {
				t.Fatalf("stop=%s at depth %d with unbounded host pids budget, want stop=cap depth=3 (report %q)", stop, depth, out)
			}
		default:
			t.Fatalf("stop = %q, want cap|budget|eagain (report %q)", stop, out)
		}
	})

	forkchainBad := [][]string{{"0"}, {"1", "0"}, {"2", "1"}, {"0", "banana"}, {"-1", "3"}, {"0", "1001"}}
	for _, a := range forkchainBad {
		t.Run("forkchain rejects "+strings.Join(a, " "), func(t *testing.T) {
			argv := append([]string{"selftest", "forkchain"}, a...)
			err := RunSandboxHelper(context.Background(), argv, strings.NewReader(""), &strings.Builder{}, nil)
			if err == nil || !strings.Contains(err.Error(), "forkchain") {
				t.Fatalf("err = %v, want forkchain bound rejection", err)
			}
		})
	}

	t.Run("spam-stderr writes the requested bytes plus tail sentinel", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		cmd := exec.Command(helperBin(t), "selftest", "spam-stderr", "2")
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("spam-stderr subprocess: %v", err)
		}
		if want := 2<<20 + len("braid-e2e-stderr-spam-done 2\n"); stderr.Len() != want {
			t.Fatalf("stderr bytes = %d, want %d", stderr.Len(), want)
		}
		if !strings.HasSuffix(stderr.String(), "braid-e2e-stderr-spam-done 2\n") {
			t.Fatalf("stderr tail = %q, want sentinel line last (rotation keeps the tail)", stderr.Bytes()[stderr.Len()-60:])
		}
		if !strings.Contains(stdout.String(), "spammed 2 MiB to stderr") {
			t.Fatalf("stdout = %q, want spam report", stdout.String())
		}
	})

	spamBad := []string{"0", "513", "banana"}
	for _, a := range spamBad {
		t.Run("spam-stderr rejects "+a, func(t *testing.T) {
			err := RunSandboxHelper(context.Background(), []string{"selftest", "spam-stderr", a}, strings.NewReader(""), &strings.Builder{}, nil)
			if err == nil || !strings.Contains(err.Error(), "spam-stderr") {
				t.Fatalf("err = %v, want spam-stderr bound rejection", err)
			}
		})
	}

	t.Run("zombies reports a scan count on host", func(t *testing.T) {
		// 宿主 PID 1(systemd)收割孤儿 → 计数 0;断言只锚「扫描真跑 +
		// 报告词形」——tini 收割语义的 0 断言属容器 e2e(--init 在场才
		// 是被测面)。等待子进程收口的窗口给足(僵尸转交是异步的)。
		var out bytes.Buffer
		cmd := exec.Command(helperBin(t), "selftest", "zombies", "4")
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("zombies subprocess: %v (%s)", err, out.String())
		}
		var z int
		if _, err := fmt.Sscanf(out.String(), "zombies=%d", &z); err != nil || z < 0 {
			t.Fatalf("out = %q, want zombies=<int> report (err=%v)", out.String(), err)
		}
	})

	zombiesBad := []string{"0", "65", "banana"}
	for _, a := range zombiesBad {
		t.Run("zombies rejects "+a, func(t *testing.T) {
			err := RunSandboxHelper(context.Background(), []string{"selftest", "zombies", a}, strings.NewReader(""), &strings.Builder{}, nil)
			if err == nil || !strings.Contains(err.Error(), "zombies") {
				t.Fatalf("err = %v, want zombies bound rejection", err)
			}
		})
	}

	t.Run("no argv", func(t *testing.T) {
		err := RunSandboxHelper(context.Background(), nil, strings.NewReader(""), &strings.Builder{}, nil)
		if err == nil || !strings.Contains(err.Error(), "usage") {
			t.Fatalf("err = %v, want usage error", err)
		}
	})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// sandboxServeUnitTestLeaf 自签叶对(127.0.0.1 SAN,ServerAuth):serve 单测
// 的服务身份。刻意不走去 generateSandboxCA——该 CA 的 NameConstraints 已
// 把可签 IP 钉死在 198.18/15 段(T10 裁决),单测绑 127.0.0.1 免 alias 免
// 特权,段外叶必被拒;serve 机制消费任意合法叶对,与 CA 策略正交。
func sandboxServeUnitTestLeaf(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate unit leaf key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{"braid"}, CommonName: "braid-serve-unit"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create unit leaf: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal unit leaf key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

// TestSandboxHelperUsesPinnedEncoder:helper 信封必须经 EncodeSandboxResponse
// 输出——错误 message 中的 &<> 保持字面量(裸 json.Marshal 会 1→6 字节
// HTML 转义,违反协议层钉死的非转义纪律)。
func TestSandboxHelperUsesPinnedEncoder(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "a&b")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := RunSandboxHelper(context.Background(), []string{"file_read"},
		strings.NewReader(helperEnvelope(t, SandboxRequest{
			Tool:   "file_read",
			Config: json.RawMessage(`{"dirs":["` + dir + `"]}`),
			Inputs: json.RawMessage(`{"path":` + mustJSON(t, filepath.Join(dir, "missing.txt")) + `}`),
		})), &out, nil); err != nil {
		t.Fatalf("helper: %v", err)
	}
	if !strings.Contains(out.String(), "a&b") {
		t.Fatalf("envelope = %q, want literal & preserved (pinned encoder)", out.String())
	}
	if strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("envelope lines != 1: %q", out.String())
	}
}

// TestSandboxSelftestEgressDialRaw(T10.2):egress-dial/raw-dial 探针的报告
// 形态——两探针都 exit 0,e2e 按打印行断言(拒答/直连拒绝的网络层证据)。
func TestSandboxSelftestEgressDialRaw(t *testing.T) {
	echoAddr := startEchoTarget(t)
	echoHost, echoPort := targetAddrHostPort(t, echoAddr)
	proxyAddr := startEgressProxy(t, EgressConfig{
		Allow:       map[string]map[int]struct{}{echoHost: {echoPort: {}}},
		IPForbidden: allowAllForbiddenOff,
	})

	run := func(t *testing.T, argv ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := RunSandboxHelper(context.Background(), argv, strings.NewReader(""), &out, nil); err != nil {
			t.Fatalf("RunSandboxHelper(%v): %v", argv, err)
		}
		return out.String()
	}

	t.Run("egress-dial allowlisted authority reports status line", func(t *testing.T) {
		got := run(t, "selftest", "egress-dial", proxyAddr, echoAddr)
		if !strings.HasPrefix(got, "HTTP/1.1 200 ") {
			t.Fatalf("out = %q, want 200 status line", got)
		}
	})
	t.Run("egress-dial non-allowlisted authority reports denial line", func(t *testing.T) {
		got := run(t, "selftest", "egress-dial", proxyAddr, "evil.example:443")
		if !strings.HasPrefix(got, "HTTP/1.1 403 "+egressReasonHostBlocked) {
			t.Fatalf("out = %q, want 403 %s", got, egressReasonHostBlocked)
		}
	})
	t.Run("egress-dial requires two args", func(t *testing.T) {
		var out bytes.Buffer
		err := RunSandboxHelper(context.Background(), []string{"selftest", "egress-dial", proxyAddr}, strings.NewReader(""), &out, nil)
		if err == nil || !strings.Contains(err.Error(), "egress-dial requires") {
			t.Fatalf("err = %v, want usage rejection", err)
		}
	})
	t.Run("raw-dial reachable reports connected", func(t *testing.T) {
		got := run(t, "selftest", "raw-dial", echoAddr)
		if got != "connected "+echoAddr+"\n" {
			t.Fatalf("out = %q, want connected line", got)
		}
	})
	t.Run("raw-dial unreachable reports failed and exits zero", func(t *testing.T) {
		got := run(t, "selftest", "raw-dial", "127.0.0.1:1")
		if !strings.HasPrefix(got, "failed 127.0.0.1:1:") {
			t.Fatalf("out = %q, want failed line", got)
		}
	})
}
