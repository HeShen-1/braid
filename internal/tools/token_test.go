package tools

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// 固定时钟:测试推进用。
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func TestTokenIssueAndVerify(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "basic"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := &fakeClock{now: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}
			iss := NewTokenIssuer([]byte("test-key-32-bytes-xxxxxxxxxxxx"), 60*time.Second, clk.Now)
			scope := TokenScope{NodeExecID: 42, AttemptNo: 3, Tool: "file_read"}
			tok, err := iss.Issue(scope)
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}
			if tok == "" {
				t.Fatal("Issue returned empty token")
			}
			clk.now = clk.now.Add(59 * time.Second) // TTL 内
			if err := iss.Verify(tok, scope); err != nil {
				t.Fatalf("Verify within TTL: %v", err)
			}
		})
	}
}

func TestTokenExpired(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}
	iss := NewTokenIssuer([]byte("k"), 60*time.Second, clk.Now)
	scope := TokenScope{NodeExecID: 1, AttemptNo: 1, Tool: "http"}
	tok, err := iss.Issue(scope)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	clk.now = clk.now.Add(61 * time.Second) // 越过 exp
	err = iss.Verify(tok, scope)
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("Verify expired = %v, want ErrTokenExpired", err)
	}
}

func TestTokenTamperRejected(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}
	iss := NewTokenIssuer([]byte("k"), 60*time.Second, clk.Now)
	scope := TokenScope{NodeExecID: 7, AttemptNo: 2, Tool: "http"}
	tok, err := iss.Issue(scope)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// 篡改 payload 首字符(签名不再匹配)。
	tampered := "X" + tok[1:]
	if err := iss.Verify(tampered, scope); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Verify tampered = %v, want ErrTokenInvalid", err)
	}
	// 篡改签名尾字符。
	tampered = tok[:len(tok)-1] + "X"
	if err := iss.Verify(tampered, scope); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Verify tampered sig = %v, want ErrTokenInvalid", err)
	}
}

func TestTokenScopeMismatch(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}
	iss := NewTokenIssuer([]byte("k"), 60*time.Second, clk.Now)
	scope := TokenScope{NodeExecID: 7, AttemptNo: 2, Tool: "http"}
	tok, err := iss.Issue(scope)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// 同 token 用在另一工具/另一 attempt/另一节点上 = 拒。
	for _, other := range []TokenScope{
		{NodeExecID: 7, AttemptNo: 2, Tool: "file_read"},
		{NodeExecID: 7, AttemptNo: 3, Tool: "http"},
		{NodeExecID: 8, AttemptNo: 2, Tool: "http"},
	} {
		if err := iss.Verify(tok, other); !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("Verify scope-mismatch = %v, want ErrTokenInvalid", err)
		}
	}
}

func TestTokenMalformed(t *testing.T) {
	iss := NewTokenIssuer([]byte("k"), 60*time.Second, nil)
	scope := TokenScope{NodeExecID: 1, AttemptNo: 1, Tool: "http"}
	for _, bad := range []string{"", "abc", "a.b.c", "!!!.???", "YQ.YQ"} {
		if err := iss.Verify(bad, scope); !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("Verify(%q) = %v, want ErrTokenInvalid", bad, err)
		}
	}
}

func TestTokenRandomKeyIsolation(t *testing.T) {
	// nil key = 进程内随机生成:两个 issuer 密钥不同,跨实例验证必拒。
	a := NewTokenIssuer(nil, 60*time.Second, nil)
	b := NewTokenIssuer(nil, 60*time.Second, nil)
	scope := TokenScope{NodeExecID: 1, AttemptNo: 1, Tool: "http"}
	tok, err := a.Issue(scope)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := b.Verify(tok, scope); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("cross-issuer Verify = %v, want ErrTokenInvalid", err)
	}
	// 同 key 重建的 issuer(重启语义)可验证。
	a2 := NewTokenIssuer([]byte("stable-key"), 60*time.Second, nil)
	b2 := NewTokenIssuer([]byte("stable-key"), 60*time.Second, nil)
	tok2, err := a2.Issue(scope)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := b2.Verify(tok2, scope); err != nil {
		t.Fatalf("same-key Verify: %v", err)
	}
}

func TestTokenIssueUniquePerCall(t *testing.T) {
	// 同 scope 两次签发:nonce 使 token 不同(per-call 语义)。
	iss := NewTokenIssuer([]byte("k"), 60*time.Second, nil)
	scope := TokenScope{NodeExecID: 1, AttemptNo: 1, Tool: "http"}
	t1, err := iss.Issue(scope)
	if err != nil {
		t.Fatalf("Issue 1: %v", err)
	}
	t2, err := iss.Issue(scope)
	if err != nil {
		t.Fatalf("Issue 2: %v", err)
	}
	if t1 == t2 {
		t.Fatal("two issues for same scope produced identical token")
	}
	if strings.Contains(t1, ".") == false || strings.Count(t1, ".") != 1 {
		t.Fatalf("token shape = %q, want payload.sig", t1)
	}
}

func TestTokenTTLDefaults(t *testing.T) {
	// TTL 0/负值 → DefaultTokenTTL。
	for _, ttl := range []time.Duration{0, -1} {
		clk := &fakeClock{now: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}
		iss := NewTokenIssuer([]byte("k"), ttl, clk.Now)
		scope := TokenScope{NodeExecID: 1, AttemptNo: 1, Tool: "http"}
		tok, err := iss.Issue(scope)
		if err != nil {
			t.Fatalf("Issue(ttl=%v): %v", ttl, err)
		}
		clk.now = clk.now.Add(DefaultTokenTTL - time.Second)
		if err := iss.Verify(tok, scope); err != nil {
			t.Fatalf("Verify at DefaultTokenTTL-1s (ttl=%v): %v", ttl, err)
		}
		clk.now = clk.now.Add(2 * time.Second)
		if err := iss.Verify(tok, scope); !errors.Is(err, ErrTokenExpired) {
			t.Fatalf("Verify past DefaultTokenTTL (ttl=%v) = %v, want ErrTokenExpired", ttl, err)
		}
	}
}
