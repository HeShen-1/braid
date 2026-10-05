// Package preflight 提供 T7.3 安装自检:错误配置下拒绝启动并输出修复提示
// (拒启动先例 = cmd/* main 的 slog.Error + os.Exit(1),本包只负责把「哪里
// 错、怎么修」说清楚)。四个自检由各 cmd 入口按自身读写面装配:
//
//   - CheckSchemaVersion(迁移版本比对):四入口全硬拒——旧 schema 上跑任何
//     组件都是未定义行为。
//   - CheckAuditAppendOnly(业务账号对 audit_log/login_audit 无 UPDATE/
//     DELETE;T7.4 扩面纳 login_audit,D-t64 ①):四入口全硬拒——append-only
//     铁律的部署层断言(DDL §append-only)。唯一豁免 = reaper 传入
//     AllowLoginAuditDelete(janitor purgeLoginAudit 保留期清理的表级
//     DELETE;audit_log 面与 UPDATE 面恒拒,授权矩阵 deploy/README.md §5)。
//   - CheckAuditPartitions(audit_log 分区水位覆盖当日):api/worker 硬拒
//     (审计写方:分区不覆盖当日则 INSERT 全部 1526);relay(零审计写)与
//     reaper(水位自愈者,janitor 首 tick 即补)仅 warn——对它们硬拒会造出
//     「没人补分区 → 永远起不来」的死锁。
//   - CheckRedisQueue(队列实例 noeviction + AOF everysec 契约):relay/
//     worker 硬拒(队列数据承载方);reaper 仅 warn(D-t25 fail-open:锁实例
//     破约的失败模式 = 锁丢失退化为 leaderless,安全,拒启反制造停摆面);
//     api 不调(零 Redis)。
//   - CheckComponentGrants(按组件授权断言,T17.3):四入口各按自身组件装配,
//     断言当前连接账号持有该组件特征写面的表级权限(断言面 = relay 的
//     outbox、reaper 的 outbox/audit_log、api/worker/reaper 的
//     snowflake_node_lease)——缺失即硬拒:least-privilege 部署下缺面的
//     失败模式 = 组件起得来、首个写路径 1142(投递状态迁移/分区回收/租约
//     acquire 全是运行期才炸),前移到启动期并给出可执行 GRANT 提示。全局
//     权限账号(dev/test 栈 root)管理平面直接过,与 CheckAuditAppendOnly
//     同判;断言面与 deploy/grants.sh 的同口径对拍锚 = TestComponentGrantsParity。
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
	"sort"
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

// CheckAuditAppendOnly 断言形态常量(第三参;T7.4,D-t64 ①)。
//
// 断言表集 = {audit_log, login_audit}:UPDATE 恒拒;DELETE 在 audit_log
// 面恒拒。唯一豁免 = AllowLoginAuditDelete(reaper:janitor purgeLoginAudit
// 保留期清理的表级 DELETE,授权矩阵 deploy/README.md §5)——且仅适用于
// 只覆盖 login_audit 的授权对象;schema 级授权(覆盖 audit_log)在任何
// 形态下都拒,强制表级精确授权。
const (
	// AppendOnlyStrict 是默认形态(api/worker/relay):两表 UPDATE/DELETE 全拒。
	AppendOnlyStrict = false
	// AllowLoginAuditDelete 是 reaper 形态:仅放行 login_audit 表级 DELETE。
	AllowLoginAuditDelete = true
)

// CheckAuditAppendOnly 断言当前连接账号(业务账号 = 各入口 BRAID_MYSQL_DSN
// 的登录者)对 audit_log/login_audit 无 UPDATE/DELETE 权限(T7.4 扩面纳
// login_audit——0005 建表后其 append-only 契约无启动期强制,D-t64 ①)。
// 授权平面分两层处理(empirical:MySQL 8 在 I_S 把 GRANT ALL 展开成逐项
// 权限行,没有 "ALL PRIVILEGES" 字面量,「管理账号豁免」只能按平面实现):
//
//   - 管理平面 = USER_PRIVILEGES(全局 *.*):UPDATE/DELETE 仅 warn 不拒。
//     root/运维账号在此(dev/test 栈即 root,硬拒 = 开发栈直接拒启);对
//     全局权限账号逐表强制 append-only 不可实施,归属运维。
//   - 业务平面 = 覆盖任一受保护表的全部授权对象,UPDATE/DELETE 硬拒:
//     schema 级(SCHEMA_PRIVILEGES)、表级(TABLE_PRIVILEGES)、列级
//     (COLUMN_PRIVILEGES——列级只有 UPDATE 面向违约,MySQL 无列级 DELETE)。
//     db/table 级 GRANT 的对象名支持 %/_ 通配(mysql.db 存原始模式,I_S
//     原样回显),`TABLE_SCHEMA = DATABASE()` 精确等值会漏行——拉全量后
//     在 Go 侧按 LIKE 语义匹配(对抗审计 F2/F3/F4)。部署契约即业务账号
//     对本库只授 INSERT/SELECT(DDL §append-only),GRANT ALL ON <db>.* 的
//     展开落业务平面 = 违约配置照拒。
//
// 已知边界:MySQL 8 角色授予不经这四表展开;部署契约是直接授权(与 DDL
// 注释同口径)。组件自身的写权限按 CheckComponentGrants 启动断言(relay 的
// outbox UPDATE、reaper 的 outbox INSERT/DELETE 与 audit_log ALTER/DROP、
// api/worker/reaper 的 snowflake_node_lease 四面——T15.2 的「预登记未断言」
// 台账已随 T17.3 收口为断言面);其余矩阵面(login_throttle、chain_heads
// 列级面等)不在启动断言,dev 期对拍锚 = TestComponentGrantsParity,授权
// 方案见 deploy/README.md §5 与 deploy/UPGRADE.md。
func CheckAuditAppendOnly(ctx context.Context, dbh *sql.DB, allowLoginAuditDelete bool) error {
	var acct, schema string
	if err := dbh.QueryRowContext(ctx, `SELECT CURRENT_USER(), DATABASE()`).Scan(&acct, &schema); err != nil {
		return fmt.Errorf("preflight: read current user: %w", err)
	}
	grantee := granteeOf(acct) // I_S GRANTEE 匹配形态(原样嵌入,见 granteeOf 注释)

	global, err := privSet(ctx, dbh, acct, `
SELECT PRIVILEGE_TYPE FROM INFORMATION_SCHEMA.USER_PRIVILEGES WHERE GRANTEE = ?`, grantee)
	if err != nil {
		return err
	}
	if global["UPDATE"] || global["DELETE"] {
		slog.Warn("preflight: account holds global UPDATE/DELETE (admin plane; audit append-only falls back to ops for globally privileged accounts)",
			"account", acct)
	}

	// 业务平面:覆盖任一受保护表的授权对象聚合(schema/表/列三平面,通配
	// 模式判定见 likeMatch;三平面共用 collectPlanes 扫描循环)。对象进
	// planes 的前提 = 覆盖 audit_log 或 login_audit 之一;coversAuditLog
	// 记录是否覆盖 audit_log(DELETE 豁免的适用性判据)。
	planes := map[string]*grantPlane{}
	add := func(on string, coversAuditLog bool, priv string) {
		p := planes[on]
		if p == nil {
			p = &grantPlane{on: on, coversAuditLog: coversAuditLog}
			planes[on] = p
		}
		p.coversAuditLog = p.coversAuditLog || coversAuditLog
		p.set(priv)
	}
	// schema 级:库模式覆盖本库 ⇒ 覆盖 audit_log(豁免永不适用)。
	if err := collectPlanes(ctx, dbh, acct, grantee, `
SELECT TABLE_SCHEMA, PRIVILEGE_TYPE FROM INFORMATION_SCHEMA.SCHEMA_PRIVILEGES`, func(row []string) error {
		if row[0] != schema && !likeMatch(row[0], schema) {
			return nil
		}
		add(tick(row[0])+".*", true, row[1])
		return nil
	}); err != nil {
		return err
	}
	// 表级(具名对象只覆盖自己;通配对象按 coversAuditLog 保守归边——
	// 覆盖 audit_log 的通配如 `%audit%` 记 coversAuditLog,豁免不适用)。
	if err := collectPlanes(ctx, dbh, acct, grantee, `
SELECT TABLE_SCHEMA, TABLE_NAME, PRIVILEGE_TYPE FROM INFORMATION_SCHEMA.TABLE_PRIVILEGES`, func(row []string) error {
		switch {
		case coversObject(row[0], schema, row[1], protectedTableAudit):
			add(tick(row[0])+"."+tick(row[1]), true, row[2])
		case coversObject(row[0], schema, row[1], protectedTableLogin):
			add(tick(row[0])+"."+tick(row[1]), false, row[2])
		}
		return nil
	}); err != nil {
		return err
	}
	// 列级(只 UPDATE 面向违约;按对象聚合列名,REVOKE 需带列清单)。
	if err := collectPlanes(ctx, dbh, acct, grantee, `
SELECT TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME, PRIVILEGE_TYPE FROM INFORMATION_SCHEMA.COLUMN_PRIVILEGES`, func(row []string) error {
		if row[3] != "UPDATE" {
			return nil
		}
		var coversAuditLog bool
		switch {
		case coversObject(row[0], schema, row[1], protectedTableAudit):
			coversAuditLog = true
		case coversObject(row[0], schema, row[1], protectedTableLogin):
		default:
			return nil
		}
		on := tick(row[0]) + "." + tick(row[1])
		p := planes[on]
		if p == nil {
			p = &grantPlane{on: on, coversAuditLog: coversAuditLog}
			planes[on] = p
		}
		p.coversAuditLog = p.coversAuditLog || coversAuditLog
		p.colUpd = append(p.colUpd, row[2])
		return nil
	}); err != nil {
		return err
	}

	ordered := make([]grantPlane, 0, len(planes))
	keys := make([]string, 0, len(planes))
	for k := range planes {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 输出与提示文本稳定,测试可锚
	for _, k := range keys {
		p := planes[k]
		sort.Strings(p.colUpd)
		ordered = append(ordered, *p)
	}
	return appendOnlyVerdict(acct, granteeSQL(acct), allowLoginAuditDelete, ordered)
}

// grantPlane 是一个授权对象(可带通配)上的违约权限聚合:REVOKE 修复提示
// 的最小单元——按对象分组出合法语句,单语句混多个 ON 子句是 1064 语法错
// (对抗审计 F7,已实证)。
type grantPlane struct {
	on     string // 授权对象字面量,如 `braid`.* / `braid%`.`audit_log`
	update bool
	delete bool
	colUpd []string // 列级 UPDATE 的列名(仅列级面)
	// coversAuditLog:对象覆盖 audit_log ⇒ DELETE 豁免(login_audit 保留期
	// 清理)不适用。对象进 planes 的前提 = 覆盖任一受保护表,false 即
	// 只覆盖 login_audit(豁免判定面,T7.4)。
	coversAuditLog bool
}

// 受保护表集(append-only 断言面;T7.4 扩面纳 login_audit,D-t64 ①)。
const (
	protectedTableAudit = "audit_log"
	protectedTableLogin = "login_audit"
)

func (p *grantPlane) set(priv string) {
	switch priv {
	case "UPDATE":
		p.update = true
	case "DELETE":
		p.delete = true
	}
}

// privSet 执行单列权限查询并收为集合。
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

// collectPlanes 拉取一个 I_S 权限平面的全量行(仅按 GRANTEE 过滤),行内容
// 的覆盖判定与聚合交给调用方回调。schema/表/列三平面共用此一处扫描循环
// (delta 复审:三份只差列数的拷贝已经开始漂移,rows.Err() 缺 preflight
// 前缀即是漂移实例)。
func collectPlanes(ctx context.Context, dbh *sql.DB, acct, grantee, query string, fn func(row []string) error) error {
	rows, err := dbh.QueryContext(ctx, query+" WHERE GRANTEE = ?", grantee)
	if err != nil {
		return fmt.Errorf("preflight: read privileges of %s: %w", acct, err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return fmt.Errorf("preflight: read privilege columns: %w", err)
	}
	for rows.Next() {
		buf := make([]string, len(cols))
		ptrs := make([]any, len(cols))
		for i := range buf {
			ptrs[i] = &buf[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return fmt.Errorf("preflight: scan privilege row: %w", err)
		}
		if err := fn(buf); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("preflight: read privilege rows: %w", err)
	}
	return nil
}

// coversObject 报告授权对象(库/表名均可含 %/_ 通配)是否覆盖实际对象。
func coversObject(schemaPat, schema, tablePat, table string) bool {
	if schemaPat != schema && !likeMatch(schemaPat, schema) {
		return false
	}
	return tablePat == table || likeMatch(tablePat, table)
}

// likeMatch 实现 SQL LIKE 语义(% 任意串、_ 单字符、\ 转义下一字符取字面;
// 库名本身区分大小写,匹配取区分大小写口径)。MySQL 对 mysql.db 的授权
// 模式匹配即此语义(my_wildcmp)。
//
// 贪心双指针 + 单点回溯(教科书 LIKE 算法):变量长通配只有 %,失配只需
// 回溯到**最后一个** % 处让它的匹配长度 +1,最坏 O(n×m)。不能用朴素回溯:
// % 密集模式是指数复杂度(delta 复审实测:`%a%a%a%a%a%b` 对 64 字符库名
// 282ms,8 组 % 超 100s),而本函数跑在四个 cmd 的启动路径上,operator
// 可控的病态模式会让组件既不起也不报错地自旋。
func likeMatch(pattern, s string) bool {
	pi, si := 0, 0
	starP, starS := -1, -1 // 最后一个 % 的模式后继位 / 文本回溯位
	for si < len(s) {
		if pi < len(pattern) {
			switch pattern[pi] {
			case '%':
				starP, starS = pi+1, si
				for pi < len(pattern) && pattern[pi] == '%' {
					pi++
				}
				continue // % 先试匹配空串,不消费文本
			case '_':
				pi++
				si++
				continue
			case '\\':
				if pi+1 < len(pattern) {
					if s[si] == pattern[pi+1] {
						pi += 2
						si++
						continue
					}
				} else if s[si] == '\\' { // 尾部孤立 \:按字面处理
					pi++
					si++
					continue
				}
			default:
				if s[si] == pattern[pi] {
					pi++
					si++
					continue
				}
			}
		}
		// 失配(或模式已尽而文本未尽):回溯到最后一个 %
		if starP < 0 {
			return false
		}
		starS++
		pi, si = starP, starS
	}
	for pi < len(pattern) && pattern[pi] == '%' { // 残余模式须可匹配空
		pi++
	}
	return pi == len(pattern)
}

// appendOnlyVerdict 是业务平面权限断言的纯判定(表驱动单测锚):逐授权对象
// 点名违约权限,REVOKE 提示与授权对象严格对齐(通配模式授的权收在同一
// 模式上——REVOKE ON braid.* 不撤销 ON `braid%`.* 的授权;列级 UPDATE 需
// 带列清单;提示必须是可直接执行的合法 SQL,对抗审计 F7)。DELETE 豁免
// (allowLoginAuditDelete)只放行「仅覆盖 login_audit」的对象——覆盖
// audit_log 的对象(schema 级、跨表通配)恒拒,强制 reaper 表级精确授权。
func appendOnlyVerdict(acct, grantee string, allowLoginAuditDelete bool, planes []grantPlane) error {
	var msgs, stmts []string
	for _, pl := range planes {
		var privs []string
		if pl.update {
			privs = append(privs, "UPDATE")
		}
		if pl.delete && (pl.coversAuditLog || !allowLoginAuditDelete) {
			privs = append(privs, "DELETE")
		}
		if len(privs) == 0 && len(pl.colUpd) == 0 {
			continue
		}
		if len(pl.colUpd) > 0 {
			msgs = append(msgs, fmt.Sprintf("UPDATE(%s) on %s", strings.Join(pl.colUpd, ","), pl.on))
			stmts = append(stmts, fmt.Sprintf("REVOKE UPDATE (%s) ON %s FROM %s",
				strings.Join(pl.colUpd, ", "), pl.on, grantee))
			continue
		}
		msgs = append(msgs, strings.Join(privs, "/")+" on "+pl.on)
		stmts = append(stmts, fmt.Sprintf("REVOKE %s ON %s FROM %s",
			strings.Join(privs, ", "), pl.on, grantee))
	}
	if len(msgs) == 0 {
		return nil
	}
	// 提示尾注限定两表面:reaper 的 login_audit DELETE 是预配的保留期清理
	// 授权(表级);relay/reaper 的其他写权限(outbox UPDATE/DELETE、
	// audit_log ALTER、audit_pending SELECT/DELETE)是另一套按组件授权,
	// 混进本提示会误导运维过度收权(对抗审计 F8);方案见 deploy/README.md
	// §5 与 deploy/UPGRADE.md(T7.4 定稿)。
	return fmt.Errorf("preflight: account %s holds %s covering audit_log/login_audit (append-only iron law violated); fix: %s (audit_log/login_audit-facing business grants: INSERT,SELECT only; reaper's table-level login_audit DELETE is the provisioned retention purge; other components' own write grants are provisioned separately — deploy/README.md §5)",
		acct, strings.Join(msgs, ", "), strings.Join(stmts, "; "))
}

// granteeOf 把 CURRENT_USER() 的 user@host 转成 I_S GRANTEE 列的字面量形态
// 'user'@'host'。按**最后一个** @ 切分(user 部可含 @,host 部是网络名不含)。
// 内部单引号**不做双写**:实测(MySQL 8.0.46,t13)服务端把含引号的账号名
// 渲染为 'we'ird'@'%'(原样嵌入、不转义),SQL 字面量式双写反而恒不匹配 →
// 权限集恒空、断言空过(对抗审计 F5)。GRANTEE 经参数占位传入,无注入面。
func granteeOf(acct string) string {
	i := strings.LastIndexByte(acct, '@')
	if i < 0 {
		return "'" + acct + "'" // 防御:CURRENT_USER 恒带 @host
	}
	return "'" + acct[:i] + "'@'" + acct[i+1:] + "'"
}

// granteeSQL 生成 REVOKE 提示里账号的 SQL 字面量(内部单引号双写)。与
// granteeOf(原始形态)是两种用途:I_S 匹配用原始形态,渲染**可执行** SQL
// 用双写——原始形态进 REVOKE 对含引号账号是 1064(delta 复审,t13 实证)。
func granteeSQL(acct string) string {
	i := strings.LastIndexByte(acct, '@')
	if i < 0 {
		return quoteSQL(acct)
	}
	return quoteSQL(acct[:i]) + "@" + quoteSQL(acct[i+1:])
}

func quoteSQL(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// tick 给标识符加反引号(内部反引号按 MySQL 惯例双写)。
func tick(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// Component 是按组件授权断言的组件(T17.3):四入口各断自身;第五组件
// sandbox-exec 零 DB 面(cmd/braid-sandbox-exec 无 MySQL 连接路径),无断言
// 可言——按组件授权方案的五账号矩阵见 deploy/README.md §5。
type Component string

const (
	ComponentAPI    Component = "api"
	ComponentWorker Component = "worker"
	ComponentRelay  Component = "relay"
	ComponentReaper Component = "reaper"
)

// componentFace 是一个组件断言面:表 + 表级全量权限集 + 用途短注(错误
// 提示的操作语境)。断言面选取 = 组件的特征写面(区分组件身份、缺面即
// 核心路径 1142 的面):relay 的投递状态迁移、reaper 的判死重派/分区回收、
// 三写入口的租约仲裁(D-t73 对抗审计登记项 + T15.2 preflight 台账,T17.3
// 收口)。逐面与 grants.sh 全量 equality(对拍锚 = TestComponentGrantsParity,
// 撤面即红)——半量断言会造出「断言绿但组件半残」的假阳性。列级面
// (chain_heads / login_throttle 的 api UPDATE)不在断言集:列级授权不落
// TABLE_PRIVILEGES,且业务语句混列级/表级,断言口径无法稳定;这两面由
// append-only 断言(反向)+ grants.sh 对拍(dev 期)承载。
type componentFace struct {
	table string
	privs []string // 声明序即错误消息序(表驱动锚)
	why   string
}

// leaseFace:雪花节点号租约仲裁面(T15.2,D-t151 决策 2)——acquire INSERT /
// 复占与过期接管 UPDATE / auto 池扫描 SELECT / SIGTERM 优雅停机释放 DELETE
// (DELETE 是设计语义;表是运行态簿记,不在 append-only 受保护表集)。
var leaseFace = componentFace{
	table: "snowflake_node_lease",
	privs: []string{"SELECT", "INSERT", "UPDATE", "DELETE"},
	why:   "snowflake node lease arbitration + SIGTERM release (T15.2)",
}

// componentRequired:组件 → 断言面集(声明序即多 face 缺失时的消息序)。
var componentRequired = map[Component][]componentFace{
	ComponentAPI:    {leaseFace},
	ComponentWorker: {leaseFace},
	ComponentRelay: {
		{table: "outbox", privs: []string{"SELECT", "UPDATE"},
			why: "delivery state transitions"},
	},
	ComponentReaper: {
		{table: "outbox", privs: []string{"SELECT", "INSERT", "DELETE"},
			why: "dead-node redispatch + janitor dispatch cleanup"},
		{table: "audit_log", privs: []string{"SELECT", "INSERT", "ALTER", "DROP"},
			why: "sync audit emission + partition retention (DROP PARTITION needs ALTER+DROP)"},
		{table: "node_attempt", privs: []string{"SELECT", "UPDATE", "DELETE"},
			why: "death-scan CAS close (UPDATE) + janitor archival retention purge (T18.6; missing grant surfaces as hourly 1142 otherwise)"},
		leaseFace,
	},
}

// CheckComponentGrants 断言当前连接账号(业务账号 = 各入口 BRAID_MYSQL_DSN
// 的登录者)持有 comp 组件断言面的表级权限(T17.3)。授权平面三层取并集
// (持有任一层即满足——与 CheckAuditAppendOnly 的拒面向判定互补,本断言
// 是正向在场断言):管理平面 = USER_PRIVILEGES(全局,dev/test root 形态
// 直接过)、schema 级 = SCHEMA_PRIVILEGES(库模式覆盖本库,通配经
// likeMatch)、表级 = TABLE_PRIVILEGES(对象覆盖判定 coversObject,通配
// aware)。列级授权只落 COLUMN_PRIVILEGES,不满足表级面(列级 UPDATE ≠
// 表级 UPDATE,业务 SQL 会 1142——有意不接受)。
func CheckComponentGrants(ctx context.Context, dbh *sql.DB, comp Component) error {
	faces := componentRequired[comp]
	if len(faces) == 0 {
		return componentGrantsVerdict("", "", "", comp, nil)
	}
	var acct, schema string
	if err := dbh.QueryRowContext(ctx, `SELECT CURRENT_USER(), DATABASE()`).Scan(&acct, &schema); err != nil {
		return fmt.Errorf("preflight: read current user: %w", err)
	}
	grantee := granteeOf(acct)

	held := map[string]map[string]bool{}
	mark := func(table, priv string) {
		if held[table] == nil {
			held[table] = map[string]bool{}
		}
		held[table][priv] = true
	}
	// 管理平面:全局持有的 priv 覆盖全部断言表。
	global, err := privSet(ctx, dbh, acct, `
SELECT PRIVILEGE_TYPE FROM INFORMATION_SCHEMA.USER_PRIVILEGES WHERE GRANTEE = ?`, grantee)
	if err != nil {
		return err
	}
	for _, f := range faces {
		for _, p := range f.privs {
			if global[p] {
				mark(f.table, p)
			}
		}
	}
	// schema 级:库模式覆盖本库即覆盖全部断言表。
	if err := collectPlanes(ctx, dbh, acct, grantee, `
SELECT TABLE_SCHEMA, PRIVILEGE_TYPE FROM INFORMATION_SCHEMA.SCHEMA_PRIVILEGES`, func(row []string) error {
		if row[0] != schema && !likeMatch(row[0], schema) {
			return nil
		}
		for _, f := range faces {
			mark(f.table, row[1])
		}
		return nil
	}); err != nil {
		return err
	}
	// 表级:对象覆盖判定(通配模式经 coversObject)。
	if err := collectPlanes(ctx, dbh, acct, grantee, `
SELECT TABLE_SCHEMA, TABLE_NAME, PRIVILEGE_TYPE FROM INFORMATION_SCHEMA.TABLE_PRIVILEGES`, func(row []string) error {
		for _, f := range faces {
			if coversObject(row[0], schema, row[1], f.table) {
				mark(f.table, row[2])
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return componentGrantsVerdict(acct, granteeSQL(acct), schema, comp, held)
}

// componentGrantsVerdict 是组件断言的纯判定(表驱动单测锚):逐 face 点名
// 缺失权限,GRANT 提示与 face 严格对齐(可执行 SQL,镜像 appendOnlyVerdict
// 的 REVOKE 提示纪律)。未知组件 = 编程错误(装配面传错),响亮拒绝而非
// 静默通过。
func componentGrantsVerdict(acct, grantee, schema string, comp Component, held map[string]map[string]bool) error {
	faces := componentRequired[comp]
	if len(faces) == 0 {
		return fmt.Errorf("preflight: unknown component %q (want api, worker, relay or reaper)", string(comp))
	}
	var msgs, stmts []string
	for _, f := range faces {
		var missing []string
		for _, p := range f.privs {
			if !held[f.table][p] {
				missing = append(missing, p)
			}
		}
		if len(missing) == 0 {
			continue
		}
		privs := strings.Join(missing, ", ")
		on := tick(schema) + "." + tick(f.table)
		msgs = append(msgs, fmt.Sprintf("%s on %s (%s)", privs, on, f.why))
		stmts = append(stmts, fmt.Sprintf("GRANT %s ON %s TO %s", privs, on, grantee))
	}
	if len(msgs) == 0 {
		return nil
	}
	return fmt.Errorf("preflight: account %s lacks privileges required by %s: %s; fix: %s; per-component matrix: deploy/README.md §5 (executable source: deploy/grants.sh)",
		acct, comp, strings.Join(msgs, "; "), strings.Join(stmts, "; "))
}
