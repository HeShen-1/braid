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

	"go.opentelemetry.io/otel/trace"

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
		{"linked trace id not hex", func(e *Event) { e.LinkedTraceID = "NOTHEX" }},
		{"linked trace id wrong length", func(e *Event) { e.LinkedTraceID = "0123456789abcdef0123456789abcde" }},
		{"linked on non-segment-head event", func(e *Event) {
			e.LinkedTraceID = "0123456789abcdef0123456789abcdef" // 词表闸:段首家族(node.state_changed 等行)之外拒绝
		}},
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
	// 遍历 knownEvents 而非硬编码清单:新词登记后此循环自动覆盖,防词表回归静默逃逸。
	// T18.3 起 node.taint_propagated 有 payload 形状闸,该词在此用合法形状
	// (其余词的通用 {} 形状不受闸影响)。
	for et := range knownEvents {
		for _, at := range []string{ActorWorker, ActorSystem, ActorUser, ActorTool, ActorLLM, ActorApproval} {
			evt := valid
			evt.EventType = et
			evt.ActorType = at
			if et == EventNodeTaintPropagated {
				evt.Payload = json.RawMessage(`{"node_id":"n1","sink":"none","paths":["inputs.q"]}`)
			}
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
	// linked_trace_id 同域(段首家族单点携带——审批双词[T6.2]+ 回滚/取消
	// 四词[T18.5];词表闸见上表与本文件正例组)。
	// T18.5 起段首家族扩四词:回滚/取消两族的触发行同携 linked 指回执行
	// root(镜像审批 decided/decision_denied);node 行仍拒绝(段首单点)。
	evt.LinkedTraceID = "fedcba9876543210fedcba9876543210"
	evt.EventType = EventApprovalDecided
	if err := validate(evt); err != nil {
		t.Fatalf("validate(approval.decided with linked) = %v, want nil", err)
	}
	evt.EventType = EventApprovalDecisionDenied
	if err := validate(evt); err != nil {
		t.Fatalf("validate(approval.decision_denied with linked) = %v, want nil", err)
	}
	for _, et := range []string{
		EventExecutionRolledBack, EventRollbackDenied,
		EventExecutionCancelled, EventCancelDenied,
	} {
		evt.EventType = et
		if err := validate(evt); err != nil {
			t.Fatalf("validate(%s with linked) = %v, want nil(T18.5 段首家族)", et, err)
		}
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
		_, _ = d.Exec(`DELETE FROM audit_pending WHERE execution_id=?`, execID) // T6.1 暂存面(防御对称)
		_, _ = d.Exec(`DELETE FROM chain_heads WHERE execution_id=?`, execID)   // T9.3 链锚(防御对称)
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
	linkedTrace sql.NullString
	payload     []byte
	prevHash    string
	piiMasked   bool
}

func readAudit(t *testing.T, d *sql.DB, execID uint64) []auditRow {
	t.Helper()
	rows, err := d.Query(`
SELECT id, exec_date, event_type, execution_id, node_execution_id, node_id,
       attempt_id, actor_type, actor_id, trace_id, linked_trace_id, payload,
       prev_hash, pii_masked
  FROM audit_log WHERE execution_id=? ORDER BY id`, execID)
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

// traceCtx 构造携带给定 trace id 的 ctx(仅 otel/trace API,零 SDK 依赖;
// 生产路径 = tracing 包的 span 注入,同值语义)。
func traceCtx(t *testing.T, hexID string) context.Context {
	t.Helper()
	tid, err := trace.TraceIDFromHex(hexID)
	if err != nil {
		t.Fatalf("trace id from hex: %v", err)
	}
	sid, _ := trace.SpanIDFromHex("0af7651916cd43dd")
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled,
	})
	return trace.ContextWithSpanContext(context.Background(), sc)
}

// T6.2 双通道:ctx 自动提取(生产主路径,十发射点零改动)+ 显式值优先
// (测试注入位);linked 段首单点显式携带。
func TestAppendTraceFromContextAndLinked(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)

	// ctx 提取臂:evt 零 TraceID,ctx span context 落列;linked 同行落列
	//(词表闸 ⇒ linked 组合用段首词,与生产决策行同形)。
	id1 := nextID(t)
	if err := engine.WithTx(traceCtx(t, "4bf92f3577b34da6a3ce929d0e0e4736"), d, func(q engine.DBTX) error {
		return Append(traceCtx(t, "4bf92f3577b34da6a3ce929d0e0e4736"), q, Event{
			ID: id1, EventType: EventApprovalDecided, ExecutionID: execID,
			NodeID: "n1", ActorType: ActorUser, ActorID: "u1",
			LinkedTraceID: "fedcba9876543210fedcba9876543210",
		})
	}); err != nil {
		t.Fatalf("Append(ctx trace): %v", err)
	}
	// 显式优先臂:ctx 带 X、evt 带 Y → 落 Y(注入位契约)。
	id2 := nextID(t)
	if err := engine.WithTx(traceCtx(t, "4bf92f3577b34da6a3ce929d0e0e4736"), d, func(q engine.DBTX) error {
		return Append(traceCtx(t, "4bf92f3577b34da6a3ce929d0e0e4736"), q, Event{
			ID: id2, EventType: EventNodeStateChanged, ExecutionID: execID,
			NodeID: "n2", ActorType: ActorWorker, ActorID: "w1",
			TraceID: "0123456789abcdef0123456789abcdef",
		})
	}); err != nil {
		t.Fatalf("Append(explicit trace): %v", err)
	}
	// 纯 background ctx 臂:无 span context → trace NULL(行为不变锚)。
	id3 := nextID(t)
	if err := engine.WithTx(context.Background(), d, func(q engine.DBTX) error {
		return Append(context.Background(), q, Event{
			ID: id3, EventType: EventNodeStateChanged, ExecutionID: execID,
			NodeID: "n3", ActorType: ActorWorker, ActorID: "w1",
		})
	}); err != nil {
		t.Fatalf("Append(no trace): %v", err)
	}

	rows := readAudit(t, d, execID)
	if len(rows) != 3 {
		t.Fatalf("audit rows = %d, want 3", len(rows))
	}
	if rows[0].traceID.String != "4bf92f3577b34da6a3ce929d0e0e4736" || !rows[0].traceID.Valid {
		t.Fatalf("row1 trace_id = %v, want ctx trace", rows[0].traceID)
	}
	if rows[0].linkedTrace.String != "fedcba9876543210fedcba9876543210" || !rows[0].linkedTrace.Valid {
		t.Fatalf("row1 linked_trace_id = %v, want explicit linked", rows[0].linkedTrace)
	}
	if rows[1].traceID.String != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("row2 trace_id = %v, want explicit value to win", rows[1].traceID)
	}
	if rows[1].linkedTrace.Valid {
		t.Fatalf("row2 linked_trace_id = %v, want NULL", rows[1].linkedTrace)
	}
	if rows[2].traceID.Valid {
		t.Fatalf("row3 trace_id = %v, want NULL (no span ctx, behavior preserved)", rows[2].traceID)
	}
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
	if err := engine.WithTx(context.Background(), d, func(q engine.DBTX) error {
		return Append(context.Background(), q, Event{
			ID: id, EventType: EventNodeStateChanged, ExecutionID: execID,
			NodeExecID: 42, NodeID: "n1", AttemptID: 99,
			ActorType: ActorWorker, ActorID: "w1", Payload: payload,
		})
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
	// 默认列:trace/linked 占位 NULL、链首 prev_hash=''(T9.3 起链仍以 ''
	// 开头,见 chain_test 全链锚)、未脱敏 0(background ctx 无 span context,
	// 行为不变锚)。
	if r.traceID.Valid || r.linkedTrace.Valid || r.prevHash != "" || r.piiMasked {
		t.Fatalf("defaults violated: trace=%v linked=%v prev_hash=%q pii=%v",
			r.traceID, r.linkedTrace, r.prevHash, r.piiMasked)
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
	if err := engine.WithTx(context.Background(), d, func(q engine.DBTX) error {
		return Append(context.Background(), q, Event{
			ID: nextID(t), EventType: EventBranchEvaluated, ExecutionID: execID,
			NodeExecID: 43, ActorType: ActorWorker, ActorID: "w1",
		})
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

// TestAppendSanitizesPayload(T6.1):同步路径同过脱敏钩子——掩蔽值落行、
// pii_masked=1;未标注行 pii_masked=0(TestAppendMaterializesRow 已锚)。
func TestAppendSanitizesPayload(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)

	if err := engine.WithTx(context.Background(), d, func(q engine.DBTX) error {
		return Append(context.Background(), q, Event{
			ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: execID,
			NodeExecID: 1, NodeID: "n1", ActorType: ActorWorker, ActorID: "w1",
			Payload: json.RawMessage(`{"user_data":{"phone":"13800000000"},"from":"PENDING"}`),
		})
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	rows := readAudit(t, d, execID)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if !rows[0].piiMasked {
		t.Fatal("pii_masked = false, want true")
	}
	if strings.Contains(string(rows[0].payload), "13800000000") {
		t.Fatalf("PII leaked into audit_log: %s", rows[0].payload)
	}
	if !strings.Contains(string(rows[0].payload), "***") {
		t.Fatalf("mask token missing: %s", rows[0].payload)
	}
}

func TestAppendExecutionVanished(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	ghost := nextID(t) // 不存在的 execution
	err := engine.WithTx(context.Background(), d, func(q engine.DBTX) error {
		return Append(context.Background(), q, Event{
			ID: nextID(t), EventType: EventNodeStateChanged, ExecutionID: ghost,
			NodeExecID: 1, NodeID: "n1", ActorType: ActorSystem, ActorID: "reaper:x",
			Payload: json.RawMessage(`{}`),
		})
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

// TestValidateLLMUsage(T3.1):usage/cost 冗余列组仅 llm.call.completed 可携;
// 域约束镜像(engine.AttemptUsage.Validate 单点)。零 DB。
func TestValidateLLMUsage(t *testing.T) {
	base := Event{
		ID: 1, EventType: EventLLMCallCompleted, ExecutionID: 2,
		NodeExecID: 3, NodeID: "n1", AttemptID: 4,
		ActorType: ActorLLM, ActorID: "glm-5.3", Payload: json.RawMessage(`{}`),
		Usage: &engine.AttemptUsage{
			Model: "glm-5.3", PromptTokens: 1000, CompletionTokens: 150, TotalTokens: 1150,
			CostAmount: "0.003200", CostCurrency: "CNY", Billed: true,
		},
	}
	if err := validate(base); err != nil {
		t.Fatalf("validate(billed llm event) = %v, want nil", err)
	}
	cases := []struct {
		name string
		mut  func(*Event)
	}{
		{"usage on non-llm event", func(e *Event) { e.EventType = EventNodeStateChanged }},
		{"unbilled carrying cost strings", func(e *Event) {
			e.Usage.Billed = false
		}},
		{"bad currency", func(e *Event) { e.Usage.CostCurrency = "cny" }},
		{"bad amount shape", func(e *Event) { e.Usage.CostAmount = "1.0000000" }},
		{"empty model", func(e *Event) { e.Usage.Model = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evt := base
			tc.mut(&evt)
			if err := validate(evt); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("validate() = %v, want ErrInvalidEvent", err)
			}
		})
	}
	// 不计费合法形:cost 两列将写 NULL,payload 缺省金额。
	unbilled := base
	unbilled.Usage = &engine.AttemptUsage{Model: "unknown", Estimated: true}
	if err := validate(unbilled); err != nil {
		t.Fatalf("validate(unbilled) = %v, want nil", err)
	}
	// 其余事件类型不携 usage(词表纪律):nil Usage 合法。
	plain := base
	plain.EventType = EventNodeStateChanged
	plain.ActorType = ActorWorker
	plain.ActorID = "w1"
	plain.Usage = nil
	if err := validate(plain); err != nil {
		t.Fatalf("validate(plain) = %v, want nil", err)
	}
	// llm.call.discarded 是 usage 冗余列组第二合法携带者(T6.4,D-t36 §7):
	// 该行是被丢弃调用唯一留下的台账痕迹,金额必须随行。(fresh Usage 指针:
	// 上方 cases 经共享指针变异已污染 base.Usage。)
	discarded := base
	discarded.EventType = EventLLMCallDiscarded
	discarded.ActorType = ActorWorker
	discarded.ActorID = "w1"
	discarded.Usage = &engine.AttemptUsage{
		Model: "glm-5.3", PromptTokens: 1000, CompletionTokens: 150, TotalTokens: 1150,
		CostAmount: "0.003200", CostCurrency: "CNY", Billed: true,
	}
	if err := validate(discarded); err != nil {
		t.Fatalf("validate(discarded with usage) = %v, want nil", err)
	}
}

// TestAppendLLMCallMaterializesRow(T3.1 双录 audit 半边):llm.call.completed
// 落 usage/cost 冗余列组(billed 与 unbilled 两形),金额/币种/estimated 与
// Event.Usage 逐字一致;payload 同步落结构体。
//
// 变异自查(实现错 → 哪条红):
//   - INSERT 丢 usage 列 → billed/unbilled 断言红;
//   - Billed=false 仍写金额 → unbilled 断言红;
//   - payload 与列单源被破坏 → payload 断言红。
func TestAppendLLMCallMaterializesRow(t *testing.T) {
	d := sqltest.MustOpenAt(t, testSchema)
	execID := seedExec(t, d)
	ctx := context.Background()

	billed := &engine.AttemptUsage{
		Model: "glm-5.3", PromptTokens: 1000, CompletionTokens: 150, TotalTokens: 1150,
		CostAmount: "0.003200", CostCurrency: "CNY", Billed: true,
	}
	unbilled := &engine.AttemptUsage{
		Model: "mystery", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, Estimated: true,
	}
	for i, u := range []*engine.AttemptUsage{billed, unbilled} {
		raw, err := json.Marshal(LLMCallCompletedPayload{
			NodeID: "n1", Model: u.Model,
			PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens,
			TotalTokens: u.TotalTokens,
			CostAmount:  u.CostAmount, CostCurrency: u.CostCurrency,
			CostEstimated: u.Estimated,
		})
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		if err := engine.WithTx(ctx, d, func(q engine.DBTX) error {
			return Append(ctx, q, Event{
				ID: nextID(t), EventType: EventLLMCallCompleted, ExecutionID: execID,
				NodeExecID: uint64(100 + i), NodeID: "n1", AttemptID: uint64(200 + i),
				ActorType: ActorLLM, ActorID: u.Model, Payload: raw, Usage: u,
			})
		}); err != nil {
			t.Fatalf("append llm event: %v", err)
		}
	}

	rows, err := d.Query(`
SELECT model, prompt_tokens, completion_tokens, total_tokens,
       cost_amount, cost_currency, cost_estimated
  FROM audit_log WHERE execution_id=? AND event_type=? ORDER BY id`,
		execID, EventLLMCallCompleted)
	if err != nil {
		t.Fatalf("query llm rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var got []engine.AttemptUsage
	for rows.Next() {
		var u engine.AttemptUsage
		var amount, currency sql.NullString
		var pt, ct, tt sql.NullInt64
		if err := rows.Scan(&u.Model, &pt, &ct, &tt, &amount, &currency, &u.Estimated); err != nil {
			t.Fatalf("scan: %v", err)
		}
		u.PromptTokens, u.CompletionTokens, u.TotalTokens = uint32(pt.Int64), uint32(ct.Int64), uint32(tt.Int64)
		u.CostAmount, u.CostCurrency = amount.String, currency.String
		u.Billed = amount.Valid
		got = append(got, u)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("llm rows = %d, want 2", len(got))
	}
	if got[0] != *billed {
		t.Errorf("billed row = %+v, want %+v", got[0], *billed)
	}
	if got[1] != *unbilled {
		t.Errorf("unbilled row = %+v, want %+v(cost NULL 语义)", got[1], *unbilled)
	}
}

// TestValidateTaintPayloadShape(T18.3,D-t183):node.taint_propagated 的 PII
// 红线形状闸——paths 必须全匹配 dsl 钉死的标识符形态、sink ∈ 闭集、非空、
// 条数有界。结构保证单点在 ComputeTaintSets,本闸是 audit 边界的机器可检
// 半边(「结构性兜底优于自觉」,防未来发射面回归静默写自由字节)。
func TestValidateTaintPayloadShape(t *testing.T) {
	valid := Event{
		ID: 1, EventType: EventNodeTaintPropagated, ExecutionID: 2,
		NodeExecID: 3, NodeID: "n1", AttemptID: 4,
		ActorType: ActorWorker, ActorID: "w1",
		Payload: json.RawMessage(`{"node_id":"n1","sink":"llm","paths":["inputs.bio","inputs.q2"]}`),
	}
	if err := validate(valid); err != nil {
		t.Fatalf("validate(valid taint) = %v, want nil", err)
	}
	cases := []struct {
		name    string
		payload string
	}{
		{"path free-text bytes", `{"node_id":"n1","sink":"llm","paths":["inputs.evil<SoS>"]}`},
		{"path caller-ish dotted key", `{"node_id":"n1","sink":"llm","paths":["inputs.a.b"]}`},
		{"path wrong namespace", `{"node_id":"n1","sink":"llm","paths":["workflow.email"]}`},
		{"path empty segment", `{"node_id":"n1","sink":"llm","paths":["inputs."]"]}`},
		{"empty paths", `{"node_id":"n1","sink":"llm","paths":[]}`},
		{"sink unknown", `{"node_id":"n1","sink":"outputs","paths":["inputs.q"]}`},
		{"empty node_id", `{"node_id":"","sink":"none","paths":["inputs.q"]}`},
		{"path count over bound", mkTaintPayloadOverflow()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evt := valid
			evt.Payload = json.RawMessage(tc.payload)
			if err := validate(evt); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("validate(%s) = %v, want ErrInvalidEvent", tc.name, err)
			}
		})
	}
}

// mkTaintPayloadOverflow 构造 65 条合法路径(越 inputs maxItems=64 上界)。
func mkTaintPayloadOverflow() string {
	paths := make([]string, 65)
	for i := range paths {
		paths[i] = fmt.Sprintf("inputs.p%02d", i)
	}
	b, _ := json.Marshal(map[string]any{"node_id": "n1", "sink": "llm", "paths": paths})
	return string(b)
}
