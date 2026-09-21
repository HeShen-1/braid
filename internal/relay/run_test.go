package relay

// relay 常规路径测试矩阵:真 MySQL(braid_relay_test 独立 schema,与 outbox 包
// 测试并行不互踩——relay 轮询会捡走全库 eligible 行,必须包级隔离)+ stub
// Enqueuer(不触 Redis;Asynq option 的正确性——TaskID/Queue/MaxRetry——由
// kill_test 经真实 Redis 断言,asynq.Option 对 stub 不可内省)。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"braid/internal/db/sqltest"
)

const relaySchema = "braid_relay_test"

var (
	idBase = uint64(time.Now().UnixNano()>>20)<<16 ^ uint64(os.Getpid())<<36
	idCtr  atomic.Uint64
)

func newID() uint64 { return idBase + idCtr.Add(1) }

// stubEnq:记录 type/payload 的假 Enqueuer,fn 注入行为(nil = 成功)。
// task_id 经 asynq.Option 传递、stub 不可内省 ⇒ 投递归属取证靠 payload
// (fixture 的 payload 自描述携带 outbox_id)。
type stubEnq struct {
	mu       sync.Mutex
	types    []string
	payloads []string
	calls    int
	fn       func(call int, typ string, payload []byte) error
}

func (s *stubEnq) EnqueueContext(_ context.Context, t *asynq.Task, _ ...asynq.Option) (*asynq.TaskInfo, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	typ := t.Type()
	payload := append([]byte(nil), t.Payload()...)
	s.mu.Unlock()
	if s.fn != nil {
		if err := s.fn(call, typ, payload); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	s.types = append(s.types, typ)
	s.payloads = append(s.payloads, string(payload))
	s.mu.Unlock()
	return &asynq.TaskInfo{ID: "stub"}, nil
}

func (s *stubEnq) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *stubEnq) payloadsSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.payloads...)
}

// rfixture:relay 包的 outbox 行种子 + 清理。直写 INSERT/UPDATE(outbox)是
// fixture 种子特权(status 恒 PENDING、仅调 available_at),生产代码对 outbox
// 的写入只经 outbox.Append 与三原语(包级铁律)。
type rfixture struct {
	t      *testing.T
	db     *sql.DB
	execID uint64
}

func newRFixture(t *testing.T) *rfixture {
	t.Helper()
	f := &rfixture{t: t, db: sqltest.MustOpenAt(t, relaySchema), execID: newID()}
	t.Cleanup(func() {
		mustExec(t, f.db, `DELETE FROM outbox WHERE execution_id=?`, f.execID)
	})
	return f
}

// row 种一条立即可投的 PENDING 事件,返回 outbox 行 id 与 task_id。
func (f *rfixture) row(queue string) (id uint64, taskID string) {
	f.t.Helper()
	id, taskID = newID(), fmt.Sprintf("att:%d:1", newID())
	mustExec(f.t, f.db, `INSERT INTO outbox
		(id, execution_id, event_type, task_id, queue, payload, status, deliver_attempts, available_at)
		VALUES (?,?, 'node.task_enqueue', ?, ?, ?, 'PENDING', 0, NOW(3))`,
		id, f.execID, taskID, queue, fmt.Sprintf(`{"outbox_id":%d}`, id))
	return id, taskID
}

func (f *rfixture) outboxRow(id uint64) (status string, attempts int, lastErr sql.NullString, availReady bool) {
	f.t.Helper()
	err := f.db.QueryRowContext(context.Background(), `
SELECT status, deliver_attempts, last_error, available_at<=NOW(3)
  FROM outbox WHERE id=?`, id).Scan(&status, &attempts, &lastErr, &availReady)
	if err != nil {
		f.t.Fatalf("scan outbox %d: %v", id, err)
	}
	return
}

func mustExec(t *testing.T, d *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := d.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("seed/exec %q: %v", query, err)
	}
}

// eventually:有界轮询(全部等待有 deadline,失败 dump 现场——防 CI 挂死)。
func eventually(t *testing.T, timeout time.Duration, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, what)
}

func testCfg() Config {
	c := DefaultConfig()
	c.IdleInterval = 10 * time.Millisecond
	c.EnqueueTimeout = 2 * time.Second
	return c
}

// TestDeliverSuccessMarksDispatched:成功路径——行转 DISPATCHED、attempts=1
// (有结论的尝试计数)、dispatched_at 落值、task type = event_type、payload 原文投递。
func TestDeliverSuccessMarksDispatched(t *testing.T) {
	f := newRFixture(t)
	id, taskID := f.row("q" + fmt.Sprint(newID()))
	stub := &stubEnq{}
	runRelay(t, f.db, stub, testCfg(), func() bool {
		status, attempts, _, _ := f.outboxRow(id)
		return status == "DISPATCHED" && attempts == 1
	})
	var dispatchedAt sql.NullTime
	if err := f.db.QueryRowContext(context.Background(),
		`SELECT dispatched_at FROM outbox WHERE id=?`, id).Scan(&dispatchedAt); err != nil {
		t.Fatalf("scan dispatched_at: %v", err)
	}
	if !dispatchedAt.Valid {
		t.Fatalf("dispatched_at NULL on DISPATCHED row")
	}
	if len(stub.types) != 1 || stub.types[0] != "node.task_enqueue" {
		t.Fatalf("task types = %v, want [node.task_enqueue]", stub.types)
	}
	_ = taskID
}

// TestDeliverFailureRecordsBackoff:失败路径——attempts 递增、last_error 落库、
// available_at 推到未来(退避期不再被扫)、行保持 PENDING。BackoffBase=1h 使
// 状态稳定可断言(无时序竞态)。
func TestDeliverFailureRecordsBackoff(t *testing.T) {
	f := newRFixture(t)
	id, _ := f.row("q" + fmt.Sprint(newID()))
	stub := &stubEnq{fn: func(int, string, []byte) error {
		return errors.New("redis: connection refused (injected)")
	}}
	cfg := testCfg()
	cfg.BackoffBase = time.Hour
	cfg.BackoffMax = time.Hour
	runRelay(t, f.db, stub, cfg, func() bool {
		status, attempts, lastErr, ready := f.outboxRow(id)
		return status == "PENDING" && attempts == 1 && lastErr.Valid && !ready
	})
	// 退避期内不再尝试(available_at = +1h;200ms 观察窗内调用数不得增长)。
	time.Sleep(200 * time.Millisecond)
	if n := stub.count(); n != 1 {
		t.Fatalf("retry during backoff window: %d calls", n)
	}
}

// TestDeliverExhaustionMarksDead:MaxAttempts=1,首次失败即 DEAD。
func TestDeliverExhaustionMarksDead(t *testing.T) {
	f := newRFixture(t)
	id, _ := f.row("q" + fmt.Sprint(newID()))
	stub := &stubEnq{fn: func(int, string, []byte) error { return errors.New("down") }}
	cfg := testCfg()
	cfg.MaxAttempts = 1
	runRelay(t, f.db, stub, cfg, func() bool {
		status, attempts, lastErr, _ := f.outboxRow(id)
		return status == "DEAD" && attempts == 1 && lastErr.Valid
	})
}

// TestAvailableAtGating:available_at 在未来的行不被扫描、不被触碰。
// 必须有**可投对照行**先收敛(证明 relay 活着且在扫),否则「relay 尚未起跑」
// 与「谓词正确」都表现为零调用,负向断言空洞。
func TestAvailableAtGating(t *testing.T) {
	f := newRFixture(t)
	id, _ := f.row("q" + fmt.Sprint(newID()))
	mustExec(t, f.db, `UPDATE outbox SET available_at=DATE_ADD(NOW(3), INTERVAL 1 HOUR) WHERE id=?`, id)
	controlID, _ := f.row("q" + fmt.Sprint(newID())) // 可投对照(后种但立即可投)
	stub := &stubEnq{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = Run(ctx, Deps{DB: f.db, Enqueuer: stub}, testCfg())
		close(done)
	}()
	eventually(t, 2*time.Second, "control row dispatched (relay is live)", func() bool {
		status, _, _, _ := f.outboxRow(controlID)
		return status == "DISPATCHED"
	})
	cancel()
	<-done
	status, attempts, lastErr, _ := f.outboxRow(id)
	if status != "PENDING" || attempts != 0 || lastErr.Valid {
		t.Fatalf("future-available row touched: status=%s attempts=%d lastErr=%v", status, attempts, lastErr.Valid)
	}
}

// TestFIFOOrder:多事件按 id 序投递(Snowflake 近似时序的 FIFO 白拿)。
// 可投时刻做成**反向阶梯**(大 id 先可投):idx_outbox_poll 的天然扫描序是
// (status, available_at, id) ⇒ 无 ORDER BY id 时会先投大 id——这让「丢
// ORDER BY id」变异可杀。投递序取 stub 调用序(payload 内嵌 outbox_id 语义
// 解析),不用 dispatched_at(DATETIME(3) 毫秒并列时排序非契约)。
// fixture 直写 UPDATE available_at 属种子特权,生产状态变更只经三原语。
func TestFIFOOrder(t *testing.T) {
	f := newRFixture(t)
	queue := "q" + fmt.Sprint(newID())
	ids := make([]uint64, 3)
	for i := range ids {
		ids[i], _ = f.row(queue)
	}
	// 反向阶梯:id 越大可投越早(全部落在过去,立即可投)。
	for i, id := range ids {
		mustExec(t, f.db,
			`UPDATE outbox SET available_at=DATE_SUB(NOW(3), INTERVAL ? SECOND) WHERE id=?`,
			(3-i)*10, id)
	}
	stub := &stubEnq{}
	runRelay(t, f.db, stub, testCfg(), func() bool {
		var n int
		_ = f.db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM outbox WHERE execution_id=? AND status='DISPATCHED'`, f.execID).Scan(&n)
		return n == 3
	})
	var got []uint64
	for _, p := range stub.payloadsSnapshot() {
		var v struct {
			OutboxID uint64 `json:"outbox_id"`
		}
		if err := json.Unmarshal([]byte(p), &v); err != nil {
			t.Fatalf("payload not json: %q", p)
		}
		got = append(got, v.OutboxID)
	}
	if !slices.Equal(got, ids) {
		t.Fatalf("delivery order %v != id order %v", got, ids)
	}
}

// TestSkipLockedConcurrency:A 在首行 enqueue 处阻塞(持扫描事务与行锁),
// B 必须**不被阻塞**地投走另一行(SKIP LOCKED 的阻塞证明反变);放行后 A 收口。
// 投递归属按 payload 内嵌 outbox_id 逐行断言——两只 stub 恰好各投自己那一行。
// 咬住两类变异:
//   - 丢 SKIP LOCKED(留 FOR UPDATE):B 的 SELECT 在 A 的行锁上等待 ⇒ row2
//     观察窗内不 DISPATCHED;
//   - 丢整段锁定子句(快照读):B 会把 row1 也投走 ⇒ stubB 的 payload 集含
//     row1(重复投递),归属断言红。
func TestSkipLockedConcurrency(t *testing.T) {
	f := newRFixture(t)
	queue := "q" + fmt.Sprint(newID())
	id1, task1 := f.row(queue)
	id2, task2 := f.row(queue)

	blocked := make(chan struct{}, 1) // 缓冲 1:信号不因主 goroutine 晚到而丢失
	release := make(chan struct{})
	stubA := &stubEnq{fn: func(int, string, []byte) error {
		select {
		case blocked <- struct{}{}:
		default: // 只在首行发信号
		}
		<-release
		return nil
	}}
	stubB := &stubEnq{}

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	go func() { _ = Run(ctxA, Deps{DB: f.db, Enqueuer: stubA}, testCfg()) }()
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatalf("relay A never reached enqueue (row1 not picked)")
	}

	// A 持锁阻塞中:B 需在 1s 内投走 row2。
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	go func() { _ = Run(ctxB, Deps{DB: f.db, Enqueuer: stubB}, testCfg()) }()
	eventually(t, 3*time.Second, "relay B delivered row2 while A holds lock on row1", func() bool {
		var n int
		_ = f.db.QueryRowContext(context.Background(), `
SELECT COUNT(*) FROM outbox WHERE execution_id=? AND task_id=? AND status='DISPATCHED'`,
			f.execID, task2).Scan(&n)
		return n == 1
	})
	close(release)
	eventually(t, 2*time.Second, "row1 dispatched after release", func() bool {
		var n int
		_ = f.db.QueryRowContext(context.Background(), `
SELECT COUNT(*) FROM outbox WHERE execution_id=? AND task_id=? AND status='DISPATCHED'`,
			f.execID, task1).Scan(&n)
		return n == 1
	})

	// 投递归属:A 恰投 row1,B 恰投 row2(payload 内嵌 outbox_id;语义解析,
	// 不做字节比较——MySQL JSON 列读回会重排版)。
	assertDeliveredIDs := func(stub *stubEnq, name string, want ...uint64) {
		t.Helper()
		got := []uint64{}
		for _, p := range stub.payloadsSnapshot() {
			var v struct {
				OutboxID uint64 `json:"outbox_id"`
			}
			if err := json.Unmarshal([]byte(p), &v); err != nil {
				t.Fatalf("%s payload not json: %q", name, p)
			}
			got = append(got, v.OutboxID)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s delivered %v, want %v (duplicate/misrouted delivery)", name, got, want)
		}
	}
	assertDeliveredIDs(stubA, "relayA", id1)
	assertDeliveredIDs(stubB, "relayB", id2)
}

// TestTwoRelaysNoDuplicateDelivery:双 relay 并发扫描 M 行,总投递次数恰 = M
// (选择→标记原子 ⇒ 稳态零重复入队)。咬住变异:非原子 claim+mark(先标记后
// 崩溃/无锁扫描 ⇒ 重复投递)。
func TestTwoRelaysNoDuplicateDelivery(t *testing.T) {
	f := newRFixture(t)
	queue := "q" + fmt.Sprint(newID())
	const m = 8
	for range m {
		f.row(queue)
	}
	stub := &stubEnq{} // 共享计数(互斥保护)
	cfg := testCfg()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = Run(ctx, Deps{DB: f.db, Enqueuer: stub}, cfg)
		}()
	}
	eventually(t, 5*time.Second, "all rows dispatched", func() bool {
		var n int
		_ = f.db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM outbox WHERE execution_id=? AND status='DISPATCHED'`, f.execID).Scan(&n)
		return n == m
	})
	cancel()
	wg.Wait()
	if n := stub.count(); n != m {
		t.Fatalf("total enqueues = %d, want exactly %d (duplicate delivery)", n, m)
	}
}

// TestRunReturnsNilOnCancel:优雅停机。
func TestRunReturnsNilOnCancel(t *testing.T) {
	f := newRFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, Deps{DB: f.db, Enqueuer: &stubEnq{}}, testCfg()) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on cancel, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after cancel")
	}
}

// TestRunRejectsBadDeps:接线缺损与非法配置启动即败。第三例给**合法 Deps**,
// 只让 cfg 非法——否则 nil-DB 先败,「删 MaxAttempts 校验」变异被掩蔽。
func TestRunRejectsBadDeps(t *testing.T) {
	f := newRFixture(t)
	if err := Run(context.Background(), Deps{}, DefaultConfig()); err == nil {
		t.Fatalf("nil deps accepted")
	}
	if err := Run(context.Background(), Deps{DB: nil, Enqueuer: &stubEnq{}}, DefaultConfig()); err == nil {
		t.Fatalf("nil DB accepted")
	}
	cfg := DefaultConfig()
	cfg.MaxAttempts = 0
	if err := Run(context.Background(), Deps{DB: f.db, Enqueuer: &stubEnq{}}, cfg); err == nil {
		t.Fatalf("invalid config accepted")
	}
}

// TestShutdownInterruptLeavesRowUntouched:停机撞上在飞轮次的核心承诺——
// enqueue 因 ctx 取消失败时,不计尝试、不动行(空事务提交释放锁),行保持
// PENDING+attempts=0 待下轮;Run 对 ctx 取消返回 nil。这是 run.go 头注释
// 「停机中断」语义的直接证据(stub 阻塞至外部 cancel 后返回错误)。
func TestShutdownInterruptLeavesRowUntouched(t *testing.T) {
	f := newRFixture(t)
	id, _ := f.row("q" + fmt.Sprint(newID()))
	ctx, cancel := context.WithCancel(context.Background())
	stub := &stubEnq{fn: func(int, string, []byte) error {
		<-ctx.Done() // 阻塞至停机,返回「失败」(模拟取消杀掉的 enqueue)
		return errors.New("context canceled (injected)")
	}}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, Deps{DB: f.db, Enqueuer: stub}, testCfg()) }()
	eventually(t, 2*time.Second, "relay reached enqueue", func() bool { return stub.count() >= 1 })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on shutdown, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after cancel")
	}
	status, attempts, lastErr, _ := f.outboxRow(id)
	if status != "PENDING" || attempts != 0 || lastErr.Valid {
		t.Fatalf("shutdown interrupt touched row: status=%s attempts=%d lastErr=%v", status, attempts, lastErr.Valid)
	}
}

// runRelay:跑一个 relay 直到 stop() 为真(或 5s 兜底),返回后 relay 已停。
// stop 必须以**行的终态**(DISPATCHED/DEAD/退避记账落库)为条件——不得用
// enqueue 调用计数:计数在 EnqueueContext 返回前即可见,cancel 会杀掉在飞的
// 标记 UPDATE(驱动以 ctx 取消杀语句)→ 行滞留 PENDING → 终态断言假红。
func runRelay(t *testing.T, db *sql.DB, eq Enqueuer, cfg Config, stop func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = Run(ctx, Deps{DB: db, Enqueuer: eq}, cfg)
		close(done)
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		if stop() {
			cancel()
			<-done
			return
		}
		select {
		case <-deadline.C:
			cancel()
			<-done
			t.Fatalf("relay stop condition not met within 5s")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
