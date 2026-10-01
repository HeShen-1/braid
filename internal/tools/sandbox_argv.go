package tools

// 沙箱 argv 白名单构造器(T10.1):docker CLI 是 root 等价注入面,本构造器
// 是纯函数——旗标序列硬编码,一切变量值(容器名/镜像/资源值/挂载源/工具
// 名)过值域校验,非法值构造期拒(schemaSuffixRe 同款纪律)。workflow 可
// 控字符串只经 stdin 信封,构造器无该参数 = 结构性不可达。
//
// 旗标集 = docs/designs/t101-sandbox-baseline.md §2 硬化矩阵(15 控制)
// 的静态锚:golden 测试钉死顺序,漂移即红。

import (
	"fmt"
	"regexp"
	"strconv"
)

// sandboxEntrypoint 镜像固定入口(deploy/sandbox.Dockerfile ENTRYPOINT)。
const sandboxEntrypoint = "/usr/local/bin/braid-sandbox-exec"

// sandboxContainerNameRe 容器名值域:字母数字开头,[A-Za-z0-9_.-] 续位,
// ≤128 字符(拒空白/逗号/前导 dash 变旗标)。
var sandboxContainerNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// sandboxMountDirRe 挂载目录值域:绝对路径、受限字符集——逗号是 --mount
// 选项分隔符(注入面),空白/控制字符一并拒;目录不可为根("/")本身。
var sandboxMountDirRe = regexp.MustCompile(`^/[A-Za-z0-9_./-]*[A-Za-z0-9_-]$`)

// sandboxSandboxableTools 可入沙箱的工具词表(网络位按工具钉死:
// file_read 恒 none 零出网;http/db_query 经 cfg.EgressNetwork 内网出网
// ——db_query 的 MySQL 连接过 egress 代理 CONNECT 隧道,T10.4/D-2026-09-30
// -t104-dbquery-egress;T10.2 换装点 = SandboxSpec 网络字段,基线 §4)。
var sandboxSandboxableTools = map[string]struct{}{
	"http":      {},
	"file_read": {},
	"db_query":  {},
}

// sandboxNetworkFor 工具 → docker --network 位:file_read 恒 "none";
// http/db_query 恒 egress 内网名(docker 模式解析期已填默认,空值 = 调用方
// 绕过解析的装配缺陷,响亮败——绝不静默回落 bridge:过渡网位已随 T10.2 死)。
func sandboxNetworkFor(cfg sandboxConfig, tool string) (string, error) {
	if tool == "file_read" {
		return "none", nil
	}
	if cfg.EgressNetwork == "" || !sandboxNetworkRe.MatchString(cfg.EgressNetwork) {
		return "", fmt.Errorf("tools: sandbox carrier requires egress network (BRAID_TOOL_SANDBOX_EGRESS_NETWORK, default %q)", sandboxEgressNetworkDefault)
	}
	return cfg.EgressNetwork, nil
}

// buildSandboxRunArgv 构造 docker run 参数(不含 docker 二进制名本身)。
// 纯函数:只消费已过 config 校验的值 + 逐项再校验的 name/mounts/tool。
func buildSandboxRunArgv(cfg sandboxConfig, name, tool string, mounts []string) ([]string, error) {
	if !cfg.Docker {
		return nil, fmt.Errorf("tools: sandbox argv requires docker mode")
	}
	if !sandboxContainerNameRe.MatchString(name) {
		return nil, fmt.Errorf("tools: sandbox container name %q invalid (want ^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$)", name)
	}
	if _, ok := sandboxSandboxableTools[tool]; !ok {
		return nil, fmt.Errorf("tools: sandbox tool %q not in vocabulary (want http|file_read)", tool)
	}
	// T15.3 owner 门(D-t151 #5):docker 模式下 owner 必填——每容器打
	// braid.sandbox.owner label 是启动清扫按家过滤的前提,缺 owner 的容器
	// 会变成「代码路径不可回收、只等 runbook 全局原语」的孤儿。api 进程
	// 结构性不可达(不执行工具);空 owner = 装配链断了,响亮败。
	if cfg.Owner == "" {
		return nil, fmt.Errorf("tools: sandbox argv requires owner identity (BRAID_TOOL_SANDBOX_OWNER, set by worker startup from WorkerID; sweep scoping per D-t151 #5)")
	}
	if !sandboxOwnerRe.MatchString(cfg.Owner) {
		return nil, fmt.Errorf("tools: sandbox owner %q invalid (want ^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$)", cfg.Owner)
	}
	network, err := sandboxNetworkFor(cfg, tool)
	if err != nil {
		return nil, err
	}
	for _, d := range mounts {
		if !sandboxMountDirRe.MatchString(d) {
			return nil, fmt.Errorf("tools: sandbox mount dir %q invalid (want absolute path, charset [A-Za-z0-9_./-], no comma/space)", d)
		}
	}

	args := []string{
		// --interactive 是 stdin 信封通道的管道前提(D-t101 #2):无 -i 时
		// docker run 不挂 stdin,信封静默丢失 → exit 0 空输出(本机 docker
		// 29.1 实证;fake shim 是 Go exec 直连管道测不出,argv golden 钉死)。
		"run", "--rm", "--interactive",
		"--name", name,
		"--label", "braid.sandbox=1",
		"--label", "braid.tool=" + tool,
		// T15.3 owner label(D-t151 #5):清扫注册面从全局单 label 升格为
		// (全局, owner) 二元组——SandboxSweep 按本 label 过滤只清自家,
		// 多 worker 同 daemon 不互杀;kill -9 孤儿身份可知(归属可判)。
		"--label", "braid.sandbox.owner=" + cfg.Owner,
		"--read-only",
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=16m",
		"--memory", cfg.Memory,
		"--memory-swap", cfg.Memory, // = memory:swap 关死,零换页余量(基线 #4)
		"--cpus", cfg.CPUs,
		"--pids-limit", strconv.Itoa(cfg.PidsLimit),
		"--ulimit", "nofile=1024:1024",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--user", "65532:65532",
		"--init",
		"--log-driver", "json-file", "--log-opt", "max-size=5m", "--log-opt", "max-file=1",
	}
	if tool == "file_read" {
		args = append(args, "--network", network)
		for _, d := range mounts {
			// 同绝对路径挂载:容器内目录树与宿主词形同构,双门代码与词形
			// 回显契约零漂移(基线 #3)。
			args = append(args, "--mount", "type=bind,src="+d+",dst="+d+",readonly")
		}
	} else {
		// http:T10.2 网络位换装——bridge → egress 内网(--internal,worker
		// 启动段幂等创建),容器唯一可达面 = 网关上的代理监听,直连在
		// 网络层结构性默认拒绝(egress.go 文件头)。
		args = append(args, "--network", network)
	}
	args = append(args,
		"--entrypoint", sandboxEntrypoint,
		cfg.Image,
		tool,
	)
	return args, nil
}
