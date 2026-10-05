package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestRunFlagLayerUsage 钉 argv 层契约:必填缺失/位置参数数目错/坏 flag
// → 退出码 2,stderr 带具体指引(不触 DB/网络)。
func TestRunFlagLayerUsage(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{"user create missing required", []string{"user", "create"}, "-username and -roles are required"},
		{"publish no file", []string{"definition", "publish"}, "exactly one DSL file"},
		{"publish two files", []string{"definition", "publish", "a.json", "b.json"}, "exactly one DSL file"},
		{"trigger missing required", []string{"execution", "trigger"}, "-definition and -input are required"},
		{"unknown flag", []string{"user", "create", "-nope"}, "not defined"},
		{"user create extra positional", []string{"user", "create", "-username", "a", "-roles", "author", "junk"}, "unexpected positional"},
		{"trigger extra positional", []string{"execution", "trigger", "-definition", "wf", "-input", "-", "junk"}, "unexpected positional"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := Run(context.Background(), tt.args, func(string) string { return "" },
				strings.NewReader(""), &out, &errb)
			if code != ExitUsage {
				t.Fatalf("Run(%q) exit = %d, want %d; stderr: %q",
					tt.args, code, ExitUsage, errb.String())
			}
			if !strings.Contains(errb.String(), tt.wantSub) {
				t.Errorf("stderr missing %q; got: %q", tt.wantSub, errb.String())
			}
		})
	}
}

// TestRunUsageErrors 钉分发契约:无参/未知命令/残缺名词组 → 退出码 2,
// stderr 带 usage(五命令面全列)。
func TestRunUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no args", nil},
		{"unknown command", []string{"frobnicate"}},
		{"unknown verb under known noun", []string{"user", "delete"}},
		{"noun without verb", []string{"user"}},
		{"unknown noun under definition", []string{"definition", "archive"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := Run(context.Background(), tt.args, func(string) string { return "" },
				strings.NewReader(""), &out, &errb)
			if code != ExitUsage {
				t.Fatalf("Run(%q) exit = %d, want %d; stderr: %q",
					tt.args, code, ExitUsage, errb.String())
			}
			if out.Len() != 0 {
				t.Errorf("usage error wrote to stdout: %q", out.String())
			}
			for _, want := range []string{
				"braid user create",
				"braid definition publish",
				"braid execution trigger",
				"braid seed",
				"braid audit-ready",
			} {
				if !strings.Contains(errb.String(), want) {
					t.Errorf("usage missing %q; got: %q", want, errb.String())
				}
			}
		})
	}
}
