package worker

// T2.2 审计移交③实证:relay 投递事务的锁窗(SELECT ... FOR UPDATE SKIP
// LOCKED,窗 = 一次跨 Redis RTT)与引擎派发事务的 outbox.Append INSERT 是否
// 互卡(gap/next-key 锁交互)。
//
// 形状:连接 A 逐字复刻 relay 扫描语句并人为放大锁窗(500ms,远超真实
// RTT);goroutine B 并发跑派发事务形状(InsertAttempt + outbox.Append,
// engine.WithTx)。B 在 A 窗内完成 = 不阻塞;B 耗时 ≥ 窗 = 阻塞(实证发现,
// 需决策调整,如 relay 连接降 READ COMMITTED 消 gap 锁)。
//
// 判定用「B 耗时 < A 锁窗」而非「B 恰在窗内完成」:留调度裕量,只对真阻塞
// 判红。实测耗时随断言输出,入 PR 描述作实证记录。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/outbox"
)

func TestRelayLockWindowDoesNotBlockDispatchInsert(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	execID, nodeExecID := seedNodeExec(t, d)
	ids := mustTestIDs(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 种 2 行 eligible PENDING(雪花 id 递增,同事务时刻即同 available_at 段):
	// A 锁第一行,第二行之后是 B 插入点的竞争地带。
	for i := 0; i < 2; i++ {
		id, err := ids.Next()
		if err != nil {
			t.Fatalf("gen seed outbox id: %v", err)
		}
		if err := outbox.Append(ctx, d, outbox.Event{
			ID:          id,
			ExecutionID: execID,
			EventType:   EventTypeNodeTask,
			TaskID:      fmt.Sprintf("att:%d:9%d", nodeExecID, i),
			Payload:     []byte(`{}`),
		}); err != nil {
			t.Fatalf("seed outbox row: %v", err)
		}
	}

	// 连接 A:独立连接上开 relay 形状的扫描事务(d.Conn 拿出池外连接,
	// 保证与 B 的池连接不同)。
	connA, err := d.Conn(ctx)
	if err != nil {
		t.Fatalf("conn A: %v", err)
	}
	defer connA.Close() //nolint:errcheck // 测试收尾
	txA, err := connA.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	var lockedID uint64
	err = txA.QueryRowContext(ctx, `
SELECT id FROM outbox
 WHERE status='PENDING' AND available_at<=NOW(3)
 ORDER BY id
 LIMIT 1
 FOR UPDATE SKIP LOCKED`).Scan(&lockedID)
	if err != nil {
		t.Fatalf("relay-shaped scan: %v", err)
	}

	// B:并发派发事务(attempt 行 + 新 outbox 事件,id 均为新生雪花,落点在
	// 已锁区间之后 = 真实并发派发面)。
	const window = 500 * time.Millisecond // 人为放大的 relay Redis RTT 窗
	type result struct {
		elapsed time.Duration
		err     error
	}
	done := make(chan result, 1)
	go func() {
		t0 := time.Now()
		attID, _ := ids.Next() // goroutine 内错误经 result.err 回传
		evtID, _ := ids.Next()
		rerr := engine.WithTx(ctx, d, func(q engine.DBTX) error {
			if err := engine.InsertAttempt(ctx, q, engine.AttemptParams{
				ID:             attID,
				NodeExecID:     nodeExecID,
				ExecID:         execID,
				AttemptNo:      99,
				WorkerID:       "gaplock-test",
				IdempotencyKey: fmt.Sprintf("att:%d:99", nodeExecID),
				AsynqTaskID:    fmt.Sprintf("att:%d:99", nodeExecID),
			}); err != nil {
				return err
			}
			return outbox.Append(ctx, q, outbox.Event{
				ID:          evtID,
				ExecutionID: execID,
				EventType:   EventTypeNodeTask,
				TaskID:      fmt.Sprintf("att:%d:99", nodeExecID),
				Payload:     []byte(`{}`),
			})
		})
		done <- result{elapsed: time.Since(t0), err: rerr}
	}()

	time.Sleep(window) // A 持锁窗
	if err := txA.Rollback(); err != nil {
		t.Fatalf("rollback A: %v", err)
	}

	res := <-done
	if res.err != nil {
		t.Fatalf("dispatch tx under relay lock window: %v", res.err)
	}
	t.Logf("dispatch tx under relay lock window (locked row %d, window %s): %v",
		lockedID, window, res.elapsed)
	if res.elapsed >= window {
		t.Errorf("dispatch tx BLOCKED by relay lock window: took %v (>= window %s) — gap lock interaction confirmed, decision required (e.g. relay session READ COMMITTED)",
			res.elapsed, window)
	}
}
