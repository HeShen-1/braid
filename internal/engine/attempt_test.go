package engine

import (
	"database/sql"
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

// CloseAttemptDead(T2.5 reaper 判死收口):RUNNING→DEAD 行效应(finished_at 恒
// NULL、worker_id 首任不动、error_code 落判死码)与谓词仲裁(仅 RUNNING 命中)。
//
// 变异自查(实现错 → 哪条红):
//   - 写了 finished_at → running_to_dead 红(DDL:DEAD 未观察到终态);
//   - 谓词丢了 status → already_final 红;
//   - 改写 worker_id → running_to_dead 红(跨接管归因指首任)。
func TestCloseAttemptDead(t *testing.T) {
	deadRow := func(t *testing.T) (*fixture, uint64) {
		t.Helper()
		f := newFixture(t)
		nodeID := f.node()
		attID := newID()
		if err := InsertAttempt(f.ctx(), f.db, AttemptParams{
			ID: attID, NodeExecID: nodeID, ExecID: f.execID, AttemptNo: 1,
			WorkerID: "w1", IdempotencyKey: "k1", AsynqTaskID: "t1",
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
		return f, attID
	}
	t.Run("running_to_dead", func(t *testing.T) {
		f, attID := deadRow(t)
		if err := CloseAttemptDead(f.ctx(), f.db, attID, "heartbeat_lost", "stale 46s worker=w1"); err != nil {
			t.Fatalf("close dead: %v", err)
		}
		var (
			status    string
			finished  sql.NullTime
			workerID  sql.NullString
			errCode   sql.NullString
			errMsg    sql.NullString
		)
		if err := f.db.QueryRowContext(f.ctx(),
			`SELECT status, finished_at, worker_id, error_code, error_message FROM node_attempt WHERE id=?`, attID).
			Scan(&status, &finished, &workerID, &errCode, &errMsg); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if status != string(AttemptDead) {
			t.Fatalf("status=%s, want DEAD", status)
		}
		if finished.Valid {
			t.Fatalf("finished_at=%v 被写入(DDL:DEAD 恒 NULL,未观察到终态)", finished)
		}
		if workerID.String != "w1" {
			t.Fatalf("worker_id=%q 被改写(跨接管归因指首任)", workerID.String)
		}
		if errCode.String != "heartbeat_lost" || errMsg.String != "stale 46s worker=w1" {
			t.Fatalf("code/msg=%q/%q, want heartbeat_lost/摘要含首任", errCode.String, errMsg.String)
		}
	})
	t.Run("already_final", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			want string
			prep func(t *testing.T, f *fixture, attID uint64)
		}{
			{"observed_succeeded", "SUCCEEDED", func(t *testing.T, f *fixture, attID uint64) {
				if err := CloseAttempt(f.ctx(), f.db, attID, AttemptSucceed, "", ""); err != nil {
					t.Fatalf("pre succeed: %v", err)
				}
			}},
			{"observed_dead", "DEAD", func(t *testing.T, f *fixture, attID uint64) {
				if err := CloseAttemptDead(f.ctx(), f.db, attID, "heartbeat_lost", ""); err != nil {
					t.Fatalf("pre dead: %v", err)
				}
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f, attID := deadRow(t)
				tc.prep(t, f, attID)
				err := CloseAttemptDead(f.ctx(), f.db, attID, "heartbeat_lost", "")
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("err=%v, want ErrConflict(终态后禁改)", err)
				}
				var ce *ConflictError
				if !errors.As(err, &ce) {
					t.Fatalf("err=%v, want *ConflictError", err)
				}
				if string(ce.Observed) != tc.want {
					t.Fatalf("Observed=%s, want %s", ce.Observed, tc.want)
				}
			})
		}
	})
	t.Run("not_found", func(t *testing.T) {
		f := newFixture(t)
		if err := CloseAttemptDead(f.ctx(), f.db, newID(), "", ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err=%v, want ErrNotFound", err)
		}
	})
}
