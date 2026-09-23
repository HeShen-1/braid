package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"time"
)

// jitter 是退避抖动幅度(±比例),固定 0.2(relay 同款;非运维旋钮不暴露 env)。
const jitter = 0.2

// maxResponseBytes 是响应体读取上限(响应受 max_tokens 约束,16MiB 是防
// 异常流的保险丝,非调优参数)。
const maxResponseBytes = 16 << 20

// OpenAICompat 是 OpenAI 兼容 Chat Completions 适配器(生产 Client,手写
// net/http 零 SDK)。重试语义:仅 429/5xx/网络错/单次调用自身超时重试,
// 外层 ctx 取消立即终止;每行错误的结构化出口 = StatusError(非 2xx)。
type OpenAICompat struct {
	cfg Config
	hc  *http.Client
	// jitterSrc 返回抖动因子原语(乘以 ±jitter 幅度)。并发安全由注入方承担:
	// 生产 = 顶层 rand.Float64(math/rand/v2 顶层函数文档化并发安全;*Rand
	// 实例不是——本类型是 executor 缝上的共享单例,worker 并发调用,持有
	// *Rand 会是数据竞态,review HIGH 修);测试注入私有 PCG 闭包换确定性
	// (单测试 goroutine 持有,无并发面)。
	jitterSrc func() float64
}

// NewOpenAICompat 构造生产 Client(配置矛盾启动即败)。
func NewOpenAICompat(cfg Config) (*OpenAICompat, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &OpenAICompat{
		cfg:       cfg,
		hc:        &http.Client{},
		jitterSrc: rand.Float64,
	}, nil
}

// ChatCompletion 执行一次调用,单 attempt 内对瞬态错退避重试。
func (c *OpenAICompat) ChatCompletion(ctx context.Context, req Request) (Response, error) {
	body, err := buildBody(req)
	if err != nil {
		return Response{}, err
	}
	var lastErr error
	attempt := 1
	for ; attempt <= c.cfg.MaxAttempts; attempt++ {
		resp, err := c.call(ctx, req, body)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if attempt == c.cfg.MaxAttempts || !retryable(ctx, err) {
			break
		}
		if !sleepCtx(ctx, c.backoff(attempt)) {
			return Response{}, ctx.Err()
		}
	}
	// 外层取消优先于穷尽包装(停机语义;取消与末次失败返回的交错在此
	// 确定性收敛为 context 错误,调用方免于解包两种终态形状)。
	if cerr := ctx.Err(); cerr != nil {
		return Response{}, cerr
	}
	return Response{}, fmt.Errorf("llm: chat completion %s failed after %d attempt(s): %w",
		req.Model, attempt, lastErr)
}

// reservedBodyKeys 是协议自有字段,ExtraBody 不得覆盖(入口拒绝,防
// 「扩展参数」名义篡改协议语义)。
var reservedBodyKeys = map[string]bool{
	"model": true, "messages": true, "temperature": true,
	"max_tokens": true, "response_format": true,
}

type responseFormat struct {
	Type string `json:"type"`
}

// wireResponseFormat 把平台词表值翻译为 OpenAI 兼容方言(HIGH② 拍板:翻译
// 点在适配器层)。平台词表 = DSL schema enum ["text","json"],与
// Request.ResponseFormat 同源;主流 OpenAI 兼容端点的合法 type 是
// "text"/"json_object"——DSL 值直传必 4xx。未知值(含直写方言值)响亮拒绝:
// 词表漂移在入口暴露,不上线。
func wireResponseFormat(v string) (string, error) {
	switch v {
	case "", "text":
		return v, nil
	case "json":
		return "json_object", nil
	default:
		return "", fmt.Errorf("llm: response_format %q not in platform vocabulary {text,json}", v)
	}
}

// buildBody 构造请求体:标准字段 + ExtraBody 顶层合并(map 键序由
// encoding/json 确定性排序)。调用方参数缺陷在此响亮拒绝,不发网络。
func buildBody(req Request) ([]byte, error) {
	if req.Model == "" || len(req.Messages) == 0 {
		return nil, fmt.Errorf("llm: request needs model and messages")
	}
	if req.IdempotencyKey == "" {
		return nil, fmt.Errorf("llm: request needs idempotency key (att:<node_exec_id>:<attempt_no>)")
	}
	base := map[string]any{
		"model":       req.Model,
		"messages":    req.Messages,
		"temperature": req.Temperature,
	}
	if req.MaxTokens > 0 {
		base["max_tokens"] = req.MaxTokens
	}
	if req.ResponseFormat != "" {
		wire, err := wireResponseFormat(req.ResponseFormat)
		if err != nil {
			return nil, err
		}
		base["response_format"] = responseFormat{Type: wire}
	}
	for k, v := range req.ExtraBody {
		if reservedBodyKeys[k] {
			return nil, fmt.Errorf("llm: extra body key %q collides with protocol field", k)
		}
		if !json.Valid(v) {
			return nil, fmt.Errorf("llm: extra body key %q not valid JSON", k)
		}
		base[k] = v
	}
	return json.Marshal(base)
}

// chatResponse 是 Chat Completions 成功响应的解析面。usage 是计费真相源:
// 对象缺失 = 响亮拒绝(ErrMissingUsage),不静默零计费。
type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

// ErrMissingUsage:2xx 但响应缺 usage 对象——计费真相源缺失(§2),拒绝
// 而非按零计费入账。确定性协议违约,不重试(retryable 白名单外)。
var ErrMissingUsage = errors.New("llm: response missing usage (billing truth source)")

// ErrBadResponse:2xx 但响应形状违约(解码失败/无 choices)——确定性错误,
// 重试同败,立即交还错误面(retryable 白名单外;review advisory 修)。
var ErrBadResponse = errors.New("llm: malformed 2xx response")

func (c *OpenAICompat) call(ctx context.Context, req Request, body []byte) (Response, error) {
	// 单次调用独立预算:每次重试拿到一整个新 Timeout(relay.EnqueueTimeout
	// 同款派生式),外层 ctx 取消仍即时穿透。
	ectx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ectx, http.MethodPost,
		c.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("llm: build request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	hreq.Header.Set("Idempotency-Key", req.IdempotencyKey)
	hresp, err := c.hc.Do(hreq)
	if err != nil {
		return Response{}, fmt.Errorf("llm: do: %w", err)
	}
	defer func() { _ = hresp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(hresp.Body, maxResponseBytes))
	if err != nil {
		return Response{}, fmt.Errorf("llm: read body: %w", err)
	}
	if hresp.StatusCode < 200 || hresp.StatusCode > 299 {
		return Response{}, parseStatusError(hresp.StatusCode, raw)
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return Response{}, fmt.Errorf("%w: decode: %v", ErrBadResponse, err)
	}
	if cr.Usage == nil {
		return Response{}, ErrMissingUsage
	}
	if len(cr.Choices) == 0 {
		return Response{}, fmt.Errorf("%w: no choices", ErrBadResponse)
	}
	return Response{
		Content:      cr.Choices[0].Message.Content,
		Model:        cr.Model,
		Usage:        *cr.Usage,
		FinishReason: cr.Choices[0].FinishReason,
	}, nil
}

// parseStatusError 尽力解析 OpenAI 风格错误体({error:{code,message}});
// 解析失败退化为截断原文(错误信息不丢)。
func parseStatusError(code int, raw []byte) *StatusError {
	se := &StatusError{StatusCode: code, Message: clampRunes(string(raw), 512)}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil {
		if body.Error.Code != "" {
			se.Code = body.Error.Code
		}
		if body.Error.Message != "" {
			se.Message = clampRunes(body.Error.Message, 512)
		}
	}
	return se
}

// clampRunes 在 rune 边界截断(错误摘要入 attempt.error_message 等
// VARCHAR(2000) 列时的前置习惯;此处用于 StatusError.Message)。
func clampRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// retryable:外层 ctx 已取消/超时 → 不重试(停机语义优先,relay 同款自判
// 不解读包内错误);StatusError 按 429/5xx;2xx 形状违约(ErrMissingUsage/
// ErrBadResponse)确定性同败 → 不重试;其余(网络错与单次调用自身超时,
// ectx deadline 而外层仍活)→ 可重试。
func retryable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Retryable()
	}
	if errors.Is(err, ErrMissingUsage) || errors.Is(err, ErrBadResponse) {
		return false
	}
	return true
}

// backoff 计算第 attempts 次失败后的等待时长(指数、封顶、±jitter 抖动;
// relay.runner.backoff 同式,私有不共享——两处重试节奏语义不同:relay 是
// outbox 投递分钟级,此处是 LLM 调用秒级)。
func (c *OpenAICompat) backoff(attempts int) time.Duration {
	b := float64(c.cfg.BackoffBase)
	for i := 1; i < attempts; i++ {
		b *= c.cfg.BackoffFactor
		if b >= float64(c.cfg.BackoffMax) {
			b = float64(c.cfg.BackoffMax)
			break
		}
	}
	b *= 1 + (c.jitterSrc()*2-1)*jitter
	if b < 0 {
		b = 0
	}
	return time.Duration(b)
}

// sleepCtx 睡 d 直到完成或 ctx 取消;返回 false = ctx 已取消(relay 同款)。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
