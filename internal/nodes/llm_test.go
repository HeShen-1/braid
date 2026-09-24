package nodes

// LLM 执行器单测(纯函数聚焦:prompt 渲染、outputs 声明抽取、台账桥)。
// 编排(读快照→调用→收口)归 worker 包 e2e 真链路验证。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"braid/internal/engine"
	"braid/internal/engine/dsl"
	"braid/internal/llm"
	"braid/internal/scheduler"
	"braid/internal/worker"
)

func TestRenderMessages(t *testing.T) {
	in := scheduler.LLMInput{
		System:   "你是助手",
		Template: "总结以下内容",
		UserData: []scheduler.LLMInputDatum{
			{Name: "q", Value: "什么是 braid"},
			// 大整数与嵌套对象:渲染口径 = RenderText(字符串原文、其余紧凑
			// JSON、json.Number 原文往返)。
			{Name: "n", Value: json.Number("12345678901234567890")},
			{Name: "o", Value: map[string]any{"k": json.Number("1")}},
		},
	}
	msgs := renderMessages(in)
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[0].Content != "你是助手" {
		t.Fatalf("messages = %+v, want leading system", msgs)
	}
	wantUser := "总结以下内容\n\nq: 什么是 braid\n\nn: 12345678901234567890\n\no: {\"k\":1}"
	if msgs[1].Role != "user" || msgs[1].Content != wantUser {
		t.Fatalf("user message = %q, want %q", msgs[1].Content, wantUser)
	}
}

func TestRenderMessagesMinimal(t *testing.T) {
	msgs := renderMessages(scheduler.LLMInput{Template: "gen"})
	if len(msgs) != 1 || msgs[0].Role != "user" || msgs[0].Content != "gen" {
		t.Fatalf("messages = %+v, want single bare user", msgs)
	}
}

func TestExtractOutputs(t *testing.T) {
	allTypes := `{"s":"x","n":42,"b":true,"o":{"a":1},"arr":[1,2]}`
	cases := []struct {
		name    string
		content string
		outs    map[string]string
		want    string // 期望 Output JSON;空 = 期望失败
	}{
		{"all declared types", allTypes,
			map[string]string{"s": "string", "n": "number", "b": "boolean", "o": "object", "arr": "array"},
			`{"arr":[1,2],"b":true,"n":42,"o":{"a":1},"s":"x"}`},
		{"subset declaration", allTypes,
			map[string]string{"n": "number"}, `{"n":42}`},
		{"no declaration yields content", "plain text answer", nil,
			`{"content":"plain text answer"}`},
		{"declared field missing", allTypes,
			map[string]string{"nope": "string"}, ""},
		{"type mismatch string vs number", allTypes,
			map[string]string{"n": "string"}, ""},
		{"type mismatch number vs string", allTypes,
			map[string]string{"s": "number"}, ""},
		{"content not json", "oops", map[string]string{"s": "string"}, ""},
		{"content json but not object", `[1,2]`, map[string]string{"s": "string"}, ""},
		{"content null", `null`, map[string]string{"s": "string"}, ""},
		{"number out of double range", `{"n":1e309}`, map[string]string{"n": "number"}, ""},
		{"nested number out of double range", `{"o":{"deep":-2e400}}`, map[string]string{"o": "object"}, ""},
		{"array element out of double range", `{"arr":[1,2e400]}`, map[string]string{"arr": "array"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := extractOutputs(tc.content, tc.outs)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("extractOutputs(%q) = %s, want error", tc.content, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("extractOutputs(%q): %v", tc.content, err)
			}
			var got, want any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("output not json: %v", err)
			}
			_ = json.Unmarshal([]byte(tc.want), &want)
			// 语义比较(键序无关);数值走字符串比较防 float64 抖动。
			gotS, _ := json.Marshal(got)
			wantS, _ := json.Marshal(want)
			if string(gotS) != string(wantS) {
				t.Fatalf("output = %s, want %s", gotS, wantS)
			}
		})
	}
}

// 大整数保真:声明 number 的字段在抽取往返后原文不变(float64 中转必失真)。
func TestExtractOutputsBigNumberFidelity(t *testing.T) {
	out, err := extractOutputs(`{"n":12345678901234567890}`, map[string]string{"n": "number"})
	if err != nil {
		t.Fatalf("extractOutputs: %v", err)
	}
	if string(out) != `{"n":12345678901234567890}` {
		t.Fatalf("output = %s, big integer lost fidelity", out)
	}
}

// 超 DOUBLE 范围数值在抽取单点被拒(T3.5 审计 HIGH):1e309 若原文穿到
// node_execution.output,MySQL JSON 列以 ERROR 3140 拒写,收口事务回滚、
// attempt 恒 RUNNING 循环重放外部调用——必须在此响亮失败(业务分类)。
func TestExtractOutputsDoubleRangeRejected(t *testing.T) {
	_, err := extractOutputs(`{"n":1e309}`, map[string]string{"n": "number"})
	if err == nil {
		t.Fatal("extractOutputs accepted 1e309, want out-of-double-range rejection")
	}
	if !strings.Contains(err.Error(), "1e309") {
		t.Fatalf("err = %v, want offending literal in diagnostics", err)
	}
}

// ---- executeCall 失败半边(T3.6 对抗审计 P8:业务失败 vs 基础设施失败的
// 语义分立此前零测试)。fake Client 零 DB,覆盖 ctx 分类、错误码细分、
// 响应不履约、坏输出、计费溢出、成功台账桥。 ----

type fakeClient struct {
	resp llm.Response
	err  error
}

func (f *fakeClient) ChatCompletion(ctx context.Context, _ llm.Request) (llm.Response, error) {
	// 模拟真实适配器的取消优先语义(openai.go:外层 ctx 错优先于业务结果)。
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	return f.resp, f.err
}

var _ llm.Client = (*fakeClient)(nil)

func okLLMResponse() llm.Response {
	return llm.Response{Content: `{"s":"x"}`, Model: "glm-5.3",
		Usage: llm.Usage{PromptTokens: 1000, CompletionTokens: 150, TotalTokens: 1150}}
}

func testPrices() llm.PriceTable {
	var t testing.T
	table, err := llm.ParsePriceTable(
		`{"glm-5.3":{"input_per_mtok":"2","output_per_mtok":"8","currency":"CNY"}}`)
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func TestExecuteCallFailureClassification(t *testing.T) {
	declared := &dsl.LLMNode{Outputs: map[string]string{"s": "string"}}
	req := worker.Request{NodeExecutionID: 7, TaskID: "att:7:1"}
	in := scheduler.LLMInput{Model: "glm-5.3", Template: "t"}

	cases := []struct {
		name      string
		client    *fakeClient
		ctxErr    error  // 非空时以已取消 ctx 调用
		wantCode  string // 空 = 期望 infra error(非 outcome)
		wantUsage bool
	}{
		{"ctx deadline is infra error", &fakeClient{}, context.DeadlineExceeded, "", false},
		{"ctx cancel is infra error", &fakeClient{}, context.Canceled, "", false},
		{"429 maps to llm_rate_limited",
			&fakeClient{err: fmt.Errorf("llm: chat completion failed after 3 attempt(s): %w",
				&llm.StatusError{StatusCode: 429, Code: "rate_limited", Message: "slow down"})},
			nil, "llm_rate_limited", false},
		{"non-429 maps to llm_call_failed",
			&fakeClient{err: fmt.Errorf("wrapped: %w", &llm.StatusError{StatusCode: 401})},
			nil, "llm_call_failed", false},
		{"plain error maps to llm_call_failed",
			&fakeClient{err: errors.New("connection reset")}, nil, "llm_call_failed", false},
		{"missing model rejected", &fakeClient{resp: func() llm.Response {
			r := okLLMResponse()
			r.Model = ""
			return r
		}()}, nil, "llm_bad_response", false},
		{"bad output content", &fakeClient{resp: llm.Response{
			Content: "not json", Model: "glm-5.3"}}, nil, "bad_llm_output", false},
		{"success carries usage", &fakeClient{resp: okLLMResponse()}, nil, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &LLMExecutor{Client: tc.client, Prices: testPrices()}
			ctx := context.Background()
			if tc.ctxErr != nil {
				cctx, cancel := context.WithCancel(context.Background())
				if tc.ctxErr == context.DeadlineExceeded {
					dctx, dcancel := context.WithDeadline(cctx, time.Now().Add(-time.Second))
					cancel, cctx = dcancel, dctx
					_ = cctx.Err()
				} else {
					cancel()
				}
				defer cancel()
				ctx = cctx
			}
			out, err := e.executeCall(ctx, req, in, declared)
			if tc.wantCode == "" && tc.ctxErr != nil {
				// infra error:非空 error、零值 outcome、无 Usage
				if err == nil {
					t.Fatalf("executeCall = outcome %+v, want infra error", out)
				}
				if out.Status != "" || out.Usage != nil {
					t.Fatalf("infra path leaked outcome %+v", out)
				}
				return
			}
			if tc.wantCode == "" && tc.wantUsage {
				// 成功:usage 桥
				if err != nil || out.Status != engine.AttemptSucceed {
					t.Fatalf("executeCall = (%+v, %v), want SUCCEEDED", out, err)
				}
				if out.Usage == nil || out.Usage.CostAmount != "0.003200" || out.Usage.Billed != true {
					t.Fatalf("usage = %+v, want billed 0.003200 CNY(T3.1 锚)", out.Usage)
				}
				return
			}
			if err != nil {
				t.Fatalf("executeCall err = %v, want FAILED outcome", err)
			}
			if out.Status != engine.AttemptFailed || out.Code != tc.wantCode {
				t.Fatalf("outcome = %+v, want FAILED/%s", out, tc.wantCode)
			}
			if out.Usage != nil {
				t.Fatalf("failed outcome carries usage: %+v(失败路径不计费)", out.Usage)
			}
			if !utf8.ValidString(out.Msg) {
				t.Fatalf("msg not valid utf-8: %q", out.Msg)
			}
		})
	}
}

// clampMessage:换行单行化(日志伪造面)+ rune 截断恒产合法 UTF-8。
func TestClampMessage(t *testing.T) {
	if got := clampMessage("a\nb\rc"); got != `a\nb\rc` {
		t.Fatalf("clampMessage = %q, want newline-flattened", got)
	}
	long := strings.Repeat("边界", 2000) // 4000 rune,每 rune 3 字节
	got := clampMessage(long)
	if n := utf8.RuneCountInString(got); n != 2000 {
		t.Fatalf("clamped runes = %d, want 2000", n)
	}
	if !utf8.ValidString(got) {
		t.Fatal("clamped message not valid utf-8(字节切割会触发 MySQL 1366)")
	}
}
