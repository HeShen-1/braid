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
	From       string `json:"from"` // 迁移前状态(NodeState)
	To         string `json:"to"`   // 迁移后状态
	NodeID     string `json:"node_id"`
	Version    uint32 `json:"version"`               // 迁移后行版本
	Code       string `json:"code,omitempty"`        // FAILED 路径:error_code
	AttemptNo  uint16 `json:"attempt_no,omitempty"`  // 关联 attempt 序号(1 起,0=未触 attempt)
	WorkerID   string `json:"worker_id,omitempty"`   // 归因快照(行内 worker_id)
	InstanceNo uint16 `json:"instance_no,omitempty"` // T9.1:parallel 实例行(控制行/单实例 0,缺省键缺席 = 0)
}

// NodeRedispatchedPayload 是 node.redispatched(status-preserving 重派)的事件体。
type NodeRedispatchedPayload struct {
	NodeID      string `json:"node_id"`
	AttemptNo   uint16 `json:"attempt_no"`             // 新 attempt 序号(已 +1)
	Trigger     string `json:"trigger"`                // TriggerAttemptFailed | TriggerReaped
	MaxAttempts uint16 `json:"max_attempts,omitempty"` // 决策时刻的预算上限(reaper 路径)
	InstanceNo  uint16 `json:"instance_no,omitempty"`  // T9.1:parallel 实例行(控制行/单实例 0)
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
	Decision string `json:"decision"` // approve | reject(请求面词表,区别于 DB ENUM;与 decisionFromRequest/Attempted 同词面)
	Mode     string `json:"mode"`     // any | all(快照同源;T14.2 会签起双值,判据单点 scheduler.ApprovalOutcome)
	// Via 是投票入口归因(T14.3):im_link = 钉钉卡片鉴权短链;缺省 =
	// Bearer Web/API 决定面。归因是观测事实不是身份——actor_id 仍是决定者
	// 本人(短链所指 approver,D-t143 决策 4)。
	Via string `json:"via,omitempty"`
}

// ApprovalDecisionDeniedPayload 是 approval.decision_denied(T4.2 越权决定
// 尝试被拒)的事件体。attempted 是尝试方向(approve|reject);尝试者身份在
// 行级 actor_id;reason 区分拒绝面:T4.2 仅 membership(DSL 名单外),
// T5.2 换装新增 role_denied(名单内但缺审批角色),T20.2 新增 scope_denied
// (scp 不足,审批决定面收编)——旧行缺省即 membership。
type ApprovalDecisionDeniedPayload struct {
	NodeID    string `json:"node_id"`
	Attempted string `json:"attempted"`        // approve | reject
	Reason    string `json:"reason,omitempty"` // membership(缺省)| role_denied | scope_denied(T20.2)
}

// ApprovalEscalatedPayload 是 approval.escalated(T4.3 审批超时升级发射)的
// 事件体。deadline_at 是升级判据证据(触发时刻节点行的挂起截止);策略
// (on_timeout)与审批人名单真相源在 input_snapshot,payload 不复制(D-t26
// §4);升级时刻真相源在 escalated_at 列。
type ApprovalEscalatedPayload struct {
	NodeID     string    `json:"node_id"`
	DeadlineAt time.Time `json:"deadline_at"`
}

// ApprovalResumeRedispatchedPayload 是 approval.resume_redispatched(T4.3
// resume 死信网幂等重投)的事件体。决策事实真相源在 approval_decision 行
// (payload 仅 id);AttemptNo = 重投事件声称的续跑 attempt 号(TaskID 第四段)。
type ApprovalResumeRedispatchedPayload struct {
	NodeID     string `json:"node_id"`
	DecisionID uint64 `json:"decision_id"`
	AttemptNo  uint16 `json:"attempt_no"`
}

// ApprovalNotifiedPayload 是 approval.notified(T4.5 SMTP 送达)的事件体。
// reason 与事件成因码同词表(ApprovalNotifyPayload.Reason 单源,P0 仅
// escalated)。收件地址/邮件主题不入行(D-t26 §4:自由文本与配置面不入审计;
// 收件配置真相源在部署 env),邮件正文的工作流上下文真相源在
// node_execution.input_snapshot(渲染由 notify 包单点)。送达时刻真相源在
// 行级 event_at。
type ApprovalNotifiedPayload struct {
	NodeID string `json:"node_id"`
	Reason string `json:"reason"` // escalated(P0 词表,scheduler.ApprovalNotifyPayload 同源)
	// Round:T9.2 轮次段(D-t92 决策 7 的消费半边)——notify 去重锚按轮匹配,
	// 回滚重挂后第二轮升级不再被第一轮行静默吞。
	Round int `json:"round"`
}

// ApprovalIMNotifiedPayload 是 approval.im_notified(T14.3 IM 卡片送达)的
// 事件体——形态镜像 ApprovalNotifiedPayload(D-t143 决策 5:per-channel
// 去重锚,事件类型本身即通道)。平台归因不复制(配置真相源在部署 env);
// 卡片内容的工作流上下文真相源在 node_execution.input_snapshot(notify
// 包渲染单点)。
type ApprovalIMNotifiedPayload struct {
	NodeID string `json:"node_id"`
	Reason string `json:"reason"` // escalated(P0 词表,scheduler.ApprovalNotifyPayload 同源)
	Round  int    `json:"round"`
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

// LLMCallBudgetDeniedPayload 是 llm.call.budget_denied(T18.2,每执行 token
// 预算 pre-call 拒绝)的事件体。只含 id/码/数值(D-t26 §4 纪律,无 prompt
// 自由文本):budget = BRAID_LLM_TOKEN_BUDGET 限额;spent = Σ 该执行已计费
// usage(node_attempt.total_tokens);estimated_prompt_tokens = tiktoken 请求前
// 估算(恒近似,§2 口径纪律——计费真相源 = API usage,两者永不互换);
// completion_reserve = DSL max_tokens 声明值(未声明 0);拒绝判据 =
// spent + estimated + reserve > budget(边界含)。行级 actor_id = 模型名
// (与 llm.call.completed 同形归因)。
type LLMCallBudgetDeniedPayload struct {
	NodeID                string `json:"node_id"`
	Model                 string `json:"model"`
	Budget                uint64 `json:"budget"`
	Spent                 uint64 `json:"spent"`
	EstimatedPromptTokens uint64 `json:"estimated_prompt_tokens"`
	CompletionReserve     uint64 `json:"completion_reserve"`
}

// LLMCallDiscardedPayload 是 llm.call.discarded(T6.4,D-t36 §7)的事件体:
// 迟到 SUCCEEDED 携 usage 在 Complete 让位路径被丢弃(供应商已计费、平台
// 台账双缺)。字段镜像 LLMCallCompletedPayload——这是该调用唯一留下的
// 台账痕迹,对账/告警按本行计数;ObservedAttempt 记录收口冲突时观测到的
// attempt 终态(恒 DEAD,防未来语义漂移时静默)。
// ExecutionRolledBackPayload 是 execution.rolled_back(T9.2 回滚决定事务,
// D-2026-09-28-t92 决策 9)的事件体。只含 id/码/枚举(D-t26 §4 纪律);
// 自由文本理由不入 payload。from_status 是回滚前执行终态(白名单三终态
// {FAILED,SUCCEEDED,CANCELLED},T19.3/D-2026-10-05-t193 决策 3 扩列,
// 冗余列作对账锚);reset_count = 本轮重置行数(含实例行);rerun_target
// 区分目标重跑(RerunNode)与重评估(Reset 后由 advance 重派)两种形态。
type ExecutionRolledBackPayload struct {
	TargetNodeID  string `json:"target_node_id"`
	FromStatus    string `json:"from_status"`    // 回滚前执行状态(白名单三终态)
	ResetCount    int    `json:"reset_count"`    // ResetNodeToPending 命中行数(含实例行)
	RerunTarget   bool   `json:"rerun_target"`   // true = RerunNode 快照读回重跑;false = advance 重评估派发
	QuiescedCount int    `json:"quiesced_count"` // T19.6(D-t196):楔停收敛的 EXECUTING/WA 残留行数(0 = 非楔停/零残留;api 重放重建同源)
}

// RollbackDeniedPayload 是 rollback.denied(回滚请求被拒,T9.2)的事件体。
// 拒绝面归审计(镜像 approval.decision_denied 的独立小事务先例)。reason
// 是错误分面枚举:not_rollbackable | blocked_inflight | bad_node_id |
// no_resume_target | too_large | role_denied | scope_denied(too_large 自
// T15.5 容量闸;scope_denied 自 T20.2 scp 拒绝收编——api 侧全业务分面
// 落行);blocked 携阻塞节点清单上限(截断如实计数);
// quiesce = 请求是否携 T19.6 破坏性收敛旗标(拒绝面的显式 opt-in 证据)。
type RollbackDeniedPayload struct {
	Reason       string `json:"reason"`
	TargetNodeID string `json:"target_node_id,omitempty"`
	BlockedCount int    `json:"blocked_count,omitempty"`
	Quiesce      bool   `json:"quiesce"`
}

// ExecutionCancelledPayload 是 execution.cancelled(T19.3 取消决定事务,
// D-2026-10-05-t193 决策 2/5)的事件体。只含 id/码/枚举(D-t26 §4 纪律);
// 取消理由归 audit(T2.6 既有)——自由文本不入 payload。from_status 是取消前
// 执行状态,受理面恒 RUNNING|WAITING_APPROVAL(PENDING 不受理:Start 锁序
// 例外面前提,对抗审计 C2;终态不受理:归回滚面);cancelled_count = 楔停
// 行数(含实例行;0 = 全终态滞留执行的直接收口形态)。
type ExecutionCancelledPayload struct {
	FromStatus     string `json:"from_status"`
	CancelledCount int    `json:"cancelled_count"`
}

// CancelDeniedPayload 是 cancel.denied(取消请求被拒,T19.3)的事件体。
// reason 是错误分面枚举:not_cancellable(PENDING 与终态同面)| too_large
// | role_denied | scope_denied(T20.2 scp 拒绝收编;镜像
// RollbackDeniedPayload 全分面纪律)。
type CancelDeniedPayload struct {
	Reason string `json:"reason"`
}

type LLMCallDiscardedPayload struct {
	NodeID           string `json:"node_id"`
	Model            string `json:"model"`
	PromptTokens     uint32 `json:"prompt_tokens"`
	CompletionTokens uint32 `json:"completion_tokens"`
	TotalTokens      uint32 `json:"total_tokens"`
	CostAmount       string `json:"cost_amount,omitempty"`
	CostCurrency     string `json:"cost_currency,omitempty"`
	CostEstimated    bool   `json:"cost_estimated"`
	ObservedAttempt  string `json:"observed_attempt"`
}

// NodeTaintPropagatedPayload 是 node.taint_propagated(T18.3,
// D-2026-10-05-t183-taint-tracking)的事件体:污点传播证据行。Paths 是传递源
// 路径集(`inputs.<声明名>` 升序),宇宙被 refs.go 正则字符集钉死(T1.2 校验
// 通过时 ⊆ DSL 声明 inputs 词表;校验旁路的防御口径下未声明名照录,形态仍
// 钉死)——作者标识符,与行级 node_id 同信任类,非自由文本(D-t26 §4 维持
// + validate 形状闸 fail-closed);
// 值永不入 payload(真相源列明文不脱敏,审计不复制)。Sink 语义 = 「进入外发
// 通道」非「送达」:运行时拦截(tool_ssrf_blocked 等)不改写静态事实,拦截
// 另有失败码/tool.call.denied 承载。词表演进位:`outputs`(工作流顶层 outputs
// lineage)预留未启用;approval IM 面(enterprise)若成外发通道再末尾追加。
type NodeTaintPropagatedPayload struct {
	NodeID string   `json:"node_id"`
	Sink   string   `json:"sink"`  // SinkLLM | SinkTool | SinkNone
	Paths  []string `json:"paths"` // 传递源路径集(升序;静态推导,钉版定义恒同集)
}

// Sink 词表(T18.3):污点在本节点流入的通道分类。
const (
	SinkLLM  = "llm"  // user_data 拼入 prompt 发往模型供应商
	SinkTool = "tool" // 已求值入参进入子进程/http 等工具执行面
	SinkNone = "none" // 本地消费:branch 路由臂求值(approval 结构性零引用,恒不发射)
)
