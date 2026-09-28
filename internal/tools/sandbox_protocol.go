package tools

// 沙箱信封协议(T10.1,D-2026-09-28-t101 #2/#6):宿主载体与容器内 helper
// 双端共享(同包常量,编译期一致)。请求经 stdin 单行 JSON 进容器(token/
// attempt/config/inputs 全走管道——docker argv 在宿主 /proc 全局可读、
// -e env 与 docker inspect 另开两条可读信道,stdin 两侧皆不可旁读);响应
// 经 stdout 单行 JSON 回宿主。
//
// 响应形态:
//
//	{"ok":true,"output":{...}}                                   → Result.Output
//	{"ok":false,"code":"ssrf_blocked|host_blocked|path_blocked"}  → 宿主重抛真哨兵
//	{"ok":false,"code":"tool_failed","message":"..."}             → 普通业务错误
//
// 哨兵经 code 跨进程、宿主以 %w 重建 errors.Is 链——runner.go:165-177 既有
// 映射原样命中,分类单点不动(D-t32 张力由 D-t101 #6 澄清)。未知码/畸形
// 信封返回普通错误,经 runner 默认分支归 tool_failed(基线 §6)。

import (
	"encoding/json"
	"errors"
	"fmt"
)

// 沙箱信封失败码(词表 {ssrf_blocked, host_blocked, path_blocked,
// tool_failed};开放失败码词表锚 runner.go 头注释,本任务零新增)。
const (
	SandboxCodeSSRFBlocked = "ssrf_blocked"
	SandboxCodeHostBlocked = "host_blocked"
	SandboxCodePathBlocked = "path_blocked"
	SandboxCodeToolFailed  = "tool_failed"
)

// SandboxRequest 宿主 → 容器的 stdin 请求信封。Config 为该工具的装配配置
// 原料(endpoints 对象 / dirs 数组),helper 在容器内以同一导出构造器重建
// 工具对象(策略代码同编译单元随行,零复制)。
type SandboxRequest struct {
	Tool           string          `json:"tool"`
	Config         json.RawMessage `json:"config,omitempty"`
	Token          string          `json:"token,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Inputs         json.RawMessage `json:"inputs,omitempty"`
	DeadlineUnix   int64           `json:"deadline_unix,omitempty"`
}

// SandboxResponse 容器 → 宿主的 stdout 响应信封。
type SandboxResponse struct {
	OK      bool            `json:"ok"`
	Output  json.RawMessage `json:"output,omitempty"`
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
}

// EncodeSandboxRequest 序列化请求信封(单行 JSON;json.Marshal 不产生换行)。
func EncodeSandboxRequest(req SandboxRequest) ([]byte, error) {
	return json.Marshal(req)
}

// DecodeSandboxResponse 解析响应信封。ok=true 返回 Result(Output 原样,
// 顶层对象/尺寸契约由 LocalRunner.validateObjectOutput 宿侧复验——不信任
// 工具自查);ok=false 按 code 分类:ssrf/host/path → 携带对应哨兵的错误
// (errors.Is 可达),tool_failed/未知码/畸形 → 普通错误(runner 默认
// tool_failed 业务分类)。
func DecodeSandboxResponse(stdout []byte) (Result, error) {
	var resp SandboxResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return Result{}, fmt.Errorf("tools: sandbox envelope not valid JSON: %w", err)
	}
	if resp.OK {
		if len(resp.Output) == 0 {
			return Result{}, errors.New("tools: sandbox envelope ok without output")
		}
		return Result{Output: resp.Output}, nil
	}
	switch resp.Code {
	case SandboxCodeSSRFBlocked:
		return Result{}, fmt.Errorf("%w: %s", ErrSSRFBlocked, resp.Message)
	case SandboxCodeHostBlocked:
		return Result{}, fmt.Errorf("%w: %s", ErrHostBlocked, resp.Message)
	case SandboxCodePathBlocked:
		return Result{}, fmt.Errorf("%w: %s", ErrPathBlocked, resp.Message)
	case SandboxCodeToolFailed:
		return Result{}, fmt.Errorf("tools: sandbox tool_failed: %s", resp.Message)
	default:
		return Result{}, fmt.Errorf("tools: sandbox envelope unknown code %q: %s", resp.Code, resp.Message)
	}
}

// SandboxCodeForError helper 侧反向映射:工具错误 → 信封码(errors.Is 对
// 包内哨兵,深度包装可达);不可归类一律 tool_failed——未知形态不猜语义。
func SandboxCodeForError(err error) string {
	switch {
	case errors.Is(err, ErrSSRFBlocked):
		return SandboxCodeSSRFBlocked
	case errors.Is(err, ErrHostBlocked):
		return SandboxCodeHostBlocked
	case errors.Is(err, ErrPathBlocked):
		return SandboxCodePathBlocked
	default:
		return SandboxCodeToolFailed
	}
}
