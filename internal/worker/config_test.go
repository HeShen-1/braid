package worker

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"braid/internal/db"
)

func TestConfigValidate(t *testing.T) {
	valid := DefaultConfig()
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"default", func(*Config) {}, false},
		{"concurrency zero", func(c *Config) { c.Concurrency = 0 }, true},
		{"concurrency over cap", func(c *Config) { c.Concurrency = 257 }, true},
		{"no queues", func(c *Config) { c.Queues = nil }, true},
		{"queue weight zero", func(c *Config) { c.Queues = map[string]int{"default": 0} }, true},
		{"queue name charset", func(c *Config) { c.Queues = map[string]int{"Bad!": 1} }, true},
		{"queue name length", func(c *Config) {
			c.Queues = map[string]int{"a12345678901234567890123456789012345678901234567890123456789012345": 1}
		}, true},
		{"worker id length", func(c *Config) { c.WorkerID = string(make([]byte, 65)) }, true},
		{"heartbeat too fast", func(c *Config) { c.HeartbeatInterval = time.Millisecond }, true},
		{"retry delay too fast", func(c *Config) { c.RetryDelay = time.Millisecond }, true},
		{"shutdown zero", func(c *Config) { c.ShutdownTimeout = 0 }, true},
		{"multi queue valid", func(c *Config) {
			c.Queues = map[string]int{"default": 1, "approval": 5}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid
			tt.mutate(&c)
			err := c.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestConfigFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr bool
	}{
		{
			name: "empty env returns defaults",
			env:  nil,
			want: DefaultConfig(),
		},
		{
			name: "full override",
			env: map[string]string{
				"BRAID_WORKER_CONCURRENCY":        "4",
				"BRAID_WORKER_QUEUES":             "default:1,approval:5",
				"BRAID_WORKER_ID":                 "worker-a",
				"BRAID_WORKER_HEARTBEAT_INTERVAL": "2s",
				"BRAID_WORKER_RETRY_DELAY":        "500ms",
				"BRAID_WORKER_SHUTDOWN_TIMEOUT":   "10s",
				"BRAID_WORKER_SNOWFLAKE_NODE":     "3",
			},
			want: Config{
				Concurrency:       4,
				Queues:            map[string]int{"default": 1, "approval": 5},
				WorkerID:          "worker-a",
				HeartbeatInterval: 2 * time.Second,
				RetryDelay:        500 * time.Millisecond,
				ShutdownTimeout:   10 * time.Second,
				SnowflakeNode:     3,
				Pool:              db.DefaultPoolConfig(),
			},
		},
		{
			name:    "snowflake node over max",
			env:     map[string]string{"BRAID_WORKER_SNOWFLAKE_NODE": "1024"},
			wantErr: true,
		},
		{
			name:    "bad duration",
			env:     map[string]string{"BRAID_WORKER_RETRY_DELAY": "fast"},
			wantErr: true,
		},
		{
			name:    "bad int",
			env:     map[string]string{"BRAID_WORKER_CONCURRENCY": "many"},
			wantErr: true,
		},
		{
			name:    "queues entry without weight",
			env:     map[string]string{"BRAID_WORKER_QUEUES": "default"},
			wantErr: true,
		},
		{
			name:    "queues entry with bad weight",
			env:     map[string]string{"BRAID_WORKER_QUEUES": "default:high"},
			wantErr: true,
		},
		{
			name:    "validate catches propagated invalidity",
			env:     map[string]string{"BRAID_WORKER_CONCURRENCY": "0"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(k string) string { return tt.env[k] }
			got, err := ConfigFromEnv(getenv)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ConfigFromEnv() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ConfigFromEnv() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestConfigFromEnvPool:池三值经 worker 换装点的三态(缺省/覆盖/坏值;
// T7.1,D-t63 移交回收)。施加行为锚在 internal/db/pool_test.go(黑盒)。
func TestConfigFromEnvPool(t *testing.T) {
	t.Run("empty env yields default pool", func(t *testing.T) {
		c, err := ConfigFromEnv(func(string) string { return "" })
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if c.Pool != db.DefaultPoolConfig() {
			t.Fatalf("pool = %+v, want defaults %+v", c.Pool, db.DefaultPoolConfig())
		}
	})
	t.Run("override", func(t *testing.T) {
		env := map[string]string{
			"BRAID_DB_MAX_OPEN":          "12",
			"BRAID_DB_MAX_IDLE":          "6",
			"BRAID_DB_CONN_MAX_LIFETIME": "2m",
		}
		c, err := ConfigFromEnv(func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		want := db.PoolConfig{MaxOpen: 12, MaxIdle: 6, ConnMaxLifetime: 2 * time.Minute}
		if c.Pool != want {
			t.Fatalf("pool = %+v, want %+v", c.Pool, want)
		}
	})
	t.Run("bad value fails at startup", func(t *testing.T) {
		env := map[string]string{"BRAID_DB_MAX_IDLE": "idle"}
		_, err := ConfigFromEnv(func(k string) string { return env[k] })
		if err == nil || !strings.Contains(err.Error(), "BRAID_DB_MAX_IDLE") {
			t.Fatalf("err = %v, want containing BRAID_DB_MAX_IDLE", err)
		}
	})
}

// TestConfigFromEnvInfra:装配输入收编(T7.1)——DSN/Redis 队列/指标地址
// 进 Config(cmd 直读清零,必填断言留 cmd 读 cfg 字段);ToolTokenSecret
// hex 解码(缺省 nil = 进程内随机,原 toolTokenKey 语义)。
func TestConfigFromEnvInfra(t *testing.T) {
	t.Run("infra fields parsed", func(t *testing.T) {
		env := map[string]string{
			"BRAID_MYSQL_DSN":        "u:p@tcp(127.0.0.1:3306)/w",
			"BRAID_REDIS_QUEUE_ADDR": "127.0.0.1:6379",
			"BRAID_METRICS_ADDR":     "127.0.0.1:9101",
		}
		c, err := ConfigFromEnv(func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if c.MySQLDSN != env["BRAID_MYSQL_DSN"] || c.RedisQueueAddr != "127.0.0.1:6379" ||
			c.MetricsAddr != "127.0.0.1:9101" {
			t.Fatalf("infra = %q/%q/%q", c.MySQLDSN, c.RedisQueueAddr, c.MetricsAddr)
		}
		if c.ToolTokenSecret != nil {
			t.Fatalf("tool token secret = %v, want nil (unset)", c.ToolTokenSecret)
		}
	})
	t.Run("tool token secret empty yields nil", func(t *testing.T) {
		c, err := ConfigFromEnv(func(string) string { return "" })
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if c.ToolTokenSecret != nil {
			t.Fatalf("tool token secret = %v, want nil", c.ToolTokenSecret)
		}
	})
	t.Run("tool token secret decoded", func(t *testing.T) {
		c, err := ConfigFromEnv(func(k string) string {
			if k == "BRAID_TOOL_TOKEN_SECRET" {
				return "0a0b"
			}
			return ""
		})
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if string(c.ToolTokenSecret) != "\x0a\x0b" {
			t.Fatalf("tool token secret = %x, want 0a0b", c.ToolTokenSecret)
		}
	})
	t.Run("tool token secret bad hex rejected", func(t *testing.T) {
		_, err := ConfigFromEnv(func(k string) string {
			if k == "BRAID_TOOL_TOKEN_SECRET" {
				return "zz"
			}
			return ""
		})
		if err == nil || !strings.Contains(err.Error(), "BRAID_TOOL_TOKEN_SECRET") {
			t.Fatalf("err = %v, want containing BRAID_TOOL_TOKEN_SECRET", err)
		}
	})
}
