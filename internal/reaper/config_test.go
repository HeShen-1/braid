package reaper

// 用例 1(config):默认值 / Validate 边界 / 跨进程 grace 硬校验三态。
// 变异自查:硬校验丢了 error 返回 → cross_fail 红;下限丢了 → validate 红。

import (
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if c.ScanInterval != 30*time.Second || c.DeadGrace != 45*time.Second {
		t.Fatalf("defaults = %+v, want scan 30s / grace 45s", c)
	}
	if c.AuditRetention != 2160*time.Hour {
		t.Fatalf("retention = %s, want 90d", c.AuditRetention)
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
		{"audit_flush_batch_over", func(c *Config) { c.AuditFlushBatch = 3101 }, false},
		{"janitor_floor", func(c *Config) { c.JanitorInterval = 4 * time.Minute }, false},
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
}
