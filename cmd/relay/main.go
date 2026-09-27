// relay 入口:读 env → 组装 → 信号 → relay.Run。薄 main,零业务逻辑;
// 交付时与 api/worker/reaper 合并为单二进制多入口(Q14)。
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
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"braid/internal/db"
	"braid/internal/metrics"
	"braid/internal/preflight"
	"braid/internal/relay"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return // 停机信号落在启动窗口:正常停机,退出码 0(不算失败重启)
		}
		slog.Error("relay: fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := relay.ConfigFromEnv(os.Getenv) // T7.1 配置体系唯一换装点
	if err != nil {
		return err
	}
	if cfg.MySQLDSN == "" { // T7.1 收编:cmd 直读 env 清零,断言读 cfg 字段
		return errors.New("relay: BRAID_MYSQL_DSN is required")
	}
	mysqlDB, err := sql.Open("mysql", db.CanonicalDSN(cfg.MySQLDSN))
	if err != nil {
		return fmt.Errorf("relay: open mysql: %w", err)
	}
	if err := db.ConfigurePool(mysqlDB, cfg.Pool); err != nil { // D-t63 移交回收;T7.1:三值经 BRAID_DB_* env 化
		return fmt.Errorf("relay: %w", err)
	}
	slog.Info("db: pool", "max_open", cfg.Pool.MaxOpen, "max_idle", cfg.Pool.MaxIdle, "conn_max_lifetime", cfg.Pool.ConnMaxLifetime)
	defer mysqlDB.Close() //nolint:errcheck // 主路径退出,关闭错误无处置义
	if err := mysqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("relay: mysql ping: %w", err)
	}

	// T7.3 安装自检:迁移版本 + append-only 硬拒;relay 零审计写,分区水位
	// 破约仅 warn(不该把投递链 brick 在审计水位上)。
	if err := errors.Join(
		preflight.CheckSchemaVersion(ctx, mysqlDB),
		preflight.CheckAuditAppendOnly(ctx, mysqlDB),
	); err != nil {
		return fmt.Errorf("relay: %w", err)
	}
	if err := preflight.CheckAuditPartitions(ctx, mysqlDB); err != nil {
		slog.Warn("relay: audit partition watermark", "err", err)
	}

	addr := cfg.RedisQueueAddr
	if addr == "" {
		return errors.New("relay: BRAID_REDIS_QUEUE_ADDR is required")
	}
	// Redis 可达性自检(Inspector.Queues 即一次真实往返),启动即败响亮:
	// 拉起一个投不出去的 relay 只会把事件烧成退避。
	// 启动自检后保留 Inspector 生命周期移交 defer(探测面复用同一连接;
	// 可达性语义不变:首轮探测在 Run 的 probe 里,失败只记日志)。
	insp := asynq.NewInspector(asynq.RedisClientOpt{Addr: addr})
	if _, err := insp.Queues(); err != nil {
		_ = insp.Close()
		return fmt.Errorf("relay: redis queue %s unreachable: %w", addr, err)
	}
	defer insp.Close() //nolint:errcheck // 停机路径,关闭错误无处置义

	// T7.3:队列 Redis 契约(noeviction + AOF everysec)硬拒——relay 是队列
	// 数据承载方,破约 = 重启烧队列、逐出静默丢事件。
	if err := preflight.CheckRedisQueue(ctx, addr); err != nil {
		return fmt.Errorf("relay: %w", err)
	}

	// T6.4:指标面(投递计数 + Redis 内存水线;BRAID_METRICS_ADDR 空 = 不启)。
	mset, mreg := metrics.NewSet()
	if _, ok, err := metrics.Serve(ctx, cfg.MetricsAddr, mreg); err != nil {
		return fmt.Errorf("relay: %w", err)
	} else if ok {
		slog.Info("metrics: serving", "addr", cfg.MetricsAddr)
	}
	go metrics.CollectDBPool(ctx, mysqlDB, mset.DBPool) // T7.1:池 gauge 30s 常量周期(braid_db_pool_*)

	eq := relay.NewAsynqEnqueuer(asynq.RedisClientOpt{Addr: addr})
	defer eq.Close() //nolint:errcheck // 同上
	rc := redis.NewClient(&redis.Options{Addr: addr})
	defer rc.Close() //nolint:errcheck // 同上
	return relay.Run(ctx, relay.Deps{
		DB: mysqlDB, Enqueuer: eq, Logger: slog.Default(),
		Metrics:   mset.Relay,
		RDB:       rc,
		Inspector: insp,
	}, cfg)
}
