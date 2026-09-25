package llm

// 单测:OpenAI 兼容适配器(httptest 假端点,零外部依赖零 DB)。
//
// 变异自查(实现错 → 哪条红):
//   - ExtraBody 合并丢键/错层 → request_shape 红;
//   - 幂等键未上头 → request_shape/retry_429 红;
//   - retryable 放宽到 4xx → no_retry_400 红(请求数断言);
//   - retryable 放宽到 2xx 形状违约 → malformed_2xx 红(单发断言);
//   - 重试计数 off-by-one → exhausted 红(3 次);
//   - 外层取消仍重试 → cancel_aborts_backoff 红;
//   - usage 缺失静默零计费 → missing_usage 红;
//   - jitterSrc 退回 *Rand 字段 → concurrent_no_race 在 -race 下红。

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient 起httptest 端点 + 指向它的适配器(退避毫秒级,重试即时)。
func newTestClient(t *testing.T, cfgMut func(*Config), handler http.HandlerFunc) (*OpenAICompat, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	cfg := DefaultConfig()
	cfg.BaseURL = srv.URL
	cfg.APIKey = "sk-test"
	cfg.Timeout = 5 * time.Second
	cfg.BackoffBase = time.Millisecond
	cfg.BackoffMax = 5 * time.Millisecond
	cfg.PriceTable = mustTable(t, minPriceTable)
	if cfgMut != nil {
		cfgMut(&cfg)
	}
	c, err := NewOpenAICompat(cfg, nil)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c, &calls
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func okBody() map[string]any {
	return map[string]any{
		"id":    "resp-1",
		"model": "glm-5.3",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "hello"},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 1000, "completion_tokens": 150, "total_tokens": 1150},
	}
}

func baseReq() Request {
	return Request{
		Model: "glm-5.3", Messages: []Message{{Role: "user", Content: "hi"}},
		IdempotencyKey: "att:42:1",
	}
}

func TestChatCompletionRequestShape(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotIdem string
	c, calls := newTestClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotIdem = r.Header.Get("Idempotency-Key")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writeJSON(w, 200, okBody())
	})
	req := baseReq()
	req.Temperature = 0.7
	req.MaxTokens = 512
	req.ResponseFormat = "json"
	req.ExtraBody = map[string]json.RawMessage{"thinking": json.RawMessage(`{"type":"enabled"}`)}
	resp, err := c.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion() = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotIdem != "att:42:1" {
		t.Errorf("Idempotency-Key = %q, want att:42:1", gotIdem)
	}
	// 协议字段 + ExtraBody 顶层合并
	if gotBody["model"] != "glm-5.3" || gotBody["temperature"] != 0.7 ||
		gotBody["max_tokens"] != float64(512) {
		t.Errorf("protocol fields = %v", gotBody)
	}
	if rf, ok := gotBody["response_format"].(map[string]any); !ok || rf["type"] != "json_object" {
		t.Errorf("response_format = %v", gotBody["response_format"])
	}
	if th, ok := gotBody["thinking"].(map[string]any); !ok || th["type"] != "enabled" {
		t.Errorf("extra body not merged top-level: %v", gotBody["thinking"])
	}
	// 响应收敛面:usage 原值 + model 回填
	if resp.Content != "hello" || resp.Model != "glm-5.3" || resp.FinishReason != "stop" {
		t.Errorf("Response = %+v", resp)
	}
	if resp.Usage != (Usage{1000, 150, 1150}) {
		t.Errorf("Usage = %+v, want 1000/150/1150", resp.Usage)
	}
}

// TestResponseFormatTranslation 钉死平台词表协议:Request.ResponseFormat 只收
// DSL 平台值("text"/"json"),OpenAI 方言(json_object)由适配器翻译产生,
// 直写方言值 = 响亮拒绝(HIGH② 拍板:翻译点在适配器层)。
func TestResponseFormatTranslation(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string // 期望线上 type;空 = 不发送
		wantErr bool
	}{
		{"json translates to json_object", "json", "json_object", false},
		{"text passes through", "text", "text", false},
		{"empty omits field", "", "", false},
		{"wire dialect rejected", "json_object", "", true},
		{"unknown rejected", "yaml", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := baseReq()
			req.ResponseFormat = tc.in
			body, err := buildBody(req)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("buildBody(%q) = nil error, want rejection", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildBody(%q): %v", tc.in, err)
			}
			var got map[string]any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("unmarshal body: %v", err)
			}
			rf, ok := got["response_format"].(map[string]any)
			if tc.want == "" {
				if ok {
					t.Fatalf("response_format sent despite empty request value: %v", rf)
				}
				return
			}
			if !ok || rf["type"] != tc.want {
				t.Fatalf("response_format = %v, want type=%q", got["response_format"], tc.want)
			}
		})
	}
}

func TestRequestShapeOmitsZeroOptionals(t *testing.T) {
	var gotBody map[string]any
	c, _ := newTestClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, 200, okBody())
	})
	req := baseReq() // MaxTokens=0, ResponseFormat="", 无 ExtraBody
	if _, err := c.ChatCompletion(context.Background(), req); err != nil {
		t.Fatalf("ChatCompletion() = %v", err)
	}
	for _, k := range []string{"max_tokens", "response_format"} {
		if _, ok := gotBody[k]; ok {
			t.Errorf("body carries %q despite zero value: %v", k, gotBody)
		}
	}
	if _, ok := gotBody["temperature"]; !ok {
		t.Error("temperature must be sent even at 0(0 是合法语义值)")
	}
}

func TestExtraBodyCollisionRejected(t *testing.T) {
	c, calls := newTestClient(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, okBody())
	})
	req := baseReq()
	req.ExtraBody = map[string]json.RawMessage{"model": json.RawMessage(`"evil"`)}
	if _, err := c.ChatCompletion(context.Background(), req); err == nil {
		t.Fatal("collision accepted, want rejection")
	}
	if calls.Load() != 0 {
		t.Fatalf("calls = %d, want 0(reject before network)", calls.Load())
	}
	// 幂等键缺失同样入口拒绝
	if _, err := c.ChatCompletion(context.Background(), Request{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
	}); err == nil {
		t.Fatal("missing idempotency key accepted, want rejection")
	}
}

func TestMissingUsageRejected(t *testing.T) {
	body := okBody()
	delete(body, "usage")
	c, _ := newTestClient(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, body)
	})
	_, err := c.ChatCompletion(context.Background(), baseReq())
	if !errors.Is(err, ErrMissingUsage) {
		t.Fatalf("err = %v, want ErrMissingUsage(计费真相源缺失不静默零计费)", err)
	}
}

func TestRetryOn429ThenSuccess(t *testing.T) {
	var idemKeys []string
	var seen atomic.Int64
	c, calls := newTestClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		idemKeys = append(idemKeys, r.Header.Get("Idempotency-Key"))
		if seen.Add(1) == 1 {
			writeJSON(w, 429, map[string]any{"error": map[string]any{
				"code": "rate_limit_exceeded", "message": "slow down"}})
			return
		}
		writeJSON(w, 200, okBody())
	})
	resp, err := c.ChatCompletion(context.Background(), baseReq())
	if err != nil {
		t.Fatalf("ChatCompletion() = %v, want 429→200 收敛", err)
	}
	if resp.Usage.TotalTokens != 1150 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
	for i, k := range idemKeys {
		if k != "att:42:1" {
			t.Errorf("attempt %d idempotency key = %q, want 常键", i+1, k)
		}
	}
}

func TestNoRetryOn400(t *testing.T) {
	c, calls := newTestClient(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 400, map[string]any{"error": map[string]any{
			"code": "invalid_request_error", "message": "bad param"}})
	})
	_, err := c.ChatCompletion(context.Background(), baseReq())
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != 400 || se.Code != "invalid_request_error" {
		t.Fatalf("err = %v, want StatusError(400, invalid_request_error)", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1(4xx 不重试)", calls.Load())
	}
}

func TestRetriesExhausted(t *testing.T) {
	c, calls := newTestClient(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 500, map[string]any{"error": map[string]any{"message": "boom"}})
	})
	_, err := c.ChatCompletion(context.Background(), baseReq())
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != 500 {
		t.Fatalf("err = %v, want wrapped StatusError(500)", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("calls = %d, want 3(MaxAttempts 默认)", got)
	}
	if want := "after 3 attempt(s)"; !strings.Contains(err.Error(), want) {
		t.Errorf("err = %q, want containing %q", err, want)
	}
}

func TestContextCancelAbortsBackoff(t *testing.T) {
	// 退避拉长到 50ms:取消 goroutine 以 1ms 轮询首响,必落在退避睡眠中
	//(毫秒级退避下取消可能落在睡眠后,竞态把断言打成 calls=2)。
	c, calls := newTestClient(t, func(c *Config) {
		c.BackoffBase = 50 * time.Millisecond
		c.BackoffMax = 100 * time.Millisecond
	}, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 429, map[string]any{})
	})
	ctx, cancel := context.WithCancel(context.Background())
	// 首个 429 返回后、退避睡眠中取消外层 ctx。
	go func() {
		for calls.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	_, err := c.ChatCompletion(ctx, baseReq())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled(停机不重试)", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1(backoff 中止,不发第二发)", calls.Load())
	}
}

func TestBackoffBounds(t *testing.T) {
	cfg := DefaultConfig()
	cfg.BackoffBase = time.Second
	cfg.BackoffFactor = 4
	cfg.BackoffMax = 5 * time.Minute
	// 私有 PCG 注入换确定性(单 goroutine 持有,无并发面)。
	det := rand.New(rand.NewPCG(42, 1))
	c := &OpenAICompat{cfg: cfg, jitterSrc: det.Float64}
	// 第 n 次失败后:base*factor^(n-1)(封顶 max)再 ±20% 抖动。1s×4^(n-1):
	// 1s/4s/16s/64s/256s,第 6 次 1024s 被封顶截断到 5m。
	want := []time.Duration{time.Second, 4 * time.Second, 16 * time.Second,
		64 * time.Second, 256 * time.Second, 5 * time.Minute}
	for n, w := range want {
		lo := time.Duration(float64(w) * 0.8)
		hi := time.Duration(float64(w) * 1.2)
		for i := 0; i < 200; i++ { // 抽样抖动分布边界
			got := c.backoff(n + 1)
			if got < lo || got > hi {
				t.Fatalf("backoff(%d) = %s, want [%s,%s]", n+1, got, lo, hi)
			}
		}
	}
}

func TestNewOpenAICompatRejectsBadConfig(t *testing.T) {
	cfg := DefaultConfig() // 空 BaseURL/APIKey/表
	if _, err := NewOpenAICompat(cfg, nil); err == nil {
		t.Fatal("bad config accepted, want rejection")
	}
	// 换合法配置后错误消息可定位
	cfg.BaseURL = "https://x/v1"
	cfg.APIKey = "k"
	if _, err := NewOpenAICompat(cfg, nil); err == nil || !strings.Contains(err.Error(), "price table") {
		t.Fatalf("err = %v, want price table error", err)
	}
}

// TestMalformed2xxNotRetried(review advisory 修):2xx 形状违约是确定性错误
// (缺 usage / 无 choices / 解码失败),重试同败——单发即返回,不烧退避预算,
// 不以同幂等键重复 POST 已 2xx 的调用。
func TestMalformed2xxNotRetried(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr error
	}{
		{"missing usage", `{"model":"m","choices":[{"message":{"content":"x"}}]}`, ErrMissingUsage},
		{"no choices", `{"model":"m","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, ErrBadResponse},
		{"undecodable", `not-json`, ErrBadResponse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, calls := newTestClient(t, nil, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := c.ChatCompletion(context.Background(), baseReq())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("calls = %d, want 1(确定性协议违约不重试)", got)
			}
		})
	}
}

// TestConcurrentChatCompletionNoRace(review HIGH 修锚):共享单例并发调用
// (生产形态:worker 并发 executor 同一 Client)。-race 下 jitterSrc 顶层
// rand.Float64 必须无竞态——曾以 *Rand 字段实现时本测试必红。
func TestConcurrentChatCompletionNoRace(t *testing.T) {
	c, calls := newTestClient(t, func(c *Config) {
		c.BackoffBase = time.Millisecond
		c.BackoffMax = 2 * time.Millisecond
	}, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, okBody())
	})
	const workers, rounds = 8, 4
	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				if _, err := c.ChatCompletion(context.Background(), baseReq()); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent call: %v", err)
	}
	if got := calls.Load(); got != workers*rounds {
		t.Fatalf("calls = %d, want %d", got, workers*rounds)
	}
}
