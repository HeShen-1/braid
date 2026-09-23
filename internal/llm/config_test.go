package llm

// 单测:config 四件套(relay/config_test.go 同款:getenv map 桩 + 拒绝表)。
// 零 DB 零网络。

import (
	"strings"
	"testing"
	"time"
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

func mustTable(t *testing.T, raw string) PriceTable {
	t.Helper()
	table, err := ParsePriceTable(raw)
	if err != nil {
		t.Fatalf("parse table: %v", err)
	}
	return table
}
