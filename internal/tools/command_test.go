package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// 验收面:沙箱环境(子进程 env)不含平台主凭证。测试进程注入假主凭证,
// 断言 CommandTool 子进程完全不可见(白名单构造,非黑名单过滤)。
func TestCommandToolEnvAllowlist(t *testing.T) {
	t.Setenv("BRAID_MYSQL_DSN", "mysql://secret-user:secret-pass@db/braid")
	t.Setenv("BRAID_LLM_API_KEY", "sk-platform-master-key")
	t.Setenv("BRAID_TOOL_TOKEN_SECRET", "hmac-signing-secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "cloud-cred")

	tool, err := NewCommandTool(CommandSpec{Name: "dump_env", Path: "/bin/sh", Args: []string{"-c", "env | sort"}})
	if err != nil {
		t.Fatalf("NewCommandTool: %v", err)
	}
	res, err := tool.Run(context.Background(), Call{
		Inputs:         map[string]any{},
		Token:          "tok-xyz",
		IdempotencyKey: "att:1:1",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	env := string(res.Output)

	// 白名单成员:PATH(可执行解析)、LANG、两个注入变量。
	for _, want := range []string{"PATH=", "LANG=", "BRAID_TOOL_TOKEN=tok-xyz", "BRAID_ATTEMPT_KEY=att:1:1"} {
		if !strings.Contains(env, want) {
			t.Errorf("sandbox env missing allowlisted %q", want)
		}
	}
	// 主凭证模式:任何出现即失败(白名单构造的面,而非逐项黑名单)。
	for _, secret := range []string{"secret-pass", "sk-platform-master-key", "hmac-signing-secret", "cloud-cred", "BRAID_MYSQL_DSN", "BRAID_LLM_API_KEY"} {
		if strings.Contains(env, secret) {
			t.Errorf("sandbox env leaks platform credential %q", secret)
		}
	}
}

func TestCommandToolStdinInputs(t *testing.T) {
	tool, err := NewCommandTool(CommandSpec{Name: "cat_tool", Path: "/bin/sh", Args: []string{"-c", "cat"}})
	if err != nil {
		t.Fatalf("NewCommandTool: %v", err)
	}
	res, err := tool.Run(context.Background(), Call{
		Inputs: map[string]any{"path": "/tmp/a.txt", "limit": json.Number("10")},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(res.Output, &got); err != nil {
		t.Fatalf("output not JSON: %v (%s)", err, res.Output)
	}
	// cat 回显 stdin:inputs 经 stdin JSON 传递(保真往返)。
	if got["path"] != "/tmp/a.txt" || got["limit"] != float64(10) {
		t.Fatalf("inputs round-trip = %v", got)
	}
}

func TestCommandToolNonZeroExit(t *testing.T) {
	tool, err := NewCommandTool(CommandSpec{Name: "boom", Path: "/bin/sh", Args: []string{"-c", "echo boom-detail >&2; exit 3"}})
	if err != nil {
		t.Fatalf("NewCommandTool: %v", err)
	}
	_, err = tool.Run(context.Background(), Call{Inputs: map[string]any{}})
	if err == nil {
		t.Fatal("non-zero exit returned nil error")
	}
	if !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "boom-detail") {
		t.Fatalf("err = %v, want exit status + stderr summary", err)
	}
}

func TestCommandToolCtxCancel(t *testing.T) {
	// 直接 exec sleep(不经 shell 中转):CommandContext 的 kill 精确命中本体,
	// 不依赖 shell 是否对单命令做 exec 优化。
	tool, err := NewCommandTool(CommandSpec{Name: "sleepy", Path: "/bin/sleep", Args: []string{"30"}})
	if err != nil {
		t.Fatalf("NewCommandTool: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = tool.Run(ctx, Call{Inputs: map[string]any{}})
	if err == nil {
		t.Fatal("cancelled run returned nil error")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("err = %v, want ctx error passthrough", err)
	}
}

func TestCommandToolNameAndValidation(t *testing.T) {
	if _, err := NewCommandTool(CommandSpec{Name: "", Path: "/bin/sh"}); err == nil {
		t.Fatal("empty name accepted")
	}
	if _, err := NewCommandTool(CommandSpec{Name: "x", Path: ""}); err == nil {
		t.Fatal("empty path accepted")
	}
	tool, err := NewCommandTool(CommandSpec{Name: "x", Path: "/bin/true"})
	if err != nil {
		t.Fatalf("NewCommandTool: %v", err)
	}
	if tool.Name() != "x" {
		t.Fatalf("Name() = %q", tool.Name())
	}
}

// TestCommandToolStdoutObjectContract:非 JSON 对象 stdout 是工具契约破坏
// (LocalRunner 统一收口校验,CommandTool 原样上抛)。
func TestCommandToolStdoutPassthrough(t *testing.T) {
	tool, err := NewCommandTool(CommandSpec{Name: "arr", Path: "/bin/sh", Args: []string{"-c", "echo '[1,2]'"}})
	if err != nil {
		t.Fatalf("NewCommandTool: %v", err)
	}
	res, err := tool.Run(context.Background(), Call{Inputs: map[string]any{}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(string(res.Output)) != "[1,2]" {
		t.Fatalf("output = %q, want passthrough", res.Output)
	}
	_ = os.Environ // keep os import if assertions above change
}

// ---- 对抗审计确认项的回归测试(HIGH/MED/LOW) ----

// HIGH:ctx 取消后,后台孙进程(shell `sleep 30 &`)继承管道写端,原实现
// 的 Wait 永无 EOF 而无限阻塞;WaitDelay 强制断管后 Run 必须及时返回且
// 错误归因 ctx(而非工具成功/ExitError)。
func TestCommandToolGrandchildPipeDeadline(t *testing.T) {
	tool, err := NewCommandTool(CommandSpec{
		Name: "bg_noise", Path: "/bin/sh", Args: []string{"-c", "echo hi; sleep 30 &"},
		WaitDelay: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewCommandTool: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = tool.Run(ctx, Call{Inputs: map[string]any{}})
	if err == nil {
		t.Fatal("deadline-exceeded run returned nil error")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("err = %v, want ctx deadline attribution", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Run blocked %v on grandchild-held pipe (WaitDelay not effective)", elapsed)
	}
}

// MED:stdout 超过缓冲上界 = 工具自身失败(业务错误),缓冲内存封顶。
func TestCommandToolOutputOverflowFails(t *testing.T) {
	tool, err := NewCommandTool(CommandSpec{
		Name: "firehose", Path: "/bin/sh",
		Args:      []string{"-c", "head -c 4096 /dev/zero | tr '\\0' 'x'"},
		MaxStdout: 1024,
	})
	if err != nil {
		t.Fatalf("NewCommandTool: %v", err)
	}
	_, err = tool.Run(context.Background(), Call{Inputs: map[string]any{}})
	if err == nil || !strings.Contains(err.Error(), "stdout exceeds 1024 bytes") {
		t.Fatalf("err = %v, want stdout overflow failure", err)
	}
}

// LOW:相对路径拒绝(exec 初始解析走父进程 PATH,同名顶替二进制会完整
// 收到沙箱 env)。
func TestCommandToolRejectsRelativePath(t *testing.T) {
	if _, err := NewCommandTool(CommandSpec{Name: "rel", Path: "python3"}); err == nil {
		t.Fatal("relative path accepted")
	}
	if _, err := NewCommandTool(CommandSpec{Name: "abs", Path: "/usr/bin/python3"}); err != nil {
		t.Fatalf("absolute path rejected: %v", err)
	}
}
