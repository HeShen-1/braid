// Package cli 实现 braid 运维 CLI(T16.2,issue #134):最小命令面三命令——
// user create(建号,DB 直连)/ definition publish(定义发布)/
// execution trigger(触发),后两者走 REST 客户端。连接形态钉死于
// D-2026-10-03-t162-cli-connection:建号复用 auth 原语同则,发布/触发走
// 真路径(RBAC/审计/幂等原生生效),不复制 handler 语义。
//
// 会话逻辑全在本包(Run 可测;cmd/braid 薄入口,参照 cmd/braid-sandbox-exec
// 先例)。env 经函数注入(os.Getenv 形态,api.ConfigFromEnv 同款)。
package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// 退出码契约(README 与脚本消费者(T16.3 seed)依赖):0 成功 / 1 运行失败
// (DB/API/文件/HTTP)/ 2 用法错误(argv 层,flag 包解析失败同判)。
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// Run 执行 CLI:args 不含程序名(env 与流注入供测试)。
func Run(ctx context.Context, args []string, env func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) >= 2 {
		switch args[0] + " " + args[1] {
		case "user create":
			return runUserCreate(ctx, args[2:], env, stdout, stderr)
		case "definition publish":
			return runDefinitionPublish(ctx, args[2:], env, stdout, stderr)
		case "execution trigger":
			return runExecutionTrigger(ctx, args[2:], env, stdin, stdout, stderr)
		}
	}
	if len(args) >= 1 && args[0] == "seed" { // T16.3 一键 demo(单词命令)
		return runSeed(ctx, args[1:], env, stdout, stderr)
	}
	say(stderr, "braid: unknown command %q\n\n", strings.Join(args, " "))
	usage(stderr)
	return ExitUsage
}

// say/sayln 写注入流并显式丢弃写错误:CLI 的 stdout/stderr 写失败无
// 处置面(管道断开时退出码是唯一通信道,失败路径已各自归因退出码);
// 测试注入 bytes.Buffer 结构性不失败(errcheck 以显式丢弃收口,仓内
// 既有 `_ =` 丢弃先例)。
func say(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

func sayln(w io.Writer, format string, a ...any) {
	say(w, format+"\n", a...)
}

// usage 是唯一用法面:四命令 + 凭证纪律一句(凭据只走 env,不进 argv)。
const usageText = `usage:
  braid user create -username <name> -roles <r1,r2> [-email <addr>] [-tenant <id>]
      Create a user (DB direct; BRAID_MYSQL_DSN + BRAID_CLI_PASSWORD env required).
  braid definition publish <file.json>
      Submit a DSL draft and publish it (REST; token via BRAID_TOKEN or
      BRAID_USERNAME/BRAID_PASSWORD, API base via BRAID_API_ADDR).
  braid execution trigger -definition <name> [-version <n>] -input <file|->
      Trigger an execution (REST; same auth env as publish).
  braid seed [-stage db|rest|all] [-wait 2m] [-definition <file>]
      One-command demo (T16.3): migrate + seed demo users + publish + trigger,
      idempotent on rerun. db stage needs BRAID_MYSQL_DSN; rest stage logs in
      as the seeded alice account (BRAID_SEED_AUTHOR_PASSWORD /
      BRAID_SEED_APPROVER_PASSWORD, dev defaults dev-author-1/dev-approver-1)
      and ignores ambient BRAID_TOKEN.

credentials never go on the command line (shell history); see README.
`

// usage 输出到 stderr(错误路径)或测试直查常量。
func usage(w io.Writer) {
	say(w, "%s", usageText)
}
