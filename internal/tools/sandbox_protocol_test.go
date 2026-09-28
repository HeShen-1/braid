package tools

// 沙箱信封协议测试(T10.1,D-t101 #2/#6):stdin 请求信封往返保真;stdout
// 响应信封三哨兵码跨进程重抛(errors.Is 命中 runner.go 既有映射,分类
// 单点不动);未知码/畸形信封 = 普通错误(经 runner 默认 tool_failed)。

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSandboxEnvelopeRoundTrip(t *testing.T) {
	req := SandboxRequest{
		Tool:           "http",
		Config:         json.RawMessage(`{"api":"https://a.test"}`),
		Token:          "tok-xyz",
		IdempotencyKey: "att:42:1",
		Inputs:         json.RawMessage(`{"endpoint":"api","path":"/x"}`),
		DeadlineUnix:   1790582400,
	}
	raw, err := EncodeSandboxRequest(req)
	if err != nil {
		t.Fatalf("encode err = %v", err)
	}
	if strings.Contains(string(raw), "\n") {
		t.Fatalf("request envelope must be single-line JSON, got %q", raw)
	}
	var back SandboxRequest
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("decode err = %v", err)
	}
	if back.Tool != req.Tool || back.Token != req.Token || back.IdempotencyKey != req.IdempotencyKey || back.DeadlineUnix != req.DeadlineUnix {
		t.Fatalf("round trip lost fields: %+v", back)
	}
	if string(back.Config) != string(req.Config) || string(back.Inputs) != string(req.Inputs) {
		t.Fatalf("round trip lost payloads: config=%s inputs=%s", back.Config, back.Inputs)
	}
}

func TestSandboxReraiseSentinels(t *testing.T) {
	cases := []struct {
		code     string
		sentinel error
	}{
		{SandboxCodeSSRFBlocked, ErrSSRFBlocked},
		{SandboxCodeHostBlocked, ErrHostBlocked},
		{SandboxCodePathBlocked, ErrPathBlocked},
	}
	for _, c := range cases {
		t.Run("code "+c.code, func(t *testing.T) {
			stdout := fmt.Sprintf(`{"ok":false,"code":%q,"message":"rejected"}`, c.code)
			_, err := DecodeSandboxResponse([]byte(stdout))
			if err == nil {
				t.Fatalf("want error for %s", c.code)
			}
			if !errors.Is(err, c.sentinel) {
				t.Fatalf("err = %v, want wrapped %v", err, c.sentinel)
			}
			if !strings.Contains(err.Error(), "rejected") {
				t.Fatalf("err = %v, want message preserved", err)
			}
		})
	}

	t.Run("tool_failed carries no sentinel", func(t *testing.T) {
		_, err := DecodeSandboxResponse([]byte(`{"ok":false,"code":"tool_failed","message":"bad input"}`))
		if err == nil {
			t.Fatalf("want error")
		}
		for _, s := range []error{ErrSSRFBlocked, ErrHostBlocked, ErrPathBlocked} {
			if errors.Is(err, s) {
				t.Fatalf("err = %v, must not carry sentinel %v", err, s)
			}
		}
	})

	t.Run("unknown code is plain error", func(t *testing.T) {
		_, err := DecodeSandboxResponse([]byte(`{"ok":false,"code":"weird","message":"x"}`))
		if err == nil || !strings.Contains(err.Error(), "weird") {
			t.Fatalf("err = %v, want unknown-code rejection", err)
		}
	})

	t.Run("ok true returns output verbatim", func(t *testing.T) {
		res, err := DecodeSandboxResponse([]byte(`{"ok":true,"output":{"status":200}}`))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if string(res.Output) != `{"status":200}` && string(res.Output) != `{"status": 200}` {
			t.Fatalf("output = %s", res.Output)
		}
	})

	t.Run("ok true without output rejected", func(t *testing.T) {
		if _, err := DecodeSandboxResponse([]byte(`{"ok":true}`)); err == nil {
			t.Fatalf("want error for ok-without-output")
		}
	})

	badPayloads := []string{
		"",
		"not json",
		`{"ok":false}`,
		`[]`,
	}
	for _, p := range badPayloads {
		t.Run("malformed "+p, func(t *testing.T) {
			_, err := DecodeSandboxResponse([]byte(p))
			if err == nil {
				t.Fatalf("want error for malformed envelope %q", p)
			}
			for _, s := range []error{ErrSSRFBlocked, ErrHostBlocked, ErrPathBlocked} {
				if errors.Is(err, s) {
					t.Fatalf("malformed envelope must not carry sentinel %v", s)
				}
			}
		})
	}
}

func TestSandboxCodeForError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"bare ssrf", ErrSSRFBlocked, SandboxCodeSSRFBlocked},
		{"wrapped host", fmt.Errorf("outer: %w", ErrHostBlocked), SandboxCodeHostBlocked},
		{"url-error wrapped path", fmt.Errorf("Get %q: %w", "https://a.test", ErrPathBlocked), SandboxCodePathBlocked},
		{"plain error", errors.New("boom"), SandboxCodeToolFailed},
		{"nil", nil, SandboxCodeToolFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SandboxCodeForError(c.err); got != c.want {
				t.Fatalf("code = %q, want %q", got, c.want)
			}
		})
	}
}
