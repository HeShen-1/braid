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
	want := map[NodeState][]NodeState{
		NodePending:         {NodeExecuting, NodeSkipped, NodeCancelled},
		NodeExecuting:       {NodeSucceeded, NodeFailed, NodeWaitingApproval, NodePending, NodeCancelled},
		NodeWaitingApproval: {NodeExecuting, NodeCancelled},
		NodeSucceeded:       {},
		NodeFailed:          {},
		NodeSkipped:         {},
		NodeCancelled:       {},
	}
	if len(want) != len(nodeStates) {
		t.Fatalf("期望表状态数 %d ≠ nodeStates %d(词表漂移)", len(want), len(nodeStates))
	}
	for _, from := range nodeStates {
		if len(want[from]) != len(nodeEdges[from]) { // 终态在 nodeEdges 缺席 = 0 出边
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
			if from.Terminal() && got {
				t.Errorf("终态 %s 不该有出边 →%s", from, to)
			}
		}
	}
}

func TestWorkflowTransitionMatrix(t *testing.T) {
	want := map[WorkflowState][]WorkflowState{
		WorkflowPending:         {WorkflowRunning, WorkflowFailed, WorkflowCancelled},
		WorkflowRunning:         {WorkflowWaitingApproval, WorkflowSucceeded, WorkflowFailed, WorkflowCancelled},
		WorkflowWaitingApproval: {WorkflowRunning, WorkflowCancelled},
		WorkflowSucceeded:       {},
		WorkflowFailed:          {},
		WorkflowCancelled:       {},
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
		{WorkflowSucceeded, WorkflowRunning},         // 终态无出边
		{WorkflowCancelled, WorkflowFailed},          // 终态无出边
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
