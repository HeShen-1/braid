package nodes

// Approval 节点执行器(T4.1,ARCHITECTURE Q4/L67):审批节点的「执行」是
// 纯挂起决策——无外部调用、无产出,attempt 以 SUCCEEDED 收口、节点挂起
// WAITING_APPROVAL、worker 任务正常结束释放并发槽。
//
// 分工(勿越界):本执行器只发信号(Outcome.Suspend);attempt/节点/执行级
// 的挂起事务归 scheduler.Suspend(worker.suspendNode 通道),审批输入快照在
// dispatchOne→ClaimNode 时已落库(approvalSnapshotData,审批人可读面),本层
// 不重读不重写。快照/类型守卫在调度器事务体内(数据缺陷响亮拒绝)。

import (
	"context"

	"braid/internal/engine"
	"braid/internal/engine/dsl"
	"braid/internal/worker"
)

// ApprovalRunner 是 approval 节点的执行面(Mux 内部路由缝;生产实现唯一
// ApprovalExecutor,fake 只存在于本包 _test.go——依赖倒置同 LLMRunner/ToolRunner)。
type ApprovalRunner interface {
	Run(ctx context.Context, req worker.Request, spec *dsl.ApprovalNode) (worker.Outcome, error)
}

// ApprovalExecutor 执行 approval 节点:零依赖结构体(无 DB/无外部调用——
// 快照在 claim 时已落库,挂起事务在调度器)。Run 恒返回挂起信号;spec 是
// 钉住 DSL 的声明面(生效值补齐在调度器 effectiveApprovalSpec 单点)。
type ApprovalExecutor struct{}

// Run 返回挂起信号(见类型注释)。ctx 不被观测:纯决策无外部调用,取消
// 语义由下游收口事务承担。
func (ApprovalExecutor) Run(_ context.Context, _ worker.Request, _ *dsl.ApprovalNode) (worker.Outcome, error) {
	return worker.Outcome{Status: engine.AttemptSucceed, Suspend: true}, nil
}
