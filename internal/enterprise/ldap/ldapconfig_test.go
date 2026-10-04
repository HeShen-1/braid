package ldap

import (
	"strings"
	"testing"
)

// TestParseLDAPConfig:配置组半开语义(零栈单元面),镜像 entoidc
// ParseOIDCConfig 惯例——全组缺省 = LDAP 关闭(零值 + 零错);任一键设
// 而必填不全 = 启动即败(正向/反向半配置双闸);URL 形态 / filter 模板
// 恰一 %s / StartTLS 词域与 ldaps 方案矛盾,全部启动期响亮拒绝。

func TestParseLDAPConfig(t *testing.T) {
	full := map[string]string{
		"BRAID_LDAP_ADDR":          "ldaps://ldap.example.com:636",
		"BRAID_LDAP_BIND_DN":       "cn=braid,ou=services,dc=example,dc=org",
		"BRAID_LDAP_BIND_PASSWORD": "svc-secret",
		"BRAID_LDAP_BASE_DN":       "ou=people,dc=example,dc=org",
	}
	tests := []struct {
		name    string
		env     map[string]string
		wantCfg LDAPConfig
		wantErr string
	}{
		{
			name:    "group absent = off",
			env:     map[string]string{},
			wantCfg: LDAPConfig{},
		},
		{
			name: "full group, optional keys default",
			env:  full,
			wantCfg: LDAPConfig{
				Addr: "ldaps://ldap.example.com:636", BindDN: "cn=braid,ou=services,dc=example,dc=org",
				BindPassword: "svc-secret", BaseDN: "ou=people,dc=example,dc=org",
				Filter: "(uid=%s)",
			},
		},
		{
			name: "full group + optional filter and starttls",
			env: func() map[string]string {
				m := map[string]string{
					"BRAID_LDAP_ADDR":          "ldap://ldap.example.com:389",
					"BRAID_LDAP_BIND_DN":       "cn=braid,dc=example,dc=org",
					"BRAID_LDAP_BIND_PASSWORD": "svc-secret",
					"BRAID_LDAP_BASE_DN":       "dc=example,dc=org",
					"BRAID_LDAP_FILTER":        "(&(objectClass=person)(uid=%s))",
					"BRAID_LDAP_STARTTLS":      "true",
				}
				return m
			}(),
			wantCfg: LDAPConfig{
				Addr: "ldap://ldap.example.com:389", BindDN: "cn=braid,dc=example,dc=org",
				BindPassword: "svc-secret", BaseDN: "dc=example,dc=org",
				Filter: "(&(objectClass=person)(uid=%s))", StartTLS: true,
			},
		},
		{
			name: "half-config: addr only",
			env: map[string]string{
				"BRAID_LDAP_ADDR": "ldaps://ldap.example.com:636",
			},
			wantErr: "auth: ldap half-configured: missing BRAID_LDAP_BIND_DN, BRAID_LDAP_BIND_PASSWORD, BRAID_LDAP_BASE_DN",
		},
		{
			name: "half-config: optional key only (reverse gate)",
			env: map[string]string{
				"BRAID_LDAP_FILTER": "(uid=%s)",
			},
			wantErr: "auth: ldap half-configured: missing BRAID_LDAP_ADDR, BRAID_LDAP_BIND_DN, BRAID_LDAP_BIND_PASSWORD, BRAID_LDAP_BASE_DN",
		},
		{
			// review LOW 修:显式 "0" 与 Filter/必填键同判(布尔假值不开
			// 特例),单设即触发反向半配置闸——与 Configured 注释口径钉死。
			name:    "starttls-only explicit \"0\" still trips the reverse gate",
			env:     map[string]string{"BRAID_LDAP_STARTTLS": "0"},
			wantErr: "auth: ldap half-configured: missing BRAID_LDAP_ADDR, BRAID_LDAP_BIND_DN, BRAID_LDAP_BIND_PASSWORD, BRAID_LDAP_BASE_DN",
		},
		{
			name:    "starttls-only \"1\" trips the gate",
			env:     map[string]string{"BRAID_LDAP_STARTTLS": "1"},
			wantErr: "auth: ldap half-configured: missing BRAID_LDAP_ADDR, BRAID_LDAP_BIND_DN, BRAID_LDAP_BIND_PASSWORD, BRAID_LDAP_BASE_DN",
		},
		{
			name:    "unsupported scheme",
			env:     withOverride(full, "BRAID_LDAP_ADDR", "http://ldap.example.com"),
			wantErr: "BRAID_LDAP_ADDR",
		},
		{
			name:    "addr without host",
			env:     withOverride(full, "BRAID_LDAP_ADDR", "ldap://:389"),
			wantErr: "BRAID_LDAP_ADDR",
		},
		{
			name:    "filter without substitution",
			env:     withOverride(full, "BRAID_LDAP_FILTER", "(objectClass=person)"),
			wantErr: "BRAID_LDAP_FILTER",
		},
		{
			name:    "filter with two substitutions",
			env:     withOverride(full, "BRAID_LDAP_FILTER", "(|(%s=%s))"),
			wantErr: "BRAID_LDAP_FILTER",
		},
		{
			// 对抗审计 LOW 修:%% 转义形态能过恰一 %s 计数,启动期须拒
			//(请求期才炸成坏 filter = 全量登录 500)。
			name:    "percent-escape template rejected",
			env:     withOverride(full, "BRAID_LDAP_FILTER", "(uid=%%s)"),
			wantErr: "stray % sequences rejected",
		},
		{
			name:    "starttls outside bool vocabulary",
			env:     withOverride(full, "BRAID_LDAP_STARTTLS", "maybe"),
			wantErr: "BRAID_LDAP_STARTTLS",
		},
		{
			name:    "starttls contradicts ldaps scheme",
			env:     withOverride(full, "BRAID_LDAP_STARTTLS", "1"),
			wantErr: "BRAID_LDAP_STARTTLS",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(k string) string { return tt.env[k] }
			got, err := ParseLDAPConfig(getenv)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseLDAPConfig() err = nil, want %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseLDAPConfig() err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLDAPConfig(): %v", err)
			}
			if got != tt.wantCfg {
				t.Fatalf("ParseLDAPConfig() = %+v, want %+v", got, tt.wantCfg)
			}
		})
	}
}

// withOverride 复制基线组并覆写一键(表驱动样板,零共享可变状态)。
func withOverride(base map[string]string, key, val string) map[string]string {
	m := make(map[string]string, len(base)+1)
	for k, v := range base {
		m[k] = v
	}
	m[key] = val
	return m
}
