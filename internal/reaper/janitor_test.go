package reaper

// 用例 12-13(janitor):分区水位维持(满水位零 DDL / 尾部缺口补齐 /
// p_min 幸存)+ outbox DISPATCHED 清理(双列谓词、DEAD·PENDING 永不清)。
// 日期全部动态(读 DB 当日水位推导),不硬编码引导分区日期。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

func readPartitions(t *testing.T, d *sql.DB) map[string]string {
	t.Helper()
	rows, err := d.Query(`
SELECT PARTITION_NAME, PARTITION_DESCRIPTION
  FROM INFORMATION_SCHEMA.PARTITIONS
 WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='audit_log'`)
	if err != nil {
		t.Fatalf("read partitions: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var n, b string
		if err := rows.Scan(&n, &b); err != nil {
			t.Fatalf("scan partition: %v", err)
		}
		out[n] = strings.Trim(b, "'\\t ")
	}
	return out
}

func dbToday(t *testing.T, d *sql.DB) time.Time {
	t.Helper()
	var today time.Time
	if err := d.QueryRow(`SELECT CAST(NOW(3) AS DATE)`).Scan(&today); err != nil {
		t.Fatalf("read today: %v", err)
	}
	return today
}

// TestJanitorFullHorizonNoDDL:steady state 一轮零 DDL(分区集不变)。
// 先收敛一轮:满水位只约束 ADD 侧零 DDL,回收侧在引导分区跨过测试保留期
// (24h)时合法 DROP——fresh schema 首跑日(边界日 = today−1d)恰好触发;
// 判据收紧为「收敛后第二轮零 DDL」,前提自建,不再依赖全局测试时序。
func TestJanitorFullHorizonNoDDL(t *testing.T) {
	r, d := newReaper(t, testCfg())
	if err := r.janitorOnce(context.Background()); err != nil {
		t.Fatalf("janitorOnce (converge): %v", err)
	}
	before := readPartitions(t, d)
	if err := r.janitorOnce(context.Background()); err != nil {
		t.Fatalf("janitorOnce: %v", err)
	}
	after := readPartitions(t, d)
	if len(before) != len(after) {
		t.Fatalf("partitions %d → %d(收敛后满水位一轮应零 DDL)", len(before), len(after))
	}
}

// TestJanitorBackfillsHorizon:削掉尾部至水位不足 → 一轮补齐至 today+7。
func TestJanitorBackfillsHorizon(t *testing.T) {
	r, d := newReaper(t, testCfg())
	today := dbToday(t, d)

	// 削尾:DROp 所有边界 > today+6 的分区,使最大边界 ≤ today+6 < today+8
	// (水位不足,触发补齐);保留足够引导分区,不碰 p_min。
	dropped := 0
	for name, bound := range readPartitions(t, d) {
		if name == "p_min" {
			continue
		}
		b, err := time.Parse("2006-01-02", bound)
		if err != nil {
			continue
		}
		if b.After(today.AddDate(0, 0, 6)) {
			if _, err := d.Exec(fmt.Sprintf("ALTER TABLE audit_log DROP PARTITION %s", name)); err != nil {
				t.Fatalf("drop %s: %v", name, err)
			}
			dropped++
		}
	}
	t.Cleanup(func() {
		// 补齐残留(测试中途失败也保水位):再跑一轮。
		_ = r.janitorOnce(context.Background())
	})
	if err := r.janitorOnce(context.Background()); err != nil {
		t.Fatalf("janitorOnce: %v", err)
	}
	after := readPartitions(t, d)
	if _, ok := after["p_min"]; !ok {
		t.Fatal("p_min 被 DROP(时钟漂移兜底分区永不回收)")
	}
	// 补齐判据:存在边界 ≥ today+8 的分区。
	want := today.AddDate(0, 0, 8).Format("2006-01-02")
	ok := false
	for _, bound := range after {
		if bound >= want {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("backfill 未补齐水位: 无边界 ≥ %s(got %v, dropped %d)", want, after, dropped)
	}
}

// TestPurgeOutbox:7d 删 / 7d 内留 / DEAD·PENDING 留 / 双列谓词
// (available 旧 + dispatched 新 → 留)。
func TestPurgeOutbox(t *testing.T) {
	r, d := newReaper(t, testCfg())
	cfg := testCfg()
	cfg.OutboxBatch = 2
	seed := func(status string, availOld, dispOld bool, extra string) uint64 {
		t.Helper()
		id := nextID(t)
		avail, disp := "NOW(3)", "NOW(3)"
		if availOld {
			avail = "NOW(3) - INTERVAL 8 DAY"
		}
		if dispOld {
			disp = "NOW(3) - INTERVAL 8 DAY"
		}
		if _, err := d.Exec(fmt.Sprintf(`
INSERT INTO outbox (id, execution_id, event_type, task_id, queue, payload, status, available_at, dispatched_at%s)
VALUES (?, 0, 'node.task_enqueue', ?, 'reap-test', '{}', ?, %s, %s)`, extra, avail, disp),
			id, fmt.Sprintf("att:%d:1", id), status); err != nil {
			t.Fatalf("seed outbox: %v", err)
		}
		return id
	}
	oldDispatched := seed("DISPATCHED", true, true, "")
	newDispatched := seed("DISPATCHED", false, false, "")
	deadRow := seed("DEAD", true, true, "")
	pendingRow := seed("PENDING", true, true, "")
	mixedRow := seed("DISPATCHED", true, false, "") // available 旧 + dispatched 新

	conn, err := d.Conn(context.Background())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := r.purgeOutbox(context.Background(), conn); err != nil {
		t.Fatalf("purgeOutbox: %v", err)
	}
	count := func(id uint64) int {
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM outbox WHERE id=?`, id).Scan(&n); err != nil {
			t.Fatalf("count %d: %v", id, err)
		}
		return n
	}
	if count(oldDispatched) != 0 {
		t.Fatal("超期 DISPATCHED 应被清理")
	}
	for _, id := range []uint64{newDispatched, deadRow, pendingRow, mixedRow} {
		if count(id) != 1 {
			t.Fatalf("row %d 被误删(保留期/状态/双列谓词)", id)
		}
	}
}

// seedLoginAudit:一行登录事件(event_at 相对 NOW 的偏移经 SQL 字面量表达,
// 与库端 NOW(3) 同钟系;id 走 nextID 池)。
func seedLoginAudit(t *testing.T, d *sql.DB, ageExpr string) uint64 {
	t.Helper()
	id := nextID(t)
	if _, err := d.Exec(fmt.Sprintf(`
INSERT INTO login_audit (id, event_type, username, client_ip, event_at)
VALUES (?, 'login.failed', 'purge-test', '127.0.0.1', %s)`, ageExpr), id); err != nil {
		t.Fatalf("seed login_audit: %v", err)
	}
	return id
}

func countLoginAudit(t *testing.T, d *sql.DB, id uint64) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM login_audit WHERE id=?`, id).Scan(&n); err != nil {
		t.Fatalf("count login_audit %d: %v", id, err)
	}
	return n
}

// TestPurgeLoginAudit:超期删 / 保留期内留(migration 0005 登记的保留期
// 接线,T7.1 落地)。retention=24h:testCfg 基线;25h 旧行删、23h 新行留。
func TestPurgeLoginAudit(t *testing.T) {
	r, d := newReaper(t, testCfg())
	oldRow := seedLoginAudit(t, d, "NOW(3) - INTERVAL 25 HOUR")
	nearRow := seedLoginAudit(t, d, "NOW(3) - INTERVAL 23 HOUR")

	conn, err := d.Conn(context.Background())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := r.purgeLoginAudit(context.Background(), conn); err != nil {
		t.Fatalf("purgeLoginAudit: %v", err)
	}
	if countLoginAudit(t, d, oldRow) != 0 {
		t.Fatal("超期 login_audit 应被清理")
	}
	if countLoginAudit(t, d, nearRow) != 1 {
		t.Fatal("保留期内 login_audit 被误删")
	}
}

// TestPurgeLoginAuditBatchConvergence:超单批常量(500)的存量在轮内
// 批间连搬收敛清空(批上限不截断成多轮等待)。
func TestPurgeLoginAuditBatchConvergence(t *testing.T) {
	r, d := newReaper(t, testCfg())
	const rows = loginAuditBatch + 10
	var b strings.Builder
	b.WriteString(`INSERT INTO login_audit (id, event_type, username, client_ip, event_at) VALUES `)
	args := make([]any, 0, rows)
	for i := 0; i < rows; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("(?, 'login.failed', 'purge-batch', '127.0.0.1', NOW(3) - INTERVAL 25 HOUR)")
		args = append(args, nextID(t))
	}
	if _, err := d.Exec(b.String(), args...); err != nil {
		t.Fatalf("seed batch: %v", err)
	}
	conn, err := d.Conn(context.Background())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := r.purgeLoginAudit(context.Background(), conn); err != nil {
		t.Fatalf("purgeLoginAudit: %v", err)
	}
	var left int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM login_audit WHERE username='purge-batch'`).Scan(&left); err != nil {
		t.Fatalf("count left: %v", err)
	}
	if left != 0 {
		t.Fatalf("批量未收敛:%d 行残留(want 0,%d 行两批内清空)", left, rows)
	}
}

// TestJanitorOnceIsolatesPurgeFailures:errors.Join 接线的行为锚——login_audit
// 清理必败(表临时改名,1146 非 Transient)时:janitorOnce 返回含 login_audit
// 的合并错误,且预埋的超期 outbox 行仍被清掉(隔离是双向的,删掉 Join 任一半
// 或回退成单 return 都会红)。
func TestJanitorOnceIsolatesPurgeFailures(t *testing.T) {
	r, d := newReaper(t, testCfg())
	id := nextID(t)
	if _, err := d.Exec(`
INSERT INTO outbox (id, execution_id, event_type, task_id, queue, payload, status, available_at, dispatched_at)
VALUES (?, 0, 'node.task_enqueue', ?, 'reap-test', '{}', 'DISPATCHED',
        NOW(3) - INTERVAL 8 DAY, NOW(3) - INTERVAL 8 DAY)`,
		id, fmt.Sprintf("att:%d:1", id)); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	if _, err := d.Exec(`RENAME TABLE login_audit TO login_audit_x`); err != nil {
		t.Fatalf("rename login_audit: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`RENAME TABLE login_audit_x TO login_audit`) })
	err := r.janitorOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "purge login_audit") {
		t.Fatalf("janitorOnce err = %v, want containing 'purge login_audit'", err)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM outbox WHERE id=?`, id).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if n != 0 {
		t.Fatal("outbox 清理被 login_audit 失败阻断(errors.Join 隔离回归)")
	}
}

// TestPurgeLoginThrottleHorizon:超龄防御行(> loginThrottleHorizon 30d)
// 回收、保留期内行保留;行生命周期与锁定语义无关(死重回收,镜像
// purgeLoginAudit 保留期测试形态)。
func TestPurgeLoginThrottleHorizon(t *testing.T) {
	r, d := newReaper(t, testCfg())
	if _, err := d.Exec(`DELETE FROM login_throttle`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := d.Exec(`
INSERT INTO login_throttle (throttle_key, kind, window_start, count, locked_until, updated_at) VALUES
  ('u:old',   'username', NOW(3) - INTERVAL 40 DAY, 2, NULL, NOW(3) - INTERVAL 40 DAY),
  ('u:fresh', 'username', NOW(3), 1, NULL, NOW(3))`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	conn, err := d.Conn(context.Background())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := r.purgeLoginThrottle(context.Background(), conn); err != nil {
		t.Fatalf("purgeLoginThrottle: %v", err)
	}
	for _, q := range []struct {
		key  string
		want int
	}{
		{"u:old", 0}, {"u:fresh", 1},
	} {
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM login_throttle WHERE throttle_key = ?`, q.key).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", q.key, err)
		}
		if n != q.want {
			t.Fatalf("%s: %d 行残留, want %d", q.key, n, q.want)
		}
	}
}

// TestPurgeLoginThrottleBatchConvergence:超单批常量(500)的存量在轮内
// 批间连搬收敛清空(镜像 login_audit 批收敛测试;审计 MEDIUM)。
func TestPurgeLoginThrottleBatchConvergence(t *testing.T) {
	r, d := newReaper(t, testCfg())
	if _, err := d.Exec(`DELETE FROM login_throttle`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	const rows = loginThrottleBatch + 10
	var b strings.Builder
	args := make([]any, 0, rows)
	b.WriteString(`INSERT INTO login_throttle (throttle_key, kind, window_start, count, updated_at) VALUES `)
	for i := 0; i < rows; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("(?, 'username', NOW(3) - INTERVAL 40 DAY, 1, NOW(3) - INTERVAL 40 DAY)")
		args = append(args, fmt.Sprintf("u:purgbatch-%d", i))
	}
	if _, err := d.Exec(b.String(), args...); err != nil {
		t.Fatalf("seed batch: %v", err)
	}
	conn, err := d.Conn(context.Background())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := r.purgeLoginThrottle(context.Background(), conn); err != nil {
		t.Fatalf("purgeLoginThrottle: %v", err)
	}
	var left int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM login_throttle WHERE throttle_key LIKE 'u:purgbatch-%'`).Scan(&left); err != nil {
		t.Fatalf("count left: %v", err)
	}
	if left != 0 {
		t.Fatalf("批量未收敛:%d 行残留(want 0,%d 行两批内清空)", left, rows)
	}
}

// TestPurgeLoginThrottleBoundaryAndGuard:恰界对(horizon 漂移/谓词放宽
// 变异锚)+ 锁定期守卫(审计 MEDIUM:锁定期未满的行永不回收)。
func TestPurgeLoginThrottleBoundaryAndGuard(t *testing.T) {
	r, d := newReaper(t, testCfg())
	if _, err := d.Exec(`DELETE FROM login_throttle`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	// 界对(orch-review rev4 HIGH 修订:恰 =NOW−30d 的种子在种子/purge
	// 同毫秒量子化下严格 < 不删,实测 ~30% flake——两侧各退 1s 钉
	// horizon ±1s 带宽,仍杀 horizon 漂移与谓词放宽变异):
	// B−1s 必删、B+1s 必留。
	if _, err := d.Exec(`
INSERT INTO login_throttle (throttle_key, kind, window_start, count, updated_at) VALUES
  ('u:edge-old',   'username', NOW(3) - INTERVAL 30 DAY - INTERVAL 1 SECOND, 1, NOW(3) - INTERVAL 30 DAY - INTERVAL 1 SECOND),
  ('u:edge-keep',  'username', NOW(3) - INTERVAL 30 DAY + INTERVAL 1 SECOND, 1, NOW(3) - INTERVAL 30 DAY + INTERVAL 1 SECOND),
  ('u:locked-keep','username', NOW(3) - INTERVAL 40 DAY, 0, NOW(3) - INTERVAL 40 DAY)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := d.Exec(
		`UPDATE login_throttle SET locked_until = NOW(3) + INTERVAL 1 HOUR WHERE throttle_key = 'u:locked-keep'`); err != nil {
		t.Fatalf("set lock: %v", err)
	}
	conn, err := d.Conn(context.Background())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := r.purgeLoginThrottle(context.Background(), conn); err != nil {
		t.Fatalf("purgeLoginThrottle: %v", err)
	}
	for _, q := range []struct {
		key  string
		want int
	}{
		{"u:edge-old", 0},    // 界外 1s 必删
		{"u:edge-keep", 1},   // 界内 1s 必留
		{"u:locked-keep", 1}, // 超龄但锁定期未满:守卫保留
	} {
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM login_throttle WHERE throttle_key = ?`, q.key).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", q.key, err)
		}
		if n != q.want {
			t.Fatalf("%s: %d 行残留, want %d", q.key, n, q.want)
		}
	}
}

// seedAttemptChain 在既有 execution 下种 node_execution + node_attempt 各
// 一行(T18.6 清理面测试):attempt 状态、started_at 与 heartbeat_at 年龄
// 分列可控(时钟列锚:两列同龄的种子杀不掉「谓词换列」变异);清理链复用
// seedExecRow 已登记的按 execution_id 级联(不重复登记)。
func seedAttemptChain(t *testing.T, d *sql.DB, execID uint64, attStatus, startedExpr, hbExpr string) uint64 {
	t.Helper()
	nodeID := nextID(t)
	attID := nextID(t)
	if _, err := d.Exec(`INSERT INTO node_execution
	  (id, execution_id, node_id, type, status, attempts, max_attempts, worker_id, version, deadline_at)
	  VALUES (?, ?, ?, 'llm', 'FAILED', 1, 1, 'seed-dispatch', 1, NOW(3) + INTERVAL 30 MINUTE)`,
		nodeID, execID, fmt.Sprintf("n%d", nodeID)); err != nil {
		t.Fatalf("seed node_execution: %v", err)
	}
	taskID := fmt.Sprintf("att:%d:1", nodeID)
	if _, err := d.Exec(fmt.Sprintf(`
INSERT INTO node_attempt
  (id, node_execution_id, execution_id, attempt_no, worker_id, status,
   idempotency_key, asynq_task_id, started_at, heartbeat_at)
  VALUES (?,?,?,?,?,'%s',?,?,%s,%s)`, attStatus, startedExpr, hbExpr),
		attID, nodeID, execID, 1, "w-purge", taskID, taskID); err != nil {
		t.Fatalf("seed node_attempt: %v", err)
	}
	return attID
}

func countAttempt(t *testing.T, d *sql.DB, id uint64) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM node_attempt WHERE id=?`, id).Scan(&n); err != nil {
		t.Fatalf("count node_attempt %d: %v", id, err)
	}
	return n
}

// TestPurgeNodeAttemptRetention:T18.6 留档清理的三重守卫——终态谓词
// (RUNNING 永不删)+ 保留期(时钟列 = heartbeat_at,非 started_at)+
// 活执行守卫(镜像 audit 分区:非终态执行钉住留档,保留期弯曲;守卫集
// 三态逐态锚,缺任一态的集合收窄变异即红)。
func TestPurgeNodeAttemptRetention(t *testing.T) {
	r, d := newReaper(t, testCfg())
	execTerminal, _ := seedExecRow(t, d, "FAILED", 0)
	execPending, _ := seedExecRow(t, d, "PENDING", 0)
	execRunning, _ := seedExecRow(t, d, "RUNNING", 0)
	execWaiting, _ := seedExecRow(t, d, "WAITING_APPROVAL", 0)

	const old = "NOW(3) - INTERVAL 25 HOUR"
	cases := []struct {
		name        string
		execID      uint64
		attStatus   string
		startedExpr string
		hbExpr      string
		wantSurvive bool
	}{
		{"terminal_exec_old_terminal_attempt", execTerminal, "SUCCEEDED", old, old, false},
		{"terminal_exec_old_dead_attempt", execTerminal, "DEAD", old, old, false},
		{"terminal_exec_fresh_attempt", execTerminal, "SUCCEEDED", "NOW(3)", "NOW(3)", true},
		{"terminal_exec_old_running_attempt", execTerminal, "RUNNING", old, old, true},  // RUNNING 永不删:判死属扫描②
		{"pending_exec_old_terminal_attempt", execPending, "SUCCEEDED", old, old, true}, // 活执行守卫①
		{"running_exec_old_terminal_attempt", execRunning, "SUCCEEDED", old, old, true}, // 活执行守卫②
		{"waiting_exec_old_terminal_attempt", execWaiting, "SUCCEEDED", old, old, true}, // 活执行守卫③
		// 时钟列锚:长跑 attempt(started 早于冻结心跳 7h)按 heartbeat_at
		// 口径在窗内必存活——谓词误换 started_at(D-t186 决策 4 否决项)即红。
		{"clock_started_old_hb_fresh", execTerminal, "SUCCEEDED",
			"NOW(3) - INTERVAL 30 HOUR", "NOW(3) - INTERVAL 23 HOUR", true},
	}
	ids := make(map[string]uint64, len(cases))
	for _, c := range cases {
		ids[c.name] = seedAttemptChain(t, d, c.execID, c.attStatus, c.startedExpr, c.hbExpr)
	}

	conn, err := d.Conn(context.Background())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := r.purgeNodeAttempt(context.Background(), conn); err != nil {
		t.Fatalf("purgeNodeAttempt: %v", err)
	}
	for _, c := range cases {
		if got := countAttempt(t, d, ids[c.name]); (got == 1) != c.wantSurvive {
			t.Fatalf("%s: survive=%v, want %v", c.name, got == 1, c.wantSurvive)
		}
	}
}

// TestPurgeNodeAttemptBoundary:恰界对(horizon 漂移/谓词放宽变异锚)——
// B−1s 必删、B+1s 必留(±1s 带宽钉严格 <,镜像 login_throttle 界对的
// 同毫秒量子化教训,orch-review rev4)。
func TestPurgeNodeAttemptBoundary(t *testing.T) {
	r, d := newReaper(t, testCfg())
	execID, _ := seedExecRow(t, d, "FAILED", 0)
	oldEdge := seedAttemptChain(t, d, execID, "FAILED",
		"NOW(3) - INTERVAL 24 HOUR - INTERVAL 1 SECOND",
		"NOW(3) - INTERVAL 24 HOUR - INTERVAL 1 SECOND")
	keepEdge := seedAttemptChain(t, d, execID, "FAILED",
		"NOW(3) - INTERVAL 24 HOUR + INTERVAL 1 SECOND",
		"NOW(3) - INTERVAL 24 HOUR + INTERVAL 1 SECOND")

	conn, err := d.Conn(context.Background())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := r.purgeNodeAttempt(context.Background(), conn); err != nil {
		t.Fatalf("purgeNodeAttempt: %v", err)
	}
	if countAttempt(t, d, oldEdge) != 0 {
		t.Fatal("界外 1s 终态留档行应被清理")
	}
	if countAttempt(t, d, keepEdge) != 1 {
		t.Fatal("界内 1s 终态留档行被误删")
	}
}

// TestPurgeNodeAttemptBatchConvergence:超单批常量(500)的存量在轮内批间
// 连搬收敛清空(镜像 login_audit/login_throttle 批收敛;uk_attempt 同
// node_execution 下 attempt_no 1..N 递增)。
func TestPurgeNodeAttemptBatchConvergence(t *testing.T) {
	r, d := newReaper(t, testCfg())
	execID, _ := seedExecRow(t, d, "FAILED", 0)
	nodeID := nextID(t)
	if _, err := d.Exec(`INSERT INTO node_execution
	  (id, execution_id, node_id, type, status, attempts, max_attempts, worker_id, version, deadline_at)
	  VALUES (?, ?, 'n1', 'llm', 'FAILED', 1, 1, 'seed-dispatch', 1, NOW(3) + INTERVAL 30 MINUTE)`,
		nodeID, execID); err != nil {
		t.Fatalf("seed node_execution: %v", err)
	}
	const rows = nodeAttemptBatch + 10
	var b strings.Builder
	args := make([]any, 0, rows*2)
	b.WriteString(`INSERT INTO node_attempt
	  (id, node_execution_id, execution_id, attempt_no, worker_id, status,
	   idempotency_key, asynq_task_id, started_at, heartbeat_at) VALUES `)
	for i := 0; i < rows; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("(?,?,?,?,'w-batch','FAILED',?,?,NOW(3) - INTERVAL 25 HOUR, NOW(3) - INTERVAL 25 HOUR)")
		args = append(args, nextID(t), nodeID, execID, i+1,
			fmt.Sprintf("att:%d:%d", nodeID, i+1), fmt.Sprintf("att:%d:%d", nodeID, i+1))
	}
	if _, err := d.Exec(b.String(), args...); err != nil {
		t.Fatalf("seed batch: %v", err)
	}
	conn, err := d.Conn(context.Background())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := r.purgeNodeAttempt(context.Background(), conn); err != nil {
		t.Fatalf("purgeNodeAttempt: %v", err)
	}
	var left int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM node_attempt WHERE node_execution_id=?`, nodeID).Scan(&left); err != nil {
		t.Fatalf("count left: %v", err)
	}
	if left != 0 {
		t.Fatalf("批量未收敛:%d 行残留(want 0,%d 行两批内清空)", left, rows)
	}
}

// TestJanitorOnceIsolatesNodeAttemptPurgeFailure:errors.Join 第四项的行为
// 锚——login_audit 与 node_attempt 双面必败(双表临时改名,1146 非
// Transient)时:janitorOnce 返回**同时含两面**的合并错误,且预埋的超期
// outbox 行仍被清掉。双面断言杀两类变异:删 Join 第四项(错误缺
// node_attempt)+ 末位改 fail-fast 独立 return(错误缺 login_audit——
// 单面失败观测面与 Join 同形,只有双面才可分)。
func TestJanitorOnceIsolatesNodeAttemptPurgeFailure(t *testing.T) {
	r, d := newReaper(t, testCfg())
	id := nextID(t)
	if _, err := d.Exec(`
INSERT INTO outbox (id, execution_id, event_type, task_id, queue, payload, status, available_at, dispatched_at)
VALUES (?, 0, 'node.task_enqueue', ?, 'reap-test', '{}', 'DISPATCHED',
        NOW(3) - INTERVAL 8 DAY, NOW(3) - INTERVAL 8 DAY)`,
		id, fmt.Sprintf("att:%d:1", id)); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	if _, err := d.Exec(`RENAME TABLE node_attempt TO node_attempt_x`); err != nil {
		t.Fatalf("rename node_attempt: %v", err)
	}
	if _, err := d.Exec(`RENAME TABLE login_audit TO login_audit_x`); err != nil {
		t.Fatalf("rename login_audit: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`RENAME TABLE node_attempt_x TO node_attempt`)
		_, _ = d.Exec(`RENAME TABLE login_audit_x TO login_audit`)
	})
	err := r.janitorOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "purge node_attempt") {
		t.Fatalf("janitorOnce err = %v, want containing 'purge node_attempt'", err)
	}
	if !strings.Contains(err.Error(), "purge login_audit") {
		t.Fatalf("janitorOnce err = %v, want containing 'purge login_audit'(末位 fail-fast 变异:两面错误必须合并)", err)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM outbox WHERE id=?`, id).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if n != 0 {
		t.Fatal("outbox 清理被 node_attempt 失败阻断(errors.Join 隔离回归)")
	}
}
