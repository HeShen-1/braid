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
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"

	"braid/internal/db"
	"braid/internal/metrics"
	"braid/internal/preflight"
	"braid/internal/reaper"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
	"braid/internal/tracing"
)

// traceShutdownGrace 是 OTel batcher 排空预算(独立停机窗,cmd/worker 同款)。
const traceShutdownGrace = 2 * time.Second

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
	if cfg.MySQLDSN == "" { // T7.1 收编:cmd 直读 env 清零,断言读 cfg 字段
		return errors.New("reaper: BRAID_MYSQL_DSN is required")
	}
	mysqlDB, err := sql.Open("mysql", db.CanonicalDSN(cfg.MySQLDSN))
	if err != nil {
		return fmt.Errorf("reaper: open mysql: %w", err)
	}
	if err := db.ConfigurePool(mysqlDB, cfg.Pool); err != nil { // D-t63 移交回收;T7.1:三值经 BRAID_DB_* env 化
		return fmt.Errorf("reaper: %w", err)
	}
	slog.Info("db: pool", "max_open", cfg.Pool.MaxOpen, "max_idle", cfg.Pool.MaxIdle, "conn_max_lifetime", cfg.Pool.ConnMaxLifetime)
	defer mysqlDB.Close() //nolint:errcheck // 主路径退出,关闭错误无处置义
	if err := mysqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("reaper: mysql ping: %w", err)
	}

	// T7.3 安装自检:迁移版本 + append-only 硬拒;reaper 是分区水位的自愈者
	// (janitor 首 tick 即补 +7d),水位破约仅 warn——硬拒会造出「没人补
	// 分区 → 永远起不来」的死锁。
	if err := errors.Join(
		preflight.CheckSchemaVersion(ctx, mysqlDB),
		// AllowLoginAuditDelete:janitor purgeLoginAudit 保留期清理的表级
		// DELETE 豁免(audit_log 面仍全拒;授权矩阵 deploy/README.md §5)。
		preflight.CheckAuditAppendOnly(ctx, mysqlDB, preflight.AllowLoginAuditDelete),
	); err != nil {
		return fmt.Errorf("reaper: %w", err)
	}
	if err := preflight.CheckAuditPartitions(ctx, mysqlDB); err != nil {
		slog.Warn("reaper: audit partition watermark", "err", err)
	}

	ids, err := snowflake.New(cfg.SnowflakeNode) // T7.1 显式化(缺省 PID 派生在 ConfigFromEnv)
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
	// T6.4:Prometheus 指标面(裁决计数 + flush 观测 + 周期 gauge 采集)。
	// BRAID_METRICS_ADDR 空 = 不启;Deps.Metrics 与采集器同生命周期。
	mset, mreg := metrics.NewSet()
	if _, ok, err := metrics.Serve(ctx, cfg.MetricsAddr, mreg); err != nil {
		return fmt.Errorf("reaper: %w", err)
	} else if ok {
		slog.Info("metrics: serving", "addr", cfg.MetricsAddr)
	}
	go metrics.CollectDBPool(ctx, mysqlDB, mset.DBPool) // T7.1:池 gauge 30s 常量周期(braid_db_pool_*)

	deps := reaper.Deps{DB: mysqlDB, Sched: sched, IDs: ids, Metrics: mset.Reaper}
	if cfg.RedisQueueAddr != "" {
		// T7.3:队列 Redis 契约探测仅 warn(D-t25 fail-open 同族:锁实例破约
		// 的失败模式 = 锁丢失退化为 leaderless,安全;拒启反制造停摆面)。
		if err := preflight.CheckRedisQueue(ctx, cfg.RedisQueueAddr); err != nil {
			slog.Warn("reaper: redis queue policy preflight", "err", err)
		}
		deps.Locker = reaper.RedisLocker(redis.NewClient(&redis.Options{Addr: cfg.RedisQueueAddr}))
		slog.Info("reaper: leader lock enabled", "addr", cfg.RedisQueueAddr)
	} else {
		slog.Warn("reaper: BRAID_REDIS_QUEUE_ADDR unset; running leaderless")
	}

	// T12.1:OTel trace——reaper 进程 span(tick/job/flush/janitor 计时面,
	// span 恒不进发射 ctx,D-t121 #4);装配镜像 cmd/worker。
	traceCfg := tracing.ConfigFromEnv(os.Getenv)
	if err := traceCfg.Validate(); err != nil {
		return fmt.Errorf("reaper: %w", err)
	}
	traceProv, err := tracing.NewProvider(tracing.Config{Exporter: traceCfg.Exporter, Endpoint: traceCfg.Endpoint, ServiceName: "braid-reaper"})
	if err != nil {
		return fmt.Errorf("reaper: %w", err)
	}
	defer func() {
		shCtx, cancel := context.WithTimeout(context.Background(), traceShutdownGrace)
		defer cancel()
		if err := traceProv.Shutdown(shCtx); err != nil {
			slog.Warn("reaper: trace provider shutdown", "err", err)
		}
	}()
	slog.Info("reaper: tracing", "exporter", traceCfg.Exporter)
	deps.Tracer = traceProv.Tracer("braid/reaper")

	r, err := reaper.New(deps, cfg)
	if err != nil {
		return err
	}
	return r.Run(ctx)
}
