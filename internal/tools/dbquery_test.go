package tools

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// TestDBQueryDSNConfig 验证 DSN 解析与安全参数强制(T10.4):运营方 DSN
// 里的危险旋钮(interpolateParams/multiStatements/transaction_read_only)
// 一律被结构覆盖——参数化与只读是工具契约,不是配置选项。凭证红线:解析
// 失败的错误不得回显 DSN(含密码)。
func TestDBQueryDSNConfig(t *testing.T) {
	cases := []struct {
		name    string
		dsn     string
		wantErr string // 非空 = 期望错误包含此子串
		check   func(t *testing.T, cfg *mysql.Config)
	}{
		{
			name: "plain tcp dsn forced safe",
			dsn:  "ro:s3cr3t@tcp(127.0.0.1:13306)/braid_test",
			check: func(t *testing.T, cfg *mysql.Config) {
				if cfg.InterpolateParams {
					t.Errorf("interpolateParams must be forced false")
				}
				if cfg.MultiStatements {
					t.Errorf("multiStatements must be forced false")
				}
				if got := cfg.Params["transaction_read_only"]; got != "1" {
					t.Errorf("transaction_read_only = %q, want 1", got)
				}
				if cfg.TLSConfig != "" {
					t.Errorf("tls config unexpectedly set: %q", cfg.TLSConfig)
				}
			},
		},
		{
			name: "operator unsafe knobs overridden",
			dsn:  "ro:s3cr3t@tcp(db.example:3306)/db?interpolateParams=true&multiStatements=true&transaction_read_only=0",
			check: func(t *testing.T, cfg *mysql.Config) {
				if cfg.InterpolateParams {
					t.Errorf("operator interpolateParams=true must be overridden to false")
				}
				if cfg.MultiStatements {
					t.Errorf("operator multiStatements=true must be overridden to false")
				}
				if got := cfg.Params["transaction_read_only"]; got != "1" {
					t.Errorf("operator transaction_read_only=0 must be overridden, got %q", got)
				}
			},
		},
		{
			name: "tls param preserved for operator choice",
			dsn:  "ro:s3cr3t@tcp(db.example:3306)/db?tls=skip-verify",
			check: func(t *testing.T, cfg *mysql.Config) {
				if cfg.TLSConfig != "skip-verify" {
					t.Errorf("tls = %q, want skip-verify (operator DSN preserved)", cfg.TLSConfig)
				}
			},
		},
		{
			name: "empty dbname tolerated",
			dsn:  "ro:s3cr3t@tcp(db.example:3306)/",
			check: func(t *testing.T, cfg *mysql.Config) {
				if cfg.DBName != "" {
					t.Errorf("dbname = %q, want empty tolerated", cfg.DBName)
				}
			},
		},
		{
			name:    "unix socket rejected",
			dsn:     "ro:s3cr3t@unix(/tmp/mysql.sock)/db",
			wantErr: "tcp",
		},
		{
			name:    "unparseable dsn rejected without password echo",
			dsn:     "ro:topsecret123@tcp(",
			wantErr: "invalid DSN",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := dbqDSNConfig(tc.dsn)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tc.wantErr)
				}
				if strings.Contains(err.Error(), "topsecret123") {
					t.Fatalf("error leaks DSN credential: %q", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("dbqDSNConfig: %v", err)
			}
			if tc.check != nil {
				tc.check(t, cfg)
			}
		})
	}
}

// TestDBQueryDSNHostPort 验证 host(:port) 抽取(egress 允许集派生与
// MySQL 拨号地址共用的归一口):无端口 = 缺省 3306;IPv6 字面量剥方括号。
func TestDBQueryDSNHostPort(t *testing.T) {
	cases := []struct {
		name     string
		addr     string
		wantHost string
		wantPort int
		wantErr  string
	}{
		{name: "host port explicit", addr: "db.example:13306", wantHost: "db.example", wantPort: 13306},
		{name: "no port defaults 3306", addr: "db.example", wantHost: "db.example", wantPort: 3306},
		{name: "ipv6 with port", addr: "[::1]:3306", wantHost: "::1", wantPort: 3306},
		{name: "ipv6 without port brackets stripped", addr: "[::1]", wantHost: "::1", wantPort: 3306},
		{name: "ipv4 literal", addr: "10.0.0.5:3307", wantHost: "10.0.0.5", wantPort: 3307},
		{name: "port out of range", addr: "db.example:70000", wantErr: "port"},
		{name: "empty host", addr: ":3306", wantErr: "host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, port, err := dbqDSNHostPort(tc.addr)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("dbqDSNHostPort: %v", err)
			}
			if host != tc.wantHost || port != tc.wantPort {
				t.Fatalf("got %s:%d, want %s:%d", host, port, tc.wantHost, tc.wantPort)
			}
		})
	}
}

// TestDBQuerySQLGate 验证参数化门(T10.4 验收:非参数化拒)。门是注入面
// 的结构拒——字符串字面量与注释是一切 SQL 注入的载体,数值字面量不是
// (数字词素不可能改变语句结构,且 LIMIT 场景在 MySQL 预处理里本就常以
// 字面量书写);fail-closed 优先于完备性:反引号标识符内的门字符按拒收
// (如 `a;b`),宁可误拒不可误放。
func TestDBQuerySQLGate(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		ok   bool
	}{
		{"select star", "SELECT 1", true},
		{"lowercase select", "select 1", true},
		{"parameterized where", "SELECT id, name FROM users WHERE tenant = ?", true},
		{"numeric literal documented ok", "SELECT * FROM t WHERE id = 5", true},
		{"limit literal", "SELECT * FROM t LIMIT 100", true},
		{"with cte", "WITH c AS (SELECT COUNT(*) AS n FROM t) SELECT n FROM c", true},
		{"with cte delete rejected", "WITH c AS (SELECT 1 AS id) DELETE FROM t WHERE id = ?", false},
		{"with cte update rejected", "WITH c AS (SELECT 1) UPDATE t SET a = ?", false},
		{"replace string function allowed", "SELECT REPLACE(name, 0x61, 0x62) FROM t", true},
		{"insert string function allowed", "SELECT INSERT(name, 1, 2, 0x62) FROM t", true},
		{"underscore join no dml false positive", "SELECT last_update, created_at FROM t", true},
		{"offset keyword no set false positive", "SELECT id FROM t ORDER BY id LIMIT 10 OFFSET 5", true},
		{"single trailing semicolon stripped", "SELECT 1 ;", true},
		{"newline tab formatting", "SELECT\n\tid\nFROM t\nWHERE id = ?", true},
		{"crlf tolerated", "SELECT id\r\nFROM t", true},
		{"backtick identifiers", "SELECT `id`, `name` FROM `t` WHERE `id` = ?", true},
		{"spaced double negative literal", "SELECT 5 - -3", true},

		{"empty", "", false},
		{"whitespace only", "   ", false},
		{"insert rejected", "INSERT INTO t VALUES (?)", false},
		{"update rejected", "UPDATE t SET a = ?", false},
		{"delete rejected", "DELETE FROM t WHERE id = ?", false},
		{"ddl rejected", "CREATE TABLE t (id INT)", false},
		{"show rejected", "SHOW TABLES", false},
		{"explain rejected", "EXPLAIN SELECT 1", false},
		{"set rejected", "SET @x = 1", false},
		{"use rejected", "USE other_db", false},
		{"head word must end", "SELECTED FROM t", false},
		{"mid semicolon", "SELECT 1; SELECT 2", false},
		{"double trailing semicolon", "SELECT 1;;", false},
		{"only semicolon", ";", false},
		{"single quote literal", "SELECT 'x'", false},
		{"double quote literal", `SELECT "x"`, false},
		{"escaped quote in literal", "SELECT 'it''s'", false},
		{"line comment", "SELECT 1 -- note", false},
		{"block comment", "SELECT /* c */ 1", false},
		{"hash comment", "SELECT 1 # note", false},
		{"into outfile", "SELECT * FROM t INTO OUTFILE '/tmp/x'", false},
		{"into dumpfile lowercase multiline", "SELECT * FROM t into\n\tdumpfile '/tmp/x'", false},
		{"nul byte", "SELECT 1\x00", false},
		{"del byte", "SELECT 1\x7f", false},
		{"vertical tab control", "SELECT 1\x0b", false},
		{"fail closed backtick semicolon", "SELECT `a;b` FROM t", false},
		{"fail closed backtick dash dash", "SELECT `a--b` FROM t", false},
		{"over length", "SELECT " + strings.Repeat("x", maxDBQuerySQLBytes), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := dbqSanitizeSQL(tc.sql)
			if tc.ok {
				if err != nil {
					t.Fatalf("dbqSanitizeSQL(%q) unexpected error: %v", tc.sql, err)
				}
				if got == "" {
					t.Fatalf("sanitized sql empty for valid input")
				}
				return
			}
			if err == nil {
				t.Fatalf("dbqSanitizeSQL(%q) = %q, want rejection", tc.sql, got)
			}
		})
	}
}

// TestDBQueryArgsNormalize 验证 args 归一:DSL/沙箱解码口径恒 json.Number
// (UseNumber 纪律),driver.Value 类型面只认标量;>2^53 整数经 int64 精确
// 往返(裸 float64 会失真)。嵌套结构响亮拒。
func TestDBQueryArgsNormalize(t *testing.T) {
	t.Run("scalar promotion", func(t *testing.T) {
		got, err := dbqNormalizeArgs([]any{
			json.Number("42"),
			json.Number("3.14"),
			json.Number("9007199254740993"), // 2^53+1:float64 失真边界
			json.Number("-7"),
			"str",
			true,
			nil,
			float64(1.5),
			int64(7),
			int(3),
		})
		if err != nil {
			t.Fatalf("dbqNormalizeArgs: %v", err)
		}
		want := []any{
			int64(42),
			float64(3.14),
			int64(9007199254740993),
			int64(-7),
			"str",
			true,
			nil,
			float64(1.5),
			int64(7),
			int64(3),
		}
		if len(got) != len(want) {
			t.Fatalf("got %d args, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("args[%d] = %#v (%T), want %#v (%T)", i, got[i], got[i], want[i], want[i])
			}
		}
	})
	t.Run("nil input is empty args", func(t *testing.T) {
		got, err := dbqNormalizeArgs(nil)
		if err != nil || len(got) != 0 {
			t.Fatalf("nil input: got %v, %v; want empty, nil", got, err)
		}
	})
	t.Run("non array rejected", func(t *testing.T) {
		if _, err := dbqNormalizeArgs("nope"); err == nil {
			t.Fatalf("non-array accepted")
		}
	})
	t.Run("object element rejected", func(t *testing.T) {
		if _, err := dbqNormalizeArgs([]any{map[string]any{"a": 1}}); err == nil {
			t.Fatalf("object element accepted")
		}
	})
	t.Run("nested array rejected", func(t *testing.T) {
		if _, err := dbqNormalizeArgs([]any{[]any{1}}); err == nil {
			t.Fatalf("nested array accepted")
		}
	})
	t.Run("arg count capped", func(t *testing.T) {
		tooMany := make([]any, maxDBQueryArgs+1)
		for i := range tooMany {
			tooMany[i] = json.Number("1")
		}
		if _, err := dbqNormalizeArgs(tooMany); err == nil {
			t.Fatalf("arg count above %d accepted", maxDBQueryArgs)
		}
	})
}

// TestNewDBQueryTool 构造器:空 DSN 拒;上限缺省/负值回缺省;Proxy 注入
// DialFunc(沙箱模式明文隧道),off 模式恒 nil。
func TestNewDBQueryTool(t *testing.T) {
	t.Run("empty dsn rejected", func(t *testing.T) {
		for _, dsn := range []string{"", "   "} {
			if _, err := NewDBQueryTool(DBQueryConfig{DSN: dsn}); err == nil {
				t.Fatalf("empty dsn %q accepted", dsn)
			}
		}
	})
	t.Run("bad dsn rejected", func(t *testing.T) {
		if _, err := NewDBQueryTool(DBQueryConfig{DSN: "://bad"}); err == nil {
			t.Fatalf("bad dsn accepted")
		}
	})
	t.Run("defaults and name", func(t *testing.T) {
		tool, err := NewDBQueryTool(DBQueryConfig{DSN: "ro:pw@tcp(127.0.0.1:13306)/db"})
		if err != nil {
			t.Fatalf("NewDBQueryTool: %v", err)
		}
		if tool.Name() != "db_query" {
			t.Fatalf("Name() = %q, want db_query", tool.Name())
		}
		if tool.maxRows != DefaultMaxDBQueryRows {
			t.Fatalf("maxRows = %d, want %d", tool.maxRows, DefaultMaxDBQueryRows)
		}
		if tool.maxOutput != DefaultMaxDBQueryOutputBytes {
			t.Fatalf("maxOutput = %d, want %d", tool.maxOutput, DefaultMaxDBQueryOutputBytes)
		}
		if tool.cfg.DialFunc != nil {
			t.Fatalf("DialFunc set without proxy (off mode must dial direct)")
		}
	})
	t.Run("negative limits fall back to defaults", func(t *testing.T) {
		tool, err := NewDBQueryTool(DBQueryConfig{DSN: "ro:pw@tcp(127.0.0.1:13306)/db", MaxRows: -5, MaxOutputBytes: -1})
		if err != nil {
			t.Fatalf("NewDBQueryTool: %v", err)
		}
		if tool.maxRows != DefaultMaxDBQueryRows || tool.maxOutput != DefaultMaxDBQueryOutputBytes {
			t.Fatalf("negative limits not defaulted: %d/%d", tool.maxRows, tool.maxOutput)
		}
	})
	t.Run("custom limits respected", func(t *testing.T) {
		tool, err := NewDBQueryTool(DBQueryConfig{DSN: "ro:pw@tcp(127.0.0.1:13306)/db", MaxRows: 3, MaxOutputBytes: 4096})
		if err != nil {
			t.Fatalf("NewDBQueryTool: %v", err)
		}
		if tool.maxRows != 3 || tool.maxOutput != 4096 {
			t.Fatalf("custom limits ignored: %d/%d", tool.maxRows, tool.maxOutput)
		}
	})
	t.Run("proxy installs dial func", func(t *testing.T) {
		u, err := url.Parse("http://127.0.0.1:9210")
		if err != nil {
			t.Fatal(err)
		}
		tool, err := NewDBQueryTool(DBQueryConfig{DSN: "ro:pw@tcp(127.0.0.1:13306)/db", Proxy: u})
		if err != nil {
			t.Fatalf("NewDBQueryTool with proxy: %v", err)
		}
		if tool.cfg.DialFunc == nil {
			t.Fatalf("DialFunc not installed with proxy set")
		}
	})
}
