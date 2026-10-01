package reaper

// T15.6 定档 A(D-t151 决策 7,随演练定档):flush/janitor 纳入各自
// leaderGate 键——N 副本稳态单跑,消除每批 N-1 次空转(读批 + 预过滤 +
// PK 锁阻塞的裸 INSERT + 1062 整批回滚)与 ERROR 日志 N× 放大;fail-open
// 语义保留(选主不可达 → 照跑,正确性由预过滤 + 裸 INSERT 收敛结构承载,
// D-t61)。

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"braid/internal/audit"
	"braid/internal/engine"
)

// keyRoutingLocker 按键路由裁决并记录查询面(测试 fake)。
type keyRoutingLocker struct {
	deny  map[string]bool
	calls map[string]int
}

func (l *keyRoutingLocker) AcquireOrRenew(_ context.Context, key, _ string, _ time.Duration) (bool, error) {
	l.calls[key]++
	return !l.deny[key], nil
}

func (l *keyRoutingLocker) Release(context.Context, string, string) error { return nil }

func newKeyRoutingLocker(deny ...string) *keyRoutingLocker {
	l := &keyRoutingLocker{deny: map[string]bool{}, calls: map[string]int{}}
	for _, k := range deny {
		l.deny[k] = true
	}
	return l
}

// stageOnePending 借 audit.Enqueue 事务语义暂存一行(串链/列归一单源)。
func stageOnePending(t *testing.T, r *Reaper, execID uint64) {
	t.Helper()
	ctx := context.Background()
	id, err := mustTestIDs(t).Next()
	if err != nil {
		t.Fatalf("gen id: %v", err)
	}
	if err := engine.WithTx(ctx, r.deps.DB, func(q engine.DBTX) error {
		return audit.Enqueue(ctx, q, audit.Event{
			ID: id, EventType: audit.EventNodeStateChanged, ExecutionID: execID,
			ActorType: audit.ActorWorker, ActorID: "gate-seed",
			Payload: json.RawMessage(`{"from":"RUNNING","to":"SUCCEEDED"}`),
		})
	}); err != nil {
		t.Fatalf("stage pending: %v", err)
	}
}

func pendingCount(t *testing.T, r *Reaper, execID uint64) int {
	t.Helper()
	var n int
	if err := r.deps.DB.QueryRow(`SELECT COUNT(*) FROM audit_pending WHERE execution_id=?`, execID).Scan(&n); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	return n
}

// TestAuditFlushLoopOwnLeaderKey:flush 循环查自己的 leader 键(不动扫描键)
// ——他人持键 → 整轮跳过(暂存行不动);放行 → 暂存行被搬运。
func TestAuditFlushLoopOwnLeaderKey(t *testing.T) {
	cfg := testCfg()
	cfg.AuditFlushInterval = 40 * time.Millisecond
	cfg.AuditFlushBatch = 100
	r, d := newReaper(t, cfg)
	sd := seedReap(t, d, 0, 0, 1)
	stageOnePending(t, r, sd.execID)
	if got := pendingCount(t, r, sd.execID); got != 1 {
		t.Fatalf("staged = %d, want 1", got)
	}

	denied := newKeyRoutingLocker(leaderKeyFlush)
	r.deps.Locker = denied
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.auditFlushLoop(ctx, "tok") }()
	time.Sleep(250 * time.Millisecond)
	cancel()
	<-done

	if denied.calls[leaderKeyFlush] < 2 {
		t.Fatalf("flush key gate calls = %d, want ≥2(逐 tick 闸)", denied.calls[leaderKeyFlush])
	}
	for _, k := range []string{leaderKey, leaderKeyJanitor} {
		if denied.calls[k] != 0 {
			t.Fatalf("flush loop touched %q (%d times), want 0(键面隔离)", k, denied.calls[k])
		}
	}
	if got := pendingCount(t, r, sd.execID); got != 1 {
		t.Fatalf("pending after denied window = %d, want 1(整轮跳过)", got)
	}

	allowed := newKeyRoutingLocker()
	r.deps.Locker = allowed
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { defer close(done2); r.auditFlushLoop(ctx2, "tok") }()
	drained := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pendingCount(t, r, sd.execID) == 0 {
			drained = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel2()
	<-done2
	if !drained {
		t.Fatalf("pending not drained within 5s after allow")
	}
	if allowed.calls[leaderKeyFlush] == 0 {
		t.Fatalf("allowed window never consulted flush key")
	}
}

// TestJanitorLoopOwnLeaderKey:janitor 循环查自己的 leader 键;他人持键 →
// 超保留期 DISPATCHED outbox 行不被清,放行 → 被清。
func TestJanitorLoopOwnLeaderKey(t *testing.T) {
	cfg := testCfg()
	cfg.JanitorInterval = 40 * time.Millisecond
	r, d := newReaper(t, cfg)
	execID, _ := seedExecRow(t, d, "SUCCEEDED", 0)
	if _, err := d.Exec(`INSERT INTO outbox
		(id, execution_id, event_type, task_id, payload, status, available_at, dispatched_at)
		VALUES (?, ?, 'execution.resume', ?, '{}', 'DISPATCHED',
		NOW(3) - INTERVAL 2592000000000 MICROSECOND, NOW(3) - INTERVAL 2592000000000 MICROSECOND)`,
		nextID(t), execID, fmt.Sprintf("exec:%d:resume", execID)); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	outboxN := func() int {
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM outbox WHERE execution_id=?`, execID).Scan(&n); err != nil {
			t.Fatalf("count outbox: %v", err)
		}
		return n
	}
	if outboxN() != 1 {
		t.Fatalf("seeded outbox = %d, want 1", outboxN())
	}

	denied := newKeyRoutingLocker(leaderKeyJanitor)
	r.deps.Locker = denied
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.janitorLoop(ctx, "tok") }()
	time.Sleep(250 * time.Millisecond)
	cancel()
	<-done

	if denied.calls[leaderKeyJanitor] < 2 {
		t.Fatalf("janitor key gate calls = %d, want ≥2(逐轮闸)", denied.calls[leaderKeyJanitor])
	}
	for _, k := range []string{leaderKey, leaderKeyFlush} {
		if denied.calls[k] != 0 {
			t.Fatalf("janitor loop touched %q (%d times), want 0(键面隔离)", k, denied.calls[k])
		}
	}
	if got := outboxN(); got != 1 {
		t.Fatalf("outbox after denied window = %d, want 1(清理整轮跳过)", got)
	}

	allowed := newKeyRoutingLocker()
	r.deps.Locker = allowed
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { defer close(done2); r.janitorLoop(ctx2, "tok") }()
	purged := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if outboxN() == 0 {
			purged = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel2()
	<-done2
	if !purged {
		t.Fatalf("outbox not purged within 10s after allow")
	}
}
