package scheduler

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
)

// 在真实结果集关闭后暂停终态判定，再让生产回滚路径竞争同一执行。
// 旧实现无锁读取后回滚可先提交，随后会用旧输出把新轮收成 SUCCEEDED。
func TestFinishIfDoneSerializesRollback(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, chainDSL, `{"q":"hi"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.Start(ctx, execID, "w1"); err != nil {
		t.Fatal(err)
	}
	for _, node := range []string{"a", "b", "c"} {
		if err := completeNode(t, s, d, execID, node, engine.AttemptSucceed,
			json.RawMessage(`{"text":"old-round"}`)); err != nil {
			t.Fatal(err)
		}
	}
	paused, release := make(chan struct{}), make(chan struct{})
	var pauseOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	wrapped := sqltest.OpenIntercepted(t, d, sqltest.Intercepts{
		AfterRows: func(ctx context.Context, query string, _ []driver.NamedValue) error {
			if !strings.Contains(query, "FROM node_execution") || !strings.Contains(query, "execution_id=?") {
				return nil
			}
			var err error
			pauseOnce.Do(func() {
				close(paused)
				select {
				case <-release:
				case <-ctx.Done():
					err = ctx.Err()
				}
			})
			return err
		},
	})
	finisher, err := New(Deps{DB: wrapped, IDs: mustTestIDs(t), Queue: "sched-test"})
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- finisher.FinishIfDone(ctx, execID) }()
	select {
	case <-paused:
	case <-ctx.Done():
		t.Fatal("finish read barrier: ", ctx.Err())
	}
	rolledBack := make(chan error, 1)
	go func() {
		_, err := s.RollbackTo(ctx, RollbackParams{
			ExecID: execID, TargetNodeID: "a", WorkerID: "api-race", ActorID: "operator",
		})
		rolledBack <- err
	}()
	// 不用固定 sleep 推测交错：回滚要么已提交(旧实现)，要么在 InnoDB
	// 锁等待表中明确等待终态判定的 node 锁(修复后)。
	rollbackDone := false
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
wait:
	for {
		select {
		case err := <-rolledBack:
			if err != nil {
				t.Fatal("rollback: ", err)
			}
			rollbackDone = true
			break wait
		case <-tick.C:
			var waits int
			if err := d.QueryRowContext(ctx, `SELECT COUNT(*)
FROM performance_schema.data_lock_waits w
JOIN performance_schema.data_locks l ON l.ENGINE_LOCK_ID=w.REQUESTING_ENGINE_LOCK_ID
WHERE l.OBJECT_SCHEMA=DATABASE() AND l.OBJECT_NAME='node_execution'`).Scan(&waits); err != nil {
				t.Fatal("observe native lock wait: ", err)
			}
			if waits > 0 {
				break wait
			}
		case <-ctx.Done():
			t.Fatal("rollback did not commit or wait on node lock: ", ctx.Err())
		}
	}
	unblock()
	if err := <-finished; err != nil {
		t.Fatal("finish: ", err)
	}
	if !rollbackDone {
		if err := <-rolledBack; err != nil {
			t.Fatal("rollback: ", err)
		}
	}
	var status engine.WorkflowState
	var hasOldOutput bool
	if err := d.QueryRowContext(ctx, `SELECT status, outputs IS NOT NULL FROM workflow_execution WHERE id=?`, execID).
		Scan(&status, &hasOldOutput); err != nil {
		t.Fatal(err)
	}
	if status != engine.WorkflowRunning || hasOldOutput {
		t.Fatalf("new round overwritten: status=%s old_output=%t (rollback committed during finish read=%t)", status, hasOldOutput, rollbackDone)
	}
	if got := readNode(t, d, execID, "a").status; got != engine.NodeExecuting {
		t.Fatalf("rerun node status=%s, want EXECUTING", got)
	}
}
