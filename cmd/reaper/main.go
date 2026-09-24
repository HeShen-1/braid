// reaper 入口:读 env → 组装 → 信号 → reaper.Run。薄 main,零业务逻辑;
// 交付时与 api/worker/relay 合并为单二进制多入口(Q14)。
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"

	"braid/internal/db"
	"braid/internal/preflight"
	"braid/internal/reaper"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return // 停机信号落在启动窗口:正常停机,退出码 0
		}
		slog.Error("reaper: fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := reaper.ConfigFromEnv(os.Getenv) // T7.1 配置体系唯一换装点
	if err != nil {
		return err
	}
	dsn := os.Getenv("BRAID_MYSQL_DSN")
	if dsn == "" {
		return errors.New("reaper: BRAID_MYSQL_DSN is required")
	}
	mysqlDB, err := sql.Open("mysql", db.CanonicalDSN(dsn))
	if err != nil {
		return fmt.Errorf("reaper: open mysql: %w", err)
	}
	defer mysqlDB.Close() //nolint:errcheck // 主路径退出,关闭错误无处置义
	if err := mysqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("reaper: mysql ping: %w", err)
	}

	// T7.3 安装自检:迁移版本 + append-only 硬拒;reaper 是分区水位的自愈者
	// (janitor 首 tick 即补 +7d),水位破约仅 warn——硬拒会造出「没人补
	// 分区 → 永远起不来」的死锁。
	if err := errors.Join(
		preflight.CheckSchemaVersion(ctx, mysqlDB),
		preflight.CheckAuditAppendOnly(ctx, mysqlDB),
	); err != nil {
		return fmt.Errorf("reaper: %w", err)
	}
	if err := preflight.CheckAuditPartitions(ctx, mysqlDB); err != nil {
		slog.Warn("reaper: audit partition watermark", "err", err)
	}

	ids, err := snowflake.New(uint16(os.Getpid() & 0x3ff))
	if err != nil {
		return fmt.Errorf("reaper: snowflake: %w", err)
	}
	sched, err := scheduler.New(scheduler.Deps{DB: mysqlDB, IDs: ids, Queue: cfg.Queue})
	if err != nil {
		return fmt.Errorf("reaper: scheduler: %w", err)
	}

	// 选主 fail-open(D-t25 #1,与 relay/worker 的「Redis 可达自检启动即败」
	// 刻意相反):正确性结构性不依赖 Redis,拉起一个连不上 Redis 的 reaper
	// 仍完全可用(leaderless 全跑),拒启反而制造「断 Redis 期间无人对账」
	// 的停摆面——恰是验收要消灭的场景。不可达只 warn。
	deps := reaper.Deps{DB: mysqlDB, Sched: sched, IDs: ids}
	if addr := os.Getenv("BRAID_REDIS_QUEUE_ADDR"); addr != "" {
		// T7.3:队列 Redis 契约探测仅 warn(D-t25 fail-open 同族:锁实例破约
		// 的失败模式 = 锁丢失退化为 leaderless,安全;拒启反制造停摆面)。
		if err := preflight.CheckRedisQueue(ctx, addr); err != nil {
			slog.Warn("reaper: redis queue policy preflight", "err", err)
		}
		deps.Locker = reaper.RedisLocker(redis.NewClient(&redis.Options{Addr: addr}))
		slog.Info("reaper: leader lock enabled", "addr", addr)
	} else {
		slog.Warn("reaper: BRAID_REDIS_QUEUE_ADDR unset; running leaderless")
	}

	r, err := reaper.New(deps, cfg)
	if err != nil {
		return err
	}
	return r.Run(ctx)
}
