package tools

// file_read 内置工具(T3.5,issue #32):目录白名单内读文本文件供下游
// LLM 链消费(doc_compliance.json 示例:文件路径输入 → file_read → 解析)。
//
// 输入契约(DSL 形态即 testdata doc_compliance.json):
//
//	path 必填,字符串——白名单内文件的绝对路径
//
// 白名单语义(路径防线,验收:白名单外路径被拒):
//  1. 仅绝对路径(相对路径依赖 cwd,白名单无法锚定);
//  2. Clean 后对「词形根 ∪ 解析根」做边界感知包含检查(dir == path 或
//     dir + 分隔符前缀——"/a/sub" 不得匹配 "/a/subway/x");白名单外在
//     此即拒,零文件系统触达(不泄漏存在性 oracle);
//  3. EvalSymlinks 后对「解析根」终判——白名单目录里一枚指向外部的符号
//     链接即可走私(威胁模型:工作流作者控制路径字符串而非文件系统,
//     既有链接在此被拒;执行期主动换链需要 FS 写权 = 已越权,TOCTOU
//     不在此扩面,P1 需要时走 openat 链)。白名单目录自身是符号链接
//     挂载点时由构造期解析根吸收(否则词形路径恒被拒);
//  4. 仅 regular file(拒目录/设备/fifo——fifo 读会阻塞 worker);
//  5. Stat 尺寸先检 + LimitReader 双防线封顶。
//
// content 以 Go string 原样承载:面向 UTF-8 文本(下游是 LLM prompt);
// 非 UTF-8 字节经 JSON 序列化被替换字符规整(U+FFFD),二进制文件不属
// P0 契约。白名单源 = BRAID_FILEREAD_DIRS(builtin.go 单源装配)。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultMaxFileRead 是单次读取字节上限(与 DefaultMaxHTTPBody/
// DefaultMaxStdout 同量级防 OOM 界)。
const DefaultMaxFileRead = 8 << 20 // 8 MiB

// ErrPathBlocked 是路径白名单拒绝的哨兵错误(白名单外路径/相对路径/
// 符号链接逃逸共用)。映射契约同 ErrSSRFBlocked:裸错误上抛,runner
// errors.Is 映射 CodePathBlocked。
var ErrPathBlocked = errors.New("tools: path not allowlisted")

// FileReadConfig 是 file_read 工具的构造配置。Dirs:绝对目录白名单
// (Clean 后使用);MaxBytes ≤0 取 DefaultMaxFileRead。
type FileReadConfig struct {
	Dirs     []string
	MaxBytes int64
}

// FileReadTool 是内置 file_read 工具(Tool 接口进程内载体)。
type FileReadTool struct {
	lex  []string // 词形根(Clean 后原样):首道包含检查,白名单外零 FS 触达
	real []string // 解析根(EvalSymlinks,构造期失败回退词形):解析后终判
	max  int64
}

// pseudoFSRoots 是构造期结构化拒绝的伪文件系统根(T3.5 审计 LOW:
// /proc/self/environ 是 root 下的 regular file,白名单 /proc 会把 worker
// 进程环境——DSN、LLM key——变成工作流可读内容;相对目录都响亮拒了,
// 这里不该静默。/dev 不拒:设备文件非 regular 被结构性拒,/dev/shm 是
// 合法 tmpfs)。
var pseudoFSRoots = []string{"/proc", "/sys"}

// NewFileReadTool 构造 file_read 工具;空白名单、相对目录、伪文件系统根
// 响亮拒。
func NewFileReadTool(cfg FileReadConfig) (*FileReadTool, error) {
	if len(cfg.Dirs) == 0 {
		return nil, fmt.Errorf("tools: file_read tool with empty dir allowlist")
	}
	lex := make([]string, 0, len(cfg.Dirs))
	real := make([]string, 0, len(cfg.Dirs))
	for _, d := range cfg.Dirs {
		if !filepath.IsAbs(d) {
			return nil, fmt.Errorf("tools: file_read allowlist dir must be absolute, got %q", d)
		}
		c := filepath.Clean(d)
		for _, bad := range pseudoFSRoots {
			if c == bad || strings.HasPrefix(c, bad+string(filepath.Separator)) {
				return nil, fmt.Errorf("tools: file_read allowlist dir %q under pseudo-filesystem %q (worker credentials readable)", d, bad)
			}
		}
		lex = append(lex, c)
		// 解析根:目录是符号链接挂载点时(/a → /mnt/data),文件解析后的
		// 真实路径不再位于词形 /a 下,不解析则白名单内合法文件恒被拒。
		// 目录构造期不存在(EvalSymlinks 失败)时回退词形——挂载可能晚于
		// 启动;per-call 文件级解析仍全量把关,降级面 = 该目录暂不可用,
		// 不扩大攻击面。
		r := c
		if resolved, err := filepath.EvalSymlinks(c); err == nil {
			r = resolved
		}
		// 解析根同样过伪文件系统检查:白名单目录是符号链接别名指向
		// /proc 的现场(词形无辜、解析失守——首道与终判都以 real 根
		// 放行)在此响亮拒。
		for _, bad := range pseudoFSRoots {
			if r == bad || strings.HasPrefix(r, bad+string(filepath.Separator)) {
				return nil, fmt.Errorf("tools: file_read allowlist dir %q resolves to pseudo-filesystem %q (worker credentials readable)", d, bad)
			}
		}
		real = append(real, r)
	}
	max := cfg.MaxBytes
	if max <= 0 {
		max = DefaultMaxFileRead
	}
	return &FileReadTool{lex: lex, real: real, max: max}, nil
}

// Name 满足 Tool 接口(与 DSL tool 词表对齐:testdata doc_compliance.json)。
func (f *FileReadTool) Name() string { return "file_read" }

// Run 读取白名单内文件。错误语义:ErrPathBlocked 携带者 = 裸错误(runner
// 分类 CodePathBlocked);其余(缺文件/权限/超限/非 regular)= 业务失败
// 裸错误(→ CodeToolFailed)。读取上限 8 MiB 且本文件读取快,不设 ctx
// 门(硬契约「ctx 强制 attempt deadline」是 http 外部调用面的)。
func (f *FileReadTool) Run(_ context.Context, call Call) (Result, error) {
	path, err := requiredString(call.Inputs, "path")
	if err != nil {
		return Result{}, fmt.Errorf("tools: file_read: %w", err)
	}
	if !filepath.IsAbs(path) {
		return Result{}, fmt.Errorf("%w: relative path %q (allowlist roots are absolute)", ErrPathBlocked, path)
	}
	clean := filepath.Clean(path)
	// 首道:词形/解析根任一命中即过(白名单目录是符号链接挂载点时词形
	// 命中;挂载晚于启动时解析根回退词形,两者同集)。白名单外在零
	// 文件系统触达下即拒——不向工作流作者泄漏主机路径存在性 oracle。
	if !underAnyDir(f.lex, clean) && !underAnyDir(f.real, clean) {
		return Result{}, fmt.Errorf("%w: %q", ErrPathBlocked, clean)
	}
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return Result{}, fmt.Errorf("tools: file_read %q: %w", clean, err)
	}
	if !underAnyDir(f.real, real) {
		if clean != real {
			// 逃逸诊断只报词形路径,不含解析目标(审计 LOW:error_message
			// 对工作流作者可见,逃逸目标会把白名单外主机绝对路径的存在性
			// 拼写给作者;目标详情属服务端运维面)。
			return Result{}, fmt.Errorf("%w: symlink escapes allowlist: %q", ErrPathBlocked, clean)
		}
		return Result{}, fmt.Errorf("%w: %q", ErrPathBlocked, clean)
	}
	info, err := os.Stat(real)
	if err != nil {
		return Result{}, fmt.Errorf("tools: file_read %q: %w", real, err)
	}
	if !info.Mode().IsRegular() {
		return Result{}, fmt.Errorf("tools: file_read %q: not a regular file (mode %s)", real, info.Mode())
	}
	if info.Size() > f.max {
		return Result{}, fmt.Errorf("tools: file_read %q: size %d exceeds %d bytes", real, info.Size(), f.max)
	}
	fh, err := os.Open(real) //nolint:gosec // 白名单+regular+尺寸已检;竞态窗口见文件头注释
	if err != nil {
		return Result{}, fmt.Errorf("tools: file_read %q: %w", real, err)
	}
	defer func() { _ = fh.Close() }()
	// Stat 尺寸之后的增长由 LimitReader 封顶(双防线)。
	data, err := io.ReadAll(io.LimitReader(fh, f.max+1))
	if err != nil {
		return Result{}, fmt.Errorf("tools: file_read %q: %w", real, err)
	}
	if int64(len(data)) > f.max {
		return Result{}, fmt.Errorf("tools: file_read %q: size exceeds %d bytes", real, f.max)
	}
	// path 回显调用方的 Clean 词形(审计 LOW:real 是 EvalSymlinks 解析形,
	// 挂载点场景会把底层卷/挂载拓扑泄漏进 node_execution.output;词形回显
	// 同时天然支撑 {{nodes.x.output.path}} 回灌——词形过首道门)。解析
	// 语义不变:读取与终判仍锚 real。
	raw, err := marshalToolOutput(map[string]any{
		"path":    clean,
		"size":    json.Number(strconv.Itoa(len(data))),
		"content": string(data),
	})
	if err != nil {
		return Result{}, fmt.Errorf("tools: file_read: %w", err)
	}
	return Result{Output: raw}, nil
}

// underAnyDir 边界感知包含检查:p 等于某白名单目录,或以「目录 + 路径
// 分隔符」为前缀(裸前缀会把 /a/subway 误判进 /a/sub)。根 "/" 特判
// (审计 MEDIUM:d+Separator = "//" 不匹配任何路径,白名单 "/" 曾静默
// 退化为全拒):"/" 的语义 = 全部绝对路径。
func underAnyDir(dirs []string, p string) bool {
	for _, d := range dirs {
		if d == "/" {
			return true
		}
		if p == d || strings.HasPrefix(p, d+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
