package snowflake

import (
	"fmt"
	"strconv"
)

// NodeMode 是节点号解析的三层形态(D-2026-10-01-t151 决策 2):显式
// env 数字 > 内建默认(1/2,视同显式 claim) > 字面 "auto" 哨兵(租约
// 池分配);缺省 = 内建默认。
type NodeMode uint8

const (
	// NodeExplicit 是显式节点号(env 数字或内建默认):按号直达租约表,
	// 同 holder 复占即时、被他持拒启。限单副本/compose 使用——k8s
	// RollingUpdate(maxSurge=1/maxUnavailable=0)下 pinned-env 永久僵局,
	// 多副本一律 NodeAuto。
	NodeExplicit NodeMode = iota
	// NodeAuto 是池分配:线性取空闲号(缺行或过期行),acquire = INSERT
	// 抢注、PK 竞态败者 ≤3 次退避重扫后拒启。
	NodeAuto
)

// NodeSpec 是组件配置侧的节点号意向:三层解析在 ConfigFromEnv 收口成
// 本类型,cmd 装配把它交给租约仲裁(lease.go StartLease)——解析与仲裁
// 分离,组件包零 SQL 依赖。
type NodeSpec struct {
	Mode NodeMode
	Node uint16 // Mode == NodeExplicit 时有效(0..MaxNodeID)
}

// Explicit 返回显式节点号 spec(内建默认同构:1/2/3 视同显式 claim)。
func Explicit(node uint16) NodeSpec { return NodeSpec{Mode: NodeExplicit, Node: node} }

// Auto 返回池分配 spec(字面 "auto" 哨兵;组件内建缺省一律为 Explicit——
// api=1 / worker=2 / reaper=3,多副本部署显式声明本 spec)。
func Auto() NodeSpec { return NodeSpec{Mode: NodeAuto} }

// ParseNodeSpec 解析 env 原文(D-t151 决策 2 三层):空 → def(内建默认);
// 字面 "auto" → Auto;数字 0..MaxNodeID → 显式。其余(非数字/越界/
// 负数/大小写变体)启动即败——越界在转换前拒绝,镜像各组件 uint16 截断
// 防护先例(65536 mod 2^16 = 0 会静默折叠回合法节点域)。
func ParseNodeSpec(raw string, def NodeSpec) (NodeSpec, error) {
	switch raw {
	case "":
		return def, nil
	case "auto":
		return Auto(), nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > int(MaxNodeID) {
		return NodeSpec{}, fmt.Errorf("snowflake: node spec %q: want \"auto\" or 0..%d", raw, MaxNodeID)
	}
	return Explicit(uint16(n)), nil
}
