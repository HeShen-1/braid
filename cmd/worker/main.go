// worker 入口:读 env → 组装 → 信号 → worker.Run。薄 main,零业务逻辑;
// 与 relay 同款骨架(Q14:交付时合并为单二进制多入口)。T3.6 起装配真实
// 节点执行器(llm 经 OpenAI 兼容适配器);tool/approval 随 T3.2/T4.1 接入。
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
	"braid/internal/llm"
	"braid/internal/nodes"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
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
		if err := cfg.Validate(); err != nil { // hostname 后填,补校验(DDL 64 上限)
			return err
		}
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

	// T3.6 生产装配:llm 配置缺 URL/KEY/PRICE_TABLE 即启动失败(生产 worker
	// 必须能调 LLM;组合预算上界在 Validate 内启动即败,HIGH① 拍板)。
	llmCfg, err := llm.ConfigFromEnv(os.Getenv)
	if err != nil {
		return fmt.Errorf("worker: llm config: %w", err)
	}
	client, err := llm.NewOpenAICompat(llmCfg)
	if err != nil {
		return fmt.Errorf("worker: llm client: %w", err)
	}

	ids, err := snowflake.New(cfg.SnowflakeNode)
	if err != nil {
		return fmt.Errorf("worker: snowflake: %w", err)
	}
	sched, err := scheduler.New(scheduler.Deps{DB: mysqlDB, IDs: ids, Logger: slog.Default()})
	if err != nil {
		return fmt.Errorf("worker: scheduler: %w", err)
	}

	executor := nodes.NewMux(sched, &nodes.LLMExecutor{
		Client: client,
		Prices: llmCfg.PriceTable,
		DB:     mysqlDB,
	})
	return worker.Run(ctx, worker.Deps{
		DB:       mysqlDB,
		IDs:      ids,
		Executor: executor,
		Sched:    sched,
		Logger:   slog.Default(),
	}, cfg, asynq.RedisClientOpt{Addr: addr})
}
