package tools

// db_query 装配矩阵测试(T10.4):env 路径(BRAID_DBQUERY_DSN 在场即声明,
// 缺席不注册)、docker 换装(载体词形 {"dsn","proxy"} 与 helper 重建面
// 同构)、签名清单路径(声明 presence-only,DSN 恒 env 通道)。

import (
	"strings"
	"testing"
)

const dbqTestDSN = "ro:pw@tcp(127.0.0.1:13306)/braid_test"

func TestBuiltinToolsFromEnvDBQuery(t *testing.T) {
	t.Run("off mode registers in-process tool", func(t *testing.T) {
		ts, err := BuiltinToolsFromEnv(envOf(map[string]string{
			"BRAID_DBQUERY_DSN": dbqTestDSN,
		}))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(ts) != 1 || ts[0].Name() != "db_query" {
			t.Fatalf("tools = %v, want [db_query]", ts)
		}
		if _, ok := ts[0].(*DBQueryTool); !ok {
			t.Fatalf("tool[0] = %T, want *DBQueryTool", ts[0])
		}
	})

	t.Run("absent dsn registers nothing", func(t *testing.T) {
		ts, err := BuiltinToolsFromEnv(envOf(map[string]string{}))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(ts) != 0 {
			t.Fatalf("tools = %d, want 0", len(ts))
		}
	})

	t.Run("bad dsn loud with key attribution, no credential echo", func(t *testing.T) {
		_, err := BuiltinToolsFromEnv(envOf(map[string]string{
			"BRAID_DBQUERY_DSN": "ro:topsecret@tcp(bad",
		}))
		if err == nil || !strings.Contains(err.Error(), "BRAID_DBQUERY_DSN") {
			t.Fatalf("err = %v, want BRAID_DBQUERY_DSN attribution", err)
		}
		if strings.Contains(err.Error(), "topsecret") {
			t.Fatalf("err leaks credential: %v", err)
		}
	})

	t.Run("docker mode wraps carrier with dsn envelope", func(t *testing.T) {
		ts, err := BuiltinToolsFromEnv(envOf(map[string]string{
			"BRAID_TOOL_SANDBOX": "docker",
			"BRAID_DBQUERY_DSN":  dbqTestDSN,
		}))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(ts) != 1 {
			t.Fatalf("tools = %d, want 1", len(ts))
		}
		dc, ok := ts[0].(*sandboxCarrier)
		if !ok {
			t.Fatalf("tool[0] = %T, want *sandboxCarrier", ts[0])
		}
		if dc.Name() != "db_query" {
			t.Fatalf("name = %q, want db_query", dc.Name())
		}
		// api 侧装配恒无 EGRESS_ADDR → 信封不带 proxy 字段(off 形态词形锚)。
		if string(dc.toolConfig) != `{"dsn":"`+dbqTestDSN+`"}` {
			t.Fatalf("db_query toolConfig = %s", dc.toolConfig)
		}
		if dc.dirs != nil {
			t.Fatalf("db_query carrier dirs = %v, want nil", dc.dirs)
		}
		if _, err := rebuildSandboxTool("db_query", dc.toolConfig); err != nil {
			t.Fatalf("rebuild db_query: %v", err)
		}
	})

	t.Run("docker mode with egress addr carries proxy word form", func(t *testing.T) {
		ts, err := BuiltinToolsFromEnv(envOf(map[string]string{
			"BRAID_TOOL_SANDBOX":             "docker",
			"BRAID_DBQUERY_DSN":              dbqTestDSN,
			"BRAID_TOOL_SANDBOX_EGRESS_ADDR": "http://127.0.0.1:9210",
		}))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		dc := ts[0].(*sandboxCarrier)
		want := `{"dsn":"` + dbqTestDSN + `","proxy":"http://127.0.0.1:9210"}`
		if string(dc.toolConfig) != want {
			t.Fatalf("db_query toolConfig = %s, want %s", dc.toolConfig, want)
		}
	})

	t.Run("three tools in docker mode share one limiter", func(t *testing.T) {
		ts, err := BuiltinToolsFromEnv(envOf(map[string]string{
			"BRAID_TOOL_SANDBOX":   "docker",
			"BRAID_HTTP_ENDPOINTS": `{"docs_api":"https://docs.example.com"}`,
			"BRAID_FILEREAD_DIRS":  `["/var/braid/docs"]`,
			"BRAID_DBQUERY_DSN":    dbqTestDSN,
		}))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(ts) != 3 {
			t.Fatalf("tools = %d, want 3", len(ts))
		}
		if got := strings.Join(registryOf(t, ts...).Names(), ","); got != "db_query,file_read,http" {
			t.Fatalf("names = %q", got)
		}
		for _, tool := range ts {
			c := tool.(*sandboxCarrier)
			if c.limiter != ts[0].(*sandboxCarrier).limiter {
				t.Fatal("carriers must share one limiter")
			}
		}
	})
}

// TestBuiltinToolsFromEnvSignedPackageDBQuery:签名态 db_query 语义——清单
// 声明 presence-only,DSN 恒 env(凭证不进签名工件);声明而 DSN 缺席 =
// 半配置响亮拒(键名归因);DSN 在场而清单未声明 = 不注册(清单是签名态
// 唯一工具来源,凭证通道不是声明通道)。
func TestBuiltinToolsFromEnvSignedPackageDBQuery(t *testing.T) {
	const dbqManifest = `{"version":1,"tools":{"db_query":{}}}`

	t.Run("declared with env dsn registers", func(t *testing.T) {
		env := signedToolPkgEnv(t, dbqManifest, map[string]string{
			"BRAID_DBQUERY_DSN": dbqTestDSN,
		})
		ts, err := BuiltinToolsFromEnv(envOf(env))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(ts) != 1 || ts[0].Name() != "db_query" {
			t.Fatalf("tools = %v, want [db_query]", ts)
		}
	})

	t.Run("declared without env dsn is loud half-configuration", func(t *testing.T) {
		env := signedToolPkgEnv(t, dbqManifest, nil)
		_, err := BuiltinToolsFromEnv(envOf(env))
		if err == nil || !strings.Contains(err.Error(), "empty DSN") {
			t.Fatalf("err = %v, want empty DSN rejection", err)
		}
		if !strings.Contains(err.Error(), "BRAID_TOOL_PACKAGE") {
			t.Fatalf("err = %v, want BRAID_TOOL_PACKAGE attribution", err)
		}
	})

	t.Run("dsn without manifest declaration registers nothing", func(t *testing.T) {
		env := signedToolPkgEnv(t, `{"version":1,"tools":{}}`, map[string]string{
			"BRAID_DBQUERY_DSN": dbqTestDSN,
		})
		ts, err := BuiltinToolsFromEnv(envOf(env))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(ts) != 0 {
			t.Fatalf("tools = %d, want 0 (manifest is the sole declaration source)", len(ts))
		}
	})

	t.Run("manifest db_query carrying config fields rejected", func(t *testing.T) {
		env := signedToolPkgEnv(t,
			`{"version":1,"tools":{"db_query":{"endpoints":{"a":"https://a.test"}}}}`, nil)
		_, err := BuiltinToolsFromEnv(envOf(env))
		if err == nil || !strings.Contains(err.Error(), "presence-only") {
			t.Fatalf("err = %v, want presence-only rejection", err)
		}
	})
}

// TestRebuildSandboxToolDBQuery:helper 重建面——好词形重建同构工具,坏
// proxy 词形响亮拒(信封可观测 tool_failed,不 panic)。
func TestRebuildSandboxToolDBQuery(t *testing.T) {
	tool, err := rebuildSandboxTool("db_query", []byte(`{"dsn":"`+dbqTestDSN+`","proxy":"http://127.0.0.1:9210"}`))
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	dt, ok := tool.(*DBQueryTool)
	if !ok {
		t.Fatalf("rebuilt = %T, want *DBQueryTool", tool)
	}
	if dt.cfg.DialFunc == nil {
		t.Fatal("rebuilt with proxy must install tunnel dial func")
	}

	if _, err := rebuildSandboxTool("db_query", []byte(`{"dsn":"`+dbqTestDSN+`","proxy":"ftp://bad"}`)); err == nil {
		t.Fatal("bad proxy word form accepted")
	}
	if _, err := rebuildSandboxTool("db_query", []byte(`{}`)); err == nil {
		t.Fatal("empty dsn accepted at rebuild")
	}
}
