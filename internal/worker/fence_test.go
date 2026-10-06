package worker

// fence_test.go = T20.1 fence 面(worker claim/接管)集成测试(t13 栈
// MySQL;栈不可达自动 skip)。口径真源 = D-2026-10-06-t201 决策 4:claim
// 物化经 fence 事务——失冠(superseded)进程不能建新 attempt 行(双执行
// 证据行不再可能由 zombie 建立),拒绝计入 FenceRejected 计数器。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"braid/internal/db/sqltest"
	"braid/internal/metrics"
	"braid/internal/snowflake"
)

// claimFenceNode 是本文件租约钉的显式号(schema 内自洽)。
const claimFenceNode = 312

func TestClaimAttemptFenceRejectsSupersededLease(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	if _, err := d.Exec(`DELETE FROM snowflake_node_lease`); err != nil {
		t.Fatalf("clear leases: %v", err)
	}
	ctx := context.Background()
	lease, err := snowflake.StartLease(ctx, d, snowflake.LeaseConfig{
		Kind:   snowflake.KindWorker,
		Spec:   snowflake.Explicit(claimFenceNode),
		Holder: "claim-fence",
	})
	if err != nil {
		t.Fatalf("StartLease: %v", err)
	}
	t.Cleanup(func() { _ = lease.Release(context.Background()) })
	mset, _ := metrics.NewSet()
	cfg := DefaultConfig()
	cfg.WorkerID = "claim-fence"
	h := &attHandler{
		deps: Deps{DB: d, IDs: mustTestIDs(t), Lease: lease, Logger: slog.Default(), Metrics: mset.Worker},
		cfg:  cfg,
		log:  slog.Default(),
	}

	// 冠内:claim 物化成功(fence 零行为扰动)。
	exec1, node1 := seedNodeExec(t, d)
	task1 := fmt.Sprintf("att:%d:1", node1)
	id1, err := h.claimAttempt(ctx, task1, nodeTaskPayload{ExecutionID: exec1, NodeExecutionID: node1, AttemptNo: 1})
	if err != nil || id1 == 0 {
		t.Fatalf("crowned claim = (%d, %v), want materialized", id1, err)
	}

	// 易主(zombie 形态:持有字符串未变,代际失配)。
	if _, err := d.Exec(
		`UPDATE snowflake_node_lease SET fencing_epoch = fencing_epoch + 1 WHERE node_id = ?`,
		claimFenceNode); err != nil {
		t.Fatalf("supersede lease: %v", err)
	}

	exec2, node2 := seedNodeExec(t, d)
	task2 := fmt.Sprintf("att:%d:1", node2)
	_, err = h.claimAttempt(ctx, task2, nodeTaskPayload{ExecutionID: exec2, NodeExecutionID: node2, AttemptNo: 1})
	if !errors.Is(err, snowflake.ErrFenced) {
		t.Fatalf("superseded claim: %v, want ErrFenced(zombie 不得建 attempt 行)", err)
	}
	var rows int
	if err := d.QueryRow(`SELECT COUNT(*) FROM node_attempt WHERE node_execution_id=?`, node2).Scan(&rows); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if rows != 0 {
		t.Fatalf("superseded claim materialized %d rows, want 0", rows)
	}
	// 观测锚:worker 侧 fence 拒绝计数器 +1(决策 7)。
	if got := testutil.ToFloat64(mset.Worker.FenceRejected); got != 1 {
		t.Fatalf("FenceRejected = %f, want 1", got)
	}
	// 冠内那次不受污染。
	if id1 == 0 {
		t.Fatalf("crowned claim id lost")
	}
}

// hbFenceSeedAttempt 直插一行 RUNNING attempt(心跳对象),返回 attempt id。
func hbFenceSeedAttempt(t *testing.T, d *sql.DB, execID, nodeExecID uint64) uint64 {
	t.Helper()
	ids := mustTestIDs(t)
	attID, err := ids.Next()
	if err != nil {
		t.Fatalf("gen attempt id: %v", err)
	}
	taskID := fmt.Sprintf("att:%d:1", nodeExecID)
	if _, err := d.Exec(`INSERT INTO node_attempt
	  (id, node_execution_id, execution_id, attempt_no, worker_id, status,
	   idempotency_key, asynq_task_id, started_at, heartbeat_at)
	  VALUES (?,?,?,?,?,'RUNNING',?,?,NOW(3),NOW(3))`,
		attID, nodeExecID, execID, 1, "hb-fence", taskID, taskID); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
	return attID
}

func heartbeatAtF(t *testing.T, d *sql.DB, attID uint64) time.Time {
	t.Helper()
	var hb time.Time
	if err := d.QueryRow(`SELECT heartbeat_at FROM node_attempt WHERE id=?`, attID).Scan(&hb); err != nil {
		t.Fatalf("read heartbeat: %v", err)
	}
	return hb
}

// TestHeartbeatFenceStopsBeats:对抗审计 HIGH 锚——心跳面 fence:冠内心跳
// 前移 heartbeat_at;失冠后心跳结构性停跳(零前移 → reaper 判死接管),
// 拒绝计入 FenceRejected。
func TestHeartbeatFenceStopsBeats(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	if _, err := d.Exec(`DELETE FROM snowflake_node_lease`); err != nil {
		t.Fatalf("clear leases: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lease, err := snowflake.StartLease(ctx, d, snowflake.LeaseConfig{
		Kind:   snowflake.KindWorker,
		Spec:   snowflake.Explicit(claimFenceNode),
		Holder: "hb-fence",
	})
	if err != nil {
		t.Fatalf("StartLease: %v", err)
	}
	t.Cleanup(func() { _ = lease.Release(context.Background()) })
	mset, _ := metrics.NewSet()
	cfg := DefaultConfig()
	cfg.WorkerID = "hb-fence"
	cfg.HeartbeatInterval = 40 * time.Millisecond // 测试压缩
	h := &attHandler{
		deps: Deps{DB: d, IDs: mustTestIDs(t), Lease: lease, Logger: slog.Default(), Metrics: mset.Worker},
		cfg:  cfg,
		log:  slog.Default(),
	}
	execID, nodeExecID := seedNodeExec(t, d)
	attID := hbFenceSeedAttempt(t, d, execID, nodeExecID)
	hb0 := heartbeatAtF(t, d, attID)

	go h.heartbeat(ctx, attID, execID)
	// 冠内:≥2 个 tick 后 heartbeat_at 前移。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if heartbeatAtF(t, d, attID).After(hb0) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	hb1 := heartbeatAtF(t, d, attID)
	if !hb1.After(hb0) {
		t.Fatalf("crowned heartbeat not advancing: %v -> %v", hb0, hb1)
	}

	// 失冠:心跳停跳(零前移)+ 计数器增长。
	if _, err := d.Exec(
		`UPDATE snowflake_node_lease SET fencing_epoch = fencing_epoch + 1 WHERE node_id = ?`,
		claimFenceNode); err != nil {
		t.Fatalf("supersede lease: %v", err)
	}
	time.Sleep(3 * cfg.HeartbeatInterval) // 失冠后 ≥3 个 tick
	hb2 := heartbeatAtF(t, d, attID)
	if hb2.After(hb1) {
		t.Fatalf("heartbeat advanced after supersede: %v -> %v, want frozen(zombie 心跳结构性停跳)", hb1, hb2)
	}
	if got := testutil.ToFloat64(mset.Worker.FenceRejected); got < 1 {
		t.Fatalf("FenceRejected = %f, want ≥1(观测锚)", got)
	}
}

// TestTakeoverFenceAbortsZombie:对抗审计 HIGH 锚——接管复核面:失冠进程
// 对重投任务撞 uk 后的接管被 fence 中止(错误上抛 → asynq 重试归活体),
// 不得落「warn 后继续执行」(reset 尽力语义与 fence 拒绝分立)。
func TestTakeoverFenceAbortsZombie(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	if _, err := d.Exec(`DELETE FROM snowflake_node_lease`); err != nil {
		t.Fatalf("clear leases: %v", err)
	}
	ctx := context.Background()
	lease, err := snowflake.StartLease(ctx, d, snowflake.LeaseConfig{
		Kind:   snowflake.KindWorker,
		Spec:   snowflake.Explicit(claimFenceNode),
		Holder: "tk-fence",
	})
	if err != nil {
		t.Fatalf("StartLease: %v", err)
	}
	t.Cleanup(func() { _ = lease.Release(context.Background()) })
	mset, _ := metrics.NewSet()
	cfg := DefaultConfig()
	cfg.WorkerID = "tk-fence"
	h := &attHandler{
		deps: Deps{DB: d, IDs: mustTestIDs(t), Lease: lease, Logger: slog.Default(), Metrics: mset.Worker},
		cfg:  cfg,
		log:  slog.Default(),
	}
	execID, nodeExecID := seedNodeExec(t, d)
	taskID := fmt.Sprintf("att:%d:1", nodeExecID)
	payload := nodeTaskPayload{ExecutionID: execID, NodeExecutionID: nodeExecID, AttemptNo: 1}
	// 冠内首达:attempt 行物化。
	rowID, err := h.claimAttempt(ctx, taskID, payload)
	if err != nil || rowID == 0 {
		t.Fatalf("crowned claim = (%d, %v)", rowID, err)
	}
	hb1 := heartbeatAtF(t, d, rowID)

	// 失冠后的重投(同 taskID):1062 → readAttempt RUNNING → 接管复核 →
	// fence 拒 → 错误上抛(不执行)。
	if _, err := d.Exec(
		`UPDATE snowflake_node_lease SET fencing_epoch = fencing_epoch + 1 WHERE node_id = ?`,
		claimFenceNode); err != nil {
		t.Fatalf("supersede lease: %v", err)
	}
	if _, err := h.claimAttempt(ctx, taskID, payload); !errors.Is(err, snowflake.ErrFenced) {
		t.Fatalf("superseded takeover claim: %v, want ErrFenced(zombie 不得接管)", err)
	}
	if hb2 := heartbeatAtF(t, d, rowID); hb2.After(hb1) {
		t.Fatalf("heartbeat_at advanced on fenced takeover: %v -> %v, want unchanged(接管重置被拒)", hb1, hb2)
	}
	if got := testutil.ToFloat64(mset.Worker.FenceRejected); got < 1 {
		t.Fatalf("FenceRejected = %f, want ≥1", got)
	}
}
