package tools

// BuiltinToolsFromEnv 解析内置工具装配 env(T3.5),返回应注册的工具集:
//
//	BRAID_HTTP_ENDPOINTS  JSON 对象 {alias: https base_url}
//	BRAID_FILEREAD_DIRS   JSON 数组 [绝对目录, ...]
//
// 语义:**未配置即不注册**——Names() 发布白名单真源(T3.2 换源)不被
// 不可用工具污染:空配置下定义引用 http/file_read 在发布时即被拒、运行时
// tool_not_found,响亮而非静默。坏 JSON/坏值同样响亮败(装配缺陷启动即现,
// 与 llm.ConfigFromEnv 同纪律)。api 与 worker 共用本函数单源装配,防
// 两进程注册集漂移(发布白名单与执行能力必须一致)。
//
// JSON-in-env 先例:BRAID_LLM_PRICE_TABLE(D-2026-09-23-t31)。

import (
	"encoding/json"
	"fmt"
)

// BuiltinToolsFromEnv 见文件头注释。getenv 注入便于测试(os.Getenv 同款
// 签名,llm/worker 包惯例)。
func BuiltinToolsFromEnv(getenv func(string) string) ([]Tool, error) {
	var out []Tool
	if raw := getenv("BRAID_HTTP_ENDPOINTS"); raw != "" {
		eps := make(map[string]string)
		if err := json.Unmarshal([]byte(raw), &eps); err != nil {
			return nil, fmt.Errorf("tools: BRAID_HTTP_ENDPOINTS: want JSON object {alias: base_url}: %w", err)
		}
		ht, err := NewHTTPTool(HTTPConfig{Endpoints: eps})
		if err != nil {
			return nil, fmt.Errorf("tools: BRAID_HTTP_ENDPOINTS: %w", err)
		}
		out = append(out, ht)
	}
	if raw := getenv("BRAID_FILEREAD_DIRS"); raw != "" {
		var dirs []string
		if err := json.Unmarshal([]byte(raw), &dirs); err != nil {
			return nil, fmt.Errorf("tools: BRAID_FILEREAD_DIRS: want JSON array of absolute dirs: %w", err)
		}
		ft, err := NewFileReadTool(FileReadConfig{Dirs: dirs})
		if err != nil {
			return nil, fmt.Errorf("tools: BRAID_FILEREAD_DIRS: %w", err)
		}
		out = append(out, ft)
	}
	return out, nil
}
