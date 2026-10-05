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
	entldap "braid/internal/enterprise/ldap"
	entoidc "braid/internal/enterprise/oidc"
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
	// T17.3 按组件授权断言:api 的租约仲裁面(snowflake_node_lease 四面)。
	if err := errors.Join(
		preflight.CheckSchemaVersion(ctx, dbh),
		preflight.CheckAuditAppendOnly(ctx, dbh, preflight.AppendOnlyStrict),
		preflight.CheckAuditPartitions(ctx, dbh),
		preflight.CheckComponentGrants(ctx, dbh, preflight.ComponentAPI),
	); err != nil {
		return fmt.Errorf("api: %w", err)
	}
	// T15.2 节点号租约仲裁(D-t151 决策 2):三层解析在 ConfigFromEnv,
	// 仲裁在此——显式号(含内建默认 1)被持即拒启(错误带持有者/到期/
	// runbook),auto 池线性抢注;SIGTERM 优雅释放,续期失败 = 停发 +
	// 响亮退出(复航 = 重启重新 acquire)。
	lease, err := snowflake.StartLease(ctx, dbh, snowflake.LeaseConfig{
		Kind:   snowflake.KindAPI,
		Spec:   cfg.SnowflakeNode,
		Holder: snowflake.DefaultHolder(),
		// T15.6 能力宣告(D-t151 决策 6):acquire 即写,api 硬闸按在场
		// 全宣告放行 mode=all 发布/触发。
		Capabilities: snowflake.DeclaredCapabilities(),
	})
	if err != nil {
		return fmt.Errorf("api: snowflake lease: %w", err)
	}
	defer releaseLease("api", lease)
	ids, err := snowflake.New(lease.Node())
	if err != nil {
		return fmt.Errorf("api: snowflake: %w", err)
	}
	lease.Guard(ids)
	slog.Info("api: snowflake lease acquired", "node", lease.Node(), "spec", cfg.SnowflakeNode)
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
	// (默认);凭证核验型插件在此换装(LDAP bind = T17.6,下方
	// BRAID_LDAP_* 组),契约见 internal/auth/identity.go。
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
	// LDAP bind 插件(T17.6,D-2026-10-04-t176;企业树 internal/enterprise/
	// ldap):BRAID_LDAP_* 组半开启动即败;组完整才构造 + 换装凭证核验缝
	// idp(构造期启动探测 = 目录不可达/服务凭据错即启动失败,镜像下方
	// entoidc discovery 先例)并同步换审计归因词(login_audit.method =
	// 'ldap',0014);未配置零行为(内置口令登录不受影响)。锚定 = 本地
	// 账号(auth.AnchorLoginUser,D-t111 同则),角色/状态自本地库;服务
	// 凭据仅经 env 注入,不进日志(铁律)。
	idp := auth.IdentityProvider(auth.BuiltinProvider{DB: dbh})
	ldapCfg, err := entldap.ParseLDAPConfig(os.Getenv)
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}
	if ldapCfg.Configured() {
		ldapProv, err := entldap.NewLDAPBindProvider(ctx, ldapCfg, dbh)
		if err != nil {
			return fmt.Errorf("api: %w", err)
		}
		idp = ldapProv
		slog.Info("api: ldap login enabled", "addr", ldapCfg.Addr, "base_dn", ldapCfg.BaseDN, "starttls", ldapCfg.StartTLS)
	}
	apiSrv := api.NewServer(cfg, dbh, ids, toolReg, sched, idp)
	if ldapCfg.Configured() {
		apiSrv.CredentialLoginMethod(api.LoginMethodLDAP) // login_audit.method 归因(0014)
	}
	apiSrv.LoginMetrics(mset.Login)
	apiSrv.RollbackMetrics(mset.Rollback)   // T9.2:回滚面三计数器(D-t92 决策 10)
	apiSrv.CancelMetrics(mset.Cancel)       // T19.3:取消面三计数器(D-t193 决策 5)
	apiSrv.FleetGateMetrics(mset.FleetGate) // T15.6:mode=all 硬闸拒绝计数
	apiSrv.Tracing(traceProv.Tracer("braid/api"))
	// OIDC 插件(T11.1,D-2026-09-28-t111-oidc-subject-anchor;T16.1 起协议
	// 实现与配置组迁企业增值面 internal/enterprise/oidc,组合根直呼解析,
	// api.Config 不再携带):BRAID_OIDC_* 组半开启动即败;配置完整才构造
	// ——discovery 启动期拉取,issuer 错/不可达即启动失败。未配置零行为
	//(路由不挂载),内置 login 不受影响;主体映射 = 本地账号锚定(claim
	// 值 == 本地账号名,锚定缝 auth.AnchorLoginUser)。
	oidcCfg, err := entoidc.ParseOIDCConfig(os.Getenv)
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}
	if oidcCfg.Configured() {
		oidcProv, err := entoidc.NewOIDCProvider(ctx, oidcCfg, dbh)
		if err != nil {
			return fmt.Errorf("api: %w", err)
		}
		apiSrv.OIDCProvider(oidcProv)
		slog.Info("api: oidc login enabled", "issuer", oidcCfg.Issuer, "claim", oidcCfg.Claim)
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
	case lerr := <-lease.Lost():
		// 续期失败:停发闸已先置位(Guard);先有界排空在飞请求再响亮退出
		//(镜像 ctx.Done 分支的优雅形态,review LOW),复航 = 重启重新
		// acquire(非 context.Canceled,main 走 Exit(1) 失败重启通道)。
		shCtx, lcancel := context.WithTimeout(context.Background(), shutdownGrace)
		if serr := srv.Shutdown(shCtx); serr != nil {
			slog.Warn("api: lost-path http drain", "err", serr)
		}
		lcancel()
		return fmt.Errorf("api: %w", lerr)
	case <-ctx.Done():
		shCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		return srv.Shutdown(shCtx)
	}
}

// releaseLease 是 SIGTERM 语义半边(决策 2:优雅停机必须 DELETE 释放):
// 排空完成后删本行;lost 路径调用安全(行非本持,DELETE 0 行幂等)。
// 独立 Background ctx——根 ctx 在停机路径已取消。
func releaseLease(component string, lease *snowflake.Lease) {
	rctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := lease.Release(rctx); err != nil {
		slog.Warn(component+": snowflake lease release", "err", err)
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
