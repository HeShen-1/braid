// Package audit 实现最小同步审计写入器(T2.6,0001_init.up.sql §6):
// 在状态迁移的同一事务内直写 audit_log 行。逐字镜像 outbox 范式:校验前置
// 不触 DB、永不自管事务边界(同 engine.DBTX 契约)。
//
// 同步语义(P0):event_at = DB 端 NOW(3),与 node_execution.status_changed_at
// 同钟系——事件时刻=迁移时刻(异步批量写的分立归 T6.1)。exec_date 分区键由
// INSERT..SELECT 从 workflow_execution.created_at 物化,应用不传(单语句零额外
// 往返,且天然钉住「审计行必有执行锚」:SELECT 0 行 = execution 缺行,响亮
// ErrNotFound,不静默无写)。
//
// 铁律:本包对 audit_log 只发 INSERT;业务账号仅 INSERT/SELECT 授权由部署层
// 承担(T7.3 安装自检断言)。payload 只含 id/码/枚举,密钥永不入内。
//
// P0 未启用(走 DDL 默认,见 D-2026-09-23-t26-audit-events):trace_id(NULL
// 占位,T6.2 前不伪造)、pii_masked(0,T6.1 脱敏钩子)、prev_hash/row_hash
// (”/NULL,未启链期)、usage/cost 冗余列(T3.1 起)。
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"

	"braid/internal/engine"
)

// ErrInvalidEvent:事件参数非法,调用方 bug(校验在触 DB 之前完成,风格对齐
// engine.ErrInvalidSnapshot / outbox.ErrInvalidEvent)。
var ErrInvalidEvent = errors.New("audit: invalid event")

// 事件词表(P0 五类,DDL 开放词表的本任务收口面;T3.x/T4/T5 接入时在此登记
// 新常量——Append 按此闭集校验,typo 在首个集成测试即暴露):
//   - node.state_changed:全部节点状态迁移(单一类型,payload 携 from/to);
//   - node.redispatched:status-preserving 重派(scheduler 重试 / reaper 重派);
//   - attempt.reaped:reaper DEAD 裁决(三种收口码),attempt_id 列锚定;
//   - node.deadline_extended:reaper 迟到启动一次性重挂;
//   - branch.evaluated:branch 路由决定(D-t24 #3)。
const (
	EventNodeStateChanged     = "node.state_changed"
	EventNodeRedispatched     = "node.redispatched"
	EventAttemptReaped        = "attempt.reaped"
	EventNodeDeadlineExtended = "node.deadline_extended"
	EventBranchEvaluated      = "branch.evaluated"
)

// actor 词表逐字 = DDL actor_type 列注释(user/system/worker/tool/llm/approval);
// P0 只发 worker(system 由 reaper)。user/tool/llm/approval 为 T5/T3.x 预留,
// 校验按全集——DDL 开放面不变,只是本任务尚无发射点。
const (
	ActorUser     = "user"
	ActorSystem   = "system"
	ActorWorker   = "worker"
	ActorTool     = "tool"
	ActorLLM      = "llm"
	ActorApproval = "approval"
)

// RedispatchTrigger 是 node.redispatched 的重派来源分码。
const (
	TriggerAttemptFailed = "attempt_failed" // scheduler Complete 重试路径
	TriggerReaped        = "reaped"         // reaper 判死后重派
)

// 列宽对齐(DDL §6):event_type/node_id VARCHAR(64)、actor_type VARCHAR(16)、
// actor_id VARCHAR(128)。
const (
	maxEventText = 64
	maxNodeID    = 64
	maxActorType = 16
	maxActorID   = 128
)

// knownEvents / knownActors 是闭集校验表(词表登记处,见上)。
var knownEvents = map[string]bool{
	EventNodeStateChanged:     true,
	EventNodeRedispatched:     true,
	EventAttemptReaped:        true,
	EventNodeDeadlineExtended: true,
	EventBranchEvaluated:      true,
}

var knownActors = map[string]bool{
	ActorUser: true, ActorSystem: true, ActorWorker: true,
	ActorTool: true, ActorLLM: true, ActorApproval: true,
}

var traceIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Event 是 audit_log 行的写入参数。ID 应用生成(snowflake,复用调用方生成器),
// 零值被拒。NodeExecID/AttemptID/NodeID/TraceID 零值/空串 → NULL(P0 各事件的
// 携带约定见 payload 结构注释)。
type Event struct {
	ID          uint64
	EventType   string
	ExecutionID uint64
	NodeExecID  uint64
	NodeID      string
	AttemptID   uint64
	ActorType   string
	ActorID     string
	TraceID     string
	Payload     json.RawMessage
}

// Append 在调用方事务内直写一条审计行(典型形状见 engine/dbtx.go 注释:
// CAS 迁移 → audit.Append → outbox.Append,同事务同存亡)。
//
// 错误分类:ErrInvalidEvent(调用方 bug,不触 DB);ErrNotFound(execution
// 缺行 = 数据缺陷,INSERT..SELECT 0 行);其余 %w 包装上抛,Transient 判定
// 归调用方(与 outbox 同规)。
func Append(ctx context.Context, q engine.DBTX, evt Event) error {
	if len(evt.Payload) == 0 {
		evt.Payload = json.RawMessage(`{}`) // DDL:无正文事件存 {}
	}
	if err := validate(evt); err != nil {
		return err
	}
	var nodeExecID, attemptID, trace any
	if evt.NodeExecID != 0 {
		nodeExecID = evt.NodeExecID
	}
	if evt.AttemptID != 0 {
		attemptID = evt.AttemptID
	}
	// 空 NodeID → NULL(DDL:NULL=执行级事件,查询按 IS NULL 判别,'' 会静默
	// 错分类——review 修,与其余三列的零值规范化同形)。
	nodeID := any(evt.NodeID)
	if evt.NodeID == "" {
		nodeID = nil
	}
	if evt.TraceID != "" {
		trace = evt.TraceID
	}
	res, err := q.ExecContext(ctx, `
INSERT INTO audit_log
  (id, exec_date, event_at, event_type, execution_id, node_execution_id,
   node_id, attempt_id, actor_type, actor_id, trace_id, payload)
SELECT ?, DATE(created_at), NOW(3), ?, ?, ?, ?, ?, ?, ?, ?, ?
  FROM workflow_execution WHERE id = ?`,
		evt.ID, evt.EventType, evt.ExecutionID, nodeExecID, nodeID,
		attemptID, evt.ActorType, evt.ActorID, trace, evt.Payload, evt.ExecutionID)
	if err != nil {
		return fmt.Errorf("audit: append %d (%s): %w", evt.ID, evt.EventType, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("audit: rows affected (%d): %w", evt.ID, err)
	}
	if n != 1 {
		// SELECT 命中 0 行(>1 不可能:WHERE id=? 走 PK)。审计行必须挂在
		// 真实执行上,静默无写会破坏「每次迁移必有审计行」验收。
		return fmt.Errorf("%w: audit event %d: execution %d vanished",
			engine.ErrNotFound, evt.ID, evt.ExecutionID)
	}
	return nil
}

func validate(evt Event) error {
	switch {
	case evt.ID == 0:
		return fmt.Errorf("%w: zero id", ErrInvalidEvent)
	case evt.ExecutionID == 0:
		return fmt.Errorf("%w: zero execution id", ErrInvalidEvent)
	case !knownEvents[evt.EventType]:
		return fmt.Errorf("%w: event_type %q not in vocabulary", ErrInvalidEvent, evt.EventType)
	case evt.ActorType == "" || len(evt.ActorType) > maxActorType || !knownActors[evt.ActorType]:
		return fmt.Errorf("%w: actor_type %q", ErrInvalidEvent, evt.ActorType)
	case evt.ActorID == "" || len(evt.ActorID) > maxActorID:
		return fmt.Errorf("%w: actor_id %q", ErrInvalidEvent, evt.ActorID)
	case len(evt.NodeID) > maxNodeID:
		return fmt.Errorf("%w: node_id %q too long", ErrInvalidEvent, evt.NodeID)
	case evt.TraceID != "" && !traceIDRe.MatchString(evt.TraceID):
		return fmt.Errorf("%w: trace_id %q", ErrInvalidEvent, evt.TraceID)
	}
	if !json.Valid(evt.Payload) {
		return fmt.Errorf("%w: payload not valid JSON", ErrInvalidEvent)
	}
	// json.Valid 只查语法不查 UTF-8,而 MySQL 会原样存入非法字节(review 修:
	// 产出 spec-invalid JSON 行,破坏 T6.1 哈希链规范化前提)。全部现有调用方
	// 走 json.Marshal(自动 U+FFFD 替换),此校验是未来直传字节调用方的防线。
	if !utf8.Valid(evt.Payload) {
		return fmt.Errorf("%w: payload not valid UTF-8", ErrInvalidEvent)
	}
	return nil
}
