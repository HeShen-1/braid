package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// Call 是单次工具调用的完整参数。Inputs 为已求值输入(scheduler
// input_snapshot 原值,纯引用已解析、内嵌引用已替换);Token 是本次调用的
// scoped token(LocalRunner 签发注入);IdempotencyKey 是 attempt 幂等键
// (与 node_attempt.idempotency_key 同值,AGENTS 铁律:外部调用幂等键的
// 工具侧传递面,消费归工具实现——如 http 工具透传为请求头)。
type Call struct {
	Inputs         map[string]any
	Token          string
	IdempotencyKey string
}

// Result 是工具产出。Output 必须是 JSON 对象(声明抽取的原料,LocalRunner
// 统一校验,不信任工具自查)。
type Result struct {
	Output json.RawMessage
}

// Tool 是 registry 条目的执行契约。进程内工具(T3.5 内置 http/file_read)
// 直接实现本接口;子进程载体用 CommandTool 适配——两类载体同一接口,
// P1 换 Docker 沙箱只替换载体实现,registry 与 Runner 不动(ARCHITECTURE
// §3.4「不动核心」的接缝)。
type Tool interface {
	Name() string
	Run(ctx context.Context, call Call) (Result, error)
}

// Registry 是内部工具注册表(P0,ARCHITECTURE §3.4「插件:P0 内部
// registry」)。Names() 即发布白名单真源(T5.4 换源:BRAID_TOOL_ALLOWLIST
// 静态 env 退役,白名单 = 实际注册的工具名;空 registry = 禁用一切工具,
// 语义与空名单一致)。
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry 以初始工具集构造;nil 工具、空名、重名响亮拒绝(装配缺陷
// 启动即现)。
func NewRegistry(tools ...Tool) (*Registry, error) {
	r := &Registry{tools: make(map[string]Tool, len(tools))}
	for _, t := range tools {
		if err := r.Register(t); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Register 运行期注册(并发安全;内置工具装配与测试注入共用)。
func (r *Registry) Register(t Tool) error {
	if t == nil {
		return fmt.Errorf("tools: register nil tool")
	}
	name := t.Name()
	if name == "" {
		return fmt.Errorf("tools: tool with empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[name]; dup {
		return fmt.Errorf("tools: duplicate tool %q", name)
	}
	r.tools[name] = t
	return nil
}

// Get 按名查找(找不到是业务事实而非错误:tool_not_found 失败码的判定点)。
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Names 返回排序后的注册名(顺序稳定:白名单比对与测试断言可复现)。
func (r *Registry) Names() []string {
	r.mu.RLock()
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	r.mu.RUnlock()
	sort.Strings(names)
	return names
}
