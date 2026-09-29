package main

// braid-sandbox-exec 是容器内 helper 入口(T10.1 T5,D-2026-09-28-t101):
// 沙箱镜像 ENTRYPOINT(deploy/sandbox.Dockerfile),argv = <tool> 或
// selftest <mode>。会话逻辑全在 internal/tools.RunSandboxHelper(可测),
// 本文件只做三件事:signal ctx(docker stop 的 SIGTERM → 会话取消)、
// watchdog 硬退(deadline+grace 有界自灭,os.Exit 直接收尸——正常返回
// 路径已解除)、错误 → 退出码映射。退出码契约(宿主载体消费):
//
//	0  信封已写(含 ok=false 业务信封)
//	3  无信封失败(ctx 取消/deadline/读超时/编码失败)
//	4  watchdog 硬退(挂死/孤儿兜底,理论只在宿主失联时触及)
//
// 退出码 3 的典型场景 = 宿主 ctx 已取消(deadline/stop),宿主按 §6 走
// infra 分类;偶发竞态(信封 deadline 与宿主 kill 同窗)绝大多数同归
// infra,极窄收割窗内宿主 ctx 采样仍存活会误归 tool_failed(方向 = 丢
// 一次重试,与确定性失败不重试哲学相容,接受——T10 审计收窄措辞)。

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"braid/internal/tools"
)

const (
	exitEnvelopeWritten = 0
	exitNoEnvelope      = 3
	exitWatchdog        = 4
)

func main() {
	// SIGTERM = docker stop -t 3 的首要信号;SIGINT 一并接(手动调试)。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	err := tools.RunSandboxHelper(ctx, os.Args[1:], os.Stdin, os.Stdout, func() { os.Exit(exitWatchdog) })
	if err != nil {
		// 无信封失败的唯一诊断面:err 文本落 stderr(carrier 侧只见
		// 退出码,ctx/读超时/IO/编码四子因全靠这行区分——T10 审计补)。
		// 文本面 = 错误描述,无 stdin 载荷无密钥,不触铁律。
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitNoEnvelope)
	}
	os.Exit(exitEnvelopeWritten)
}
