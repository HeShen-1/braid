package relay

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"braid/internal/db"
)

func TestDefaultConfigValid(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
}

// TestConfigFromEnv:解析、默认回填与错误表驱动。
func TestConfigFromEnv(t *testing.T) {
	t.Run("empty env yields defaults", func(t *testing.T) {
		c, err := ConfigFromEnv(func(string) string { return "" })
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if c != DefaultConfig() {
			t.Fatalf("want defaults, got %+v", c)
		}
	})
	t.Run("parses all keys", func(t *testing.T) {
		env := map[string]string{
			"BRAID_RELAY_IDLE_INTERVAL":   "250ms",
			"BRAID_RELAY_ENQUEUE_TIMEOUT": "2s",
			"BRAID_RELAY_MAX_ATTEMPTS":    "3",
			"BRAID_RELAY_BACKOFF_BASE":    "1s",
			"BRAID_RELAY_BACKOFF_FACTOR":  "2.5",
			"BRAID_RELAY_BACKOFF_MAX":     "30s",
		}
		c, err := ConfigFromEnv(func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		want := Config{
			IdleInterval:   250 * time.Millisecond,
			EnqueueTimeout: 2 * time.Second,
			MaxAttempts:    3,
			BackoffBase:    time.Second,
			BackoffFactor:  2.5,
			BackoffMax:     30 * time.Second,
			Jitter:         0.2,
			Pool:           db.DefaultPoolConfig(),
		}
		if c != want {
			t.Fatalf("got %+v want %+v", c, want)
		}
	})
	t.Run("rejects bad values", func(t *testing.T) {
		for _, tc := range []struct{ key, val string }{
			{"BRAID_RELAY_IDLE_INTERVAL", "notaduration"},
			{"BRAID_RELAY_IDLE_INTERVAL", "1ms"}, // < 下限(防全速轮询打 MySQL)
			{"BRAID_RELAY_ENQUEUE_TIMEOUT", "0s"},
			{"BRAID_RELAY_MAX_ATTEMPTS", "0"},
			{"BRAID_RELAY_MAX_ATTEMPTS", "70000"}, // > SMALLINT UNSIGNED 域
			{"BRAID_RELAY_BACKOFF_BASE", "-1s"},
			{"BRAID_RELAY_BACKOFF_FACTOR", "0.5"},
			{"BRAID_RELAY_BACKOFF_FACTOR", "NaN"},
			{"BRAID_RELAY_BACKOFF_FACTOR", "inf"},
			{"BRAID_RELAY_BACKOFF_MAX", "1ms"}, // < base(默认 5s)
		} {
			if _, err := ConfigFromEnv(func(k string) string {
				if k == tc.key {
					return tc.val
				}
				return ""
			}); err == nil {
				t.Fatalf("%s=%s accepted, want reject", tc.key, tc.val)
			}
		}
	})
}

// TestConfigFromEnvPool:池三值经 relay 换装点的三态(缺省/覆盖/坏值;
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
			"BRAID_DB_MAX_OPEN":          "8",
			"BRAID_DB_MAX_IDLE":          "2",
			"BRAID_DB_CONN_MAX_LIFETIME": "3m",
		}
		c, err := ConfigFromEnv(func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		want := db.PoolConfig{MaxOpen: 8, MaxIdle: 2, ConnMaxLifetime: 3 * time.Minute}
		if c.Pool != want {
			t.Fatalf("pool = %+v, want %+v", c.Pool, want)
		}
	})
	t.Run("bad value fails at startup", func(t *testing.T) {
		env := map[string]string{"BRAID_DB_CONN_MAX_LIFETIME": "forever"}
		_, err := ConfigFromEnv(func(k string) string { return env[k] })
		if err == nil || !strings.Contains(err.Error(), "BRAID_DB_CONN_MAX_LIFETIME") {
			t.Fatalf("err = %v, want containing BRAID_DB_CONN_MAX_LIFETIME", err)
		}
	})
}

// TestConfigFromEnvInfra:装配输入收编(T7.1)——DSN/Redis 队列/指标地址
// 进 Config(cmd 直读清零;必填断言留 cmd 读 cfg 字段)。
func TestConfigFromEnvInfra(t *testing.T) {
	env := map[string]string{
		"BRAID_MYSQL_DSN":        "u:p@tcp(127.0.0.1:3306)/r",
		"BRAID_REDIS_QUEUE_ADDR": "127.0.0.1:6379",
		"BRAID_METRICS_ADDR":     "127.0.0.1:9102",
	}
	c, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if c.MySQLDSN != env["BRAID_MYSQL_DSN"] || c.RedisQueueAddr != "127.0.0.1:6379" ||
		c.MetricsAddr != "127.0.0.1:9102" {
		t.Fatalf("infra = %q/%q/%q", c.MySQLDSN, c.RedisQueueAddr, c.MetricsAddr)
	}
	t.Run("unset yields empty (optional at config layer)", func(t *testing.T) {
		c, err := ConfigFromEnv(func(string) string { return "" })
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if c.MySQLDSN != "" || c.RedisQueueAddr != "" || c.MetricsAddr != "" {
			t.Fatalf("unset infra = %q/%q/%q, want empty", c.MySQLDSN, c.RedisQueueAddr, c.MetricsAddr)
		}
	})
}

// TestBackoffSchedule:指数、封顶、抖动界。抖动段用确定性 rand 源多样本采样,
// 断言落在裸值 ±Jitter 内(runner 按值持 cfg,勿在构造后再改外层变量)。
func TestBackoffSchedule(t *testing.T) {
	bare := DefaultConfig()
	bare.Jitter = 0
	r := &runner{cfg: bare}
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{1, 5 * time.Second},
		{2, 20 * time.Second},
		{3, 80 * time.Second},
		{4, 5 * time.Minute}, // 320s > 5m cap ⇒ 截断
		{5, 5 * time.Minute},
		{6, 5 * time.Minute},
		{99, 5 * time.Minute},
	}
	for _, tc := range cases {
		if got := r.backoff(tc.attempts); got != tc.want {
			t.Fatalf("backoff(%d) = %s, want %s", tc.attempts, got, tc.want)
		}
	}

	jcfg := DefaultConfig()
	jr := &runner{cfg: jcfg, rnd: rand.New(rand.NewPCG(1, 1))}
	for _, tc := range cases {
		lo := time.Duration(float64(tc.want) * (1 - jcfg.Jitter) * 0.999)
		hi := time.Duration(float64(tc.want) * (1 + jcfg.Jitter) * 1.001)
		for sample := 0; sample < 50; sample++ {
			got := jr.backoff(tc.attempts)
			if got < lo || got > hi {
				t.Fatalf("jittered backoff(%d) = %s outside [%s, %s]", tc.attempts, got, lo, hi)
			}
		}
	}
}
