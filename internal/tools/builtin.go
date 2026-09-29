package tools

// BuiltinToolsFromEnv 解析内置工具装配 env(T3.5),返回应注册的工具集:
//
//	BRAID_HTTP_ENDPOINTS  JSON 对象 {alias: https base_url}
//	BRAID_FILEREAD_DIRS   JSON 数组 [绝对目录, ...]
//	BRAID_TOOL_SANDBOX    off(未设/空=缺省)| docker(T10.1 沙箱词表,sandbox.go)
//
// 语义:**未配置即不注册**——Names() 发布白名单真源(T3.2 换源)不被
// 不可用工具污染:空配置下定义引用 http/file_read 在发布时即被拒、运行时
// tool_not_found,响亮而非静默。坏 JSON/坏值同样响亮败(装配缺陷启动即现,
// 与 llm.ConfigFromEnv 同纪律)。api 与 worker 共用本函数单源装配,防
// 两进程注册集漂移(发布白名单与执行能力必须一致)。
//
// 沙箱装配缝(T10.1 T6,ARCHITECTURE L74「不动核心」):mode=docker 时
// http/file_read 同名换装 sandboxCarrier(执行进一次性容器,registry/
// runner 零感知);off/未设 = 进程内 builtin 逐字节现状,对沙箱键零消费。
// 构造全程惰性——零 docker 接触(api 进程 docker-free 的结构保证;部署
// 检查在 worker 侧 SandboxStartupCheck 拒启)。值校验两模式同门走原构造
// 器(docker 模式借构造器校验后弃置进程内实例——容器内 helper 重建走同
// 一批构造器,策略零复制,装配缺陷启动即现);并发信号量装配期单例,跨
// 工具共享同一 daemon 预算。
//
// JSON-in-env 先例:BRAID_LLM_PRICE_TABLE(D-2026-09-23-t31)。

import (
	"encoding/json"
	"fmt"
)

// BuiltinToolsFromEnv 见文件头注释。getenv 注入便于测试(os.Getenv 同款
// 签名,llm/worker 包惯例)。
func BuiltinToolsFromEnv(getenv func(string) string) ([]Tool, error) {
	sbcfg, err := sandboxConfigFromEnv(getenv)
	if err != nil {
		return nil, err
	}
	var limiter *sandboxLimiter // docker 模式装配期单例;off 恒 nil(零分配)
	if sbcfg.Docker {
		limiter = newSandboxLimiter(sbcfg.MaxConcurrency)
	}
	var out []Tool
	if raw := getenv("BRAID_HTTP_ENDPOINTS"); raw != "" {
		eps := make(map[string]string)
		if err := json.Unmarshal([]byte(raw), &eps); err != nil {
			return nil, fmt.Errorf("tools: BRAID_HTTP_ENDPOINTS: want JSON object {alias: base_url}: %w", err)
		}
		ht, err := NewHTTPTool(HTTPConfig{Endpoints: eps}) // 值校验(docker 模式仅校验,off 注册该实例)
		if err != nil {
			return nil, fmt.Errorf("tools: BRAID_HTTP_ENDPOINTS: %w", err)
		}
		if sbcfg.Docker {
			c, err := newSandboxCarrierHTTP(sbcfg, eps, limiter)
			if err != nil {
				return nil, fmt.Errorf("tools: BRAID_HTTP_ENDPOINTS: %w", err)
			}
			out = append(out, c)
		} else {
			out = append(out, ht)
		}
	}
	if raw := getenv("BRAID_FILEREAD_DIRS"); raw != "" {
		var dirs []string
		if err := json.Unmarshal([]byte(raw), &dirs); err != nil {
			return nil, fmt.Errorf("tools: BRAID_FILEREAD_DIRS: want JSON array of absolute dirs: %w", err)
		}
		ft, err := NewFileReadTool(FileReadConfig{Dirs: dirs}) // 值校验(docker 模式仅校验,off 注册该实例)
		if err != nil {
			return nil, fmt.Errorf("tools: BRAID_FILEREAD_DIRS: %w", err)
		}
		if sbcfg.Docker {
			c, err := newSandboxCarrierFileRead(sbcfg, dirs, limiter)
			if err != nil {
				return nil, fmt.Errorf("tools: BRAID_FILEREAD_DIRS: %w", err)
			}
			out = append(out, c)
		} else {
			out = append(out, ft)
		}
	}
	return out, nil
}
