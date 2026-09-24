package audit

import "time"

// Payload 结构族:各事件类型的 payload 列形状(scheduler 与 reaper 共用发射,
// 单点定义防两方漂移)。字段只含 id/码/枚举/时间戳,密钥永不入内(铁律)。
// 携带约定:状态迁移行 attempt_no 为迁移时的工作 attempt(claim 语义:已 +1);
// 观察者路径(Complete/reaper)同时携 attempt_id 列锚定具体 attempt 行。

// NodeStateChangedPayload 是 node.state_changed 的事件体。无 Msg 字段——自由
// 文本违反 D-t26 §4「payload 只含 id/码/枚举」,失败摘要真相源在
// node_execution.error_message(review 修;T6.1 脱敏钩子落地后再评估放开)。
type NodeStateChangedPayload struct {
	From      string `json:"from"` // 迁移前状态(NodeState)
	To        string `json:"to"`   // 迁移后状态
	NodeID    string `json:"node_id"`
	Version   uint32 `json:"version"`              // 迁移后行版本
	Code      string `json:"code,omitempty"`       // FAILED 路径:error_code
	AttemptNo uint16 `json:"attempt_no,omitempty"` // 关联 attempt 序号(1 起,0=未触 attempt)
	WorkerID  string `json:"worker_id,omitempty"`  // 归因快照(行内 worker_id)
}

// NodeRedispatchedPayload 是 node.redispatched(status-preserving 重派)的事件体。
type NodeRedispatchedPayload struct {
	NodeID      string `json:"node_id"`
	AttemptNo   uint16 `json:"attempt_no"`             // 新 attempt 序号(已 +1)
	Trigger     string `json:"trigger"`                // TriggerAttemptFailed | TriggerReaped
	MaxAttempts uint16 `json:"max_attempts,omitempty"` // 决策时刻的预算上限(reaper 路径)
}

// AttemptReapedPayload 是 attempt.reaped(reaper DEAD 裁决)的事件体。
type AttemptReapedPayload struct {
	NodeExecutionID uint64 `json:"node_execution_id"`
	Code            string `json:"code"`                  // heartbeat_lost | deadline_exceeded | stale_attempt
	WorkerID        string `json:"worker_id,omitempty"`   // 首任归因(node_attempt 行内)
	Stale           string `json:"stale,omitempty"`       // 心跳陈旧时长(heartbeat 判死)
	NodeStatus      string `json:"node_status,omitempty"` // stale_attempt:触发它的节点终态
}

// NodeDeadlineExtendedPayload 是 node.deadline_extended(迟到启动一次性重挂)的事件体。
type NodeDeadlineExtendedPayload struct {
	NodeID     string    `json:"node_id"`
	AttemptNo  uint16    `json:"attempt_no"`
	DeadlineAt time.Time `json:"deadline_at"` // 新 deadline
}

// BranchEvaluatedPayload 是 branch.evaluated(路由决定)的事件体。
type BranchEvaluatedPayload struct {
	NodeID string `json:"node_id"`
	Next   string `json:"next"` // 取路目标(operands 全文在 input_snapshot 真相源)
}

// LLMCallCompletedPayload 是 llm.call.completed(T3.1)的事件体。与冗余列组
// 部分同字段的理由:列是 T6.4 聚合的免解 JSON 快路径,payload 是证据行自描述
// 全量——两者由同一发射点单源填充,不同消费者各取所需。
// 无 prompt/模型返回自由文本(D-t26 §4 纪律):prompt 求值真相源在
// node_execution.input_snapshot,响应摘要在 node_execution.output,审计副本
// 不复制大文本;T6.1 脱敏钩子落地后再评估放开。
type LLMCallCompletedPayload struct {
	NodeID           string `json:"node_id"`
	Model            string `json:"model"`
	PromptTokens     uint32 `json:"prompt_tokens"`
	CompletionTokens uint32 `json:"completion_tokens"`
	TotalTokens      uint32 `json:"total_tokens"`
	CostAmount       string `json:"cost_amount,omitempty"` // 不计费时缺省(与列 NULL 同语义)
	CostCurrency     string `json:"cost_currency,omitempty"`
	CostEstimated    bool   `json:"cost_estimated"`
}

// ToolCallCompletedPayload 是 tool.call.completed(T3.2)的事件体。无工具
// 入参/产出/token 自由文本(D-t26 §4:入参真相源在 input_snapshot,产出在
// node_execution.output,token 是短时凭证本就不该留痕);时长是工具调用的
// 观测维度(T6.4 指标字源)。
type ToolCallCompletedPayload struct {
	NodeID     string `json:"node_id"`
	Tool       string `json:"tool"`
	DurationMs uint64 `json:"duration_ms"`
}

// ApprovalRequestedPayload 是 approval.requested(T4.1)的事件体:审批挂起的
// 域事件,只携 T4.4 待办列表/T6.4 积压指标的免 join 字段。instructions 等
// 快照全文真相源在 node_execution.input_snapshot(审批人可读面),审计行不
// 复制自由文本(D-t26 §4);deadline_at 与 node_execution.deadline_at 单源
// 由同一发射点填充。approvers 是 DSL 声明列表(id/枚举面,非自由文本)。
type ApprovalRequestedPayload struct {
	NodeID       string    `json:"node_id"`
	Approvers    []string  `json:"approvers"`
	Mode         string    `json:"mode"`
	TimeoutHours float64   `json:"timeout_hours"`
	OnTimeout    string    `json:"on_timeout"`
	DeadlineAt   time.Time `json:"deadline_at"`
}

// ApprovalDecidedPayload 是 approval.decided(T4.2 审批决定落库)的事件体。
// 决定者身份在行级列(actor_id = JWT sub;审批人词表真相源在
// node_execution.input_snapshot,审计不复制);决定全文(备注、时刻)真相源
// 在 approval_decision 行(D-t26 §4:payload 只含 id/码/枚举,业务事实表
// 自证);T4.4 审批历史/T6.4 审批吞吐指标的免 join 字段在此。
type ApprovalDecidedPayload struct {
	NodeID   string `json:"node_id"`
	Decision string `json:"decision"` // approved | rejected(请求面词表,区别于 DB ENUM)
	Mode     string `json:"mode"`     // any(P0 唯一值;快照同源,首票定局的裁决依据)
}

// ApprovalDecisionDeniedPayload 是 approval.decision_denied(T4.2 越权决定
// 尝试被拒)的事件体。attempted 是尝试方向(approve|reject);尝试者身份在
// 行级 actor_id;reason 区分拒绝面:T4.2 仅 membership(DSL 名单外),
// T5.2 换装新增 role_denied(名单内但缺审批角色)——旧行缺省即 membership。
type ApprovalDecisionDeniedPayload struct {
	NodeID    string `json:"node_id"`
	Attempted string `json:"attempted"`       // approve | reject
	Reason    string `json:"reason,omitempty"` // membership(缺省)| role_denied
}

// ExecutionTriggeredPayload 是 execution.triggered(T5.5 API 触发面)的
// 事件体。只锚定触发事实:触发者身份在行级列(actor_type/actor_id + 执行行
// triggered_by 事实源),input 正文不影子存储(D-t26 §4:自由数据面真相源
// 在 workflow_execution.input)。
type ExecutionTriggeredPayload struct {
	DefinitionID uint64 `json:"definition_id"` // 钉版定义行 id
	TriggerType  string `json:"trigger_type"`  // DDL ENUM(API|SCHEDULE,词表单源 DDL)
}

// ToolCallDeniedPayload 是 tool.call.denied(T5.2 执行时 RBAC 越权工具调用
// 被拒)的事件体。被拒主体在行级 actor_id(= workflow_execution.
// triggered_by);actor_roles 是被拒时主体实持角色(id/枚举面,运维对账
// 「为什么拒」的免 join 字段);reason 是拒绝分因(unknown_subject/
// disabled/role_mismatch——账号态细节只进审计,不进公开的节点
// error_message,sec 评审 LOW);拒绝时点即调用时点,attempt_id NULL
// (拒绝发生在 attempt 收口之前——工具零触达)。
type ToolCallDeniedPayload struct {
	NodeID     string   `json:"node_id"`
	Tool       string   `json:"tool"`
	ActorRoles []string `json:"actor_roles"` // 拒绝时实持角色(可能空:主体无用户行)
	Reason     string   `json:"reason"`      // unknown_subject | disabled | role_mismatch
}
