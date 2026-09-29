package reaper

// 用例 1(config):默认值 / Validate 边界 / 跨进程 grace 硬校验三态。
// 变异自查:硬校验丢了 error 返回 → cross_fail 红;下限丢了 → validate 红。

import (
	"os"
	"strings"
	"testing"
	"time"

	"braid/internal/db"
	"braid/internal/snowflake"
)

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if c.ScanInterval != 30*time.Second || c.DeadGrace != 45*time.Second {
		t.Fatalf("defaults = %+v, want scan 30s / grace 45s", c)
	}
	if c.AuditRetention != 2160*time.Hour {
		t.Fatalf("retention = %s, want 90d", c.AuditRetention)
	}
	if c.LoginAuditRetention != 2160*time.Hour {
		t.Fatalf("login audit retention = %s, want 90d(镜像 AuditRetention)", c.LoginAuditRetention)
	}
	if c.AuditFlushInterval != time.Second || c.AuditFlushBatch != 500 {
		t.Fatalf("audit flush defaults = %s/%d, want 1s/500", c.AuditFlushInterval, c.AuditFlushBatch)
	}
}

func TestConfigValidateBounds(t *testing.T) {
	base := func() Config {
		c := DefaultConfig()
		c.ID = "h"
		return c
	}
	tests := []struct {
		name string
		mut  func(*Config)
		ok   bool
	}{
		{"base", func(*Config) {}, true},
		{"scan_too_fast", func(c *Config) { c.ScanInterval = 500 * time.Millisecond }, false},
		{"grace_floor", func(c *Config) { c.DeadGrace = 19 * time.Second }, false},
		{"grace_ok", func(c *Config) { c.DeadGrace = 20 * time.Second }, true},
		{"exec_stale_below_scan", func(c *Config) { c.ExecStale = c.ScanInterval - time.Second }, false},
		{"pending_ttl_floor", func(c *Config) { c.PendingTTL = 14 * time.Minute }, false},
		{"dispatch_stale_floor", func(c *Config) { c.DispatchStale = 9 * time.Minute }, false},
		{"retention_floor", func(c *Config) { c.AuditRetention = 23 * time.Hour }, false},
		{"audit_flush_interval_floor", func(c *Config) { c.AuditFlushInterval = 99 * time.Millisecond }, false},
		{"audit_flush_interval_ok", func(c *Config) { c.AuditFlushInterval = 100 * time.Millisecond }, true},
		{"audit_flush_batch_zero", func(c *Config) { c.AuditFlushBatch = 0 }, false},
		{"audit_flush_batch_over", func(c *Config) { c.AuditFlushBatch = 2801 }, false},
		{"audit_flush_batch_edge", func(c *Config) { c.AuditFlushBatch = 2800 }, true}, // T9.3:23 列×2800=64400<65535 占位符预算新上界
		{"janitor_floor", func(c *Config) { c.JanitorInterval = 4 * time.Minute }, false},
		{"login_retention_floor", func(c *Config) { c.LoginAuditRetention = 23 * time.Hour }, false},
		{"login_retention_ok", func(c *Config) { c.LoginAuditRetention = 24 * time.Hour }, true},
		{"outbox_batch_zero", func(c *Config) { c.OutboxBatch = 0 }, false},
		{"outbox_batch_over", func(c *Config) { c.OutboxBatch = 10001 }, false},
		{"queue_bad_charset", func(c *Config) { c.Queue = "Q1" }, false},
		{"id_empty", func(c *Config) { c.ID = "" }, false},
		{"id_too_long", func(c *Config) { c.ID = string(make([]byte, 58)) }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.mut(&c)
			err := c.Validate()
			if tt.ok && err != nil {
				t.Fatalf("Validate: %v(want nil)", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("Validate 应拒绝")
			}
		})
	}
}

func TestCrossValidateWorkerTimings(t *testing.T) {
	env := func(kv map[string]string) func(string) string {
		return func(k string) string { return kv[k] }
	}
	t.Run("both_set_grace_within_staleness_fails", func(t *testing.T) {
		err := crossValidateWorkerTimings(env(map[string]string{
			"BRAID_WORKER_RETRY_DELAY": "5s", "BRAID_WORKER_HEARTBEAT_INTERVAL": "10s",
		}), 15*time.Second)
		if err == nil {
			t.Fatal("grace ≤ retry+hb 应启动即败")
		}
	})
	t.Run("both_set_healthy_grace_ok", func(t *testing.T) {
		if err := crossValidateWorkerTimings(env(map[string]string{
			"BRAID_WORKER_RETRY_DELAY": "5s", "BRAID_WORKER_HEARTBEAT_INTERVAL": "10s",
		}), 45*time.Second); err != nil {
			t.Fatalf("健康 grace 应通过: %v", err)
		}
	})
	t.Run("unset_degrades_silent", func(t *testing.T) {
		if err := crossValidateWorkerTimings(env(nil), 20*time.Second); err != nil {
			t.Fatalf("不可读应退化(不阻断): %v", err)
		}
	})
}

func TestConfigFromEnvParses(t *testing.T) {
	c, err := ConfigFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatalf("empty env: %v", err)
	}
	if c.Queue != "default" || c.ID == "" {
		t.Fatalf("defaults = %q/%q", c.Queue, c.ID)
	}
	if _, err := ConfigFromEnv(func(k string) string {
		if k == "BRAID_REAPER_SCAN_INTERVAL" {
			return "bogus"
		}
		return ""
	}); err == nil {
		t.Fatal("坏值应报错")
	}
	// T6.1 新旋钮:flush 间隔/batch 可经 env 覆盖。
	c2, err := ConfigFromEnv(func(k string) string {
		switch k {
		case "BRAID_REAPER_AUDIT_FLUSH_INTERVAL":
			return "5s"
		case "BRAID_REAPER_AUDIT_FLUSH_BATCH":
			return "250"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("flush env: %v", err)
	}
	if c2.AuditFlushInterval != 5*time.Second || c2.AuditFlushBatch != 250 {
		t.Fatalf("flush env = %s/%d, want 5s/250", c2.AuditFlushInterval, c2.AuditFlushBatch)
	}
	c3, err := ConfigFromEnv(func(k string) string {
		if k == "BRAID_REAPER_LOGIN_AUDIT_RETENTION" {
			return "48h"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("login retention env: %v", err)
	}
	if c3.LoginAuditRetention != 48*time.Hour {
		t.Fatalf("login retention env = %s, want 48h", c3.LoginAuditRetention)
	}
}

// TestConfigFromEnvPool:池三值经 reaper 换装点的三态(缺省/覆盖/坏值;
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
			"BRAID_DB_MAX_OPEN":          "6",
			"BRAID_DB_MAX_IDLE":          "3",
			"BRAID_DB_CONN_MAX_LIFETIME": "4m",
		}
		c, err := ConfigFromEnv(func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		want := db.PoolConfig{MaxOpen: 6, MaxIdle: 3, ConnMaxLifetime: 4 * time.Minute}
		if c.Pool != want {
			t.Fatalf("pool = %+v, want %+v", c.Pool, want)
		}
	})
	t.Run("bad value fails at startup", func(t *testing.T) {
		env := map[string]string{"BRAID_DB_MAX_OPEN": "many"}
		_, err := ConfigFromEnv(func(k string) string { return env[k] })
		if err == nil || !strings.Contains(err.Error(), "BRAID_DB_MAX_OPEN") {
			t.Fatalf("err = %v, want containing BRAID_DB_MAX_OPEN", err)
		}
	})
}

// TestConfigFromEnvInfra:装配输入收编(T7.1)——DSN/Redis 队列(可选 =
// leaderless)/指标地址 + 雪花节点显式化(缺省 PID 派生,原 cmd 逻辑)。
func TestConfigFromEnvInfra(t *testing.T) {
	env := map[string]string{
		"BRAID_MYSQL_DSN":        "u:p@tcp(127.0.0.1:3306)/p",
		"BRAID_REDIS_QUEUE_ADDR": "127.0.0.1:6379",
		"BRAID_METRICS_ADDR":     "127.0.0.1:9103",
	}
	c, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if c.MySQLDSN != env["BRAID_MYSQL_DSN"] || c.RedisQueueAddr != "127.0.0.1:6379" ||
		c.MetricsAddr != "127.0.0.1:9103" {
		t.Fatalf("infra = %q/%q/%q", c.MySQLDSN, c.RedisQueueAddr, c.MetricsAddr)
	}
	t.Run("snowflake unset derives pid", func(t *testing.T) {
		c, err := ConfigFromEnv(func(string) string { return "" })
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if want := uint16(os.Getpid() & snowflake.MaxNodeID); c.SnowflakeNode != want {
			t.Fatalf("snowflake node = %d, want pid-derived %d", c.SnowflakeNode, want)
		}
	})
	t.Run("snowflake explicit", func(t *testing.T) {
		c, err := ConfigFromEnv(func(k string) string {
			if k == "BRAID_REAPER_SNOWFLAKE_NODE" {
				return "5"
			}
			return ""
		})
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if c.SnowflakeNode != 5 {
			t.Fatalf("snowflake node = %d, want 5", c.SnowflakeNode)
		}
	})
	for raw := range map[string]bool{"1024": true, "65536": true, "-1": true, "x": true} {
		t.Run("snowflake rejects "+raw, func(t *testing.T) {
			_, err := ConfigFromEnv(func(k string) string {
				if k == "BRAID_REAPER_SNOWFLAKE_NODE" {
					return raw
				}
				return ""
			})
			if err == nil || !strings.Contains(err.Error(), "BRAID_REAPER_SNOWFLAKE_NODE") {
				t.Fatalf("err = %v, want containing BRAID_REAPER_SNOWFLAKE_NODE", err)
			}
		})
	}
}
