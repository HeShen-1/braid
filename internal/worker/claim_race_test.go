package worker

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/redistest"
	"braid/internal/scheduler"
)

func seedClaimRace(t *testing.T) (*sql.DB, uint64, uint64) {
	t.Helper()
	d := sqltest.MustOpenAt(t, workerSchema)
	execID, nodeID := seedNodeExec(t, d)
	var defID uint64
	if err := d.QueryRow(`SELECT definition_id FROM workflow_execution WHERE id=?`, execID).Scan(&defID); err != nil {
		t.Fatal(err)
	}
	// 取消/回滚会留下未投递 outbox；本组用例必须自清理，不能改变后续
	// relay 全局扫描的锁范围。worker 的停止清理在本清理之前执行。
	t.Cleanup(func() { cleanupE2E(t, d, execID, defID) })
	return d, execID, nodeID
}

// 接管的资格读失败不能沿“心跳刷新尽力”分支放行执行。
func TestClaimAttemptTakeoverGuardReadFailsClosed(t *testing.T) {
	d, execID, nodeID := seedClaimRace(t)
	ctx := context.Background()
	payload := nodeTaskPayload{ExecutionID: execID, NodeExecutionID: nodeID, AttemptNo: 1}
	taskID := fmt.Sprintf("att:%d:1", nodeID)
	h := &attHandler{deps: Deps{DB: d, IDs: mustTestIDs(t)}, cfg: DefaultConfig(), log: slog.Default()}
	if id, err := h.claimAttempt(ctx, taskID, payload); err != nil || id == 0 {
		t.Fatalf("seed active attempt: id=%d err=%v", id, err)
	}
	injected := errors.New("injected claim guard read failure")
	var reads atomic.Uint32
	wrapped := sqltest.OpenIntercepted(t, d, sqltest.Intercepts{
		AfterRows: func(_ context.Context, query string, _ []driver.NamedValue) error {
			if strings.Contains(query, "SELECT input_snapshot") && reads.Add(1) == 2 {
				return injected
			}
			return nil
		},
	})
	h.deps.DB = wrapped
	if id, err := h.claimAttempt(ctx, taskID, payload); id != 0 || !errors.Is(err, injected) {
		t.Fatalf("guard read failure admitted takeover: id=%d err=%v", id, err)
	}
}

// 相反交错：认领已经读到快照，取消必须等该认领提交，不能穿过读/写窗口。
// 直接去掉 FOR UPDATE 时本例会失败，防止“只移进事务但仍是快照读”的半修。
func TestClaimSnapshotSerializesCancel(t *testing.T) {
	d, execID, nodeID := seedClaimRace(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := engine.StartExecution(ctx, d, execID); err != nil {
		t.Fatal(err)
	}
	paused, release := make(chan struct{}), make(chan struct{})
	var pauseOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	wrapped := sqltest.OpenIntercepted(t, d, sqltest.Intercepts{
		AfterRows: func(ctx context.Context, query string, _ []driver.NamedValue) error {
			if !strings.Contains(query, "SELECT input_snapshot") {
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
	h := &attHandler{deps: Deps{DB: wrapped, IDs: mustTestIDs(t)}, cfg: DefaultConfig(), log: slog.Default()}
	type result struct {
		id  uint64
		err error
	}
	claimed := make(chan result, 1)
	go func() {
		id, err := h.claimAttempt(ctx, fmt.Sprintf("att:%d:1", nodeID),
			nodeTaskPayload{ExecutionID: execID, NodeExecutionID: nodeID, AttemptNo: 1})
		claimed <- result{id, err}
	}()
	select {
	case <-paused:
	case <-ctx.Done():
		t.Fatal("snapshot barrier: ", ctx.Err())
	}
	sched, err := scheduler.New(scheduler.Deps{DB: d, IDs: mustTestIDs(t)})
	if err != nil {
		t.Fatal(err)
	}
	cancelled := make(chan error, 1)
	go func() {
		_, err := sched.CancelExecution(ctx, scheduler.CancelParams{
			ExecID: execID, WorkerID: "api-race", ActorID: "operator",
		})
		cancelled <- err
	}()
	cancelWon := false
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
wait:
	for {
		select {
		case err := <-cancelled:
			if err != nil {
				t.Fatal(err)
			}
			cancelWon = true
			break wait
		case <-tick.C:
			var waits int
			if err := d.QueryRowContext(ctx, `SELECT COUNT(*)
FROM performance_schema.data_lock_waits w
JOIN performance_schema.data_locks l ON l.ENGINE_LOCK_ID=w.REQUESTING_ENGINE_LOCK_ID
WHERE l.OBJECT_SCHEMA=DATABASE() AND l.OBJECT_NAME='node_execution'`).Scan(&waits); err != nil {
				t.Fatal(err)
			}
			if waits > 0 {
				break wait
			}
		case <-ctx.Done():
			t.Fatal("cancel did not commit or wait on node lock: ", ctx.Err())
		}
	}
	unblock()
	r := <-claimed
	if r.err != nil || r.id == 0 {
		t.Fatalf("snapshot-first claim: id=%d err=%v", r.id, r.err)
	}
	if !cancelWon {
		if err := <-cancelled; err != nil {
			t.Fatal(err)
		}
	}
	if cancelWon {
		t.Fatal("cancel committed between claim snapshot and attempt insertion")
	}
	if got := readAttempt(t, d, nodeID, 1).Status; got != engine.AttemptDead {
		t.Fatalf("cancel did not close committed attempt: %s", got)
	}
}

// 暂停认领事务开始，让真实取消/回滚先提交。旧实现此时已读了事务外
// 快照，会错误物化旧 attempt 并调用 executor；修复后锁定读看见最新轮次。
func TestClaimAttemptMutationBeforeTransaction(t *testing.T) {
	for _, takeover := range []bool{false, true} {
		for _, rollback := range []bool{false, true} {
			t.Run(fmt.Sprintf("takeover=%t/rollback=%t", takeover, rollback), func(t *testing.T) {
				d, execID, nodeID := seedClaimRace(t)
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				if err := engine.StartExecution(ctx, d, execID); err != nil {
					t.Fatal(err)
				}
				if _, err := d.Exec(`UPDATE node_execution SET input_snapshot='{"template":"seed","user_data":[]}' WHERE id=?`, nodeID); err != nil {
					t.Fatal(err)
				}
				payload := nodeTaskPayload{ExecutionID: execID, NodeExecutionID: nodeID, AttemptNo: 1}
				taskID := fmt.Sprintf("att:%d:1", nodeID)
				if takeover {
					h := &attHandler{deps: Deps{DB: d, IDs: mustTestIDs(t)}, cfg: DefaultConfig(), log: slog.Default()}
					if id, err := h.claimAttempt(ctx, taskID, payload); err != nil || id == 0 {
						t.Fatalf("seed active attempt: id=%d err=%v", id, err)
					}
				}
				paused, release := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				defer unblock()
				var begins atomic.Uint32
				pauseAt := uint32(1)
				if takeover {
					pauseAt = 2 // 重复 INSERT 回滚之后的接管事务。
				}
				wrapped := sqltest.OpenIntercepted(t, d, sqltest.Intercepts{
					BeforeBegin: func(ctx context.Context) error {
						if begins.Add(1) != pauseAt {
							return nil
						}
						close(paused)
						select {
						case <-release:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					},
				})
				ex := &fakeExec{fn: func(Request, int) (Outcome, error) {
					return Outcome{Status: engine.AttemptSucceed}, nil
				}}
				queue := startWorker(t, wrapped, ex, DefaultConfig())
				client := asynq.NewClient(redistest.MustOpt(t))
				t.Cleanup(func() { _ = client.Close() })
				if _, err := enqueue(t, client, queue, taskID, payload); err != nil {
					t.Fatal(err)
				}
				select {
				case <-paused:
				case <-ctx.Done():
					t.Fatal("claim begin barrier: ", ctx.Err())
				}
				sched, err := scheduler.New(scheduler.Deps{DB: d, IDs: mustTestIDs(t), Queue: queue})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := sched.CancelExecution(ctx, scheduler.CancelParams{
					ExecID: execID, WorkerID: "api-race", ActorID: "operator",
				}); err != nil {
					t.Fatal("cancel while claim paused: ", err)
				}
				if rollback {
					if _, err := sched.RollbackTo(ctx, scheduler.RollbackParams{
						ExecID: execID, TargetNodeID: "seed_node", WorkerID: "api-race", ActorID: "operator",
					}); err != nil {
						t.Fatal("rollback while claim paused: ", err)
					}
				}
				unblock()
				insp := asynq.NewInspector(redistest.MustOpt(t))
				t.Cleanup(func() { _ = insp.Close() })
				waitTaskGone(t, insp, queue, taskID)
				if calls := ex.count(); calls != 0 {
					t.Errorf("stale executor calls=%d, want 0", calls)
				}
				var rows int
				if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM node_attempt WHERE node_execution_id=? AND attempt_no=1`, nodeID).Scan(&rows); err != nil {
					t.Fatal(err)
				}
				want := 0
				if takeover {
					want = 1 // 只保留取消前已存在且被关闭的证据行。
				}
				if rows != want {
					t.Errorf("old attempt rows=%d, want %d", rows, want)
				}
			})
		}
	}
}
