package notify

// IMConfig 契约测试(T14.3,D-t143 决策 5):三道闸(webhook 设 ⇒
// link_secret 熵地板 + base_url 必填;timeout 界;webhook URL 形状)+
// 未配置容忍(双端键孤儿 = api 侧独立供给,部署契约面)+ env 解析。

import (
	"strings"
	"testing"
	"time"
)

func TestIMConfigValidate(t *testing.T) {
	ok := IMConfig{
		WebhookURL: "https://oapi.dingtalk.com/robot/send?access_token=abc",
		LinkSecret: "braid-im-link-secret-0123456789ab",
		BaseURL:    "https://braid.test.example",
		Timeout:    10 * time.Second,
	}
	cases := []struct {
		name string
		mut  func(*IMConfig)
		want string // 空串 = 期望合法
	}{
		{"valid", func(*IMConfig) {}, ""},
		{
			"unconfigured tolerates orphan link secret (dual-end key, D-t143 #7)",
			func(c *IMConfig) { c.WebhookURL = ""; c.BaseURL = "" },
			"",
		},
		{
			"webhook set without link secret",
			func(c *IMConfig) { c.LinkSecret = "" },
			"link secret",
		},
		{
			"link secret below entropy floor",
			func(c *IMConfig) { c.LinkSecret = "short" },
			"link secret",
		},
		{
			"webhook set without base url",
			func(c *IMConfig) { c.BaseURL = "" },
			"base url",
		},
		{
			"webhook not absolute",
			func(c *IMConfig) { c.WebhookURL = "oapi.dingtalk.com/robot/send" },
			"webhook url",
		},
		{
			"webhook http on non-loopback rejected (plaintext gate, D-t143)",
			func(c *IMConfig) { c.WebhookURL = "http://oapi.dingtalk.com/robot/send?access_token=x" },
			"https",
		},
		{
			"webhook http on loopback tolerated (test stub form)",
			func(c *IMConfig) { c.WebhookURL = "http://127.0.0.1:9911/robot/send?access_token=x" },
			"",
		},
		{
			"base url http on non-loopback rejected (capability in URL)",
			func(c *IMConfig) { c.BaseURL = "http://braid.test.example" },
			"https",
		},
		{
			"webhook with fragment",
			func(c *IMConfig) { c.WebhookURL = "https://oapi.dingtalk.com/robot/send#f" },
			"webhook url",
		},
		{
			"timeout zero",
			func(c *IMConfig) { c.Timeout = 0 },
			"timeout",
		},
		{
			"timeout over 1m",
			func(c *IMConfig) { c.Timeout = 2 * time.Minute },
			"timeout",
		},
		{
			"base url not absolute http(s)",
			func(c *IMConfig) { c.BaseURL = "braid.test.example" },
			"base url",
		},
		{
			"base url with fragment (anchor suffix kills card links)",
			func(c *IMConfig) { c.BaseURL = "https://braid.test.example/#/console" },
			"base url",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ok
			tc.mut(&c)
			err := c.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestIMConfigFromEnv(t *testing.T) {
	const full = "braid-im-link-secret-0123456789ab"
	cases := []struct {
		name    string
		env     map[string]string
		want    IMConfig
		wantErr bool
	}{
		{
			name: "unset = channel off, defaults live",
			env:  map[string]string{},
			want: DefaultIMConfig(),
		},
		{
			name: "full group parses and trims base url slash",
			env: map[string]string{
				"BRAID_IM_WEBHOOK_URL":    "https://oapi.dingtalk.com/robot/send?access_token=abc",
				"BRAID_IM_SECRET":         "SECsecret",
				"BRAID_IM_LINK_SECRET":    full,
				"BRAID_APPROVAL_BASE_URL": "https://braid.test.example/",
			},
			want: IMConfig{
				WebhookURL: "https://oapi.dingtalk.com/robot/send?access_token=abc",
				Secret:     "SECsecret",
				LinkSecret: full,
				BaseURL:    "https://braid.test.example",
				Timeout:    10 * time.Second,
			},
		},
		{
			name: "half-configured fails loud",
			env: map[string]string{
				"BRAID_IM_WEBHOOK_URL": "https://oapi.dingtalk.com/robot/send?access_token=abc",
			},
			wantErr: true,
		},
		{
			name:    "bad timeout fails loud",
			env:     map[string]string{"BRAID_IM_TIMEOUT": "nope"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := IMConfigFromEnv(func(k string) string { return tc.env[k] })
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", c)
				}
				return
			}
			if err != nil {
				t.Fatalf("env: %v", err)
			}
			if c != tc.want {
				t.Fatalf("got %+v want %+v", c, tc.want)
			}
		})
	}
}
