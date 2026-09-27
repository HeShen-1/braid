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
	tests := []struct {
		name    string
		exempt  bool // AllowLoginAuditDelete(reaper 保留期清理豁免形态)
		planes  []grantPlane
		wantSub []string
	}{
		{name: "no planes at all"},
		{
			name: "read-only planes invisible",
			planes: []grantPlane{
				{on: "`sch`.*", coversAuditLog: true},
				{on: "`sch`.`audit_log`", coversAuditLog: true},
				{on: "`sch`.`login_audit`"},
			},
		},
		{
			name:    "schema update rejected",
			planes:  []grantPlane{{on: "`sch`.*", update: true, coversAuditLog: true}},
			wantSub: []string{"UPDATE on `sch`.*", "REVOKE UPDATE ON `sch`.* FROM 'app'@'%'", "append-only"},
		},
		{
			name:    "table delete rejected",
			planes:  []grantPlane{{on: "`sch`.`audit_log`", delete: true, coversAuditLog: true}},
			wantSub: []string{"DELETE on `sch`.`audit_log`", "REVOKE DELETE ON `sch`.`audit_log` FROM 'app'@'%'"},
		},
		// ---- login_audit 扩面臂(T7.4,D-t64 ①):coversAuditLog=false 即
		// 对象仅覆盖 login_audit,DELETE 的豁免判定面。----
		{
			name:    "login_audit delete rejected strict",
			planes:  []grantPlane{{on: "`sch`.`login_audit`", delete: true}},
			wantSub: []string{"DELETE on `sch`.`login_audit`", "REVOKE DELETE ON `sch`.`login_audit` FROM 'app'@'%'"},
		},
		{
			// reaper 形态:janitor purgeLoginAudit 的表级 DELETE 豁免
			// (授权矩阵 deploy/README.md §5;api 形态仍拒)。
			name:   "login_audit delete passes under exemption",
			exempt: true,
			planes: []grantPlane{{on: "`sch`.`login_audit`", delete: true}},
		},
		{
			// 豁免只放行 DELETE:改历史行(UPDATE)在任何组件、任何一张
			// 受保护表上都违约。
			name:    "login_audit update rejected even under exemption",
			exempt:  true,
			planes:  []grantPlane{{on: "`sch`.`login_audit`", update: true}},
			wantSub: []string{"UPDATE on `sch`.`login_audit`", "REVOKE UPDATE ON `sch`.`login_audit` FROM 'app'@'%'"},
		},
		{
			// schema 级授权覆盖 audit_log ⇒ 豁免不适用:reaper 的
			// login_audit DELETE 必须表级精确授权,schema 级照拒。
			name:    "schema delete rejected even under exemption",
			exempt:  true,
			planes:  []grantPlane{{on: "`sch`.*", delete: true, coversAuditLog: true}},
			wantSub: []string{"DELETE on `sch`.*", "REVOKE DELETE ON `sch`.* FROM 'app'@'%'"},
		},
		{
			name:    "audit_log delete rejected even under exemption",
			exempt:  true,
			planes:  []grantPlane{{on: "`sch`.`audit_log`", delete: true, coversAuditLog: true}},
			wantSub: []string{"DELETE on `sch`.`audit_log`", "REVOKE DELETE ON `sch`.`audit_log` FROM 'app'@'%'"},
		},
		{
			// GRANT ALL ON sch.* 的展开落业务平面 = 违约配置,拒;同对象
			// 双权限合成一条合法 REVOKE(对抗审计 F7:单语句混 ON 子句 = 1064)。
			name:    "schema ALL expansion grouped into one statement",
			planes:  []grantPlane{{on: "`sch`.*", update: true, delete: true, coversAuditLog: true}},
			wantSub: []string{"UPDATE/DELETE on `sch`.*", "REVOKE UPDATE, DELETE ON `sch`.* FROM 'app'@'%'"},
		},
		{
			// 多对象 = 多条语句,分号分隔,各自合法。
			name: "multi-plane statements stay separately executable",
			planes: []grantPlane{
				{on: "`sch`.*", update: true, delete: true, coversAuditLog: true},
				{on: "`sch%`.`audit_log`", delete: true, coversAuditLog: true},
			},
			wantSub: []string{
				"REVOKE UPDATE, DELETE ON `sch`.* FROM 'app'@'%'; REVOKE DELETE ON `sch%`.`audit_log` FROM 'app'@'%'",
			},
		},
		{
			name:    "column update carries column list",
			planes:  []grantPlane{{on: "`sch`.`audit_log`", colUpd: []string{"payload", "id"}, coversAuditLog: true}},
			wantSub: []string{"UPDATE(payload,id) on `sch`.`audit_log`", "REVOKE UPDATE (payload, id) ON `sch`.`audit_log` FROM 'app'@'%'"},
		},
		{
			// 列级 UPDATE 打在 login_audit 上同样违约(豁免只有 DELETE 面)。
			name:    "column update on login_audit rejected",
			planes:  []grantPlane{{on: "`sch`.`login_audit`", colUpd: []string{"username"}}},
			wantSub: []string{"REVOKE UPDATE (username) ON `sch`.`login_audit`"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := appendOnlyVerdict("app@%", "'app'@'%'", tt.exempt, tt.planes)
			assertSubstrings(t, err, tt.wantSub)
		})
	}
}

func TestLikeMatch(t *testing.T) {
	tests := []struct {
		pattern, s string
		want       bool
	}{
		{pattern: "braid", s: "braid", want: true},
		{pattern: "braid", s: "braidx", want: false},
		{pattern: "braid%", s: "braid_preflight_test_ab12cd34", want: true},
		{pattern: "bra_d", s: "braid", want: true},
		{pattern: "bra_d", s: "brad", want: false},        // _ 恰一字符
		{pattern: "braid\\_d", s: "braid_d", want: true},  // 转义 _ 取字面
		{pattern: "braid\\_d", s: "braidxd", want: false}, // 字面 _ 不匹配 x
		{pattern: "%", s: "anything", want: true},
		{pattern: "braid\\%", s: "braidx", want: false}, // 转义 % 取字面
		{pattern: "braid\\%", s: "braid%", want: true},
		{pattern: "", s: "", want: true},
		{pattern: "a%c", s: "abc", want: true},
		{pattern: "a%c", s: "ac", want: true}, // % 匹配空串
		// 病态 % 密集模式:贪心双指针下微秒级;若回退成朴素回溯(指数),
		// 本用例会以超时形式红(delta 复审实测 8 组 % 即 >100s)。
		{pattern: "%a%a%a%a%a%a%a%a%a%a%b", s: strings.Repeat("a", 64), want: false},
	}
	for _, tt := range tests {
		if got := likeMatch(tt.pattern, tt.s); got != tt.want {
			t.Errorf("likeMatch(%q, %q) = %v, want %v", tt.pattern, tt.s, got, tt.want)
		}
	}
}

func TestGranteeOf(t *testing.T) {
	tests := []struct {
		acct string
		want string
	}{
		{acct: "root@%", want: "'root'@'%'"},
		{acct: "app@10.0.0.1", want: "'app'@'10.0.0.1'"},
		// 服务端 GRANTEE 渲染对内部单引号原样嵌入、不双写(MySQL 8.0.46
		// t13 实证:'we'ird'@'%')——双写恒不匹配(对抗审计 F5)。
		{acct: "u'x@h", want: "'u'x'@'h'"},
		{acct: "a@b@c", want: "'a@b'@'c'"}, // 最后一个 @ 是 user/host 分界
		{acct: "weird", want: "'weird'"},   // 防御分支
	}
	for _, tt := range tests {
		if got := granteeOf(tt.acct); got != tt.want {
			t.Errorf("granteeOf(%q) = %q, want %q", tt.acct, got, tt.want)
		}
	}
}

// granteeSQL 是 REVOKE 渲染形态:含引号账号双写(delta 复审,否则提示 1064)。
func TestGranteeSQL(t *testing.T) {
	tests := []struct {
		acct string
		want string
	}{
		{acct: "root@%", want: "'root'@'%'"},
		{acct: "u'x@h", want: "'u''x'@'h'"},
		{acct: "weird", want: "'weird'"},
	}
	for _, tt := range tests {
		if got := granteeSQL(tt.acct); got != tt.want {
			t.Errorf("granteeSQL(%q) = %q, want %q", tt.acct, got, tt.want)
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
	if err := CheckAuditAppendOnly(ctx, dbh, AppendOnlyStrict); err != nil {
		t.Fatalf("CheckAuditAppendOnly(strict): %v", err)
	}
	// 豁免形态同账号也应过(root 落管理平面;豁免只影响 login_audit DELETE 判定)。
	if err := CheckAuditAppendOnly(ctx, dbh, AllowLoginAuditDelete); err != nil {
		t.Fatalf("CheckAuditAppendOnly(exempt): %v", err)
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
	// 无条件 DELETE+INSERT 而非 UPDATE:空表臂中途 Fatalf 时 UPDATE 匹配
	// 0 行,共享 schema 会带着空表泄给后续测试(delta 复审)。
	t.Cleanup(func() {
		_, _ = dbh.Exec(`DELETE FROM schema_migrations`)
		_, _ = dbh.Exec(`INSERT INTO schema_migrations (version, dirty) VALUES (?, 0)`, latest)
	})
	// 空 schema_migrations(表在、行无)→ 独立错误臂(对抗审计 F11)。
	if _, err := dbh.Exec(`DELETE FROM schema_migrations`); err != nil {
		t.Fatalf("empty setup: %v", err)
	}
	err = CheckSchemaVersion(ctx, dbh)
	if err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("empty schema_migrations: err = %v, want 'is empty'", err)
	}
	if _, err := dbh.Exec(`INSERT INTO schema_migrations (version, dirty) VALUES (?, 0)`, latest); err != nil {
		t.Fatalf("restore row: %v", err)
	}
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
// warn 不拒、业务平面(schema/table/column/通配模式)拒、收权即绿;root 的
// 管理平面豁免臂已在 green 测试覆盖。账号名拼 worktree 键:t13 是全 worktree
// 共享实例,固定名会被并行 worktree 的 GRANT/REVOKE 互踩出静默假判定
// (对抗审计 F9,与 Redis 槽位纪律同族)。
func TestAuditAppendOnlyEnforcement(t *testing.T) {
	admin := openMigrated(t)
	ctx := context.Background()
	var schema string
	if err := admin.QueryRow(`SELECT DATABASE()`).Scan(&schema); err != nil {
		t.Fatalf("read schema: %v", err)
	}
	user := "braid_t73_" + db.WorktreeKey() // ≤ 32 chars:10 前缀 + 8 hex
	pass := "braid_t73_pw"                  // t13 占位凭证,非任何真实系统凭证(sqltest 同款口径)
	cfg, err := mysql.ParseDSN(db.TestServerDSN())
	if err != nil {
		t.Fatalf("parse test server DSN: %v", err)
	}
	mustExec := func(q string) {
		t.Helper()
		if _, err := admin.Exec(q); err != nil {
			t.Fatalf("grant setup %q: %v", q, err)
		}
	}
	// 先 DROP 再 CREATE:清掉上次异常中断残留的同名授权态。
	mustExec(fmt.Sprintf("DROP USER IF EXISTS '%s'@'%%'", user))
	t.Cleanup(func() {
		_, _ = admin.Exec(fmt.Sprintf("DROP USER IF EXISTS '%s'@'%%'", user))
	})
	mustExec(fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s'", user, pass))
	limited, err := sql.Open("mysql", db.CanonicalDSN(
		fmt.Sprintf("%s:%s@tcp(%s)/%s", user, pass, cfg.Addr, schema)))
	if err != nil {
		t.Fatalf("open limited account: %v", err)
	}
	t.Cleanup(func() { _ = limited.Close() })

	steps := []struct {
		name       string
		setup      []string
		wantStrict []string // AppendOnlyStrict 期望;空 = 通过
		wantExempt []string // AllowLoginAuditDelete 期望;空 = 通过
		execHint   bool     // 把提示里的 REVOKE 原样执行并断言随后判定通过
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
			name:       "schema UPDATE rejected",
			setup:      []string{fmt.Sprintf("GRANT UPDATE ON %s.* TO '%s'@'%%'", schema, user)},
			wantStrict: []string{fmt.Sprintf("REVOKE UPDATE ON `%s`.* FROM '%s'@'%%'", schema, user)},
			wantExempt: []string{fmt.Sprintf("REVOKE UPDATE ON `%s`.* FROM '%s'@'%%'", schema, user)},
		},
		{
			name:  "schema revoked passes",
			setup: []string{fmt.Sprintf("REVOKE UPDATE ON %s.* FROM '%s'@'%%'", schema, user)},
		},
		{
			// 对抗审计 F2:通配模式授权(`%` 覆盖本库)不能被精确等值漏掉;
			// REVOKE 必须收在同一模式上(收在实名上不撤销模式授权)。
			name:       "wildcard schema pattern rejected",
			setup:      []string{fmt.Sprintf("GRANT UPDATE ON `%s%%`.* TO '%s'@'%%'", schema, user)},
			wantStrict: []string{"REVOKE UPDATE ON `" + schema + "%`.* FROM '" + user + "'@'%'"},
			wantExempt: []string{"REVOKE UPDATE ON `" + schema + "%`.* FROM '" + user + "'@'%'"},
		},
		{
			name:  "wildcard revoked on the pattern passes",
			setup: []string{fmt.Sprintf("REVOKE UPDATE ON `%s%%`.* FROM '%s'@'%%'", schema, user)},
		},
		{
			name:       "table DELETE on audit_log rejected",
			setup:      []string{fmt.Sprintf("GRANT DELETE ON %s.audit_log TO '%s'@'%%'", schema, user)},
			wantStrict: []string{"REVOKE DELETE ON `" + schema + "`.`audit_log` FROM '" + user + "'@'%'"},
			wantExempt: []string{"REVOKE DELETE ON `" + schema + "`.`audit_log` FROM '" + user + "'@'%'"},
		},
		{
			name:  "table revoked passes",
			setup: []string{fmt.Sprintf("REVOKE DELETE ON %s.audit_log FROM '%s'@'%%'", schema, user)},
		},
		// ---- login_audit 扩面臂(T7.4,D-t64 ①)。----
		{
			// api/worker/relay 形态(strict):login_audit 的业务授权只有
			// INSERT/SELECT,DELETE 违约拒启;同授权态下 reaper 豁免形态
			// (wantExempt 空 = 通过)双验一步完成。
			name:       "table DELETE on login_audit rejected strict, passes exempt",
			setup:      []string{fmt.Sprintf("GRANT DELETE ON %s.login_audit TO '%s'@'%%'", schema, user)},
			wantStrict: []string{"REVOKE DELETE ON `" + schema + "`.`login_audit` FROM '" + user + "'@'%'"},
		},
		{
			name:  "login_audit table DELETE revoked passes",
			setup: []string{fmt.Sprintf("REVOKE DELETE ON %s.login_audit FROM '%s'@'%%'", schema, user)},
		},
		{
			// UPDATE 面无豁免:改历史行在任何形态都违约。
			name:       "table UPDATE on login_audit rejected both forms",
			setup:      []string{fmt.Sprintf("GRANT UPDATE ON %s.login_audit TO '%s'@'%%'", schema, user)},
			wantStrict: []string{"REVOKE UPDATE ON `" + schema + "`.`login_audit` FROM '" + user + "'@'%'"},
			wantExempt: []string{"REVOKE UPDATE ON `" + schema + "`.`login_audit` FROM '" + user + "'@'%'"},
		},
		{
			name:  "login_audit UPDATE revoked passes",
			setup: []string{fmt.Sprintf("REVOKE UPDATE ON %s.login_audit FROM '%s'@'%%'", schema, user)},
		},
		{
			// schema 级 DELETE 覆盖 audit_log ⇒ 豁免形态照拒:reaper 的
			// login_audit DELETE 必须表级精确授权(issue #70 按组件授权)。
			name:       "schema DELETE rejected even under exemption",
			setup:      []string{fmt.Sprintf("GRANT DELETE ON %s.* TO '%s'@'%%'", schema, user)},
			wantStrict: []string{"REVOKE DELETE ON `" + schema + "`.* FROM '" + user + "'@'%'"},
			wantExempt: []string{"REVOKE DELETE ON `" + schema + "`.* FROM '" + user + "'@'%'"},
		},
		{
			name:  "schema DELETE revoked passes",
			setup: []string{fmt.Sprintf("REVOKE DELETE ON %s.* FROM '%s'@'%%'", schema, user)},
		},
		{
			// 对抗审计 F3/F4:列级 UPDATE 只落 COLUMN_PRIVILEGES。
			name:       "column UPDATE on audit_log rejected",
			setup:      []string{fmt.Sprintf("GRANT UPDATE (payload, id) ON %s.audit_log TO '%s'@'%%'", schema, user)},
			wantStrict: []string{"REVOKE UPDATE (id, payload) ON `" + schema + "`.`audit_log` FROM '" + user + "'@'%'"},
			wantExempt: []string{"REVOKE UPDATE (id, payload) ON `" + schema + "`.`audit_log` FROM '" + user + "'@'%'"},
		},
		{
			name:  "column revoked passes",
			setup: []string{fmt.Sprintf("REVOKE UPDATE (payload, id) ON %s.audit_log FROM '%s'@'%%'", schema, user)},
		},
		{
			// GRANT ALL ON sch.* 展开落业务平面(I_S 无 ALL 字面量,实测
			// 逐项展开)= 违约配置,拒;随后把提示里的 REVOKE 语句原样执行,
			// 锚死「提示是可直接执行的合法 SQL」(对抗审计 F7 的 1064 回归;
			// execHint 显式字段,防用例改名静默丢锚——delta 复审)。
			name: "schema ALL expansion rejected and hinted REVOKE executes",
			setup: []string{
				fmt.Sprintf("GRANT SELECT, INSERT ON %s.* TO '%s'@'%%'", schema, user),
				fmt.Sprintf("GRANT ALL ON %s.* TO '%s'@'%%'", schema, user),
			},
			wantStrict: []string{"REVOKE UPDATE, DELETE ON `" + schema + "`.* FROM '" + user + "'@'%'"},
			wantExempt: []string{"REVOKE UPDATE, DELETE ON `" + schema + "`.* FROM '" + user + "'@'%'"},
			execHint:   true,
		},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			for _, q := range st.setup {
				mustExec(q)
			}
			assertSubstrings(t, CheckAuditAppendOnly(ctx, limited, AppendOnlyStrict), st.wantStrict)
			assertSubstrings(t, CheckAuditAppendOnly(ctx, limited, AllowLoginAuditDelete), st.wantExempt)
			if st.execHint {
				err := CheckAuditAppendOnly(ctx, limited, AppendOnlyStrict)
				if err == nil {
					t.Fatal("execHint step: strict form unexpectedly passed")
				}
				hint := err.Error()
				i := strings.Index(hint, "fix: ")
				j := strings.Index(hint, " (audit_log/login_audit-facing")
				if i < 0 || j <= i {
					t.Fatalf("cannot locate fix hint in: %v", err)
				}
				for _, stmt := range strings.Split(hint[i+len("fix: "):j], "; ") {
					if _, xerr := admin.Exec(stmt); xerr != nil {
						t.Fatalf("hinted REVOKE not executable: %q: %v", stmt, xerr)
					}
				}
				if aerr := CheckAuditAppendOnly(ctx, limited, AppendOnlyStrict); aerr != nil {
					t.Fatalf("after executing hinted REVOKEs, check should pass: %v", aerr)
				}
			}
		})
	}
}
