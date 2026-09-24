package audit

// Enqueue 集成测试(t13 真 MySQL,不可达自动 skip):同事务暂存、回滚同亡、
// 列落位(含 usage 组与 pii_masked)、校验前置。WAL 半边的验收锚——
// 「staged ⟺ 事务提交」是崩溃窗零丢失语义的地基。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
)

// stagedRow 是 audit_pending 的断言快照。
type stagedRow struct {
	id          uint64
	eventAt     string
	eventType   string
	executionID uint64
	nodeExecID  sql.NullInt64
	nodeID      sql.NullString
	attemptID   sql.NullInt64
	actorType   string
	actorID     string
	traceID     sql.NullString
	piiMasked   bool
	model       sql.NullString
	costAmount  sql.NullFloat64
	payload     []byte
}

func readStaged(t *testing.T, d *sql.DB, execID uint64) []stagedRow {
	t.Helper()
	rows, err := d.Query(`
SELECT id, DATE_FORMAT(event_at, '%Y-%m-%dT%H:%i:%s'), event_type, execution_id,
       node_execution_id, node_id, attempt_id, actor_type, actor_id, trace_id,
       pii_masked, model, cost_amount, payload
  FROM audit_pending WHERE execution_id=? ORDER BY id`, execID)
	if err != nil {
		t.Fatalf("read staged: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []stagedRow
	for rows.Next() {
		var r stagedRow
		if err := rows.Scan(&r.id, &r.eventAt, &r.eventType, &r.executionID,
			&r.nodeExecID, &r.nodeID, &r.attemptID, &r.actorType, &r.actorID,
			&r.traceID, &r.piiMasked, &r.model, &r.costAmount, &r.payload); err != nil {
			t.Fatalf("scan staged: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("staged rows: %v", err)
	}
	return out
}

// stageCleanup 排空暂存行(测试隔离;对账断言各自管理 audit_log 侧)。
func stageCleanup(t *testing.T, d *sql.DB, execID uint64) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM audit_pending WHERE execution_id=?`, execID)
	})
}

func TestEnqueueInvalidEventRejectedBeforeDB(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)
	stageCleanup(t, d, execID)

	err := Enqueue(context.Background(), d, Event{
		ID: 0, EventType: EventNodeStateChanged, ExecutionID: execID, // zero id
		NodeExecID: 1, NodeID: "n1", ActorType: ActorWorker, ActorID: "w1",
		Payload: json.RawMessage(`{}`),
	})
	if !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("Enqueue(zero id) = %v, want ErrInvalidEvent", err)
	}
	if rows := readStaged(t, d, execID); len(rows) != 0 {
		t.Fatalf("staged after invalid = %d, want 0", len(rows))
	}
}

func TestEnqueueStagesRowInCallerTx(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)
	ctx := context.Background()

	payload, err := json.Marshal(LLMCallCompletedPayload{
		NodeID: "n1", Model: "glm-5.3",
		PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120,
		CostAmount: "0.003200", CostCurrency: "CNY",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	evt := Event{
		ID: nextID(t), EventType: EventLLMCallCompleted, ExecutionID: execID,
		NodeExecID: 7, NodeID: "n1", AttemptID: 9,
		ActorType: ActorLLM, ActorID: "glm-5.3",
		Payload: payload,
		Usage: &engine.AttemptUsage{
			Model: "glm-5.3", PromptTokens: 100, CompletionTokens: 20,
			TotalTokens: 120, CostAmount: "0.003200", CostCurrency: "CNY", Billed: true,
		},
	}
	id := evt.ID

	// 回滚臂:暂存行与调用方事务同亡(镜像 TestAppendDiesWithTransaction)。
	boom := errors.New("boom")
	err = engine.WithTx(ctx, d, func(q engine.DBTX) error {
		if e := Enqueue(ctx, q, evt); e != nil {
			return e
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithTx err = %v, want boom", err)
	}
	if rows := readStaged(t, d, execID); len(rows) != 0 {
		t.Fatalf("staged after rollback = %d, want 0", len(rows))
	}

	// 提交臂:行落 audit_pending,列逐位断言。
	if err := engine.WithTx(ctx, d, func(q engine.DBTX) error {
		return Enqueue(ctx, q, evt)
	}); err != nil {
		t.Fatalf("Enqueue commit: %v", err)
	}
	stageCleanup(t, d, execID)
	rows := readStaged(t, d, execID)
	if len(rows) != 1 {
		t.Fatalf("staged = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.id != id || r.eventType != EventLLMCallCompleted || r.executionID != execID {
		t.Fatalf("identity = %+v", r)
	}
	if r.nodeExecID.Int64 != 7 || r.nodeID.String != "n1" || r.attemptID.Int64 != 9 {
		t.Fatalf("anchors = %+v", r)
	}
	if r.model.String != "glm-5.3" || !r.costAmount.Valid || r.costAmount.Float64 != 0.0032 {
		t.Fatalf("usage cols = model %v cost %v", r.model, r.costAmount)
	}
	if r.piiMasked {
		t.Fatal("pii_masked = true on unmarked payload")
	}
	// event_at 落在暂存时刻:与「事件时刻=暂存事务时刻」契约一致(近now断言,
	// DB 端格式化到秒)。
	var now string
	if err := d.QueryRow(`SELECT DATE_FORMAT(NOW(3), '%Y-%m-%dT%H:%i:%s')`).Scan(&now); err != nil {
		t.Fatalf("read now: %v", err)
	}
	if r.eventAt != now && !withinOneSecond(r.eventAt, now) {
		t.Fatalf("event_at = %s, want ~%s", r.eventAt, now)
	}
	var got LLMCallCompletedPayload
	if err := json.Unmarshal(r.payload, &got); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if got.Model != "glm-5.3" || got.TotalTokens != 120 {
		t.Fatalf("payload = %+v", got)
	}
}

func withinOneSecond(a, b string) bool {
	// 秒级字符串邻近判定(跨秒边界容差);形态 YYYY-MM-DDTHH:MM:SS。
	if len(a) != 19 || len(b) != 19 {
		return false
	}
	diff := 0
	for i := 0; i < 19; i++ {
		if a[i] != b[i] {
			diff++
		}
	}
	return diff <= 2 && strings.HasPrefix(a[:14], b[:14])
}

// TestEnqueueSanitizesBeforeStaging:脱敏先于暂存落库(「原始永不入 payload」
// 含 WAL 表);pii_masked 标志与掩蔽动作单编。
func TestEnqueueSanitizesBeforeStaging(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)
	stageCleanup(t, d, execID)

	if err := Enqueue(context.Background(), d, Event{
		ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: execID,
		NodeExecID: 1, NodeID: "n1", ActorType: ActorWorker, ActorID: "w1",
		Payload: json.RawMessage(`{"user_data":{"email":"a@b.c"},"from":"PENDING"}`),
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	rows := readStaged(t, d, execID)
	if len(rows) != 1 {
		t.Fatalf("staged = %d, want 1", len(rows))
	}
	if !rows[0].piiMasked {
		t.Fatal("pii_masked = false, want true")
	}
	if strings.Contains(string(rows[0].payload), "a@b.c") {
		t.Fatalf("raw PII leaked into staging: %s", rows[0].payload)
	}
	if !strings.Contains(string(rows[0].payload), "***") {
		t.Fatalf("masked token missing: %s", rows[0].payload)
	}
}
