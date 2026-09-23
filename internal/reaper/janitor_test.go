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

// TestJanitorFullHorizonNoDDL:水位已覆盖 today+7 → 一轮零 DDL(分区集不变)。
func TestJanitorFullHorizonNoDDL(t *testing.T) {
	r, d := newReaper(t, testCfg())
	before := readPartitions(t, d)
	if err := r.janitorOnce(context.Background()); err != nil {
		t.Fatalf("janitorOnce: %v", err)
	}
	after := readPartitions(t, d)
	if len(before) != len(after) {
		t.Fatalf("partitions %d → %d(满水位不应有 DDL)", len(before), len(after))
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

