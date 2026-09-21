package main

// 启动即败路径(t.Setenv 注入;不依赖验证栈——坏 DSN 在 Ping 前即败或
// Ping 报错,两条路径都必须响亮返回错误而非挂起)。

import (
	"context"
	"testing"
)

func TestRunRequiresDSN(t *testing.T) {
	t.Setenv("BRAID_MYSQL_DSN", "")
	if err := run(context.Background()); err == nil {
		t.Fatalf("missing DSN accepted")
	}
}

func TestRunRejectsBadDSNAndMissingRedis(t *testing.T) {
	t.Setenv("BRAID_MYSQL_DSN", "invalid-dsn")
	t.Setenv("BRAID_REDIS_QUEUE_ADDR", "")
	if err := run(context.Background()); err == nil {
		t.Fatalf("bad DSN / missing redis addr accepted")
	}
}
