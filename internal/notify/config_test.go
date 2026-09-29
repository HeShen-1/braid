package notify

// 配置面单测(零 DB,t13 不可达也跑):四件套契约镜像 relay/llm 同款测试。

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
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

// TestConfigFromEnvCAFile BRAID_SMTP_CA_FILE(T12.2,D-t45 §7 蒸发回收):
// env→路径→PEM 信任池注入;Host 在场 + 文件不可读/PEM 空 = 启动即败
// (半配置纪律);Host 空时孤儿容忍(纯旋钮先例:PORT/TIMEOUT/STARTTLS
// 同待遇——不进反向半配置闸,但值本身仍须可解析)。变异自查:加载丢 →
// happy_path 红;Host 空硬拒 → orphan_tolerated 红。
func TestConfigFromEnvCAFile(t *testing.T) {
	// 自签证书 PEM(fakesmtp 同款生成法,config 测试面自带不依赖桩)。
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{"relay.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	writeFile := func(t *testing.T, content []byte) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "ca.pem")
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatalf("write ca file: %v", err)
		}
		return path
	}
	baseEnv := map[string]string{
		"BRAID_SMTP_HOST":         "smtp.example.com",
		"BRAID_SMTP_FROM":         "braid@example.com",
		"BRAID_SMTP_NOTIFY_TO":    "ops@example.com",
		"BRAID_APPROVAL_BASE_URL": "https://braid.example.com",
	}
	withCA := func(path string) map[string]string {
		env := make(map[string]string, len(baseEnv)+1)
		for k, v := range baseEnv {
			env[k] = v
		}
		env["BRAID_SMTP_CA_FILE"] = path
		return env
	}

	t.Run("happy path loads pool", func(t *testing.T) {
		c, err := ConfigFromEnv(func(k string) string { return withCA(writeFile(t, pemBytes))[k] })
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if c.TLSRootCAs == nil {
			t.Fatalf("TLSRootCAs = nil, want pool from CA file")
		}
	})

	t.Run("unreadable file fails closed", func(t *testing.T) {
		env := withCA(filepath.Join(t.TempDir(), "missing.pem"))
		_, err := ConfigFromEnv(func(k string) string { return env[k] })
		if err == nil || !strings.Contains(err.Error(), "BRAID_SMTP_CA_FILE") {
			t.Fatalf("err = %v, want BRAID_SMTP_CA_FILE error", err)
		}
	})

	t.Run("non-pem content fails closed", func(t *testing.T) {
		env := withCA(writeFile(t, []byte("not a certificate")))
		_, err := ConfigFromEnv(func(k string) string { return env[k] })
		if err == nil || !strings.Contains(err.Error(), "BRAID_SMTP_CA_FILE") {
			t.Fatalf("err = %v, want BRAID_SMTP_CA_FILE error (no certs parsed)", err)
		}
	})

	t.Run("orphan tolerated when host unset", func(t *testing.T) {
		env := map[string]string{"BRAID_SMTP_CA_FILE": writeFile(t, pemBytes)}
		c, err := ConfigFromEnv(func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("orphan CA (host unset) err = %v, want tolerated", err)
		}
		if c.TLSRootCAs == nil {
			t.Fatalf("TLSRootCAs = nil, want pool loaded even when orphaned (value parses regardless)")
		}
	})
}
