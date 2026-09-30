package tools

// 工具包签名(T10.3,ARCHITECTURE §3.4「插件:P1 工具包签名」):发布物 =
// 签名的工具定义清单——工具名与安全相关配置(http 端点白名单、file_read
// 目录白名单)整体进签名面。签名 = Ed25519 对清单原始字节(签什么验
// 什么,无 canonical 重排;stdlib 零新依赖);发布侧持私钥(openssl
// 命令见 deploy/README.md §8),加载侧(api+worker)持公钥验签。
//
// fail closed 纪律(issue #88):验签失败即拒 = 装配错误 = 启动即败,
// 无降级开关(降级须 D- 拍板)。BRAID_TOOL_SIGNING_PUBLIC_KEY 在场即
// 签名态:BRAID_TOOL_PACKAGE(_SIGNATURE) 必须全配,unsigned 内置工具
// env(BRAID_HTTP_ENDPOINTS/BRAID_FILEREAD_DIRS)必须清空——开签名忘摘
// 旧键 = 响亮拒绝而非静默双源(配置漂移防)。整组缺省 = 签名未部署,
// 内置 env 路径逐字节现状(dev 栈零破坏)。t13.2 MCP 工具包接入时复用
// 本验签面(新增可声明工具 = 三处联动:knownPackageTools 词表、
// parseToolPackage 交叉携带检查、resolveToolSet 装配消费分支)。
//
// env 三键(base64,StdEncoding):
//
//	BRAID_TOOL_SIGNING_PUBLIC_KEY   32B Ed25519 公钥
//	BRAID_TOOL_PACKAGE              清单 JSON 原始字节
//	BRAID_TOOL_PACKAGE_SIGNATURE    64B Ed25519 签名

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// toolPackageVersion 清单格式版本;v1 唯一支持(错版本即拒)。
const toolPackageVersion = 1

// toolPackage 是签名清单的 Go 形态。字段面封闭(DisallowUnknownFields):
// 清单只声明工具与安全配置,多余字段 = 发布缺陷 = 拒。
type toolPackage struct {
	Version int                        `json:"version"`
	Tools   map[string]toolPackageTool `json:"tools"`
}

// toolPackageTool 是单工具声明。Endpoints(http)/Dirs(file_read)按工具
// 各归其位(交叉携带即拒——签名面内也不容声明歧义);值校验全权交给
// 既有构造器(与 env 路径同门:声明但空配置 = 构造器响亮拒)。
type toolPackageTool struct {
	Endpoints map[string]string `json:"endpoints,omitempty"`
	Dirs      []string          `json:"dirs,omitempty"`
}

// knownPackageTools:清单可声明的工具词表 = 内置装配面支持的载体。新增
// 工具名须三处联动(见文件头注释)。db_query(T10.4)在此只声明**在场**,
// 不携带 DSN——凭证恒 env 通道(BRAID_DBQUERY_DSN),签名工件零凭证。
var knownPackageTools = map[string]struct{}{"http": {}, "file_read": {}, "db_query": {}}

// verifyToolPackage 加载侧验签原语:base64 → 长度面 → Ed25519 验签 →
// 严格清单解码与词表校验。任一步失败即拒(fail closed);成功返回解析
// 后的清单供装配消费。发布侧签名原语(signToolPackage)归测试侧——生产
// 构建只验不签,发布物由 openssl 产出(README §8)。
func verifyToolPackage(pubkeyB64, manifestB64, sigB64 string) (toolPackage, error) {
	pubRaw, err := base64.StdEncoding.DecodeString(pubkeyB64)
	if err != nil {
		return toolPackage{}, fmt.Errorf("tools: tool package public key: invalid base64: %w", err)
	}
	if len(pubRaw) != ed25519.PublicKeySize {
		return toolPackage{}, fmt.Errorf("tools: tool package public key: want base64 of %d-byte ed25519 key, got %d bytes",
			ed25519.PublicKeySize, len(pubRaw))
	}
	manifest, err := base64.StdEncoding.DecodeString(manifestB64)
	if err != nil {
		return toolPackage{}, fmt.Errorf("tools: tool package manifest: invalid base64: %w", err)
	}
	sigRaw, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return toolPackage{}, fmt.Errorf("tools: tool package signature: invalid base64: %w", err)
	}
	if len(sigRaw) != ed25519.SignatureSize {
		return toolPackage{}, fmt.Errorf("tools: tool package signature: want base64 of %d-byte ed25519 signature, got %d bytes",
			ed25519.SignatureSize, len(sigRaw))
	}
	if !ed25519.Verify(ed25519.PublicKey(pubRaw), manifest, sigRaw) {
		return toolPackage{}, fmt.Errorf("tools: tool package signature mismatch")
	}
	return parseToolPackage(manifest)
}

// parseToolPackage 严格解码 + 结构校验(验签之后、装配之前)。签名已绑定
// 字节,这里的校验管的是「合法签名的畸形清单」(发布侧缺陷)。
func parseToolPackage(manifest []byte) (toolPackage, error) {
	dec := json.NewDecoder(bytes.NewReader(manifest))
	dec.DisallowUnknownFields()
	var p toolPackage
	if err := dec.Decode(&p); err != nil {
		return toolPackage{}, fmt.Errorf("tools: tool package manifest: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return toolPackage{}, fmt.Errorf("tools: tool package manifest: trailing data")
	}
	if p.Version != toolPackageVersion {
		return toolPackage{}, fmt.Errorf("tools: tool package version %d unsupported (want %d)",
			p.Version, toolPackageVersion)
	}
	// "tools" 键必须在场且非 null:键缺席与 null 均解码为 nil map,显式
	// {} 为非 nil 空 map——恰好三分。空发布 = 显式 {}。
	if p.Tools == nil {
		return toolPackage{}, fmt.Errorf(`tools: tool package manifest: "tools" is required`)
	}
	// 遍历序确定化:多缺陷清单的报错可复现(map 随机序下每次启动
	// 可能报出不同缺陷名,排障不可比对)。
	for _, name := range slices.Sorted(maps.Keys(p.Tools)) {
		tp := p.Tools[name]
		if _, known := knownPackageTools[name]; !known {
			return toolPackage{}, fmt.Errorf("tools: tool package declares unknown tool %q", name)
		}
		if name == "http" && len(tp.Dirs) > 0 {
			return toolPackage{}, fmt.Errorf(`tools: tool package tool "http" must not carry dirs`)
		}
		if name == "file_read" && len(tp.Endpoints) > 0 {
			return toolPackage{}, fmt.Errorf(`tools: tool package tool "file_read" must not carry endpoints`)
		}
		if name == "db_query" && (len(tp.Endpoints) > 0 || len(tp.Dirs) > 0) {
			return toolPackage{}, fmt.Errorf(`tools: tool package tool "db_query" is presence-only (DSN stays in BRAID_DBQUERY_DSN; credentials never enter signed artifacts)`)
		}
	}
	return p, nil
}

// toolSet 是装配原料的解析结果:内置 env 路径与签名包路径共同的目标
// 形态。Declared 位区分「未声明」(不注册)与「声明但配置空」(构造器
// 响亮拒)——与 env 路径 raw!="" 的既有语义对齐。source 进错误包装:
// legacy 路径逐字节保留键名报错,签名路径如实归因。
type toolSet struct {
	httpSource       string
	httpDeclared     bool
	httpEndpoints    map[string]string
	filereadSource   string
	filereadDeclared bool
	filereadDirs     []string
	dbSource         string
	dbDeclared       bool
	dbDSN            string // 凭证:恒 env 通道,不进签名工件(见 resolveToolSet 注释)
}

// resolveToolSet 是 T10.3 env 矩阵(语义全表见文件头注释):
//
//	pubkey 缺席 + package/signature 在场  → 拒(无钥不可验,fail closed)
//	pubkey 缺席 + 两者全缺                → 内置 env 路径(逐字节现状)
//	pubkey 在场 + legacy 工具键非空       → 拒(unsigned 源,配置漂移防)
//	                                         (BRAID_DBQUERY_DSN 例外:T10.4 起
//	                                         DSN 是凭证通道而非工具配置,两路径
//	                                         恒 env 注入,签名工件零凭证)
//	pubkey 在场 + package/signature 半配  → 拒(半配置即败)
//	pubkey 在场 + 验签/清单校验失败       → 拒(验签失败即拒)
//	pubkey 在场 + 验签通过                → 清单为唯一工具来源
func resolveToolSet(getenv func(string) string) (toolSet, error) {
	pub := getenv("BRAID_TOOL_SIGNING_PUBLIC_KEY")
	pkg := getenv("BRAID_TOOL_PACKAGE")
	sig := getenv("BRAID_TOOL_PACKAGE_SIGNATURE")
	if pub == "" {
		if pkg != "" || sig != "" {
			return toolSet{}, fmt.Errorf(
				"tools: BRAID_TOOL_PACKAGE(_SIGNATURE) set without BRAID_TOOL_SIGNING_PUBLIC_KEY: cannot verify, fail closed")
		}
		return legacyToolSet(getenv)
	}
	if v := getenv("BRAID_HTTP_ENDPOINTS"); v != "" {
		return toolSet{}, fmt.Errorf(
			"tools: BRAID_HTTP_ENDPOINTS forbidden while BRAID_TOOL_SIGNING_PUBLIC_KEY is set (declare endpoints in the signed package)")
	}
	if v := getenv("BRAID_FILEREAD_DIRS"); v != "" {
		return toolSet{}, fmt.Errorf(
			"tools: BRAID_FILEREAD_DIRS forbidden while BRAID_TOOL_SIGNING_PUBLIC_KEY is set (declare dirs in the signed package)")
	}
	if pkg == "" || sig == "" {
		return toolSet{}, fmt.Errorf(
			"tools: BRAID_TOOL_SIGNING_PUBLIC_KEY set: BRAID_TOOL_PACKAGE and BRAID_TOOL_PACKAGE_SIGNATURE are both required")
	}
	p, err := verifyToolPackage(pub, pkg, sig)
	if err != nil {
		// 中性前缀:verifyToolPackage 内部分量已各自定位(公钥/清单/签名),
		// 外层不再点名单一 env 键——坏公钥/坏签名归因见内层消息。
		return toolSet{}, fmt.Errorf("tools: tool package rejected: %w", err)
	}
	var ts toolSet
	if t, ok := p.Tools["http"]; ok {
		ts.httpSource, ts.httpDeclared, ts.httpEndpoints = "BRAID_TOOL_PACKAGE", true, t.Endpoints
	}
	if t, ok := p.Tools["file_read"]; ok {
		ts.filereadSource, ts.filereadDeclared, ts.filereadDirs = "BRAID_TOOL_PACKAGE", true, t.Dirs
	}
	if _, ok := p.Tools["db_query"]; ok {
		// 声明在场即 declared;DSN 恒 env 通道——签名清单是可分发/可落
		// 诊断的工件,凭证永不入内。声明但 DSN 缺席 = 部署半配置,交给
		// NewDBQueryTool 响亮拒(http 声明空 endpoints 同语义)。
		ts.dbSource, ts.dbDeclared, ts.dbDSN = "BRAID_TOOL_PACKAGE", true, strings.TrimSpace(getenv("BRAID_DBQUERY_DSN"))
	}
	return ts, nil
}

// legacyToolSet:既有内置 env 路径(raw 非空 = 声明;解析失败键名报错,
// 语义与 T3.5 起逐字节一致)。db_query 的声明位 = BRAID_DBQUERY_DSN 在场
// (值校验归 NewDBQueryTool;本层不解析凭证)。
func legacyToolSet(getenv func(string) string) (toolSet, error) {
	var ts toolSet
	if raw := getenv("BRAID_HTTP_ENDPOINTS"); raw != "" {
		ts.httpSource, ts.httpDeclared = "BRAID_HTTP_ENDPOINTS", true
		eps := make(map[string]string)
		if err := json.Unmarshal([]byte(raw), &eps); err != nil {
			return toolSet{}, fmt.Errorf("tools: BRAID_HTTP_ENDPOINTS: want JSON object {alias: base_url}: %w", err)
		}
		ts.httpEndpoints = eps
	}
	if raw := getenv("BRAID_FILEREAD_DIRS"); raw != "" {
		ts.filereadSource, ts.filereadDeclared = "BRAID_FILEREAD_DIRS", true
		var dirs []string
		if err := json.Unmarshal([]byte(raw), &dirs); err != nil {
			return toolSet{}, fmt.Errorf("tools: BRAID_FILEREAD_DIRS: want JSON array of absolute dirs: %w", err)
		}
		ts.filereadDirs = dirs
	}
	if raw := strings.TrimSpace(getenv("BRAID_DBQUERY_DSN")); raw != "" {
		ts.dbSource, ts.dbDeclared, ts.dbDSN = "BRAID_DBQUERY_DSN", true, raw
	}
	return ts, nil
}
