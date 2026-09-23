// Package nodes 实现节点执行器(T3.6 起):worker.Executor 缝的真实节点
// 实现,按 node type 路由。Mux 是 worker 侧唯一装配点(cmd/worker 注入
// DefReader + 各类型执行器);类型判别用 dsl.Node 的判别指针而非裸字符串,
// 与 DSL 解析面同构。
//
// 失败语义(GATE 1 拍板,worker.Outcome 语义分立的延伸):
//   - no_executor:类型无已装配执行器 = 部署配置错,终态 FAILED 收口可见,
//     不留 RUNNING 行给 reaper 判死循环重派(同 attempt 重派也不会凭空
//     长出执行器);
//   - node_def_missing:定义行缺失 = 数据缺陷,同上业务失败收口;
//   - 定义读取的 DB 错误 = 基础设施失败,error 返回交 asynq 重试接管。
//
// approval 类型(T4.1)是纯挂起决策:不发 FAILED,走 Outcome.Suspend 通道
// (见 approval.go)。
package nodes

import (
	"context"
	"errors"
	"fmt"

	"braid/internal/engine"
	"braid/internal/engine/dsl"
	"braid/internal/worker"
)

// DefReader 是定义读取缝(scheduler.NodeDefinition 的窄接口,依赖倒置:
// 生产实现唯一 *scheduler.Scheduler,fake 只存在于本包 _test.go)。
type DefReader interface {
	NodeDefinition(ctx context.Context, nodeExecID uint64) (*dsl.Node, error)
}

// LLMRunner 是 llm 节点的执行面(Mux 内部路由缝;LLMExecutor 实现)。
type LLMRunner interface {
	Run(ctx context.Context, req worker.Request, spec *dsl.LLMNode) (worker.Outcome, error)
}

// Mux 实现 worker.Executor:按钉住 DSL 节点类型分派(llm/tool/approval 三类
// 执行器已接线;P1 parallel 等未知类型落 no_executor 终态)。
type Mux struct {
	defs     DefReader
	llm      LLMRunner
	tool     ToolRunner
	approval ApprovalRunner
}

// NewMux 构造执行器路由;nil 依赖响亮拒绝(装配缺陷启动即现)。
func NewMux(defs DefReader, llm LLMRunner, tool ToolRunner, approval ApprovalRunner) *Mux {
	if defs == nil || llm == nil || tool == nil || approval == nil {
		panic("nodes: nil DefReader, LLMRunner, ToolRunner or ApprovalRunner")
	}
	return &Mux{defs: defs, llm: llm, tool: tool, approval: approval}
}

// Execute 满足 worker.Executor(结构化匹配;handler 收口路径不感知节点类型)。
func (m *Mux) Execute(ctx context.Context, req worker.Request) (worker.Outcome, error) {
	node, err := m.defs.NodeDefinition(ctx, req.NodeExecutionID)
	if err != nil {
		if errors.Is(err, engine.ErrNotFound) {
			return failedOutcome("node_def_missing", err.Error()), nil
		}
		return worker.Outcome{}, fmt.Errorf("nodes: read node definition (node_exec %d): %w", req.NodeExecutionID, err)
	}
	switch {
	case node.LLM != nil:
		return m.llm.Run(ctx, req, node.LLM)
	case node.Tool != nil:
		return m.tool.Run(ctx, req, node.Tool)
	case node.Approval != nil:
		return m.approval.Run(ctx, req, node.Approval)
	default:
		return failedOutcome("no_executor",
			fmt.Sprintf("node %q type %q has no executor wired", node.ID, node.Type)), nil
	}
}
