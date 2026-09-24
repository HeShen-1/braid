package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// gateSpy 记录授权门收到的调用(参数面断言锚;可注入应答)。
type gateSpy struct {
	calls    []gateCall
	response func(call gateCall) error
}

type gateCall struct {
	NodeExecID uint64
	Tool       string
}

func (g *gateSpy) AuthorizeCall(_ context.Context, nodeExecID uint64, tool string) error {
	c := gateCall{NodeExecID: nodeExecID, Tool: tool}
	g.calls = append(g.calls, c)
	if g.response != nil {
		return g.response(c)
	}
	return nil
}

// TestRunnerAuthzDeniedBeforeDispatch:T5.2 执行时 RBAC 门——授权拒绝
// (ErrCallDenied 哨兵,含外层包装形态)= 业务失败 CodeRBACDenied,且
// 发生在任何 token 签发与工具分派之前(越权调用零触达)。
func TestRunnerAuthzDeniedBeforeDispatch(t *testing.T) {
	spy := &spyTool{}
	gate := &gateSpy{response: func(c gateCall) error {
		return fmt.Errorf("rbac: user lacks role for %q (node_exec %d): %w", c.Tool, c.NodeExecID, ErrCallDenied)
	}}
	r := NewLocalRunner(registryOf(t, spy), NewTokenIssuer(nil, 0, nil), gate)
	_, err := r.Run(context.Background(), RunRequest{
		Tool: "spy", NodeExecID: 7, AttemptNo: 3, IdempotencyKey: "att:7:3",
	})
	if got := callCodeOf(t, err); got != CodeRBACDenied {
		t.Fatalf("code = %q, want %q (err=%v)", got, CodeRBACDenied, err)
	}
	if !errors.Is(err, ErrCallDenied) {
		t.Fatalf("err = %v, want ErrCallDenied preserved in chain", err)
	}
	if !strings.Contains(err.Error(), "user lacks role") {
		t.Fatalf("err = %v, want gate detail preserved", err)
	}
	if spy.got.Token != "" {
		t.Fatal("tool was dispatched despite denied call")
	}
	if len(gate.calls) != 1 || gate.calls[0].NodeExecID != 7 || gate.calls[0].Tool != "spy" {
		t.Fatalf("gate calls = %+v, want exactly one call {7,spy}", gate.calls)
	}
}

// TestRunnerAuthzInfraErrorIsNotDenial:授权门自身故障(DB 不可达等,无
// ErrCallDenied 哨兵)= 基础设施失败:裸 error 透传(不包 CallError)且
// 挂 ErrAuthorizerUnavailable 哨兵(nodes 消费侧据此留 RUNNING 由重试
// 接管)——授权面故障不得伪装成越权拒绝,也不得静默放行。
func TestRunnerAuthzInfraErrorIsNotDenial(t *testing.T) {
	spy := &spyTool{}
	infra := errors.New("authz store: connection refused")
	gate := &gateSpy{response: func(gateCall) error { return infra }}
	r := NewLocalRunner(registryOf(t, spy), NewTokenIssuer(nil, 0, nil), gate)
	_, err := r.Run(context.Background(), RunRequest{Tool: "spy", NodeExecID: 1, AttemptNo: 1})
	if err == nil {
		t.Fatal("infra-failed gate returned nil")
	}
	var ce *CallError
	if errors.As(err, &ce) {
		t.Fatalf("infra failure wrapped as CallError(%s)", ce.Code)
	}
	if !errors.Is(err, ErrAuthorizerUnavailable) {
		t.Fatalf("err = %v, want ErrAuthorizerUnavailable in chain", err)
	}
	if !errors.Is(err, infra) {
		t.Fatalf("err = %v, want gate error in chain", err)
	}
	if spy.got.Token != "" {
		t.Fatal("tool was dispatched despite gate infra failure")
	}
}

// TestRunnerAuthzAllowPasses:放行路径——门被咨询恰一次,调用照常完成
// (门不得改写参数、不得重复咨询)。
func TestRunnerAuthzAllowPasses(t *testing.T) {
	spy := &spyTool{}
	gate := &gateSpy{}
	r := NewLocalRunner(registryOf(t, spy), NewTokenIssuer(nil, 0, nil), gate)
	res, err := r.Run(context.Background(), RunRequest{
		Tool: "spy", NodeExecID: 5, AttemptNo: 2, IdempotencyKey: "att:5:2",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(res.Output) != `{"ok":true}` {
		t.Fatalf("Output = %s", res.Output)
	}
	if len(gate.calls) != 1 {
		t.Fatalf("gate calls = %d, want 1", len(gate.calls))
	}
}

// TestRunnerNilAuthorizerPanics:nil 授权门 = 装配缺陷,构造即 panic
// (静默空缺会把「漏配 RBAC」伪装成「无 RBAC」,同 nil Registry 纪律)。
func TestRunnerNilAuthorizerPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewLocalRunner(nil authorizer) did not panic")
		}
	}()
	NewLocalRunner(registryOf(t), NewTokenIssuer(nil, 0, nil), nil)
}

// TestAllowAllAuthorizer:测试缝本体恒放行(生产装配禁用——见类型注释)。
func TestAllowAllAuthorizer(t *testing.T) {
	var a CallAuthorizer = AllowAllAuthorizer{}
	if err := a.AuthorizeCall(context.Background(), 1, "http"); err != nil {
		t.Fatalf("AllowAllAuthorizer: %v", err)
	}
}
