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
	entim "braid/internal/enterprise/im"
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

// buildNotifier 是通知装配的决策面(T4.5 SMTP;T14.3 扩双通道扇出,
// D-t143 决策 5;抽函数 = 装配分支可测,cmd/worker/main_test.go):SMTP/IM
// 各自整组缺省 → 该通道不装(两通道全缺 → nil,worker 对 approval.notify
// 走降级 log-only ack);半配置/坏配置 → error 启动即败(notify.Validate
// / IMConfig.Validate);≥1 通道 → 直连(单通道)或 Fanout 扇出(双通道,
// per-channel 去重锚隔离失败域)。返回的 smtp/im 布尔是可用性指标真值
// (braid_smtp_configured / braid_im_configured——T6.4 通知丢失对账的
// 静默闸按通道关闭,单通道指标不再被「任意 notifier 非 nil」误点亮)。
func buildNotifier(getenv func(string) string, db *sql.DB, ids *snowflake.Generator, workerID string) (n worker.ApprovalNotifier, smtpOK, imOK bool, err error) {
	var chans []notify.Channel
	smtpCfg, err := notify.ConfigFromEnv(getenv)
	if err != nil {
		return nil, false, false, fmt.Errorf("worker: smtp config: %w", err)
	}
	if smtpCfg.Host != "" {
		s, err := notify.NewSender(notify.Deps{
			DB: db, IDs: ids, WorkerID: workerID, Logger: slog.Default(),
		}, smtpCfg)
		if err != nil {
			return nil, false, false, fmt.Errorf("worker: notify sender: %w", err)
		}
		chans = append(chans, notify.Channel{Label: "smtp", N: s})
	}
	imCfg, err := entim.IMConfigFromEnv(getenv)
	if err != nil {
		return nil, false, false, fmt.Errorf("worker: im config: %w", err)
	}
	// 共享键联合孤儿闸(T14.3):BRAID_APPROVAL_BASE_URL 是 SMTP/IM 双通道
	// 共享键,单通道视角不可判孤儿——两通道全未配置而它非空 = 死配置
	//(SMTP 反向半配置家族),在此启动即败;任一通道在场则它是合法供给。
	if smtpCfg.Host == "" && imCfg.WebhookURL == "" && smtpCfg.ApprovalBaseURL != "" {
		return nil, false, false, fmt.Errorf("worker: approval base url set but no notify channel (fix BRAID_SMTP_HOST / BRAID_IM_WEBHOOK_URL or clear BRAID_APPROVAL_BASE_URL)")
	}
	if imCfg.WebhookURL != "" {
		im, err := entim.NewIMSender(notify.Deps{
			DB: db, IDs: ids, WorkerID: workerID, Logger: slog.Default(),
		}, imCfg)
		if err != nil {
			return nil, false, false, fmt.Errorf("worker: im sender: %w", err)
		}
		chans = append(chans, notify.Channel{Label: "im", N: im})
	}
	switch len(chans) {
	case 0:
		return nil, false, false, nil
	case 1:
		n = chans[0].N
	default:
		n = notify.NewFanout(chans...)
	}
	return n, smtpCfg.Host != "", imCfg.WebhookURL != "", nil
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
	// T15.3 沙箱 owner 交接(D-t151 #5):owner = WorkerID,进程内 env 自写
	// 自读(EGRESS_ADDR handoff 同款;egress addr 写在下、此处写上——两消
	// 费点 SandboxStartupCheck 与 BuiltinToolsFromEnv 都在本行之后)。api
	// 进程不写不读(api 不执行工具,owner 恒空 = 永不构造 owner label)。
	// 值域若越界(hostname 异常形态),SandboxStartupCheck/argv 构造器响亮
	// 败——多 worker 同 daemon 的清扫互杀红线 fail-closed。
	if err := os.Setenv("BRAID_TOOL_SANDBOX_OWNER", cfg.WorkerID); err != nil {
		return fmt.Errorf("worker: sandbox owner handoff: %w", err)
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
	// T17.3 按组件授权断言:worker 的租约仲裁面(snowflake_node_lease 四面)。
	if err := errors.Join(
		preflight.CheckSchemaVersion(ctx, mysqlDB),
		preflight.CheckAuditAppendOnly(ctx, mysqlDB, preflight.AppendOnlyStrict),
		preflight.CheckAuditPartitions(ctx, mysqlDB),
		preflight.CheckComponentGrants(ctx, mysqlDB, preflight.ComponentWorker),
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
	traceProv, err := tracing.NewProvider(tracing.Config{Exporter: traceCfg.Exporter, Endpoint: traceCfg.Endpoint, ServiceName: "braid-worker"})
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

	// T20.4 沙箱清扫前租约活性复核(D-t151 决策 8-③ 转正):决策段独立
	// 成 sandboxSweepHoldoff(单测锚面),语义与边界详见 D-t204——判活
	// DB 端 NOW(3) 口径同 t15.2;时点刻意在 StartLease 之前(复占子面:
	// acquire 后同行即自身持有,后查会漏活体 peer)。
	sweepHoldoff := sandboxSweepHoldoff(ctx, mysqlDB, cfg.WorkerID)

	// T15.2 节点号租约仲裁(D-t151 决策 2):三层解析在 ConfigFromEnv,
	// 仲裁在此——显式号(含内建默认 2)被持即拒启,auto 池线性抢注;
	// SIGTERM 优雅释放;续期失败 = 停发 + 优雅下线 asynq 消费 + 响亮
	// 退出(只停签发会把副本变成烧 MaxRetry=1 预算的任务黑洞)。Holder
	// 对齐(T20.4,D-t204 决策 3)与能力宣告(T15.6,D-t151 决策 6)
	// 钉在 workerLeaseConfig(单测锚面)。
	lease, err := snowflake.StartLease(ctx, mysqlDB, workerLeaseConfig(cfg))
	if err != nil {
		return fmt.Errorf("worker: snowflake lease: %w", err)
	}
	defer releaseLease("worker", lease)
	ids, err := snowflake.New(lease.Node())
	if err != nil {
		return fmt.Errorf("worker: snowflake: %w", err)
	}
	lease.Guard(ids)
	slog.Info("worker: snowflake lease acquired", "node", lease.Node(), "spec", cfg.SnowflakeNode)

	// lost → 取消派生 ctx:worker.Run 的 <-ctx.Done()+srv.Shutdown() 即
	// 「优雅下线 asynq 消费」(决策 2);排空完成后响亮返回非 Canceled
	// 错误(main 走 Exit(1) 失败重启通道,复航 = 重启重新 acquire)。
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var lostErr error
	lostDone := make(chan struct{})
	go func() {
		defer close(lostDone)
		select {
		case err, ok := <-lease.Lost():
			if ok {
				lostErr = err
				cancel()
			}
		case <-ctx.Done():
		}
	}()
	// Metrics 适配 = metrics.Set(T12.2 起:LLM 丢弃 + Redispatches{attempt_failed}
	// 双发射;原 mset.LLM 单方法注入已随 Set 适配方法收编)。接线验证纪律
	// (D-t64 两次漏接教训):grep 可见此行即接线面。
	sched, err := scheduler.New(scheduler.Deps{DB: mysqlDB, IDs: ids, Logger: slog.Default(), Metrics: mset})
	if err != nil {
		return fmt.Errorf("worker: scheduler: %w", err)
	}

	// 通知装配(T4.5 SMTP;T14.3 IM 双通道):决策面在 buildNotifier
	//(两通道整组缺省 → nil 降级;半配置/坏配置启动即败)。凭证仅经 env
	// 注入,不进日志/审计(铁律)。
	notifier, smtpOK, imOK, err := buildNotifier(os.Getenv, mysqlDB, ids, cfg.WorkerID)
	if err != nil {
		return err
	}
	// 通道可用性按通道暴露:通知丢失对账告警的静默闸(未配置通道的
	// approval.notified / approval.im_notified 设计性缺席,缺失不告警,
	// D-t45;T14.3 起双指标,单通道未配不误点亮)。
	if smtpOK {
		mset.Worker.SMTPConfigured.Set(1)
	}
	if imOK {
		mset.Worker.IMConfigured.Set(1)
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
	// T10.2 egress 启动段先行(装配依赖其地址):docker 模式下幂等建
	// --internal 内网 + 网关上起默认拒绝 CONNECT 代理,地址写回
	// BRAID_TOOL_SANDBOX_EGRESS_ADDR 供下方装配单读(信封 proxy 字段与
	// 代理 allow 集同源);off = nil 零接触。T15.4:网名带 worker_id
	// 实例段(一 worker 一网,D-t151 决策 5)——实例名同样写回
	// BRAID_TOOL_SANDBOX_EGRESS_NETWORK 覆写基段,容器 --network 位与
	// 建网/监听三面同源;同 worker_id 二启 = 同网关同端口 bind 拒启
	// (显式端口;EGRESS_PORT=0 内核指派 = dev/test,不执法)。
	egress, err := tools.SandboxEgressStartup(ctx, os.Getenv, cfg.WorkerID)
	if err != nil {
		return fmt.Errorf("worker: sandbox egress startup: %w", err)
	}
	if egress != nil {
		if err := os.Setenv("BRAID_TOOL_SANDBOX_EGRESS_ADDR", egress.ProxyURL.String()); err != nil {
			return fmt.Errorf("worker: sandbox egress addr handoff: %w", err)
		}
		if err := os.Setenv("BRAID_TOOL_SANDBOX_EGRESS_NETWORK", egress.Network); err != nil {
			return fmt.Errorf("worker: sandbox egress network handoff: %w", err)
		}
		slog.Info("worker: sandbox egress proxy serving", "addr", egress.ProxyURL.String(), "network", egress.Network)
	}
	builtins, err := tools.BuiltinToolsFromEnv(os.Getenv)
	if err != nil {
		return fmt.Errorf("worker: builtin tools: %w", err)
	}
	// T10.1 沙箱启动接线:mode=docker 时 Probe 部署性前移(缺 bin/daemon
	// 不可达/镜像缺失 = 拒启,绝不静默回落 local)+ label 清扫(cancel 窗
	// 僵尸与 kill -9 孤儿兜底);off = 无操作零 docker 接触(dev 栈照跑)。
	// t20.4:sweepHoldoff 非空 ⇒ 清扫跳过(上方 StartLease 前复核结论,
	// probe 照跑不因跳过失效)。
	swept, err := tools.SandboxStartupCheck(ctx, os.Getenv, sweepHoldoff)
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
		// T18.2 每执行 token 预算闸(D-t182):旋钮经 llm.ConfigFromEnv→Validate
		// 启动即败接线已闭环;IDs 供拒绝审计行 snowflake id(拒绝现场独立小事务)。
		Budget: llmCfg.TokenBudget,
		IDs:    ids,
	}, toolExec, nodes.ApprovalExecutor{})
	runErr := worker.Run(ctx, worker.Deps{
		DB:       mysqlDB,
		IDs:      ids,
		Executor: executor,
		Sched:    sched,
		Notifier: notifier,
		Logger:   slog.Default(),
		Metrics:  mset.Worker,                      // T6.4:notify 计数 + archived 探测(漏接 = 观测生产死代码)
		Tracer:   traceProv.Tracer("braid/worker"), // T6.2:消费半边 payload extract 续链
	}, cfg, asynq.RedisClientOpt{Addr: addr})
	cancel() // 幂等:Run 早退错误路径也放行 <-lostDone(否则悬挂,review MEDIUM)
	<-lostDone
	if runErr != nil {
		return runErr
	}
	if lostErr != nil {
		return fmt.Errorf("worker: %w", lostErr)
	}
	return nil
}

// releaseLease 是 SIGTERM 语义半边(决策 2:优雅停机必须 DELETE 释放):
// asynq 排空完成后删本行;lost 路径调用安全(行非本持,幂等)。独立
// Background ctx——根 ctx 在停机路径已取消。
// sandboxSweepHoldoff 是 t20.4 沙箱清扫前租约活性复核的决策段(D-t151
// 决策 8-③ 转正,D-2026-10-05-t204):同 owner 的 worker 租约在场
// (expires_at >= DB 端 NOW(3),口径与 t15.2 一致)⇒ 非空跳过原因——
// 活体同 owner 冲突(部署契约违例)下,后来者不清活体 peer 的在飞沙箱;
// 死 owner 残容器照清("" = 清扫执行)。复核不可达 ⇒ 非空(fail-safe
// 跳过,误跳过 < 误杀,同 D-t151 决策 6 取舍);误跳过窗(kill -9 或
// release 失败残窗 <TTL 内重启)由下次启动/runbook 收敛(D-t204 边界①)。
// 时点契约 = StartLease 之前调用:显式号同 holder 复占后同行即自身持有,
// acquire 后按持有行查会把活体 peer 漏成自身;acquire 前任何同 holder
// 在场行都可见(含即将被复占的行,D-t204 决策 2)。
func sandboxSweepHoldoff(ctx context.Context, dbh *sql.DB, workerID string) string {
	peer, err := snowflake.HolderLive(ctx, dbh, snowflake.KindWorker, workerID)
	if err != nil {
		slog.Warn("worker: sandbox sweep holdoff: holder liveness probe failed", "err", err)
		return fmt.Sprintf("liveness probe failed: %v", err)
	}
	if peer != nil {
		slog.Warn("worker: sandbox sweep holdoff: live peer holds this owner",
			"owner", workerID, "peer_node", peer.Node, "peer_holder", peer.HolderID, "peer_expires_at", peer.ExpiresAt)
		return fmt.Sprintf("live peer node %d (%s) expires %s", peer.Node, peer.HolderID, peer.ExpiresAt)
	}
	return ""
}

// workerLeaseConfig 钉 worker 租约装配(D-t204 决策 3):Holder = cfg.
// WorkerID——身份唯一性契约与 owner 同源(D-t151 决策 5),缺省路径
// WorkerID = hostname 与 DefaultHolder 字节等价;显式 BRAID_WORKER_ID
// 时 holder 与 owner 同串,sandboxSweepHoldoff 的复核才命中(一行回退
// DefaultHolder = 显式形态下复核恒空转,误杀面复活)。能力宣告(T15.6,
// D-t151 决策 6:worker 的 mode=all 续跑复核腿)同锚于此。独立构造
// 单点 = 单测锚面。
func workerLeaseConfig(cfg worker.Config) snowflake.LeaseConfig {
	return snowflake.LeaseConfig{
		Kind:         snowflake.KindWorker,
		Spec:         cfg.SnowflakeNode,
		Holder:       cfg.WorkerID,
		Capabilities: snowflake.DeclaredCapabilities(),
	}
}

func releaseLease(component string, lease *snowflake.Lease) {
	rctx, cancel := context.WithTimeout(context.Background(), traceShutdownGrace)
	defer cancel()
	if err := lease.Release(rctx); err != nil {
		slog.Warn(component+": snowflake lease release", "err", err)
	}
}
