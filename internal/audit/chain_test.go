package audit

// T9.3 审计哈希链写路径集成测试(t13 真 MySQL,不可达自动 skip):
// 逐行链写入即算(D-2026-09-29-t93 #1)——Append/Enqueue 在 chain_heads
// 行锁下串链,暂存行带 hash 运输;同步行经 head 拿到在途暂存行 hash
// (对批内链的结构性胜因);DB 读回值(规范化形态)与写入侧 hash 等价
// (校验半边的地基);断链闩 fail-closed。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
)

// chainHashPair:audit_log / audit_pending 的链列断言快照。
type chainHashPair struct {
	id       uint64
	prevHash string
	rowHash  sql.NullString
}

func readAuditHashes(t *testing.T, d *sql.DB, execID uint64) []chainHashPair {
	t.Helper()
	rows, err := d.Query(
		`SELECT id, prev_hash, row_hash FROM audit_log WHERE execution_id=? ORDER BY id`, execID)
	if err != nil {
		t.Fatalf("read audit hashes: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []chainHashPair
	for rows.Next() {
		var r chainHashPair
		if err := rows.Scan(&r.id, &r.prevHash, &r.rowHash); err != nil {
			t.Fatalf("scan audit hash: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("audit hash rows: %v", err)
	}
	return out
}

func readStagedHashes(t *testing.T, d *sql.DB, execID uint64) []chainHashPair {
	t.Helper()
	rows, err := d.Query(
		`SELECT id, prev_hash, row_hash FROM audit_pending WHERE execution_id=? ORDER BY id`, execID)
	if err != nil {
		t.Fatalf("read staged hashes: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []chainHashPair
	for rows.Next() {
		var r chainHashPair
		if err := rows.Scan(&r.id, &r.prevHash, &r.rowHash); err != nil {
			t.Fatalf("scan staged hash: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("staged hash rows: %v", err)
	}
	return out
}

type headSnapshot struct {
	lastID         uint64
	lastRowHash    string
	verifiedLastID uint64
	verifiedAt     sql.NullTime
	brokenAt       sql.NullTime
}

func readHead(t *testing.T, d *sql.DB, execID uint64) headSnapshot {
	t.Helper()
	var h headSnapshot
	err := d.QueryRow(
		`SELECT last_id, last_row_hash, verified_last_id, verified_at, broken_at FROM chain_heads WHERE execution_id=?`,
		execID).Scan(&h.lastID, &h.lastRowHash, &h.verifiedLastID, &h.verifiedAt, &h.brokenAt)
	if err != nil {
		t.Fatalf("read head: %v", err)
	}
	return h
}

func chainEvent(t *testing.T, execID uint64, nodeID string) Event {
	t.Helper()
	return Event{
		ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: execID,
		NodeExecID: nextID(t), NodeID: nodeID, AttemptID: nextID(t),
		ActorType: ActorWorker, ActorID: "w1",
		Payload: json.RawMessage(`{"from":"PENDING","to":"RUNNING"}`),
	}
}

func assertChained(t *testing.T, pairs []chainHashPair, where string) {
	t.Helper()
	if len(pairs) == 0 {
		t.Fatalf("%s: no rows", where)
	}
	if pairs[0].prevHash != "" {
		t.Fatalf("%s: 链首 prev_hash = %q, want ''", where, pairs[0].prevHash)
	}
	for i := range pairs {
		if !pairs[i].rowHash.Valid || !isHex64(pairs[i].rowHash.String) {
			t.Fatalf("%s[%d]: row_hash = %v, want 64-hex", where, i, pairs[i].rowHash)
		}
		if i > 0 && pairs[i].prevHash != pairs[i-1].rowHash.String {
			t.Fatalf("%s[%d]: prev_hash %s ≠ 前行 row_hash %s", where, i,
				pairs[i].prevHash, pairs[i-1].rowHash.String)
		}
	}
}

// TestAppendChainsRows:同步直写逐行串链——链首 prev=”、行行咬合、head
// 链尾与最后行一致、水位未动。
func TestAppendChainsRows(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)
	ctx := context.Background()

	for _, node := range []string{"n1", "n2", "n3"} {
		if err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Append(ctx, q, chainEvent(t, execID, node)) }); err != nil {
			t.Fatalf("Append(%s): %v", node, err)
		}
	}
	pairs := readAuditHashes(t, d, execID)
	if len(pairs) != 3 {
		t.Fatalf("rows = %d, want 3", len(pairs))
	}
	assertChained(t, pairs, "audit_log")

	h := readHead(t, d, execID)
	if h.lastID != pairs[2].id || h.lastRowHash != pairs[2].rowHash.String {
		t.Fatalf("head = (%d,%s), want 链尾 (%d,%s)", h.lastID, h.lastRowHash,
			pairs[2].id, pairs[2].rowHash.String)
	}
	if h.verifiedLastID != 0 || h.verifiedAt.Valid {
		t.Fatalf("verified = (%d, %v), want 未校验(校验作业未跑)", h.verifiedLastID, h.verifiedAt)
	}
}

// TestChainHashMatchesDBReadback:写侧 hash(Go 形输入)与读侧重算(MySQL
// 读回形态输入:payload 规范化 JSON、cost 六位定标)必须逐行相等——
// 校验半边(verify.go)的地基锚。
func TestChainHashMatchesDBReadback(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)
	ctx := context.Background()

	// 普通行 + usage 行(cost "0.0032" 写入 → DB "0.003200" 读回,六位定标锚)
	evt1 := chainEvent(t, execID, "n1")
	if err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Append(ctx, q, evt1) }); err != nil {
		t.Fatalf("Append: %v", err)
	}
	payload, err := json.Marshal(LLMCallCompletedPayload{
		NodeID: "n2", Model: "glm-5.3",
		PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120,
		CostAmount: "0.0032", CostCurrency: "CNY",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	evt2 := Event{
		ID: nextID(t), EventType: EventLLMCallCompleted, ExecutionID: execID,
		NodeExecID: nextID(t), NodeID: "n2", AttemptID: nextID(t),
		ActorType: ActorLLM, ActorID: "glm-5.3",
		Payload: payload,
		Usage: &engine.AttemptUsage{
			Model: "glm-5.3", PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120,
			CostAmount: "0.0032", CostCurrency: "CNY", Billed: true,
		},
	}
	if err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Append(ctx, q, evt2) }); err != nil {
		t.Fatalf("Append(usage): %v", err)
	}

	links := readChainLinks(ctx, t, d, execID)
	if len(links) != 2 {
		t.Fatalf("links = %d, want 2", len(links))
	}
	prev := ""
	for i, l := range links {
		got, err := l.row.rowHash(prev)
		if err != nil {
			t.Fatalf("recompute[%d]: %v", i, err)
		}
		pairs := readAuditHashes(t, d, execID)
		if got != pairs[i].rowHash.String {
			t.Fatalf("读回重算[%d] = %s, want 写侧 %s\n读回 row = %+v", i, got,
				pairs[i].rowHash.String, l.row)
		}
		prev = got
	}
}

// TestSyncAppendChainsFromPendingTail:交错核心——同事务暂存两行(链在
// 暂存面延伸),随后同步 Append 经 head 拿到**在途暂存行**的 hash 接链;
// 逐行链对批内链的结构性胜因的直接锚。
func TestSyncAppendChainsFromPendingTail(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)
	ctx := context.Background()

	e1 := chainEvent(t, execID, "n1")
	e2 := chainEvent(t, execID, "n2")
	if err := engine.WithTx(ctx, d, func(q engine.DBTX) error {
		if err := Enqueue(ctx, q, e1); err != nil {
			return err
		}
		return Enqueue(ctx, q, e2)
	}); err != nil {
		t.Fatalf("Enqueue×2: %v", err)
	}
	staged := readStagedHashes(t, d, execID)
	if len(staged) != 2 {
		t.Fatalf("staged = %d, want 2", len(staged))
	}
	assertChained(t, staged, "audit_pending")
	h := readHead(t, d, execID)
	if h.lastID != staged[1].id || h.lastRowHash != staged[1].rowHash.String {
		t.Fatalf("head = (%d,%s), want 暂存链尾 (%d,%s)", h.lastID, h.lastRowHash,
			staged[1].id, staged[1].rowHash.String)
	}

	// 同步行:prev_hash 必须等于在途暂存行的 row_hash(head 携带)。
	evt := chainEvent(t, execID, "n3")
	if err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Append(ctx, q, evt) }); err != nil {
		t.Fatalf("Append after staged: %v", err)
	}
	pairs := readAuditHashes(t, d, execID)
	if len(pairs) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(pairs))
	}
	if pairs[0].prevHash != staged[1].rowHash.String {
		t.Fatalf("同步行 prev_hash = %s, want 在途暂存链尾 %s(链叉!)",
			pairs[0].prevHash, staged[1].rowHash.String)
	}
}

// TestBrokenLatchRefusesNewAppends:断链闩 fail-closed——broken_at 落定后
// Append/Enqueue 双路径响亮拒绝(ErrChainBroken),零新行;闩不自行复位。
func TestBrokenLatchRefusesNewAppends(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)
	ctx := context.Background()

	if err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Append(ctx, q, chainEvent(t, execID, "n1")) }); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := d.Exec(
		`UPDATE chain_heads SET broken_at=NOW(3), broken_reason='test tamper' WHERE execution_id=?`,
		execID); err != nil {
		t.Fatalf("latch: %v", err)
	}

	err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Append(ctx, q, chainEvent(t, execID, "n2")) })
	if !errors.Is(err, ErrChainBroken) {
		t.Fatalf("Append(broken) = %v, want ErrChainBroken", err)
	}
	err = engine.WithTx(ctx, d, func(q engine.DBTX) error { return Enqueue(ctx, q, chainEvent(t, execID, "n3")) })
	if !errors.Is(err, ErrChainBroken) {
		t.Fatalf("Enqueue(broken) = %v, want ErrChainBroken", err)
	}
	if pairs := readAuditHashes(t, d, execID); len(pairs) != 1 {
		t.Fatalf("断链后新行 = %d, want 1(拒绝生效)", len(pairs))
	}
	if rows := readStagedHashes(t, d, execID); len(rows) != 0 {
		t.Fatalf("断链后暂存 = %d, want 0", len(rows))
	}
	// 其余执行不受累(per-execution 闩)
	other := seedExec(t, d)
	if err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Append(ctx, q, chainEvent(t, other, "m1")) }); err != nil {
		t.Fatalf("Append(other exec) = %v, want 正常", err)
	}
}

// TestAppendInvertedIdHeadFollowsChain:rev2 对抗审查 HIGH 回归锚——**写路径**
// 的诚实倒置(后链者 id 更小,经真 appendChain/advanceChainHead,非手工
// 插行):head 必须跟随真链尾(而非 id 最大行),第三次 append 从真链尾
// 接链,零叉链。id 单调 guard(`last_id < ?`)在该形态下静默滞留头 ⇒
// 下一次 append 自造叉链——本锚钉死其不回归。
func TestAppendInvertedIdHeadFollowsChain(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)
	ctx := context.Background()

	// 行 W(id 大,先链):手工钉 id 形态模拟跨进程同毫秒倒置(生产成因
	// = node 位次序;测试单生成器下用手工分配等价复现)。
	evtW := chainEvent(t, execID, "nW")
	idW := nextID(t)
	evtW.ID = idW
	if err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Append(ctx, q, evtW) }); err != nil {
		t.Fatalf("Append(W): %v", err)
	}
	rowW := chainRowFromEvent(evtW, evtW.Payload, false)
	hW, err := rowW.rowHash("")
	if err != nil {
		t.Fatalf("hash W: %v", err)
	}

	// 行 A(id 更小,后链):真写路径,head 必须推进到 A。
	evtA := chainEvent(t, execID, "nA")
	evtA.ID = idW - 1 // 后链者 id 更小
	payloadA := evtA.Payload
	if err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Append(ctx, q, evtA) }); err != nil {
		t.Fatalf("Append(A): %v", err)
	}
	rowA := chainRowFromEvent(evtA, payloadA, false)
	hA, err := rowA.rowHash(hW)
	if err != nil {
		t.Fatalf("hash A: %v", err)
	}
	h := readHead(t, d, execID)
	if h.lastID != evtA.ID || h.lastRowHash != hA {
		t.Fatalf("head = (%d, %.8s…), want 真链尾 A (%d, %.8s…)(头滞留 = 单调 guard 回归)",
			h.lastID, h.lastRowHash, evtA.ID, hA)
	}

	// 第三次 append:必须从 A 的 hash 接链(若头滞留在 W,则 C 与 A 同
	// prev = 系统自造叉链)。
	evtC := chainEvent(t, execID, "nC")
	if err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Append(ctx, q, evtC) }); err != nil {
		t.Fatalf("Append(C): %v", err)
	}
	pairs := readAuditHashes(t, d, execID)
	if len(pairs) != 3 {
		t.Fatalf("rows = %d, want 3", len(pairs))
	}
	// 注:此处不断言 id 序咬合(assertChained 是 id 序断言,与倒置链不兼容);
	// 链接序正确性由 head 断言 + 下方 VerifyChain 全链复走绿承载。
	out, err := VerifyChain(ctx, d, execID)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if out.Broken || !out.Verified {
		t.Fatalf("倒置链校验 = %+v, want green", out)
	}
}

// TestChainRollbackAtomicity:head 推进与行落库同事务同存亡——回滚后链上
// 无孤儿(head 不指向未落行)。
func TestChainRollbackAtomicity(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)
	ctx := context.Background()

	if err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Append(ctx, q, chainEvent(t, execID, "n1")) }); err != nil {
		t.Fatalf("Append: %v", err)
	}
	before := readHead(t, d, execID)

	boom := errors.New("boom")
	err := engine.WithTx(ctx, d, func(q engine.DBTX) error {
		if err := Enqueue(ctx, q, chainEvent(t, execID, "n2")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithTx = %v, want boom", err)
	}
	after := readHead(t, d, execID)
	if after.lastID != before.lastID || after.lastRowHash != before.lastRowHash {
		t.Fatalf("回滚后 head 前移:(%d,%s) → (%d,%s)", before.lastID,
			before.lastRowHash, after.lastID, after.lastRowHash)
	}
	if rows := readStagedHashes(t, d, execID); len(rows) != 0 {
		t.Fatalf("回滚后暂存 = %d, want 0", len(rows))
	}
}

// readChainLinks 测试辅助:拉全量链行(读回形态);生产读半边同函数
// (chain.go readChainWindow,verify 消费;此处只取落地臂断言等价性)。
func readChainLinks(ctx context.Context, t *testing.T, d *sql.DB, execID uint64) []chainLink {
	t.Helper()
	var execDate string
	if err := d.QueryRowContext(ctx,
		`SELECT DATE_FORMAT(created_at, '%Y-%m-%d') FROM workflow_execution WHERE id=?`,
		execID).Scan(&execDate); err != nil {
		t.Fatalf("exec_date: %v", err)
	}
	links, err := readWholeChain(ctx, d, execDate, execID)
	if err != nil {
		t.Fatalf("readChainWindow: %v", err)
	}
	return links
}
