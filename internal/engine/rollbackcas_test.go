package engine

import (
	"errors"
	"testing"
	"time"
)

// T9.2 回滚三原语(ResetNodeToPending / RerunNode / RestartExecution,
// D-2026-09-28-t92-rollback-same-branch)的表驱动 CAS 契约面:from 集合、
// 全清列清单、attempts_base 基线落定、0 行 Conflict/NotFound 分面。
// 种状态一律走既有 helper(真实边),INSERT 只承担 PENDING 起点——
// 「矩阵是唯一真源」在测试侧的同款纪律。

// driveNode 把 PENDING 种子行推进到 want 状态(经真实边;快照/产出/终局列
// 尽量携带非空值,使 Reset/Rerun 的清列断言有牙)。want=CANCELLED 走
// EXECUTING 路径(快照在行,回滚可读回);自 PENDING 连坐的 CANCELLED
// (快照 NULL)由独立测试覆盖分流语义。
func driveNode(t *testing.T, f *fixture, id uint64, want NodeState) {
	t.Helper()
	deadline := time.Now().Add(time.Hour)
	snap := []byte(`{"q":"为什么天空是蓝色的"}`)
	if want == NodeSkipped {
		if _, err := SkipNode(f.ctx(), f.db, id, 1); err != nil {
			t.Fatalf("skip: %v", err)
		}
		return
	}
	if want == NodePending {
		return // 种子即 PENDING,零动作
	}
	if _, err := ClaimNode(f.ctx(), f.db, id, 1, ClaimInput{
		WorkerID: "w1", DeadlineAt: deadline, InputSnapshot: snap,
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	ver := scanNode(t, f.db, id).Version
	var err error
	switch want {
	case NodeExecuting:
		return // claim 后即 EXECUTING
	case NodeWaitingApproval:
		_, err = SuspendNode(f.ctx(), f.db, id, ver, "w1", deadline)
	case NodeSucceeded:
		_, err = SucceedNode(f.ctx(), f.db, id, ver, "w1", []byte(`{"a":"答案"}`))
	case NodeFailed:
		_, err = FailNode(f.ctx(), f.db, id, ver, "w1", "llm_rate_limited", "boom")
	case NodeCancelled:
		_, err = CancelNode(f.ctx(), f.db, id, ver)
	default:
		t.Fatalf("driveNode: 不支持的目标态 %s", want)
	}
	if err != nil {
		t.Fatalf("drive to %s: %v", want, err)
	}
}

func TestResetNodeToPending(t *testing.T) {
	for _, tc := range []struct {
		from    NodeState
		wantErr error // nil = 成功
	}{
		{from: NodeSucceeded},
		{from: NodeFailed},
		{from: NodeSkipped},
		{from: NodeCancelled},
		{from: NodePending, wantErr: ErrConflict},         // 重开边仅自终态
		{from: NodeExecuting, wantErr: ErrConflict},       // EXECUTING 重置必须先走收口/取消
		{from: NodeWaitingApproval, wantErr: ErrConflict}, // WA 残留由守卫 409,原语不可达
	} {
		t.Run(string(tc.from), func(t *testing.T) {
			f := newFixture(t)
			id := f.node()
			driveNode(t, f, id, tc.from)
			before := scanNode(t, f.db, id)
			newVer, err := ResetNodeToPending(f.ctx(), f.db, id, before.Version)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("reset: %v", err)
			}
			r := scanNode(t, f.db, id)
			if r.Status != NodePending {
				t.Fatalf("status = %s, want PENDING", r.Status)
			}
			if r.Version != newVer || r.Version != before.Version+1 {
				t.Fatalf("version = %d (newVer %d), want %d", r.Version, newVer, before.Version+1)
			}
			// 全清列清单(D-t92 决策 2):快照/租约/产出/终局错误/截止/终态时刻/
			// 升级锚全 NULL——PENDING 单向不变量 + 新轮干净起点。
			for name, valid := range map[string]bool{
				"worker_id":  r.WorkerID.Valid,
				"snapshot":   r.Snapshot.Valid,
				"output":     r.Output.Valid,
				"deadline":   r.Deadline.Valid,
				"error_code": r.ErrorCode.Valid,
				"finished":   r.FinishedAt.Valid,
				"escalated":  r.EscalatedAt.Valid,
			} {
				if valid {
					t.Errorf("%s 未清空", name)
				}
			}
			// attempts 不动 + 基线 = 当轮 attempts(预算三判定点的数据源);
			// started_at 保留(首次 EXECUTING 跨轮回滚不变)。
			if r.Attempts != before.Attempts {
				t.Fatalf("attempts = %d, want 不动 %d", r.Attempts, before.Attempts)
			}
			if r.AttemptsBase != before.Attempts {
				t.Fatalf("attempts_base = %d, want %d(当轮 attempts)", r.AttemptsBase, before.Attempts)
			}
			if r.StartedAt.Valid != before.StartedAt.Valid {
				t.Fatalf("started_at 应原样保留(before valid=%v, got %v;SKIPPED 行本就未起跑)",
					before.StartedAt.Valid, r.StartedAt.Valid)
			}
		})
	}
}

// 自 PENDING 连坐的 CANCELLED 行快照恒 NULL:Reset 可达(重评估语义),
// Rerun 不可达(validSnapshot 拒绝 = 调用方必须分流的编译期外防线)。
func TestCancelledFromPendingSplit(t *testing.T) {
	f := newFixture(t)
	id := f.node()
	if _, err := CancelNode(f.ctx(), f.db, id, 1); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	before := scanNode(t, f.db, id)
	if before.Snapshot.Valid {
		t.Fatalf("自 PENDING 取消行快照应为 NULL")
	}
	if _, err := RerunNode(f.ctx(), f.db, id, before.Version, ClaimInput{
		WorkerID: "w", InputSnapshot: nil,
	}); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("rerun err = %v, want ErrInvalidSnapshot(分流信号)", err)
	}
	if _, err := ResetNodeToPending(f.ctx(), f.db, id, before.Version); err != nil {
		t.Fatalf("reset(重评估路径): %v", err)
	}
}

func TestResetNodeToPendingVersionMismatch(t *testing.T) {
	f := newFixture(t)
	id := f.node()
	driveNode(t, f, id, NodeFailed)
	if _, err := ResetNodeToPending(f.ctx(), f.db, id, 999); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if _, err := ResetNodeToPending(f.ctx(), f.db, newID(), 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRerunNode(t *testing.T) {
	for _, tc := range []struct {
		from    NodeState
		wantErr error
	}{
		{from: NodeSucceeded},
		{from: NodeFailed},
		{from: NodeCancelled},
		{from: NodeSkipped, wantErr: ErrConflict}, // SKIPPED 重跑必经 Reset 重评估,无 EXECUTING 直达边
		{from: NodePending, wantErr: ErrConflict},
	} {
		t.Run(string(tc.from), func(t *testing.T) {
			f := newFixture(t)
			id := f.node()
			driveNode(t, f, id, tc.from)
			before := scanNode(t, f.db, id)
			// 读回旧轮快照 = 「以 input_snapshot 重跑」的输入源(D-t92 决策 2b);
			// SKIPPED/PENDING 行快照 NULL,用哑快照使「矩阵边拒绝」先于
			// 快照校验被观察到(被拒路径的输入值无关紧要)。
			snap, err := ReadNodeInput(f.ctx(), f.db, id)
			if err != nil {
				t.Fatalf("read input: %v", err)
			}
			if snap == nil {
				if tc.wantErr == nil {
					t.Fatalf("%s 行快照应为 NOT NULL", tc.from)
				}
				snap = []byte(`{}`)
			}
			deadline := time.Now().Add(30 * time.Minute)
			newVer, err := RerunNode(f.ctx(), f.db, id, before.Version, ClaimInput{
				WorkerID: "w-rerun", DeadlineAt: deadline, InputSnapshot: snap,
			})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("rerun: %v", err)
			}
			r := scanNode(t, f.db, id)
			if r.Status != NodeExecuting {
				t.Fatalf("status = %s, want EXECUTING", r.Status)
			}
			if r.Version != newVer || r.Version != before.Version+1 {
				t.Fatalf("version = %d, want %d", r.Version, before.Version+1)
			}
			// attempts+1 = 本语句即新轮 claim;基线 = 旧轮总数(同轮预算从 1 计)。
			if r.Attempts != before.Attempts+1 {
				t.Fatalf("attempts = %d, want %d", r.Attempts, before.Attempts+1)
			}
			if r.AttemptsBase != before.Attempts {
				t.Fatalf("attempts_base = %d, want %d", r.AttemptsBase, before.Attempts)
			}
			if !r.WorkerID.Valid || r.WorkerID.String != "w-rerun" {
				t.Fatalf("worker_id = %v, want w-rerun", r.WorkerID)
			}
			jsonEq(t, r.Snapshot, snap) // 读回值重写(单写者契约第三写者)
			if r.Output.Valid || r.ErrorCode.Valid || r.FinishedAt.Valid || r.EscalatedAt.Valid {
				t.Errorf("output/error/finished/escalated 未清空")
			}
			if !r.Deadline.Valid {
				t.Errorf("deadline 未设置")
			}
			if !r.StartedAt.Valid {
				t.Fatalf("started_at 不应被清")
			}
		})
	}
}

func TestRerunNodeRejectsInvalidSnapshot(t *testing.T) {
	f := newFixture(t)
	id := f.node()
	driveNode(t, f, id, NodeFailed)
	if _, err := RerunNode(f.ctx(), f.db, id, scanNode(t, f.db, id).Version, ClaimInput{
		WorkerID: "w", InputSnapshot: []byte(`{bad`),
	}); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("err = %v, want ErrInvalidSnapshot", err)
	}
}

func TestRestartExecution(t *testing.T) {
	f := newFixture(t)
	// PENDING→FAILED 是既有合法边(reconciler 判死形态);终态列组由
	// FinishExecution 写就,Restart 的清列断言有牙。
	if err := FinishExecution(f.ctx(), f.db, f.execID, WorkflowFailed, nil, "reconciler_timeout", "boom"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	before := scanWf(t, f)
	if !before.finishedAt.Valid || !before.errorCode.Valid {
		t.Fatalf("种子行应携带终态列组")
	}
	if err := RestartExecution(f.ctx(), f.db, f.execID); err != nil {
		t.Fatalf("restart: %v", err)
	}
	r := scanWf(t, f)
	if r.status != WorkflowRunning {
		t.Fatalf("status = %s, want RUNNING", r.status)
	}
	if r.outputs.Valid || r.errorCode.Valid || r.finishedAt.Valid {
		t.Errorf("终态列组未清空(outputs/error_code/finished_at)")
	}
	if !r.startedAt.Valid {
		t.Fatalf("started_at 应被 IFNULL 补齐(未起跑执行的复活=首次起跑,无 NULL 洞)")
	}
	// 幂等面:非 FAILED 来源一律 Conflict(含并发双回滚败者);缺行 NotFound。
	if err := RestartExecution(f.ctx(), f.db, f.execID); !errors.Is(err, ErrConflict) {
		t.Fatalf("重复 restart err = %v, want ErrConflict", err)
	}
	if err := RestartExecution(f.ctx(), f.db, newID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("缺行 err = %v, want ErrNotFound", err)
	}
}
