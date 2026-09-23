package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// 输出缓冲上界(对抗审计 MED:无上限 Collect 下,大产出工具单次调用即可
// OOM worker 进程;上界取远低于 max_allowed_packet 的量级,超出 = 工具
// 自身失败)。stderr 只供 512 字节错误摘要,独立小上界。
const (
	DefaultMaxStdout = 8 << 20 // 8 MiB
	maxStderr        = 64 << 10
	// DefaultWaitDelay 是 kill 后管道收尾宽限(对抗审计 HIGH):SIGKILL 只杀
	// 直接子进程,后台孙进程(shell `sleep 5 &` 类)继承 stdout/stderr 管道
	// 写端使 copy goroutine 永无 EOF——WaitDelay 到期 exec 强制关闭管道,
	// Run 得以返回而非无限阻塞(deadline 与停机语义的兜底防线)。
	DefaultWaitDelay = 10 * time.Second
)

// CommandSpec 声明一个子进程工具:固定可执行路径与参数前缀。Path 必须
// 绝对路径(NewCommandTool 强制):exec 的初始二进制解析走父进程 PATH,
// 沙箱 env 的 PATH 只约束子进程自身派生的命令——相对名会让「继承 PATH
// 里更靠前的同名二进制」顶替预期解释器并完整接收沙箱 env(审计 LOW)。
// WaitDelay/MaxStdout 为 0 时取默认;测试注入短值。
type CommandSpec struct {
	Name      string
	Path      string
	Args      []string
	WaitDelay time.Duration // kill 后管道收尾宽限(默认 10s)
	MaxStdout int           // stdout 缓冲上界字节(默认 8 MiB)
}

// CommandTool 是本地进程载体(「本地进程实现」的 P0 直译,验收面 = 沙箱
// 环境):inputs 经 stdin 传 JSON,产出 = stdout,非零退出 = 工具失败
// (stderr 摘要随错误上抛,经 nodes 侧 clampMessage 整形后落
// node_execution.error_message)。
//
// 环境白名单(验收:沙箱环境不含平台主凭证):子进程 env 从零构造,
// 仅 PATH(可执行解析)/ LANG / BRAID_TOOL_TOKEN(scoped token 传递)/
// BRAID_ATTEMPT_KEY(幂等键传递)四类可见——构造式白名单,父进程的任何
// 其他环境变量(含 DSN、LLM key、HMAC 密钥)结构性不可达。P1 Docker
// 载体沿用同一 env 契约。
type CommandTool struct {
	spec CommandSpec
}

// NewCommandTool 构造子进程工具;空名、空路径、相对路径响亮拒绝。
func NewCommandTool(spec CommandSpec) (*CommandTool, error) {
	if spec.Name == "" {
		return nil, fmt.Errorf("tools: command tool with empty name")
	}
	if spec.Path == "" {
		return nil, fmt.Errorf("tools: command tool %q with empty path", spec.Name)
	}
	if !filepath.IsAbs(spec.Path) {
		return nil, fmt.Errorf("tools: command tool %q: path must be absolute, got %q", spec.Name, spec.Path)
	}
	return &CommandTool{spec: spec}, nil
}

// Name 满足 Tool 接口。
func (c *CommandTool) Name() string { return c.spec.Name }

// Run 执行子进程。ctx 取消 = kill 子进程并把 ctx 错误原样上抛(基础设施
// 失败语义,nodes 侧据此走 error 返回而非业务失败收口)。
func (c *CommandTool) Run(ctx context.Context, call Call) (Result, error) {
	in, err := json.Marshal(call.Inputs)
	if err != nil {
		return Result{}, fmt.Errorf("tools: %s marshal inputs: %w", c.spec.Name, err)
	}
	waitDelay := c.spec.WaitDelay
	if waitDelay <= 0 {
		waitDelay = DefaultWaitDelay
	}
	maxOut := c.spec.MaxStdout
	if maxOut <= 0 {
		maxOut = DefaultMaxStdout
	}
	cmd := exec.CommandContext(ctx, c.spec.Path, c.spec.Args...)
	cmd.Stdin = bytes.NewReader(in)
	stdout := &limitBuffer{limit: maxOut}
	stderr := &limitBuffer{limit: maxStderr}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = sandboxEnv(call)
	cmd.WaitDelay = waitDelay
	runErr := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		// ctx 取消优先于 ExitError/ErrWaitDelay 呈现(调用方按 errors.Is
		// 分类;kill 后 Wait 的返回值在此场景只是收尾噪声,ctx 原因才是
		// 语义本体。含孙进程持管被 WaitDelay 强制断管的路径)。
		return Result{}, fmt.Errorf("tools: %s: %w", c.spec.Name, ctxErr)
	}
	if runErr != nil {
		return Result{}, fmt.Errorf("tools: %s: %v: %s",
			c.spec.Name, runErr, clamp(stderr.String(), 512))
	}
	if stdout.overflow {
		return Result{}, fmt.Errorf("tools: %s: stdout exceeds %d bytes", c.spec.Name, maxOut)
	}
	out := bytes.TrimSpace(stdout.Bytes())
	if len(out) == 0 {
		return Result{}, fmt.Errorf("tools: %s: empty stdout", c.spec.Name)
	}
	return Result{Output: out}, nil
}

// sandboxEnv 构造子进程白名单环境。PATH 继承父进程(仅路径解析用途,不含
// 凭证);其余从零注入。
func sandboxEnv(call Call) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"LANG=C.UTF-8",
		"BRAID_TOOL_TOKEN=" + call.Token,
		"BRAID_ATTEMPT_KEY=" + call.IdempotencyKey,
	}
}

// limitBuffer 是带上界的缓冲:超出 limit 后丢弃写入并记 overflow,进程
// 仍可继续产出(写端不阻塞,靠 ctx kill 收口),但缓冲内存封顶——OOM 面
// 从「工具决定」收窄为「配置常量」。
type limitBuffer struct {
	limit    int
	buf      bytes.Buffer
	overflow bool
}

func (b *limitBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) <= room {
			return b.buf.Write(p)
		}
		_, _ = b.buf.Write(p[:room])
	}
	b.overflow = true
	return len(p), nil // 谎报全收:对 exec 的 copy goroutine 而言写端永不背压
}

func (b *limitBuffer) String() string { return b.buf.String() }
func (b *limitBuffer) Bytes() []byte  { return b.buf.Bytes() }

// clamp 截断 stderr 摘要(error_message 通道的字节纪律;最终落列的字符级
// clamp 在 nodes.failedOutcome,这里只防 stderr 海量文本撑爆错误链)。
func clamp(s string, n int) string {
	if len(s) > n {
		s = s[:n] + "…"
	}
	return strings.TrimSpace(s)
}
