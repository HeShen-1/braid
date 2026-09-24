package tools

// file_read 工具测试(T3.5)。验收锚(issue #32):白名单外路径被拒
// (含符号链接逃逸);白名单内(含子目录、符号链接挂载根)正常读出。

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runFileRead(t *testing.T, tool *FileReadTool, path string) (Result, error) {
	t.Helper()
	return tool.Run(context.Background(), Call{Inputs: map[string]any{"path": path}})
}

func mustFileReadTool(t *testing.T, cfg FileReadConfig) *FileReadTool {
	t.Helper()
	tool, err := NewFileReadTool(cfg)
	if err != nil {
		t.Fatalf("NewFileReadTool: %v", err)
	}
	return tool
}

// TestNewFileReadToolConfig 锁定构造校验。
func TestNewFileReadToolConfig(t *testing.T) {
	bad := []struct {
		name string
		dirs []string
		want string
	}{
		{"empty allowlist", nil, "empty dir allowlist"},
		{"relative dir", []string{"relative/dir"}, "absolute"},
		{"pseudo-fs root /proc", []string{"/proc"}, "pseudo-filesystem"},
		{"pseudo-fs under /proc", []string{"/proc/self"}, "pseudo-filesystem"},
		{"pseudo-fs root /sys", []string{"/sys"}, "pseudo-filesystem"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewFileReadTool(FileReadConfig{Dirs: c.dirs})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want contains %q", err, c.want)
			}
		})
	}
}

// TestFileReadTool 是主验收矩阵。
func TestFileReadTool(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFileT(t, filepath.Join(dir, "a.txt"), "alpha")
	writeFileT(t, filepath.Join(sub, "b.txt"), "beta")
	tool := mustFileReadTool(t, FileReadConfig{Dirs: []string{dir}})

	t.Run("reads file in allowlist", func(t *testing.T) {
		res, err := runFileRead(t, tool, filepath.Join(dir, "a.txt"))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		out := decodeToolOutput(t, res)
		if out["content"] != "alpha" {
			t.Fatalf("content = %v", out["content"])
		}
		if out["size"] != json.Number("5") {
			t.Fatalf("size = %v", out["size"])
		}
		if out["path"] != filepath.Join(dir, "a.txt") {
			t.Fatalf("path = %v", out["path"])
		}
	})

	t.Run("reads nested file", func(t *testing.T) {
		res, err := runFileRead(t, tool, filepath.Join(dir, "sub", "b.txt"))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if out := decodeToolOutput(t, res); out["content"] != "beta" {
			t.Fatalf("content = %v", out["content"])
		}
	})

	t.Run("outside allowlist blocked", func(t *testing.T) {
		outside := t.TempDir()
		writeFileT(t, filepath.Join(outside, "secret.txt"), "secret")
		_, err := runFileRead(t, tool, filepath.Join(outside, "secret.txt"))
		if !errors.Is(err, ErrPathBlocked) {
			t.Fatalf("err = %v, want ErrPathBlocked", err)
		}
	})

	t.Run("sibling prefix is not confused with containment", func(t *testing.T) {
		// /tmp/T/x 是白名单,/tmp/T/x-sibling/f 不得因裸前缀误入。
		sibling := filepath.Join(filepath.Dir(dir), filepath.Base(dir)+"-sibling")
		if err := os.MkdirAll(sibling, 0o750); err != nil {
			t.Fatalf("mkdir sibling: %v", err)
		}
		writeFileT(t, filepath.Join(sibling, "f.txt"), "x")
		_, err := runFileRead(t, tool, filepath.Join(sibling, "f.txt"))
		if !errors.Is(err, ErrPathBlocked) {
			t.Fatalf("err = %v, want ErrPathBlocked (boundary)", err)
		}
	})

	t.Run("relative path blocked", func(t *testing.T) {
		_, err := runFileRead(t, tool, "a.txt")
		if !errors.Is(err, ErrPathBlocked) {
			t.Fatalf("err = %v, want ErrPathBlocked", err)
		}
	})

	t.Run("missing file is plain failure not path block", func(t *testing.T) {
		_, err := runFileRead(t, tool, filepath.Join(dir, "ghost.txt"))
		if err == nil {
			t.Fatal("err = nil, want failure")
		}
		if errors.Is(err, ErrPathBlocked) {
			t.Fatalf("missing file misclassified as path block: %v", err)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("err = %v, want fs.ErrNotExist in chain", err)
		}
	})

	t.Run("directory rejected as non-regular", func(t *testing.T) {
		_, err := runFileRead(t, tool, sub)
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v, want non-regular rejection", err)
		}
	})

	t.Run("symlink escape blocked", func(t *testing.T) {
		outside := t.TempDir()
		target := filepath.Join(outside, "outside-secret.txt")
		writeFileT(t, target, "outside")
		link := filepath.Join(dir, "escape-link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		_, err := runFileRead(t, tool, link)
		if !errors.Is(err, ErrPathBlocked) {
			t.Fatalf("err = %v, want ErrPathBlocked", err)
		}
		if !strings.Contains(err.Error(), "symlink escapes") {
			t.Fatalf("err = %v, want escape diagnostics", err)
		}
		// 逃逸诊断不携带白名单外目标路径(审计 LOW:error_message 对
		// 工作流作者可见,目标是主机布局信息)。
		if strings.Contains(err.Error(), outside) {
			t.Fatalf("err = %v leaks outside target path", err)
		}
	})

	t.Run("symlink to in-allowlist target allowed", func(t *testing.T) {
		link := filepath.Join(dir, "inner-link")
		if err := os.Symlink(filepath.Join(dir, "a.txt"), link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		res, err := runFileRead(t, tool, link)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if out := decodeToolOutput(t, res); out["content"] != "alpha" {
			t.Fatalf("content = %v", out["content"])
		}
	})

	t.Run("symlinked allowlist root resolving to /proc rejected", func(t *testing.T) {
		// 别名场景:词形根无辜、解析根失守(real = /proc)——构造期
		// 解析根同样过伪文件系统检查,响亮拒。
		alias := filepath.Join(t.TempDir(), "proc-alias")
		if err := os.Symlink("/proc", alias); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if _, err := NewFileReadTool(FileReadConfig{Dirs: []string{alias}}); err == nil || !strings.Contains(err.Error(), "pseudo-filesystem") {
			t.Fatalf("err = %v, want resolves-to-pseudo-filesystem rejection", err)
		}
	})

	t.Run("symlinked allowlist root resolved at construction", func(t *testing.T) {
		// 挂载点场景:白名单目录本身是符号链接——构造期解析根,否则
		// 白名单内合法文件因真实路径不在词形根下恒被拒。
		real := t.TempDir()
		writeFileT(t, filepath.Join(real, "f.txt"), "via-root-link")
		linkRoot := filepath.Join(t.TempDir(), "root-link")
		if err := os.Symlink(real, linkRoot); err != nil {
			t.Fatalf("symlink root: %v", err)
		}
		linked := mustFileReadTool(t, FileReadConfig{Dirs: []string{linkRoot}})
		res, err := runFileRead(t, linked, filepath.Join(linkRoot, "f.txt"))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		out := decodeToolOutput(t, res)
		if out["content"] != "via-root-link" {
			t.Fatalf("content = %v", out["content"])
		}
		// path 回显词形(不泄漏底层挂载布局),且词形/解析形双形态
		// 均可读(首道门 real 半边——output.path 回灌链路,审计 MEDIUM
		// 变异锚:删 real 半边对解析形读取误拒)。
		if out["path"] != filepath.Join(linkRoot, "f.txt") {
			t.Fatalf("path = %v, want lexical echo", out["path"])
		}
		if _, err := runFileRead(t, linked, filepath.Join(real, "f.txt")); err != nil {
			t.Fatalf("Run via resolved path: %v", err)
		}
	})

	t.Run("root slash allowlist reads everything", func(t *testing.T) {
		// 审计 MEDIUM:underAnyDir 对 "/" 根 d+Separator="//" 不匹配
		// 任何路径,白名单 "/" 曾静默退化为全拒。
		all := mustFileReadTool(t, FileReadConfig{Dirs: []string{"/"}})
		res, err := runFileRead(t, all, "/etc/hostname")
		if err != nil {
			t.Fatalf("Run under / whitelist: %v", err)
		}
		if out := decodeToolOutput(t, res); out["path"] != "/etc/hostname" {
			t.Fatalf("path = %v", out["path"])
		}
	})

	t.Run("size cap", func(t *testing.T) {
		capped := mustFileReadTool(t, FileReadConfig{Dirs: []string{dir}, MaxBytes: 4})
		writeFileT(t, filepath.Join(dir, "long.txt"), "toolongfile")
		_, err := runFileRead(t, capped, filepath.Join(dir, "long.txt"))
		// 精确锚 Stat 半边(size %d exceeds):与 LimitReader 半边
		// (size exceeds)形态可分,删 Stat 先检的变异可被杀(增长
		// 竞态半边不可确定性测试,双防线冗余接受)。
		if err == nil || !strings.Contains(err.Error(), "size 11 exceeds 4 bytes") {
			t.Fatalf("err = %v, want stat-path exact message", err)
		}
	})

	t.Run("multi-dir whitelist", func(t *testing.T) {
		other := t.TempDir()
		writeFileT(t, filepath.Join(other, "o.txt"), "other")
		multi := mustFileReadTool(t, FileReadConfig{Dirs: []string{dir, other}})
		if _, err := runFileRead(t, multi, filepath.Join(other, "o.txt")); err != nil {
			t.Fatalf("Run (second dir): %v", err)
		}
		if _, err := runFileRead(t, multi, filepath.Join(dir, "a.txt")); err != nil {
			t.Fatalf("Run (first dir): %v", err)
		}
	})

	t.Run("missing path input", func(t *testing.T) {
		if _, err := tool.Run(context.Background(), Call{Inputs: map[string]any{}}); err == nil {
			t.Fatal("err = nil, want missing-input rejection")
		}
	})

	t.Run("non-string path input", func(t *testing.T) {
		if _, err := tool.Run(context.Background(), Call{Inputs: map[string]any{"path": 42}}); err == nil {
			t.Fatal("err = nil, want type rejection")
		}
	})

	t.Run("runner maps block to tool_path_blocked", func(t *testing.T) {
		r := NewLocalRunner(registryOf(t, tool), NewTokenIssuer(nil, 0, nil), AllowAllAuthorizer{})
		_, err := r.Run(context.Background(), RunRequest{
			Tool: "file_read", Inputs: map[string]any{"path": "/etc/passwd"},
			NodeExecID: 9, AttemptNo: 1,
		})
		if got := callCodeOf(t, err); got != CodePathBlocked {
			t.Fatalf("code = %q, want %q (err=%v)", got, CodePathBlocked, err)
		}
	})
}
