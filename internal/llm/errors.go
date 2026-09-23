package llm

import "fmt"

// StatusError 是非 2xx 响应的结构化承载:StatusCode 供消费方映射
// attempt.error_code 细分(429/超时等,DDL §4;T6.4 429 率指标字源),
// Code/Message 来自响应体 error 对象(尽力解析,缺省退化原文)。
//
// 重试穷尽的错误形态约定:ChatCompletion 返回
// fmt.Errorf("... after %d attempt(s): %w", n, lastErr),lastErr 恒可
// errors.As 解包出 *StatusError(非 2xx 路径)——消费方按此细分错误码。
type StatusError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *StatusError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("llm: http %d (%s): %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("llm: http %d: %s", e.StatusCode, e.Message)
}

// Retryable:429 与 5xx 是仅有的可重试类别;其余 4xx(鉴权/参数/模型名)
// 重试注定同败,重试权威立即交还平台节点层。
func (e *StatusError) Retryable() bool { return e.StatusCode == 429 || e.StatusCode >= 500 }
