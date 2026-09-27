package main

// 启动即败路径锚(T7.1 cmd 直读 env 收编后,DSN 必填断言的新位置读
// cfg.MySQLDSN):t.Setenv 注入,不触网不触 DB——断言在 sql.Open 前返回。

import (
	"context"
	"net/http"
	"testing"
	"time"

	"braid/internal/api"
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

// stubHandler 是可比较的 http.Handler 桩(接口相等断言需要非 func 底层类型)。
type stubHandler struct{}

func (stubHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}

// TestBuildHTTPServer(#67-②):cfg 四超时字段 → http.Server 四字段装配锚。
// 表两行:默认口径(DefaultConfig 四常量)与显式覆盖口径(env 注入后的
// 任意合法值);Addr/Handler 同传同接一并锚定。
func TestBuildHTTPServer(t *testing.T) {
	overrides := api.DefaultConfig()
	overrides.Addr = "127.0.0.1:18080"
	overrides.ReadHeaderTimeout = 1 * time.Second
	overrides.ReadTimeout = 2 * time.Second
	overrides.WriteTimeout = 3 * time.Second
	overrides.IdleTimeout = 4 * time.Second
	cases := []struct {
		name string
		cfg  api.Config
	}{
		{"defaults", api.DefaultConfig()},
		{"env overrides", overrides},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := stubHandler{}
			srv := buildHTTPServer(tc.cfg, h)
			if srv.Addr != tc.cfg.Addr {
				t.Errorf("Addr = %q, want %q", srv.Addr, tc.cfg.Addr)
			}
			if srv.Handler != h {
				t.Error("Handler not the passed handler")
			}
			if srv.ReadHeaderTimeout != tc.cfg.ReadHeaderTimeout {
				t.Errorf("ReadHeaderTimeout = %v, want %v", srv.ReadHeaderTimeout, tc.cfg.ReadHeaderTimeout)
			}
			if srv.ReadTimeout != tc.cfg.ReadTimeout {
				t.Errorf("ReadTimeout = %v, want %v", srv.ReadTimeout, tc.cfg.ReadTimeout)
			}
			if srv.WriteTimeout != tc.cfg.WriteTimeout {
				t.Errorf("WriteTimeout = %v, want %v", srv.WriteTimeout, tc.cfg.WriteTimeout)
			}
			if srv.IdleTimeout != tc.cfg.IdleTimeout {
				t.Errorf("IdleTimeout = %v, want %v", srv.IdleTimeout, tc.cfg.IdleTimeout)
			}
		})
	}
}
