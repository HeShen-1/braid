package ldap

// LDAP bind 留桩(T11.1):恒定失败 + 缝形状钉死。零栈。T16.1 随实现
// 迁企业增值面(D-2026-10-01-t161);缝面(IdentityProvider)留核心 auth。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"braid/internal/auth"
)

// 桩满足 IdentityProvider(缝形状锚:后续实现只换 Authenticate 体)。
var _ auth.IdentityProvider = LDAPBindProvider{}

func TestLDAPBindProviderStub(t *testing.T) {
	p := LDAPBindProvider{Addr: "ldaps://ldap.example.com:636"}
	_, err := p.Authenticate(context.Background(), "alice", "pw")
	if err == nil {
		t.Fatalf("留桩不得成功")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("err = %v, want 显式 not-implemented", err)
	}
	// 不误入凭据错误族:凭据判定语义随实现定形,桩期一律 500 面。
	var creds *auth.CredentialsError
	if errors.As(err, &creds) {
		t.Fatalf("留桩不得返回 CredentialsError: %v", err)
	}
}
