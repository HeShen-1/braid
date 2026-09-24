// Package audit 实现审计写入器(T2.6 同步直写;T6.1 起双路径:本文件
// Append 同步直写 + stage.go Enqueue 同事务暂存 audit_pending、flush.go
// reaper 批量搬运)。两条路径同纪律:校验前置不触 DB、永不自管事务边界
// (同 engine.DBTX 契约)、脱敏先于任何落库(sanitize.go)。
//
// 同步语义(P0):event_at = DB 端 NOW(3),与 node_execution.status_changed_at
// 同钟系——事件时刻=迁移时刻(异步半边同钟:暂存事务时刻,flush 原样
// 搬运)。exec_date 分区键由
// 普通一致读从 workflow_execution.created_at 物化后 INSERT VALUES 落行
// (D-2026-09-23-t26-audit-writer-lockfree-read,修订 D-t26 #2 的单语句
// INSERT..SELECT:RR 下其源表读取取共享 next-key 锁并持有至 COMMIT,reaper
// leaderless 双扫描各持 S 后在 wf FOR UPDATE 升级 X 处 1213——PR #13 review
// #9;一致读零锁,写入器对 workflow_execution 的锁面归零)。SELECT 0 行 =
// execution 缺行,响亮 ErrNotFound,不静默无写。
//
// 铁律:本包对 audit_log 只发 INSERT;业务账号仅 INSERT/SELECT 授权由部署层
// 承担(T7.3 安装自检断言)。payload 只含 id/码/枚举,密钥永不入内。
//
// P0 未启用(走 DDL 默认):trace_id(NULL 占位,T6.2 前不伪造)、
// prev_hash/row_hash(”/NULL,未启链期)。pii_masked 已随 T6.1 脱敏钩子
// 接线(SanitizePayload 掩蔽标志;现有词表无自由文本 ⇒ 恒 0,见
// sanitize.go)。usage/cost 冗余列已随 T3.1 llm.call.completed 启用
// (Event.Usage,与 node_attempt 双录;其余事件类型仍不携带)。
// T6.1 起本包双写入路径:Append(本文件,同步直写,低频/回读事件)与
// Enqueue(stage.go,同事务暂存 audit_pending,高频四类)——分阶段口径
// 见 stage.go 头注释;搬运半边 = flush.go(reaper flush 循环消费)。
package audit

import (
	"context"
	"database/sql"
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

// 事件词表(P0 五类 + T3.1/T3.2 各一类,DDL 开放词表的任务收口面;T4/T5
// 接入时在此登记新常量——Append 按此闭集校验,typo 在首个集成测试即暴露):
//   - node.state_changed:全部节点状态迁移(单一类型,payload 携 from/to);
//   - node.redispatched:status-preserving 重派(scheduler 重试 / reaper 重派);
//   - attempt.reaped:reaper DEAD 裁决(三种收口码),attempt_id 列锚定;
//   - node.deadline_extended:reaper 迟到启动一次性重挂;
//   - branch.evaluated:branch 路由决定(D-t24 #3);
//   - llm.call.completed:LLM 调用成功收口(T3.1),usage/cost 冗余列组唯一
//     携带者(Event.Usage;与 node_attempt 双录,T6.4 聚合免解 JSON);
//   - tool.call.completed:工具调用成功收口(T3.2,DDL 词表注释预留同名);
//     payload 无 token/凭证(铁律),时长观测归 T6.4;
//   - approval.requested:审批节点挂起(T4.1,DDL 词表注释预留同名);与
//     node.state_changed(EXECUTING→WAITING_APPROVAL)同事务双行——本行是
//     审批域事件(T4.4 待办/T6.4 积压指标字源),快照全文真相源在
//     node_execution.input_snapshot,payload 不复制自由文本(D-t26 §4);
//   - approval.decided:审批决定落库(T4.2;与 approval_decision 行 +
//     outbox execution.resume 事件同事务,api 审批面发射)。actor=user/
//     决定者;node_id 携目标节点,attempt_id NULL(决定先于续跑 attempt 创建);
//   - approval.decision_denied:越权决定尝试被拒(T4.2;issue #33 硬约束
//     「越权调用被拒并落审计」)。actor=user/尝试者;独立小事务(主事务无
//     别的写入,拒绝路径不与决定行同存亡)。
//   - approval.escalated:审批超时升级发射(T4.3,Q9 escalate 策略;与
//     escalated_at 落定 + approval.notify outbox 事件同事务,reaper 发射,
//     恰好一次锚在 escalated_at IS NULL 谓词)。actor=system/reaper。
//   - approval.resume_redispatched:resume 死信网幂等重投(T4.3,D-t42 §8-②
//     移交;已决票+超阈 WAITING_APPROVAL 的续跑事件重投,与 outbox 行同事务,
//     reaper 发射)。actor=system/reaper。
//   - approval.notified:审批通知送达(T4.5 SMTP;worker 消费 approval.notify
//     事件、邮件发送成功后落行)。与 escalated(意图)/notify outbox 事件不同
//     事务——本行是**送达**事实,兼作 at-least-once 重投的去重锚(先查行后
//     发送,命中即 ack 跳过;task_id=notify:<node_exec_id> 与 (execution_id,
//     node_execution_id) 单射)。actor=worker/消费进程。T6.4 对账面:
//     escalated 有行而本行缺 = 通知丢失(D-t43 已知边界获得观测信号)。
//   - execution.triggered:API 触发落执行(T5.5;DDL 开放词表注释预留同名,
//     D-t26 §6 显式留给 T5 接入登记)。执行级事件(node_id NULL),与
//     scheduler 侧行不同事务(Instantiate 不写审计,api 二段事务补)。
//   - tool.call.denied:执行时 RBAC 越权工具调用被拒(T5.2;ARCHITECTURE
//     §3.4 双重校验执行侧半边,先例 = approval.decision_denied)。actor=
//     user/被拒主体(workflow_execution.triggered_by);独立小事务(由
//     auth.ExecutionAuthorizer 在调用现场发射,best-effort——拒绝裁决
//     不受旁路写入失败影响)。
const (
	EventNodeStateChanged           = "node.state_changed"
	EventNodeRedispatched           = "node.redispatched"
	EventAttemptReaped              = "attempt.reaped"
	EventNodeDeadlineExtended       = "node.deadline_extended"
	EventBranchEvaluated            = "branch.evaluated"
	EventLLMCallCompleted           = "llm.call.completed"
	EventToolCallCompleted          = "tool.call.completed"
	EventApprovalRequested          = "approval.requested"
	EventApprovalDecided            = "approval.decided"
	EventApprovalDecisionDenied     = "approval.decision_denied"
	EventApprovalEscalated          = "approval.escalated"
	EventApprovalResumeRedispatched = "approval.resume_redispatched"
	EventApprovalNotified           = "approval.notified"
	EventExecutionTriggered         = "execution.triggered"
	EventToolCallDenied             = "tool.call.denied"
)

// actor 词表逐字 = DDL actor_type 列注释(user/system/worker/tool/llm/approval)。
// 发射面演进:P0 worker(system 由 reaper);T4.2 起 user(api 审批面);
// T5.2 起 user 的第二发射点(auth.ExecutionAuthorizer 的 tool.call.denied)。
// tool/llm/approval 仍为预留,校验按全集——DDL 开放面不变。
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
	EventNodeStateChanged:           true,
	EventNodeRedispatched:           true,
	EventAttemptReaped:              true,
	EventNodeDeadlineExtended:       true,
	EventBranchEvaluated:            true,
	EventLLMCallCompleted:           true,
	EventToolCallCompleted:          true,
	EventApprovalRequested:          true,
	EventApprovalDecided:            true,
	EventApprovalDecisionDenied:     true,
	EventApprovalEscalated:          true,
	EventApprovalResumeRedispatched: true,
	EventApprovalNotified:           true,
	EventExecutionTriggered:         true,
	EventToolCallDenied:             true,
}

var knownActors = map[string]bool{
	ActorUser: true, ActorSystem: true, ActorWorker: true,
	ActorTool: true, ActorLLM: true, ActorApproval: true,
}

var traceIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Event 是 audit_log 行的写入参数。ID 应用生成(snowflake,复用调用方生成器),
// 零值被拒。NodeExecID/AttemptID/NodeID/TraceID 零值/空串 → NULL(P0 各事件的
// 携带约定见 payload 结构注释)。Usage 非 nil 时落 usage/cost 冗余列组
// (llm.call.completed 专属;engine.AttemptUsage 与 node_attempt 半边同源)。
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
	Usage       *engine.AttemptUsage
}

// Append 在调用方事务内直写一条审计行(典型形状见 engine/dbtx.go 注释:
// CAS 迁移 → audit.Append → outbox.Append,同事务同存亡)。
//
// 两语句形状(零锁物化):[1] 普通一致读 DATE_FORMAT(created_at,'%Y-%m-%d')
// 直出字符串——不取任何锁,审计写入器因此在全局锁序之外;created_at 行
// INSERT 后不可变,一致读快照与当前读恒同值。[2] INSERT VALUES 落行(恒 1 行
// 或报错,无 RowsAffected 裁决面)。代价 = 每审计行一次 PK 点查,P0 量级噪声;
// 批量去重归 T6.1。
//
// 错误分类:ErrInvalidEvent(调用方 bug,不触 DB);ErrNotFound(execution
// 缺行 = 数据缺陷,[1] ErrNoRows);其余 %w 包装上抛,Transient 判定归调用方
// (与 outbox 同规)。
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
	// PII 脱敏钩子(T6.1):先于落库(校验前置不触 DB 契约的延续),
	// 「原始永不入 payload」;未掩蔽时字节原样(零成本路径)。同步/异步
	// (Append/Enqueue)两侧同规。
	payload, piiMasked, sanitizeErr := SanitizePayload(evt.Payload)
	if sanitizeErr != nil {
		return sanitizeErr
	}
	// [1] 一致读物化分区键(零锁,见方法注释)。DATE_FORMAT 直出字符串:
	// 不经 driver 的 time.Location 解析——go-sql-driver 把 DATE 解析为 DSN loc
	// 的午夜,loc≠UTC 时 .UTC() 换算会静默移日(review 修;字符串往返 loc 无关,
	// 日期语义完全留在 DB 端)。
	var execDate string
	err := q.QueryRowContext(ctx,
		`SELECT DATE_FORMAT(created_at, '%Y-%m-%d') FROM workflow_execution WHERE id=?`,
		evt.ExecutionID).Scan(&execDate)
	if errors.Is(err, sql.ErrNoRows) {
		// 审计行必须挂在真实执行上,静默无写会破坏「每次迁移必有审计行」验收。
		return fmt.Errorf("%w: audit event %d: execution %d vanished",
			engine.ErrNotFound, evt.ID, evt.ExecutionID)
	}
	if err != nil {
		return fmt.Errorf("audit: read exec_date (exec %d): %w", evt.ExecutionID, err)
	}
	// [2] 落行:event_at = DB 端 NOW(3)。Usage 非 nil 时同语句落 usage/cost
	// 冗余列组(llm.call.completed 专属形状;Billed=false ⇒ cost 两列 NULL,
	// 与 node_attempt 半边成对 CHECK 同语义)。
	if evt.Usage != nil {
		var costArg, currencyArg any
		if evt.Usage.Billed {
			costArg, currencyArg = evt.Usage.CostAmount, evt.Usage.CostCurrency
		}
		_, err = q.ExecContext(ctx, `
INSERT INTO audit_log
  (id, exec_date, event_at, event_type, execution_id, node_execution_id,
   node_id, attempt_id, actor_type, actor_id, trace_id, pii_masked, payload,
   model, prompt_tokens, completion_tokens, total_tokens,
   cost_amount, cost_currency, cost_estimated)
VALUES (?, ?, NOW(3), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
        ?, ?, ?, ?, ?, ?, ?)`,
			evt.ID, execDate, evt.EventType, evt.ExecutionID,
			nodeExecID, nodeID, attemptID, evt.ActorType, evt.ActorID, trace, piiMasked, payload,
			evt.Usage.Model, evt.Usage.PromptTokens, evt.Usage.CompletionTokens,
			evt.Usage.TotalTokens, costArg, currencyArg, evt.Usage.Estimated)
	} else {
		_, err = q.ExecContext(ctx, `
INSERT INTO audit_log
  (id, exec_date, event_at, event_type, execution_id, node_execution_id,
   node_id, attempt_id, actor_type, actor_id, trace_id, pii_masked, payload)
VALUES (?, ?, NOW(3), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			evt.ID, execDate, evt.EventType, evt.ExecutionID,
			nodeExecID, nodeID, attemptID, evt.ActorType, evt.ActorID, trace, piiMasked, payload)
	}
	if err != nil {
		return fmt.Errorf("audit: append %d (%s): %w", evt.ID, evt.EventType, err)
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
	case evt.Usage != nil && evt.EventType != EventLLMCallCompleted:
		return fmt.Errorf("%w: usage columns on %q (llm.call.completed only)",
			ErrInvalidEvent, evt.EventType)
	}
	// usage 组域约束镜像(engine 单点定义,双录两半边同规)。
	if err := evt.Usage.Validate(); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidEvent, err)
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
