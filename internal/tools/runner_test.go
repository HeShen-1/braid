package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// failTool 总是返回固定错误(业务失败路径)。
type failTool struct{}

func (failTool) Name() string { return "always_fail" }
func (failTool) Run(_ context.Context, _ Call) (Result, error) {
	return Result{}, errors.New("tool inner failure detail")
}

// badOutputTool 返回非 JSON 对象(契约破坏路径)。
type badOutputTool struct{}

func (badOutputTool) Name() string { return "bad_out" }
func (badOutputTool) Run(_ context.Context, _ Call) (Result, error) {
	return Result{Output: json.RawMessage(`[1,2]`)}, nil
}

// spyTool 记录收到的 Call(token/幂等键传递面的断言锚)。
type spyTool struct {
	got Call
}

func (s *spyTool) Name() string { return "spy" }
func (s *spyTool) Run(_ context.Context, call Call) (Result, error) {
	s.got = call
	return Result{Output: json.RawMessage(`{"ok":true}`)}, nil
}

func registryOf(t *testing.T, tools ...Tool) *Registry {
	t.Helper()
	r, err := NewRegistry(tools...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

func callCodeOf(t *testing.T, err error) string {
	t.Helper()
	var ce *CallError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *CallError", err)
	}
	return ce.Code
}

func TestRunnerHappyPath(t *testing.T) {
	spy := &spyTool{}
	r := NewLocalRunner(registryOf(t, spy), NewTokenIssuer(nil, 0, nil))
	res, err := r.Run(context.Background(), RunRequest{
		Tool: "spy", Inputs: map[string]any{"k": "v"},
		NodeExecID: 5, AttemptNo: 2, IdempotencyKey: "att:5:2",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(res.Output) != `{"ok":true}` {
		t.Fatalf("Output = %s", res.Output)
	}
	// 传递面:token 与幂等键必须到达工具(per-call 签发、attempt 键透传)。
	if spy.got.Token == "" {
		t.Fatal("tool received empty scoped token")
	}
	if spy.got.IdempotencyKey != "att:5:2" {
		t.Fatalf("IdempotencyKey = %q", spy.got.IdempotencyKey)
	}
	if spy.got.Inputs["k"] != "v" {
		t.Fatalf("Inputs = %v", spy.got.Inputs)
	}
}

func TestRunnerToolNotFound(t *testing.T) {
	r := NewLocalRunner(registryOf(t), NewTokenIssuer(nil, 0, nil))
	_, err := r.Run(context.Background(), RunRequest{Tool: "ghost", NodeExecID: 1, AttemptNo: 1})
	if code := callCodeOf(t, err); code != CodeToolNotFound {
		t.Fatalf("code = %q, want %q", code, CodeToolNotFound)
	}
}

func TestRunnerExpiredTokenRejected(t *testing.T) {
	// 验收(ROADMAP T3.2):token 过期后工具调用被拒。构造一枚已过期 token
	// 注入 RunRequest(时钟推进后的重放等价物),门必须拒且不触工具。
	spy := &spyTool{}
	clk := &fakeClock{now: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}
	iss := NewTokenIssuer([]byte("k"), 30*time.Second, clk.Now)
	expired, err := iss.Issue(TokenScope{NodeExecID: 9, AttemptNo: 4, Tool: "spy"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	clk.now = clk.now.Add(time.Minute) // 越过 exp

	now := &fakeClock{now: clk.now}
	r := NewLocalRunner(registryOf(t, spy), NewTokenIssuer([]byte("k"), 30*time.Second, now.Now))
	_, err = r.Run(context.Background(), RunRequest{
		Tool: "spy", Token: expired, NodeExecID: 9, AttemptNo: 4,
	})
	if code := callCodeOf(t, err); code != CodeTokenExpired {
		t.Fatalf("code = %q, want %q", code, CodeTokenExpired)
	}
	if spy.got.Token != "" {
		t.Fatal("tool was dispatched despite expired token")
	}
}

func TestRunnerTamperedTokenRejected(t *testing.T) {
	spy := &spyTool{}
	iss := NewTokenIssuer([]byte("k"), time.Minute, nil)
	r := NewLocalRunner(registryOf(t, spy), iss)
	_, err := r.Run(context.Background(), RunRequest{
		Tool: "spy", Token: "broken.sig", NodeExecID: 9, AttemptNo: 4,
	})
	if code := callCodeOf(t, err); code != CodeTokenInvalid {
		t.Fatalf("code = %q, want %q", code, CodeTokenInvalid)
	}
}

func TestRunnerToolFailure(t *testing.T) {
	r := NewLocalRunner(registryOf(t, failTool{}), NewTokenIssuer(nil, 0, nil))
	_, err := r.Run(context.Background(), RunRequest{Tool: "always_fail", NodeExecID: 1, AttemptNo: 1})
	var ce *CallError
	if !errors.As(err, &ce) || ce.Code != CodeToolFailed {
		t.Fatalf("err = %v, want CallError(%s)", err, CodeToolFailed)
	}
	if !strings.Contains(ce.Error(), "tool inner failure detail") {
		t.Fatalf("err = %v, want inner detail preserved", err)
	}
}

func TestRunnerBadOutput(t *testing.T) {
	r := NewLocalRunner(registryOf(t, badOutputTool{}), NewTokenIssuer(nil, 0, nil))
	_, err := r.Run(context.Background(), RunRequest{Tool: "bad_out", NodeExecID: 1, AttemptNo: 1})
	if code := callCodeOf(t, err); code != CodeBadToolOutput {
		t.Fatalf("code = %q, want %q", code, CodeBadToolOutput)
	}
}

// blockerTool 阻塞直到 ctx 取消或通道放行(基础设施失败分类的测试桩)。
type blockerTool struct{ release <-chan struct{} }

func (blockerTool) Name() string { return "blocker" }
func (b blockerTool) Run(ctx context.Context, _ Call) (Result, error) {
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-b.release:
		return Result{}, nil
	}
}

func TestRunnerCtxCancelIsInfraError(t *testing.T) {
	// ctx 取消 = 基础设施失败:裸 error 透传(不包 CallError),nodes 侧据此
	// 走 error 返回留 RUNNING,asynq 重试接管(与 LLM 执行器同语义)。
	blk := blockerTool{release: make(chan struct{})}
	r := NewLocalRunner(registryOf(t, blk), NewTokenIssuer(nil, 0, nil))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := r.Run(ctx, RunRequest{Tool: "blocker", NodeExecID: 1, AttemptNo: 1})
	if err == nil {
		t.Fatal("cancelled Run returned nil")
	}
	var ce *CallError
	if errors.As(err, &ce) {
		t.Fatalf("ctx cancel wrapped as CallError(%s)", ce.Code)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled in chain", err)
	}
}
