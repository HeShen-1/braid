package notify

// 配置面单测(零 DB,t13 不可达也跑):四件套契约镜像 relay/llm 同款测试。

import (
	"testing"
	"time"
)

func TestConfigFromEnv(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr bool
		check   func(*testing.T, Config)
	}{
		{
			name: "unset group = unconfigured, valid",
			check: func(t *testing.T, c Config) {
				if c.Host != "" {
					t.Fatalf("host = %q, want empty (unconfigured)", c.Host)
				}
			},
		},
		{
			name: "complete group",
			env: map[string]string{
				// loopback + 显式 STARTTLS=false:合法形态(明文+认证的退化面),
				// 保持对 Port/Username/Password/StartTLS/Timeout 全显式解析的断言面。
				"BRAID_SMTP_HOST":         "127.0.0.1",
				"BRAID_SMTP_PORT":         "2525",
				"BRAID_SMTP_USERNAME":     "braid",
				"BRAID_SMTP_PASSWORD":     "secret",
				"BRAID_SMTP_FROM":         "braid@example.com",
				"BRAID_SMTP_NOTIFY_TO":    "ops@example.com, approval@example.com",
				"BRAID_SMTP_STARTTLS":     "false",
				"BRAID_SMTP_TIMEOUT":      "30s",
				"BRAID_APPROVAL_BASE_URL": "https://braid.example.com",
			},
			check: func(t *testing.T, c Config) {
				if c.Port != 2525 || c.Username != "braid" || c.Password != "secret" ||
					c.From != "braid@example.com" || c.StartTLS || c.Timeout != 30*time.Second {
					t.Fatalf("parsed = %+v", c)
				}
				if len(c.NotifyTo) != 2 || c.NotifyTo[1] != "approval@example.com" {
					t.Fatalf("notify_to = %v", c.NotifyTo)
				}
				if c.ApprovalBaseURL != "https://braid.example.com" {
					t.Fatalf("base = %q", c.ApprovalBaseURL)
				}
			},
		},
		{
			name: "defaults when only host-ish required keys set",
			env: map[string]string{
				"BRAID_SMTP_HOST":         "smtp.example.com",
				"BRAID_SMTP_FROM":         "braid@example.com",
				"BRAID_SMTP_NOTIFY_TO":    "ops@example.com",
				"BRAID_APPROVAL_BASE_URL": "https://braid.example.com/",
			},
			check: func(t *testing.T, c Config) {
				if c.Port != 587 || !c.StartTLS || c.Timeout != 15*time.Second {
					t.Fatalf("defaults = %+v", c)
				}
				// 尾斜杠规范化(链接拼接单源,不依赖调用方)。
				if c.ApprovalBaseURL != "https://braid.example.com" {
					t.Fatalf("base = %q, want trailing slash trimmed", c.ApprovalBaseURL)
				}
			},
		},
		{"host without from", map[string]string{
			"BRAID_SMTP_HOST":         "smtp.example.com",
			"BRAID_SMTP_NOTIFY_TO":    "ops@example.com",
			"BRAID_APPROVAL_BASE_URL": "https://braid.example.com",
		}, true, nil},
		{"host without notify_to", map[string]string{
			"BRAID_SMTP_HOST":         "smtp.example.com",
			"BRAID_SMTP_FROM":         "braid@example.com",
			"BRAID_APPROVAL_BASE_URL": "https://braid.example.com",
		}, true, nil},
		{"host without base url", map[string]string{
			"BRAID_SMTP_HOST":      "smtp.example.com",
			"BRAID_SMTP_FROM":      "braid@example.com",
			"BRAID_SMTP_NOTIFY_TO": "ops@example.com",
		}, true, nil},
		{"empty notify_to entry", map[string]string{
			"BRAID_SMTP_HOST":         "smtp.example.com",
			"BRAID_SMTP_FROM":         "braid@example.com",
			"BRAID_SMTP_NOTIFY_TO":    "ops@example.com,",
			"BRAID_APPROVAL_BASE_URL": "https://braid.example.com",
		}, true, nil},
		{"bad address", map[string]string{
			"BRAID_SMTP_HOST":         "smtp.example.com",
			"BRAID_SMTP_FROM":         "not-an-address",
			"BRAID_SMTP_NOTIFY_TO":    "ops@example.com",
			"BRAID_APPROVAL_BASE_URL": "https://braid.example.com",
		}, true, nil},
		{"address with space", map[string]string{
			"BRAID_SMTP_HOST":         "smtp.example.com",
			"BRAID_SMTP_FROM":         "braid@example.com",
			"BRAID_SMTP_NOTIFY_TO":    "ops @example.com",
			"BRAID_APPROVAL_BASE_URL": "https://braid.example.com",
		}, true, nil},
		{"base url not http(s)", map[string]string{
			"BRAID_SMTP_HOST":         "smtp.example.com",
			"BRAID_SMTP_FROM":         "braid@example.com",
			"BRAID_SMTP_NOTIFY_TO":    "ops@example.com",
			"BRAID_APPROVAL_BASE_URL": "ftp://braid.example.com",
		}, true, nil},
		{"base url relative", map[string]string{
			"BRAID_SMTP_HOST":         "smtp.example.com",
			"BRAID_SMTP_FROM":         "braid@example.com",
			"BRAID_SMTP_NOTIFY_TO":    "ops@example.com",
			"BRAID_APPROVAL_BASE_URL": "braid.example.com/approvals",
		}, true, nil},
		{"bad port", map[string]string{
			"BRAID_SMTP_HOST":         "smtp.example.com",
			"BRAID_SMTP_PORT":         "not-a-port",
			"BRAID_SMTP_FROM":         "braid@example.com",
			"BRAID_SMTP_NOTIFY_TO":    "ops@example.com",
			"BRAID_APPROVAL_BASE_URL": "https://braid.example.com",
		}, true, nil},
		{"bad starttls", map[string]string{
			"BRAID_SMTP_HOST":         "smtp.example.com",
			"BRAID_SMTP_STARTTLS":     "yes-please",
			"BRAID_SMTP_FROM":         "braid@example.com",
			"BRAID_SMTP_NOTIFY_TO":    "ops@example.com",
			"BRAID_APPROVAL_BASE_URL": "https://braid.example.com",
		}, true, nil},
		{"bad timeout", map[string]string{
			"BRAID_SMTP_HOST":         "smtp.example.com",
			"BRAID_SMTP_TIMEOUT":      "nope",
			"BRAID_SMTP_FROM":         "braid@example.com",
			"BRAID_SMTP_NOTIFY_TO":    "ops@example.com",
			"BRAID_APPROVAL_BASE_URL": "https://braid.example.com",
		}, true, nil},
		// ---- 对抗审计补锚:范围分支(解析层不拦的越界值)----
		{"port out of range", map[string]string{
			"BRAID_SMTP_HOST":         "smtp.example.com",
			"BRAID_SMTP_PORT":         "70000",
			"BRAID_SMTP_FROM":         "braid@example.com",
			"BRAID_SMTP_NOTIFY_TO":    "ops@example.com",
			"BRAID_APPROVAL_BASE_URL": "https://braid.example.com",
		}, true, nil},
		{"timeout over ceiling", map[string]string{
			"BRAID_SMTP_HOST":         "smtp.example.com",
			"BRAID_SMTP_TIMEOUT":      "5m",
			"BRAID_SMTP_FROM":         "braid@example.com",
			"BRAID_SMTP_NOTIFY_TO":    "ops@example.com",
			"BRAID_APPROVAL_BASE_URL": "https://braid.example.com",
		}, true, nil},
		// ---- 对抗审计补锚:反向半配置闸(HOST 拼错/丢失 + 语义键孤儿)----
		{"host missing but from set", map[string]string{
			"BRAID_SMTP_FROM":         "braid@example.com",
			"BRAID_SMTP_NOTIFY_TO":    "ops@example.com",
			"BRAID_APPROVAL_BASE_URL": "https://braid.example.com",
		}, true, nil},
		{"host missing but notify_to set", map[string]string{
			"BRAID_SMTP_NOTIFY_TO": "ops@example.com",
		}, true, nil},
		{"host missing but base url set", map[string]string{
			"BRAID_APPROVAL_BASE_URL": "https://braid.example.com",
		}, true, nil},
		{"host missing with knob-only orphans tolerated", map[string]string{
			"BRAID_SMTP_PORT":     "2525",
			"BRAID_SMTP_TIMEOUT":  "30s",
			"BRAID_SMTP_STARTTLS": "false",
		}, false, func(t *testing.T, c Config) {
			if c.Host != "" {
				t.Fatalf("host = %q, want empty", c.Host)
			}
		}},
		// ---- 对抗审计补锚:凭证轴配对闸 ----
		{"password without username", map[string]string{
			"BRAID_SMTP_HOST":         "smtp.example.com",
			"BRAID_SMTP_PASSWORD":     "secret",
			"BRAID_SMTP_FROM":         "braid@example.com",
			"BRAID_SMTP_NOTIFY_TO":    "ops@example.com",
			"BRAID_APPROVAL_BASE_URL": "https://braid.example.com",
		}, true, nil},
		{"auth requires starttls off-loopback", map[string]string{
			"BRAID_SMTP_HOST":         "smtp.example.com",
			"BRAID_SMTP_USERNAME":     "braid",
			"BRAID_SMTP_PASSWORD":     "secret",
			"BRAID_SMTP_STARTTLS":     "false",
			"BRAID_SMTP_FROM":         "braid@example.com",
			"BRAID_SMTP_NOTIFY_TO":    "ops@example.com",
			"BRAID_APPROVAL_BASE_URL": "https://braid.example.com",
		}, true, nil},
		{"auth plaintext ok on loopback", map[string]string{
			"BRAID_SMTP_HOST":         "127.0.0.1",
			"BRAID_SMTP_USERNAME":     "braid",
			"BRAID_SMTP_PASSWORD":     "secret",
			"BRAID_SMTP_STARTTLS":     "false",
			"BRAID_SMTP_FROM":         "braid@example.com",
			"BRAID_SMTP_NOTIFY_TO":    "ops@example.com",
			"BRAID_APPROVAL_BASE_URL": "https://braid.example.com",
		}, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ConfigFromEnv(func(k string) string { return tc.env[k] })
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ConfigFromEnv(%s) = nil error, want error", tc.name)
				}
				return
			}
			if err != nil {
				t.Fatalf("ConfigFromEnv(%s): %v", tc.name, err)
			}
			if tc.check != nil {
				tc.check(t, c)
			}
		})
	}
}
