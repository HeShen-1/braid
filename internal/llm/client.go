// Package llm 实现自研薄 LLMClient 封装与 OpenAI 兼容适配器(T3.1)。
//
// 架构锁定(ARCHITECTURE §2):平台原语只用 Chat Completions,不用 Responses
// API;计费真相源 = API 返回 usage,tiktoken 仅预算估算(T7.2 离线化,本包
// 零依赖零 go.mod 变更);extra_body 类供应商参数(国产模型必需,go-openai
// 无此字段发不出)经 Request.ExtraBody 顶层合并——故适配器手写 net/http
// 而非 SDK。
//
// 缝模板与 relay.Enqueuer 同构:Client 接口签名即协议,生产实现唯一
// (OpenAICompat),fake 只存在于消费方 _test.go。重试权威分层(锁定,勿混):
// 本包退避重试仅限单 attempt 内的瞬态错(429/5xx/网络/单次超时),节点级
// 重试权威在平台调度层(worker.Outcome 语义分立,新 attempt = 新幂等键)。
package llm

import (
	"context"
	"encoding/json"
)

// Message 是 Chat Completions 消息(Role:system|user|assistant)。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request 是一次 Chat Completions 调用参数。IdempotencyKey = attempt 幂等键
// (att:<node_exec_id>:<attempt_no>,AGENTS 铁律「所有外部调用带 attempt 幂等键」),
// 经 Idempotency-Key 头发出(供应商侧同键去重;DB 侧 uk_attempt_idem 是
// 永久锚)。空串在适配器入口被拒。
type Request struct {
	Model       string
	Messages    []Message
	Temperature float64
	MaxTokens   int // 0 = 供应商默认(不发送该字段)
	// ResponseFormat 响应格式约束("" 不发送 | "json_object" 等 DSL 词表值)。
	ResponseFormat string
	// ExtraBody 顶层合并进请求体的供应商扩展参数(extra_body 语义:
	// thinking/厂商必需字段);键不得与协议字段碰撞(入口拒绝)。
	ExtraBody      map[string]json.RawMessage
	IdempotencyKey string
}

// Usage 是 API 返回 usage 三值,计费真相源(§2);台账原样存不求和
// (DDL:便于与供应商账单逐行核对)。
type Usage struct {
	PromptTokens     uint32 `json:"prompt_tokens"`
	CompletionTokens uint32 `json:"completion_tokens"`
	TotalTokens      uint32 `json:"total_tokens"`
}

// Response 是 Chat Completions 成功响应的收敛面。
type Response struct {
	Content      string
	Model        string // 实际解析后的模型名(供应商可回填别名/降级,台账归因用)
	Usage        Usage
	FinishReason string
}

// Client 是节点执行缝对 LLM 供应商的全部行为依赖(依赖倒置而非测试后门:
// 生产路径唯一,升级/换供应商的 blast radius = 1 接口 + 1 适配器)。
type Client interface {
	ChatCompletion(ctx context.Context, req Request) (Response, error)
}
