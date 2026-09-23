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

func TestMuxRoutesLLM(t *testing.T) {
	spec := &dsl.LLMNode{Model: "glm-5.3"}
	defs := &fakeDefs{node: &dsl.Node{ID: "n", Type: "llm", LLM: spec}}
	r := &fakeRunner{out: worker.Outcome{Status: engine.AttemptSucceed}}
	m := NewMux(defs, r)

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

// no_executor 是终态业务失败(GATE 1 语义 b):部署配置错要可见收口,
// 不留 RUNNING 行给 reaper 判死循环重派。
func TestMuxNoExecutorIsFailedOutcome(t *testing.T) {
	defs := &fakeDefs{node: &dsl.Node{ID: "t", Type: "tool", Tool: &dsl.ToolNode{Tool: "http"}}}
	r := &fakeRunner{}
	m := NewMux(defs, r)

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
	m := NewMux(&fakeDefs{err: fmt.Errorf("%w: node_execution 9", engine.ErrNotFound)}, &fakeRunner{})
	out, err := m.Execute(context.Background(), worker.Request{NodeExecutionID: 9})
	if err != nil {
		t.Fatalf("not-found should be business failure, got infra error %v", err)
	}
	if out.Status != engine.AttemptFailed || out.Code != "node_def_missing" {
		t.Fatalf("outcome = %+v, want FAILED/node_def_missing", out)
	}

	boom := errors.New("db down")
	m2 := NewMux(&fakeDefs{err: boom}, &fakeRunner{})
	if _, err := m2.Execute(context.Background(), worker.Request{NodeExecutionID: 9}); err == nil {
		t.Fatal("infra read error must surface as error (asynq retry), not outcome")
	}
}
