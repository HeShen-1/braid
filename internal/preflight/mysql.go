// Package preflight 提供 T7.3 安装自检:错误配置下拒绝启动并输出修复提示
// (拒启动先例 = cmd/* main 的 slog.Error + os.Exit(1),本包只负责把「哪里
// 错、怎么修」说清楚)。四个自检由各 cmd 入口按自身读写面装配:
//
//   - CheckSchemaVersion(迁移版本比对):四入口全硬拒——旧 schema 上跑任何
//     组件都是未定义行为。
//   - CheckAuditAppendOnly(业务账号对 audit_log 无 UPDATE/DELETE):四入口
//     全硬拒——append-only 铁律的部署层断言(DDL §append-only)。
//   - CheckAuditPartitions(audit_log 分区水位覆盖当日):api/worker 硬拒
//     (审计写方:分区不覆盖当日则 INSERT 全部 1526);relay(零审计写)与
//     reaper(水位自愈者,janitor 首 tick 即补)仅 warn——对它们硬拒会造出
//     「没人补分区 → 永远起不来」的死锁。
//   - CheckRedisQueue(队列实例 noeviction + AOF everysec 契约):relay/
//     worker 硬拒(队列数据承载方);reaper 仅 warn(D-t25 fail-open:锁实例
//     破约的失败模式 = 锁丢失退化为 leaderless,安全,拒启反制造停摆面);
//     api 不调(零 Redis)。
//
// 词表自检不在本包:cmd/worker 的 llm.PrebuildTokenizers(T7.2 接线)即是,
// 复用不重复(D-t72 #4)。水位不足 janitor 口径(today+7)恒为 warn——那是
// reaper 的作业进度,不是任何组件的启动前提。
package preflight

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"braid/internal/db"
)

// CheckSchemaVersion 断言 schema_migrations 落库版本 = 二进制内嵌迁移最新版
// 且非 dirty。cur > latest(旧二进制对新 schema)仅 warn:滚动升级的 N-1
// 兼容窗口归 T7.4 定稿,当前拒启只会把「能跑」误伤成「起不来」。
func CheckSchemaVersion(ctx context.Context, dbh *sql.DB) error {
	latest, err := db.LatestMigrationVersion()
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	var cur uint
	var dirty bool
	err = dbh.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&cur, &dirty)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("preflight: schema_migrations is empty (migrations recorded but never applied); fix: run the migrator (golang-migrate up) against this schema before start")
	case err != nil:
		// 最常见形态即「迁移从没跑过」(1146 table missing 等)。
		return fmt.Errorf("preflight: read schema_migrations (migrations never applied? run the migrator / golang-migrate up first): %w", err)
	}
	if cur > latest {
		slog.Warn("preflight: schema newer than binary (N-1 window, see T7.4)",
			"schema_version", cur, "binary_version", latest)
	}
	return schemaVerdict(latest, cur, dirty)
}

// schemaVerdict 是版本比对的纯判定(表驱动单测锚)。
func schemaVerdict(latest, cur uint, dirty bool) error {
	if dirty {
		return fmt.Errorf("preflight: schema_migrations dirty at version %d (previous migration interrupted); fix: resolve the failed statement, then `migrate force %d` and re-run up (runbook: T7.4)", cur, cur)
	}
	if cur < latest {
		return fmt.Errorf("preflight: schema at migration %d, binary ships %d; fix: apply migrations (golang-migrate up) before start", cur, latest)
	}
	return nil
}

// CheckAuditPartitions 断言 audit_log 的 RANGE 分区水位覆盖当日:当日审计行
// (exec_date=today)必须落得进某个分区,否则一切 INSERT 以 1526 失败
// (T2.5 时间弹同族,安装口拦下)。读取同 janitor 先例:
// INFORMATION_SCHEMA.PARTITIONS + DB 时钟(NOW(3) 会话 UTC,连接契约保证)。
func CheckAuditPartitions(ctx context.Context, dbh *sql.DB) error {
	var maxBound sql.NullString
	if err := dbh.QueryRowContext(ctx, `
SELECT MAX(PARTITION_DESCRIPTION) FROM INFORMATION_SCHEMA.PARTITIONS
 WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='audit_log'`).Scan(&maxBound); err != nil {
		return fmt.Errorf("preflight: read audit_log partitions: %w", err)
	}
	if !maxBound.Valid {
		return fmt.Errorf("preflight: audit_log has no partitions (non-partitioned table?); fix: re-apply migrations (audit_log is RANGE-partitioned by DDL) before start")
	}
	// MAX() 的字典序 = 同格式日期的时序('YYYY-MM-DD' 定长;sqltest 同款依赖)。
	bound, err := time.ParseInLocation("2006-01-02", strings.Trim(maxBound.String, "' \t"), time.UTC)
	if err != nil {
		return fmt.Errorf("preflight: parse audit_log max partition bound %q: %w", maxBound.String, err)
	}
	var today time.Time
	if err := dbh.QueryRowContext(ctx, `SELECT CAST(NOW(3) AS DATE)`).Scan(&today); err != nil {
		return fmt.Errorf("preflight: read db date: %w", err)
	}
	if horizon := today.AddDate(0, 0, 7); !bound.After(horizon) {
		slog.Warn("preflight: audit_log partition watermark below janitor horizon (+7d); reaper janitor will top up",
			"max_bound", bound.Format("2006-01-02"))
	}
	return partitionVerdict(bound, today)
}

// partitionVerdict 是水位地板的纯判定:bound 必须严格大于 today(VALUES
// LESS THAN 语义:覆盖 today 需 bound ≥ today+1;bound == today 只盖到昨天)。
func partitionVerdict(maxBound, today time.Time) error {
	if !maxBound.After(today) {
		next := today.AddDate(0, 0, 1)
		return fmt.Errorf("preflight: audit_log partition watermark %s does not cover today %s: audit INSERTs would all fail (MySQL 1526); fix: start reaper (janitor pre-creates +7d) or ALTER TABLE audit_log ADD PARTITION (PARTITION p%s VALUES LESS THAN ('%s'))",
			maxBound.Format("2006-01-02"), today.Format("2006-01-02"),
			next.Format("20060102"), next.AddDate(0, 0, 1).Format("2006-01-02"))
	}
	return nil
}

// CheckAuditAppendOnly 断言当前连接账号(业务账号 = 各入口 BRAID_MYSQL_DSN
// 的登录者)对 audit_log 无 UPDATE/DELETE 权限,按两个授权平面分别处理
// (empirical:MySQL 8 在 I_S 把 GRANT ALL 展开成逐项权限行,没有
// "ALL PRIVILEGES" 字面量,「管理账号豁免」只能按平面实现):
//
//   - 管理平面 = USER_PRIVILEGES(全局 *.*):UPDATE/DELETE 仅 warn 不拒。
//     root/运维账号在此(dev/test 栈即 root,硬拒 = 开发栈直接拒启);对
//     全局权限账号逐表强制 append-only 不可实施,归属运维。
//   - 业务平面 = SCHEMA/TABLE_PRIVILEGES(限本库/audit_log):UPDATE/DELETE
//     硬拒——部署契约即业务账号对本库只授 INSERT/SELECT(DDL §append-only),
//     GRANT ALL ON <db>.* 的展开落在这也拒(该形态就是违约配置)。
//
// 已知边界:MySQL 8 角色授予不经这三表展开;部署契约是直接授权(与 DDL
// 注释同口径)。
func CheckAuditAppendOnly(ctx context.Context, dbh *sql.DB) error {
	var acct, schema string
	if err := dbh.QueryRowContext(ctx, `SELECT CURRENT_USER(), DATABASE()`).Scan(&acct, &schema); err != nil {
		return fmt.Errorf("preflight: read current user: %w", err)
	}
	grantee := granteeOf(acct)
	global, err := privSet(ctx, dbh, acct, `
SELECT PRIVILEGE_TYPE FROM INFORMATION_SCHEMA.USER_PRIVILEGES WHERE GRANTEE = ?`, grantee)
	if err != nil {
		return err
	}
	schemaPrivs, err := privSet(ctx, dbh, acct, `
SELECT PRIVILEGE_TYPE FROM INFORMATION_SCHEMA.SCHEMA_PRIVILEGES
 WHERE GRANTEE = ? AND TABLE_SCHEMA = DATABASE()`, grantee)
	if err != nil {
		return err
	}
	tablePrivs, err := privSet(ctx, dbh, acct, `
SELECT PRIVILEGE_TYPE FROM INFORMATION_SCHEMA.TABLE_PRIVILEGES
 WHERE GRANTEE = ? AND TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'audit_log'`, grantee)
	if err != nil {
		return err
	}
	if global["UPDATE"] || global["DELETE"] {
		slog.Warn("preflight: account holds global UPDATE/DELETE (admin plane; audit append-only falls back to ops for globally privileged accounts)",
			"account", acct)
	}
	return appendOnlyVerdict(schema, acct, grantee, schemaPrivs, tablePrivs)
}

// privSet 执行权限查询并收为集合。
func privSet(ctx context.Context, dbh *sql.DB, acct, query string, args ...any) (map[string]bool, error) {
	rows, err := dbh.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("preflight: read privileges of %s: %w", acct, err)
	}
	defer func() { _ = rows.Close() }()
	privs := make(map[string]bool)
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("preflight: scan privilege row: %w", err)
		}
		privs[p] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("preflight: read privilege rows: %w", err)
	}
	return privs, nil
}

// appendOnlyVerdict 是业务平面权限断言的纯判定(表驱动单测锚):逐平面点名
// 违约权限,REVOKE 提示与授权平面严格对齐(schema 级授的权收 ON db.*,
// 表级授的收 ON db.audit_log——错平面的 REVOKE 不生效,提示必须可执行)。
func appendOnlyVerdict(schema, acct, grantee string, schemaPrivs, tablePrivs map[string]bool) error {
	planes := []struct {
		scope string
		privs map[string]bool
	}{
		{schema + ".*", schemaPrivs},
		{schema + ".audit_log", tablePrivs},
	}
	var held, revoke []string
	for _, p := range []string{"UPDATE", "DELETE"} {
		for _, pl := range planes {
			if pl.privs[p] {
				held = append(held, p+" on "+pl.scope)
				revoke = append(revoke, p+" ON "+pl.scope)
			}
		}
	}
	if len(held) == 0 {
		return nil
	}
	return fmt.Errorf("preflight: account %s holds %s covering audit_log (append-only iron law violated); fix: REVOKE %s FROM %s (business accounts get INSERT,SELECT only — migrations/0001_init.up.sql header)",
		acct, strings.Join(held, ", "), strings.Join(revoke, ", "), grantee)
}

// granteeOf 把 CURRENT_USER() 的 user@host 转成 I_S GRANTEE 列的字面量形态
// 'user'@'host'(引号是列值的一部分;内部单引号按 SQL 字面量双写)。按
// **最后一个** @ 切分:user 部可含 @,host 部是网络名不含。
func granteeOf(acct string) string {
	i := strings.LastIndexByte(acct, '@')
	if i < 0 {
		return quoteSQL(acct) // 防御:CURRENT_USER 恒带 @host
	}
	return quoteSQL(acct[:i]) + "@" + quoteSQL(acct[i+1:])
}

func quoteSQL(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
