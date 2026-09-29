package tools

// 内置工具装配测试(T3.5):BuiltinToolsFromEnv 是 api 与 worker 的单源
// 装配缝——env 契约、未配置即不注册、坏配置响亮败。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envOf(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestBuiltinToolsFromEnv(t *testing.T) {
	t.Run("empty env registers nothing", func(t *testing.T) {
		ts, err := BuiltinToolsFromEnv(envOf(nil))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(ts) != 0 {
			t.Fatalf("tools = %d, want 0", len(ts))
		}
	})

	t.Run("both tools registered with sorted names", func(t *testing.T) {
		ts, err := BuiltinToolsFromEnv(envOf(map[string]string{
			"BRAID_HTTP_ENDPOINTS": `{"docs_api":"https://docs.example.com"}`,
			"BRAID_FILEREAD_DIRS":  `["/var/braid/docs"]`,
		}))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		reg := registryOf(t, ts...)
		names := reg.Names()
		if strings.Join(names, ",") != "file_read,http" {
			t.Fatalf("names = %v, want [file_read http]", names)
		}
	})

	t.Run("invalid endpoints json", func(t *testing.T) {
		_, err := BuiltinToolsFromEnv(envOf(map[string]string{"BRAID_HTTP_ENDPOINTS": "not json"}))
		if err == nil || !strings.Contains(err.Error(), "BRAID_HTTP_ENDPOINTS") {
			t.Fatalf("err = %v, want env-named rejection", err)
		}
	})

	t.Run("non-https base url rejected via env", func(t *testing.T) {
		_, err := BuiltinToolsFromEnv(envOf(map[string]string{
			"BRAID_HTTP_ENDPOINTS": `{"api":"http://a.test"}`,
		}))
		if err == nil || !strings.Contains(err.Error(), "must be https") {
			t.Fatalf("err = %v, want https rejection", err)
		}
	})

	t.Run("null endpoints rejected", func(t *testing.T) {
		_, err := BuiltinToolsFromEnv(envOf(map[string]string{"BRAID_HTTP_ENDPOINTS": "null"}))
		if err == nil || !strings.Contains(err.Error(), "empty endpoints") {
			t.Fatalf("err = %v, want empty rejection", err)
		}
	})

	t.Run("invalid dirs json", func(t *testing.T) {
		_, err := BuiltinToolsFromEnv(envOf(map[string]string{"BRAID_FILEREAD_DIRS": "42"}))
		if err == nil || !strings.Contains(err.Error(), "BRAID_FILEREAD_DIRS") {
			t.Fatalf("err = %v, want env-named rejection", err)
		}
	})

	t.Run("empty dirs array rejected", func(t *testing.T) {
		_, err := BuiltinToolsFromEnv(envOf(map[string]string{"BRAID_FILEREAD_DIRS": "[]"}))
		if err == nil || !strings.Contains(err.Error(), "empty dir allowlist") {
			t.Fatalf("err = %v, want empty rejection", err)
		}
	})

	t.Run("relative dir rejected via env", func(t *testing.T) {
		_, err := BuiltinToolsFromEnv(envOf(map[string]string{"BRAID_FILEREAD_DIRS": `["rel"]`}))
		if err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Fatalf("err = %v, want absolute rejection", err)
		}
	})

	t.Run("file_read alone when endpoints unset", func(t *testing.T) {
		ts, err := BuiltinToolsFromEnv(envOf(map[string]string{
			"BRAID_FILEREAD_DIRS": `["/var/braid/docs"]`,
		}))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(ts) != 1 || ts[0].Name() != "file_read" {
			t.Fatalf("tools = %v, want [file_read]", ts)
		}
	})
}

// TestBuiltinToolsFromEnvSandboxOffParity 钉死 off 逐字节 parity(T6 装配缝
// 的第一半):unset/off/空 × 沙箱干扰键在场 → 与无沙箱代码的现状完全
// 同型——具体 builtin 类型、同名、干扰键零消费(零 docker 代码路径)。
func TestBuiltinToolsFromEnvSandboxOffParity(t *testing.T) {
	toolEnv := func(extra map[string]string) map[string]string {
		env := map[string]string{
			"BRAID_HTTP_ENDPOINTS": `{"docs_api":"https://docs.example.com"}`,
			"BRAID_FILEREAD_DIRS":  `["/var/braid/docs"]`,
		}
		for k, v := range extra {
			env[k] = v
		}
		return env
	}
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"unset", toolEnv(nil)},
		{"explicit off", toolEnv(map[string]string{"BRAID_TOOL_SANDBOX": "off"})},
		{"empty mode equals unset", toolEnv(map[string]string{"BRAID_TOOL_SANDBOX": ""})},
		{"off ignores inert sandbox keys", toolEnv(map[string]string{
			"BRAID_TOOL_SANDBOX":        "off",
			"BRAID_TOOL_SANDBOX_IMAGE":  "--privileged", // off 模式零消费:坏值不报错
			"BRAID_TOOL_SANDBOX_MEMORY": "garbage",
		})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts, err := BuiltinToolsFromEnv(envOf(c.env))
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if len(ts) != 2 {
				t.Fatalf("tools = %d, want 2", len(ts))
			}
			if _, ok := ts[0].(*HTTPTool); !ok {
				t.Fatalf("tool[0] = %T, want *HTTPTool (off stays in-process)", ts[0])
			}
			if _, ok := ts[1].(*FileReadTool); !ok {
				t.Fatalf("tool[1] = %T, want *FileReadTool (off stays in-process)", ts[1])
			}
			if got := strings.Join(registryOf(t, ts...).Names(), ","); got != "file_read,http" {
				t.Fatalf("registry names = %q", got)
			}
		})
	}
}

// TestBuiltinToolsFromEnvSandboxDockerWrap 钉死装配缝第二半(T6):docker
// 模式同名换装 sandboxCarrier——Names() 恒等、单例信号量跨工具共享、
// toolConfig 词形与 helper 重建面同构、dirs 同源。
func TestBuiltinToolsFromEnvSandboxDockerWrap(t *testing.T) {
	ts, err := BuiltinToolsFromEnv(envOf(map[string]string{
		"BRAID_TOOL_SANDBOX":   "docker",
		"BRAID_HTTP_ENDPOINTS": `{"docs_api":"https://docs.example.com"}`,
		"BRAID_FILEREAD_DIRS":  `["/var/braid/docs"]`,
	}))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(ts) != 2 {
		t.Fatalf("tools = %d, want 2", len(ts))
	}
	hc, ok := ts[0].(*sandboxCarrier)
	if !ok {
		t.Fatalf("tool[0] = %T, want *sandboxCarrier", ts[0])
	}
	fc, ok := ts[1].(*sandboxCarrier)
	if !ok {
		t.Fatalf("tool[1] = %T, want *sandboxCarrier", ts[1])
	}
	// Names() 恒等:同名替换,发布白名单两模式一致。
	if hc.Name() != "http" || fc.Name() != "file_read" {
		t.Fatalf("names = %q/%q, want http/file_read", hc.Name(), fc.Name())
	}
	if got := strings.Join(registryOf(t, ts...).Names(), ","); got != "file_read,http" {
		t.Fatalf("registry names = %q", got)
	}
	// 装配期单例信号量:两载体共享同一 limiter(同一 daemon 并发预算)。
	if hc.limiter != fc.limiter {
		t.Fatal("carriers must share one limiter")
	}
	// cfg 传播(缺省档)与 docker 二进制缺省。
	if hc.cfg.Image != "braid-tool-sandbox:1" || hc.cfg.MaxConcurrency != 8 {
		t.Fatalf("carrier cfg = %+v, want pinned defaults", hc.cfg)
	}
	if hc.docker != "docker" || fc.docker != "docker" {
		t.Fatalf("carrier docker bin = %q/%q", hc.docker, fc.docker)
	}
	// toolConfig 词形:与 helper rebuildSandboxTool 消费面同构。
	if string(hc.toolConfig) != `{"endpoints":{"docs_api":"https://docs.example.com"}}` {
		t.Fatalf("http toolConfig = %s", hc.toolConfig)
	}
	if string(fc.toolConfig) != `{"dirs":["/var/braid/docs"]}` {
		t.Fatalf("file_read toolConfig = %s", fc.toolConfig)
	}
	// dirs 同源:白名单原值(未 Clean、未筛),http 恒空。
	if len(fc.dirs) != 1 || fc.dirs[0] != "/var/braid/docs" {
		t.Fatalf("carrier dirs = %v", fc.dirs)
	}
	if hc.dirs != nil {
		t.Fatalf("http carrier dirs = %v, want nil", hc.dirs)
	}
	// 信封原料可重建同名工具(装配缝契约闭环:容器内重建吃同一份词形)。
	if _, err := rebuildSandboxTool("http", hc.toolConfig); err != nil {
		t.Fatalf("rebuild http: %v", err)
	}
	if _, err := rebuildSandboxTool("file_read", fc.toolConfig); err != nil {
		t.Fatalf("rebuild file_read: %v", err)
	}
}

// TestBuiltinToolsFromEnvSandboxDockerValidation:docker 模式保留装配期
// 响亮值校验(与 off 同门——装配缺陷启动即现),坏沙箱键同样键名报错;
// 构造全程零 docker 接触(PATH 清空 = 结构锚,api docker-free)。
func TestBuiltinToolsFromEnvSandboxDockerValidation(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"non-https endpoint rejected in docker mode", map[string]string{
			"BRAID_TOOL_SANDBOX":   "docker",
			"BRAID_HTTP_ENDPOINTS": `{"api":"http://a.test"}`,
		}, "must be https"},
		{"relative dir rejected in docker mode", map[string]string{
			"BRAID_TOOL_SANDBOX":  "docker",
			"BRAID_FILEREAD_DIRS": `["rel"]`,
		}, "absolute"},
		{"invalid sandbox mode is loud at assembly", map[string]string{
			"BRAID_TOOL_SANDBOX": "DOCKER",
		}, "BRAID_TOOL_SANDBOX"},
		{"invalid image rejected in docker mode", map[string]string{
			"BRAID_TOOL_SANDBOX":       "docker",
			"BRAID_TOOL_SANDBOX_IMAGE": "-evil",
			"BRAID_HTTP_ENDPOINTS":     `{"api":"https://a.test"}`,
		}, "BRAID_TOOL_SANDBOX_IMAGE"},
		{"space dir rejected in docker mode at assembly", map[string]string{
			"BRAID_TOOL_SANDBOX":  "docker",
			"BRAID_FILEREAD_DIRS": `["/var/braid/my docs"]`,
		}, "docker mount value domain"},
		{"duplicate dir (clean collision) rejected at assembly", map[string]string{
			"BRAID_TOOL_SANDBOX":  "docker",
			"BRAID_FILEREAD_DIRS": `["/var/braid/docs", "/var/braid/docs/"]`,
		}, "duplicates earlier entry"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := BuiltinToolsFromEnv(envOf(c.env))
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, c.wantErr)
			}
		})
	}

	t.Run("docker mode without tool env registers nothing", func(t *testing.T) {
		ts, err := BuiltinToolsFromEnv(envOf(map[string]string{"BRAID_TOOL_SANDBOX": "docker"}))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(ts) != 0 {
			t.Fatalf("tools = %d, want 0", len(ts))
		}
	})

	t.Run("trailing slash dir passes mount precheck via clean form", func(t *testing.T) {
		// 预检取 Clean 后形态(mountDirs 同款变换):尾斜杠冗余词形不误拒
		// (基线 #3「同路径同构」);信封 config 与载体 dirs 保留原词形。
		ts, err := BuiltinToolsFromEnv(envOf(map[string]string{
			"BRAID_TOOL_SANDBOX":  "docker",
			"BRAID_FILEREAD_DIRS": `["/var/braid/docs/"]`,
		}))
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		fc, ok := ts[0].(*sandboxCarrier)
		if !ok {
			t.Fatalf("tool[0] = %T, want *sandboxCarrier", ts[0])
		}
		if len(fc.dirs) != 1 || fc.dirs[0] != "/var/braid/docs/" {
			t.Fatalf("dirs = %v, want raw word form", fc.dirs)
		}
		if string(fc.toolConfig) != `{"dirs":["/var/braid/docs/"]}` {
			t.Fatalf("toolConfig = %s, want raw word form", fc.toolConfig)
		}
	})

	t.Run("existing non-dir entry rejected at assembly", func(t *testing.T) {
		// 单文件白名单条目本地可服务(NewFileReadTool 无 IsDir 门),但
		// docker 只能 bind-mount 目录——在场且非目录 = 装配期拒;不存在
		// 条目放行(挂载晚于启动语义保留)。
		f := filepath.Join(t.TempDir(), "data.json")
		if err := os.WriteFile(f, []byte("{}"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		_, err := BuiltinToolsFromEnv(envOf(map[string]string{
			"BRAID_TOOL_SANDBOX":  "docker",
			"BRAID_FILEREAD_DIRS": `["` + f + `"]`,
		}))
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("err = %v, want non-dir rejection", err)
		}
	})

	t.Run("limiter capacity anchored to max concurrency env", func(t *testing.T) {
		// BRAID_TOOL_SANDBOX_MAX_CONCURRENCY → 信号量容量的最后一跳锚
		// (退化成硬编码 8 时红)。
		ts, err := BuiltinToolsFromEnv(envOf(map[string]string{
			"BRAID_TOOL_SANDBOX":                 "docker",
			"BRAID_TOOL_SANDBOX_MAX_CONCURRENCY": "16",
			"BRAID_FILEREAD_DIRS":                `["/var/braid/docs"]`,
		}))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		fc, ok := ts[0].(*sandboxCarrier)
		if !ok {
			t.Fatalf("tool[0] = %T, want *sandboxCarrier", ts[0])
		}
		if cap(fc.limiter.slots) != 16 {
			t.Fatalf("limiter capacity = %d, want 16", cap(fc.limiter.slots))
		}
	})

	t.Run("assembly is docker-free (lazy construction)", func(t *testing.T) {
		// PATH 清空:任何 LookPath/daemon 探测混进装配路径在此即红——
		// 构造纯配置是 api 进程 docker-free 的结构保证(Probe 归 worker)。
		t.Setenv("PATH", "")
		ts, err := BuiltinToolsFromEnv(envOf(map[string]string{
			"BRAID_TOOL_SANDBOX":   "docker",
			"BRAID_HTTP_ENDPOINTS": `{"docs_api":"https://docs.example.com"}`,
			"BRAID_FILEREAD_DIRS":  `["/var/braid/docs"]`,
		}))
		if err != nil {
			t.Fatalf("err = %v, want nil (construction must not resolve docker)", err)
		}
		if len(ts) != 2 {
			t.Fatalf("tools = %d, want 2", len(ts))
		}
	})
}
