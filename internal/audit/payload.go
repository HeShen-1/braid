package audit

import "time"

// Payload 结构族:各事件类型的 payload 列形状(scheduler 与 reaper 共用发射,
// 单点定义防两方漂移)。字段只含 id/码/枚举/时间戳,密钥永不入内(铁律)。
// 携带约定:状态迁移行 attempt_no 为迁移时的工作 attempt(claim 语义:已 +1);
// 观察者路径(Complete/reaper)同时携 attempt_id 列锚定具体 attempt 行。

// NodeStateChangedPayload 是 node.state_changed 的事件体。
type NodeStateChangedPayload struct {
	From      string `json:"from"` // 迁移前状态(NodeState)
	To        string `json:"to"`   // 迁移后状态
	NodeID    string `json:"node_id"`
	Version   uint32 `json:"version"`              // 迁移后行版本
	Code      string `json:"code,omitempty"`       // FAILED 路径:error_code
	Msg       string `json:"msg,omitempty"`        // FAILED 路径:摘要
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
