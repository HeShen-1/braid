package main

// 启动即败路径锚(T7.1 cmd 直读 env 收编后,DSN 必填断言的新位置读
// cfg.MySQLDSN):不触网不触 DB——断言在 sql.Open 前返回。

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
