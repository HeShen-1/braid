package outbox

// 三原语的直接契约测试(relay 集成路径经 run/kill 测试覆盖;此处钉死
// 0 行裁决与列形状)。

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"braid/internal/db/sqltest"
)

func seedRow(t *testing.T, d *sql.DB, status string) uint64 {
	t.Helper()
	evt := validEvent()
	t.Cleanup(func() { mustExec(t, d, `DELETE FROM outbox WHERE execution_id=?`, evt.ExecutionID) })
	mustExec(t, d, `INSERT INTO outbox
		(id, execution_id, event_type, task_id, queue, payload, status, deliver_attempts, available_at)
		VALUES (?,?,?,?,?,?,?,0,NOW(3))`,
		evt.ID, evt.ExecutionID, evt.EventType, evt.TaskID, "default", evt.Payload, status)
	return evt.ID
}

// TestMarkDispatchedShape:PENDING 行成功标记的列形状(status/attempts/
// dispatched_at)。注:status-only CAS 命中只依赖行当前是 PENDING,「持锁」
// 前置是并发安全论证的前提而非机制开关——本测试在无竞争单行上验证列语义。
func TestMarkDispatchedShape(t *testing.T) {
	d := sqltest.MustOpen(t)
	id := seedRow(t, d, "PENDING")
	if err := MarkDispatched(context.Background(), d, id); err != nil {
		t.Fatalf("MarkDispatched: %v", err)
	}
	var (
		status       string
		attempts     int
		dispatchedAt sql.NullTime
	)
	if err := d.QueryRowContext(context.Background(),
		`SELECT status, deliver_attempts, dispatched_at FROM outbox WHERE id=?`, id).
		Scan(&status, &attempts, &dispatchedAt); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "DISPATCHED" || attempts != 1 || !dispatchedAt.Valid {
		t.Fatalf("shape: %s/%d/dispatched=%v", status, attempts, dispatchedAt.Valid)
	}
}

// TestMarkZeroRowsIsLoud:0 行(行已非 PENDING,谓词不命中)必须响亮报错,
// 不得静默 nil——否则无锁误用/竞态会以「成功」伪装。
func TestMarkZeroRowsIsLoud(t *testing.T) {
	d := sqltest.MustOpen(t)
	id := seedRow(t, d, "DEAD")
	if err := MarkDispatched(context.Background(), d, id); err == nil {
		t.Fatalf("MarkDispatched on DEAD row returned nil, want loud error")
	}
	if err := MarkDead(context.Background(), d, id, "x"); err == nil {
		t.Fatalf("MarkDead on DEAD row returned nil, want loud error")
	}
	if err := RecordFailure(context.Background(), d, id, "x", time.Second); err == nil {
		t.Fatalf("RecordFailure on DEAD row returned nil, want loud error")
	}
	var status string
	var attempts int
	if err := d.QueryRowContext(context.Background(),
		`SELECT status, deliver_attempts FROM outbox WHERE id=?`, id).Scan(&status, &attempts); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "DEAD" || attempts != 0 {
		t.Fatalf("failed mark mutated row: %s/%d", status, attempts)
	}
}

// TestRecordFailureShape:失败记账的列形状(attempts+1、last_error 截断、
// available_at 后推到未来)。
func TestRecordFailureShape(t *testing.T) {
	d := sqltest.MustOpen(t)
	id := seedRow(t, d, "PENDING")
	longMsg := strings.Repeat("x", 3000)
	if err := RecordFailure(context.Background(), d, id, longMsg, time.Hour); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	var (
		status      string
		attempts    int
		lastErr     sql.NullString
		availFuture bool
	)
	err := d.QueryRowContext(context.Background(), `
SELECT status, deliver_attempts, last_error, available_at > NOW(3)
  FROM outbox WHERE id=?`, id).Scan(&status, &attempts, &lastErr, &availFuture)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "PENDING" || attempts != 1 || !lastErr.Valid || len(lastErr.String) > maxErrText {
		t.Fatalf("shape: %s/%d/len(lastErr)=%d", status, attempts, len(lastErr.String))
	}
	if !availFuture {
		t.Fatalf("available_at not pushed to future")
	}
}
