package db

import (
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// testBaseDSN 指向 deploy/migrate-test.docker-compose.yml 起的验证容器
// (docker compose -f deploy/migrate-test.docker-compose.yml -p braid-t13 up -d)。
// 凭证是该一次性本地容器的占位值,非任何真实系统凭证。schema 经
// TestSchema("braid") 拼 worktree 后缀(T2.7):脱离旧固定库名,并行
// worktree 互不互踩;server 级 DSN 与 sqltest 共用 BRAID_TEST_MYSQL_DSN 口。
var testBaseDSN = TestServerDSN() + TestSchema("braid")

// skipOrFatalStack:栈不可达时按环境分流——本地默认 Skipf,CI
// (BRAID_TEST_REQUIRE_STACK=1)Fatalf(sqltest.MustOpenAt 同口径;本包不
// import sqltest 防循环,T8.1 审计 MEDIUM 修——「CI 绿 = 集成测试真跑」
// 的覆盖缺口)。
func skipOrFatalStack(t *testing.T, what string, err error) {
	t.Helper()
	if os.Getenv("BRAID_TEST_REQUIRE_STACK") == "1" {
		t.Fatalf("%s 不可达(REQUIRE_STACK=1,栈必达断言): %v", what, err)
	}
	t.Skipf("%s 不可达,先起验证容器: %v", what, err)
}

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
		skipOrFatalStack(t, "braid-t13-mysql", err)
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
		skipOrFatalStack(t, "braid-t13-mysql", err)
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
	"chain_heads":         true, // 0008(T9.3 审计哈希链 per-execution 锚,可 UPDATE 簿记面)
}

// authTables:0002 起按域分 map 登记(T5.1)——后续新增域只加自己的 map,
// 不改写 engine map 的语义;assertTables 遍历全部域。
var authTables = map[string]bool{
	"users":          true,
	"user_role":      true,
	"login_audit":    true, // 0005(T6.4 登录审计,append-only 账号域事实)
	"login_throttle": true, // 0010(T11.3 登录防御 DB 锚定,可 UPDATE 簿记面——镜像 chain_heads 判)
}

// tenantTables:0011 起租户域(T11.2,D-2026-09-30-t112):tenants 单表
// (归属锚 + 平台哨兵 id=0);行级隔离的冗余 tenant_id 列落在各域既有表
// (users/workflow_definition/workflow_execution/audit_log/audit_pending),
// 不为本域加第二张表。
var tenantTables = map[string]bool{
	"tenants": true, // 0011(T11.2 租户模型)
}

// instanceTables:0012 起实例注册域(T15.2,D-2026-10-01-t151 决策 2):
// 雪花节点租约单表 = 节点号仲裁 + 实例身份/capabilities 注册面
// (node_id 0..1023 全局单池,api/worker/reaper 共用;relay 无生成器不参与)。
var instanceTables = map[string]bool{
	"snowflake_node_lease": true, // 0012(T15.2 雪花租约)
}

var allDomainTables = []map[string]bool{engineTables, authTables, tenantTables, instanceTables}

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

	// 0010 login_throttle 结构锚(每迁移防回退惯例,审计 LOW):PK 单列于
	// throttle_key 且 janitor 谓词索引在场。
	var pkCols, idxCnt int
	if err := d.QueryRow(`
SELECT COUNT(*) FROM information_schema.KEY_COLUMN_USAGE
 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'login_throttle'
   AND CONSTRAINT_NAME = 'PRIMARY'`).Scan(&pkCols); err != nil {
		t.Fatalf("login_throttle pk: %v", err)
	}
	if err := d.QueryRow(`
SELECT COUNT(*) FROM information_schema.STATISTICS
 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'login_throttle'
   AND INDEX_NAME = 'idx_login_throttle_updated'`).Scan(&idxCnt); err != nil {
		t.Fatalf("login_throttle idx: %v", err)
	}
	if pkCols != 1 || idxCnt != 1 {
		t.Fatalf("login_throttle 锚:pk cols=%d(Composite? want 1)、idx=%d(want 1)", pkCols, idxCnt)
	}

	// 0011 租户模型锚(T11.2,D-2026-09-30-t112 防回退):平台哨兵行在场;
	// 五表 tenant_id NOT NULL DEFAULT 0(存量回填 0 = 平台租户,既有路径
	// 迁移后即绿——分项 commit 各自独立绿的锚);definition 租户键三列化
	// + execution 租户列表索引;旧全局键退役(旧键在场 = 租户撞名闸缺失,
	// 静默降级面)。
	var seedSlug string
	if err := d.QueryRow(`SELECT slug FROM tenants WHERE id = 0`).Scan(&seedSlug); err != nil {
		t.Fatalf("tenants 平台哨兵行(id=0)缺(存量归属锚,D-t112): %v", err)
	}
	if seedSlug != "platform" {
		t.Fatalf("tenants id=0 slug = %q, want platform", seedSlug)
	}
	for _, tbl := range []string{"users", "workflow_definition", "workflow_execution", "audit_log", "audit_pending"} {
		var nullable, dflt string
		if err := d.QueryRow(`
SELECT IS_NULLABLE, COLUMN_DEFAULT FROM information_schema.COLUMNS
 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
   AND COLUMN_NAME = 'tenant_id'`, tbl).Scan(&nullable, &dflt); err != nil {
			t.Fatalf("%s.tenant_id 缺列(D-t112 五表归属列): %v", tbl, err)
		}
		if nullable != "NO" || dflt != "0" {
			t.Fatalf("%s.tenant_id nullable=%q default=%q, want NO/\"0\"(存量回填 = 平台租户)", tbl, nullable, dflt)
		}
	}
	var defTenantUK, defOldUK, defTenantList, execTenantList, execOldList int
	if err := d.QueryRow(`
SELECT COUNT(*) FROM information_schema.STATISTICS
 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_definition'
   AND INDEX_NAME = 'uk_def_tenant_name_version' AND NON_UNIQUE = 0`).Scan(&defTenantUK); err != nil {
		t.Fatalf("uk_def_tenant_name_version: %v", err)
	}
	if err := d.QueryRow(`
SELECT COUNT(*) FROM information_schema.STATISTICS
 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_definition'
   AND INDEX_NAME = 'uk_def_name_version'`).Scan(&defOldUK); err != nil {
		t.Fatalf("old uk_def_name_version: %v", err)
	}
	if err := d.QueryRow(`
SELECT COUNT(*) FROM information_schema.STATISTICS
 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_definition'
   AND INDEX_NAME = 'idx_def_tenant_status_updated'`).Scan(&defTenantList); err != nil {
		t.Fatalf("idx_def_tenant_status_updated: %v", err)
	}
	if err := d.QueryRow(`
SELECT COUNT(*) FROM information_schema.STATISTICS
 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_execution'
   AND INDEX_NAME = 'idx_exec_tenant_name_created'`).Scan(&execTenantList); err != nil {
		t.Fatalf("idx_exec_tenant_name_created: %v", err)
	}
	if err := d.QueryRow(`
SELECT COUNT(*) FROM information_schema.STATISTICS
 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_execution'
   AND INDEX_NAME = 'idx_exec_name_created'`).Scan(&execOldList); err != nil {
		t.Fatalf("old idx_exec_name_created: %v", err)
	}
	if defTenantUK != 3 || defOldUK != 0 || defTenantList != 3 || execTenantList != 3 || execOldList != 0 {
		t.Fatalf("0011 租户键锚:uk(tenant,name,version)=%d want 3、旧 uk=%d want 0、def 列表=%d want 3、exec 列表=%d want 3、旧 exec 列表=%d want 0",
			defTenantUK, defOldUK, defTenantList, execTenantList, execOldList)
	}

	// 0012 雪花租约锚(T15.2,D-t151 决策 2 防回退):PK 单列于 node_id
	//(INSERT 抢注面);capabilities 可空(NULL = 未宣告 fail-closed,
	//T15.6 消费);holder_id/kind/expires_at 三身份列齐备。
	var leasePK, leaseCapNull int
	if err := d.QueryRow(`
SELECT COUNT(*) FROM information_schema.KEY_COLUMN_USAGE
 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'snowflake_node_lease'
   AND CONSTRAINT_NAME = 'PRIMARY'`).Scan(&leasePK); err != nil {
		t.Fatalf("snowflake_node_lease pk: %v", err)
	}
	if err := d.QueryRow(`
SELECT COUNT(*) FROM information_schema.COLUMNS
 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'snowflake_node_lease'
   AND COLUMN_NAME = 'capabilities' AND IS_NULLABLE = 'YES'`).Scan(&leaseCapNull); err != nil {
		t.Fatalf("snowflake_node_lease capabilities: %v", err)
	}
	if leasePK != 1 || leaseCapNull != 1 {
		t.Fatalf("0012 租约锚:pk cols=%d(want 1)、capabilities nullable=%d(want 1)", leasePK, leaseCapNull)
	}
	for _, col := range []string{"kind", "holder_id", "expires_at"} {
		var cnt int
		if err := d.QueryRow(`
SELECT COUNT(*) FROM information_schema.COLUMNS
 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'snowflake_node_lease'
   AND COLUMN_NAME = ? AND IS_NULLABLE = 'NO'`, col).Scan(&cnt); err != nil {
			t.Fatalf("snowflake_node_lease.%s: %v", col, err)
		}
		if cnt != 1 {
			t.Fatalf("0012 租约锚:%s NOT NULL 列缺失(want 1)", col)
		}
	}
	// 列型钉死(对抗审计 advisory:列型放宽 = 池语义漂移静默通过):node_id
	// 恒 smallint unsigned(10bit 池序号载体;INT 放宽 = 词域静默越
	// MaxNodeID 的 DB 侧防线失守)、kind 恒 ENUM 三词表(T15.6 硬闸扫描
	// 面;重排/加词须过迁移评审)、capabilities 恒 JSON(T15.6 消费契约)。
	for col, wantType := range map[string]string{
		"node_id":      "smallint unsigned", // MySQL 8 I_S 不带显示宽度(8.0.19 起弃用)
		"kind":         "enum('api','worker','reaper')",
		"capabilities": "json",
	} {
		var got string
		if err := d.QueryRow(`
SELECT COLUMN_TYPE FROM information_schema.COLUMNS
 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'snowflake_node_lease'
   AND COLUMN_NAME = ?`, col).Scan(&got); err != nil {
			t.Fatalf("snowflake_node_lease.%s type: %v", col, err)
		}
		if got != wantType {
			t.Fatalf("0012 租约锚:%s COLUMN_TYPE = %q, want %q", col, got, wantType)
		}
	}
	var leaseChk string
	if err := d.QueryRow(`
SELECT CHECK_CLAUSE FROM information_schema.CHECK_CONSTRAINTS
 WHERE CONSTRAINT_SCHEMA = DATABASE() AND CONSTRAINT_NAME = 'chk_lease_node'`,
	).Scan(&leaseChk); err != nil {
		t.Fatalf("chk_lease_node: %v", err)
	}
	if leaseChk != "(`node_id` <= 1023)" {
		t.Fatalf("0012 租约锚:CHECK = %q, want node_id <= 1023", leaseChk)
	}

	// 0013 login_audit 签发审计锚(T17.2 防回退,每迁移惯例):method 列
	// 词型/可空/缺省(backfill 口径的 DB 半边——DEFAULT 'password' 使
	// 存量行与省列 INSERT 同判);0014(T17.6)ENUM 尾追加 'ldap',词型
	// 断言联动为全迁移后三值;event_type ENUM 尾追加 scoped_token_issued
	//(词表封闭仅末尾追加契约);签发观测列组(scopes ascii / ttl 秒 /
	// token_hash char(64) ascii 镜像 audit_log 链列判)。行为半边(CHECK
	// 执法与 DEFAULT 实测)= TestLoginAuditScopedTokenContract。
	var methodType, methodNullable, methodDefault string
	if err := d.QueryRow(`
SELECT COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT FROM information_schema.COLUMNS
 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'login_audit'
   AND COLUMN_NAME = 'method'`).Scan(&methodType, &methodNullable, &methodDefault); err != nil {
		t.Fatalf("login_audit.method: %v", err)
	}
	if methodType != "enum('oidc','password','ldap')" || methodNullable != "YES" || methodDefault != "password" {
		t.Fatalf("0013 method 锚:type=%q nullable=%q default=%q, want enum('oidc','password','ldap')/YES/password",
			methodType, methodNullable, methodDefault)
	}
	// 0014 method 注释锚(T17.6,每迁移惯例):列注释兑现 0013 的「ldap
	// 预留 T17.6」承诺(ldap = LDAP bind,企业树换装凭证核验缝的审计
	// 归因词);ENUM 本体即词域,词域执法 = ENUM 拒收,DEFAULT/NULL/
	// CHECK 零变动。行为半边('ldap' 行落库 + Go 闭集单源同步)=
	// enterprise/ldap 容器 e2e + api TestKnownLoginMethodsClosedVocabulary。
	var methodComment string
	if err := d.QueryRow(`
SELECT COLUMN_COMMENT FROM information_schema.COLUMNS
 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'login_audit'
   AND COLUMN_NAME = 'method'`).Scan(&methodComment); err != nil {
		t.Fatalf("login_audit.method comment: %v", err)
	}
	if !strings.Contains(methodComment, "ldap = LDAP bind") {
		t.Fatalf("0014 method 注释锚:comment = %q, want 含 %q", methodComment, "ldap = LDAP bind")
	}
	for col, wantType := range map[string]string{
		"event_type":  "enum('login.succeeded','login.failed','login.locked','login.rate_limited','scoped_token_issued')",
		"scopes":      "varchar(255)",
		"ttl_seconds": "int unsigned",
		"token_hash":  "char(64)",
	} {
		var got string
		if err := d.QueryRow(`
SELECT COLUMN_TYPE FROM information_schema.COLUMNS
 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'login_audit'
   AND COLUMN_NAME = ?`, col).Scan(&got); err != nil {
			t.Fatalf("login_audit.%s type: %v", col, err)
		}
		if got != wantType {
			t.Fatalf("0013 签发审计锚:%s COLUMN_TYPE = %q, want %q", col, got, wantType)
		}
	}
	for _, col := range []string{"scopes", "token_hash"} {
		var charset string
		if err := d.QueryRow(`
SELECT CHARACTER_SET_NAME FROM information_schema.COLUMNS
 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'login_audit'
   AND COLUMN_NAME = ?`, col).Scan(&charset); err != nil {
			t.Fatalf("login_audit.%s charset: %v", col, err)
		}
		if charset != "ascii" {
			t.Fatalf("0013 签发审计锚:%s charset = %q, want ascii(闭词表 ASCII 域)", col, charset)
		}
	}

	// CHECK 约束齐备:13 个(definition 1 + node 3 + attempt 2 + audit 1 + users 3;
	// node +1 = 0006 chk_node_instance;users +1 = 0009 chk_user_email;
	// tenants +1 = 0011 chk_tenant_slug;lease +1 = 0012 chk_lease_node;
	// login_audit +1 = 0013 chk_login_audit_method;0014 零新增——ENUM
	// 尾追加不动约束面)
	var checks int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM information_schema.CHECK_CONSTRAINTS
		 WHERE CONSTRAINT_SCHEMA = DATABASE()`).Scan(&checks); err != nil {
		t.Fatalf("check constraints: %v", err)
	}
	if checks != 13 {
		t.Fatalf("check constraints = %d, want 13", checks)
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

	// 0006 并行实例契约(T9.1):type ENUM 含 parallel、instance_no NOT NULL
	// DEFAULT 0、uk_node_exec 三列化。钉住迁移不被无声降级。
	var enumHasParallel int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'node_execution'
		   AND COLUMN_NAME = 'type' AND COLUMN_TYPE LIKE "%'parallel'%"`).Scan(&enumHasParallel); err != nil {
		t.Fatalf("type enum parallel: %v", err)
	}
	if enumHasParallel != 1 {
		t.Fatalf("node_execution.type ENUM 不含 'parallel'(0006 防回退)")
	}
	var instanceNoDefault string
	if err := d.QueryRow(
		`SELECT COLUMN_DEFAULT FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'node_execution'
		   AND COLUMN_NAME = 'instance_no'`).Scan(&instanceNoDefault); err != nil {
		t.Fatalf("instance_no column: %v", err)
	}
	if instanceNoDefault != "0" {
		t.Fatalf("node_execution.instance_no DEFAULT = %q, want 0(存量行零迁移语义)", instanceNoDefault)
	}
	var ukExecCols int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'node_execution'
		   AND INDEX_NAME = 'uk_node_exec' AND NON_UNIQUE = 0`).Scan(&ukExecCols); err != nil {
		t.Fatalf("uk_node_exec: %v", err)
	}
	if ukExecCols != 3 {
		t.Fatalf("uk_node_exec columns = %d, want 3 (execution_id, node_id, instance_no)", ukExecCols)
	}

	// T9.3 审计哈希链契约(D-2026-09-29-t93 防回退锚):audit_pending 两列
	// 镜像 audit_log 语义(逐行链写入即算,暂存行带 hash 运输,flush 纯搬运;
	// 存量在途行按未启链期默认值落位);chain_heads = per-execution 锚,PK 钉
	// 1:1,断链闩与水位可空。
	var pendingHashCols int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'audit_pending'
		   AND COLUMN_NAME IN ('prev_hash', 'row_hash')
		   AND COLUMN_TYPE = 'char(64)' AND CHARACTER_SET_NAME = 'ascii'`).Scan(&pendingHashCols); err != nil {
		t.Fatalf("audit_pending hash columns: %v", err)
	}
	if pendingHashCols != 2 {
		t.Fatalf("audit_pending prev_hash/row_hash char(64) ascii 列 = %d, want 2", pendingHashCols)
	}
	var pendingRowHashNullable string
	if err := d.QueryRow(
		`SELECT IS_NULLABLE FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'audit_pending'
		   AND COLUMN_NAME = 'row_hash'`).Scan(&pendingRowHashNullable); err != nil {
		t.Fatalf("audit_pending.row_hash nullable: %v", err)
	}
	if pendingRowHashNullable != "YES" {
		t.Fatalf("audit_pending.row_hash IS_NULLABLE=%q, want YES(镜像 audit_log:NULL=未启链期)", pendingRowHashNullable)
	}

	var headPKCols int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'chain_heads'
		   AND INDEX_NAME = 'PRIMARY' AND NON_UNIQUE = 0`).Scan(&headPKCols); err != nil {
		t.Fatalf("chain_heads PK: %v", err)
	}
	if headPKCols != 1 {
		t.Fatalf("chain_heads PK columns = %d, want 1 (execution_id)", headPKCols)
	}
	headNullable := map[string]string{}
	headRows, err := d.Query(
		`SELECT COLUMN_NAME, IS_NULLABLE FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'chain_heads'
		   AND COLUMN_NAME IN ('broken_at', 'broken_reason', 'verified_at', 'last_row_hash')`)
	if err != nil {
		t.Fatalf("chain_heads columns: %v", err)
	}
	for headRows.Next() {
		var name, isNil string
		if err := headRows.Scan(&name, &isNil); err != nil {
			t.Fatalf("scan chain_heads: %v", err)
		}
		headNullable[name] = isNil
	}
	_ = headRows.Close()
	for _, col := range []string{"broken_at", "broken_reason", "verified_at"} {
		if headNullable[col] != "YES" {
			t.Fatalf("chain_heads.%s IS_NULLABLE=%q, want YES", col, headNullable[col])
		}
	}
	if headNullable["last_row_hash"] != "NO" {
		t.Fatalf("chain_heads.last_row_hash IS_NULLABLE=%q, want NO(head 存在即有链尾)", headNullable["last_row_hash"])
	}
}

// TestLoginAuditScopedTokenContract 是 0013 的行为半边(TestSchemaContract
// 只锚结构):DEFAULT 'password' 缺省实测(backfill 口径的 DB 侧同源语义
// ——省列 INSERT 与存量行回填同判)与 chk_login_audit_method 双向执法
// (login.* 必携非空 method、scoped_token_issued 恒 NULL)。逐号断言
// 3819(ER_CHECK_CONSTRAINT_VIOLATED)防「错源也当绿」:词法错误/列
// 缺失同样报错,只看 err≠nil 会把结构缺陷误判成约束执法。
func TestLoginAuditScopedTokenContract(t *testing.T) {
	d := mustSQL(t)
	m := newMigrator(t)
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("up: %v", err)
	}
	// 固定测试 id + 前置清扫 + t.Cleanup:测试可重复执行,不依赖执行序。
	const (
		idDefault   = 880001 // 省列 INSERT(DEFAULT 口径)
		idNoMethod  = 880002 // login.* + method NULL ⇒ 拒
		idBadIssued = 880003 // scoped_token_issued + method 值 ⇒ 拒
		idIssued    = 880004 // 签发行完整形态 ⇒ 过
		idOIDC      = 880005 // login.* + method='oidc' ⇒ 过(正控)
	)
	sweep := func() {
		_, _ = d.Exec(`DELETE FROM login_audit WHERE id IN (?, ?, ?, ?, ?)`,
			idDefault, idNoMethod, idBadIssued, idIssued, idOIDC)
	}
	sweep()
	t.Cleanup(sweep)

	// ① DEFAULT:省 method 列 ⇒ 'password'(与存量行 backfill 同源语义)。
	if _, err := d.Exec(`INSERT INTO login_audit (id, event_type, username, client_ip)
		VALUES (?, 'login.succeeded', 't172_default', '127.0.0.1')`, idDefault); err != nil {
		t.Fatalf("insert default-method row: %v", err)
	}
	var def string
	if err := d.QueryRow(`SELECT method FROM login_audit WHERE id = ?`, idDefault).Scan(&def); err != nil {
		t.Fatalf("read default method: %v", err)
	}
	if def != "password" {
		t.Fatalf("省列 INSERT method = %q, want password(0013 backfill 口径同源)", def)
	}

	// ② CHECK 双向执法:违例 INSERT 必须 3819,不是任意错误。
	for _, tc := range []struct {
		name string
		id   int64
		sql  string
	}{
		{"login row without method", idNoMethod, `INSERT INTO login_audit (id, event_type, username, client_ip, method)
			VALUES (?, 'login.succeeded', 't172_nomethod', '127.0.0.1', NULL)`},
		{"issuance row with method", idBadIssued, `INSERT INTO login_audit (id, event_type, username, client_ip, method, scopes, ttl_seconds, token_hash)
			VALUES (?, 'scoped_token_issued', 't172_bad', '127.0.0.1', 'oidc', 'executions:read', 60, '00')`},
	} {
		if _, err := d.Exec(tc.sql, tc.id); err == nil {
			t.Fatalf("%s: 违例行被接受(chk_login_audit_method 未执法)", tc.name)
		} else {
			var me *mysql.MySQLError
			if !errors.As(err, &me) || me.Number != 3819 {
				t.Fatalf("%s: err = %v, want MySQL 3819 check violation", tc.name, err)
			}
		}
	}

	// ③ 签发行完整形态过闸 + 回读字段原样(scopes/ttl/hash 的 ASCII 域)。
	if _, err := d.Exec(`INSERT INTO login_audit (id, event_type, username, client_ip, method, scopes, ttl_seconds, token_hash)
		VALUES (?, 'scoped_token_issued', 't172_issued', '127.0.0.1', NULL, 'workflows:read,executions:write', 3600, ?)`,
		idIssued, strings.Repeat("ab", 32)); err != nil {
		t.Fatalf("insert issuance row: %v", err)
	}
	var scopes string
	var ttlSecs uint32
	var hash string
	var method sql.NullString
	var reason sql.NullString
	if err := d.QueryRow(`SELECT method, scopes, ttl_seconds, token_hash, reason
		FROM login_audit WHERE id = ?`, idIssued).
		Scan(&method, &scopes, &ttlSecs, &hash, &reason); err != nil {
		t.Fatalf("read issuance row: %v", err)
	}
	if method.Valid || reason.Valid {
		t.Fatalf("签发行 method/reason 应恒 NULL,得 %q/%q", method.String, reason.String)
	}
	if scopes != "workflows:read,executions:write" || ttlSecs != 3600 || len(hash) != 64 {
		t.Fatalf("签发行字段回读失真:scopes=%q ttl=%d hashlen=%d", scopes, ttlSecs, len(hash))
	}

	// ④ 正控:login.* + method='oidc' 合法(词域开放给 OIDC 路)。
	if _, err := d.Exec(`INSERT INTO login_audit (id, event_type, username, client_ip, method)
		VALUES (?, 'login.succeeded', 't172_oidc', '127.0.0.1', 'oidc')`, idOIDC); err != nil {
		t.Fatalf("insert oidc login row: %v", err)
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
