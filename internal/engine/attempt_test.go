package engine

import (
	"errors"
	"fmt"
	"testing"
)

func TestInsertAttemptDuplicateRejected(t *testing.T) {
	f := newFixture(t)
	nodeID := f.node()
	p := func(no uint16, id uint64) AttemptParams {
		return AttemptParams{
			ID: id, NodeExecID: nodeID, ExecID: f.execID, AttemptNo: no,
			WorkerID:       "w1",
			IdempotencyKey: fmt.Sprintf("att:%d:%d", nodeID, no),
			AsynqTaskID:    fmt.Sprintf("att:%d:%d", nodeID, no),
		}
	}
	if err := InsertAttempt(f.ctx(), f.db, p(1, newID())); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// 双投递第二发:同 (node_execution_id, attempt_no) → ErrConflict(T2.3 幂等锚)
	if err := InsertAttempt(f.ctx(), f.db, p(1, newID())); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate attempt_no got %v, want ErrConflict", err)
	}
	// 幂等键全局唯一:不同 attempt_no 撞同 idempotency_key 也拒
	if err := InsertAttempt(f.ctx(), f.db, p(2, newID())); err != nil {
		t.Fatalf("insert no=2: %v", err)
	}
	dupKey := p(3, newID())
	dupKey.IdempotencyKey = fmt.Sprintf("att:%d:1", nodeID)
	dupKey.AsynqTaskID = fmt.Sprintf("att:%d:1", nodeID)
	if err := InsertAttempt(f.ctx(), f.db, dupKey); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate idempotency_key got %v, want ErrConflict", err)
	}
}

func TestCloseAttempt(t *testing.T) {
	f := newFixture(t)
	nodeID := f.node()
	attID := newID()
	if err := InsertAttempt(f.ctx(), f.db, AttemptParams{
		ID: attID, NodeExecID: nodeID, ExecID: f.execID, AttemptNo: 1,
		WorkerID: "w1", IdempotencyKey: "k1", AsynqTaskID: "t1",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := CloseAttempt(f.ctx(), f.db, attID, AttemptDead, "", ""); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("DEAD 收口应拒绝(归 T2.5),got %v", err)
	}
	if err := CloseAttempt(f.ctx(), f.db, attID, AttemptFailed, "429", "rate limited"); err != nil {
		t.Fatalf("close: %v", err)
	}
	// 终态后禁改:二次收口 0 行 → Conflict(行无 updated_at,收口是一次终写)
	if err := CloseAttempt(f.ctx(), f.db, attID, AttemptSucceed, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("二次收口 got %v, want ErrConflict", err)
	}
	var status string
	if err := f.db.QueryRowContext(f.ctx(),
		`SELECT status FROM node_attempt WHERE id=?`, attID).Scan(&status); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != string(AttemptFailed) {
		t.Fatalf("status=%s, want FAILED", status)
	}
}
