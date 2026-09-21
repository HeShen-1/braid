package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// 验收 1:两 worker 抢同一节点仅一个成功。四个测试构成不可伪造的证据链:
//   - 确定性双连接(A 持锁 B 阻塞,A 提交/回滚两种裁决)
//   - 阻塞证明(B 在 A 持锁期间确实不返回 = 真竞争,非顺序侥幸)
//   - N 路真竞争(barrier 同时发起,恰 1 胜)
//   - 乐观锁隔离(同 status 同 version 双提交恰 1 胜,单独证明 version 列生效)

// conn 钉一条独占物理连接(杜绝对池复用伪装竞争),会话级缩短锁等待防 CI 挂死。
func conn(t *testing.T, f *fixture) *sql.Conn {
	t.Helper()
	c, err := f.db.Conn(f.ctx())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	if _, err := c.ExecContext(f.ctx(), "SET innodb_lock_wait_timeout=5"); err != nil {
		t.Fatalf("set lock wait: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestClaimTwoConnectionsDeterministic(t *testing.T) {
	for _, tc := range []struct {
		name      string
		commitA   bool
		wantBWins bool
	}{
		{"A 提交则 B 败", true, false},
		{"A 回滚则 B 胜", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			id := f.node()
			cA, cB := conn(t, f), conn(t, f)

			txA, err := cA.BeginTx(f.ctx(), nil)
			if err != nil {
				t.Fatalf("begin A: %v", err)
			}
			if _, err := ClaimNode(f.ctx(), txA, id, 1, ClaimInput{
				WorkerID: "wA", InputSnapshot: json.RawMessage(`{"winner":"A"}`),
			}); err != nil {
				t.Fatalf("A claim: %v", err)
			}

			done := make(chan error, 1)
			go func() {
				txB, err := cB.BeginTx(f.ctx(), nil)
				if err != nil {
					done <- err
					return
				}
				_, err = ClaimNode(f.ctx(), txB, id, 1, ClaimInput{
					WorkerID: "wB", InputSnapshot: json.RawMessage(`{"winner":"B"}`),
				})
				if err != nil {
					_ = txB.Rollback()
					done <- err
					return
				}
				done <- txB.Commit()
			}()

			// 阻塞证明:B 的 UPDATE 在 A 持锁期间不返回(同一行撞锁的证据)
			select {
			case err := <-done:
				t.Fatalf("B 未阻塞即返回(假并发): %v", err)
			case <-time.After(300 * time.Millisecond):
			}

			if tc.commitA {
				if err := txA.Commit(); err != nil {
					t.Fatalf("A commit: %v", err)
				}
			} else {
				if err := txA.Rollback(); err != nil {
					t.Fatalf("A rollback: %v", err)
				}
			}

			err = <-done
			r := scanNode(t, f.db, id)
			if tc.wantBWins {
				if err != nil {
					t.Fatalf("B 应胜,got %v", err)
				}
				if r.Status != NodeExecuting || r.WorkerID.String != "wB" || r.Version != 2 || r.Attempts != 1 {
					t.Fatalf("B 胜后行=%+v", r)
				}
				jsonEq(t, r.Snapshot, json.RawMessage(`{"winner":"B"}`))
			} else {
				var ce *ConflictError
				if !errors.As(err, &ce) || !errors.Is(err, ErrConflict) {
					t.Fatalf("B 应败于 Conflict,got %v", err)
				}
				if ce.Observed != NodeExecuting || ce.ObservedVer != 2 {
					t.Fatalf("B 观测=%+v, want EXECUTING/v2(A 的已提交效果)", ce)
				}
				if r.WorkerID.String != "wA" || r.Version != 2 {
					t.Fatalf("行=%+v, A 的效果不应被 B 篡改", r)
				}
				jsonEq(t, r.Snapshot, json.RawMessage(`{"winner":"A"}`))
			}
		})
	}
}

func TestClaimNRaceExactlyOneWinner(t *testing.T) {
	for _, n := range []int{2, 4, 8} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			f := newFixture(t)
			id := f.node()
			start := make(chan struct{})
			type result struct {
				worker string
				snap   json.RawMessage
				err    error
			}
			results := make([]result, n)
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					in := ClaimInput{
						WorkerID:      fmt.Sprintf("w%d", i),
						InputSnapshot: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)),
					}
					<-start
					_, err := ClaimNode(f.ctx(), f.db, id, 1, in)
					results[i] = result{worker: in.WorkerID, snap: in.InputSnapshot, err: err}
				}(i)
			}
			close(start) // barrier:全部就绪后同时发起
			wg.Wait()

			wins := 0
			var winner result
			for _, r := range results {
				if r.err == nil {
					wins++
					winner = r
					continue
				}
				var ce *ConflictError
				if !errors.As(r.err, &ce) || !errors.Is(r.err, ErrConflict) {
					t.Fatalf("败者应得 Conflict,got %v", r.err)
				}
			}
			if wins != 1 {
				t.Fatalf("胜者数=%d, want 1", wins)
			}
			r := scanNode(t, f.db, id)
			if r.Status != NodeExecuting || r.Version != 2 || r.Attempts != 1 || r.WorkerID.String != winner.worker {
				t.Fatalf("行=%+v, winner=%s", r, winner.worker)
			}
			jsonEq(t, r.Snapshot, winner.snap)
			if r.Attempts != 1 {
				t.Fatalf("attempts=%d, want 1(败者 0 行不可能推进任何列)", r.Attempts)
			}
		})
	}
}

// 同 status 同 version 的双提交:证明 version 列本身在仲裁(上面的测试里 status
// 恰好也变了,尚不能单独证明 version 生效)。
func TestOptimisticLockIsolation(t *testing.T) {
	f := newFixture(t)
	id := f.node()
	claimOK(t, f, id, "w1")
	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := SucceedNode(f.ctx(), f.db, id, 2, "w1", json.RawMessage(fmt.Sprintf(`{"who":%d}`, i)))
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("恰一胜失败: %v / %v", errs[0], errs[1])
	}
	r := scanNode(t, f.db, id)
	if r.Status != NodeSucceeded || r.Version != 3 {
		t.Fatalf("行=%+v, want SUCCEEDED/v3", r)
	}
}
