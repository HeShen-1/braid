package main

// 启动即败路径锚(T7.1 cmd 直读 env 收编后,DSN 必填断言的新位置读
// cfg.MySQLDSN):t.Setenv 注入,不触网不触 DB——断言在 sql.Open 前返回。

import (
	"context"
	"testing"
)

func TestRunRequiresDSN(t *testing.T) {
	t.Setenv("BRAID_MYSQL_DSN", "")
	// 密钥给足长度:ConfigFromEnv 的 JWT 校验先于 DSN 断言,缺密钥的错误
	// 会掩盖 DSN 断言位(词组形态,同 internal/api testSecret 口径)。
	t.Setenv("BRAID_AUTH_JWT_SECRET", "api-run-test-jwt-signing-secret-words-only-placeholder")
	if err := run(context.Background()); err == nil {
		t.Fatalf("missing DSN accepted")
	}
}
