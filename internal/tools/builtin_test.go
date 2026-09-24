package tools

// 内置工具装配测试(T3.5):BuiltinToolsFromEnv 是 api 与 worker 的单源
// 装配缝——env 契约、未配置即不注册、坏配置响亮败。

import (
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
