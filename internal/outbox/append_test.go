package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
)

// idGen:进程唯一假 Snowflake(与 engine/fixtures_test.go 同手法:启动时刻
// 基座异或 PID + 单调计数)。PID 熵是必须的:本包与 engine 共享
// braid_engine_test,go test ./... 并行时两二进制同 ~1ms 桶启动会产生
// 同基座 → 跨进程 PK 1062 假红。重复 ~10 行是包级隔离的价格(engine 的
// fixture 是包私有,不可跨包 import)。
var (
	idBase = uint64(time.Now().UnixNano()>>20)<<16 ^ uint64(os.Getpid())<<36
	idCtr  atomic.Uint64
)

func newID() uint64 { return idBase + idCtr.Add(1) }

// countDBTX:统计 ExecContext 次数的 DBTX 包装——「校验不触 DB」的断言锚。
type countDBTX struct {
	d     *sql.DB
	execs atomic.Int64
}

func (c *countDBTX) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	c.execs.Add(1)
	return c.d.ExecContext(ctx, q, args...)
}

func (c *countDBTX) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return c.d.QueryRowContext(ctx, q, args...)
}

func validEvent() Event {
	return Event{
		ID:          newID(),
		ExecutionID: newID(),
		EventType:   "node.task_enqueue",
		TaskID:      fmt.Sprintf("att:%d:1", newID()),
		Payload:     json.RawMessage(`{"node_execution_id":1}`),
	}
}

// TestAppendRejectsInvalidEvent:非法输入表驱动,全部不触 DB(ErrInvalidEvent +
// execs==0)。咬住变异:校验挪到 INSERT 之后 / 错误被吞。
func TestAppendRejectsInvalidEvent(t *testing.T) {
	d := sqltest.MustOpen(t)
	cases := []struct {
		name string
		mut  func(e *Event)
	}{
		{"zero id", func(e *Event) { e.ID = 0 }},
		{"zero execution id", func(e *Event) { e.ExecutionID = 0 }},
		{"empty event type", func(e *Event) { e.EventType = "" }},
		{"event type over width", func(e *Event) { e.EventType = string(make([]byte, 65)) }},
		{"empty task id", func(e *Event) { e.TaskID = "" }},
		{"task id outside vocabulary", func(e *Event) { e.TaskID = "bogus:1" }},
		{"empty payload", func(e *Event) { e.Payload = nil }},
		{"non-json payload", func(e *Event) { e.Payload = json.RawMessage(`{nope`) }},
		// 合法 JSON 且超 cap(原始用例含未转义控制字符,会先命中 json.Valid 分支,
		// cap 检查零覆盖)
		{"payload over cap", func(e *Event) {
			e.Payload = json.RawMessage(`"` + strings.Repeat("a", maxPayloadBytes) + `"`)
		}},
		{"bad queue charset", func(e *Event) { e.Queue = "Q1" }},
		{"bad trace id", func(e *Event) { e.TraceID = "XYZ" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &countDBTX{d: d}
			evt := validEvent()
			tc.mut(&evt)
			err := Append(context.Background(), w, evt)
			if !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("want ErrInvalidEvent, got %v", err)
			}
			if n := w.execs.Load(); n != 0 {
				t.Fatalf("validation touched DB: %d execs", n)
			}
		})
	}
}

// TestAppendInsertsPendingRow:正例——合法最小事件落行,形状逐列断言。
// available_at 用 SQL 侧比较,绕开应用钟差。
func TestAppendInsertsPendingRow(t *testing.T) {
	d := sqltest.MustOpen(t)
	evt := validEvent()
	evt.Queue = "" // 空 → App 侧规范化为 default
	evt.TraceID = "0123456789abcdef0123456789abcdef"
	execID := evt.ExecutionID
	t.Cleanup(func() { mustExec(t, d, `DELETE FROM outbox WHERE execution_id=?`, execID) })

	if err := Append(context.Background(), d, evt); err != nil {
		t.Fatalf("Append: %v", err)
	}
	var (
		status           string
		attempts         int
		queue            string
		payload          sql.NullString
		dispatchedAt     sql.NullTime
		lastErr          sql.NullString
		availableElapsed sql.NullBool
		traceID          sql.NullString
	)
	err := d.QueryRowContext(context.Background(), `
SELECT status, deliver_attempts, queue, payload, dispatched_at, last_error, trace_id,
       available_at BETWEEN NOW(3) - INTERVAL 5 SECOND AND NOW(3)
  FROM outbox WHERE id=?`, evt.ID).
		Scan(&status, &attempts, &queue, &payload, &dispatchedAt, &lastErr, &traceID, &availableElapsed)
	if err != nil {
		t.Fatalf("scan outbox %d: %v", evt.ID, err)
	}
	if status != "PENDING" || attempts != 0 || queue != "default" {
		t.Fatalf("shape: status=%s attempts=%d queue=%s", status, attempts, queue)
	}
	if dispatchedAt.Valid || lastErr.Valid {
		t.Fatalf("shape: dispatched_at/last_error set on fresh row")
	}
	if !availableElapsed.Valid || !availableElapsed.Bool {
		t.Fatalf("available_at not within [NOW-5s, NOW]")
	}
	jsonEqStr(t, payload.String, evt.Payload)
	if !traceID.Valid || traceID.String != evt.TraceID {
		t.Fatalf("trace_id = %v, want %s", traceID, evt.TraceID)
	}
}

// TestTaskIDVocabularyResume:resume: 词表四段式形态钉死(T4.2,append.go 包
// 注释的不变量锚)。旧三段式(0001 §5 注释形态)必须拒收——它正是「同
// (exec,node) 二次续跑复用同 ID 被 archived 副本挡死」豁口的载体;变异:
// 正则回退三段式 → 旧形态用例红;放开 attempt 段词表 → 非数字段红。
func TestTaskIDVocabularyResume(t *testing.T) {
	d := sqltest.MustOpen(t)
	base := Event{
		ExecutionID: newID(),
		EventType:   "execution.resume",
		Payload:     json.RawMessage(`{"execution_id":1}`),
	}
	t.Cleanup(func() { mustExec(t, d, `DELETE FROM outbox WHERE execution_id=?`, base.ExecutionID) })
	ok := []string{
		fmt.Sprintf("resume:%d:boss:2", newID()),
		fmt.Sprintf("resume:%d:a1_b2:10", newID()),
	}
	bad := map[string]string{
		"旧三段式(T4.2 前 DDL 注释形态,已扩)": fmt.Sprintf("resume:%d:boss", newID()),
		"attempt 段非数字":             fmt.Sprintf("resume:%d:boss:x", newID()),
		"node_id 段大写":              fmt.Sprintf("resume:%d:Boss:1", newID()),
		"attempt 段非数字(字母)":         fmt.Sprintf("resume:%d:boss:x", newID()),
		"前缀错位":                     fmt.Sprintf("resumex:%d:boss:1", newID()),
	}
	for _, id := range ok {
		evt := base
		evt.TaskID = id
		evt.ID = newID()
		if err := Append(context.Background(), d, evt); err != nil {
			t.Fatalf("valid resume task id %q rejected: %v", id, err)
		}
	}
	for name, id := range bad {
		evt := base
		evt.TaskID = id
		evt.ID = newID()
		w := &countDBTX{d: d}
		if err := Append(context.Background(), w, evt); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("%s: task id %q: want ErrInvalidEvent, got %v", name, id, err)
		}
		if n := w.execs.Load(); n != 0 {
			t.Fatalf("%s: validation touched DB", name)
		}
	}
}

// TestTaskIDVocabularyNotify:notify: 词表钉死(T4.3 审批超时升级通知,approval.notify
// 事件的 TaskID)。恰好一次语义锚在 escalated_at(DB 谓词),单段 node_execution_id
// 即可承载「每个挂起节点至多一条通知」;变异:正则放开二段 → 二段形态红。
func TestTaskIDVocabularyNotify(t *testing.T) {
	d := sqltest.MustOpen(t)
	base := Event{
		ExecutionID: newID(),
		EventType:   "approval.notify",
		Payload:     json.RawMessage(`{"execution_id":1}`),
	}
	t.Cleanup(func() { mustExec(t, d, `DELETE FROM outbox WHERE execution_id=?`, base.ExecutionID) })
	ok := []string{
		fmt.Sprintf("notify:%d", newID()),
	}
	bad := map[string]string{
		"空 id 段":   "notify:",
		"非数字段":     fmt.Sprintf("notify:node-%d", newID()),
		"二段式(词表外)": fmt.Sprintf("notify:%d:1", newID()),
		"前缀错位":     fmt.Sprintf("notifx:%d", newID()),
		"大写前缀":     fmt.Sprintf("Notify:%d", newID()),
	}
	for _, id := range ok {
		evt := base
		evt.TaskID = id
		evt.ID = newID()
		if err := Append(context.Background(), d, evt); err != nil {
			t.Fatalf("valid notify task id %q rejected: %v", id, err)
		}
	}
	for name, id := range bad {
		evt := base
		evt.TaskID = id
		evt.ID = newID()
		w := &countDBTX{d: d}
		if err := Append(context.Background(), w, evt); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("%s: task id %q: want ErrInvalidEvent, got %v", name, id, err)
		}
		if n := w.execs.Load(); n != 0 {
			t.Fatalf("%s: validation touched DB", name)
		}
	}
}

// TestClampErrLen:截断保持在 rune 边界(字节拦腰截会产出非法 UTF-8,utf8mb4
// 严格模式 1366 拒收 → 记账回滚 → 行永动重试)。
func TestClampErrLen(t *testing.T) {
	ascii := strings.Repeat("x", 3000)
	if got := clampErrLen(ascii); len(got) != maxErrText {
		t.Fatalf("ascii clamp len = %d, want %d", len(got), maxErrText)
	}
	multibyte := strings.Repeat("世", 1100) // 3 bytes/rune
	got := clampErrLen(multibyte)
	if !utf8.ValidString(got) {
		t.Fatalf("clamped multibyte invalid utf8: %q…", got[:16])
	}
	if len(got) > maxErrText {
		t.Fatalf("clamped len %d > %d", len(got), maxErrText)
	}
	if got2 := clampErrLen("short"); got2 != "short" {
		t.Fatalf("short string mutated: %q", got2)
	}
}

// TestAppendInsideWithTxCommits:与 engine.WithTx 组合的冒烟(完整判别性
// 故障测试见 tx_test.go 验收①)。
func TestAppendInsideWithTxCommits(t *testing.T) {
	d := sqltest.MustOpen(t)
	evt := validEvent()
	execID := evt.ExecutionID
	t.Cleanup(func() { mustExec(t, d, `DELETE FROM outbox WHERE execution_id=?`, execID) })

	err := engine.WithTx(context.Background(), d, func(q engine.DBTX) error {
		return Append(context.Background(), q, evt)
	})
	if err != nil {
		t.Fatalf("WithTx+Append: %v", err)
	}
	var n int
	if err := d.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM outbox WHERE execution_id=?`, execID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 row, got %d", n)
	}
}

func mustExec(t *testing.T, d *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := d.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("seed/exec %q: %v", query, err)
	}
}

func jsonEqStr(t *testing.T, got string, want json.RawMessage) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("got invalid JSON %q: %v", got, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("want invalid JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("JSON not semantically equal: got %s want %s", got, want)
	}
}
