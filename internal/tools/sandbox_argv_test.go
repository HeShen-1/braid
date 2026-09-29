package tools

// 沙箱 argv 白名单构造器测试(T10.1):docker CLI 是 root 等价注入面——
// 构造器是纯函数,一切变量值过值域正则(schemaSuffixRe 同款纪律),
// workflow 来源字符串结构性进不了 argv(inputs 走 stdin 信封,构造器
// 根本没有该参数)。golden 钉死旗标序(基线 15 控制矩阵的静态锚)。

import (
	"strings"
	"testing"
)

func TestSandboxArgvGolden(t *testing.T) {
	cfg := sandboxConfig{
		Docker:         true,
		Image:          "braid-tool-sandbox:1",
		Memory:         "256m",
		CPUs:           "0.5",
		PidsLimit:      64,
		MaxConcurrency: 8,
	}

	t.Run("file_read gets network none and mounts", func(t *testing.T) {
		args, err := buildSandboxRunArgv(cfg, "braid-sbx-1-2-abcd1234", "file_read", []string{"/var/braid/docs"})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		want := strings.Join([]string{
			"run", "--rm", "--interactive",
			"--name", "braid-sbx-1-2-abcd1234",
			"--label", "braid.sandbox=1",
			"--label", "braid.tool=file_read",
			"--read-only",
			"--tmpfs", "/tmp:rw,noexec,nosuid,size=16m",
			"--memory", "256m", "--memory-swap", "256m",
			"--cpus", "0.5",
			"--pids-limit", "64",
			"--ulimit", "nofile=1024:1024",
			"--cap-drop", "ALL",
			"--security-opt", "no-new-privileges",
			"--user", "65532:65532",
			"--init",
			"--log-driver", "json-file", "--log-opt", "max-size=5m", "--log-opt", "max-file=1",
			"--network", "none",
			"--mount", "type=bind,src=/var/braid/docs,dst=/var/braid/docs,readonly",
			"--entrypoint", "/usr/local/bin/braid-sandbox-exec",
			"braid-tool-sandbox:1",
			"file_read",
		}, " ")
		if got := strings.Join(args, " "); got != want {
			t.Fatalf("argv =\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("http gets egress network and no mounts", func(t *testing.T) {
		// T10.2 换装:bridge → egress 内网(值 = 解析期已填的 cfg 字段,
		// 这里显式设值钉死 golden;空值形态见下方响亮败用例)。
		cfg := cfg
		cfg.EgressNetwork = "braid-sbx-egress"
		args, err := buildSandboxRunArgv(cfg, "braid-sbx-1-2-abcd1234", "http", nil)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		// 全序列 golden(与 file_read 用例同款):共享硬化前缀 + egress
		// 网位逐字钉死,顺带结构性排除 -e/--env-file/--env 与 --mount——
		// 信任材料的 env 通道被 argv 层封死(基线 §5 / T7 私 CA 契约
		// 「运行期零通道」的 env 半边机械锚)。
		want := strings.Join([]string{
			"run", "--rm", "--interactive",
			"--name", "braid-sbx-1-2-abcd1234",
			"--label", "braid.sandbox=1",
			"--label", "braid.tool=http",
			"--read-only",
			"--tmpfs", "/tmp:rw,noexec,nosuid,size=16m",
			"--memory", "256m", "--memory-swap", "256m",
			"--cpus", "0.5",
			"--pids-limit", "64",
			"--ulimit", "nofile=1024:1024",
			"--cap-drop", "ALL",
			"--security-opt", "no-new-privileges",
			"--user", "65532:65532",
			"--init",
			"--log-driver", "json-file", "--log-opt", "max-size=5m", "--log-opt", "max-file=1",
			"--network", "braid-sbx-egress",
			"--entrypoint", "/usr/local/bin/braid-sandbox-exec",
			"braid-tool-sandbox:1",
			"http",
		}, " ")
		if got := strings.Join(args, " "); got != want {
			t.Fatalf("argv =\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("http without egress network fails loud", func(t *testing.T) {
		// 直接构造零值配置(绕过 env 解析的装配缺陷形态):绝不静默回落
		// bridge——过渡网位已随 T10.2 死,缺网络名 = 配置面断了。
		_, err := buildSandboxRunArgv(cfg, "braid-sbx-1-2-abcd1234", "http", nil)
		if err == nil || !strings.Contains(err.Error(), "egress network") {
			t.Fatalf("err = %v, want loud egress network requirement", err)
		}
	})
}

func TestSandboxArgvRejectsHostile(t *testing.T) {
	cfg := sandboxConfig{Docker: true, Image: "braid-tool-sandbox:1", Memory: "256m", CPUs: "0.5", PidsLimit: 64, MaxConcurrency: 8}

	hostile := []struct {
		name   string
		arg    func() ([]string, error)
		wantIn string
	}{
		{"container name with space", func() ([]string, error) {
			return buildSandboxRunArgv(cfg, "braid sbx", "http", nil)
		}, "container name"},
		{"container name leading dash", func() ([]string, error) {
			return buildSandboxRunArgv(cfg, "-evil", "http", nil)
		}, "container name"},
		{"unknown tool", func() ([]string, error) {
			return buildSandboxRunArgv(cfg, "braid-sbx-1-2-abcd1234", "exec", nil)
		}, "tool"},
		{"tool with space", func() ([]string, error) {
			return buildSandboxRunArgv(cfg, "braid-sbx-1-2-abcd1234", "http x", nil)
		}, "tool"},
		{"mount dir with comma injects options", func() ([]string, error) {
			return buildSandboxRunArgv(cfg, "braid-sbx-1-2-abcd1234", "file_read", []string{"/a,readonly"})
		}, "mount"},
		{"mount dir relative", func() ([]string, error) {
			return buildSandboxRunArgv(cfg, "braid-sbx-1-2-abcd1234", "file_read", []string{"rel"})
		}, "mount"},
		{"mount dir with space", func() ([]string, error) {
			return buildSandboxRunArgv(cfg, "braid-sbx-1-2-abcd1234", "file_read", []string{"/a b"})
		}, "mount"},
		// 注:--mount 值位恒以 "type=bind," 前缀,dash 不可能变旗标;
		// 真实注入向量是逗号(选项分隔)与空白,均已覆盖。
	}
	for _, h := range hostile {
		t.Run(h.name, func(t *testing.T) {
			args, err := h.arg()
			if err == nil || !strings.Contains(err.Error(), h.wantIn) {
				t.Fatalf("err = %v, want rejection naming %q", err, h.wantIn)
			}
			if args != nil {
				t.Fatalf("argv must be nil on rejection, got %v", args)
			}
		})
	}
}
