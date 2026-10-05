package engine

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// 矩阵唯一真源的对拍面:手写期望表 vs nodeEdges/wfEdges 全组合。
// DDL 冻结下这是机器可查的契约面——任一侧改边即红。

func TestNodeTransitionMatrix(t *testing.T) {
	// T9.2 重开边(D-2026-09-28-t92-rollback-same-branch 决策 2a):终态出边
	// 仅限显式回滚原语(ResetNodeToPending→PENDING / RerunNode→EXECUTING),
	// 自动路径视角下终态仍不可达新意图。
	want := map[NodeState][]NodeState{
		NodePending:         {NodeExecuting, NodeSkipped, NodeCancelled},
		NodeExecuting:       {NodeSucceeded, NodeFailed, NodeWaitingApproval, NodePending, NodeCancelled},
		NodeWaitingApproval: {NodeExecuting, NodeFailed, NodeCancelled},
		NodeSucceeded:       {NodePending, NodeExecuting},
		NodeFailed:          {NodePending, NodeExecuting},
		NodeSkipped:         {NodePending},
		NodeCancelled:       {NodePending, NodeExecuting},
	}
	if len(want) != len(nodeStates) {
		t.Fatalf("期望表状态数 %d ≠ nodeStates %d(词表漂移)", len(want), len(nodeStates))
	}
	for _, from := range nodeStates {
		if len(want[from]) != len(nodeEdges[from]) { // 期望表缺席 = 0 出边
			t.Fatalf("%s 出边数 %d ≠ %d", from, len(want[from]), len(nodeEdges[from]))
		}
	}
	for _, from := range nodeStates {
		for _, to := range nodeStates {
			got := CanTransitionNode(from, to)
			expect := false
			for _, w := range want[from] {
				if w == to {
					expect = true
					break
				}
			}
			if got != expect {
				t.Errorf("CanTransitionNode(%s→%s)=%v, want %v", from, to, got, expect)
			}
			// T9.2 例外通道外的终态出边纪律:终态只允许 →PENDING(Reset)与
			// →EXECUTING(Rerun,SKIPPED 无此边——被剪枝节点重跑须回 PENDING
			// 重评估),其余终态出边(如 SUCCEEDED→FAILED 直达)仍然非法。
			if from.Terminal() && got && to != NodePending && to != NodeExecuting {
				t.Errorf("终态 %s 的 T9.2 例外外出边 →%s", from, to)
			}
		}
	}
}

func TestWorkflowTransitionMatrix(t *testing.T) {
	want := map[WorkflowState][]WorkflowState{
		WorkflowPending:         {WorkflowRunning, WorkflowFailed, WorkflowCancelled},
		WorkflowRunning:         {WorkflowWaitingApproval, WorkflowSucceeded, WorkflowFailed, WorkflowCancelled},
		WorkflowWaitingApproval: {WorkflowRunning, WorkflowCancelled},
		// T9.2(D-t92 决策 2a):FAILED→RUNNING 仅 RestartExecution(显式回滚);
		// T19.3(D-2026-10-05-t193):SUCCEEDED/CANCELLED 同边扩列。
		WorkflowSucceeded: {WorkflowRunning},
		WorkflowFailed:    {WorkflowRunning},
		WorkflowCancelled: {WorkflowRunning},
	}
	if len(want) != len(wfStates) {
		t.Fatalf("期望表状态数 %d ≠ wfStates %d(词表漂移)", len(want), len(wfStates))
	}
	for _, from := range wfStates {
		if len(want[from]) != len(wfEdges[from]) {
			t.Fatalf("%s 出边数 %d ≠ %d", from, len(want[from]), len(wfEdges[from]))
		}
	}
	for _, from := range wfStates {
		for _, to := range wfStates {
			got := CanTransitionWorkflow(from, to)
			expect := false
			for _, w := range want[from] {
				if w == to {
					expect = true
					break
				}
			}
			if got != expect {
				t.Errorf("CanTransitionWorkflow(%s→%s)=%v, want %v", from, to, got, expect)
			}
		}
	}
}

func TestTerminalSets(t *testing.T) {
	for _, s := range []NodeState{NodeSucceeded, NodeFailed, NodeSkipped, NodeCancelled} {
		if !s.Terminal() {
			t.Errorf("node %s 应为终态", s)
		}
	}
	for _, s := range []NodeState{NodePending, NodeExecuting, NodeWaitingApproval} {
		if s.Terminal() {
			t.Errorf("node %s 不应为终态", s)
		}
	}
	for _, s := range []WorkflowState{WorkflowSucceeded, WorkflowFailed, WorkflowCancelled} {
		if !s.Terminal() {
			t.Errorf("workflow %s 应为终态", s)
		}
	}
}

// execRecorder 断言非法迁移零 DB 往返(矩阵在进程内拒绝)。
type execRecorder struct {
	execs int
}

func (r *execRecorder) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	r.execs++
	return nil, nil
}

func (r *execRecorder) QueryRowContext(context.Context, string, ...any) *sql.Row {
	return nil
}

func TestIllegalTransitionRejectedInProcess(t *testing.T) {
	rec := &execRecorder{}
	for _, tc := range []struct{ from, to WorkflowState }{
		{WorkflowPending, WorkflowSucceeded},         // 未起跑不得直接成功
		{WorkflowPending, WorkflowWaitingApproval},   // 挂起仅自 RUNNING
		{WorkflowSucceeded, WorkflowFailed},          // 终态间直达非法(T19.3 后终态唯一出边 = →RUNNING 回滚复活)
		{WorkflowCancelled, WorkflowFailed},          // 终态间直达非法
		{WorkflowSucceeded, WorkflowCancelled},       // 终态间直达非法
		{WorkflowWaitingApproval, WorkflowSucceeded}, // 终局前必须回 RUNNING
	} {
		err := TransitionWorkflow(t.Context(), rec, 1, tc.from, tc.to)
		if !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("%s→%s: got %v, want ErrIllegalTransition", tc.from, tc.to, err)
		}
	}
	if rec.execs != 0 {
		t.Fatalf("非法迁移触发了 %d 次 DB 写,应零往返", rec.execs)
	}
}
