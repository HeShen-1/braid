// worker 入口:读 env → 组装 → 信号 → worker.Run。薄 main,零业务逻辑;
// 与 relay 同款骨架(Q14:交付时合并为单二进制多入口)。Executor 实现随
// T2.4/T3.x 接入,当前骨架阶段以显式报错占位——不静默跑空 worker。
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

	"braid/internal/db"
	"braid/internal/worker"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return // 停机信号落在启动窗口:正常停机,退出码 0(不算失败重启)
		}
		slog.Error("worker: fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := worker.ConfigFromEnv(os.Getenv) // T7.1 配置体系唯一换装点
	if err != nil {
		return err
	}
	if cfg.WorkerID == "" {
		hn, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("worker: hostname (set BRAID_WORKER_ID to override): %w", err)
		}
		cfg.WorkerID = hn
	}
	dsn := os.Getenv("BRAID_MYSQL_DSN")
	if dsn == "" {
		return errors.New("worker: BRAID_MYSQL_DSN is required")
	}
	mysqlDB, err := sql.Open("mysql", db.CanonicalDSN(dsn))
	if err != nil {
		return fmt.Errorf("worker: open mysql: %w", err)
	}
	defer mysqlDB.Close() //nolint:errcheck // 主路径退出,关闭错误无处置义
	if err := mysqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("worker: mysql ping: %w", err)
	}

	addr := os.Getenv("BRAID_REDIS_QUEUE_ADDR")
	if addr == "" {
		return errors.New("worker: BRAID_REDIS_QUEUE_ADDR is required")
	}
	// Redis 可达性自检(Inspector.Queues 即一次真实往返),启动即败响亮:
	// 拉起一个连不上队列的 worker 只会静默空转。
	insp := asynq.NewInspector(asynq.RedisClientOpt{Addr: addr})
	if _, err := insp.Queues(); err != nil {
		_ = insp.Close()
		return fmt.Errorf("worker: redis queue %s unreachable: %w", addr, err)
	}
	if err := insp.Close(); err != nil {
		return fmt.Errorf("worker: close inspector: %w", err)
	}

	// T2.3 骨架止步于此:Executor 实现随 T2.4(调度回写)/T3.x(节点实现)
	// 接入后,在此装配 snowflake(BRAID_SNOWFLAKE_NODE_ID,P0 静态槽位)+ 调
	// worker.Run。在此之前拒绝空转 serve——静默跑一个不执行任何节点的 worker
	// 只会把任务烧成重试。
	return errors.New("worker: no Executor wired yet (T2.4/T3.x 接入点);refusing to serve")
}
