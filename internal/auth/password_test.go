package auth

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	hash, err := HashPassword("staple-horse-battery-01", 4)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "staple-horse-battery-01" {
		t.Fatalf("哈希等于明文")
	}
	if !validHashShape(hash) {
		t.Fatalf("哈希形态非法: %q", hash)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("staple-horse-battery-01")); err != nil {
		t.Fatalf("round trip verify: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("wrong-password-xyz")); err == nil {
		t.Fatalf("错密码通过校验")
	}
}

func TestHashPasswordSalts(t *testing.T) {
	// 同口令两次哈希必须不同(bcrypt 盐);两者都可验证。
	h1, _ := HashPassword("same-password-1234", 4)
	h2, _ := HashPassword("same-password-1234", 4)
	if h1 == h2 {
		t.Fatalf("两次哈希相同,盐缺失")
	}
}

func TestHashPasswordPolicy(t *testing.T) {
	cases := []struct {
		name     string
		password string
		ok       bool
	}{
		{"7 chars", "1234567", false},
		{"8 chars ok", "12345678", true},
		{"72 chars ok", strings.Repeat("a", 72), true},
		{"73 chars (bcrypt 截断线)", strings.Repeat("a", 73), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := HashPassword(tc.password, 4)
			if tc.ok && err != nil {
				t.Fatalf("HashPassword(len=%d) = %v, want nil", len(tc.password), err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatalf("HashPassword(len=%d) = nil, want error", len(tc.password))
				}
				if !errors.Is(err, ErrInvalidInput) {
					t.Fatalf("HashPassword(len=%d) = %v, want ErrInvalidInput", len(tc.password), err)
				}
			}
		})
	}
}

func TestHashPasswordCostBounds(t *testing.T) {
	for _, cost := range []int{3, 32} {
		if _, err := HashPassword("valid-password-1", cost); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("cost=%d: err=%v, want ErrInvalidInput", cost, err)
		}
	}
	if _, err := HashPassword("valid-password-1", bcrypt.MinCost); err != nil {
		t.Fatalf("MinCost: %v", err)
	}
}
