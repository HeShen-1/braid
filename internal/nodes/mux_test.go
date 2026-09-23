package nodes

// Mux 单测(零 DB 零网络):node-type 路由、no_executor 终态语义、定义读取
// 失败的基础设施/数据缺陷分流。执行编排真链路归 worker 包 e2e(T3.6 验收)。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"braid/internal/engine"
	"braid/internal/engine/dsl"
	"braid/internal/worker"
)

type fakeDefs struct {
	node  *dsl.Node
	err   error
	gotID uint64
	calls int
}

func (f *fakeDefs) NodeDefinition(_ context.Context, id uint64) (*dsl.Node, error) {
	f.calls++
	f.gotID = id
	return f.node, f.err
}

type fakeRunner struct {
	gotReq  worker.Request
	gotSpec *dsl.LLMNode
	out     worker.Outcome
	calls   int
}

func (f *fakeRunner) Run(_ context.Context, req worker.Request, spec *dsl.LLMNode) (worker.Outcome, error) {
	f.calls++
	f.gotReq, f.gotSpec = req, spec
	return f.out, nil
}

// fakeApprovalRunner:ApprovalRunner 路由缝桩(同 fakeRunner 形状)。
type fakeApprovalRunner struct {
	gotReq  worker.Request
	gotSpec *dsl.ApprovalNode
	out     worker.Outcome
	calls   int
}

func (f *fakeApprovalRunner) Run(_ context.Context, req worker.Request, spec *dsl.ApprovalNode) (worker.Outcome, error) {
	f.calls++
	f.gotReq, f.gotSpec = req, spec
	return f.out, nil
}

func TestMuxRoutesLLM(t *testing.T) {
	spec := &dsl.LLMNode{Model: "glm-5.3"}
	defs := &fakeDefs{node: &dsl.Node{ID: "n", Type: "llm", LLM: spec}}
	r := &fakeRunner{out: worker.Outcome{Status: engine.AttemptSucceed}}
	m := NewMux(defs, r, &fakeToolExec{}, &fakeApprovalRunner{})

	out, err := m.Execute(context.Background(), worker.Request{NodeExecutionID: 7, AttemptNo: 2})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if defs.gotID != 7 || defs.calls != 1 {
		t.Fatalf("defs got id=%d calls=%d, want 7/1", defs.gotID, defs.calls)
	}
	if r.gotSpec != spec || r.gotReq.NodeExecutionID != 7 || r.gotReq.AttemptNo != 2 {
		t.Fatalf("runner got req=%+v spec=%v", r.gotReq, r.gotSpec)
	}
	if out.Status != engine.AttemptSucceed {
		t.Fatalf("outcome not passed through: %+v", out)
	}
}

// TestMuxRoutesApproval(T4.1):approval 判别指针 → ApprovalRunner,req/spec
// 透传、Outcome 透传(挂起信号在 handler 收口分支消费,路由层不解释)。
func TestMuxRoutesApproval(t *testing.T) {
	spec := &dsl.ApprovalNode{Approvers: []string{"carol"}, Mode: "any", TimeoutHours: 24, OnTimeout: "fail"}
	defs := &fakeDefs{node: &dsl.Node{ID: "b", Type: "approval", Approval: spec}}
	llm := &fakeRunner{}
	tool := &fakeToolExec{}
	appr := &fakeApprovalRunner{out: worker.Outcome{Status: engine.AttemptSucceed, Suspend: true}}
	m := NewMux(defs, llm, tool, appr)

	out, err := m.Execute(context.Background(), worker.Request{NodeExecutionID: 5, AttemptNo: 1})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if appr.calls != 1 || appr.gotSpec != spec || appr.gotReq.NodeExecutionID != 5 {
		t.Fatalf("approval runner got req=%+v spec=%v calls=%d", appr.gotReq, appr.gotSpec, appr.calls)
	}
	if llm.calls != 0 {
		t.Fatalf("llm runner called %d times, want 0", llm.calls)
	}
	if out.Status != engine.AttemptSucceed || !out.Suspend || out.Code != "" || len(out.Output) > 0 {
		t.Fatalf("outcome not passed through: %+v", out)
	}
}

// no_executor 是终态业务失败(GATE 1 语义 b):部署配置错要可见收口,
// 不留 RUNNING 行给 reaper 判死循环重派。T4.1 起 approval 已接线,钉死用
// 无判别指针的合成类型(P1 parallel 前的一切未知类型)。
func TestMuxNoExecutorIsFailedOutcome(t *testing.T) {
	defs := &fakeDefs{node: &dsl.Node{ID: "a", Type: "parallel"}}
	r := &fakeRunner{}
	m := NewMux(defs, r, &fakeToolExec{}, &fakeApprovalRunner{})

	out, err := m.Execute(context.Background(), worker.Request{NodeExecutionID: 1})
	if err != nil {
		t.Fatalf("Execute err = %v, want FAILED outcome not infra error", err)
	}
	if out.Status != engine.AttemptFailed || out.Code != "no_executor" {
		t.Fatalf("outcome = %+v, want FAILED/no_executor", out)
	}
	if r.calls != 0 {
		t.Fatalf("runner called %d times, want 0", r.calls)
	}
}

// 定义行缺失是数据缺陷(业务失败收口可见),DB 读失败才是基础设施错误。
func TestMuxDefNotFoundVsInfra(t *testing.T) {
	m := NewMux(&fakeDefs{err: fmt.Errorf("%w: node_execution 9", engine.ErrNotFound)}, &fakeRunner{}, &fakeToolExec{}, &fakeApprovalRunner{})
	out, err := m.Execute(context.Background(), worker.Request{NodeExecutionID: 9})
	if err != nil {
		t.Fatalf("not-found should be business failure, got infra error %v", err)
	}
	if out.Status != engine.AttemptFailed || out.Code != "node_def_missing" {
		t.Fatalf("outcome = %+v, want FAILED/node_def_missing", out)
	}

	boom := errors.New("db down")
	m2 := NewMux(&fakeDefs{err: boom}, &fakeRunner{}, &fakeToolExec{}, &fakeApprovalRunner{})
	if _, err := m2.Execute(context.Background(), worker.Request{NodeExecutionID: 9}); err == nil {
		t.Fatal("infra read error must surface as error (asynq retry), not outcome")
	}
}

// fakeToolExec:ToolRunner 路由缝桩(mux 层测试不触内层运行时)。
type fakeToolExec struct{}

func (f *fakeToolExec) Run(context.Context, worker.Request, *dsl.ToolNode) (worker.Outcome, error) {
	return worker.Outcome{Status: engine.AttemptFailed, Code: "tool_not_routed"}, nil
}
