package cli

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"time"

	"braid/internal/db"
	"braid/internal/preflight"
)

// auditReadyPollInterval 是 -wait 轮询间隔(独立于等待窗:等待窗定义
// 「等多久」,间隔定义「多密地问」;2s 对 janitor 首 tick 的秒级收敛
// 足够密,对 DB 又足够轻——安装闸路径每次进程起只跑一轮)。
const auditReadyPollInterval = 2 * time.Second

// runAuditReady 是 `braid audit-ready` 的 argv 层:flag 解析 + 用法闸
// (退出 2),库路径在 auditReady(复用 preflight.CheckAuditPartitions
// 语义,DB 直连形态同 user create,D-2026-10-03-t162)。
func runAuditReady(ctx context.Context, args []string, env func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("braid audit-ready", flag.ContinueOnError)
	fs.SetOutput(stderr)
	wait := fs.Duration("wait", 0, "bounded poll until the watermark covers today (e.g. 120s); default single probe")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *wait < 0 { // 负值拒绝,与 seed -wait 同口径(静默吞 = 单探针退化)
		sayln(stderr, "%s", "braid audit-ready: -wait must be >= 0")
		return ExitUsage
	}
	if fs.NArg() != 0 { // 位置参闸同则:多余即用法错,不静默吞
		say(stderr, "braid audit-ready: unexpected positional arguments %v\n", fs.Args())
		return ExitUsage
	}
	return auditReady(ctx, env, *wait, stdout, stderr)
}

// auditReady 判定体(T20.7 安装口径的单一真源信号):水位覆盖当日 = 0,
// 否则 1(preflight 错误文本自带 start reaper / ALTER 修复指引)。判定的
// 是 **DB 水位状态**而非 reaper 进程活——进程活≠分区已补(e2e-up.sh 同
// 口径先例)。-wait 窗内任何失败(含 DB 不可达:helm initContainer 起
// 在 migrate Job 完成前)都重试,窗尽返回最后一次错误。
func auditReady(ctx context.Context, env func(string) string, wait time.Duration, stdout, stderr io.Writer) int {
	dsn := env("BRAID_MYSQL_DSN")
	if dsn == "" {
		sayln(stderr, "%s", "braid audit-ready: BRAID_MYSQL_DSN is required (same env contract as cmd/api)")
		return ExitError
	}
	dbh, err := sql.Open("mysql", db.CanonicalDSN(dsn))
	if err != nil {
		say(stderr, "braid audit-ready: open mysql: %v\n", err)
		return ExitError
	}
	defer dbh.Close() //nolint:errcheck // 一次性进程退出路径
	err = waitReady(ctx, wait, auditReadyPollInterval, func() error {
		return preflight.CheckAuditPartitions(ctx, dbh)
	})
	if err != nil {
		say(stderr, "braid audit-ready: %v\n", err)
		return ExitError
	}
	say(stdout, "%s", "audit-ready: audit_log partition watermark covers today\n")
	return ExitOK
}

// waitReady 是有界等待环(纯逻辑,表驱动单测锚):探针绿即返回;wait=0
// 单探针;窗内每 interval 重试,窗尽返回最后一次探针错误。ctx 取消 =
// 调用方撤梯子(脚本闸的 SIGTERM 路径),带最后一次探针错误返回。
func waitReady(ctx context.Context, wait, interval time.Duration, probe func() error) error {
	deadline := time.Now().Add(wait)
	for {
		err := probe()
		if err == nil {
			return nil
		}
		if wait <= 0 {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return err
		}
		sleep := interval
		if remaining < sleep {
			sleep = remaining
		}
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			// 双 %w(Go 1.20+):canceled 因与最后一次探针错误都可
			// errors.Is 追溯——脚本闸的 SIGTERM 路径与真超时可区分。
			return fmt.Errorf("wait interrupted (%w); last probe: %w", ctx.Err(), err)
		case <-timer.C:
		}
	}
}
