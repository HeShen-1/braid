package tools

// db_query t13 栈集成测试(T10.4,off 模式真 MySQL):参数化绑定与数值
// 往返、行上限超限报错(GATE 1:不静默截断)、只读账号服务端拒写(纵深
// 第三层的实证)、客户端门先于服务端。栈不可达自动 skip;CI
// BRAID_TEST_REQUIRE_STACK=1 下不可达即红。schema 名带 worktree 后缀
// (T2.7 隔离),账号/表 IF NOT EXISTS 幂等(t13 tmpfs 契约:restart 即清)。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"braid/internal/db"
)

// dbqStackGate:验证 MySQL 可达性门(契约端口 127.0.0.1:13306;
// BRAID_TEST_MYSQL_DSN 覆盖时由 TestServerDSN 归一)。
func dbqStackGate(t *testing.T) string {
	t.Helper()
	dsn := db.TestServerDSN()
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse test server dsn: %v", err)
	}
	addr := cfg.Addr
	if _, _, serr := net.SplitHostPort(addr); serr != nil {
		addr = net.JoinHostPort(strings.Trim(addr, "[]"), "3306")
	}
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		if os.Getenv("BRAID_TEST_REQUIRE_STACK") == "1" {
			t.Fatalf("verify MySQL unreachable and BRAID_TEST_REQUIRE_STACK=1: %v", err)
		}
		t.Skipf("verify MySQL unreachable (self-provided stack or BRAID_TEST_MYSQL_DSN): %v", err)
	}
	_ = conn.Close()
	return dsn
}

// dbqStackFixture:测试 schema + 只读账号(GRANT SELECT-only,部署纪律的
// 测试内镜像)+ 5 行 fixture。返回工具用 ro DSN 与管理 DSN(层 2 锚
// 直连 root 用,承载账号/库;安全参数由 NewDBQueryTool 强制,fixture
// 不预设)。
func dbqStackFixture(t *testing.T) (roDSN, adminDSN string) {
	t.Helper()
	adminDSN = dbqStackGate(t)
	admin, err := sql.Open("mysql", adminDSN)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.Ping(); err != nil {
		t.Fatalf("ping admin: %v", err)
	}
	exec := func(q string) {
		t.Helper()
		if _, err := admin.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	schema := db.TestSchema("tools_dbquery")
	exec("CREATE DATABASE IF NOT EXISTS `" + schema + "`")
	const roUser, roPass = "braid_ro_t104", "ro_pw_t104"
	exec(fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'", roUser, roPass))
	exec(fmt.Sprintf("GRANT SELECT ON `%s`.* TO '%s'@'%%'", schema, roUser))
	exec("CREATE TABLE IF NOT EXISTS `" + schema + "`.t (" +
		"id BIGINT PRIMARY KEY, name VARCHAR(64), price DECIMAL(10,2), flag BOOLEAN, note TEXT)")
	exec("DELETE FROM `" + schema + "`.t")
	exec("INSERT INTO `" + schema + "`.t (id, name, price, flag, note) VALUES " +
		"(1,'alpha',10.50,TRUE,NULL)," +
		"(2,'O''Brien',3.25,FALSE,'has '' quote')," +
		"(3,'gamma',0.99,TRUE,'plain')," +
		"(4,'delta',1.00,FALSE,NULL)," +
		"(9007199254740993,'big',2.00,TRUE,'precision')")

	rcfg, err := mysql.ParseDSN(adminDSN)
	if err != nil {
		t.Fatalf("parse admin dsn for ro derivation: %v", err)
	}
	rcfg.User, rcfg.Passwd, rcfg.DBName = roUser, roPass, schema
	rcfg.Timeout = 5 * time.Second
	return rcfg.FormatDSN(), adminDSN
}

// dbqRun 小助手:跑一次工具并解组输出。
func dbqRun(t *testing.T, tool *DBQueryTool, sqlText string, args []any) (map[string]any, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := tool.Run(ctx, Call{Inputs: map[string]any{"sql": sqlText, "args": args}})
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(res.Output)))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("unmarshal output %s: %v", res.Output, err)
	}
	return out, nil
}

func TestDBQueryIntegration(t *testing.T) {
	dsn, adminDSN := dbqStackFixture(t)
	tool, err := NewDBQueryTool(DBQueryConfig{DSN: dsn})
	if err != nil {
		t.Fatalf("NewDBQueryTool: %v", err)
	}

	t.Run("parameterized select with quote and precision round-trip", func(t *testing.T) {
		out, err := dbqRun(t, tool, "SELECT id, name, price, flag, note FROM t WHERE id = ?", []any{json.Number("2")})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if out["row_count"].(json.Number).String() != "1" {
			t.Fatalf("row_count = %v, want 1", out["row_count"])
		}
		rows := out["rows"].([]any)
		row := rows[0].([]any)
		// O'Brien 与带引号 note:值经占位符绑定,引号在值内无害(参数化
		// 强制的存在性证明)。
		if row[1] != "O'Brien" || row[4] != "has ' quote" {
			t.Fatalf("row = %v, want name=O'Brien note=has ' quote", row)
		}
		// DECIMAL → 字符串词形;BOOLEAN = TINYINT(1) → 数值 0/1(MySQL
		// 语义,非 JSON 布尔);NULL 原样。
		if row[2] != "3.25" {
			t.Fatalf("price = %v (%T), want \"3.25\" string", row[2], row[2])
		}
		if got := row[3].(json.Number).String(); got != "0" {
			t.Fatalf("flag = %v, want 0 (BOOLEAN = TINYINT numeric)", row[3])
		}
	})

	t.Run("big integer exact round-trip beyond float53", func(t *testing.T) {
		out, err := dbqRun(t, tool, "SELECT id FROM t WHERE name = ?", []any{"big"})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		rows := out["rows"].([]any)
		if got := rows[0].([]any)[0].(json.Number).String(); got != "9007199254740993" {
			t.Fatalf("id = %s, want exact 9007199254740993", got)
		}
	})

	t.Run("in-list parameterized", func(t *testing.T) {
		out, err := dbqRun(t, tool, "SELECT id FROM t WHERE id IN (?, ?) ORDER BY id", []any{json.Number("2"), json.Number("3")})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if out["row_count"].(json.Number).String() != "2" {
			t.Fatalf("row_count = %v, want 2", out["row_count"])
		}
	})

	t.Run("zero rows is empty result not error", func(t *testing.T) {
		out, err := dbqRun(t, tool, "SELECT id FROM t WHERE id = ?", []any{json.Number("999999")})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if out["row_count"].(json.Number).String() != "0" {
			t.Fatalf("row_count = %v, want 0", out["row_count"])
		}
		if len(out["rows"].([]any)) != 0 {
			t.Fatalf("rows = %v, want empty", out["rows"])
		}
	})

	t.Run("row cap rejects without truncation", func(t *testing.T) {
		capped, err := NewDBQueryTool(DBQueryConfig{DSN: dsn, MaxRows: 2})
		if err != nil {
			t.Fatalf("NewDBQueryTool: %v", err)
		}
		_, err = dbqRun(t, capped, "SELECT id FROM t", nil)
		if err == nil || !strings.Contains(err.Error(), "exceeds 2 rows") {
			t.Fatalf("err = %v, want exceeds 2 rows (loud rejection, no silent truncation)", err)
		}
	})

	t.Run("byte budget enforced during scan", func(t *testing.T) {
		// orch-review MEDIUM 收口锚:单行巨值(REPEAT 十六进制字面量形态,
		// 过参数化门——无引号)在扫描期即被字节预算拒,不先全量驻留。
		tiny, err := NewDBQueryTool(DBQueryConfig{DSN: dsn, MaxOutputBytes: 4096})
		if err != nil {
			t.Fatalf("NewDBQueryTool: %v", err)
		}
		_, err = dbqRun(t, tiny, "SELECT REPEAT(0x41, 10000) AS payload", nil)
		if err == nil || !strings.Contains(err.Error(), "exceeds 4096 bytes") {
			t.Fatalf("err = %v, want scan-time byte budget rejection", err)
		}
	})

	t.Run("client gate rejects before server", func(t *testing.T) {
		_, err := dbqRun(t, tool, "INSERT INTO t (id) VALUES (?)", []any{json.Number("9")})
		if err == nil || !strings.Contains(err.Error(), "DML/admin verb rejected") {
			t.Fatalf("err = %v, want client-side read-only vocabulary rejection", err)
		}
	})

	t.Run("placeholder count mismatch is server-side failure", func(t *testing.T) {
		_, err := dbqRun(t, tool, "SELECT id FROM t WHERE id = ? AND name = ?", []any{json.Number("1")})
		if err == nil {
			t.Fatal("mismatched args accepted")
		}
	})

	t.Run("layer2 readonly transaction blocks with-prefixed dml", func(t *testing.T) {
		// 层 2 锚(orch-review MEDIUM 收口):绕开工具层 1 门与 ro 账号
		// 层 3,直连 root + dbqDSNConfig 强制参数,证 transaction_read_only=1
		// 在服务端真实执法(1792/25006)——层 1 被绕过的世界里承重层的
		// 唯一回归锚;数据零损失同点断言。
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		mc, err := dbqDSNConfig(adminDSN)
		if err != nil {
			t.Fatalf("dbqDSNConfig(admin): %v", err)
		}
		conn, err := mysql.NewConnector(mc)
		if err != nil {
			t.Fatalf("NewConnector: %v", err)
		}
		root := sql.OpenDB(conn)
		defer func() { _ = root.Close() }()
		schema := db.TestSchema("tools_dbquery")
		if _, err := root.ExecContext(ctx,
			"WITH c AS (SELECT 1 AS id) DELETE FROM `"+schema+"`.t WHERE id = ?", 1); err == nil {
			t.Fatal("CTE-prefixed DELETE on root connection accepted: transaction_read_only enforcement missing")
		} else if !strings.Contains(err.Error(), "read-only") && !strings.Contains(err.Error(), "READ ONLY") {
			t.Fatalf("err = %v, want read-only transaction rejection (1792)", err)
		}
		out, err := dbqRun(t, tool, "SELECT COUNT(*) AS n FROM t", nil)
		if err != nil {
			t.Fatalf("count after denied delete: %v", err)
		}
		if got := out["rows"].([]any)[0].([]any)[0].(json.Number).String(); got != "5" {
			t.Fatalf("row count = %s after denied delete, want 5 (data intact)", got)
		}
	})

	t.Run("readonly account posture server-enforced", func(t *testing.T) {
		// 纵深第三层实证:即便绕过客户端门,账号也无写权(GRANT SELECT-only)。
		ro, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Fatalf("open ro: %v", err)
		}
		defer func() { _ = ro.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		schema := db.TestSchema("tools_dbquery")
		if _, err := ro.ExecContext(ctx, "INSERT INTO `"+schema+"`.t (id) VALUES (?)", 100); err == nil {
			t.Fatal("ro account INSERT accepted: GRANT SELECT-only posture broken")
		}
	})

	t.Run("canceled ctx propagates", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := tool.Run(ctx, Call{Inputs: map[string]any{"sql": "SELECT id FROM t"}})
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}
