package reaper

// T17.4(D-2026-10-04-t174)分支 B asynq 活性对拍测试:asynq 积压 >
// DISPATCH_STALE 时 outbox 死证据会说谎(任务仍在队列),重派 = 用新
// TaskID 制造第二活副本(D-t25#10 残余窗)。四臂:活让位 / 死重派 /
// 探测失败 fail-open / probe 缺席降级。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"braid/internal/metrics"
	"braid/internal/scheduler"
)

// fakeAsynqProbe 是 AsynqProbe 的测试替身(计数 + 可编程结果)。
type fakeAsynqProbe struct {
	alive    bool
	err      error
	calls    int
	lastTask string
	lastQ    string
}

func (p *fakeAsynqProbe) TaskAlive(_ context.Context, queue, taskID string) (bool, error) {
	p.calls++
	p.lastTask = taskID
	p.lastQ = queue
	return p.alive, p.err
}

// seedBranchBStaleRows 种分支 B + 陈旧 DISPATCHED 现场:seedReap(attempts=1,
// attempt 1 RUNNING 心跳陈旧)后抬 attempts=2(当前号无行 → [2] 落空 →
// 分支 B),再落一行当前 attempt 的陈旧 DISPATCHED 派发事件(dispatched_at
// 回拨 5min > testCfg DispatchStale 1m)。返回当前 task id。
func seedBranchBStaleRows(t *testing.T, d *sql.DB, s seeded) string {
	t.Helper()
	if _, err := d.Exec(`UPDATE node_execution SET attempts=2 WHERE id=?`, s.nodeExecID); err != nil {
		t.Fatalf("seed branch-b attempts: %v", err)
	}
	curTask := fmt.Sprintf("att:%d:2", s.nodeExecID)
	payload, err := json.Marshal(scheduler.NodeTaskPayload{
		ExecutionID: s.execID, NodeExecutionID: s.nodeExecID, AttemptNo: 2,
	})
	if err != nil {
		t.Fatalf("marshal node task payload: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO outbox
	  (id, execution_id, event_type, task_id, queue, payload, status, dispatched_at, available_at)
	  VALUES (?, ?, 'node.task_enqueue', ?, 'reap-test', ?, 'DISPATCHED', NOW(3) - INTERVAL 5 MINUTE, NOW(3))`,
		nextID(t), s.execID, curTask, payload); err != nil {
		t.Fatalf("seed stale dispatch event: %v", err)
	}
	return curTask
}

// TestRedispatchDeferredWhenAsynqAlive:死证据只剩陈旧 DISPATCHED + 任务仍活
// → 本 tick 让位(零重派、counter+1、probe 恰一次),节点原样。
func TestRedispatchDeferredWhenAsynqAlive(t *testing.T) {
	set, _ := metrics.NewSet()
	probe := &fakeAsynqProbe{alive: true}
	r, d := newReaperDeps(t, testCfg(), func(dp *Deps) {
		dp.Metrics = set.Reaper
		dp.AsynqProbe = probe
	})
	s := seedReap(t, d, 5*time.Minute, 0, 3)
	curTask := seedBranchBStaleRows(t, d, s)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if probe.calls != 1 {
		t.Fatalf("probe calls = %d, want 1", probe.calls)
	}
	if probe.lastTask != curTask || probe.lastQ != "reap-test" {
		t.Fatalf("probe = (%s, %s), want (%s, reap-test)", probe.lastQ, probe.lastTask, curTask)
	}
	if got := testutil.ToFloat64(set.Reaper.AsynqAliveDefers); got != 1 {
		t.Fatalf("asynq_alive_defers = %v, want 1", got)
	}
	if rows := fetchOutbox(t, d, s.execID, "node.task_enqueue"); len(rows) != 1 {
		t.Fatalf("task_enqueue 行数 = %d, want 1(零重派,只剩原陈旧行)", len(rows))
	}
	var attempts int
	var status string
	if err := d.QueryRow(`SELECT attempts, status FROM node_execution WHERE id=?`, s.nodeExecID).
		Scan(&attempts, &status); err != nil {
		t.Fatalf("read node: %v", err)
	}
	if attempts != 2 || status != "EXECUTING" {
		t.Fatalf("node = (%d, %s), want (2, EXECUTING)(让位零动作)", attempts, status)
	}
}

// TestRedispatchProceedsWhenAsynqDead:对拍非活(NotFound/archived 族)→
// 核验后二次入事务照旧重派(新 TaskID,行为与既有语义一致)。
func TestRedispatchProceedsWhenAsynqDead(t *testing.T) {
	set, _ := metrics.NewSet()
	probe := &fakeAsynqProbe{alive: false}
	r, d := newReaperDeps(t, testCfg(), func(dp *Deps) {
		dp.Metrics = set.Reaper
		dp.AsynqProbe = probe
	})
	s := seedReap(t, d, 5*time.Minute, 0, 3)
	seedBranchBStaleRows(t, d, s)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if probe.calls != 1 {
		t.Fatalf("probe calls = %d, want 1", probe.calls)
	}
	want := fmt.Sprintf("att:%d:3", s.nodeExecID)
	if err := d.QueryRow(`SELECT 1 FROM outbox WHERE task_id=?`, want).Scan(new(int)); err != nil {
		t.Fatalf("重派行 %s 缺席(对拍非活必须照旧重派): %v", want, err)
	}
	if got := testutil.ToFloat64(set.Reaper.AsynqAliveDefers); got != 0 {
		t.Fatalf("asynq_alive_defers = %v, want 0", got)
	}
}

// TestRedispatchFailOpenOnProbeError:探测不可达 → fail-open(D-t25#2 同族):
// 重派照旧(DispatchStale 有界窗语义),零让位计数。
func TestRedispatchFailOpenOnProbeError(t *testing.T) {
	set, _ := metrics.NewSet()
	probe := &fakeAsynqProbe{err: errors.New("redis: connection refused")}
	r, d := newReaperDeps(t, testCfg(), func(dp *Deps) {
		dp.Metrics = set.Reaper
		dp.AsynqProbe = probe
	})
	s := seedReap(t, d, 5*time.Minute, 0, 3)
	seedBranchBStaleRows(t, d, s)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	want := fmt.Sprintf("att:%d:3", s.nodeExecID)
	if err := d.QueryRow(`SELECT 1 FROM outbox WHERE task_id=?`, want).Scan(new(int)); err != nil {
		t.Fatalf("重派行 %s 缺席(fail-open 必须照旧重派): %v", want, err)
	}
	if got := testutil.ToFloat64(set.Reaper.AsynqAliveDefers); got != 0 {
		t.Fatalf("asynq_alive_defers = %v, want 0(探测失败不是让位)", got)
	}
}

// TestRedispatchNilProbeKeepsLegacyBehavior:probe 缺席(未配置 Redis 的
// 降级部署)→ 陈旧 DISPATCHED 死证据直接走重派,D-t25#10 有界接受照旧。
func TestRedispatchNilProbeKeepsLegacyBehavior(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedReap(t, d, 5*time.Minute, 0, 3)
	seedBranchBStaleRows(t, d, s)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	want := fmt.Sprintf("att:%d:3", s.nodeExecID)
	if err := d.QueryRow(`SELECT 1 FROM outbox WHERE task_id=?`, want).Scan(new(int)); err != nil {
		t.Fatalf("重派行 %s 缺席(nil probe = 现行为): %v", want, err)
	}
}
