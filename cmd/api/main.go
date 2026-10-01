// api 入口:读 env → 组装 → serve → 信号优雅停机。薄 main,零业务逻辑;
// 与 worker/relay 同款骨架(Q14:交付时合并为单二进制多入口)。
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"

	"braid/internal/api"
	"braid/internal/auth"
	"braid/internal/db"
	"braid/internal/metrics"
	"braid/internal/preflight"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
	"braid/internal/tools"
	"braid/internal/tracing"
)

// shutdownGrace 是优雅停机窗:在途请求排空、新请求已由 Shutdown 拒绝。
const shutdownGrace = 10 * time.Second

// traceShutdownGrace 是 OTel batcher 排空预算(独立于 HTTP 停机窗)。
const traceShutdownGrace = 2 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return // 停机信号落在启动窗口:正常停机,退出码 0(不算失败重启)
		}
		slog.Error("api: fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := api.ConfigFromEnv(os.Getenv) // T7.1 配置体系唯一换装点
	if err != nil {
		return err
	}
	if cfg.MySQLDSN == "" { // T7.1 收编:cmd 直读 env 清零,断言读 cfg 字段
		return errors.New("api: BRAID_MYSQL_DSN is required")
	}
	dbh, err := sql.Open("mysql", db.CanonicalDSN(cfg.MySQLDSN))
	if err != nil {
		return fmt.Errorf("api: open mysql: %w", err)
	}
	if err := db.ConfigurePool(dbh, cfg.Pool); err != nil { // D-t63 移交回收;T7.1:三值经 BRAID_DB_* env 化
		return fmt.Errorf("api: %w", err)
	}
	slog.Info("db: pool", "max_open", cfg.Pool.MaxOpen, "max_idle", cfg.Pool.MaxIdle, "conn_max_lifetime", cfg.Pool.ConnMaxLifetime)
	defer dbh.Close() //nolint:errcheck // 主路径退出,关闭错误无处置义
	if err := dbh.PingContext(ctx); err != nil {
		return fmt.Errorf("api: mysql ping: %w", err)
	}

	// T7.3 安装自检:api 是审计写方(触发审计二段事务),MySQL 三项全硬拒;
	// 错误自带修复提示,经 main 的 fatal 口径输出(slog.Error + Exit(1))。
	if err := errors.Join(
		preflight.CheckSchemaVersion(ctx, dbh),
		preflight.CheckAuditAppendOnly(ctx, dbh, preflight.AppendOnlyStrict),
		preflight.CheckAuditPartitions(ctx, dbh),
	); err != nil {
		return fmt.Errorf("api: %w", err)
	}
	ids, err := snowflake.New(cfg.SnowflakeNode)
	if err != nil {
		return fmt.Errorf("api: snowflake: %w", err)
	}
	// 发布白名单真源(T3.2 换源):tools.Registry 注册名。内置 http/
	// file_read 按配置注册(T3.5,未配置即不注册 = 引用即发布被拒);
	// api 与 worker 经 BuiltinToolsFromEnv 单源装配同一注册集。
	builtins, err := tools.BuiltinToolsFromEnv(os.Getenv)
	if err != nil {
		return fmt.Errorf("api: builtin tools: %w", err)
	}
	toolReg, err := tools.NewRegistry(builtins...)
	if err != nil {
		return fmt.Errorf("api: tool registry: %w", err)
	}
	// 触发缝真源(T5.5):scheduler.Instantiate 物化(钉版四件套 + 全节点行
	// + outbox execution.start),Deps 仅 MySQL+snowflake——api 进程零 Redis,
	// 推进链(outbox→relay→asynq→worker)在 worker/relay 进程侧。
	// T15.5:回滚容量闸(锁窗守卫,BRAID_ROLLBACK_MAX_ROWS,缺省
	// DefaultMaxRollbackRows 定档值;坏值启动即败)。
	maxRollbackRows, err := scheduler.MaxRollbackRowsFromEnv(os.Getenv)
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}
	sched, err := scheduler.New(scheduler.Deps{DB: dbh, IDs: ids, MaxRollbackRows: maxRollbackRows})
	if err != nil {
		return fmt.Errorf("api: scheduler: %w", err)
	}

	// 认证身份源真源(T5.3):内置 BuiltinProvider = 用户名口令 + bcrypt
	// (默认);P1 凭证核验型插件(如 LDAP bind)在此换装,契约见
	// internal/auth/identity.go。
	// T6.4:Prometheus 指标面——与 /healthz 同端口同回环绑定(BRAID_API_ADDR
	// 部署约定);登录防御计数(login_audit 0005 的指标半边)在此注册。
	mset, mreg := metrics.NewSet()
	go metrics.CollectDBPool(ctx, dbh, mset.DBPool) // T7.1:池 gauge 30s 常量周期(braid_db_pool_*)
	// T6.2:OTel trace 贯穿——off = 丢弃导出器(span/trace id 照生成,审计列
	// 不随观测开关退化);Tracer 显式注入组件,不注册 otel 全局(镜像 metrics
	// 先例)。P0 装配面 = api + worker;D-2026-09-27-t62-otel-trace。
	traceCfg := tracing.ConfigFromEnv(os.Getenv)
	if err := traceCfg.Validate(); err != nil {
		return fmt.Errorf("api: %w", err)
	}
	traceProv, err := tracing.NewProvider(tracing.Config{Exporter: traceCfg.Exporter, Endpoint: traceCfg.Endpoint, ServiceName: "braid-api"})
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}
	defer func() {
		shCtx, cancel := context.WithTimeout(context.Background(), traceShutdownGrace)
		defer cancel()
		if err := traceProv.Shutdown(shCtx); err != nil {
			slog.Warn("api: trace provider shutdown", "err", err)
		}
	}()
	slog.Info("api: tracing", "exporter", traceCfg.Exporter)
	apiSrv := api.NewServer(cfg, dbh, ids, toolReg, sched, auth.BuiltinProvider{DB: dbh})
	apiSrv.LoginMetrics(mset.Login)
	apiSrv.RollbackMetrics(mset.Rollback) // T9.2:回滚面三计数器(D-t92 决策 10)
	apiSrv.Tracing(traceProv.Tracer("braid/api"))
	// OIDC 插件(T11.1,D-2026-09-28-t111-oidc-subject-anchor):BRAID_OIDC_*
	// 组经 ConfigFromEnv 解析(半开启动即败);配置完整才构造——discovery
	// 启动期拉取,issuer 错/不可达即启动失败。未配置零行为(路由不挂载),
	// 内置 login 不受影响;主体映射 = 本地账号锚定(claim 值 == 本地账号名)。
	if cfg.OIDC.Configured() {
		oidcProv, err := auth.NewOIDCProvider(ctx, cfg.OIDC, dbh)
		if err != nil {
			return fmt.Errorf("api: %w", err)
		}
		apiSrv.OIDCProvider(oidcProv)
		slog.Info("api: oidc login enabled", "issuer", cfg.OIDC.Issuer, "claim", cfg.OIDC.Claim)
	}
	router := apiSrv.Router()
	// /metrics 与 /healthz 同端口同回环绑定(BRAID_API_ADDR 部署约定)。
	// 公开面部署注记:反向代理只应转发 /api/v1 与 /ui/,/metrics 与
	// /healthz 留在内网(review advisory)。
	router.GET("/metrics", gin.WrapH(metrics.Handler(mreg)))
	srv := buildHTTPServer(cfg, router)
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	slog.Info("api: serving", "addr", cfg.Addr)
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		return srv.Shutdown(shCtx)
	}
}

// buildHTTPServer 把 cfg 四超时字段装配进 http.Server(#67-②:装配步纯函数
// 化,表驱动单测锚定映射;config 三态锚在 internal/api/config_test.go)。
func buildHTTPServer(cfg api.Config, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           h,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout, // T7.1 收编(原 cmd 常量,默认值原样)
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}
}
