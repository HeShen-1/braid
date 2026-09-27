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
