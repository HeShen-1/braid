package dsl

import (
	"encoding/json"
	"fmt"
)

// Option 配置 Parse 行为。
type Option func(*parseConfig)

type parseConfig struct {
	// toolAllowlist 非nil时启用发布级工具白名单校验(T5.4 发布路径接入;
	// nil = 跳过,供草稿预览/测试使用,registry 建立前的默认态)。
	toolAllowlist map[string]bool
}

// WithToolAllowlist 启用工具白名单校验:任何 tool 节点引用不在名单内的工具
// 都会产生 IssueToolNotAllowed。名单为空集 = 禁用一切工具。
func WithToolAllowlist(tools ...string) Option {
	set := make(map[string]bool, len(tools))
	for _, t := range tools {
		set[t] = true
	}
	return func(c *parseConfig) { c.toolAllowlist = set }
}

// Parse 是 DSL 文档的唯一入口:JSON 解码 → T1.1 schema 结构校验 → T1.2 语义校验。
// 语义发现聚合在 *ParseError 中返回(不 fail-fast);结构错误原样包装返回。
// 通过(返回 nil error)即得可调度的工作流模型。
func Parse(data []byte, opts ...Option) (*Workflow, error) {
	if err := Validate(data); err != nil {
		return nil, fmt.Errorf("dsl: schema validation: %w", err)
	}
	var wf Workflow
	if err := json.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("dsl: decode workflow: %w", err)
	}
	cfg := parseConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	if issues := wf.validate(cfg); len(issues) > 0 {
		return &wf, &ParseError{Issues: issues}
	}
	return &wf, nil
}
