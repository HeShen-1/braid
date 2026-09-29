package audit

// T9.3 链校验引擎集成测试(t13 真 MySQL,不可达自动 skip):增量走链、
// 水位推进、head 在场性跨表查、exec_date re-derivation、断链闩落定。
// 篡改检测验收(ROADMAP「改一行 → 校验红」)的机制锚:改 payload/删行/
// 丢暂存尾/挪分区四族全红,诚实链绿且水位幂等。

import (
	"context"
	"testing"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
)

func TestVerifyWalksAndAdvancesWatermark(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)
	ctx := context.Background()

	// 交错全管道:暂存×2 → flush → 同步×1(链跨两表三行)
	for _, n := range []string{"n1", "n2"} {
		evt := chainEvent(t, execID, n)
		if err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Enqueue(ctx, q, evt) }); err != nil {
			t.Fatalf("Enqueue(%s): %v", n, err)
		}
	}
	if _, err := FlushAll(ctx, d, 10, 100); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}
	if err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Append(ctx, q, chainEvent(t, execID, "n3")) }); err != nil {
		t.Fatalf("Append: %v", err)
	}
	pairs := readAuditHashes(t, d, execID)
	if len(pairs) != 3 {
		t.Fatalf("rows = %d, want 3", len(pairs))
	}

	out, err := VerifyChain(ctx, d, execID)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if out.Broken || out.Checked != 3 || !out.Verified {
		t.Fatalf("out = %+v, want checked=3 advanced green", out)
	}
	h := readHead(t, d, execID)
	if h.verifiedLastID != h.lastID || !h.verifiedAt.Valid || h.brokenAt.Valid {
		t.Fatalf("head = %+v, want 轮换追平(verified_last_id=last_id + verified_at 落定)且无闩", h)
	}

	// 幂等(全链复走语义):无新行 → 复走全部行仍绿(每轮重验历史行 =
	// 历史篡改检测面,rev2 设计);不重复落闩、不重复计数
	out, err = VerifyChain(ctx, d, execID)
	if err != nil {
		t.Fatalf("VerifyChain(2nd): %v", err)
	}
	if out.Broken || out.Checked != 3 || !out.Verified || out.NewlyBroken {
		t.Fatalf("second pass = %+v, want 全链复走 checked=3 green", out)
	}
}

// TestVerifyAcceptsInvertedIdOrder:对抗审查 HIGH 回归锚——链序 = 链接序,
// **不是 id 序**(snowflake 同毫秒跨进程倒置,先链者 id 可更小)。手工
// 构造诚实倒置链(id 大者为链首,id 小者接链),校验必须绿;id 序走链
// 的 rev1 实现在此形态误闩。
func TestVerifyAcceptsInvertedIdOrder(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)
	ctx := context.Background()

	var execDate string
	if err := d.QueryRow(
		`SELECT DATE_FORMAT(created_at, '%Y-%m-%d') FROM workflow_execution WHERE id=?`,
		execID).Scan(&execDate); err != nil {
		t.Fatalf("exec_date: %v", err)
	}
	// 行 A(id 大)先链:prev='';行 B(id 小)后链:prev=h(A)——
	// 诚实的「先链后写者 id 更小」形态。hash 用生产同源函数算。
	mkRow := func(id uint64, node string) chainRow {
		return chainRow{ID: id, EventType: EventNodeStateChanged, ExecutionID: execID,
			NodeID: node, ActorType: ActorWorker, ActorID: "w1",
			Payload: []byte(`{"from":"PENDING","to":"RUNNING"}`)}
	}
	idA := nextID(t) // 先分配 = id 大
	rowA := mkRow(idA, "nA")
	hA, err := rowA.rowHash("")
	if err != nil {
		t.Fatalf("hash A: %v", err)
	}
	idB := idA - 1 // 后链者 id 更小(手工构造倒置;同毫秒 node 位倒置的生产形态)
	rowB := mkRow(idB, "nB")
	rowB.ID = idB
	hB, err := rowB.rowHash(hA)
	if err != nil {
		t.Fatalf("hash B: %v", err)
	}
	ins := func(id uint64, node, prev, hash string) {
		t.Helper()
		if _, err := d.Exec(`
INSERT INTO audit_log (id, exec_date, event_at, event_type, execution_id, node_id,
                       actor_type, actor_id, payload, prev_hash, row_hash)
VALUES (?, ?, NOW(3), ?, ?, ?, 'worker', 'w1', '{"from":"PENDING","to":"RUNNING"}', ?, ?)`,
			id, execDate, EventNodeStateChanged, execID, node, prev, hash); err != nil {
			t.Fatalf("insert %d: %v", id, err)
		}
	}
	ins(idA, "nA", "", hA)
	ins(idB, "nB", hA, hB)
	if _, err := d.Exec(
		`INSERT INTO chain_heads (execution_id, last_id, last_row_hash) VALUES (?, ?, ?)`,
		execID, idB, hB); err != nil {
		t.Fatalf("head: %v", err)
	}

	out, err := VerifyChain(ctx, d, execID)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if out.Broken || !out.Verified || out.Checked != 2 {
		t.Fatalf("倒置链 = %+v, want green(链序=链接序,id 倒置是常态)", out)
	}
}

// TestVerifyDetectsHistoricalTamper:对抗审计 MEDIUM 修的回归锚——已验行
// 的事后篡改必须仍可检(全链复走语义:没有「验过即免检」的水位盲区)。
func TestVerifyDetectsHistoricalTamper(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)
	ctx := context.Background()

	for _, n := range []string{"n1", "n2"} {
		if err := engine.WithTx(ctx, d, func(q engine.DBTX) error {
			return Append(ctx, q, chainEvent(t, execID, n))
		}); err != nil {
			t.Fatalf("Append(%s): %v", n, err)
		}
	}
	out, err := VerifyChain(ctx, d, execID)
	if err != nil || out.Broken || !out.Verified {
		t.Fatalf("first pass = %+v err=%v, want green", out, err)
	}

	pairs := readAuditHashes(t, d, execID)
	if _, err := d.Exec(`UPDATE audit_log SET actor_id='tamperer' WHERE id=?`, pairs[0].id); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	out, err = VerifyChain(ctx, d, execID)
	if err != nil {
		t.Fatalf("VerifyChain(after tamper): %v", err)
	}
	if !out.Broken {
		t.Fatalf("已验行篡改未被检出: %+v(全链复走无水位盲区)", out)
	}
}

// TestVerifyForkLatchLandsWithinReasonColumn:rev3 对抗审计 MEDIUM 回归锚
// ——12 行伪叉链(直写注入,本特性防的篡改面)共享同一 prev 时,fork
// 归因必须截断进 broken_reason VARCHAR(255):闩落得了地(1406 = 闩失败
// = 告警面被压制)、作业不因该头报错。
func TestVerifyForkLatchLandsWithinReasonColumn(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)
	ctx := context.Background()

	var execDate string
	if err := d.QueryRow(
		`SELECT DATE_FORMAT(created_at, '%Y-%m-%d') FROM workflow_execution WHERE id=?`,
		execID).Scan(&execDate); err != nil {
		t.Fatalf("exec_date: %v", err)
	}
	// 链首 + 12 行伪叉(全部 prev=链首 hash;id 用连续分配)
	first := chainEvent(t, execID, "genesis")
	hFirst, err := chainRowFromEvent(first, first.Payload, false).rowHash("")
	if err != nil {
		t.Fatalf("genesis hash: %v", err)
	}
	if err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Append(ctx, q, first) }); err != nil {
		t.Fatalf("Append genesis: %v", err)
	}
	for i := 0; i < 12; i++ {
		evt := chainEvent(t, execID, "fork")
		row := chainRowFromEvent(evt, evt.Payload, false)
		forkHash, err := row.rowHash(hFirst)
		if err != nil {
			t.Fatalf("fork hash: %v", err)
		}
		if _, err := d.Exec(`
INSERT INTO audit_log (id, exec_date, event_at, event_type, execution_id, node_id,
                       actor_type, actor_id, payload, prev_hash, row_hash)
VALUES (?, ?, NOW(3), ?, ?, ?, 'worker', 'w1', '{}', ?, ?)`,
			evt.ID, execDate, EventNodeStateChanged, execID, "fork", hFirst, forkHash); err != nil {
			t.Fatalf("inject fork %d: %v", i, err)
		}
	}

	out, err := VerifyChain(ctx, d, execID)
	if err != nil {
		t.Fatalf("VerifyChain: %v(闩落不了地 = 告警面被压制)", err)
	}
	if !out.Broken || !out.NewlyBroken {
		t.Fatalf("out = %+v, want fork 检出且闩落定", out)
	}
	var reasonLen int
	if err := d.QueryRow(
		`SELECT CHAR_LENGTH(broken_reason) FROM chain_heads WHERE execution_id=?`,
		execID).Scan(&reasonLen); err != nil {
		t.Fatalf("reason len: %v", err)
	}
	if reasonLen > 255 {
		t.Fatalf("broken_reason 长 %d > 255(截断失效)", reasonLen)
	}
}

func TestVerifyDetectsTamperedPayload(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)
	ctx := context.Background()

	for _, n := range []string{"n1", "n2", "n3"} {
		if err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Append(ctx, q, chainEvent(t, execID, n)) }); err != nil {
			t.Fatalf("Append(%s): %v", n, err)
		}
	}
	pairs := readAuditHashes(t, d, execID)
	// 改一行:payload 被篡改(超级用户直连 UPDATE;业务账号无权)
	if _, err := d.Exec(`UPDATE audit_log SET payload='{"tampered":1}' WHERE id=?`, pairs[1].id); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	out, err := VerifyChain(ctx, d, execID)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if !out.Broken {
		t.Fatalf("out = %+v, want broken(改一行必红)", out)
	}
	// 闩已落定 → 写路径拒绝(响亮执法)
	if err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Append(ctx, q, chainEvent(t, execID, "n4")) }); err == nil {
		t.Fatal("Append after tamper: want ErrChainBroken")
	}
	// 已断链的重复校验:保持断链,不误清
	out, err = VerifyChain(ctx, d, execID)
	if err != nil {
		t.Fatalf("VerifyChain(2nd): %v", err)
	}
	if !out.Broken {
		t.Fatal("second pass cleared the latch")
	}
}

func TestVerifyDetectsDeletedRow(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)
	ctx := context.Background()

	for _, n := range []string{"n1", "n2", "n3"} {
		if err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Append(ctx, q, chainEvent(t, execID, n)) }); err != nil {
			t.Fatalf("Append(%s): %v", n, err)
		}
	}
	pairs := readAuditHashes(t, d, execID)
	if _, err := d.Exec(`DELETE FROM audit_log WHERE id=?`, pairs[1].id); err != nil {
		t.Fatalf("delete: %v", err)
	}

	out, err := VerifyChain(ctx, d, execID)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if !out.Broken {
		t.Fatalf("out = %+v, want broken(删行断链接力)", out)
	}
}

func TestVerifyDetectsVanishedPendingTail(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)
	ctx := context.Background()

	if err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Append(ctx, q, chainEvent(t, execID, "n1")) }); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Enqueue(ctx, q, chainEvent(t, execID, "n2")) }); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	h := readHead(t, d, execID)
	// 模拟 WAL 丢失:暂存尾行消失(head 指向它)——0004「异步面回退放弃
	// 该窗事件」的旧语义在链上升级为可检测。
	if _, err := d.Exec(`DELETE FROM audit_pending WHERE id=?`, h.lastID); err != nil {
		t.Fatalf("drop pending tail: %v", err)
	}

	out, err := VerifyChain(ctx, d, execID)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if !out.Broken {
		t.Fatalf("out = %+v, want broken(链尾双表缺场)", out)
	}
}

func TestVerifyDetectsExecDateMove(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)
	ctx := context.Background()

	for _, n := range []string{"n1", "n2"} {
		if err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Append(ctx, q, chainEvent(t, execID, n)) }); err != nil {
			t.Fatalf("Append(%s): %v", n, err)
		}
	}
	pairs := readAuditHashes(t, d, execID)
	// 挪分区:exec_date 改写(行迁出真分区——hash 不含 exec_date,由
	// re-derivation 检查面兜住)
	if _, err := d.Exec(`UPDATE audit_log SET exec_date='2026-01-01' WHERE id=?`, pairs[1].id); err != nil {
		t.Fatalf("move partition: %v", err)
	}

	out, err := VerifyChain(ctx, d, execID)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if !out.Broken {
		t.Fatalf("out = %+v, want broken(exec_date re-derivation)", out)
	}
}

func TestVerifySkipsLegacyUnhashedRows(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)
	ctx := context.Background()

	// 存量未启链行(直插:prev='', row_hash NULL)+ 启链后新链
	var execDate string
	if err := d.QueryRow(
		`SELECT DATE_FORMAT(created_at, '%Y-%m-%d') FROM workflow_execution WHERE id=?`,
		execID).Scan(&execDate); err != nil {
		t.Fatalf("exec_date: %v", err)
	}
	if _, err := d.Exec(`
INSERT INTO audit_log (id, exec_date, event_at, event_type, execution_id, actor_type, actor_id, payload)
VALUES (?, ?, NOW(3), ?, ?, 'system', 'legacy', '{}')`,
		nextID(t), execDate, EventNodeStateChanged, execID); err != nil {
		t.Fatalf("legacy row: %v", err)
	}
	for _, n := range []string{"n1", "n2"} {
		if err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Append(ctx, q, chainEvent(t, execID, n)) }); err != nil {
			t.Fatalf("Append(%s): %v", n, err)
		}
	}

	out, err := VerifyChain(ctx, d, execID)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if out.Broken || out.Checked != 2 {
		t.Fatalf("out = %+v, want checked=2 green(存量行跳过)", out)
	}
}
