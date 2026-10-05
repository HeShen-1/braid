package llm

// 单测:config 四件套(relay/config_test.go 同款:getenv map 桩 + 拒绝表)。
// 零 DB 零网络。

import (
	"strings"
	"testing"
	"time"

	"braid/internal/scheduler"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

const minPriceTable = `{"m":{"input_per_mtok":"1","output_per_mtok":"1","currency":"USD"}}`

func TestConfigFromEnv(t *testing.T) {
	t.Run("defaults with required keys only", func(t *testing.T) {
		c, err := ConfigFromEnv(envMap(map[string]string{
			"BRAID_LLM_BASE_URL":    "https://api.example.com/v1",
			"BRAID_LLM_API_KEY":     "sk-test",
			"BRAID_LLM_PRICE_TABLE": minPriceTable,
		}))
		if err != nil {
			t.Fatalf("ConfigFromEnv() = %v, want nil", err)
		}
		if c.Timeout != 120*time.Second || c.MaxAttempts != 3 ||
			c.BackoffBase != time.Second || c.BackoffFactor != 2 || c.BackoffMax != 30*time.Second {
			t.Errorf("defaults drifted: %+v", c)
		}
		if c.PriceTable.Len() != 1 {
			t.Errorf("price table len = %d, want 1", c.PriceTable.Len())
		}
		if c.TokenBudget != 0 || c.TokenBudget.Enabled() {
			t.Errorf("token budget default = %d, want 0 (gate off, D-t182 决策 1)", c.TokenBudget)
		}
	})
	t.Run("token budget knob", func(t *testing.T) {
		base := map[string]string{
			"BRAID_LLM_BASE_URL":    "https://api.example.com/v1",
			"BRAID_LLM_API_KEY":     "sk-test",
			"BRAID_LLM_PRICE_TABLE": minPriceTable,
		}
		with := func(v string) map[string]string {
			m := map[string]string{}
			for k, val := range base {
				m[k] = val
			}
			m["BRAID_LLM_TOKEN_BUDGET"] = v
			return m
		}
		c, err := ConfigFromEnv(envMap(with("5000")))
		if err != nil {
			t.Fatalf("ConfigFromEnv() = %v, want nil", err)
		}
		if c.TokenBudget != 5000 || !c.TokenBudget.Enabled() {
			t.Fatalf("token budget = %d, want 5000 enabled", c.TokenBudget)
		}
		// 上界恰含(≤ 1e9 误配防御,D-t182 决策 5);超界错误归 Validate
		// (字段名口径,与 timeout/max_attempts 分工一致——env 键只在词法错)。
		if _, err := ConfigFromEnv(envMap(with("1000000001"))); err == nil ||
			!strings.Contains(err.Error(), "token budget") {
			t.Errorf("over 1e9: err = %v, want validate rejection naming token budget", err)
		}
		if _, err := ConfigFromEnv(envMap(with("1000000000"))); err != nil {
			t.Fatalf("boundary 1e9: ConfigFromEnv() = %v, want nil", err)
		}
		for name, raw := range map[string]string{
			"not integer": "1.5",
			"negative":    "-1",
			"garbage":     "fast",
		} {
			_, err := ConfigFromEnv(envMap(with(raw)))
			if err == nil {
				t.Errorf("%s (%q): ConfigFromEnv() = nil, want error", name, raw)
				continue
			}
			if !strings.Contains(err.Error(), "BRAID_LLM_TOKEN_BUDGET") {
				t.Errorf("%s (%q): err = %v, want env key in error", name, raw, err)
			}
		}
	})
	t.Run("overrides", func(t *testing.T) {
		c, err := ConfigFromEnv(envMap(map[string]string{
			"BRAID_LLM_BASE_URL":       "https://api.example.com/v1",
			"BRAID_LLM_API_KEY":        "sk-test",
			"BRAID_LLM_PRICE_TABLE":    minPriceTable,
			"BRAID_LLM_TIMEOUT":        "30s",
			"BRAID_LLM_MAX_ATTEMPTS":   "5",
			"BRAID_LLM_BACKOFF_BASE":   "200ms",
			"BRAID_LLM_BACKOFF_FACTOR": "3",
			"BRAID_LLM_BACKOFF_MAX":    "1m",
		}))
		if err != nil {
			t.Fatalf("ConfigFromEnv() = %v", err)
		}
		if c.Timeout != 30*time.Second || c.MaxAttempts != 5 ||
			c.BackoffBase != 200*time.Millisecond || c.BackoffFactor != 3 || c.BackoffMax != time.Minute {
			t.Errorf("overrides not applied: %+v", c)
		}
	})
	t.Run("missing required", func(t *testing.T) {
		for name, env := range map[string]map[string]string{
			"no base url":    {"BRAID_LLM_API_KEY": "k", "BRAID_LLM_PRICE_TABLE": minPriceTable},
			"no api key":     {"BRAID_LLM_BASE_URL": "https://x/v1", "BRAID_LLM_PRICE_TABLE": minPriceTable},
			"no price table": {"BRAID_LLM_BASE_URL": "https://x/v1", "BRAID_LLM_API_KEY": "k"},
		} {
			if _, err := ConfigFromEnv(envMap(env)); err == nil {
				t.Errorf("%s: ConfigFromEnv() = nil, want error", name)
			}
		}
	})
	t.Run("bad price table json", func(t *testing.T) {
		_, err := ConfigFromEnv(envMap(map[string]string{
			"BRAID_LLM_BASE_URL":    "https://x/v1",
			"BRAID_LLM_API_KEY":     "k",
			"BRAID_LLM_PRICE_TABLE": "{",
		}))
		if err == nil || !strings.Contains(err.Error(), "price table") {
			t.Errorf("bad table err = %v, want price table error", err)
		}
	})
	t.Run("bad duration", func(t *testing.T) {
		_, err := ConfigFromEnv(envMap(map[string]string{
			"BRAID_LLM_BASE_URL":    "https://x/v1",
			"BRAID_LLM_API_KEY":     "k",
			"BRAID_LLM_PRICE_TABLE": minPriceTable,
			"BRAID_LLM_TIMEOUT":     "fast",
		}))
		if err == nil || !strings.Contains(err.Error(), "BRAID_LLM_TIMEOUT") {
			t.Errorf("bad duration err = %v, want env key in error", err)
		}
	})
}

func TestConfigValidate(t *testing.T) {
	valid := func() Config {
		c := DefaultConfig()
		c.BaseURL = "https://api.example.com/v1"
		c.APIKey = "sk-test"
		c.PriceTable = mustTable(t, minPriceTable)
		return c
	}
	cases := []struct {
		name string
		mut  func(*Config)
	}{
		{"relative url", func(c *Config) { c.BaseURL = "api.example.com/v1" }},
		{"ftp scheme", func(c *Config) { c.BaseURL = "ftp://api.example.com/v1" }},
		{"zero timeout", func(c *Config) { c.Timeout = 0 }},
		{"timeout over 10m", func(c *Config) { c.Timeout = 11 * time.Minute }},
		{"zero attempts", func(c *Config) { c.MaxAttempts = 0 }},
		{"attempts over 10", func(c *Config) { c.MaxAttempts = 11 }},
		{"zero backoff base", func(c *Config) { c.BackoffBase = 0 }},
		{"factor below 1", func(c *Config) { c.BackoffFactor = 0.5 }},
		{"max below base", func(c *Config) { c.BackoffMax = time.Millisecond }},
		{"empty price table", func(c *Config) { c.PriceTable = PriceTable{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.mut(&c)
			if err := c.Validate(); err == nil {
				t.Fatalf("Validate() = nil, want error")
			}
		})
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid config Validate() = %v", err)
	}
}

// TestConfigValidateAttemptBudget 钉死组合上界(HIGH① 拍板):单 attempt 内
// 最坏停留时长(MaxAttempts×Timeout + 精确退避序列总和)不得超过 llm 节点
// deadline 减派发余量(scheduler.NodeTimeout("llm") - dispatchMargin)——
// asynq 每投递 ctx 自 dequeue 起算 30min,恒先于 reaper(自 claim)收紧;
// 超窗即 mid-call 掐断,重派新幂等键 = 供应商重复调用重复计费且首调 usage
// 丢失。DefaultConfig 预算 = 3×120s + 3.6s ≈ 6m,余量充足。
func TestConfigValidateAttemptBudget(t *testing.T) {
	valid := func() Config {
		c := DefaultConfig()
		c.BaseURL = "https://api.example.com/v1"
		c.APIKey = "sk-test"
		c.PriceTable = mustTable(t, minPriceTable)
		return c
	}
	// 恰触线:MaxAttempts=3、退避 1s×1.2+2s×1.2=3.6s(含抖动上界)⇒
	// Timeout=(25m-3.6s)/3,单参数上界(≤10m)之内、组合恰等于预算上限
	// (30m-5m)——边界含(≤ 而非 <)。
	budget := scheduler.NodeTimeout("llm") - dispatchMargin
	boundary := (budget - 3600*time.Millisecond) / 3
	cases := []struct {
		name    string
		mut     func(*Config)
		wantErr bool
	}{
		{"default within budget", nil, false},
		{"exact boundary passes", func(c *Config) { c.Timeout = boundary }, false},
		{"one nanosecond over", func(c *Config) { c.Timeout = boundary + time.Nanosecond }, true},
		{"extreme combo rejected", func(c *Config) {
			c.Timeout = 10 * time.Minute
			c.MaxAttempts = 10
		}, true},
		{"long timeout few attempts ok", func(c *Config) {
			c.Timeout = 10 * time.Minute
			c.MaxAttempts = 2
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			if tc.mut != nil {
				tc.mut(&c)
			}
			err := c.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want budget rejection")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want pass", err)
			}
		})
	}
}

func mustTable(t *testing.T, raw string) PriceTable {
	t.Helper()
	table, err := ParsePriceTable(raw)
	if err != nil {
		t.Fatalf("parse table: %v", err)
	}
	return table
}
