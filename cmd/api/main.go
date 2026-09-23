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

	_ "github.com/go-sql-driver/mysql"

	"braid/internal/api"
	"braid/internal/db"
	"braid/internal/snowflake"
)

// readHeaderTimeout 封慢速头部攻击(slowloris)的硬上限;值配置化归 T7.1。
const readHeaderTimeout = 10 * time.Second

// 读写/空闲超时:ReadHeaderTimeout 只封头部相,1MiB DSL 请求体与空闲
// keep-alive 连接仍无界——公开面(BRAID_API_ADDR 显式放开)下是连接
// 耗尽向量。固定 JSON API,整请求 60s 足够宽。值配置化归 T7.1。
const (
	readTimeout  = 60 * time.Second
	writeTimeout = 30 * time.Second
	idleTimeout  = 120 * time.Second
)

// shutdownGrace 是优雅停机窗:在途请求排空、新请求已由 Shutdown 拒绝。
const shutdownGrace = 10 * time.Second

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
	dsn := os.Getenv("BRAID_MYSQL_DSN")
	if dsn == "" {
		return errors.New("api: BRAID_MYSQL_DSN is required")
	}
	dbh, err := sql.Open("mysql", db.CanonicalDSN(dsn))
	if err != nil {
		return fmt.Errorf("api: open mysql: %w", err)
	}
	defer dbh.Close() //nolint:errcheck // 主路径退出,关闭错误无处置义
	if err := dbh.PingContext(ctx); err != nil {
		return fmt.Errorf("api: mysql ping: %w", err)
	}
	ids, err := snowflake.New(cfg.SnowflakeNode)
	if err != nil {
		return fmt.Errorf("api: snowflake: %w", err)
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.NewServer(cfg, dbh, ids).Router(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
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
