package nodes

// ApprovalExecutor 单测(零 DB 零网络):执行面 = 纯挂起决策(T4.1)。
// 事务半边(attempt 收口 + 节点挂起 + 执行镜像)归 scheduler.Suspend,链路
// 半边归 worker 包;本文件只钉「无外部调用、恒定挂起信号、合法形」。

import (
	"context"
	"testing"

	"braid/internal/engine"
	"braid/internal/engine/dsl"
	"braid/internal/worker"
)

func TestApprovalExecutorIsPureSuspendDecision(t *testing.T) {
	e := ApprovalExecutor{}
	spec := &dsl.ApprovalNode{Approvers: []string{"carol"}, Mode: "any", TimeoutHours: 24, OnTimeout: "fail"}
	req := worker.Request{NodeExecutionID: 5, AttemptNo: 1, TaskID: "att:5:1", WorkerID: "w1"}

	out, err := e.Run(context.Background(), req, spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 恒定挂起信号:attempt 成功 + Suspend,无 code/产出/台账/观测
	// (worker.suspendOK 的合法形;挂起事务在 handler 收口路径)。Outcome 含
	// json.RawMessage 不可整体比较,逐字段断言。
	assertSuspendOutcome(t, out)

	// 纯决策无外部调用:ctx 已取消也不改判定(无副作用可取消;取消语义由
	// 下游收口事务承担——那里才触 DB)。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	out2, err := e.Run(cancelled, req, spec)
	if err != nil {
		t.Fatalf("Run(cancelled): %v", err)
	}
	assertSuspendOutcome(t, out2)
}

func assertSuspendOutcome(t *testing.T, out worker.Outcome) {
	t.Helper()
	if out.Status != engine.AttemptSucceed || !out.Suspend {
		t.Fatalf("outcome = %+v, want SUCCEEDED+Suspend", out)
	}
	if out.Code != "" || out.Msg != "" || len(out.Output) > 0 || out.Usage != nil || out.ToolCall != nil {
		t.Fatalf("suspend outcome carries payload fields: %+v", out)
	}
}
