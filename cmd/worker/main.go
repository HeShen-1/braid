// worker 入口:读 env → 组装 → 信号 → worker.Run。薄 main,零业务逻辑;
// 与 relay 同款骨架(Q14:交付时合并为单二进制多入口)。T3.6 起装配真实
// 节点执行器(llm 经 OpenAI 兼容适配器);T3.2 接入 tool 执行器(registry +
// LocalRunner + scoped token);T4.1 接入 approval 执行器(纯挂起决策,
// 挂起事务在 scheduler.Suspend)。
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
	"github.com/hibiken/asynq"

	"braid/internal/auth"
	"braid/internal/db"
	"braid/internal/llm"
	"braid/internal/metrics"
	"braid/internal/nodes"
	"braid/internal/notify"
	"braid/internal/preflight"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
	"braid/internal/tools"
	"braid/internal/tracing"
	"braid/internal/worker"
)

// traceShutdownGrace 是 OTel batcher 排空预算(独立停机窗)。
const traceShutdownGrace = 2 * time.Second

// buildNotifier 是 T4.5 SMTP 通知装配的决策面(抽函数 = 装配分支可测,
// cmd/worker/main_test.go):整组缺省(BRAID_SMTP_HOST 空)→ nil(worker
// 对 approval.notify 走降级 log-only ack);反向/正向半配置、坏配置 →
// error 启动即败(notify.Validate);全配置 → notify.Sender。
func buildNotifier(getenv func(string) string, db *sql.DB, ids *snowflake.Generator, workerID string) (worker.ApprovalNotifier, error) {
	cfg, err := notify.ConfigFromEnv(getenv)
	if err != nil {
		return nil, fmt.Errorf("worker: smtp config: %w", err)
	}
	if cfg.Host == "" {
		return nil, nil
	}
	s, err := notify.NewSender(notify.Deps{
		DB: db, IDs: ids, WorkerID: workerID, Logger: slog.Default(),
	}, cfg)
	if err != nil {
		return nil, fmt.Errorf("worker: notify sender: %w", err)
	}
	return s, nil
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
	if cfg.MySQLDSN == "" { // T7.1 收编:cmd 直读 env 清零,断言读 cfg 字段
		return errors.New("worker: BRAID_MYSQL_DSN is required")
	}
	mysqlDB, err := sql.Open("mysql", db.CanonicalDSN(cfg.MySQLDSN))
	if err != nil {
		return fmt.Errorf("worker: open mysql: %w", err)
	}
	if err := db.ConfigurePool(mysqlDB, cfg.Pool); err != nil { // D-t63 移交回收;T7.1:三值经 BRAID_DB_* env 化
		return fmt.Errorf("worker: %w", err)
	}
	slog.Info("db: pool", "max_open", cfg.Pool.MaxOpen, "max_idle", cfg.Pool.MaxIdle, "conn_max_lifetime", cfg.Pool.ConnMaxLifetime)
	defer mysqlDB.Close() //nolint:errcheck // 主路径退出,关闭错误无处置义
	if err := mysqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("worker: mysql ping: %w", err)
	}

	// T7.3 安装自检:worker 是审计写方(节点事件),MySQL 三项全硬拒;词表
	// 自检 = 下方 PrebuildTokenizers(T7.2 接线,D-t72 #4 复用不重复)。
	if err := errors.Join(
		preflight.CheckSchemaVersion(ctx, mysqlDB),
		preflight.CheckAuditAppendOnly(ctx, mysqlDB, preflight.AppendOnlyStrict),
		preflight.CheckAuditPartitions(ctx, mysqlDB),
	); err != nil {
		return fmt.Errorf("worker: %w", err)
	}

	addr := cfg.RedisQueueAddr
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

	// T7.3:队列 Redis 契约(noeviction + AOF everysec)硬拒——worker 是队列
	// 数据承载方,破约 = 逐出静默丢任务、重启烧在途租约。
	if err := preflight.CheckRedisQueue(ctx, addr); err != nil {
		return fmt.Errorf("worker: %w", err)
	}

	// T6.4:Prometheus 指标面。BRAID_METRICS_ADDR 空 = 不启(观测可选,
	// 与内置工具「未配置即不注册」同姿势);registry 每进程新建。
	mset, mreg := metrics.NewSet()
	if _, ok, err := metrics.Serve(ctx, cfg.MetricsAddr, mreg); err != nil {
		return fmt.Errorf("worker: %w", err)
	} else if ok {
		slog.Info("metrics: serving", "addr", cfg.MetricsAddr)
	}
	go metrics.CollectDBPool(ctx, mysqlDB, mset.DBPool) // T7.1:池 gauge 30s 常量周期(braid_db_pool_*)

	// T6.2:OTel trace 贯穿——off = 丢弃导出器(trace id 照生成,审计列不随
	// 观测开关退化);消费侧 asynq middleware 从 payload extract 续链。
	// D-2026-09-27-t62-otel-trace。
	traceCfg := tracing.ConfigFromEnv(os.Getenv)
	if err := traceCfg.Validate(); err != nil {
		return fmt.Errorf("worker: %w", err)
	}
	traceProv, err := tracing.NewProvider(tracing.Config{Exporter: traceCfg.Exporter, ServiceName: "braid-worker"})
	if err != nil {
		return fmt.Errorf("worker: %w", err)
	}
	defer func() {
		shCtx, cancel := context.WithTimeout(context.Background(), traceShutdownGrace)
		defer cancel()
		if err := traceProv.Shutdown(shCtx); err != nil {
			slog.Warn("worker: trace provider shutdown", "err", err)
		}
	}()
	slog.Info("worker: tracing", "exporter", traceCfg.Exporter)

	// T3.6 生产装配:llm 配置缺 URL/KEY/PRICE_TABLE 即启动失败(生产 worker
	// 必须能调 LLM;组合预算上界在 Validate 内启动即败,HIGH① 拍板)。
	llmCfg, err := llm.ConfigFromEnv(os.Getenv)
	if err != nil {
		return fmt.Errorf("worker: llm config: %w", err)
	}
	client, err := llm.NewOpenAICompat(llmCfg, mset.LLM)
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
	sched, err := scheduler.New(scheduler.Deps{DB: mysqlDB, IDs: ids, Logger: slog.Default(), Metrics: mset.LLM})
	if err != nil {
		return fmt.Errorf("worker: scheduler: %w", err)
	}

	// T4.5 SMTP 通知装配:决策面在 buildNotifier(整组缺省 → nil 降级;
	// 半配置/坏配置启动即败)。凭证仅经 env 注入,不进日志/审计(铁律)。
	notifier, err := buildNotifier(os.Getenv, mysqlDB, ids, cfg.WorkerID)
	if err != nil {
		return err
	}
	// SMTP 可用性作为指标暴露:通知丢失对账告警的静默闸(未配置时
	// approval.notified 设计性缺席,缺失不告警,D-t45)。
	if notifier != nil {
		mset.Worker.SMTPConfigured.Set(1)
	}

	// T3.2/T3.5 工具运行时装配 + T5.2 执行时 RBAC 门:registry(内置
	// http/file_read 按配置注册,未配置即不注册——空 = 一切 tool 节点
	// tool_not_found 收口;api 与 worker 经 BuiltinToolsFromEnv 单源装配,
	// 注册集与发布白名单一致;T10.1:mode=docker 时装配缝同名换装
	// sandboxCarrier,off = 进程内 builtin 逐字节现状)+ scoped token
	// 签发器(BRAID_TOOL_TOKEN_SECRET 缺省 = 进程内随机,重启即轮换——
	// P0 校验点在同进程调用门,自洽;P1 工具回调平台时经部署配置显式
	// 供给)+ 执行时授权器(发布期白名单之外的调用现场再校验,拒绝落
	// tool.call.denied 审计;默认策略 author|admin,system 触发哨兵放行)。
	builtins, err := tools.BuiltinToolsFromEnv(os.Getenv)
	if err != nil {
		return fmt.Errorf("worker: builtin tools: %w", err)
	}
	// T10.1 沙箱启动接线:mode=docker 时 Probe 部署性前移(缺 bin/daemon
	// 不可达/镜像缺失 = 拒启,绝不静默回落 local)+ label 清扫(cancel 窗
	// 僵尸与 kill -9 孤儿兜底);off = 无操作零 docker 接触(dev 栈照跑)。
	swept, err := tools.SandboxStartupCheck(ctx, os.Getenv)
	if err != nil {
		return fmt.Errorf("worker: sandbox startup check: %w", err)
	}
	if len(swept) > 0 {
		slog.Warn("worker: sandbox startup swept orphaned containers", "count", len(swept), "ids", swept)
	}
	toolReg, err := tools.NewRegistry(builtins...)
	if err != nil {
		return fmt.Errorf("worker: tool registry: %w", err)
	}
	toolKey := cfg.ToolTokenSecret // T7.1 收编:hex 解析进 worker.ConfigFromEnv
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
		Notifier: notifier,
		Logger:   slog.Default(),
		Metrics:  mset.Worker,                      // T6.4:notify 计数 + archived 探测(漏接 = 观测生产死代码)
		Tracer:   traceProv.Tracer("braid/worker"), // T6.2:消费半边 payload extract 续链
	}, cfg, asynq.RedisClientOpt{Addr: addr})
}
