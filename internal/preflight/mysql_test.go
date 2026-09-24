package preflight

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"braid/internal/db"
	"braid/internal/db/sqltest"
)

// ---- 纯判定层(离线,表驱动) ----

func TestSchemaVerdict(t *testing.T) {
	tests := []struct {
		name    string
		latest  uint
		cur     uint
		dirty   bool
		wantSub []string // 空 = 期望通过;否则错误须含全部子串
	}{
		{name: "at latest", latest: 3, cur: 3},
		{name: "binary newer than schema", latest: 3, cur: 2, wantSub: []string{"binary ships 3", "golang-migrate up"}},
		{name: "dirty even at latest", latest: 3, cur: 3, dirty: true, wantSub: []string{"dirty at version 3", "migrate force 3"}},
		{name: "schema newer passes (warn path, not error)", latest: 2, cur: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := schemaVerdict(tt.latest, tt.cur, tt.dirty)
			assertSubstrings(t, err, tt.wantSub)
		})
	}
}

func TestPartitionVerdict(t *testing.T) {
	today := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		bound   time.Time
		wantSub []string
	}{
		{name: "covers today (bound = tomorrow)", bound: today.AddDate(0, 0, 1)},
		{name: "full janitor horizon", bound: today.AddDate(0, 0, 7)},
		{name: "bound equals today covers only yesterday", bound: today, wantSub: []string{"does not cover today", "1526", "reaper"}},
		{name: "stale watermark", bound: today.AddDate(0, 0, -1), wantSub: []string{"ALTER TABLE audit_log ADD PARTITION"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := partitionVerdict(tt.bound, today)
			assertSubstrings(t, err, tt.wantSub)
		})
	}
}

func TestAppendOnlyVerdict(t *testing.T) {
	// schema 平面展开集(MySQL 8 对 GRANT ALL ON sch.* 的实测形态,截断到
	// 判定相关的写权限即可)。
	allExpansion := map[string]bool{
		"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true,
		"ALTER": true, "CREATE": true, "DROP": true,
	}
	tests := []struct {
		name        string
		schemaPrivs map[string]bool
		tablePrivs  map[string]bool
		wantSub     []string
	}{
		{name: "no privileges at all"},
		{name: "business steady state", schemaPrivs: map[string]bool{"SELECT": true, "INSERT": true}},
		{
			name:        "schema update rejected",
			schemaPrivs: map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": true},
			wantSub:     []string{"UPDATE on sch.*", "REVOKE UPDATE ON sch.* FROM 'app'@'%'", "append-only"},
		},
		{
			name:       "table delete rejected",
			tablePrivs: map[string]bool{"DELETE": true},
			wantSub:    []string{"DELETE on sch.audit_log", "REVOKE DELETE ON sch.audit_log"},
		},
		{
			// GRANT ALL ON sch.* 的展开落业务平面 = 违约配置,拒。
			name:        "schema ALL expansion rejected",
			schemaPrivs: allExpansion,
			wantSub:     []string{"UPDATE on sch.*", "DELETE on sch.*"},
		},
		{
			name:        "both planes both privileges named",
			schemaPrivs: map[string]bool{"UPDATE": true},
			tablePrivs:  map[string]bool{"DELETE": true},
			wantSub:     []string{"UPDATE on sch.*", "DELETE on sch.audit_log"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := appendOnlyVerdict("sch", "app@%", "'app'@'%'", tt.schemaPrivs, tt.tablePrivs)
			assertSubstrings(t, err, tt.wantSub)
		})
	}
}

func TestGranteeOf(t *testing.T) {
	tests := []struct {
		acct string
		want string
	}{
		{acct: "root@%", want: "'root'@'%'"},
		{acct: "app@10.0.0.1", want: "'app'@'10.0.0.1'"},
		{acct: "u'x@h", want: "'u''x'@'h'"},
		{acct: "a@b@c", want: "'a@b'@'c'"}, // 最后一个 @ 是 user/host 分界
		{acct: "weird", want: "'weird'"},   // 防御分支
	}
	for _, tt := range tests {
		if got := granteeOf(tt.acct); got != tt.want {
			t.Errorf("granteeOf(%q) = %q, want %q", tt.acct, got, tt.want)
		}
	}
}

// assertSubstrings:wantSub 空 ⇒ err 必须为 nil;否则 err 必须非 nil 且逐子串命中。
func assertSubstrings(t *testing.T, err error, wantSub []string) {
	t.Helper()
	if len(wantSub) == 0 {
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("err = nil, want containing %v", wantSub)
	}
	for _, sub := range wantSub {
		if !strings.Contains(err.Error(), sub) {
			t.Fatalf("err = %q, want substring %q", err.Error(), sub)
		}
	}
}

// ---- t13 集成(栈不可达自动 skip) ----

// preflightSchemaBase:本包独占 schema(分区 DROP/版本回拨都在上面做,不碰
// 其他包共用的 braid_engine_test)。
const preflightSchemaBase = "braid_preflight_test"

func openMigrated(t *testing.T) *sql.DB {
	t.Helper()
	return sqltest.MustOpenAt(t, preflightSchemaBase)
}

func dbToday(t *testing.T, dbh *sql.DB) time.Time {
	t.Helper()
	var today time.Time
	if err := dbh.QueryRow(`SELECT CAST(NOW(3) AS DATE)`).Scan(&today); err != nil {
		t.Fatalf("read db date: %v", err)
	}
	return today
}

func TestMySQLChecksGreenOnMigratedSchema(t *testing.T) {
	dbh := openMigrated(t)
	ctx := context.Background()
	// root@% 的 UPDATE/DELETE 落管理平面(全局)→ 仅 warn 判定过;sqltest
	// 已把分区前滚到 today+7 → 水位覆盖当日。
	if err := CheckSchemaVersion(ctx, dbh); err != nil {
		t.Fatalf("CheckSchemaVersion: %v", err)
	}
	if err := CheckAuditAppendOnly(ctx, dbh); err != nil {
		t.Fatalf("CheckAuditAppendOnly: %v", err)
	}
	if err := CheckAuditPartitions(ctx, dbh); err != nil {
		t.Fatalf("CheckAuditPartitions: %v", err)
	}
}

func TestSchemaVersionRejectsStaleAndDirty(t *testing.T) {
	dbh := openMigrated(t)
	ctx := context.Background()
	latest, err := db.LatestMigrationVersion()
	if err != nil {
		t.Fatalf("LatestMigrationVersion: %v", err)
	}
	// 铁律管的是 audit_log;schema_migrations 是迁移元数据,测试可写可还原。
	t.Cleanup(func() {
		_, _ = dbh.Exec(`UPDATE schema_migrations SET version = ?, dirty = 0`, latest)
	})
	if _, err := dbh.Exec(`UPDATE schema_migrations SET version = 1, dirty = 0`); err != nil {
		t.Fatalf("stale version setup: %v", err)
	}
	err = CheckSchemaVersion(ctx, dbh)
	if err == nil || !strings.Contains(err.Error(), "binary ships") {
		t.Fatalf("stale schema: err = %v, want 'binary ships'", err)
	}
	if _, err := dbh.Exec(`UPDATE schema_migrations SET version = ?, dirty = 1`, latest); err != nil {
		t.Fatalf("dirty setup: %v", err)
	}
	err = CheckSchemaVersion(ctx, dbh)
	if err == nil || !strings.Contains(err.Error(), "migrate force") {
		t.Fatalf("dirty schema: err = %v, want 'migrate force'", err)
	}
}

func TestAuditPartitionsRejectsExpiredWatermark(t *testing.T) {
	dbh := openMigrated(t)
	ctx := context.Background()
	// 先绿:sqltest 前滚后水位 ≥ today+7。
	if err := CheckAuditPartitions(ctx, dbh); err != nil {
		t.Fatalf("green path: %v", err)
	}
	today := dbToday(t, dbh)

	// 收集当日之后的分区并全 DROP → maxBound 停在今天,地板破约。
	type part struct{ name, bound string }
	rows, err := dbh.Query(`
SELECT PARTITION_NAME, PARTITION_DESCRIPTION FROM INFORMATION_SCHEMA.PARTITIONS
 WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='audit_log'
 ORDER BY PARTITION_DESCRIPTION`)
	if err != nil {
		t.Fatalf("list partitions: %v", err)
	}
	var dropped []part
	for rows.Next() {
		var p part
		if err := rows.Scan(&p.name, &p.bound); err != nil {
			t.Fatalf("scan partition: %v", err)
		}
		bound, err := time.ParseInLocation("2006-01-02", strings.Trim(p.bound, "' \t"), time.UTC)
		if err != nil || !bound.After(today) {
			continue
		}
		dropped = append(dropped, p)
	}
	_ = rows.Close()
	if len(dropped) == 0 {
		t.Fatal("setup: expected at least one future partition after sqltest roll-forward")
	}
	// 还原:日期升序重建(dropped 已按 PARTITION_DESCRIPTION 升序收集)。
	t.Cleanup(func() {
		for _, p := range dropped {
			_, _ = dbh.Exec(fmt.Sprintf(
				"ALTER TABLE audit_log ADD PARTITION (PARTITION %s VALUES LESS THAN ('%s'))",
				p.name, strings.Trim(p.bound, "' \t")))
		}
	})
	for _, p := range dropped {
		if _, err := dbh.Exec("ALTER TABLE audit_log DROP PARTITION " + p.name); err != nil {
			t.Fatalf("drop partition %s: %v", p.name, err)
		}
	}
	err = CheckAuditPartitions(ctx, dbh)
	if err == nil || !strings.Contains(err.Error(), "does not cover today") {
		t.Fatalf("expired watermark: err = %v, want 'does not cover today'", err)
	}
}

// TestAuditAppendOnlyEnforcement:按平面各建红绿臂——管理平面(global)只
// warn 不拒、业务平面(schema/table)拒、收权即绿;root 的管理平面豁免臂
// 已在 green 测试覆盖。
func TestAuditAppendOnlyEnforcement(t *testing.T) {
	admin := openMigrated(t)
	ctx := context.Background()
	var schema string
	if err := admin.QueryRow(`SELECT DATABASE()`).Scan(&schema); err != nil {
		t.Fatalf("read schema: %v", err)
	}
	const (
		user = "braid_t73_acct"
		pass = "braid_t73_pw" // t13 占位凭证,非任何真实系统凭证(sqltest 同款口径)
	)
	cfg, err := mysql.ParseDSN(db.TestServerDSN())
	if err != nil {
		t.Fatalf("parse test server DSN: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(fmt.Sprintf("DROP USER IF EXISTS '%s'@'%%'", user))
	})
	mustExec := func(q string) {
		t.Helper()
		if _, err := admin.Exec(q); err != nil {
			t.Fatalf("grant setup %q: %v", q, err)
		}
	}
	mustExec(fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'", user, pass))
	limited, err := sql.Open("mysql", db.CanonicalDSN(
		fmt.Sprintf("%s:%s@tcp(%s)/%s", user, pass, cfg.Addr, schema)))
	if err != nil {
		t.Fatalf("open limited account: %v", err)
	}
	t.Cleanup(func() { _ = limited.Close() })

	steps := []struct {
		name    string
		setup   []string
		wantSub []string // 空 = 期望通过
	}{
		{
			// 管理平面:全局 UPDATE 只 warn(判定过),dev/test root 同形态。
			name:  "global UPDATE passes (admin plane)",
			setup: []string{fmt.Sprintf("GRANT SELECT ON *.* TO '%s'@'%%'", user), fmt.Sprintf("GRANT UPDATE ON *.* TO '%s'@'%%'", user)},
		},
		{
			name:  "globals revoked passes",
			setup: []string{fmt.Sprintf("REVOKE SELECT, UPDATE ON *.* FROM '%s'@'%%'", user)},
		},
		{
			name:    "schema UPDATE rejected",
			setup:   []string{fmt.Sprintf("GRANT UPDATE ON %s.* TO '%s'@'%%'", schema, user)},
			wantSub: []string{"REVOKE UPDATE ON " + schema + ".*"},
		},
		{
			name:  "schema revoked passes",
			setup: []string{fmt.Sprintf("REVOKE UPDATE ON %s.* FROM '%s'@'%%'", schema, user)},
		},
		{
			name:    "table DELETE on audit_log rejected",
			setup:   []string{fmt.Sprintf("GRANT DELETE ON %s.audit_log TO '%s'@'%%'", schema, user)},
			wantSub: []string{"REVOKE DELETE ON " + schema + ".audit_log"},
		},
		{
			name:  "table revoked passes",
			setup: []string{fmt.Sprintf("REVOKE DELETE ON %s.audit_log FROM '%s'@'%%'", schema, user)},
		},
		{
			// GRANT ALL ON sch.* 展开落业务平面(I_S 无 ALL 字面量,实测
			// 逐项展开)= 违约配置,拒。
			name:    "schema ALL expansion rejected",
			setup:   []string{fmt.Sprintf("GRANT ALL ON %s.* TO '%s'@'%%'", schema, user)},
			wantSub: []string{"UPDATE on " + schema + ".*", "DELETE on " + schema + ".*"},
		},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			for _, q := range st.setup {
				mustExec(q)
			}
			err := CheckAuditAppendOnly(ctx, limited)
			assertSubstrings(t, err, st.wantSub)
		})
	}
}
