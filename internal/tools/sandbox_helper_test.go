package tools

// 沙箱 helper 测试(T10.1 T5):容器内会话决策逻辑全离线可测(真 stdin/
// stdout 注入,信封往返真编解码)。分类契约锚(基线 §6 镜像 runner 序):
//   - 哨兵先于 ctx 门:工具携带三哨兵 → 信封回传对应码(宿主重抛真哨兵);
//   - ctx 取消/deadline(SIGTERM 经 cmd 层 signal ctx 传入)→ 无信封、
//     非零退出——deadline 的 infra 分类归宿在宿主,helper 不得以
//     tool_failed 信封改写;
//   - 其余工具错误 → tool_failed 信封;请求信封层失败(畸形/超限/空/
//     工具名不符/配置重建失败)→ tool_failed 信封(可观测诊断)。
// selftest:dump-env(凭证不可见 e2e 探针)+ alloc(8MiB 步长资源探针)。

import (
	"bufio"
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
	"path/filepath"
	"strings"
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
