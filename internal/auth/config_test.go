package auth

import (
	"strings"
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	ok := Config{JWTSecret: testSecret, TokenTTL: time.Hour, BcryptCost: 10}
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法配置被拒: %v", err)
	}
	// 上界边界本身合法:7d = DISABLED 吊销语义允许的最大暴露窗。
	boundary := ok
	boundary.TokenTTL = MaxTokenTTL
	if err := boundary.Validate(); err != nil {
		t.Fatalf("7d 边界应合法: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*Config)
	}{
		{"short secret", func(c *Config) { c.JWTSecret = "short" }},
		{"empty secret", func(c *Config) { c.JWTSecret = "" }},
		{"zero ttl", func(c *Config) { c.TokenTTL = 0 }},
		{"negative ttl", func(c *Config) { c.TokenTTL = -time.Hour }},
		{"ttl over 7d", func(c *Config) { c.TokenTTL = MaxTokenTTL + time.Second }},
		{"cost below min", func(c *Config) { c.BcryptCost = 3 }},
		{"cost above max", func(c *Config) { c.BcryptCost = 32 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ok
			tc.mut(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("矛盾配置被放行")
			}
			if !strings.HasPrefix(err.Error(), "auth:") {
				t.Fatalf("错误无包前缀: %v", err)
			}
		})
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Run("full", func(t *testing.T) {
		env := map[string]string{
			"BRAID_AUTH_JWT_SECRET":  testSecret,
			"BRAID_AUTH_TOKEN_TTL":   "30m",
			"BRAID_AUTH_BCRYPT_COST": "12",
		}
		c, err := ConfigFromEnv(func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if c.JWTSecret != testSecret || c.TokenTTL != 30*time.Minute || c.BcryptCost != 12 {
			t.Fatalf("c = %+v", c)
		}
	})
	t.Run("defaults with secret", func(t *testing.T) {
		c, err := ConfigFromEnv(func(string) string { return "" })
		if err == nil {
			t.Fatalf("缺 secret 应启动即败(无默认值)")
		}
		if c.TokenTTL != DefaultTokenTTL || c.BcryptCost != DefaultBcryptCost {
			t.Fatalf("部分构造时默认值仍应就位: %+v", c)
		}
	})
	t.Run("bad duration", func(t *testing.T) {
		env := map[string]string{"BRAID_AUTH_JWT_SECRET": testSecret, "BRAID_AUTH_TOKEN_TTL": "abc"}
		if _, err := ConfigFromEnv(func(k string) string { return env[k] }); err == nil {
			t.Fatalf("坏 duration 被放行")
		}
	})
	t.Run("bad cost", func(t *testing.T) {
		env := map[string]string{"BRAID_AUTH_JWT_SECRET": testSecret, "BRAID_AUTH_BCRYPT_COST": "xyz"}
		if _, err := ConfigFromEnv(func(k string) string { return env[k] }); err == nil {
			t.Fatalf("坏 cost 被放行")
		}
	})
	t.Run("secret fails validate", func(t *testing.T) {
		// ConfigFromEnv 末尾过 Validate:短 secret 走启动即败路径。
		env := map[string]string{"BRAID_AUTH_JWT_SECRET": "short"}
		if _, err := ConfigFromEnv(func(k string) string { return env[k] }); err == nil {
			t.Fatalf("短 secret 经 env 路径被放行")
		}
	})
}
