package audit

// Flush 集成测试(t13 真 MySQL):批量搬运、exec_date 物化、事件时刻保真、
// 崩溃重放幂等收敛(批量契约 = 预过滤 + 裸 INSERT,ON DUP 已否决——
// UPDATE 权限与 T7.3 append-only 预检冲突,见 flush.go 头注释)、ghost
// execution 隔离、单批上限语义。「批量写无丢失」总对账在
// TestFlushReconcilesByAttempt(片 5)。

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
)

// flushAuditRows 直读 audit_log(按 execution 集合),返回断言快照。
func flushAuditRows(t *testing.T, d *sql.DB, execIDs ...uint64) []auditRow {
	t.Helper()
	if len(execIDs) == 0 {
		t.Fatal("flushAuditRows: no exec ids")
	}
	ph := ""
	args := make([]any, 0, len(execIDs))
	for i, id := range execIDs {
		if i > 0 {
			ph += ","
		}
		ph += "?"
		args = append(args, id)
	}
	rows, err := d.Query(`
SELECT id, exec_date, event_type, execution_id, node_execution_id, node_id,
       attempt_id, actor_type, actor_id, trace_id, linked_trace_id, payload,
       prev_hash, pii_masked
  FROM audit_log WHERE execution_id IN (`+ph+`) ORDER BY id`, args...)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.id, &r.execDate, &r.eventType, &r.executionID, &r.nodeExecID,
			&r.nodeID, &r.attemptID, &r.actorType, &r.actorID, &r.traceID, &r.linkedTrace,
			&r.payload, &r.prevHash, &r.piiMasked); err != nil {
			t.Fatalf("scan audit: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("audit rows: %v", err)
	}
	return out
}

// T6.2:trace/linked 两列随暂存行搬运落 audit_log(Enqueue→flush 全管道)。
func TestFlushCarriesTraceColumns(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)
	stageEvent(t, d, Event{
		ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: execID,
		NodeID: "n1", ActorType: ActorWorker, ActorID: "w1",
		TraceID: "0123456789abcdef0123456789abcdef",
	})
	stageEvent(t, d, Event{
		ID: nextID(t), EventType: EventApprovalDecisionDenied, ExecutionID: execID,
		NodeID: "n2", ActorType: ActorWorker, ActorID: "w1",
		LinkedTraceID: "fedcba9876543210fedcba9876543210",
	})
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM audit_pending WHERE execution_id=?`, execID) })

	if _, err := FlushAll(context.Background(), d, 10, 100); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}
	rows := flushAuditRows(t, d, execID)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].traceID.String != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("row1 trace_id = %v, want staged value", rows[0].traceID)
	}
	if rows[0].linkedTrace.Valid {
		t.Fatalf("row1 linked = %v, want NULL", rows[0].linkedTrace)
	}
	if rows[1].linkedTrace.String != "fedcba9876543210fedcba9876543210" {
		t.Fatalf("row2 linked_trace_id = %v, want staged value", rows[1].linkedTrace)
	}
	if rows[1].traceID.Valid {
		t.Fatalf("row2 trace_id = %v, want NULL", rows[1].traceID)
	}
}

// stageEvent 便捷落暂存(独立事务;Enqueue 已有同存亡测试)。
func stageEvent(t *testing.T, d *sql.DB, evt Event) {
	t.Helper()
	if err := engine.WithTx(context.Background(), d, func(q engine.DBTX) error {
		return Enqueue(context.Background(), q, evt)
	}); err != nil {
		t.Fatalf("stage event %d: %v", evt.ID, err)
	}
}

func TestFlushDrainsStagedToAuditLog(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	ctx := context.Background()
	exec1 := seedExec(t, d)
	exec2 := seedExec(t, d)
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM audit_pending WHERE execution_id IN (?,?)`, exec1, exec2)
	})

	// 混合形状:两执行 × 三类事件,含 usage 组、空 payload、掩蔽行。
	llmPayload, err := json.Marshal(LLMCallCompletedPayload{NodeID: "n1", Model: "glm-5.3", TotalTokens: 120})
	if err != nil {
		t.Fatalf("marshal llm payload: %v", err)
	}
	events := []Event{
		{ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: exec1,
			NodeExecID: 1, NodeID: "n1", AttemptID: 11, ActorType: ActorWorker, ActorID: "w1",
			Payload: json.RawMessage(`{"from":"PENDING","to":"EXECUTING"}`)},
		{ID: nextID(t), EventType: EventLLMCallCompleted, ExecutionID: exec1,
			NodeExecID: 1, NodeID: "n1", AttemptID: 11, ActorType: ActorLLM, ActorID: "glm-5.3",
			Payload: llmPayload,
			Usage: &engine.AttemptUsage{Model: "glm-5.3", TotalTokens: 120,
				CostAmount: "0.003200", CostCurrency: "CNY", Billed: true}},
		{ID: nextID(t), EventType: EventBranchEvaluated, ExecutionID: exec2,
			NodeExecID: 2, NodeID: "n2", ActorType: ActorWorker, ActorID: "w1"}, // 空 payload → {}
		{ID: nextID(t), EventType: EventToolCallCompleted, ExecutionID: exec2,
			NodeExecID: 2, NodeID: "n2", AttemptID: 22, ActorType: ActorTool, ActorID: "http",
			Payload: json.RawMessage(`{"user_data":{"path":"secret"},"tool":"http"}`)},
	}
	for i := range events {
		stageEvent(t, d, events[i])
	}
	// T9.3:暂存面 hash 快照(flush 前拍下,搬运断言的字源)。
	staged1 := readStagedHashes(t, d, exec1)
	staged2 := readStagedHashes(t, d, exec2)
	if len(staged1) != 2 || len(staged2) != 2 {
		t.Fatalf("staged = %d/%d, want 2/2", len(staged1), len(staged2))
	}

	res, err := Flush(ctx, d, 0, 100)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if res.Staged != 4 || res.Flushed != 4 || res.Skipped != 0 {
		t.Fatalf("result = %+v, want staged/flushed 4/4 skip 0", res)
	}
	if res.Inserted != 4 {
		t.Fatalf("inserted = %d, want 4(全新行)", res.Inserted)
	}
	if n := len(readStaged(t, d, exec1)) + len(readStaged(t, d, exec2)); n != 0 {
		t.Fatalf("staged after flush = %d, want 0(排空)", n)
	}

	rows := flushAuditRows(t, d, exec1, exec2)
	if len(rows) != 4 {
		t.Fatalf("audit rows = %d, want 4", len(rows))
	}
	byID := map[uint64]auditRow{}
	for _, r := range rows {
		byID[r.id] = r
	}
	// exec_date 物化 = DATE(execution.created_at)(seed 钉 2026-09-22,判别力;
	// 两侧同走 driver 的 DATE→time 转换,镜像 writer_test 的比对形态)。
	for _, evt := range events {
		r, ok := byID[evt.ID]
		if !ok {
			t.Fatalf("event %d (%s) lost in flush", evt.ID, evt.EventType)
		}
		var wantDate string
		if err := d.QueryRow(`SELECT DATE(created_at) FROM workflow_execution WHERE id=?`,
			evt.ExecutionID).Scan(&wantDate); err != nil {
			t.Fatalf("read created_at: %v", err)
		}
		if r.execDate != wantDate || !strings.HasPrefix(wantDate, "2026-09-22") {
			t.Fatalf("event %d exec_date = %s, want %s(=2026-09-22)", evt.ID, r.execDate, wantDate)
		}
		if r.eventType != evt.EventType || r.executionID != evt.ExecutionID {
			t.Fatalf("event %d identity = %+v", evt.ID, r)
		}
	}
	// T9.3:链列原样搬运(flush 纯搬运不重算)——prev/row 双列与暂存快照
	// 逐位相等。
	assertTransported := func(execID uint64, staged []chainHashPair) {
		t.Helper()
		got := readAuditHashes(t, d, execID)
		if len(got) != len(staged) {
			t.Fatalf("exec %d rows = %d, want %d", execID, len(got), len(staged))
		}
		for i := range staged {
			if got[i].id != staged[i].id || got[i].prevHash != staged[i].prevHash ||
				!got[i].rowHash.Valid || got[i].rowHash.String != staged[i].rowHash.String {
				t.Fatalf("exec %d row[%d] hash 搬运失真: got (%d,%s,%v) staged (%d,%s,%v)",
					execID, i, got[i].id, got[i].prevHash, got[i].rowHash,
					staged[i].id, staged[i].prevHash, staged[i].rowHash)
			}
		}
	}
	assertTransported(exec1, staged1)
	assertTransported(exec2, staged2)
	// 掩蔽行:标志与掩蔽值随列搬运。
	masked := byID[events[3].ID]
	if !masked.piiMasked {
		t.Fatal("pii_masked lost in flush")
	}
	if !strings.Contains(string(masked.payload), "***") {
		t.Fatalf("masked token missing after flush: %s", masked.payload)
	}
	if strings.Contains(string(masked.payload), "secret") {
		t.Fatalf("PII leaked through flush: %s", masked.payload)
	}
	// usage 组搬运:七列逐位断言(对抗审计 test-gaps:仅断 cost_amount 时
	// model/token/currency/estimated 整列丢弃全绿)。
	llm := byID[events[1].ID]
	var (
		um               sql.NullString
		pt, ct, tt       sql.NullInt64
		amount, currency sql.NullString
		est              bool
	)
	if err := d.QueryRow(`SELECT model, prompt_tokens, completion_tokens, total_tokens,
	       cost_amount, cost_currency, cost_estimated FROM audit_log WHERE id=? AND exec_date=?`,
		llm.id, llm.execDate).Scan(&um, &pt, &ct, &tt, &amount, &currency, &est); err != nil {
		t.Fatalf("read llm usage cols: %v", err)
	}
	if um.String != "glm-5.3" || pt.Int64 != 0 || ct.Int64 != 0 || tt.Int64 != 120 {
		t.Fatalf("llm usage model/tokens = %v/%v/%v/%v, want glm-5.3/0/0/120", um, pt, ct, tt)
	}
	if !amount.Valid || amount.String != "0.003200" || currency.String != "CNY" || est {
		t.Fatalf("llm cost = %v/%v/%v, want 0.003200/CNY/false", amount, currency, est)
	}
	// 空 payload 规范化 {} 随搬运。
	branch := byID[events[2].ID]
	if string(branch.payload) != "{}" {
		t.Fatalf("branch payload = %s, want {}", branch.payload)
	}

	// 幂等空转:排空后再 Flush 无操作、无错。
	res, err = Flush(ctx, d, 0, 100)
	if err != nil {
		t.Fatalf("Flush(idle): %v", err)
	}
	if res.Staged != 0 {
		t.Fatalf("idle result = %+v, want staged 0", res)
	}
}

// TestFlushCrashRedoConverges:批量契约臂——模拟「audit_log 已落、暂存 DELETE
// 未及」的崩溃重放(重插同 id 暂存行再 Flush)。预过滤识别在场终行 → 仅
// 核销暂存(Replayed=1、Flushed=0、零 INSERT):终行恰一、无重复、暂存排空。
// 裸 INSERT 下同事务 {INSERT,DELETE} 使该窗口结构性不可达,本臂锚定预过滤
// 防御(历史残行/未来非原子路径);INSERT IGNORE 被禁的理由(分区越界/截断
// 静默降级)由裸 INSERT 的响亮错误承载。
func TestFlushCrashRedoConverges(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	ctx := context.Background()
	execID := seedExec(t, d)
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM audit_pending WHERE execution_id=?`, execID) })

	evt := Event{
		ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: execID,
		NodeExecID: 1, NodeID: "n1", AttemptID: 5, ActorType: ActorWorker, ActorID: "w1",
		Payload: json.RawMessage(`{"from":"EXECUTING","to":"SUCCEEDED"}`),
	}
	stageEvent(t, d, evt)
	if _, err := Flush(ctx, d, 0, 100); err != nil {
		t.Fatalf("first flush: %v", err)
	}

	// 崩溃重放形:同 id 重回暂存(audit_log 行已在)。
	stageEvent(t, d, evt)
	res, err := Flush(ctx, d, 0, 100)
	if err != nil {
		t.Fatalf("redo flush: %v", err)
	}
	if res.Replayed != 1 {
		t.Fatalf("redo replayed = %d, want 1(预过滤核销)", res.Replayed)
	}
	if res.Inserted != 0 || res.Flushed != 0 {
		t.Fatalf("redo inserted/flushed = %d/%d, want 0/0(零 INSERT)", res.Inserted, res.Flushed)
	}
	rows := flushAuditRows(t, d, execID)
	if len(rows) != 1 {
		t.Fatalf("audit rows after redo = %d, want 1(无重复)", len(rows))
	}
	if n := len(readStaged(t, d, execID)); n != 0 {
		t.Fatalf("staged after redo = %d, want 0", n)
	}
}

// TestFlushMissingExecutionQuarantined:ghost execution 不阻断同批健康行
// (毒行隔离不逐批硬错——否则队头毒行饿死尾部),留驻暂存 + 响亮记日志 +
// 结果计数;audit_log 零行。
func TestFlushMissingExecutionQuarantined(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	ctx := context.Background()
	execID := seedExec(t, d)
	ghost := nextID(t)
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM audit_pending WHERE execution_id IN (?,?)`, execID, ghost)
	})

	stageEvent(t, d, Event{
		ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: ghost,
		NodeExecID: 1, NodeID: "n1", ActorType: ActorSystem, ActorID: "reaper:x",
		Payload: json.RawMessage(`{}`),
	})
	stageEvent(t, d, Event{
		ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: execID,
		NodeExecID: 2, NodeID: "n1", ActorType: ActorWorker, ActorID: "w1",
		Payload: json.RawMessage(`{}`),
	})

	res, err := Flush(ctx, d, 0, 100)
	if err != nil {
		t.Fatalf("Flush(ghost in batch): %v", err)
	}
	if res.Skipped != 1 || res.Flushed != 1 {
		t.Fatalf("result = %+v, want flushed 1 skipped 1", res)
	}
	if rows := flushAuditRows(t, d, execID); len(rows) != 1 {
		t.Fatalf("healthy rows = %d, want 1(毒行不阻断)", len(rows))
	}
	var ghostRows int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE execution_id=?`, ghost).
		Scan(&ghostRows); err != nil {
		t.Fatalf("count ghost: %v", err)
	}
	if ghostRows != 0 {
		t.Fatalf("ghost audit rows = %d, want 0", ghostRows)
	}
	if rows := readStaged(t, d, ghost); len(rows) != 1 {
		t.Fatalf("ghost staged = %d, want 1(隔离留驻)", len(rows))
	}
}

// TestFlushBatchLimit:单批上限语义——一批只搬 LIMIT N 行,余量留驻。
func TestFlushBatchLimit(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	ctx := context.Background()
	execID := seedExec(t, d)
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM audit_pending WHERE execution_id=?`, execID) })

	for i := 0; i < 3; i++ {
		stageEvent(t, d, Event{
			ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: execID,
			NodeExecID: uint64(i + 1), NodeID: "n1", ActorType: ActorWorker, ActorID: "w1",
			Payload: json.RawMessage(`{}`),
		})
	}
	res, err := Flush(ctx, d, 0, 2)
	if err != nil {
		t.Fatalf("Flush(batch 2): %v", err)
	}
	if res.Flushed != 2 {
		t.Fatalf("flushed = %d, want 2", res.Flushed)
	}
	if n := len(readStaged(t, d, execID)); n != 1 {
		t.Fatalf("staged remainder = %d, want 1", n)
	}

	if _, err := Flush(ctx, d, 0, 2); err != nil {
		t.Fatalf("Flush(drain): %v", err)
	}
	if n := len(readStaged(t, d, execID)); n != 0 {
		t.Fatalf("staged after drain = %d, want 0", n)
	}
}

// TestFlushRejectsBadBatch:batch < 1 = 调用方 bug,响亮拒绝。
func TestFlushRejectsBadBatch(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	if _, err := Flush(context.Background(), d, 0, 0); err == nil {
		t.Fatal("Flush(batch 0) = nil, want error")
	}
}

// TestFlushAllNoHeadOfLineStarvation:饿死回归(对抗审计 HIGH 实锤修复锚)
// ——ghost 行 ≥ batch 占满队头时,健康行仍被搬运(游标翻页推进)。
// 变异:FlushAll 退回 Staged<batch-only 终止或 Flush 不带游标 → 本测红。
func TestFlushAllNoHeadOfLineStarvation(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	ctx := context.Background()
	execID := seedExec(t, d)
	ghost := nextID(t)
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM audit_pending WHERE execution_id IN (?,?)`, execID, ghost)
	})

	// 队头 3 个 ghost(batch=3 恰被占满)+ 其后 1 个健康行。
	for i := 0; i < 3; i++ {
		stageEvent(t, d, Event{
			ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: ghost,
			NodeExecID: uint64(i + 1), NodeID: "g", ActorType: ActorSystem, ActorID: "reaper:x",
			Payload: json.RawMessage(`{}`),
		})
	}
	healthy := Event{
		ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: execID,
		NodeExecID: 9, NodeID: "n1", ActorType: ActorWorker, ActorID: "w1",
		Payload: json.RawMessage(`{}`),
	}
	stageEvent(t, d, healthy)

	agg, err := FlushAll(ctx, d, 3, 1000)
	if err != nil {
		t.Fatalf("FlushAll: %v", err)
	}
	if agg.Flushed != 1 {
		t.Fatalf("flushed = %d, want 1(健康行必须越过 ghost 队头)", agg.Flushed)
	}
	if agg.Batches < 1 || agg.Skipped < 3 {
		t.Fatalf("aggregate = %+v, want batches>=1 skipped>=3", agg)
	}
	rows := flushAuditRows(t, d, execID)
	if len(rows) != 1 || rows[0].id != healthy.ID {
		t.Fatalf("healthy row = %+v, want id %d", rows, healthy.ID)
	}
	if g := len(readStaged(t, d, ghost)); g != 3 {
		t.Fatalf("ghost staged = %d, want 3(隔离留驻)", g)
	}
}

// TestSelectPresentIDsDualDate:预过滤/对账查询的单点锚——两个 exec_date 组
// 必须都被覆盖(对抗审计 test-gaps:多日期 OR 臂从未被触发,漏臂变异存活)。
func TestSelectPresentIDsDualDate(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	ctx := context.Background()
	exec1 := seedExec(t, d)
	exec2 := seedExec(t, d)
	// exec2 挪到另一日(引导分区内,2026-09-23):批内两日期 → OR 双臂。
	if _, err := d.Exec(`UPDATE workflow_execution SET created_at='2026-09-23 12:00:00' WHERE id=?`, exec2); err != nil {
		t.Fatalf("shift created_at: %v", err)
	}

	e1 := Event{ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: exec1,
		NodeExecID: 1, NodeID: "n1", ActorType: ActorWorker, ActorID: "w1", Payload: json.RawMessage(`{}`)}
	e2 := Event{ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: exec2,
		NodeExecID: 1, NodeID: "n1", ActorType: ActorWorker, ActorID: "w1", Payload: json.RawMessage(`{}`)}
	for _, e := range []Event{e1, e2} {
		if err := engine.WithTx(ctx, d, func(q engine.DBTX) error { return Append(ctx, q, e) }); err != nil {
			t.Fatalf("append %d: %v", e.ID, err)
		}
	}
	d1, d2 := "2026-09-22", "2026-09-23"
	ids := map[string][]uint64{d1: {e1.ID}, d2: {e2.ID}}
	order := []string{d1, d2}

	got, err := selectPresentIDs(ctx, d, order, ids)
	if err != nil {
		t.Fatalf("selectPresentIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("present = %v, want both dates(漏日期臂)", got)
	}
	cnt, err := countPresent(ctx, d, order, ids)
	if err != nil {
		t.Fatalf("countPresent: %v", err)
	}
	if cnt != 2 {
		t.Fatalf("count = %d, want 2(漏组在和上红)", cnt)
	}
	// 反向判别力:不存在的 id 不计。
	ids[d1] = []uint64{nextID(t)}
	if cnt, err = countPresent(ctx, d, order, ids); err != nil || cnt != 1 {
		t.Fatalf("count(absent) = %d/%v, want 1", cnt, err)
	}
}

// TestFlushUnbilledUsageNulls:未计费 usage 臂(对抗审计 test-gaps:Billed=false
// 时无条件写 cost 的变异存活)——搬运后 cost 两列 NULL、estimated 在列。
func TestFlushUnbilledUsageNulls(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	ctx := context.Background()
	execID := seedExec(t, d)
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM audit_pending WHERE execution_id=?`, execID) })

	stageEvent(t, d, Event{
		ID: nextID(t), EventType: EventLLMCallCompleted, ExecutionID: execID,
		NodeExecID: 1, NodeID: "n1", AttemptID: 2, ActorType: ActorLLM, ActorID: "mystery",
		Payload: json.RawMessage(`{}`),
		Usage:   &engine.AttemptUsage{Model: "mystery", TotalTokens: 7, Estimated: true},
	})
	if _, err := Flush(ctx, d, 0, 10); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	var amount, currency sql.NullString
	var est bool
	if err := d.QueryRow(`SELECT cost_amount, cost_currency, cost_estimated
	   FROM audit_log WHERE execution_id=? AND event_type=?`,
		execID, EventLLMCallCompleted).Scan(&amount, &currency, &est); err != nil {
		t.Fatalf("read unbilled row: %v", err)
	}
	if amount.Valid || currency.Valid || !est {
		t.Fatalf("unbilled = %v/%v/%v, want NULL/NULL/true", amount, currency, est)
	}
}

// TestFlushPreservesTraceAndPiiDefaults:trace_id 搬运 + 未掩蔽行 pii_masked=0
// (对抗审计 LOW:两列零断言)。
func TestFlushPreservesTraceAndPiiDefaults(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	ctx := context.Background()
	execID := seedExec(t, d)
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM audit_pending WHERE execution_id=?`, execID) })

	stageEvent(t, d, Event{
		ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: execID,
		NodeExecID: 1, NodeID: "n1", ActorType: ActorWorker, ActorID: "w1",
		TraceID: "0123456789abcdef0123456789abcdef",
		Payload: json.RawMessage(`{}`),
	})
	if _, err := Flush(ctx, d, 0, 10); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	rows := flushAuditRows(t, d, execID)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if !rows[0].traceID.Valid || rows[0].traceID.String != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("trace_id = %+v, want preserved", rows[0].traceID)
	}
	if rows[0].piiMasked {
		t.Fatal("pii_masked = true on unmarked row, want false")
	}
}
