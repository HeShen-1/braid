package main

// cmd/worker 装配决策面单测(T4.5,对抗审计 LOW⑧):buildNotifier 的三分支
// ——整组缺省 → nil(降级);半配置(正向/反向)→ error;全配置 → 非 nil。
// sql.Open 惰性零连接(成功路径仅构造 Sender,不触网不触 DB)。

import (
	"database/sql"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"braid/internal/snowflake"
)

func TestBuildNotifier(t *testing.T) {
	// 惰性连接池:NewSender 只校验非 nil,不拨号。
	db, err := sql.Open("mysql", "unused:unused@tcp(127.0.0.1:1)/")
	if err != nil {
		t.Fatalf("open stub pool: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ids, err := snowflake.New(9)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	full := map[string]string{
		"BRAID_SMTP_HOST":         "127.0.0.1",
		"BRAID_SMTP_PORT":         "2525",
		"BRAID_SMTP_STARTTLS":     "false",
		"BRAID_SMTP_FROM":         "braid@test.example",
		"BRAID_SMTP_NOTIFY_TO":    "ops@test.example",
		"BRAID_APPROVAL_BASE_URL": "https://braid.test.example",
	}
	cases := []struct {
		name      string
		env       map[string]string
		wantNil   bool
		wantError bool
	}{
		{"unconfigured group degrades to nil", nil, true, false},
		{"knob-only orphans still nil", map[string]string{
			"BRAID_SMTP_PORT": "2525",
		}, true, false},
		{"full config builds sender", full, false, false},
		{"forward half-config fails", map[string]string{
			"BRAID_SMTP_HOST":      "127.0.0.1",
			"BRAID_SMTP_FROM":      "braid@test.example",
			"BRAID_SMTP_NOTIFY_TO": "ops@test.example",
			// 缺 BRAID_APPROVAL_BASE_URL
		}, false, true},
		{"reverse half-config fails", map[string]string{
			"BRAID_SMTP_FROM":         "braid@test.example",
			"BRAID_SMTP_NOTIFY_TO":    "ops@test.example",
			"BRAID_APPROVAL_BASE_URL": "https://braid.test.example",
			// 缺 BRAID_SMTP_HOST
		}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, err := buildNotifier(func(k string) string { return tc.env[k] }, db, ids, "test-worker")
			if tc.wantError {
				if err == nil {
					t.Fatalf("buildNotifier(%s) = nil error, want error", tc.name)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildNotifier(%s): %v", tc.name, err)
			}
			if tc.wantNil && n != nil {
				t.Fatalf("buildNotifier(%s) = %T, want nil (degraded)", tc.name, n)
			}
			if !tc.wantNil && n == nil {
				t.Fatalf("buildNotifier(%s) = nil, want sender", tc.name)
			}
		})
	}
}
