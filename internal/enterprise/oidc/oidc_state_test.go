package oidc

// 握手态签名 cookie(T11.1):roundtrip / 篡改 / 过期 / 错钥 / 不完整。
// 零栈。T16.1 随实现迁企业增值面(D-2026-10-01-t161)。
//
// 变异自查:删除 hmac.Equal 比对 → tamper_* 行红;删除 Exp 复核 →
// expired 行红;删除三字段非空断言 → incomplete 行红。

import (
	"strings"
	"testing"
	"time"

	"braid/internal/auth"
)

func stateKeyForTest(t *testing.T, secret string) []byte {
	t.Helper()
	key, err := deriveStateKey(secret)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	return key
}

func TestOIDCStateRoundtrip(t *testing.T) {
	key := stateKeyForTest(t, "state-secret-words-placeholder-for-test")
	st := oidcAuthState{
		State: "state-value", Nonce: "nonce-value", Verifier: "verifier-value",
		Exp: time.Now().Add(auth.OIDCStateTTL).Unix(),
	}
	v, err := signOIDCState(key, st)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got, err := parseOIDCState(key, v)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != st {
		t.Fatalf("roundtrip = %+v, want %+v", got, st)
	}
}

func TestOIDCStateRejections(t *testing.T) {
	key := stateKeyForTest(t, "state-secret-words-placeholder-for-test")
	valid := func() oidcAuthState {
		return oidcAuthState{State: "s", Nonce: "n", Verifier: "v",
			Exp: time.Now().Add(time.Minute).Unix()}
	}
	cases := []struct {
		name       string
		value      func() string // 构造待验值
		wantSubstr string
	}{
		{
			name:       "tampered_payload",
			value:      func() string { v, _ := signOIDCState(key, valid()); return "!" + v },
			wantSubstr: "state payload", // 首字符破坏 base64 解码
		},
		{
			name: "tampered_signature",
			value: func() string {
				v, _ := signOIDCState(key, valid())
				return v[:len(v)-2] + "AA" // 签名段尾字节替换
			},
			wantSubstr: "signature mismatch",
		},
		{
			name: "expired",
			value: func() string {
				st := valid()
				st.Exp = time.Now().Add(-time.Second).Unix()
				v, _ := signOIDCState(key, st)
				return v
			},
			wantSubstr: "expired",
		},
		{
			name: "wrong_key",
			value: func() string {
				other := stateKeyForTest(t, "different-secret-words-placeholder")
				v, _ := signOIDCState(other, valid())
				return v
			},
			wantSubstr: "signature mismatch",
		},
		{
			name:       "malformed_no_dot",
			value:      func() string { return "garbage-without-dot" },
			wantSubstr: "malformed",
		},
		{
			name: "incomplete_fields",
			value: func() string {
				st := valid()
				st.Verifier = ""
				v, _ := signOIDCState(key, st)
				return v
			},
			wantSubstr: "incomplete",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseOIDCState(key, tc.value()); err == nil || !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantSubstr)
			}
		})
	}
}

// TestOIDCStateKeyDerivationDomainSeparated:不同 secret 派生互异密钥
// (HKDF 域分隔的基础锚;相同 secret 确定性派生)。
func TestOIDCStateKeyDerivationDomainSeparated(t *testing.T) {
	a := stateKeyForTest(t, "secret-a-words")
	b := stateKeyForTest(t, "secret-b-words")
	if string(a) == string(b) {
		t.Fatalf("distinct secrets derived identical keys")
	}
	if again := stateKeyForTest(t, "secret-a-words"); string(a) != string(again) {
		t.Fatalf("same secret derived unstable key")
	}
	if len(a) != 32 {
		t.Fatalf("key len = %d, want 32", len(a))
	}
}
