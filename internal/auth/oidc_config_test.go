package auth

// OIDC 配置面(T11.1):半开双闸(正向半配置 / 反向半配置孤儿)、claim
// 词表、URL 形态(https,仅回环豁免 http)。零栈纯表驱动。
//
// 变异自查:半开闸删除 → missing_* 行红;claim 词表放空 → bad_claim 行红;
// 回环豁免删除 → http_loopback_* 行红(https 校验保留时非回环 http 红在
// http_public_* 行)。

import (
	"strings"
	"testing"
)

func TestParseOIDCConfig(t *testing.T) {
	const secret = "unit-test-oidc-client-secret-words-placeholder"
	// full 组基线;用例在其上做覆盖/删除。
	full := func() map[string]string {
		return map[string]string{
			"BRAID_OIDC_ISSUER":        "https://idp.example.com",
			"BRAID_OIDC_CLIENT_ID":     "braid",
			"BRAID_OIDC_CLIENT_SECRET": secret,
			"BRAID_OIDC_REDIRECT_URL":  "https://braid.example.com/api/v1/auth/oidc/callback",
		}
	}
	cases := []struct {
		name           string
		env            func(map[string]string) // nil = 原样;否则对基线改写
		wantErrSubstr  string
		wantClaim      string // 期望成功时的 Claim;关闭组 = ""
		wantConfigured bool
	}{
		{
			name: "off_all_empty",
			env: func(m map[string]string) {
				for k := range m {
					delete(m, k)
				}
			},
		},
		{
			name:           "full_group_claim_defaults",
			env:            nil,
			wantClaim:      DefaultOIDCClaim,
			wantConfigured: true,
		},
		{
			// email 词源结构性死路(RFC 5322 形态永不过 username 词域):
			// 配置面即拒,优于可配置但恒 401 的运维陷阱(review MEDIUM)。
			name: "claim_email_rejected_structurally_dead",
			env: func(m map[string]string) {
				m["BRAID_OIDC_CLAIM"] = "email"
			},
			wantErrSubstr: `BRAID_OIDC_CLAIM="email": want one of sub|preferred_username`,
		},
		{
			name: "claim_explicit_sub",
			env: func(m map[string]string) {
				m["BRAID_OIDC_CLAIM"] = OIDCClaimSub
			},
			wantClaim:      OIDCClaimSub,
			wantConfigured: true,
		},
		{
			// 正向半配置:issuer 设、凭证缺。
			name: "half_open_issuer_only",
			env: func(m map[string]string) {
				delete(m, "BRAID_OIDC_CLIENT_ID")
				delete(m, "BRAID_OIDC_CLIENT_SECRET")
				delete(m, "BRAID_OIDC_REDIRECT_URL")
			},
			wantErrSubstr: "half-configured: missing BRAID_OIDC_CLIENT_ID, BRAID_OIDC_CLIENT_SECRET, BRAID_OIDC_REDIRECT_URL",
		},
		{
			// 反向半配置:语义键(CLAIM)孤儿——词面键设而组主体缺。
			name: "half_open_claim_orphan",
			env: func(m map[string]string) {
				for k := range m {
					delete(m, k)
				}
				m["BRAID_OIDC_CLAIM"] = OIDCClaimSub
			},
			wantErrSubstr: "half-configured",
		},
		{
			name: "claim_outside_vocabulary",
			env: func(m map[string]string) {
				m["BRAID_OIDC_CLAIM"] = "email_verified"
			},
			wantErrSubstr: `BRAID_OIDC_CLAIM="email_verified": want one of sub|preferred_username`,
		},
		{
			name: "issuer_http_public_rejected",
			env: func(m map[string]string) {
				m["BRAID_OIDC_ISSUER"] = "http://idp.example.com"
			},
			wantErrSubstr: "http only allowed for loopback",
		},
		{
			name: "issuer_http_loopback_allowed",
			env: func(m map[string]string) {
				m["BRAID_OIDC_ISSUER"] = "http://127.0.0.1:9999"
			},
			wantClaim:      DefaultOIDCClaim,
			wantConfigured: true,
		},
		{
			name: "issuer_ftp_scheme_rejected",
			env: func(m map[string]string) {
				m["BRAID_OIDC_ISSUER"] = "ftp://idp.example.com"
			},
			wantErrSubstr: "want absolute http(s) url with host",
		},
		{
			name: "redirect_http_public_rejected",
			env: func(m map[string]string) {
				m["BRAID_OIDC_REDIRECT_URL"] = "http://braid.example.com/cb"
			},
			wantErrSubstr: "http only allowed for loopback",
		},
		{
			name: "redirect_https_ok",
			env: func(m map[string]string) {
				m["BRAID_OIDC_REDIRECT_URL"] = "https://braid.example.com/cb"
			},
			wantClaim:      DefaultOIDCClaim,
			wantConfigured: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := full()
			if tc.env != nil {
				tc.env(env)
			}
			cfg, err := ParseOIDCConfig(func(k string) string { return env[k] })
			if tc.wantErrSubstr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSubstr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErrSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if cfg.Claim != tc.wantClaim {
				t.Fatalf("claim = %q, want %q", cfg.Claim, tc.wantClaim)
			}
			if cfg.Configured() != tc.wantConfigured {
				t.Fatalf("configured = %v, want %v", cfg.Configured(), tc.wantConfigured)
			}
		})
	}
}

// TestOIDCConfigValidateZeroValue:零值组(全键缺省)Validate 通过——
// 「关闭」是合法状态,不是矛盾。
func TestOIDCConfigValidateZeroValue(t *testing.T) {
	var c OIDCConfig
	if c.Configured() {
		t.Fatalf("zero value reports configured")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("zero value validate: %v", err)
	}
}
