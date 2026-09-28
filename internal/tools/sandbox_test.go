package tools

// 沙箱载体测试(T10.1):env 词表契约(D-t101)——unset=off=现状逐字节,
// mode=docker 下六键全部构造期校验、坏值键名响亮败;off 模式对沙箱键零
// 消费(零 docker 代码路径,api docker-free 的结构保证)。

import (
	"strings"
	"testing"
)

func TestSandboxConfigFromEnv(t *testing.T) {
	t.Run("unset defaults to off and ignores sandbox keys", func(t *testing.T) {
		cfg, err := sandboxConfigFromEnv(envOf(map[string]string{
			"BRAID_TOOL_SANDBOX_MEMORY": "garbage", // off 模式零消费:坏值不报错
		}))
		if err != nil {
			t.Fatalf("err = %v, want nil (off mode is inert)", err)
		}
		if cfg.Docker {
			t.Fatalf("docker = true, want false")
		}
	})

	t.Run("explicit off equals unset", func(t *testing.T) {
		cfg, err := sandboxConfigFromEnv(envOf(map[string]string{"BRAID_TOOL_SANDBOX": "off"}))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if cfg.Docker {
			t.Fatalf("docker = true, want false")
		}
	})

	t.Run("docker mode applies pinned defaults", func(t *testing.T) {
		cfg, err := sandboxConfigFromEnv(envOf(map[string]string{"BRAID_TOOL_SANDBOX": "docker"}))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !cfg.Docker {
			t.Fatalf("docker = false, want true")
		}
		want := sandboxConfig{
			Docker:         true,
			Image:          "braid-tool-sandbox:1",
			Memory:         "256m",
			CPUs:           "0.5",
			PidsLimit:      64,
			MaxConcurrency: 8,
		}
		if cfg != want {
			t.Fatalf("cfg = %+v, want %+v", cfg, want)
		}
	})

	t.Run("docker mode accepts full custom set", func(t *testing.T) {
		cfg, err := sandboxConfigFromEnv(envOf(map[string]string{
			"BRAID_TOOL_SANDBOX":                 "docker",
			"BRAID_TOOL_SANDBOX_IMAGE":           "registry.local/braid/sandbox@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			"BRAID_TOOL_SANDBOX_MEMORY":          "512m",
			"BRAID_TOOL_SANDBOX_CPUS":            "2",
			"BRAID_TOOL_SANDBOX_PIDS":            "128",
			"BRAID_TOOL_SANDBOX_MAX_CONCURRENCY": "16",
		}))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if cfg.Image != "registry.local/braid/sandbox@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" ||
			cfg.Memory != "512m" || cfg.CPUs != "2" || cfg.PidsLimit != 128 || cfg.MaxConcurrency != 16 {
			t.Fatalf("cfg = %+v, want custom values preserved", cfg)
		}
	})

	badMode := []string{"DOCKER", "auto", "local", "1"} // " docker " 经 TrimSpace 接受(tracing 先例)
	for _, v := range badMode {
		t.Run("rejects mode "+strings.TrimSpace(v), func(t *testing.T) {
			_, err := sandboxConfigFromEnv(envOf(map[string]string{"BRAID_TOOL_SANDBOX": v}))
			if err == nil || !strings.Contains(err.Error(), "BRAID_TOOL_SANDBOX") || !strings.Contains(err.Error(), "want off|docker") {
				t.Fatalf("err = %v, want named-key rejection with vocabulary", err)
			}
		})
	}

	badKeys := []struct {
		key  string
		val  string
		want string
	}{
		{"BRAID_TOOL_SANDBOX_IMAGE", "braid sandbox", "BRAID_TOOL_SANDBOX_IMAGE"},
		{"BRAID_TOOL_SANDBOX_IMAGE", "-evil", "BRAID_TOOL_SANDBOX_IMAGE"}, // 前导 - 可变旗标
		{"BRAID_TOOL_SANDBOX_MEMORY", "abc", "BRAID_TOOL_SANDBOX_MEMORY"},
		{"BRAID_TOOL_SANDBOX_MEMORY", "-1", "BRAID_TOOL_SANDBOX_MEMORY"},
		{"BRAID_TOOL_SANDBOX_CPUS", "0", "BRAID_TOOL_SANDBOX_CPUS"},
		{"BRAID_TOOL_SANDBOX_CPUS", "abc", "BRAID_TOOL_SANDBOX_CPUS"},
		{"BRAID_TOOL_SANDBOX_PIDS", "0", "BRAID_TOOL_SANDBOX_PIDS"},
		{"BRAID_TOOL_SANDBOX_PIDS", "5000", "BRAID_TOOL_SANDBOX_PIDS"},
		{"BRAID_TOOL_SANDBOX_MAX_CONCURRENCY", "0", "BRAID_TOOL_SANDBOX_MAX_CONCURRENCY"},
		{"BRAID_TOOL_SANDBOX_MAX_CONCURRENCY", "x", "BRAID_TOOL_SANDBOX_MAX_CONCURRENCY"},
	}
	for _, bad := range badKeys {
		t.Run("rejects "+bad.key+"="+bad.val, func(t *testing.T) {
			kv := map[string]string{
				"BRAID_TOOL_SANDBOX": "docker",
				bad.key:              bad.val,
			}
			_, err := sandboxConfigFromEnv(envOf(kv))
			if err == nil || !strings.Contains(err.Error(), bad.want) {
				t.Fatalf("err = %v, want named-key rejection for %s", err, bad.key)
			}
		})
	}
}
