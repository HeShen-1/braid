// braid 是运维 CLI 入口(T16.2,issue #134):user create / definition
// publish / execution trigger / seed / audit-ready 命令面(连接形态
// D-2026-10-03-t162;audit-ready = T20.7 安装口径判定动词)。
// 会话逻辑全在 internal/cli.Run(可测,参照 cmd/braid-sandbox-exec 先例);
// 本文件只做 signal ctx + 退出码直通。退出码契约:0 成功 / 1 运行失败 /
// 2 用法错误。
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/go-sql-driver/mysql" // user create 直连路径的驱动注册(组合根,cmd/api 同款)

	"braid/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(cli.Run(ctx, os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
}
