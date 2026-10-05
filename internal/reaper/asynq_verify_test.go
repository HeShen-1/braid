package reaper

// T17.4(D-2026-10-04-t174)分支 B asynq 活性对拍测试:asynq 积压 >
// DISPATCH_STALE 时 outbox 死证据会说谎(任务仍在队列),重派 = 用新
// TaskID 制造第二活副本(D-t25#10 残余窗)。基础四臂:活让位 / 死重派 /
// 探测失败 fail-open / probe 缺席降级;T18.7 观测缝隙收口补锚:哨兵发射
// 谓词只认当前 attempt / 活事件闸先行不触发 probe / queue 空串回落 /
// probe 恒在事务外(锁传感)。

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
	if got := testutil.ToFloat64(set.Reaper.AsynqProbeUnreachable); got != 0 {
		t.Fatalf("asynq_probe_unreachable = %v, want 0(活判不是 fail-open)", got)
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
	if got := testutil.ToFloat64(set.Reaper.AsynqProbeUnreachable); got != 0 {
		t.Fatalf("asynq_probe_unreachable = %v, want 0(死判核验不是 fail-open)", got)
	}
}

// TestRedispatchFailOpenOnProbeError:探测不可达 → fail-open(D-t25#2 同族):
// 重派照旧(DispatchStale 有界窗语义),零让位计数;T18.7:fail-open 面
// 从仅 warn 升格带计数(D-t174 已知边界收口)。
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
	if got := testutil.ToFloat64(set.Reaper.AsynqProbeUnreachable); got != 1 {
		t.Fatalf("asynq_probe_unreachable = %v, want 1(fail-open 面必须计数,T18.7)", got)
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

// TestRedispatchDeferredProbesRowQueue(review HIGH 修):对拍队列必须取
// outbox 死证据行的 queue 列,不是 reaper 自己的 cfg.Queue——attempt-1
// 任务由 api/worker 调度器发往常量 default,BRAID_REAPER_QUEUE 隔离部署下
// 查错队列 = NotFound 被映射成干净死判、闸静默失效(变异:probe 调用回
// cfg.Queue → lastQ 断言红)。
func TestRedispatchDeferredProbesRowQueue(t *testing.T) {
	set, _ := metrics.NewSet()
	probe := &fakeAsynqProbe{alive: true}
	r, d := newReaperDeps(t, testCfg(), func(dp *Deps) {
		dp.Metrics = set.Reaper
		dp.AsynqProbe = probe
	})
	s := seedReap(t, d, 5*time.Minute, 0, 3)
	if _, err := d.Exec(`UPDATE node_execution SET attempts=2 WHERE id=?`, s.nodeExecID); err != nil {
		t.Fatalf("seed branch-b attempts: %v", err)
	}
	// 死证据行落在外来队列(api/worker 调度器形态,≠ reaper 的 reap-test)。
	curTask := fmt.Sprintf("att:%d:2", s.nodeExecID)
	payload, err := json.Marshal(scheduler.NodeTaskPayload{
		ExecutionID: s.execID, NodeExecutionID: s.nodeExecID, AttemptNo: 2,
	})
	if err != nil {
		t.Fatalf("marshal node task payload: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO outbox
	  (id, execution_id, event_type, task_id, queue, payload, status, dispatched_at, available_at)
	  VALUES (?, ?, 'node.task_enqueue', ?, 'default', ?, 'DISPATCHED', NOW(3) - INTERVAL 5 MINUTE, NOW(3))`,
		nextID(t), s.execID, curTask, payload); err != nil {
		t.Fatalf("seed foreign-queue dispatch event: %v", err)
	}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if probe.lastQ != "default" {
		t.Fatalf("probe queue = %q, want %q(死证据行的 queue 列)", probe.lastQ, "default")
	}
	if probe.lastTask != curTask {
		t.Fatalf("probe task = %q, want %q", probe.lastTask, curTask)
	}
	if got := testutil.ToFloat64(set.Reaper.AsynqAliveDefers); got != 1 {
		t.Fatalf("asynq_alive_defers = %v, want 1(外来队列活任务照常让位)", got)
	}
}

// TestRedispatchNoProbeForOlderAttemptStaleRow(T18.7 补锚):哨兵谓词只认
// 「当前 attempt」的陈旧 DISPATCHED 行——旧 attempt 的陈旧行不是当前
// 活性对拍的证据(它对应的 attempt 已收口,重派不制造第二副本),probe
// 必须零调用、分支 B 直接照旧重派(变异:谓词丢 task_id 等值条件扫全
// att:<n>:% → probe.calls 红)。
func TestRedispatchNoProbeForOlderAttemptStaleRow(t *testing.T) {
	set, _ := metrics.NewSet()
	probe := &fakeAsynqProbe{alive: true}
	r, d := newReaperDeps(t, testCfg(), func(dp *Deps) {
		dp.Metrics = set.Reaper
		dp.AsynqProbe = probe
	})
	s := seedReap(t, d, 5*time.Minute, 0, 3)
	// 分支 B(attempts=2,当前号无行);陈旧行落旧 attempt 号(att:<n>:1,
	// 即 seedReap 种下的 attempt 1 的派发事件)——非当前号,不触发哨兵。
	if _, err := d.Exec(`UPDATE node_execution SET attempts=2 WHERE id=?`, s.nodeExecID); err != nil {
		t.Fatalf("seed branch-b attempts: %v", err)
	}
	payload, err := json.Marshal(scheduler.NodeTaskPayload{
		ExecutionID: s.execID, NodeExecutionID: s.nodeExecID, AttemptNo: 1,
	})
	if err != nil {
		t.Fatalf("marshal node task payload: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO outbox
	  (id, execution_id, event_type, task_id, queue, payload, status, dispatched_at, available_at)
	  VALUES (?, ?, 'node.task_enqueue', ?, 'reap-test', ?, 'DISPATCHED', NOW(3) - INTERVAL 5 MINUTE, NOW(3))`,
		nextID(t), s.execID, fmt.Sprintf("att:%d:1", s.nodeExecID), payload); err != nil {
		t.Fatalf("seed older-attempt stale dispatch event: %v", err)
	}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if probe.calls != 0 {
		t.Fatalf("probe calls = %d, want 0(旧 attempt 陈旧行不触发活性对拍)", probe.calls)
	}
	want := fmt.Sprintf("att:%d:3", s.nodeExecID)
	if err := d.QueryRow(`SELECT 1 FROM outbox WHERE task_id=?`, want).Scan(new(int)); err != nil {
		t.Fatalf("重派行 %s 缺席(无当前号死证据必须照旧重派): %v", want, err)
	}
}

// TestRedispatchLiveGateDefersWithoutProbe(T18.7 补锚):活事件闸(PENDING/
// 新鲜 DISPATCHED)先于哨兵判——派发在途时本 tick 让位,probe 零调用。
// 双触发种子(对抗审计修正:单 PENDING 行下两谓词互斥,换序变异行为
// 惰性杀不死):当前 attempt 的陈旧 DISPATCHED 行(哨兵可命中)+ 旧
// attempt 的 PENDING 行(活闸可命中,LIKE 前缀跨 attempt 号)——正是
// relay 积压下的真实形态(旧轮重派事件滞留 PENDING)。正确序:活闸先
// 让位(零 probe 零写);变异「哨兵判移到活闸前」:哨兵命中当前号陈旧
// 行发射 → 事务外 probe 被调 → calls 红。
func TestRedispatchLiveGateDefersWithoutProbe(t *testing.T) {
	set, _ := metrics.NewSet()
	probe := &fakeAsynqProbe{alive: false}
	r, d := newReaperDeps(t, testCfg(), func(dp *Deps) {
		dp.Metrics = set.Reaper
		dp.AsynqProbe = probe
	})
	s := seedReap(t, d, 5*time.Minute, 0, 3)
	seedBranchBStaleRows(t, d, s) // 分支 B + 当前号(att:n:2)陈旧 DISPATCHED 行
	// 旧 attempt(att:n:1)的 PENDING 重派事件:活闸证据,在途未决。
	payload, err := json.Marshal(scheduler.NodeTaskPayload{
		ExecutionID: s.execID, NodeExecutionID: s.nodeExecID, AttemptNo: 1,
	})
	if err != nil {
		t.Fatalf("marshal node task payload: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO outbox
	  (id, execution_id, event_type, task_id, queue, payload, status, available_at)
	  VALUES (?, ?, 'node.task_enqueue', ?, 'reap-test', ?, 'PENDING', NOW(3))`,
		nextID(t), s.execID, fmt.Sprintf("att:%d:1", s.nodeExecID), payload); err != nil {
		t.Fatalf("seed pending dispatch event: %v", err)
	}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if probe.calls != 0 {
		t.Fatalf("probe calls = %d, want 0(活事件闸先行让位,无需 asynq 对拍)", probe.calls)
	}
	var attempts int
	var status string
	if err := d.QueryRow(`SELECT attempts, status FROM node_execution WHERE id=?`, s.nodeExecID).
		Scan(&attempts, &status); err != nil {
		t.Fatalf("read node: %v", err)
	}
	if attempts != 2 || status != "EXECUTING" {
		t.Fatalf("node = (%d, %s), want (2, EXECUTING)(活闸让位零动作)", attempts, status)
	}
	if rows := fetchOutbox(t, d, s.execID, "node.task_enqueue"); len(rows) != 2 {
		t.Fatalf("task_enqueue 行数 = %d, want 2(零重派:陈旧行 + PENDING 行原样)", len(rows))
	}
}

// TestRedispatchProbeQueueEmptyFallsBackToCfg(T18.7 补锚):死证据行 queue
// 列为空串(COALESCE 兜底形态)→ 对拍防御性回落 reaper 自己的 cfg.Queue
// (变异:回落分支删除 → lastQ 红,空串直查 asynq = NotFound 假死判)。
func TestRedispatchProbeQueueEmptyFallsBackToCfg(t *testing.T) {
	set, _ := metrics.NewSet()
	probe := &fakeAsynqProbe{alive: true}
	r, d := newReaperDeps(t, testCfg(), func(dp *Deps) {
		dp.Metrics = set.Reaper
		dp.AsynqProbe = probe
	})
	s := seedReap(t, d, 5*time.Minute, 0, 3)
	curTask := fmt.Sprintf("att:%d:2", s.nodeExecID)
	payload, err := json.Marshal(scheduler.NodeTaskPayload{
		ExecutionID: s.execID, NodeExecutionID: s.nodeExecID, AttemptNo: 2,
	})
	if err != nil {
		t.Fatalf("marshal node task payload: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO outbox
	  (id, execution_id, event_type, task_id, queue, payload, status, dispatched_at, available_at)
	  VALUES (?, ?, 'node.task_enqueue', ?, '', ?, 'DISPATCHED', NOW(3) - INTERVAL 5 MINUTE, NOW(3))`,
		nextID(t), s.execID, curTask, payload); err != nil {
		t.Fatalf("seed empty-queue dispatch event: %v", err)
	}
	if _, err := d.Exec(`UPDATE node_execution SET attempts=2 WHERE id=?`, s.nodeExecID); err != nil {
		t.Fatalf("seed branch-b attempts: %v", err)
	}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if probe.lastQ != "reap-test" {
		t.Fatalf("probe queue = %q, want %q(空串回落 cfg.Queue)", probe.lastQ, "reap-test")
	}
	if probe.lastTask != curTask {
		t.Fatalf("probe task = %q, want %q", probe.lastTask, curTask)
	}
}

// lockSensingProbe 是「probe 恒在事务外」结构纪律的传感替身(T18.7 补
// 锚,D-t174 已知边界:该纪律原无直接测试锚):探测时从第二连接对被
// reaper 处置事务锁定的 node_execution 行做 FOR UPDATE——正确形态下
// 哨兵已空回滚、行锁已释放,加锁即刻成功;若实现漂移为「事务内探测」,
// 行锁仍在 → innodb_lock_wait_timeout=1s 超时 → lockHeld 置位,测试红。
type lockSensingProbe struct {
	db       *sql.DB
	nodeID   uint64
	calls    int
	lockHeld bool
	senseErr error
}

func (p *lockSensingProbe) TaskAlive(ctx context.Context, _, _ string) (bool, error) {
	p.calls++
	conn, err := p.db.Conn(ctx)
	if err != nil {
		p.senseErr = err
		return false, err
	}
	defer func() { _ = conn.Close() }() //nolint:errcheck // 探测路径,关闭错误无处置义
	if _, err := conn.ExecContext(ctx, `SET SESSION innodb_lock_wait_timeout=1`); err != nil {
		p.senseErr = err
		return false, err
	}
	var one int
	if err := conn.QueryRowContext(ctx,
		`SELECT 1 FROM node_execution WHERE id=? FOR UPDATE`, p.nodeID).Scan(&one); err != nil {
		// 锁等待超时 = 行锁仍被处置事务持有 = probe 被挪进了事务内。
		p.lockHeld = true
		p.senseErr = err
		return false, err
	}
	return true, nil
}

// TestRedispatchProbeRunsOutsideTransaction(T18.7 补锚):哨兵空回滚后对拍
// ——Redis 调用永不持行锁(CAPACITY §5 锁窗纪律;D-t174 决策 2 的结构
// 半边)。传感失败时 fail-open 语义仍收敛(让位/重派不受影响),真正的
// 判据是 lockHeld 必须为假。
func TestRedispatchProbeRunsOutsideTransaction(t *testing.T) {
	set, _ := metrics.NewSet()
	probe := &lockSensingProbe{}
	r, d := newReaperDeps(t, testCfg(), func(dp *Deps) {
		dp.Metrics = set.Reaper
		probe.db = dp.DB
		dp.AsynqProbe = probe
	})
	s := seedReap(t, d, 5*time.Minute, 0, 3)
	probe.nodeID = s.nodeExecID
	seedBranchBStaleRows(t, d, s)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if probe.calls != 1 {
		t.Fatalf("probe calls = %d, want 1", probe.calls)
	}
	if probe.lockHeld {
		t.Fatalf("probe 在处置事务内被调用(行锁仍在: %v)——必须事务外对拍(CAPACITY §5)", probe.senseErr)
	}
	if got := testutil.ToFloat64(set.Reaper.AsynqAliveDefers); got != 1 {
		t.Fatalf("asynq_alive_defers = %v, want 1(传感探测活判 → 让位)", got)
	}
}
