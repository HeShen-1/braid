package api

import (
	"strings"
	"testing"
	"time"

	"braid/internal/db"
)

// testSecret 满足 auth.MinSecretLen 的低熵占位词组(纯词组无 hex/熵段,
// auth fixtures_test 同款形态——带数字串的值会触发 gitleaks
// generic-api-key 熵规则;测试密钥不进任何真实面)。
const testSecret = "t54-api-test-jwt-signing-secret-words-only-placeholder"

func TestConfigFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr string
	}{
		{
			name:    "secret missing fails at startup",
			env:     map[string]string{},
			wantErr: "jwt secret shorter than",
		},
		{
			name: "secret too short fails",
			env:  map[string]string{"BRAID_AUTH_JWT_SECRET": "short"},
			// 长度 < MinSecretLen 的判定在 auth.Validate 与本包一致
			wantErr: "jwt secret shorter than",
		},
		{
			name: "defaults with secret",
			env:  map[string]string{"BRAID_AUTH_JWT_SECRET": testSecret},
			want: Config{Addr: DefaultAddr, JWTSecret: testSecret, SnowflakeNode: 1},
		},
		{
			name: "full env",
			env: map[string]string{
				"BRAID_AUTH_JWT_SECRET":    testSecret,
				"BRAID_API_ADDR":           "10.0.0.1:9000",
				"BRAID_API_SNOWFLAKE_NODE": "7",
			},
			want: Config{
				Addr:          "10.0.0.1:9000",
				JWTSecret:     testSecret,
				SnowflakeNode: 7,
			},
		},
		{
			name: "snowflake node overflow rejected",
			env: map[string]string{
				"BRAID_AUTH_JWT_SECRET":    testSecret,
				"BRAID_API_SNOWFLAKE_NODE": "1024",
			},
			wantErr: "want 0..1023",
		},
		{
			name: "snowflake node negative rejected",
			env: map[string]string{
				"BRAID_AUTH_JWT_SECRET":    testSecret,
				"BRAID_API_SNOWFLAKE_NODE": "-1",
			},
			wantErr: "want 0..1023",
		},
		{
			// uint16 截断带:65536 mod 2^16 = 0,若在转换后才校验会静默
			// 变成合法节点号 0(审查发现 #2 的回归锚)。
			name: "snowflake node uint16 truncation band rejected",
			env: map[string]string{
				"BRAID_AUTH_JWT_SECRET":    testSecret,
				"BRAID_API_SNOWFLAKE_NODE": "65536",
			},
			wantErr: "want 0..1023",
		},
		{
			name: "bad addr rejected",
			env: map[string]string{
				"BRAID_AUTH_JWT_SECRET": testSecret,
				"BRAID_API_ADDR":        "no-port-here",
			},
			wantErr: "addr",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ConfigFromEnv(func(k string) string { return tt.env[k] })
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ConfigFromEnv() err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ConfigFromEnv() unexpected err: %v", err)
			}
			if got.Addr != tt.want.Addr || got.JWTSecret != tt.want.JWTSecret ||
				got.SnowflakeNode != tt.want.SnowflakeNode {
				t.Fatalf("ConfigFromEnv() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestConfigValidateSecretBoundary 钉死密钥下界与 auth.MinSecretLen 一致:
// 32 字节恰好通过,31 字节拒绝。
func TestConfigValidateSecretBoundary(t *testing.T) {
	c := DefaultConfig()
	c.JWTSecret = strings.Repeat("x", 31)
	if err := c.Validate(); err == nil {
		t.Fatal("31-byte secret accepted, want rejection")
	}
	c.JWTSecret = strings.Repeat("x", 32)
	if err := c.Validate(); err != nil {
		t.Fatalf("32-byte secret rejected: %v", err)
	}
}

// TestConfigFromEnvPool:池三值经 api 换装点的三态(缺省/覆盖/坏值启动即败;
// T7.1,D-t63 移交回收)。施加行为锚在 internal/db/pool_test.go(黑盒)。
func TestConfigFromEnvPool(t *testing.T) {
	t.Run("empty env yields default pool", func(t *testing.T) {
		env := map[string]string{"BRAID_AUTH_JWT_SECRET": testSecret}
		c, err := ConfigFromEnv(func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if c.Pool != db.DefaultPoolConfig() {
			t.Fatalf("pool = %+v, want defaults %+v", c.Pool, db.DefaultPoolConfig())
		}
	})
	t.Run("override", func(t *testing.T) {
		env := map[string]string{
			"BRAID_AUTH_JWT_SECRET":      testSecret,
			"BRAID_DB_MAX_OPEN":          "10",
			"BRAID_DB_MAX_IDLE":          "4",
			"BRAID_DB_CONN_MAX_LIFETIME": "90s",
		}
		c, err := ConfigFromEnv(func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		want := db.PoolConfig{MaxOpen: 10, MaxIdle: 4, ConnMaxLifetime: 90 * time.Second}
		if c.Pool != want {
			t.Fatalf("pool = %+v, want %+v", c.Pool, want)
		}
	})
	t.Run("bad value fails at startup", func(t *testing.T) {
		env := map[string]string{
			"BRAID_AUTH_JWT_SECRET": testSecret,
			"BRAID_DB_MAX_OPEN":     "ten",
		}
		_, err := ConfigFromEnv(func(k string) string { return env[k] })
		if err == nil || !strings.Contains(err.Error(), "BRAID_DB_MAX_OPEN") {
			t.Fatalf("err = %v, want containing BRAID_DB_MAX_OPEN", err)
		}
	})
}

// TestConfigFromEnvDSN:DSN 收编进 Config(T7.1;必填断言留 cmd 读 cfg
// 字段——装配输入非旋钮,ConfigFromEnv 保持纯解析)。
func TestConfigFromEnvDSN(t *testing.T) {
	env := map[string]string{
		"BRAID_AUTH_JWT_SECRET": testSecret,
		"BRAID_MYSQL_DSN":       "u:p@tcp(127.0.0.1:3306)/a",
	}
	c, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if c.MySQLDSN != env["BRAID_MYSQL_DSN"] {
		t.Fatalf("dsn = %q, want %q", c.MySQLDSN, env["BRAID_MYSQL_DSN"])
	}
}

// TestConfigFromEnvTimeouts:http.Server 四超时经 api 换装点的三态(缺省
// 逐值钉死 cmd 期常量——慢速攻击面口径不得随收编漂移;覆盖;坏值)。
func TestConfigFromEnvTimeouts(t *testing.T) {
	base := map[string]string{"BRAID_AUTH_JWT_SECRET": testSecret}
	t.Run("empty env pins cmd-era constants", func(t *testing.T) {
		c, err := ConfigFromEnv(func(k string) string { return base[k] })
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if c.ReadHeaderTimeout != 10*time.Second || c.ReadTimeout != 60*time.Second ||
			c.WriteTimeout != 30*time.Second || c.IdleTimeout != 120*time.Second {
			t.Fatalf("timeouts = %s/%s/%s/%s, want 10s/60s/30s/120s",
				c.ReadHeaderTimeout, c.ReadTimeout, c.WriteTimeout, c.IdleTimeout)
		}
	})
	t.Run("override all four", func(t *testing.T) {
		env := map[string]string{
			"BRAID_AUTH_JWT_SECRET":         testSecret,
			"BRAID_API_READ_HEADER_TIMEOUT": "3s",
			"BRAID_API_READ_TIMEOUT":        "45s",
			"BRAID_API_WRITE_TIMEOUT":       "20s",
			"BRAID_API_IDLE_TIMEOUT":        "90s",
		}
		c, err := ConfigFromEnv(func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if c.ReadHeaderTimeout != 3*time.Second || c.ReadTimeout != 45*time.Second ||
			c.WriteTimeout != 20*time.Second || c.IdleTimeout != 90*time.Second {
			t.Fatalf("timeouts = %s/%s/%s/%s, want 3s/45s/20s/90s",
				c.ReadHeaderTimeout, c.ReadTimeout, c.WriteTimeout, c.IdleTimeout)
		}
	})
	for key, val := range map[string]string{
		"BRAID_API_READ_HEADER_TIMEOUT": "0s",
		"BRAID_API_READ_TIMEOUT":        "-5s",
		"BRAID_API_WRITE_TIMEOUT":       "forever",
		"BRAID_API_IDLE_TIMEOUT":        "0s",
	} {
		t.Run(key+" bad rejected", func(t *testing.T) {
			env := map[string]string{"BRAID_AUTH_JWT_SECRET": testSecret, key: val}
			if _, err := ConfigFromEnv(func(k string) string { return env[k] }); err == nil {
				t.Fatalf("%s=%s accepted, want reject", key, val)
			}
		})
	}
}
