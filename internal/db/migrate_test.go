package db

import (
	"database/sql"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// testBaseDSN 指向 deploy/migrate-test.docker-compose.yml 起的验证容器
// (docker compose -f deploy/migrate-test.docker-compose.yml -p braid-t13 up -d)。
// 凭证是该一次性本地容器的占位值,非任何真实系统凭证。schema 经
// TestSchema("braid") 拼 worktree 后缀(T2.7):脱离旧固定库名,并行
// worktree 互不互踩;server 级 DSN 与 sqltest 共用 BRAID_TEST_MYSQL_DSN 口。
var testBaseDSN = TestServerDSN() + TestSchema("braid")

// ensureTestSchema 懒建本包独立 schema:本包 DSN 直连带库名,库不存在时
// Ping 即「Unknown database」→ 会被误判成栈不可达而全量 skip(sqltest 的
// initSchema 在消费包,此处不 import 它防循环)。
func ensureTestSchema(t *testing.T) {
	t.Helper()
	admin, err := sql.Open("mysql", CanonicalDSN(TestServerDSN()))
	if err != nil {
		t.Skipf("mysql driver: %v", err)
	}
	defer func() { _ = admin.Close() }()
	if err := admin.Ping(); err != nil {
		t.Skipf("braid-t13-mysql 不可达,先起验证容器: %v", err)
	}
	if _, err := admin.Exec(
		`CREATE DATABASE IF NOT EXISTS ` + TestSchema("braid") +
			` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci`); err != nil {
		t.Fatalf("create %s: %v", TestSchema("braid"), err)
	}
}

func mustSQL(t *testing.T) *sql.DB {
	t.Helper()
	ensureTestSchema(t)
	d, err := sql.Open("mysql", CanonicalDSN(testBaseDSN))
	if err != nil {
		t.Skipf("mysql driver: %v", err)
	}
	if err := d.Ping(); err != nil {
		t.Skipf("braid-t13-mysql 不可达,先起验证容器: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func newMigrator(t *testing.T) *migrate.Migrate {
	t.Helper()
	ensureTestSchema(t)
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("migration source: %v", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, "mysql://"+CanonicalDSN(testBaseDSN))
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	return m
}

var engineTables = map[string]bool{
	"workflow_definition": true,
	"workflow_execution":  true,
	"node_execution":      true,
	"node_attempt":        true,
	"approval_decision":   true, // 0003(T4.2 审批决定事实表,引擎域)
	"outbox":              true,
	"audit_log":           true,
	"audit_pending":       true, // 0004(T6.1 审计异步批量 WAL 暂存表,引擎域)
}

// authTables:0002 起按域分 map 登记(T5.1)——后续新增域只加自己的 map,
// 不改写 engine map 的语义;assertTables 遍历全部域。
var authTables = map[string]bool{
	"users":     true,
	"user_role": true,
}

var allDomainTables = []map[string]bool{engineTables, authTables}

func tables(t *testing.T, d *sql.DB) map[string]bool {
	t.Helper()
	rows, err := d.Query("SHOW TABLES")
	if err != nil {
		t.Fatalf("show tables: %v", err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = true
	}
	return got
}

func assertTables(t *testing.T, d *sql.DB, wantAll bool) {
	t.Helper()
	got := tables(t, d)
	if g := got["schema_migrations"]; !g {
		t.Fatalf("schema_migrations 缺失: %v", got)
	}
	total := 0
	for _, domain := range allDomainTables {
		for name := range domain {
			if got[name] != wantAll {
				t.Fatalf("表 %s 存在=%v, 期望=%v; 全部: %v", name, got[name], wantAll, got)
			}
		}
		total += len(domain)
	}
	want := 1 // schema_migrations
	if wantAll {
		want = total + 1
	}
	if len(got) != want {
		t.Fatalf("表数量 %d ≠ 期望 %d: %v", len(got), want, got)
	}
}

// T1.3 验收核心:migrate up/down 幂等(up→up no-op→down→down no-op→up 复建),
// 且 up 后结构断言锁死审计修复不回退。
func TestMigrateUpDownIdempotent(t *testing.T) {
	d := mustSQL(t)
	m := newMigrator(t)

	// 基线:先清到 version 0(首次运行库为空时 ErrNoChange;容忍)
	if err := m.Down(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("baseline down: %v", err)
	}
	assertTables(t, d, false)

	// 第一遍 up:实际执行
	if err := m.Up(); err != nil {
		t.Fatalf("first up: %v", err)
	}
	assertTables(t, d, true)

	// 第二遍 up:必须 no-op
	if err := m.Up(); err != migrate.ErrNoChange {
		t.Fatalf("second up: want ErrNoChange, got %v", err)
	}

	// down:全拆
	if err := m.Down(); err != nil {
		t.Fatalf("down: %v", err)
	}
	assertTables(t, d, false)

	// 第二遍 down:必须 no-op
	if err := m.Down(); err != migrate.ErrNoChange {
		t.Fatalf("second down: want ErrNoChange, got %v", err)
	}

	// 复建(验收:up/down 可重复)
	if err := m.Up(); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	assertTables(t, d, true)
}

// 结构断言:分区数、审计修复关键列(NOT NULL)、CHECK 约束、对账索引。
func TestSchemaContract(t *testing.T) {
	d := mustSQL(t)
	m := newMigrator(t)
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("up: %v", err)
	}

	var parts int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM information_schema.PARTITIONS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'audit_log'`).Scan(&parts); err != nil {
		t.Fatalf("partition count: %v", err)
	}
	if parts != 15 { // p_min + 2026-09-21..2026-10-04
		t.Fatalf("audit_log partitions = %d, want 15", parts)
	}

	// 对抗审计修复的防回退锚:heartbeat_at / asynq_task_id 必为 NOT NULL
	rows, err := d.Query(
		`SELECT COLUMN_NAME, IS_NULLABLE FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'node_attempt'
		   AND COLUMN_NAME IN ('heartbeat_at', 'asynq_task_id')`)
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	defer func() { _ = rows.Close() }()
	nullable := map[string]string{}
	for rows.Next() {
		var name, isNil string
		if err := rows.Scan(&name, &isNil); err != nil {
			t.Fatalf("scan: %v", err)
		}
		nullable[name] = isNil
	}
	for _, col := range []string{"heartbeat_at", "asynq_task_id"} {
		if nullable[col] != "NO" {
			t.Fatalf("node_attempt.%s IS_NULLABLE=%q, want NO(对抗审计修复防回退)", col, nullable[col])
		}
	}

	// CHECK 约束齐备:8 个(definition 1 + node 2 + attempt 2 + audit 1 + users 2)
	var checks int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM information_schema.CHECK_CONSTRAINTS
		 WHERE CONSTRAINT_SCHEMA = DATABASE()`).Scan(&checks); err != nil {
		t.Fatalf("check constraints: %v", err)
	}
	if checks != 8 {
		t.Fatalf("check constraints = %d, want 8", checks)
	}

	// T6.1 对账索引存在
	var idx int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'audit_log'
		   AND INDEX_NAME = 'idx_audit_attempt'`).Scan(&idx); err != nil {
		t.Fatalf("idx_audit_attempt: %v", err)
	}
	if idx != 1 {
		t.Fatalf("idx_audit_attempt missing")
	}

	// 0003 复合 FK 载体键(对抗审计 LOW 修防回退):approval_decision 的
	// (node_execution_id, execution_id) 复合外键依赖本键把「同链配对」钉死
	// 为 DB 不变量——键被误删时复合 FK 建不起来,迁移本身会红;此断言钉住
	// 键形态不被无声降级为普通索引。
	var ukCols int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'node_execution'
		   AND INDEX_NAME = 'uk_node_id_exec' AND NON_UNIQUE = 0`).Scan(&ukCols); err != nil {
		t.Fatalf("uk_node_id_exec: %v", err)
	}
	if ukCols != 2 {
		t.Fatalf("uk_node_id_exec columns = %d, want 2 (id, execution_id)", ukCols)
	}
}

func TestCanonicalDSN(t *testing.T) {
	base := "root:p@tcp(127.0.0.1:3306)/braid"
	want := base + "?loc=UTC&parseTime=true&multiStatements=true&time_zone=%27%2B00%3A00%27"
	if got := CanonicalDSN(base); got != want {
		t.Fatalf("CanonicalDSN = %q, want %q", got, want)
	}
	if got := CanonicalDSN(want); got != want {
		t.Fatalf("CanonicalDSN 幂等失败: %q", got)
	}
	// 逐参数补齐:只补缺的,不覆盖调用方显式给的
	partial := "root:p@tcp(127.0.0.1:3306)/braid?loc=Local"
	wantPartial := partial + "&parseTime=true&multiStatements=true&time_zone=%27%2B00%3A00%27"
	if got := CanonicalDSN(partial); got != wantPartial {
		t.Fatalf("部分参数 DSN: got %q, want %q", got, wantPartial)
	}
}

// 连接契约钉死:会话时区必须 '+00:00'。DATETIME 存字面值、NOW(3) 按会话时区求值
// (DDL 文件头);缺此断言时「非 UTC 的 mysqld 上库端时间全体漂移」仍全绿——
// T2.1 对抗审计发现该契约此前只靠容器系统时区碰巧 UTC 成立。
func TestConnectionContractTimeZone(t *testing.T) {
	d := mustSQL(t)
	var tz string
	if err := d.QueryRow("SELECT @@session.time_zone").Scan(&tz); err != nil {
		t.Fatalf("session time_zone: %v", err)
	}
	if tz != "+00:00" {
		t.Fatalf("@@session.time_zone=%q, want '+00:00'", tz)
	}
	var eq int
	if err := d.QueryRow("SELECT NOW(3) = UTC_TIMESTAMP(3)").Scan(&eq); err != nil {
		t.Fatalf("now vs utc: %v", err)
	}
	if eq != 1 {
		t.Fatalf("NOW(3) ≠ UTC_TIMESTAMP(3):会话时区违约,库端时间非 UTC")
	}
}
