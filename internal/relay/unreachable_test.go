package relay

// T8.1 Redis 断连注入:队列 Redis 对 relay 不可达(真 asynq client 指向死
// 端口 = 真实 connection refused 错误族,reaper leaderless 同款结构性口径
// D-t25 #2)→ 失败记账(deliver_attempts≥1、行留 PENDING、last_error 落值)
// → 恢复(真端口)→ 同行重投收敛 DISPATCHED 恰一次。
//
// 与 kill_test 的分工:本臂 = 队列不可达(首投从未到达 Redis);kill_test
// B1-B4 = 首投已达 Redis 但标记前进程亡(重投撞同 TaskID ⇒
// ErrTaskIDConflict 收敛)。两个断连面互补,均无 asynq 副本重复。
//
// trace 连续性(T6.2 增补):断连/重投不改行内容——trace 列与 payload
// 逐字节保真;asynq 侧每 task 恰一个副本(Inspector 计数)。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"braid/internal/redistest"
)

// unreachableCfg:断连臂的压缩配置(退避收紧,恢复臂 SLA 可用;MaxAttempts
// 放大——断连窗内行须留 PENDING 等恢复,不进 DEAD 死信网)。
func unreachableCfg() Config {
	c := DefaultConfig()
	c.IdleInterval = 30 * time.Millisecond
	c.EnqueueTimeout = time.Second
	c.BackoffBase = 50 * time.Millisecond
	c.BackoffFactor = 2
	c.BackoffMax = 200 * time.Millisecond
	c.MaxAttempts = 100
	return c
}

// TestRelayRedisUnreachableThenReconnect:断连 → 记账 → 恢复 → 恰一次收敛
// + trace/payload 保真。
func TestRelayRedisUnreachableThenReconnect(t *testing.T) {
	f := newRFixture(t)
	opt := redistest.MustOpt(t)
	queue := "q" + fmt.Sprint(newID())

	const m = 6
	type srow struct {
		id     uint64
		taskID string
		trace  string
	}
	var rows []srow
	for range m {
		id, taskID := f.row(queue)
		z := fmt.Sprintf("%032x", id) // 行内唯一合法 32-hex(fixture 种子特权补列)
		mustExec(t, f.db, `UPDATE outbox SET trace_id=? WHERE id=?`, z, id)
		rows = append(rows, srow{id: id, taskID: taskID, trace: z})
	}

	// ---- 阶段一:队列不可达(死端口,真实 connection refused)。----
	deadOpt := asynq.RedisClientOpt{Addr: "127.0.0.1:1", DB: opt.DB, DialTimeout: 100 * time.Millisecond}
	cfg := unreachableCfg()
	ctx1, stop1 := context.WithCancel(context.Background())
	done1 := make(chan error, 1)
	go func() { done1 <- Run(ctx1, Deps{DB: f.db, Enqueuer: NewAsynqEnqueuer(deadOpt)}, cfg) }()
	var waitOnce sync.Once
	waitPhase1 := func() {
		waitOnce.Do(func() {
			stop1()
			select {
			case <-done1:
			case <-time.After(15 * time.Second):
				t.Error("relay(phase1) did not stop within 15s after cancel")
			}
		})
	}
	t.Cleanup(waitPhase1)
	// 注入咬合:≥1 行记失败(压缩退避下 asynq client 内部重试延迟 ≈ 退避
	// 间隔,ORDER BY id LIMIT 1 轮转可能饿死大 id 行——全行记账不可依赖;
	// 恢复面由阶段二「全行 DISPATCHED」完整断言)。
	eventually(t, 10*time.Second, "failures recorded while unreachable", func() bool {
		var n int
		_ = f.db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM outbox WHERE execution_id=? AND deliver_attempts>=1`,
			f.execID).Scan(&n)
		return n >= 1
	})
	var lastErr sql.NullString
	if err := f.db.QueryRowContext(context.Background(),
		`SELECT last_error FROM outbox WHERE id=?`, rows[0].id).Scan(&lastErr); err != nil {
		t.Fatalf("read last_error: %v", err)
	}
	if !lastErr.Valid || lastErr.String == "" {
		t.Fatalf("last_error 空:断连失败未被记账")
	}
	waitPhase1()

	// ---- 阶段二:恢复(真端口)→ SLA 内全 DISPATCHED。----
	ctx2, stop2 := context.WithCancel(context.Background())
	defer stop2()
	done2 := make(chan error, 1)
	go func() { done2 <- Run(ctx2, Deps{DB: f.db, Enqueuer: NewAsynqEnqueuer(opt)}, cfg) }()
	sla := 3*(cfg.IdleInterval+cfg.BackoffMax) + 3*time.Second
	eventually(t, sla, "all rows dispatched after recovery", func() bool {
		var n int
		_ = f.db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM outbox WHERE execution_id=? AND status='DISPATCHED'`,
			f.execID).Scan(&n)
		return n == m
	})
	stop2()
	select {
	case <-done2:
	case <-time.After(15 * time.Second):
		t.Fatalf("relay(phase2) did not stop")
	}

	// ---- 保真断言:每行 trace 列不变、payload 语义不变(JSON 列读回重
	// 排版,语义比较——run_test 投递归属同口径)、attempts ≥2(败+成)。----
	for _, r := range rows {
		var status string
		var attempts int
		var trace sql.NullString
		var payload string
		if err := f.db.QueryRowContext(context.Background(),
			`SELECT status, deliver_attempts, trace_id, payload FROM outbox WHERE id=?`, r.id).
			Scan(&status, &attempts, &trace, &payload); err != nil {
			t.Fatalf("scan row %d: %v", r.id, err)
		}
		if status != "DISPATCHED" {
			t.Fatalf("row %d status = %s, want DISPATCHED", r.id, status)
		}
		// 注:attempts 不逐行断言——MarkDispatched 恒 +1,DISPATCHED 已蕴含
		// ≥1(逐行「断连记账」主张对被饿行不成立,咬合由阶段一聚合检查
		// 与 rows[0] last_error 断言承担;T8.1 审计 LOW 修,删空转断言)。
		if !trace.Valid || trace.String != r.trace {
			t.Fatalf("row %d trace = %v, want %q(断连/重投不得改值)", r.id, trace, r.trace)
		}
		var v struct {
			OutboxID uint64 `json:"outbox_id"`
		}
		if err := json.Unmarshal([]byte(payload), &v); err != nil || v.OutboxID != r.id {
			t.Fatalf("row %d payload = %q, want outbox_id=%d(语义保真)", r.id, payload, r.id)
		}
	}

	// ---- asynq 侧恰一次:pending 任务集合与种子 task_id 恰等(无重复副本)。----
	insp := asynq.NewInspector(opt)
	t.Cleanup(func() { _ = insp.Close() })
	pending, err := insp.ListPendingTasks(queue)
	if err != nil {
		t.Fatalf("inspector list pending: %v", err)
	}
	got := make(map[string]bool, len(pending))
	for _, ti := range pending {
		got[ti.ID] = true
	}
	if len(got) != m {
		t.Fatalf("queue pending distinct tasks = %d, want %d(恰一次投递,无重复副本)", len(got), m)
	}
	for _, r := range rows {
		if !got[r.taskID] {
			t.Fatalf("task %s 不在 pending(投递丢失)", r.taskID)
		}
	}
}
