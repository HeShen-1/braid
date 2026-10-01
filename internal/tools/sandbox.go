package tools

// 容器沙箱载体(T10.1,D-2026-09-28-t101):ARCHITECTURE.md L74 锁定
// 「P1 换 Docker 容器沙箱不动核心」——载体在 Tool 接口之下同名替换
// (registry.go 注释锚),LocalRunner/Registry/nodes 零改动。基线全表见
// docs/designs/t101-sandbox-baseline.md。
//
// env 词表(构造期全量校验,坏值键名响亮败——装配缺陷启动即现):
//
//	BRAID_TOOL_SANDBOX                 off(未设/空=缺省,行为逐字节不变)| docker
//	BRAID_TOOL_SANDBOX_IMAGE           镜像引用,默认 braid-tool-sandbox:1
//	BRAID_TOOL_SANDBOX_MEMORY          docker 旗标原样,默认 256m
//	BRAID_TOOL_SANDBOX_CPUS            docker 旗标原样,默认 0.5
//	BRAID_TOOL_SANDBOX_PIDS            整数 1..4096,默认 64
//	BRAID_TOOL_SANDBOX_MAX_CONCURRENCY 并发容器上界 1..1000,默认 8
//	BRAID_TOOL_SANDBOX_EGRESS_NETWORK  egress 内网名基段(docker 网络名值域 ≤64;worker egress 启动段另限基段 ≤55——实例段 8 hex 预算,超限拒启),默认 braid-sbx-egress(T10.2/T15.4)
//	BRAID_TOOL_SANDBOX_EGRESS_ADDR     egress 代理地址 http://host:port(worker 启动自算自写,装配单读;
//	                                   api 进程不执行工具,缺省 nil = 信封不带 proxy 字段)
//
// off 模式对沙箱键零消费(零 docker 代码路径):api 进程永不触 docker 的
// 结构保证。键家族 BRAID_TOOL_SANDBOX* 结构性不进容器 env(防 helper 再入
// wrap,基线 §5;T10.2 的代理地址经 stdin 信封 Config 下发,不经 env)。

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// sandboxImageRefRe 镜像引用值域:首字符字母/数字(拒前导 -,防变旗标),
// 其后 [A-Za-z0-9._/:-],可选 @sha256:<64 hex> digest 钉死形态。空白、
// shell 元字符、前导 dash 一律拒——镜像引用最终落 docker argv,构造期
// 白名单是唯一防线(schemaSuffixRe 同款纪律)。
var sandboxImageRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]*(@sha256:[0-9a-f]{64})?$`)

// sandboxImageDefault 缺省镜像引用(deploy/README.md §7 同源登记)。
// sandbox_image_test.go 的 e2e tag 守卫引用本常量:--target sandbox-e2e
// 恒不得构建进缺省引用(防 ephemeral CA 镜像滞留 daemon 被 prod Probe
// 放行,基线 §3)。
const sandboxImageDefault = "braid-tool-sandbox:1"

// sandboxMemoryRe docker 内存旗标值域:纯数字或数字+单位(b/k/m/g/t)。
var sandboxMemoryRe = regexp.MustCompile(`^[0-9]+[bkmgt]?$`)

// sandboxCPUsRe docker cpus 旗标值域:小数十进制,语义上须 >0。
var sandboxCPUsRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// sandboxNetworkRe docker 网络名值域(egress 内网名最终落 docker argv
// --network 位,镜像引用同款纪律:拒前导 dash/空白/逗号等旗标注入形)。
var sandboxNetworkRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// sandboxEgressNetworkDefault 缺省 egress 内网名(deploy/README §egress
// 同源登记;worker 启动幂等创建 --internal 网络,见 SandboxStartupCheck)。
const sandboxEgressNetworkDefault = "braid-sbx-egress"

// sandboxConfig 沙箱载体配置(不可变值对象;docker 二进制解析不在构造——
// 构造保持纯配置惰性,worker 侧 Probe 负责 LookPath/daemon/镜像检查)。
type sandboxConfig struct {
	Docker         bool
	Image          string
	Memory         string
	CPUs           string
	PidsLimit      int
	MaxConcurrency int
	// EgressNetwork http 载体的 --network 位(T10.2 换装:bridge → egress
	// 内网;docker 模式解析期恒非空,缺省 sandboxEgressNetworkDefault)。
	EgressNetwork string
	// EgressAddr egress 代理地址(信封 proxy 字段原料;api 装配恒 nil,
	// worker 由启动段自写 env 后装配非 nil——见文件头 env 词表)。
	EgressAddr *url.URL
}

// sandboxConfigFromEnv 解析并校验沙箱 env 词表;off 模式(含未设)不读
// 其余键,返回零 Docker 字段。
func sandboxConfigFromEnv(getenv func(string) string) (sandboxConfig, error) {
	var cfg sandboxConfig
	mode := strings.TrimSpace(getenv("BRAID_TOOL_SANDBOX"))
	switch mode {
	case "", "off": // 未设/空 = 缺省 = 现状逐字节(dev 栈零 docker 照跑)
	case "docker":
		cfg.Docker = true
	default:
		return cfg, fmt.Errorf("tools: BRAID_TOOL_SANDBOX=%q: want off|docker", mode)
	}
	if !cfg.Docker {
		return cfg, nil
	}

	cfg.Image = getenv("BRAID_TOOL_SANDBOX_IMAGE")
	if cfg.Image == "" {
		cfg.Image = sandboxImageDefault
	}
	if !sandboxImageRefRe.MatchString(cfg.Image) {
		return sandboxConfig{}, fmt.Errorf("tools: BRAID_TOOL_SANDBOX_IMAGE=%q: invalid image reference (want repo[:tag][@sha256:<digest>], no leading dash)", cfg.Image)
	}

	cfg.Memory = getenv("BRAID_TOOL_SANDBOX_MEMORY")
	if cfg.Memory == "" {
		cfg.Memory = "256m"
	}
	if !sandboxMemoryRe.MatchString(cfg.Memory) {
		return sandboxConfig{}, fmt.Errorf("tools: BRAID_TOOL_SANDBOX_MEMORY=%q: want <digits>[b|k|m|g|t]", cfg.Memory)
	}
	// 正则放行 "0"/"00"/"0m",而 docker 对 --memory 0 = 不设限——恰是
	// 基线 #4 要防的威胁,须显式拒零(镜像 CPUS 分支同款纪律)。
	if n, err := strconv.ParseInt(strings.TrimRight(cfg.Memory, "bkmgt"), 10, 64); err != nil || n <= 0 {
		return sandboxConfig{}, fmt.Errorf("tools: BRAID_TOOL_SANDBOX_MEMORY=%q: want positive <digits>[b|k|m|g|t]", cfg.Memory)
	}

	cfg.CPUs = getenv("BRAID_TOOL_SANDBOX_CPUS")
	if cfg.CPUs == "" {
		cfg.CPUs = "0.5"
	}
	if !sandboxCPUsRe.MatchString(cfg.CPUs) {
		return sandboxConfig{}, fmt.Errorf("tools: BRAID_TOOL_SANDBOX_CPUS=%q: want decimal > 0", cfg.CPUs)
	}
	if v, err := strconv.ParseFloat(cfg.CPUs, 64); err != nil || v <= 0 {
		return sandboxConfig{}, fmt.Errorf("tools: BRAID_TOOL_SANDBOX_CPUS=%q: want decimal > 0", cfg.CPUs)
	}

	cfg.PidsLimit = sandboxIntFromEnv(getenv, "BRAID_TOOL_SANDBOX_PIDS", 64, 1, 4096)
	if cfg.PidsLimit == 0 {
		return sandboxConfig{}, fmt.Errorf("tools: BRAID_TOOL_SANDBOX_PIDS: want integer in [1, 4096]")
	}

	cfg.MaxConcurrency = sandboxIntFromEnv(getenv, "BRAID_TOOL_SANDBOX_MAX_CONCURRENCY", 8, 1, 1000)
	if cfg.MaxConcurrency == 0 {
		return sandboxConfig{}, fmt.Errorf("tools: BRAID_TOOL_SANDBOX_MAX_CONCURRENCY: want integer in [1, 1000]")
	}

	// T10.2 egress 段:内网名(值域过 argv 注入面防线)+ 代理地址(词形
	// 单门 parseEgressProxyURL;空 = 未设——worker 启动段恒写,api 恒空)。
	cfg.EgressNetwork = strings.TrimSpace(getenv("BRAID_TOOL_SANDBOX_EGRESS_NETWORK"))
	if cfg.EgressNetwork == "" {
		cfg.EgressNetwork = sandboxEgressNetworkDefault
	}
	if !sandboxNetworkRe.MatchString(cfg.EgressNetwork) {
		return sandboxConfig{}, fmt.Errorf("tools: BRAID_TOOL_SANDBOX_EGRESS_NETWORK=%q: invalid docker network name (want ^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$)", cfg.EgressNetwork)
	}
	if raw := strings.TrimSpace(getenv("BRAID_TOOL_SANDBOX_EGRESS_ADDR")); raw != "" {
		u, err := parseEgressProxyURL(raw)
		if err != nil {
			return sandboxConfig{}, fmt.Errorf("tools: BRAID_TOOL_SANDBOX_EGRESS_ADDR: %w", err)
		}
		cfg.EgressAddr = u
	}
	return cfg, nil
}

// sandboxIntFromEnv 读整数键:空取默认;越界/非整数返回 0(调用方键名报错)。
func sandboxIntFromEnv(getenv func(string) string, key string, def, lo, hi int) int {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < lo || v > hi {
		return 0
	}
	return v
}
