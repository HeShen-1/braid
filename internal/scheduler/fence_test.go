package scheduler

// fence_test.go = T20.1 fence 面(scheduler 编排层)集成测试(t13 栈
// MySQL;栈不可达自动 skip)。口径真源 = D-2026-10-06-t201 决策 3/4:写
// 事务首置加冕校验,失冠(superseded)进程的物化/起跑/收口/取消结构性
// 回滚;冠内路径零行为变化。

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/snowflake"
)

// fenceLeaseNode 是本文件租约测试钉的显式号(与生产池无关节奏,仅在
// 测试 schema 内自洽;避开 api=1/worker=2 内建默认的语义联想)。
const fenceLeaseNode = 311

// newSchedLeased 返回带真实租约的调度器(模拟生产 cmd 装配:Deps.Lease
// = 本进程租约)+ 连接。
func newSchedLeased(t *testing.T) (*Scheduler, *sql.DB, *snowflake.Lease) {
	t.Helper()
	d := sqltest.MustOpenAt(t, testSchema)
	lease, err := snowflake.StartLease(context.Background(), d, snowflake.LeaseConfig{
		Kind:   snowflake.KindWorker,
		Spec:   snowflake.Explicit(fenceLeaseNode),
		Holder: "sched-fence",
	})
	if err != nil {
		t.Fatalf("StartLease: %v", err)
	}
	t.Cleanup(func() { _ = lease.Release(context.Background()) })
	s, err := New(Deps{DB: d, IDs: mustTestIDs(t), Lease: lease, Queue: "sched-test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, d, lease
}

// supersedeLease 直改真源 bump 代际(接管者已易主的等价形态;绕开第二
// StartLease 使失冠进程保持原持有字符串,聚焦 epoch 失配面)。
func supersedeLease(t *testing.T, d *sql.DB) {
	t.Helper()
	if _, err := d.Exec(
		`UPDATE snowflake_node_lease SET fencing_epoch = fencing_epoch + 1 WHERE node_id = ?`,
		fenceLeaseNode); err != nil {
		t.Fatalf("supersede lease: %v", err)
	}
}

// TestSchedulerFenceRejectsSupersededWrites:冠内全链绿(物化/起跑/收口)
// → 易主 → 物化、起跑、收口、取消四写面全部 ErrFenced,行集零变化。
func TestSchedulerFenceRejectsSupersededWrites(t *testing.T) {
	s, d, lease := newSchedLeased(t)
	ctx := context.Background()

	// 冠内控制路径:物化 + 起跑 + 首节点收口全绿(fence 零行为扰动)。
	_, execID := inst(t, s, d, chainDSL, `{"q":"hi"}`)
	if err := s.Start(ctx, execID, "w1"); err != nil {
		t.Fatalf("Start (crowned): %v", err)
	}
	if err := completeNode(t, s, d, execID, "a", engine.AttemptSucceed,
		[]byte(`{"text":"ok"}`)); err != nil {
		t.Fatalf("Complete node a (crowned): %v", err)
	}

	// 易主(zombie 形态:本进程不知情,持有字符串未变,代际已失配)。
	supersedeLease(t, d)
	if err := lease.Fence(ctx, d); !errors.Is(err, snowflake.ErrFenced) {
		t.Fatalf("precondition: fence after supersede = %v, want ErrFenced", err)
	}

	// 物化:ErrFenced,零新执行行。
	defID := seedDef(t, d, chainDSL)
	if _, err := s.Instantiate(ctx, InstantiateParams{DefinitionID: defID, Input: []byte(`{}`), TriggeredBy: "zombie"}); !errors.Is(err, snowflake.ErrFenced) {
		t.Fatalf("Instantiate (superseded): %v, want ErrFenced", err)
	}
	var rows int
	if err := d.QueryRow(`SELECT COUNT(*) FROM workflow_execution WHERE definition_id=?`, defID).Scan(&rows); err != nil {
		t.Fatalf("count executions: %v", err)
	}
	if rows != 0 {
		t.Fatalf("superseded Instantiate materialized %d rows, want 0", rows)
	}

	// 收口:b 已派发(EXECUTING),zombie 的 Complete 被拒且行不动。
	rowB := readNode(t, d, execID, "b")
	if rowB.status != engine.NodeExecuting {
		t.Fatalf("precondition: node b = %s, want EXECUTING", rowB.status)
	}
	if err := completeNode(t, s, d, execID, "b", engine.AttemptSucceed, []byte(`{"text":"no"}`)); !errors.Is(err, snowflake.ErrFenced) {
		t.Fatalf("Complete (superseded): %v, want ErrFenced", err)
	}
	if got := readNode(t, d, execID, "b"); got.status != engine.NodeExecuting {
		t.Fatalf("node b = %s after fenced Complete, want EXECUTING(行集零变化)", got.status)
	}

	// 取消:同一拒面(裁决面事务同源同判)。
	if _, err := s.CancelExecution(ctx, CancelParams{ExecID: execID, WorkerID: "zombie", ActorID: "zombie"}); !errors.Is(err, snowflake.ErrFenced) {
		t.Fatalf("CancelExecution (superseded): %v, want ErrFenced", err)
	}
	if e := readExec(t, d, execID); e.status != engine.WorkflowRunning {
		t.Fatalf("execution = %s after fenced cancel, want RUNNING", e.status)
	}
}

// TestSchedulerFenceAllWriteFaces:对抗审计 HIGH 锚——八个 scheduler 写
// 事务闭包逐一非 vacuous(fence 删除变异 ⇒ 域错误/成功 ≠ ErrFenced ⇒ 红)。
// Start/Rollback/Resume/Suspend/ReapInstanceExhausted 五面此前零锚(Instantiate/
// Complete/Cancel 见主测试);fence 在事务首语句,先于一切域守卫。
func TestSchedulerFenceAllWriteFaces(t *testing.T) {
	s, d, _ := newSchedLeased(t)
	ctx := context.Background()
	_, execID := inst(t, s, d, chainDSL, `{"q":"hi"}`)

	// 冠内控制路径:Start 达 RUNNING(其余四面域形状在冠内即可合法触达
	// 事务——Rollback 守卫在其事务内,Suspend/Resume/ReapInstanceExhausted
	// 的域守卫同样在 fence 之后;本锚只需「事务被打开」,成败语义归各自
	// 域测试)。
	if err := s.Start(ctx, execID, "w1"); err != nil {
		t.Fatalf("Start (crowned): %v", err)
	}
	supersedeLease(t, d)

	if err := s.Start(ctx, execID, "zombie"); !errors.Is(err, snowflake.ErrFenced) {
		t.Fatalf("Start (superseded): %v, want ErrFenced", err)
	}
	if _, err := s.RollbackTo(ctx, RollbackParams{ExecID: execID, WorkerID: "zombie", ActorID: "zombie"}); !errors.Is(err, snowflake.ErrFenced) {
		t.Fatalf("RollbackTo (superseded): %v, want ErrFenced", err)
	}
	if err := s.Resume(ctx, ResumeRequest{ExecID: execID, NodeExecID: 1, DecisionID: 1, AttemptNo: 1, WorkerID: "zombie"}); !errors.Is(err, snowflake.ErrFenced) {
		t.Fatalf("Resume (superseded): %v, want ErrFenced", err)
	}
	if err := s.Suspend(ctx, SuspendRequest{ExecID: execID, NodeExecID: 1, AttemptID: 1, WorkerID: "zombie"}); !errors.Is(err, snowflake.ErrFenced) {
		t.Fatalf("Suspend (superseded): %v, want ErrFenced", err)
	}
	if err := s.ReapInstanceExhausted(ctx, execID, 1, "zombie"); !errors.Is(err, snowflake.ErrFenced) {
		t.Fatalf("ReapInstanceExhausted (superseded): %v, want ErrFenced", err)
	}
}

// TestSchedulerFenceNilLeaseIsLegacyNoOp:Lease = nil(滚动窗/既有测试装配)
// 恒放行——全仓既有测试不设租约即零扰动(nil 接收者语义的回归锚)。
func TestSchedulerFenceNilLeaseIsLegacyNoOp(t *testing.T) {
	s, d := newSched(t)
	if err := s.fence(context.Background(), nil); err != nil {
		t.Fatalf("nil-lease fence: %v, want nil(legacy 放行)", err)
	}
	_, execID := inst(t, s, d, chainDSL, `{"q":"hi"}`)
	if e := readExec(t, d, execID); e.status != engine.WorkflowPending {
		t.Fatalf("execution = %s, want PENDING(nil lease 物化不受扰)", e.status)
	}
}

// TestSchedulerFenceExpiredUncrownedStillWrites:加冕语义(决策 3:夺写只因
// 易主,不因墙钟)——过期未易主的行不夺写,DB 抖动/续期迟滞窗内合法在飞
// 收口照常落库。
func TestSchedulerFenceExpiredUncrownedStillWrites(t *testing.T) {
	s, d, _ := newSchedLeased(t)
	ctx := context.Background()
	_, execID := inst(t, s, d, chainDSL, `{"q":"hi"}`)
	if err := s.Start(ctx, execID, "w1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 过期未易主(懒回收:行还在、holder 未变、无人接管)。
	if _, err := d.Exec(
		`UPDATE snowflake_node_lease SET expires_at = DATE_SUB(NOW(3), INTERVAL 1 SECOND) WHERE node_id = ?`,
		fenceLeaseNode); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if err := completeNode(t, s, d, execID, "a", engine.AttemptSucceed, []byte(`{"text":"ok"}`)); err != nil {
		t.Fatalf("Complete after expiry-without-takeover: %v, want nil(加冕语义)", err)
	}
}
