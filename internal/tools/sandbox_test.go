package tools

// 沙箱载体测试(T10.1):env 词表契约(D-t101)——unset=off=现状逐字节,
// mode=docker 下六键全部构造期校验、坏值键名响亮败;off 模式对沙箱键零
// 消费(零 docker 代码路径,api docker-free 的结构保证)。

import (
	"net/url"
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
			EgressNetwork:  sandboxEgressNetworkDefault,
		}
		if cfg != want {
			t.Fatalf("cfg = %+v, want %+v", cfg, want)
		}
	})

	t.Run("docker mode accepts full custom set", func(t *testing.T) {
		proxy, err := url.Parse("http://172.18.0.1:9210")
		if err != nil {
			t.Fatalf("parse proxy: %v", err)
		}
		cfg, err := sandboxConfigFromEnv(envOf(map[string]string{
			"BRAID_TOOL_SANDBOX":                 "docker",
			"BRAID_TOOL_SANDBOX_IMAGE":           "registry.local/braid/sandbox@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			"BRAID_TOOL_SANDBOX_MEMORY":          "512m",
			"BRAID_TOOL_SANDBOX_CPUS":            "2",
			"BRAID_TOOL_SANDBOX_PIDS":            "128",
			"BRAID_TOOL_SANDBOX_MAX_CONCURRENCY": "16",
			"BRAID_TOOL_SANDBOX_EGRESS_NETWORK":  "braid-egress-custom",
			"BRAID_TOOL_SANDBOX_EGRESS_ADDR":     "http://172.18.0.1:9210",
		}))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if cfg.Image != "registry.local/braid/sandbox@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" ||
			cfg.Memory != "512m" || cfg.CPUs != "2" || cfg.PidsLimit != 128 || cfg.MaxConcurrency != 16 ||
			cfg.EgressNetwork != "braid-egress-custom" || cfg.EgressAddr == nil || cfg.EgressAddr.String() != proxy.String() {
			t.Fatalf("cfg = %+v, want custom values preserved (egress %v)", cfg, proxy)
		}
	})

	// T10.2 egress 键:网络名值域(argv 注入面)与代理地址词形单门;空
	// ADDR 合法(worker 启动段恒写,api 恒空——nil proxy 的信封 omitted)。
	for _, bad := range []struct{ key, val string }{
		{"BRAID_TOOL_SANDBOX_EGRESS_NETWORK", "-evil"},          // 前导 - 变旗标
		{"BRAID_TOOL_SANDBOX_EGRESS_NETWORK", "has space"},      // 空白
		{"BRAID_TOOL_SANDBOX_EGRESS_NETWORK", "a,b"},            // 逗号(docker 选项分隔面)
		{"BRAID_TOOL_SANDBOX_EGRESS_ADDR", "https://h:1"},       // 只接受明文 http 说代理
		{"BRAID_TOOL_SANDBOX_EGRESS_ADDR", "http://h"},          // 端口必显式
		{"BRAID_TOOL_SANDBOX_EGRESS_ADDR", "http://h:1/p"},      // 无 path
		{"BRAID_TOOL_SANDBOX_EGRESS_ADDR", "http://u:p@h:1"},    // 无 userinfo
		{"BRAID_TOOL_SANDBOX_EGRESS_ADDR", "not a url at all!"}, // 词形外
	} {
		t.Run("rejects "+bad.key+"="+bad.val, func(t *testing.T) {
			_, err := sandboxConfigFromEnv(envOf(map[string]string{
				"BRAID_TOOL_SANDBOX": "docker",
				bad.key:              bad.val,
			}))
			if err == nil || !strings.Contains(err.Error(), bad.key) {
				t.Fatalf("err = %v, want named-key rejection for %s", err, bad.key)
			}
		})
	}

	t.Run("egress addr unset is legal nil", func(t *testing.T) {
		cfg, err := sandboxConfigFromEnv(envOf(map[string]string{"BRAID_TOOL_SANDBOX": "docker"}))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if cfg.EgressAddr != nil {
			t.Fatalf("egress addr = %v, want nil (api assembly path)", cfg.EgressAddr)
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
		{"BRAID_TOOL_SANDBOX_MEMORY", "0", "BRAID_TOOL_SANDBOX_MEMORY"}, // docker --memory 0 = 无上限
		{"BRAID_TOOL_SANDBOX_MEMORY", "00", "BRAID_TOOL_SANDBOX_MEMORY"},
		{"BRAID_TOOL_SANDBOX_MEMORY", "0m", "BRAID_TOOL_SANDBOX_MEMORY"},
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
