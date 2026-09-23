package audit

// 单测:参数校验表驱动(零 DB,t13 不可达也跑)。
// 集成:t13 真 MySQL,不可达自动 skip;验收锚 = exec_date 物化、列落位、
// execution 缺行响亮失败、同事务同存亡。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/snowflake"
)

const testSchema = "braid_audit_test"

var (
	testIDsOnce sync.Once
	testIDs     *snowflake.Generator
)

func mustTestIDs(t *testing.T) *snowflake.Generator {
	t.Helper()
	testIDsOnce.Do(func() {
		g, err := snowflake.New(1)
		if err != nil {
			t.Fatalf("test snowflake: %v", err)
		}
		testIDs = g
	})
	if testIDs == nil {
		t.Fatal("test snowflake generator not initialized")
	}
	return testIDs
}

func nextID(t *testing.T) uint64 {
	t.Helper()
	id, err := mustTestIDs(t).Next()
	if err != nil {
		t.Fatalf("gen id: %v", err)
	}
	return id
}

func TestValidateRejectsInvalidEvents(t *testing.T) {
	valid := Event{
		ID: 1, EventType: EventNodeStateChanged, ExecutionID: 2,
		NodeExecID: 3, NodeID: "n1", AttemptID: 4,
		ActorType: ActorWorker, ActorID: "w1", Payload: json.RawMessage(`{}`),
	}
	cases := []struct {
		name string
		mut  func(*Event)
	}{
		{"zero id", func(e *Event) { e.ID = 0 }},
		{"zero execution id", func(e *Event) { e.ExecutionID = 0 }},
		{"event type not in vocabulary", func(e *Event) { e.EventType = "node.state_change" }},
		{"event type typo", func(e *Event) { e.EventType = "node.stae_changed" }},
		{"empty event type", func(e *Event) { e.EventType = "" }},
		{"actor type unknown", func(e *Event) { e.ActorType = "robot" }},
		{"actor type empty", func(e *Event) { e.ActorType = "" }},
		{"actor type too long", func(e *Event) { e.ActorType = fmt.Sprintf("%17s", "x") }},
		{"actor id empty", func(e *Event) { e.ActorID = "" }},
		{"actor id too long", func(e *Event) { e.ActorID = fmt.Sprintf("%129s", "x") }},
		{"node id too long", func(e *Event) { e.NodeID = fmt.Sprintf("%65s", "x") }},
		{"trace id not hex", func(e *Event) { e.TraceID = "XYZ" }},
		{"trace id wrong length", func(e *Event) { e.TraceID = "0123456789abcdef0123456789abcde" }},
		{"payload not json", func(e *Event) { e.Payload = json.RawMessage(`{`) }},
		{"payload invalid utf-8", func(e *Event) {
			// json.Valid 只查语法:字符串内 0xFF 字节合法过检,但 MySQL 会原样
			// 存入 → spec-invalid JSON 行(utf8.Valid 是防线,review 修)。
			e.Payload = json.RawMessage{'"', 0xFF, '"'}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evt := valid
			tc.mut(&evt)
			err := validate(evt)
			if !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("validate() = %v, want ErrInvalidEvent", err)
			}
		})
	}
	// 合法全集快速回归(全部事件词表 × 两种 actor)。
	for _, et := range []string{
		EventNodeStateChanged, EventNodeRedispatched, EventAttemptReaped,
		EventNodeDeadlineExtended, EventBranchEvaluated,
	} {
		for _, at := range []string{ActorWorker, ActorSystem, ActorUser, ActorTool, ActorLLM, ActorApproval} {
			evt := valid
			evt.EventType = et
			evt.ActorType = at
			if err := validate(evt); err != nil {
				t.Fatalf("validate(%s/%s) = %v, want nil", et, at, err)
			}
		}
	}
	// trace_id 合法形(32 hex 小写)。
	evt := valid
	evt.TraceID = "0123456789abcdef0123456789abcdef"
	if err := validate(evt); err != nil {
		t.Fatalf("validate(valid trace) = %v, want nil", err)
	}
}

// seedExec 落最小 definition + execution 对(FK),返回 execID。created_at 钉在
// 固定历史日期(必须在 0001 引导分区 [2026-09-21, 2026-10-04) 内):exec_date
// 物化断言因此有判别力——DATE(NOW(3)) 变异会立即红(review 修)。
const seedExecCreatedAt = "2026-09-22 12:00:00"

func seedExec(t *testing.T, d *sql.DB) uint64 {
	t.Helper()
	defID := nextID(t)
	execID := nextID(t)
	name := fmt.Sprintf("audit-wf-%d", defID)
	ins := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	ins(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', '{}', ?, 'test', 'test', 'test', NOW(3))`,
		defID, name, string(make([]byte, 64)))
	ins(`INSERT INTO workflow_execution
	  (id, definition_id, definition_name, definition_version, definition_hash, status, triggered_by, input, created_at)
	  VALUES (?, ?, ?, 1, ?, 'RUNNING', 'audit-test', '{}', ?)`,
		execID, defID, name, string(make([]byte, 64)), seedExecCreatedAt)
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM audit_log WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM workflow_execution WHERE id=?`, execID)
		_, _ = d.Exec(`DELETE FROM workflow_definition WHERE id=?`, defID)
	})
	return execID
}

// auditRow 断言用快照。
type auditRow struct {
	id          uint64
	execDate    string
	eventType   string
	executionID uint64
	nodeExecID  sql.NullInt64
	nodeID      sql.NullString
	attemptID   sql.NullInt64
	actorType   string
	actorID     string
	traceID     sql.NullString
	payload     []byte
	prevHash    string
	piiMasked   bool
}

func readAudit(t *testing.T, d *sql.DB, execID uint64) []auditRow {
	t.Helper()
	rows, err := d.Query(`
SELECT id, exec_date, event_type, execution_id, node_execution_id, node_id,
       attempt_id, actor_type, actor_id, trace_id, payload, prev_hash, pii_masked
  FROM audit_log WHERE execution_id=? ORDER BY id`, execID)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.id, &r.execDate, &r.eventType, &r.executionID, &r.nodeExecID,
			&r.nodeID, &r.attemptID, &r.actorType, &r.actorID, &r.traceID,
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

func TestAppendMaterializesRow(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)

	payload, err := json.Marshal(NodeStateChangedPayload{
		From: "PENDING", To: "EXECUTING", NodeID: "n1", Version: 7, AttemptNo: 1, WorkerID: "w1",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	id := nextID(t)
	if err := Append(context.Background(), d, Event{
		ID: id, EventType: EventNodeStateChanged, ExecutionID: execID,
		NodeExecID: 42, NodeID: "n1", AttemptID: 99,
		ActorType: ActorWorker, ActorID: "w1", Payload: payload,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	rows := readAudit(t, d, execID)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	r := rows[0]
	// exec_date = DATE(execution.created_at),与写入时刻不同日(seed 钉
	// 2026-09-22)——DATE(NOW(3)) 变异在此红(review 修:判别力)。驱动把
	// DATE 回读成 time 字符串(RFC3339 形),字面比对用前缀。
	var wantDate string
	if err := d.QueryRow(`SELECT DATE(created_at) FROM workflow_execution WHERE id=?`, execID).
		Scan(&wantDate); err != nil {
		t.Fatalf("read exec created_at: %v", err)
	}
	if r.execDate != wantDate || !strings.HasPrefix(wantDate, "2026-09-22") {
		t.Fatalf("exec_date = %s, want %s(=2026-09-22)", r.execDate, wantDate)
	}
	if r.id != id || r.eventType != EventNodeStateChanged || r.executionID != execID {
		t.Fatalf("row identity = %+v", r)
	}
	if r.nodeExecID.Int64 != 42 || r.nodeID.String != "n1" || r.attemptID.Int64 != 99 {
		t.Fatalf("node anchors = %+v", r)
	}
	if r.actorType != ActorWorker || r.actorID != "w1" {
		t.Fatalf("actor = %s/%s", r.actorType, r.actorID)
	}
	// P0 默认列:trace 占位 NULL、未启链 prev_hash=''、未脱敏 0。
	if r.traceID.Valid || r.prevHash != "" || r.piiMasked {
		t.Fatalf("P0 defaults violated: trace=%v prev_hash=%q pii=%v", r.traceID, r.prevHash, r.piiMasked)
	}
	var got NodeStateChangedPayload
	if err := json.Unmarshal(r.payload, &got); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if got.From != "PENDING" || got.To != "EXECUTING" || got.Version != 7 || got.AttemptNo != 1 {
		t.Fatalf("payload = %+v", got)
	}

	// 空 payload 规范化为 {}(DDL:无正文事件存 {});空 NodeID → NULL
	// (执行级事件的判别锚,'' 会静默错分类——review 修)。
	if err := Append(context.Background(), d, Event{
		ID: nextID(t), EventType: EventBranchEvaluated, ExecutionID: execID,
		NodeExecID: 43, ActorType: ActorWorker, ActorID: "w1",
	}); err != nil {
		t.Fatalf("Append(empty payload): %v", err)
	}
	rows = readAudit(t, d, execID)
	last := rows[len(rows)-1]
	if string(last.payload) != "{}" {
		t.Fatalf("empty payload = %s, want {}", last.payload)
	}
	if last.nodeID.Valid {
		t.Fatalf("empty node_id = %q, want NULL", last.nodeID.String)
	}
}

func TestAppendExecutionVanished(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	ghost := nextID(t) // 不存在的 execution
	err := Append(context.Background(), d, Event{
		ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: ghost,
		NodeExecID: 1, NodeID: "n1", ActorType: ActorSystem, ActorID: "reaper:x",
		Payload: json.RawMessage(`{}`),
	})
	if !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("Append(vanished exec) = %v, want ErrNotFound", err)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE execution_id=?`, ghost).
		Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("ghost rows = %d, want 0", n)
	}
}

func TestAppendDiesWithTransaction(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)

	boom := errors.New("boom")
	err := engine.WithTx(context.Background(), d, func(q engine.DBTX) error {
		if err := Append(context.Background(), q, Event{
			ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: execID,
			NodeExecID: 1, NodeID: "n1", ActorType: ActorWorker, ActorID: "w1",
			Payload: json.RawMessage(`{}`),
		}); err != nil {
			return err
		}
		return boom // 审计行已写、事务回滚 ⇒ 行同亡(状态迁移与审计同存亡的地基)
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithTx err = %v, want boom", err)
	}
	if rows := readAudit(t, d, execID); len(rows) != 0 {
		t.Fatalf("rows after rollback = %d, want 0", len(rows))
	}

	if err := engine.WithTx(context.Background(), d, func(q engine.DBTX) error {
		return Append(context.Background(), q, Event{
			ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: execID,
			NodeExecID: 1, NodeID: "n1", ActorType: ActorWorker, ActorID: "w1",
			Payload: json.RawMessage(`{}`),
		})
	}); err != nil {
		t.Fatalf("WithTx commit: %v", err)
	}
	if rows := readAudit(t, d, execID); len(rows) != 1 {
		t.Fatalf("rows after commit = %d, want 1", len(rows))
	}
}

// TestAppendHoldsNoLockOnExecution:T2.6-F1 回归锚。tx1 写审计行后保持未提交,
// tx2 对同 execution 行 FOR UPDATE 必须立即成功——修订前 INSERT..SELECT 在 RR
// 下持共享锁至 COMMIT(1205);修订后一致读零锁。innodb_lock_wait_timeout=1
// 把回归从「50s 挂死」收敛为「1s 内失败」(行锁等待归 innodb 变量;
// lock_wait_timeout 只管 MDL,review 修;先例 engine/concurrency_test.go)。
func TestAppendHoldsNoLockOnExecution(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)
	ctx := context.Background()

	tx1, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	defer func() { _ = tx1.Rollback() }()
	if err := Append(ctx, tx1, Event{
		ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: execID,
		NodeExecID: 1, NodeID: "n1", ActorType: ActorWorker, ActorID: "w1",
		Payload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("Append in tx1: %v", err)
	}

	tx2, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	defer func() { _ = tx2.Rollback() }()
	if _, err := tx2.ExecContext(ctx, `SET SESSION innodb_lock_wait_timeout=1`); err != nil {
		t.Fatalf("set innodb_lock_wait_timeout: %v", err)
	}
	var st string
	if err := tx2.QueryRowContext(ctx,
		`SELECT status FROM workflow_execution WHERE id=? FOR UPDATE`, execID).Scan(&st); err != nil {
		t.Fatalf("FOR UPDATE blocked by audit write (写入器持锁,回归): %v", err)
	}
	if st != "RUNNING" {
		t.Fatalf("locked row status = %s, want RUNNING", st)
	}
}
