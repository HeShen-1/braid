package preflight

// deploy/grants.sh 解析与按组件授权对拍(T17.3)。
//
// grants.sh 是按组件授权矩阵(deploy/README.md §5)的可执行真源;本文件把
// 其 heredoc SQL 解析为「账号 × 表 → 表级权限集」模型,承担:
//   1. TestGrantsShParses(离线):解析健全性锚——账号集、relay 零审计面、
//      schema_migrations 全员 SELECT 等 grants.sh 结构不变量;
//   2. TestComponentGrantsSnapshot(t13):grants.sh 的 GRANT 语句原样应用到
//      WorktreeKey 后缀账号(仅库名与账号名替换)→ SHOW GRANTS 快照 = 五账号
//      授权面快照的执行锚(含 0012 租约四面 / 0013 login_audit 面的在场实证)。
// 组件断言面与解析模型的 equality 对拍、绿/红臂见 TestComponentGrantsParity /
// TestComponentGrantsEnforcement(与本文件同包纪律:账号名拼 WorktreeKey,
// t13 共享实例防并行 worktree 互踩)。
//
// 解析边界:只消费表级 GRANT 语句;列级语句(priv 后带列清单,如 chain_heads
// 的 (last_id, last_row_hash))单独归边登记,不进表级模型——组件断言面只断
// 表级全量面,列级面是边界(见 mysql.go CheckComponentGrants 注释)。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"braid/internal/db"
)

// grantsLine 是一条表级/列级 GRANT 语句的解析结果。
type grantsLine struct {
	account string // 账号名(braid_api 等,不含 @host)
	table   string // 表名
	privs   []string
	columns bool // true = 列级语句(privs 中至少一个 priv 带列清单)
}

// grantsShPath 从本包定位 deploy/grants.sh(go.mod 为根)。
func grantsShPath(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "deploy", "grants.sh")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("locate deploy/grants.sh: go.mod not found upward")
		}
		dir = parent
	}
}

var grantsLineRe = regexp.MustCompile(`^GRANT\s+(.*?)\s+ON\s+(\S+)\s+TO\s+'([^']+)'@'%'`)

// grantsShSQLLines 提取 heredoc SQL 段的原始行并剥离行内注释(heredoc 标记/
// 终止符/注释剥离规则单点维护;grants.sh 的 GRANT 行不带字符串字面量,--
// 不歧义)。parseGrantsSh 与 applyColumnScoped 共同消费。
func grantsShSQLLines(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(grantsShPath(t))
	if err != nil {
		t.Fatalf("read deploy/grants.sh: %v", err)
	}
	var inSQL bool
	var out []string
	for _, ln := range strings.Split(string(raw), "\n") {
		ln = strings.TrimSpace(ln)
		if !inSQL {
			if strings.Contains(ln, "<<SQL") {
				inSQL = true
			}
			continue
		}
		if ln == "SQL" {
			return out
		}
		if i := strings.Index(ln, "--"); i >= 0 {
			ln = strings.TrimSpace(ln[:i])
		}
		if ln != "" {
			out = append(out, ln)
		}
	}
	t.Fatal("grants.sh: heredoc <<SQL not found")
	return nil
}

// parseGrantsSh 解析全部 GRANT 语句(CREATE USER 跳过——授权面只由 GRANT
// 语句承载)。
func parseGrantsSh(t *testing.T) []grantsLine {
	t.Helper()
	var lines []grantsLine
	for _, ln := range grantsShSQLLines(t) {
		if strings.HasPrefix(ln, "CREATE USER") {
			continue
		}
		m := grantsLineRe.FindStringSubmatch(ln)
		if m == nil {
			t.Fatalf("grants.sh: unparsable line %q", ln)
		}
		obj := strings.ReplaceAll(m[2], "`", "")
		i := strings.LastIndexByte(obj, '.')
		if i < 0 {
			t.Fatalf("grants.sh: object %q lacks schema qualifier", m[2])
		}
		gl := grantsLine{account: m[3], table: obj[i+1:]}
		for _, priv := range splitTopLevel(m[1]) {
			if strings.Contains(priv, "(") {
				gl.columns = true
				continue // 列级 priv:归边登记,不进表级权限集
			}
			gl.privs = append(gl.privs, strings.TrimSpace(priv))
		}
		lines = append(lines, gl)
	}
	if len(lines) == 0 {
		t.Fatal("grants.sh: no GRANT statements parsed")
	}
	return lines
}

// splitTopLevel 按括号深度 0 的逗号切分(列清单内的逗号不切)。
func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// grantsTableLevel 把解析行聚合为 账号 → 表 → 表级权限集(列级语句不并入)。
func grantsTableLevel(lines []grantsLine) map[string]map[string]map[string]bool {
	m := map[string]map[string]map[string]bool{}
	for _, gl := range lines {
		if gl.columns {
			continue
		}
		tm, ok := m[gl.account]
		if !ok {
			tm = map[string]map[string]bool{}
			m[gl.account] = tm
		}
		pm, ok := tm[gl.table]
		if !ok {
			pm = map[string]bool{}
			tm[gl.table] = pm
		}
		for _, p := range gl.privs {
			pm[p] = true
		}
	}
	return m
}

func TestGrantsShParses(t *testing.T) {
	lines := parseGrantsSh(t)
	tableLevel := grantsTableLevel(lines)

	// 四账号集(grants.sh 只建四个 MySQL 账号;sandbox-exec 零 DB 面,无账号
	// ——按组件授权方案的第五行在 deploy/README.md §5,不在此)。
	wantAccounts := []string{"braid_api", "braid_worker", "braid_relay", "braid_reaper"}
	got := make([]string, 0, len(tableLevel))
	for a := range tableLevel {
		got = append(got, a)
	}
	sort.Strings(got)
	sort.Strings(wantAccounts)
	if fmt.Sprint(got) != fmt.Sprint(wantAccounts) {
		t.Fatalf("accounts = %v, want %v", got, wantAccounts)
	}
	// 结构不变量:全员 schema_migrations 只读;relay 恰两面(outbox +
	// schema_migrations,零审计写是设计而非遗漏);0012 租约四面三账号在场
	// (T15.2);0013 后 login_audit 面 = api INSERT / reaper SELECT+DELETE。
	for _, a := range wantAccounts {
		if !tableLevel[a]["schema_migrations"]["SELECT"] || len(tableLevel[a]["schema_migrations"]) != 1 {
			t.Fatalf("%s: schema_migrations = %v, want exactly SELECT", a, keys(tableLevel[a]["schema_migrations"]))
		}
	}
	if rel := tableLevel["braid_relay"]; len(rel) != 2 || !rel["outbox"]["UPDATE"] || !rel["outbox"]["SELECT"] || len(rel["outbox"]) != 2 {
		t.Fatalf("relay surface = %v, want exactly outbox{SELECT,UPDATE} + schema_migrations{SELECT}", rel)
	}
	for _, a := range []string{"braid_api", "braid_worker", "braid_reaper"} {
		lease := tableLevel[a]["snowflake_node_lease"]
		for _, p := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
			if !lease[p] {
				t.Fatalf("%s: snowflake_node_lease missing %s (0012 lease face)", a, p)
			}
		}
	}
	if !tableLevel["braid_api"]["login_audit"]["INSERT"] || len(tableLevel["braid_api"]["login_audit"]) != 1 {
		t.Fatalf("api login_audit = %v, want exactly INSERT", keys(tableLevel["braid_api"]["login_audit"]))
	}
	ra := tableLevel["braid_reaper"]["login_audit"]
	if !ra["SELECT"] || !ra["DELETE"] || len(ra) != 2 {
		t.Fatalf("reaper login_audit = %v, want {SELECT,DELETE}", keys(ra))
	}
	// T18.6:reaper 对 node_attempt 的 UPDATE(判死收口)+ DELETE(留档
	// 清理)面——与 componentRequired 断言面同源。
	rna := tableLevel["braid_reaper"]["node_attempt"]
	if !rna["SELECT"] || !rna["UPDATE"] || !rna["DELETE"] || len(rna) != 3 {
		t.Fatalf("reaper node_attempt = %v, want {SELECT,UPDATE,DELETE}", keys(rna))
	}
	// 列级语句归边在位(chain_heads / login_throttle 的 api 面)。
	colLines := 0
	for _, gl := range lines {
		if gl.columns {
			colLines++
		}
	}
	if colLines == 0 {
		t.Fatal("column-scoped GRANT lines missing (chain_heads/login_throttle expected)")
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- t13 集成(栈不可达自动 skip) ----

// snapshotAccount 是一个快照账号:WorktreeKey 后缀防并行互踩(mysql_test.go
// 同款纪律),组件名取 grants.sh 原账号名的 braid_ 前缀后段。
type snapshotAccount struct {
	name string // 实建账号名(后缀形)
	base string // grants.sh 原账号名(替换源)
}

// setupSnapshotAccounts 在 preflight 测试 schema 上原样应用 grants.sh 的全部
// GRANT 语句(仅库名 braid → 测试 schema、账号名 → 后缀形替换;权限字面
// 零改动——快照口径 = grants.sh 可执行真源)。返回实建账号与有限账号 DSN。
func setupSnapshotAccounts(t *testing.T) (accounts []snapshotAccount, dsn func(snapshotAccount) string) {
	t.Helper()
	admin := openMigrated(t)
	var schema string
	if err := admin.QueryRow(`SELECT DATABASE()`).Scan(&schema); err != nil {
		t.Fatalf("read schema: %v", err)
	}
	key := db.WorktreeKey()
	for _, base := range []string{"braid_api", "braid_worker", "braid_relay", "braid_reaper"} {
		name := "braid_t173_" + strings.TrimPrefix(base, "braid_") + "_" + key
		accounts = append(accounts, snapshotAccount{name: name, base: base})
	}
	mustExec := func(q string) {
		t.Helper()
		if _, err := admin.Exec(q); err != nil {
			t.Fatalf("snapshot setup %q: %v", q, err)
		}
	}
	for _, a := range accounts {
		mustExec(fmt.Sprintf("DROP USER IF EXISTS '%s'@'%%'", a.name))
	}
	t.Cleanup(func() {
		for _, a := range accounts {
			_, _ = admin.Exec(fmt.Sprintf("DROP USER IF EXISTS '%s'@'%%'", a.name))
		}
	})
	for _, a := range accounts {
		mustExec(fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s'", a.name, snapshotPass))
	}
	for _, gl := range parseGrantsSh(t) {
		// 原语句重建:GRANT <privs> ON <schema>.<table> TO '<acct>'@'%';
		// 列级语句单独回放(applyColumnScoped,带列清单原样)。
		if gl.columns {
			continue
		}
		mustExec(fmt.Sprintf("GRANT %s ON %s.%s TO '%s'@'%%'",
			strings.Join(gl.privs, ", "), schema, gl.table, snapshotAccountOf(t, accounts, gl.account)))
	}
	applyColumnScoped(t, admin, schema, accounts)

	cfg, err := mysql.ParseDSN(db.TestServerDSN())
	if err != nil {
		t.Fatalf("parse test server DSN: %v", err)
	}
	return accounts, func(a snapshotAccount) string {
		return db.CanonicalDSN(fmt.Sprintf("%s:%s@tcp(%s)/%s", a.name, snapshotPass, cfg.Addr, schema))
	}
}

const snapshotPass = "braid_t173_pw" // t13 占位凭证,非任何真实系统凭证

func snapshotAccountOf(t *testing.T, accounts []snapshotAccount, base string) string {
	t.Helper()
	for _, a := range accounts {
		if a.base == base {
			return a.name
		}
	}
	t.Fatalf("grants.sh account %q outside setup list", base)
	return ""
}

// applyColumnScoped 回放列级 GRANT 语句(权限 + 列清单原样;解析模型只留
// 表级面,列级从原文重取)。
func applyColumnScoped(t *testing.T, admin *sql.DB, schema string, accounts []snapshotAccount) {
	t.Helper()
	for _, ln := range grantsShSQLLines(t) {
		m := grantsLineRe.FindStringSubmatch(ln)
		if m == nil || !strings.Contains(m[1], "(") {
			continue // 非列级语句
		}
		// 对象段取表名(同 parseGrantsSh 的 LastIndex 切分)。
		obj := strings.ReplaceAll(m[2], "`", "")
		i := strings.LastIndexByte(obj, '.')
		stmt := fmt.Sprintf("GRANT %s ON %s.%s TO '%s'@'%%'", m[1], schema, obj[i+1:], snapshotAccountOf(t, accounts, m[3]))
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("column-scoped grant %q: %v", stmt, err)
		}
	}
}

// snapshotFaceAnchors:快照自证的逐账号特征面(字面锚,独立于
// componentRequired——Parity 管「断言面 ↔ grants.sh 同口径」,此处管
// 「应用出的授权面确含组件特征写面」;权限串仅入失败消息,SHOW GRANTS
// 的权限渲染序不保证)。
var snapshotFaceAnchors = map[string][]struct{ table, privs string }{
	"braid_api":    {{"snowflake_node_lease", "SELECT,INSERT,UPDATE,DELETE"}},
	"braid_worker": {{"snowflake_node_lease", "SELECT,INSERT,UPDATE,DELETE"}},
	"braid_relay":  {{"outbox", "SELECT,UPDATE"}},
	"braid_reaper": {
		{"outbox", "SELECT,INSERT,DELETE"},
		{"audit_log", "SELECT,INSERT,ALTER,DROP"},
		{"node_attempt", "SELECT,UPDATE,DELETE"},
		{"snowflake_node_lease", "SELECT,INSERT,UPDATE,DELETE"},
	},
}

// TestComponentGrantsSnapshot:五账号授权面快照(含 0012/0013 新面)。
// grants.sh 语句原样应用到后缀账号,SHOW GRANTS 逐账号断言在场(快照 =
// 断言锚:后续 CheckComponentGrants 的绿臂直接消费本快照形态)。
// 第五账号 sandbox-exec 零 DB 面(无 MySQL 连接路径),快照断言其「无账号
// 可快照」——按组件授权方案的完整矩阵见 deploy/README.md §5。
func TestComponentGrantsSnapshot(t *testing.T) {
	accounts, dsn := setupSnapshotAccounts(t)
	admin := openMigrated(t)
	var schema string
	if err := admin.QueryRow(`SELECT DATABASE()`).Scan(&schema); err != nil {
		t.Fatalf("read schema: %v", err)
	}
	for _, a := range accounts {
		rows, err := admin.Query(fmt.Sprintf("SHOW GRANTS FOR '%s'@'%%'", a.name))
		if err != nil {
			t.Fatalf("show grants %s: %v", a.name, err)
		}
		var stmts []string
		for rows.Next() {
			var g string
			if err := rows.Scan(&g); err != nil {
				_ = rows.Close()
				t.Fatalf("scan grant row: %v", err)
			}
			stmts = append(stmts, g)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatalf("show grants rows: %v", err)
		}
		_ = rows.Close()
		if len(stmts) == 0 {
			t.Fatalf("%s: no grants after apply", a.name)
		}
		joined := strings.Join(stmts, "\n")
		// 逐账号自证特征面在场(不全外置到 Parity/Enforcement):组件
		// 特征写面在快照输出上点名——面缺 = 应用面本身漏授,不是断言漂移。
		for _, want := range snapshotFaceAnchors[a.base] {
			if !strings.Contains(joined, fmt.Sprintf("ON `%s`.`%s`", schema, want.table)) {
				t.Fatalf("%s: snapshot lacks %s face (want %s on `%s`.`%s`)", a.base, want.table, want.privs, schema, want.table)
			}
		}
		for _, g := range stmts {
			t.Logf("snapshot %s: %s", a.name, g)
		}
	}
	// 快照消费面:后缀账号可连、可见本 schema(为绿臂预验 DSN 形态)。
	limited, err := sql.Open("mysql", dsn(accounts[0]))
	if err != nil {
		t.Fatalf("open limited account: %v", err)
	}
	t.Cleanup(func() { _ = limited.Close() })
	var one int
	if err := limited.QueryRow(`SELECT 1`).Scan(&one); err != nil || one != 1 {
		t.Fatalf("limited account probe: one=%d err=%v", one, err)
	}
}

// TestComponentGrantsParity:断言面 ↔ grants.sh 同口径对拍(离线,无栈)。
// 逐组件逐表 equality(双向):grants.sh 多授/漏授任何一面都红——防「文档
// 一套、SQL 一套、断言第三套」(issue #146 流程注记 1)。断言面外的表
// (如 login_throttle、api 的 chain_heads 列级面)不在比对集,是登记边界
// (mysql.go CheckComponentGrants 注释),不属漂移。
func TestComponentGrantsParity(t *testing.T) {
	tableLevel := grantsTableLevel(parseGrantsSh(t))
	acctOf := map[Component]string{
		ComponentAPI:    "braid_api",
		ComponentWorker: "braid_worker",
		ComponentRelay:  "braid_relay",
		ComponentReaper: "braid_reaper",
	}
	if len(componentRequired) != len(acctOf) {
		t.Fatalf("componentRequired covers %d components, want %d", len(componentRequired), len(acctOf))
	}
	for comp, acct := range acctOf {
		faces, ok := componentRequired[comp]
		if !ok || len(faces) == 0 {
			t.Fatalf("component %s: no required faces", comp)
		}
		for _, f := range faces {
			got := tableLevel[acct][f.table]
			want := map[string]bool{}
			for _, p := range f.privs {
				want[p] = true
			}
			for _, p := range keys(want) {
				if !got[p] {
					t.Fatalf("%s: grants.sh lacks %s on %s (assertion face drift)", comp, p, f.table)
				}
			}
			for _, p := range keys(got) {
				if !want[p] {
					t.Fatalf("%s: grants.sh grants %s on %s beyond assertion face (narrow the face or drop the grant)", comp, p, f.table)
				}
			}
		}
	}
}

// TestComponentGrantsVerdict:纯判定层(离线,表驱动)。
func TestComponentGrantsVerdict(t *testing.T) {
	lease := map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true}
	tests := []struct {
		name    string
		comp    Component
		held    map[string]map[string]bool
		wantSub []string // 空 = 期望通过
	}{
		{
			name: "relay green",
			comp: ComponentRelay,
			held: map[string]map[string]bool{"outbox": {"SELECT": true, "UPDATE": true}},
		},
		{
			name:    "relay missing outbox update",
			comp:    ComponentRelay,
			held:    map[string]map[string]bool{"outbox": {"SELECT": true}},
			wantSub: []string{"UPDATE on `sch`.`outbox`", "GRANT UPDATE ON `sch`.`outbox` TO 'app'@'%'", "relay", "deploy/README.md §5"},
		},
		{
			name: "api green (lease only)",
			comp: ComponentAPI,
			held: map[string]map[string]bool{"snowflake_node_lease": lease},
		},
		{
			name:    "api missing lease delete",
			comp:    ComponentAPI,
			held:    map[string]map[string]bool{"snowflake_node_lease": {"SELECT": true, "INSERT": true, "UPDATE": true}},
			wantSub: []string{"DELETE on `sch`.`snowflake_node_lease`", "GRANT DELETE ON `sch`.`snowflake_node_lease`"},
		},
		{
			name: "reaper green",
			comp: ComponentReaper,
			held: map[string]map[string]bool{
				"outbox":               {"SELECT": true, "INSERT": true, "DELETE": true},
				"audit_log":            {"SELECT": true, "INSERT": true, "ALTER": true, "DROP": true},
				"node_attempt":         {"SELECT": true, "UPDATE": true, "DELETE": true},
				"snowflake_node_lease": lease,
			},
		},
		{
			// 同表双缺:一条 face 消息、一条 GRANT 提示(分组纪律镜像
			// appendOnlyVerdict 的按对象分组,对抗审计 F7 同族)。
			name: "reaper missing audit_log alter and drop",
			comp: ComponentReaper,
			held: map[string]map[string]bool{
				"outbox":               {"SELECT": true, "INSERT": true, "DELETE": true},
				"audit_log":            {"SELECT": true, "INSERT": true},
				"node_attempt":         {"SELECT": true, "UPDATE": true, "DELETE": true},
				"snowflake_node_lease": lease,
			},
			wantSub: []string{"ALTER, DROP on `sch`.`audit_log`", "GRANT ALTER, DROP ON `sch`.`audit_log` TO 'app'@'%'"},
		},
		{
			// 多 face 缺失:同错并列(排序 = componentRequired 声明序,可锚)。
			name: "reaper two faces missing",
			comp: ComponentReaper,
			held: map[string]map[string]bool{
				"outbox":               {"SELECT": true},
				"audit_log":            {"SELECT": true, "INSERT": true, "ALTER": true, "DROP": true},
				"node_attempt":         {"SELECT": true, "UPDATE": true, "DELETE": true},
				"snowflake_node_lease": {"SELECT": true},
			},
			wantSub: []string{"INSERT, DELETE on `sch`.`outbox`", "INSERT, UPDATE, DELETE on `sch`.`snowflake_node_lease`", "; "},
		},
		{
			// T18.6 断言面单缺:node_attempt DELETE(留档清理)撤面即拒启,
			// why 链路可见。
			name: "reaper missing node_attempt delete",
			comp: ComponentReaper,
			held: map[string]map[string]bool{
				"outbox":               {"SELECT": true, "INSERT": true, "DELETE": true},
				"audit_log":            {"SELECT": true, "INSERT": true, "ALTER": true, "DROP": true},
				"node_attempt":         {"SELECT": true, "UPDATE": true},
				"snowflake_node_lease": lease,
			},
			wantSub: []string{"DELETE on `sch`.`node_attempt`", "GRANT DELETE ON `sch`.`node_attempt`"},
		},
		{
			// 列级 UPDATE 不满足表级面:held 集只有表级形态,列级面天然不在
			// 其中(边界注释的判定层锚)。
			name:    "worker lease partially held",
			comp:    ComponentWorker,
			held:    map[string]map[string]bool{"snowflake_node_lease": {"SELECT": true, "UPDATE": true}},
			wantSub: []string{"INSERT, DELETE on `sch`.`snowflake_node_lease`"},
		},
		{
			name:    "unknown component",
			comp:    Component("nosuch"),
			wantSub: []string{"unknown component"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := componentGrantsVerdict("app@%", "'app'@'%'", "sch", tt.comp, tt.held)
			assertSubstrings(t, err, tt.wantSub)
		})
	}
}

// TestComponentGrantsEnforcement(t13):快照账号绿臂 + 单权收回红臂。
// 红臂逐组件覆盖:relay 的 outbox UPDATE、reaper 的 outbox DELETE 与
// audit_log ALTER、api/worker 的 lease DELETE/INSERT——grants.sh 撤面后
// 启动断言必须响亮点名(错误文本锚 = verdict 表驱动的集成镜像)。
func TestComponentGrantsEnforcement(t *testing.T) {
	accounts, dsn := setupSnapshotAccounts(t)
	admin := openMigrated(t)
	ctx := context.Background()
	var schema string
	if err := admin.QueryRow(`SELECT DATABASE()`).Scan(&schema); err != nil {
		t.Fatalf("read schema: %v", err)
	}
	byBase := map[string]snapshotAccount{}
	for _, a := range accounts {
		byBase[a.base] = a
	}
	mustExec := func(t *testing.T, q string) {
		t.Helper()
		if _, err := admin.Exec(q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}

	// 绿臂:grants.sh 原样授权下四组件断言全过。
	for comp, base := range map[Component]string{
		ComponentAPI:    "braid_api",
		ComponentWorker: "braid_worker",
		ComponentRelay:  "braid_relay",
		ComponentReaper: "braid_reaper",
	} {
		limited, err := sql.Open("mysql", dsn(byBase[base]))
		if err != nil {
			t.Fatalf("open %s: %v", base, err)
		}
		if err := CheckComponentGrants(ctx, limited, comp); err != nil {
			t.Fatalf("%s green arm: %v", comp, err)
		}
		_ = limited.Close()
	}
	// root 绿臂:全局权限(管理平面)满足一切在场断言,dev/test 栈零破坏。
	for _, comp := range []Component{ComponentAPI, ComponentWorker, ComponentRelay, ComponentReaper} {
		if err := CheckComponentGrants(ctx, admin, comp); err != nil {
			t.Fatalf("root green arm %s: %v", comp, err)
		}
	}

	// 红臂:单权收回 → 断言红 → 复授 → 断言复绿(逐臂自愈,臂间零残留)。
	steps := []struct {
		name    string
		comp    Component
		base    string
		revoke  string // PRIV ON schema.table(不带 TO)
		wantSub []string
	}{
		{
			name: "relay missing outbox update", comp: ComponentRelay, base: "braid_relay",
			revoke:  "UPDATE ON " + schema + ".outbox",
			wantSub: []string{"UPDATE on `" + schema + "`.`outbox`", "GRANT UPDATE ON `" + schema + "`.`outbox`"},
		},
		{
			name: "reaper missing outbox delete", comp: ComponentReaper, base: "braid_reaper",
			revoke:  "DELETE ON " + schema + ".outbox",
			wantSub: []string{"DELETE on `" + schema + "`.`outbox`"},
		},
		{
			name: "reaper missing audit_log alter", comp: ComponentReaper, base: "braid_reaper",
			revoke:  "ALTER ON " + schema + ".audit_log",
			wantSub: []string{"ALTER on `" + schema + "`.`audit_log`", "retention"},
		},
		{
			name: "reaper missing node_attempt delete", comp: ComponentReaper, base: "braid_reaper",
			revoke:  "DELETE ON " + schema + ".node_attempt",
			wantSub: []string{"DELETE on `" + schema + "`.`node_attempt`"},
		},
		{
			name: "api missing lease delete", comp: ComponentAPI, base: "braid_api",
			revoke:  "DELETE ON " + schema + ".snowflake_node_lease",
			wantSub: []string{"DELETE on `" + schema + "`.`snowflake_node_lease`"},
		},
		{
			name: "worker missing lease insert", comp: ComponentWorker, base: "braid_worker",
			revoke:  "INSERT ON " + schema + ".snowflake_node_lease",
			wantSub: []string{"INSERT on `" + schema + "`.`snowflake_node_lease`"},
		},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			acct := byBase[st.base]
			// 复授先挂 Cleanup:红臂任一断言失败也不把撤面泄给同账号的
			// 后续臂(reaper 两臂共用账号;重复 GRANT 幂等)。
			t.Cleanup(func() {
				_, _ = admin.Exec(fmt.Sprintf("GRANT %s TO '%s'@'%%'", st.revoke, acct.name))
			})
			mustExec(t, fmt.Sprintf("REVOKE %s FROM '%s'@'%%'", st.revoke, acct.name))
			limited, err := sql.Open("mysql", dsn(acct))
			if err != nil {
				t.Fatalf("open %s: %v", acct.name, err)
			}
			defer func() { _ = limited.Close() }()
			err = CheckComponentGrants(ctx, limited, st.comp)
			assertSubstrings(t, err, st.wantSub)
			if err == nil {
				t.Fatalf("%s: expected rejection after REVOKE", st.name)
			}
			mustExec(t, fmt.Sprintf("GRANT %s TO '%s'@'%%'", st.revoke, acct.name))
			if err := CheckComponentGrants(ctx, limited, st.comp); err != nil {
				t.Fatalf("%s: restore arm still red: %v", st.name, err)
			}
		})
	}
}
