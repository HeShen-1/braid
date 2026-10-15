package sqltest

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/go-sql-driver/mysql"

	"braid/internal/db"
)

var componentAccountSeq atomic.Uint32

// MustComponentDB 在 admin 指向的隔离测试库建立组件账号。权限只从
// deploy/grants.sh 原文回放(仅替换库名/账号名,含列级授权),不能在业务
// 回归里额外 GRANT 掩盖交付缺口。admin 仅用于 fixture 与清理。
func MustComponentDB(t TB, admin *sql.DB, component string) *sql.DB {
	t.Helper()
	switch component {
	case "api", "worker", "relay", "reaper":
	default:
		t.Fatalf("unknown test component %q", component)
	}
	var schema string
	if err := admin.QueryRow(`SELECT DATABASE()`).Scan(&schema); err != nil {
		t.Fatalf("read component test schema: %v", err)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_]+$`).MatchString(schema) || !strings.HasSuffix(schema, db.TestSchema("")) {
		t.Fatalf("component grants require an isolated test schema, got %q", schema)
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("locate grants.sh: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("locate grants.sh: go.mod not found")
		}
		dir = parent
	}
	raw, err := os.ReadFile(filepath.Join(dir, "deploy", "grants.sh"))
	if err != nil {
		t.Fatalf("read grants.sh: %v", err)
	}
	name := fmt.Sprintf("t232_%s_%s_%d", component, db.WorktreeKey(), componentAccountSeq.Add(1))
	account := "'" + name + "'@'%'"
	base := "'braid_" + component + "'@'%'"
	const password = "braid_component_test_pw" // 测试占位值,非真实凭证
	mustExec := func(stmt string) {
		t.Helper()
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("component account fixture: %v", err)
		}
	}
	mustExec("DROP USER IF EXISTS " + account)
	t.Cleanup(func() { _, _ = admin.Exec("DROP USER IF EXISTS " + account) })
	mustExec("CREATE USER " + account + " IDENTIFIED BY '" + password + "'")
	var grants int
	for _, line := range strings.Split(string(raw), "\n") {
		stmt := strings.TrimSpace(strings.SplitN(line, "--", 2)[0])
		if !strings.HasPrefix(stmt, "GRANT ") || !strings.Contains(stmt, " TO "+base) {
			continue
		}
		stmt = strings.Replace(stmt, " ON braid.", " ON `"+schema+"`.", 1)
		stmt = strings.Replace(stmt, base, account, 1)
		mustExec(stmt)
		grants++
	}
	if grants == 0 {
		t.Fatalf("grants.sh has no grants for %s", component)
	}
	cfg, err := mysql.ParseDSN(db.TestServerDSN())
	if err != nil {
		t.Fatalf("parse component test DSN: %v", err)
	}
	cfg.User, cfg.Passwd, cfg.DBName = name, password, schema
	limited, err := sql.Open("mysql", db.CanonicalDSN(cfg.FormatDSN()))
	if err != nil {
		t.Fatalf("open component account: %v", err)
	}
	t.Cleanup(func() { _ = limited.Close() })
	var identity string
	if err := limited.QueryRow(`SELECT CURRENT_USER()`).Scan(&identity); err != nil || identity != name+"@%" {
		t.Fatalf("component account identity = %q, err = %v", identity, err)
	}
	return limited
}
