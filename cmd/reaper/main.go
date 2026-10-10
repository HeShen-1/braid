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
	"github.com/hibiken/asynq"
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
	// 分区 → 永远起不来」的死锁。T17.3 按组件授权断言:reaper 的特征写面
	// (outbox 判死重派、audit_log 分区回收的 ALTER+DROP、租约四面)。
	if err := errors.Join(
		preflight.CheckSchemaVersion(ctx, mysqlDB),
		// AllowLoginAuditDelete:janitor purgeLoginAudit 保留期清理的表级
		// DELETE 豁免(audit_log 面仍全拒;授权矩阵 deploy/README.md §5)。
		preflight.CheckAuditAppendOnly(ctx, mysqlDB, preflight.AllowLoginAuditDelete),
		preflight.CheckComponentGrants(ctx, mysqlDB, preflight.ComponentReaper),
	); err != nil {
		return fmt.Errorf("reaper: %w", err)
	}
	if err := preflight.CheckAuditPartitions(ctx, mysqlDB); err != nil {
		slog.Warn("reaper: audit partition watermark", "err", err)
	}

	// T15.2 节点号租约仲裁(D-t151 决策 2):三层解析在 ConfigFromEnv,
	// 仲裁在此——内建缺省 = 显式 3(T20.9/D-2026-10-10-t209;PID 派生已
	// 退役:容器 PID 恒 1 撞 api 默认 1),显式号被持即拒启;多副本经 env
	// "auto" 走池分配;SIGTERM 优雅释放;续期失败 = 停发 + 响亮退出(复航 =
	// 重启重新 acquire)。
	lease, err := snowflake.StartLease(ctx, mysqlDB, snowflake.LeaseConfig{
		Kind:   snowflake.KindReaper,
		Spec:   cfg.SnowflakeNode,
		Holder: snowflake.DefaultHolder(),
		// T15.6 能力宣告(D-t151 决策 6):reaper 的 mode=all 已定局判定腿。
		Capabilities: snowflake.DeclaredCapabilities(),
	})
	if err != nil {
		return fmt.Errorf("reaper: snowflake lease: %w", err)
	}
	defer releaseLease("reaper", lease)
	ids, err := snowflake.New(lease.Node())
	if err != nil {
		return fmt.Errorf("reaper: snowflake: %w", err)
	}
	lease.Guard(ids)
	slog.Info("reaper: snowflake lease acquired", "node", lease.Node(), "spec", cfg.SnowflakeNode)

	// lost → 取消派生 ctx:扫描/flush/janitor 循环优雅退出;排空完成后
	// 响亮返回非 Canceled 错误(main 走 Exit(1) 失败重启通道)。
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
	// 选主 fail-open(D-t25 #1,与 relay/worker 的「Redis 可达自检启动即败」
	// 刻意相反):正确性结构性不依赖 Redis,拉起一个连不上 Redis 的 reaper
	// 仍完全可用(leaderless 全跑),拒启反而制造「断 Redis 期间无人对账」
	// 的停摆面——恰是验收要消灭的场景。不可达只 warn。
	// T6.4:Prometheus 指标面(裁决计数 + flush 观测 + 周期 gauge 采集)。
	// BRAID_METRICS_ADDR 空 = 不启;Deps.Metrics 与采集器同生命周期。
	// T20.1:构造先于 scheduler(编排层 fence 拒绝计数经 MetricsSink 注入,
	// 对抗审计 MEDIUM 修复——zombie reaper 停写面对告警可见)。
	mset, mreg := metrics.NewSet()
	sched, err := scheduler.New(scheduler.Deps{DB: mysqlDB, IDs: ids, Lease: lease, Metrics: mset, Queue: cfg.Queue})
	if err != nil {
		return fmt.Errorf("reaper: scheduler: %w", err)
	}
	if _, ok, err := metrics.Serve(ctx, cfg.MetricsAddr, mreg); err != nil {
		return fmt.Errorf("reaper: %w", err)
	} else if ok {
		slog.Info("metrics: serving", "addr", cfg.MetricsAddr)
	}
	go metrics.CollectDBPool(ctx, mysqlDB, mset.DBPool) // T7.1:池 gauge 30s 常量周期(braid_db_pool_*)

	deps := reaper.Deps{DB: mysqlDB, Sched: sched, IDs: ids, Lease: lease, Metrics: mset.Reaper}
	if cfg.RedisQueueAddr != "" {
		// T7.3:队列 Redis 契约探测仅 warn(D-t25 fail-open 同族:锁实例破约
		// 的失败模式 = 锁丢失退化为 leaderless,安全;拒启反制造停摆面)。
		if err := preflight.CheckRedisQueue(ctx, cfg.RedisQueueAddr); err != nil {
			slog.Warn("reaper: redis queue policy preflight", "err", err)
		}
		deps.Locker = reaper.RedisLocker(redis.NewClient(&redis.Options{Addr: cfg.RedisQueueAddr}))
		// T17.4(D-2026-10-04-t174):asynq 活性对拍 probe——分支 B 重派前的
		// 陈旧 DISPATCHED 死证据复核(积压 ≠ 死,D-t25#10 残余窗收口)。
		// 生命周期随 run;按需调用(只在 reapAttempt 哨兵路径),无后台探测。
		// ReadTimeout 显式钉 3s(黑洞 Redis 下单次探测至多 ~3s,不吞 tick;
		// Inspector 无 ctx 面,超时预算在连接层承载,review LOW 修)。
		insp := asynq.NewInspector(asynq.RedisClientOpt{
			Addr:        cfg.RedisQueueAddr,
			ReadTimeout: 3 * time.Second,
		})
		defer func() { _ = insp.Close() }() //nolint:errcheck // 停机路径,关闭错误无处置义
		deps.AsynqProbe = inspectorProbe{insp: insp}
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
	// T9.3 启动自检:排空待校验链头(断链闩在此落定,写路径随即拒绝;
	// infra 失败只响亮不阻断启动——flush/janitor 职责不因此停摆)。
	r.StartupVerify(ctx)
	runErr := r.Run(ctx)
	cancel() // 幂等:Run 早退错误路径也放行 <-lostDone(镜像 cmd/worker,review MEDIUM)
	<-lostDone
	if runErr != nil {
		return runErr
	}
	if lostErr != nil {
		return fmt.Errorf("reaper: %w", lostErr)
	}
	return nil
}

// releaseLease 是 SIGTERM 语义半边(决策 2:优雅停机必须 DELETE 释放):
// 扫描循环退出后删本行;lost 路径调用安全(行非本持,幂等)。独立
// Background ctx——根 ctx 在停机路径已取消。
func releaseLease(component string, lease *snowflake.Lease) {
	rctx, cancel := context.WithTimeout(context.Background(), traceShutdownGrace)
	defer cancel()
	if err := lease.Release(rctx); err != nil {
		slog.Warn(component+": snowflake lease release", "err", err)
	}
}
