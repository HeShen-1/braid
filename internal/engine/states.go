// Package engine 实现 braid 执行引擎内核:节点/工作流状态机与 MySQL CAS 迁移。
//
// 契约真源 = internal/db/migrations/0001_init.up.sql(表注释 + 文件尾 CAS 参考语句):
//   - 状态机全集逐字镜像 DDL 注释,本包不增改语义;
//   - 节点状态迁移只经本包 CAS helper(AGENTS 铁律:禁止裸 UPDATE);
//   - helper 永不自管事务边界:全部方法收 DBTX(*sql.DB / *sql.Tx 均满足),
//     T2.2 起状态迁移与 outbox 同事务 = 调用方传同一 *sql.Tx,签名零改动。
//
// 三层防线:命名方法(编译期,非法边在调用点不可表达)→ 进程内矩阵
// (ErrIllegalTransition,零 DB 往返)→ SQL 谓词(最终仲裁,检查与执行之间的
// 竞争不被进程内检查绕过)。矩阵是唯一真源,各 SQL 谓词是其投影,测试逐边对拍。
package engine

import "sort"

// NodeState 是 node_execution.status 的 Go 侧词表,常量值逐字 = DDL ENUM。
// 演进约束(DDL 文件头):ENUM 只允许末尾追加;改词表必须同步迁移 SQL 与
// information_schema 词表钉死测试(enum_test.go)。
type NodeState string

const (
	NodePending         NodeState = "PENDING"
	NodeExecuting       NodeState = "EXECUTING"
	NodeWaitingApproval NodeState = "WAITING_APPROVAL"
	NodeSucceeded       NodeState = "SUCCEEDED"
	NodeFailed          NodeState = "FAILED"
	NodeSkipped         NodeState = "SKIPPED"
	NodeCancelled       NodeState = "CANCELLED"
)

// Terminal 报告 s 是否终态。终态无出边(DDL:SUCCEEDED/FAILED/SKIPPED/CANCELLED)。
func (s NodeState) Terminal() bool {
	switch s {
	case NodeSucceeded, NodeFailed, NodeSkipped, NodeCancelled:
		return true
	}
	return false
}

// WorkflowState 是 workflow_execution.status 的 Go 侧词表,常量值逐字 = DDL ENUM。
type WorkflowState string

const (
	WorkflowPending         WorkflowState = "PENDING"
	WorkflowRunning         WorkflowState = "RUNNING"
	WorkflowWaitingApproval WorkflowState = "WAITING_APPROVAL"
	WorkflowSucceeded       WorkflowState = "SUCCEEDED"
	WorkflowFailed          WorkflowState = "FAILED"
	WorkflowCancelled       WorkflowState = "CANCELLED"
)

// Terminal 报告 s 是否终态(DDL:SUCCEEDED/FAILED/CANCELLED)。
func (s WorkflowState) Terminal() bool {
	switch s {
	case WorkflowSucceeded, WorkflowFailed, WorkflowCancelled:
		return true
	}
	return false
}

// AttemptState 是 node_attempt.status 的 Go 侧词表,常量值逐字 = DDL ENUM。
// RUNNING→SUCCEEDED|FAILED(观察终态)|DEAD(reconciler 判死,T2.5)。
type AttemptState string

const (
	AttemptRunning AttemptState = "RUNNING"
	AttemptSucceed AttemptState = "SUCCEEDED"
	AttemptFailed  AttemptState = "FAILED"
	AttemptDead    AttemptState = "DEAD"
)

// nodeEdges 是节点状态机唯一真源,逐字镜像 DDL node_execution 表注释:
// PENDING→EXECUTING→SUCCEEDED|FAILED|WAITING_APPROVAL;
// WAITING_APPROVAL→EXECUTING(审批复入,新 attempt);
// WAITING_APPROVAL→FAILED(审批超时 fail 策略终败,T4.3;0001 DDL 注释形态
// 由 D-2026-09-24-t43-approval-timeout 超越记录,error_code=approval_timeout
// 是 DDL error_code 词表预留词);
// EXECUTING→PENDING 仅限 reconciler 重入队(T2.5);
// PENDING→SKIPPED(分支剪枝,T2.4);任意非终态→CANCELLED。
// T9.2 重开边(D-2026-09-28-t92-rollback-same-branch 决策 2a,超越旧注
// 「否决 FAILED→PENDING:FAILED 必须终态」):「终态对自动路径恒无出边」立场
// 不变;显式回滚是唯一例外通道——SUCCEEDED|FAILED|SKIPPED|CANCELLED→PENDING
// 仅经 ResetNodeToPending(闭包重置)、SUCCEEDED|FAILED|CANCELLED→EXECUTING
// 仅经 RerunNode(目标重跑,快照读回),两者仅 api 回滚同步事务可达且同事务
// 落审计行(D-t26)。原否决的保护意图(防自动路径静默重跑、节点 FAILED 与
// attempt DEAD 混淆)由「attempt 台账不可变 + 审计链 + 单一调用面」承载;
// 自动重入队仍只从 EXECUTING 走(RequeueNode,语义不变)。
var nodeEdges = map[NodeState][]NodeState{
	NodePending:         {NodeExecuting, NodeSkipped, NodeCancelled},
	NodeExecuting:       {NodeSucceeded, NodeFailed, NodeWaitingApproval, NodePending /*仅 reconciler(T2.5)*/, NodeCancelled},
	NodeWaitingApproval: {NodeExecuting, NodeFailed, NodeCancelled},
	NodeSucceeded:       {NodePending /*仅 ResetNodeToPending(T9.2 回滚)*/, NodeExecuting /*仅 RerunNode(T9.2 回滚目标重跑)*/},
	NodeFailed:          {NodePending /*仅 ResetNodeToPending(T9.2 回滚)*/, NodeExecuting /*仅 RerunNode(T9.2 回滚目标重跑)*/},
	NodeSkipped:         {NodePending /*仅 ResetNodeToPending(T9.2 回滚重评估)*/},
	NodeCancelled:       {NodePending /*仅 ResetNodeToPending(T9.2 回滚)*/, NodeExecuting /*仅 RerunNode(T9.2,快照非 NULL 时;自 PENDING 连坐的 CANCELLED 快照 NULL 走 Reset 重评估)*/},
}

// wfEdges 是工作流状态机唯一真源(DDL workflow_execution.status 列注释):
// PENDING→RUNNING→(WAITING_APPROVAL⇄RUNNING)*→SUCCEEDED|FAILED|CANCELLED;
// PENDING 可直接→FAILED(reconciler 判死)。
// 终态(SUCCEEDED)仅自 RUNNING 出:挂起的执行必须先回 RUNNING 才能终局。
// T9.2(D-2026-09-28-t92 决策 2a):FAILED→RUNNING 仅经 RestartExecution
// (显式回滚复活,同语句清终态列组);终态对自动路径仍恒无出边,SUCCEEDED/
// CANCELLED 不可回滚(白名单仅 FAILED,决策 4)。
var wfEdges = map[WorkflowState][]WorkflowState{
	WorkflowPending:         {WorkflowRunning, WorkflowFailed, WorkflowCancelled},
	WorkflowRunning:         {WorkflowWaitingApproval, WorkflowSucceeded, WorkflowFailed, WorkflowCancelled},
	WorkflowWaitingApproval: {WorkflowRunning, WorkflowCancelled},
	WorkflowFailed:          {WorkflowRunning /*仅 RestartExecution(T9.2 显式回滚)*/},
}

// CanTransitionNode 报告节点边 from→to 是否在矩阵内。矩阵外的迁移是调用方 bug。
func CanTransitionNode(from, to NodeState) bool {
	for _, t := range nodeEdges[from] {
		if t == to {
			return true
		}
	}
	return false
}

// CanTransitionWorkflow 报告工作流边 from→to 是否在矩阵内。
func CanTransitionWorkflow(from, to WorkflowState) bool {
	for _, t := range wfEdges[from] {
		if t == to {
			return true
		}
	}
	return false
}

// wfFroms 返回能一步迁入 to 的状态集(由 wfEdges 生成,防 ENUM 尾部追加时遗漏;
// FinishExecution 的 SQL IN 谓词由此拼接——值全部来自本包常量,无注入面)。
// 结果排序:map 迭代序不确定,排序后 froms[0]/IN 序均确定(可测、日志稳定)。
func wfFroms(to WorkflowState) []WorkflowState {
	var froms []WorkflowState
	for from, tos := range wfEdges {
		for _, t := range tos {
			if t == to {
				froms = append(froms, from)
				break
			}
		}
	}
	sort.Slice(froms, func(i, j int) bool { return froms[i] < froms[j] })
	return froms
}

// nodeStates / wfStates 是全词表,供全组合矩阵测试与 ENUM 词表钉死测试遍历。
var nodeStates = []NodeState{
	NodePending, NodeExecuting, NodeWaitingApproval,
	NodeSucceeded, NodeFailed, NodeSkipped, NodeCancelled,
}

var attemptStates = []AttemptState{
	AttemptRunning, AttemptSucceed, AttemptFailed, AttemptDead,
}

var wfStates = []WorkflowState{
	WorkflowPending, WorkflowRunning, WorkflowWaitingApproval,
	WorkflowSucceeded, WorkflowFailed, WorkflowCancelled,
}
