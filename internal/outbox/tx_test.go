package outbox

// T2.2 验收①「判别性故障测试」:状态写入后、outbox 写入前注入失败(断连/panic),
// 断言两写同事务回滚。本文件是 dbtx.go 注释所预留形状的首个直接证据。
//
// 变异自查(实现错 → 哪条红):
//   M1 Append 用独立连接/独立事务 → panic/断连变体的「outbox 0 行」红(node
//      回滚了,事件行独存);
//   M2 ClaimNode 移出 WithTx(autocommit)→ 两变体的「node 仍 PENDING」红;
//   M3 WithTx 丢 defer-Rollback / 吞错误仍 Commit → panic 变体咬 defer 路径、
//      断连变体咬错误路径(双保险);
//   M4 两段式(先提交状态再写事件)→ 断连变体的 node PENDING 红(EXECUTING
//      已落库不可逆);
//   M6 组合顺序(先事件后状态)在 fn 内由调用方排定,T2.2 无生产组合 helper,
//      记录为测试边界不虚称覆盖。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
)

// ofixture:验收①需要 def→exec→node 三层父行(ClaimNode 的对象)+ 事件。
// 清理保持叶先根后(outbox 无 FK,放最先是惯例;禁 TRUNCATE)。
type ofixture struct {
	t      *testing.T
	db     *sql.DB
	execID uint64
	nodeID uint64
	defID  uint64
}

func newOFixture(t *testing.T) *ofixture {
	t.Helper()
	d := sqltest.MustOpen(t)
	f := &ofixture{t: t, db: d, execID: newID(), nodeID: newID(), defID: newID()}
	name := fmt.Sprintf("wf-%d", f.defID)
	mustExec(t, d, `INSERT INTO workflow_definition
		(id, name, dsl_version, status, description, dsl, dsl_hash, created_by, updated_by)
		VALUES (?,?, '1.0', 'DRAFT', '', '{}', '', 'test', 'test')`, f.defID, name)
	mustExec(t, d, `INSERT INTO workflow_execution
		(id, definition_id, definition_name, definition_version, definition_hash, status, triggered_by, input)
		VALUES (?,?,?, 1, '', 'PENDING', 'test', '{}')`, f.execID, f.defID, name)
	mustExec(t, d, `INSERT INTO node_execution
		(id, execution_id, node_id, type, status, max_attempts, version)
		VALUES (?,?, 'n1', 'llm', 'PENDING', 1, 1)`, f.nodeID, f.execID)
	t.Cleanup(func() {
		mustExec(t, d, `DELETE FROM outbox WHERE execution_id=?`, f.execID)
		mustExec(t, d, `DELETE FROM node_attempt WHERE execution_id=?`, f.execID)
		mustExec(t, d, `DELETE FROM node_execution WHERE execution_id=?`, f.execID)
		mustExec(t, d, `DELETE FROM workflow_execution WHERE id=?`, f.execID)
		mustExec(t, d, `DELETE FROM workflow_definition WHERE id=?`, f.defID)
	})
	return f
}

func (f *ofixture) event() Event {
	return Event{
		ID:          newID(),
		ExecutionID: f.execID,
		EventType:   "node.task_enqueue",
		TaskID:      fmt.Sprintf("att:%d:1", f.nodeID),
		Payload:     json.RawMessage(`{"node_execution_id":` + fmt.Sprint(f.nodeID) + `}`),
	}
}

func (f *ofixture) claimInput() engine.ClaimInput {
	return engine.ClaimInput{
		WorkerID:      "worker-test",
		DeadlineAt:    deadlineFuture(f.t),
		InputSnapshot: json.RawMessage(`{"prompt":"hi"}`),
	}
}

// nodeState 回读断言面。
type nodeState struct {
	Status   string
	Version  uint32
	Attempts uint16
	Snapshot sql.NullString
}

func scanNode(t *testing.T, d *sql.DB, id uint64) nodeState {
	t.Helper()
	var s nodeState
	err := d.QueryRowContext(context.Background(),
		`SELECT status, version, attempts, input_snapshot FROM node_execution WHERE id=?`, id).
		Scan(&s.Status, &s.Version, &s.Attempts, &s.Snapshot)
	if err != nil {
		t.Fatalf("scan node %d: %v", id, err)
	}
	return s
}

func outboxCount(t *testing.T, d *sql.DB, execID uint64) int {
	t.Helper()
	var n int
	if err := d.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM outbox WHERE execution_id=?`, execID).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return n
}

// assertRolledBack:双写同亡的断言体(node 未被认领 + 无事件行)。
func assertRolledBack(t *testing.T, f *ofixture, variant string) {
	t.Helper()
	got := scanNode(t, f.db, f.nodeID)
	if got.Status != "PENDING" || got.Version != 1 || got.Attempts != 0 || got.Snapshot.Valid {
		t.Fatalf("%s: node not rolled back: %+v", variant, got)
	}
	if n := outboxCount(t, f.db, f.execID); n != 0 {
		t.Fatalf("%s: outbox has %d rows, want 0", variant, n)
	}
}

// TestSameTxPositiveControl:正例对照——无注入时双写原子落库。没有它,回滚断言
// 是空洞的(「Append 根本没插行」也能全绿)。
func TestSameTxPositiveControl(t *testing.T) {
	f := newOFixture(t)
	evt := f.event()
	err := engine.WithTx(context.Background(), f.db, func(q engine.DBTX) error {
		if _, err := engine.ClaimNode(context.Background(), q, f.nodeID, 1, f.claimInput()); err != nil {
			return err
		}
		return Append(context.Background(), q, evt)
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	got := scanNode(t, f.db, f.nodeID)
	if got.Status != "EXECUTING" || got.Version != 2 || got.Attempts != 1 || !got.Snapshot.Valid {
		t.Fatalf("node not claimed: %+v", got)
	}
	if n := outboxCount(t, f.db, f.execID); n != 1 {
		t.Fatalf("outbox rows = %d, want 1", n)
	}
	var status string
	var attempts int
	if err := f.db.QueryRowContext(context.Background(),
		`SELECT status, deliver_attempts FROM outbox WHERE id=?`, evt.ID).Scan(&status, &attempts); err != nil {
		t.Fatalf("scan outbox: %v", err)
	}
	if status != "PENDING" || attempts != 0 {
		t.Fatalf("outbox shape: status=%s attempts=%d", status, attempts)
	}
}

// TestSameTxPanicRollsBackBoth:状态写入后、outbox 写入前 panic → 双写同亡。
// 咬 M1/M2/M3(panic 路径)。
func TestSameTxPanicRollsBackBoth(t *testing.T) {
	f := newOFixture(t)
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatalf("expected panic to propagate out of WithTx fn")
			}
		}()
		_ = engine.WithTx(context.Background(), f.db, func(q engine.DBTX) error {
			if _, err := engine.ClaimNode(context.Background(), q, f.nodeID, 1, f.claimInput()); err != nil {
				return err
			}
			panic("inject: post-claim pre-append")
		})
	}()
	assertRolledBack(t, f, "panic")
}

// TestSameTxKilledConnectionRollsBackBoth:状态写入后、outbox 写入前 KILL 事务
// 连接 → 双写同亡。确定性时序:CONNECTION_ID 经事务自身连接读取(*sql.Tx 钉死
// 单条物理连接),KILL 与 Append 都在 fn 体内串行表达,零竞态窗口。
// 咬 M1/M2/M3(错误路径)/M4。
func TestSameTxKilledConnectionRollsBackBoth(t *testing.T) {
	f := newOFixture(t)
	admin, err := f.db.Conn(context.Background())
	if err != nil {
		t.Fatalf("admin conn: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	txErr := engine.WithTx(context.Background(), f.db, func(q engine.DBTX) error {
		if _, err := engine.ClaimNode(context.Background(), q, f.nodeID, 1, f.claimInput()); err != nil {
			return err
		}
		var cid int64
		if err := q.QueryRowContext(context.Background(), `SELECT CONNECTION_ID()`).Scan(&cid); err != nil {
			return fmt.Errorf("read connection id: %w", err)
		}
		// KILL 需 CONNECTION_ADMIN;sqltest DSN 是 root(前提已在注释标注)。
		if _, err := admin.ExecContext(context.Background(), fmt.Sprintf("KILL %d", cid)); err != nil {
			return fmt.Errorf("kill %d: %w", cid, err)
		}
		return Append(context.Background(), q, f.event()) // 必须失败:连接已被服务端断开
	})
	if txErr == nil {
		t.Fatalf("Append on killed connection unexpectedly succeeded")
	}
	if errors.Is(txErr, ErrInvalidEvent) {
		t.Fatalf("failure came from validation, not the killed connection: %v", txErr)
	}
	assertRolledBack(t, f, "killed-connection")
}

// deadlineFuture:给 ClaimInput 一个显然未来的 deadline(零值会落 NULL,让
// 「快照列被清」类断言失去前置状态——T2.1 变异验证教训)。
func deadlineFuture(t *testing.T) time.Time {
	t.Helper()
	return time.Now().Add(time.Hour)
}
