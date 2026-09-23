package nodes

// ToolExecutor 执行半边(executeCall)的错误分类矩阵与传递面断言:
// CallError 码透传、ctx 分类、未分类兜底、声明抽取、ToolCall 桥、
// 幂等键/输入传递。无栈单测(DB 之后半边,同 LLMExecutor 独立函数模式)。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"braid/internal/engine"
	"braid/internal/engine/dsl"
	"braid/internal/scheduler"
	"braid/internal/tools"
	"braid/internal/worker"
)

// fakeRuntime:ToolRuntime 缝桩(记录请求,回放结果/错误)。
type fakeRuntime struct {
	res    tools.Result
	err    error
	gotReq tools.RunRequest
}

func (f *fakeRuntime) Run(_ context.Context, req tools.RunRequest) (tools.Result, error) {
	f.gotReq = req
	return f.res, f.err
}

func TestToolExecErrorClassification(t *testing.T) {
	tests := []struct {
		name      string
		rtErr     error
		wantCode  string
		wantErrIs error // 非 nil = 期待 error 返回(基础设施失败)而非 Outcome
	}{
		{name: "token_expired", rtErr: &tools.CallError{Code: tools.CodeTokenExpired, Err: errors.New("expired")},
			wantCode: tools.CodeTokenExpired},
		{name: "token_invalid", rtErr: &tools.CallError{Code: tools.CodeTokenInvalid, Err: errors.New("bad sig")},
			wantCode: tools.CodeTokenInvalid},
		{name: "tool_not_found", rtErr: &tools.CallError{Code: tools.CodeToolNotFound, Err: errors.New("nope")},
			wantCode: tools.CodeToolNotFound},
		{name: "tool_failed", rtErr: &tools.CallError{Code: tools.CodeToolFailed, Err: errors.New("exit 1")},
			wantCode: tools.CodeToolFailed},
		{name: "bad_output", rtErr: &tools.CallError{Code: tools.CodeBadToolOutput, Err: errors.New("array")},
			wantCode: tools.CodeBadToolOutput},
		{name: "ctx_cancelled", rtErr: fmt.Errorf("tools: x: %w", context.Canceled), wantErrIs: context.Canceled},
		{name: "ctx_deadline", rtErr: fmt.Errorf("tools: x: %w", context.DeadlineExceeded), wantErrIs: context.DeadlineExceeded},
		{name: "unclassified_becomes_tool_failed", rtErr: errors.New("weird"), wantCode: tools.CodeToolFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &ToolExecutor{Runner: &fakeRuntime{err: tt.rtErr}}
			out, err := e.executeCall(context.Background(),
				workerRequest7(), scheduler.ToolInput{Tool: "t", Inputs: map[string]any{}},
				&dsl.ToolNode{Tool: "t"})
			if tt.wantErrIs != nil {
				if err == nil {
					t.Fatalf("want error %v, got outcome %+v", tt.wantErrIs, out)
				}
				if !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("err = %v, want %v in chain", err, tt.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("want FAILED outcome, got error %v", err)
			}
			if out.Status != engine.AttemptFailed || out.Code != tt.wantCode {
				t.Fatalf("outcome = %s/%s, want FAILED/%s", out.Status, out.Code, tt.wantCode)
			}
		})
	}
}

func TestToolExecHappyPathAndBridge(t *testing.T) {
	rt := &fakeRuntime{res: tools.Result{Output: json.RawMessage(`{"summary":"ok","extra":1}`)}}
	e := &ToolExecutor{Runner: rt}
	req := workerRequest7()
	out, err := e.executeCall(context.Background(), req,
		scheduler.ToolInput{Tool: "file_read", Inputs: map[string]any{"path": "/tmp/x"}},
		&dsl.ToolNode{Tool: "file_read", Outputs: map[string]string{"summary": "string"}})
	if err != nil {
		t.Fatalf("executeCall: %v", err)
	}
	if out.Status != engine.AttemptSucceed {
		t.Fatalf("status = %s", out.Status)
	}
	// 声明抽取:只含声明字段(llm 同款契约)。
	var m map[string]any
	if err := json.Unmarshal(out.Output, &m); err != nil {
		t.Fatalf("output: %v", err)
	}
	if m["summary"] != "ok" || len(m) != 1 {
		t.Fatalf("output = %v, want declared-only {summary}", m)
	}
	// 传递面:工具名/输入/幂等键/attempt 锚全达运行时。
	if rt.gotReq.Tool != "file_read" || rt.gotReq.Inputs["path"] != "/tmp/x" {
		t.Fatalf("runtime got = %+v", rt.gotReq)
	}
	if rt.gotReq.IdempotencyKey != "att:7:1" {
		t.Fatalf("idempotency key = %q", rt.gotReq.IdempotencyKey)
	}
	if rt.gotReq.NodeExecID != 7 || rt.gotReq.AttemptNo != 1 {
		t.Fatalf("scope anchor = %d/%d", rt.gotReq.NodeExecID, rt.gotReq.AttemptNo)
	}
	// 调用观测桥:审计行参数(tool.call.completed)。
	if out.ToolCall == nil || out.ToolCall.Name != "file_read" {
		t.Fatalf("ToolCall = %+v, want {file_read}", out.ToolCall)
	}
}

func TestToolExecDeclaredOutputMissing(t *testing.T) {
	e := &ToolExecutor{Runner: &fakeRuntime{res: tools.Result{Output: json.RawMessage(`{"other":1}`)}}}
	out, err := e.executeCall(context.Background(), workerRequest7(),
		scheduler.ToolInput{Tool: "t", Inputs: map[string]any{}},
		&dsl.ToolNode{Tool: "t", Outputs: map[string]string{"summary": "string"}})
	if err != nil {
		t.Fatalf("executeCall: %v", err)
	}
	if out.Status != engine.AttemptFailed || out.Code != tools.CodeBadToolOutput {
		t.Fatalf("outcome = %s/%s, want FAILED/bad_tool_output", out.Status, out.Code)
	}
}

// TestToolExecToolCallBridge:调用观测桥存活性——executeCall 成功路径产出
// Outcome.ToolCall(审计 tool.call.completed 的参数载体;token 不在桥上是
// 结构事实:ToolCallInfo 字段集在 scheduler 包编译期钉死,零 token 面)。
func TestToolExecToolCallBridge(t *testing.T) {
	e := &ToolExecutor{Runner: &fakeRuntime{
		res: tools.Result{Output: json.RawMessage(`{"summary":"ok"}`)},
	}}
	out, err := e.executeCall(context.Background(), workerRequest7(),
		scheduler.ToolInput{Tool: "t", Inputs: map[string]any{}},
		&dsl.ToolNode{Tool: "t", Outputs: map[string]string{"summary": "string"}})
	if err != nil {
		t.Fatalf("executeCall: %v", err)
	}
	if out.ToolCall == nil || out.ToolCall.Name != "t" {
		t.Fatalf("ToolCall = %+v, want {t}", out.ToolCall)
	}
}

func workerRequest7() worker.Request {
	return worker.Request{ExecutionID: 1, NodeExecutionID: 7, AttemptNo: 1, TaskID: "att:7:1"}
}
