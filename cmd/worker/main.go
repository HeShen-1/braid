// worker 入口:读 env → 组装 → 信号 → worker.Run。薄 main,零业务逻辑;
// 与 relay 同款骨架(Q14:交付时合并为单二进制多入口)。T3.6 起装配真实
// 节点执行器(llm 经 OpenAI 兼容适配器);T3.2 接入 tool 执行器(registry +
// LocalRunner + scoped token);T4.1 接入 approval 执行器(纯挂起决策,
// 挂起事务在 scheduler.Suspend)。
package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/go-sql-driver/mysql"
	"github.com/hibiken/asynq"

	"braid/internal/auth"
	"braid/internal/db"
	"braid/internal/llm"
	"braid/internal/nodes"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
	"braid/internal/tools"
	"braid/internal/worker"
)

// toolTokenKey 读 BRAID_TOOL_TOKEN_SECRET(hex,可缺省):显式供给 = 跨
// 进程校验预留(P1 工具回调平台);缺省 nil = 进程内随机(tools 包内生成)。
func toolTokenKey(getenv func(string) string) ([]byte, error) {
	raw := getenv("BRAID_TOOL_TOKEN_SECRET")
	if raw == "" {
		return nil, nil
	}
	key, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("worker: BRAID_TOOL_TOKEN_SECRET: want hex: %w", err)
	}
	return key, nil
}

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

	// T7.2 离线 tokenizer 预构建:随包词表 SHA-256 校验 + BPE 解析在此
	// 启动即败;此后首个 LLM 节点的请求前估算零懒加载、零外网(断网
	// 冷启动验收)。
	if err := llm.PrebuildTokenizers(); err != nil {
		return fmt.Errorf("worker: prebuild tokenizers: %w", err)
	}

	ids, err := snowflake.New(cfg.SnowflakeNode)
	if err != nil {
		return fmt.Errorf("worker: snowflake: %w", err)
	}
	sched, err := scheduler.New(scheduler.Deps{DB: mysqlDB, IDs: ids, Logger: slog.Default()})
	if err != nil {
		return fmt.Errorf("worker: scheduler: %w", err)
	}

	// T3.2/T3.5 工具运行时装配 + T5.2 执行时 RBAC 门:registry(内置
	// http/file_read 按配置注册,未配置即不注册——空 = 一切 tool 节点
	// tool_not_found 收口;api 与 worker 经 BuiltinToolsFromEnv 单源装配,
	// 注册集与发布白名单一致)+ scoped token 签发器(BRAID_TOOL_TOKEN_SECRET
	// 缺省 = 进程内随机,重启即轮换——P0 校验点在同进程调用门,自洽;P1
	// 工具回调平台时经部署配置显式供给)+ 执行时授权器(发布期白名单之外
	// 的调用现场再校验,拒绝落 tool.call.denied 审计;默认策略 author|admin,
	// system 触发哨兵放行)。
	builtins, err := tools.BuiltinToolsFromEnv(os.Getenv)
	if err != nil {
		return fmt.Errorf("worker: builtin tools: %w", err)
	}
	toolReg, err := tools.NewRegistry(builtins...)
	if err != nil {
		return fmt.Errorf("worker: tool registry: %w", err)
	}
	toolKey, err := toolTokenKey(os.Getenv)
	if err != nil {
		return err
	}
	toolExec := &nodes.ToolExecutor{
		Runner: tools.NewLocalRunner(
			toolReg,
			tools.NewTokenIssuer(toolKey, 0, nil),
			auth.NewExecutionAuthorizer(mysqlDB, ids, auth.DefaultToolPolicy()),
		),
		DB: mysqlDB,
	}

	executor := nodes.NewMux(sched, &nodes.LLMExecutor{
		Client: client,
		Prices: llmCfg.PriceTable,
		DB:     mysqlDB,
	}, toolExec, nodes.ApprovalExecutor{})
	return worker.Run(ctx, worker.Deps{
		DB:       mysqlDB,
		IDs:      ids,
		Executor: executor,
		Sched:    sched,
		Logger:   slog.Default(),
	}, cfg, asynq.RedisClientOpt{Addr: addr})
}
