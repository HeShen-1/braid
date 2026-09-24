// Package outbox 实现事务性发件箱(0001_init.up.sql §5):事件行与节点/执行状态
// 变更在同一事务写入,relay 独立投递 Asynq。本包只提供存储原语,永不自管事务
// 边界(同 engine.DBTX 契约:事务时长与锁持有窗口归调用方)。
//
// DDL 契约要点(契约真源见 §5 列注释):
//   - 无 FK(写在状态迁移热路径上,省外键开销):「状态行与事件行同存亡」由
//     事务契约保证,T2.2 判别性故障测试断言,idx_outbox_exec 对账兜底;
//   - task_id 确定性且写入时已知(NOT NULL);不唯一——reconciler 幂等重入队可
//     合法产生携带同 task_id 的新事件行,Asynq 侧 ErrTaskIDConflict 幂等去重;
//   - relay 只投不写业务表(payload 自描述,仅 id 引用,投递不回查业务表)。
//
// TaskID 词表不变量(ErrTaskIDConflict 幂等收敛的安全前提,勿破坏):词表全部
// 由不可变标识符构成——同一 task_id 永不表达「需要再投一次的新意图」。判死
// 重入队天然携带新 attempt_no(att: 词表)满足此条;resume: 词表同理四段化
// (T4.2,原三段式 resume:<exec>:<node> 是已知豁口:同 (exec,node) 二次续跑
// 复用同 ID,被 asynq 侧仍滞留的 archived 副本误判「已投递」而静默挡死)——
// 现形态 resume:<exec>:<node>:<attempt>,attempt = 本次续跑将创建的 attempt 号
// (API 事务 node 行锁内读出,确定性):同一续跑意图的对账重投复用同 ID(冲突
// 即收敛);新续跑轮次(P1 回滚重挂→再挂起)携带新 attempt ⇒ 新 ID。契约记录
// = 迁移 0003 头注释 + D-2026-09-24-t42-approval-resume(0001 §5 注释形态由
// 0003 扩展,0001 已应用不可改);该不变量由 T2.3 验收断言承接。
package outbox

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
// engine.ErrInvalidSnapshot)。
var ErrInvalidEvent = errors.New("outbox: invalid event")

const (
	// maxPayloadBytes 是事件体上限(bug 防线:payload 契约是「仅 id 引用」的
	// 自描述小事件,业务大数据应留在业务表由 worker 回读)。
	maxPayloadBytes = 64 << 10

	maxEventText = 64 // event_type/queue 与 DDL VARCHAR(64) 对齐
	// maxErrText 与 DDL last_error VARCHAR(2000) 对齐(RecordFailure 截断用)。
	maxErrText = 2000
)

// TaskID 词表(全库统一,DDL §5 表注释 + 0003 扩展):node_id 段与 DSL 词表
// 一致(dsl/refs.go 同款 [a-z][a-z0-9_]{0,63})。notify: 是 T4.3 审批超时
// 升级通知(approval.notify)的 TaskID:单段 node_execution_id——恰好一次
// 语义锚在 node_execution.escalated_at(DB 谓词),同节点永无第二条通知意图,
// 不变量「同一 task_id 永不表达需要再投一次的新意图」因此保持。
var (
	taskIDReExec   = regexp.MustCompile(`^exec:\d+$`)
	taskIDReResume = regexp.MustCompile(`^resume:\d+:[a-z][a-z0-9_]{0,63}:\d+$`)
	taskIDReAtt    = regexp.MustCompile(`^att:\d+:\d+$`)
	taskIDReNotify = regexp.MustCompile(`^notify:\d+$`)
	queueRe        = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	traceIDRe      = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// Event 是 outbox 行的写入参数。ID 应用生成(snowflake),零值被拒。
//
// Queue 边界:本包只校验字符集,不校验队列是否被 worker ServeQueues——入队到
// 未 serve 的队列会永挂 pending(outbox 已 DISPATCHED、execution 卡 QUEUED,
// 靠 T2.5 reconciler 超时兜底);队列集合正确性归 worker 配置 + 运维 runbook
// (T6.4 积压告警)。
type Event struct {
	ID          uint64
	ExecutionID uint64
	EventType   string          // 开放词表(node.task_enqueue/execution.resume/…),亦作 Asynq task type
	TaskID      string          // 词表校验,见上
	Queue       string          // 空 → "default"
	Payload     json.RawMessage // 自描述事件体,合法 JSON
	TraceID     string          // 可空;非空必须 32 hex 小写(OTel,T6.2 前占位不伪造)
}

// Append 在调用方事务内写入一条 PENDING 事件行(available_at=NOW(3) 立即可投)。
// 时间权威在 DB 端(T2.1 同规)。典型形状(见 engine/dbtx.go 注释):
//
//	engine.WithTx(ctx, db, func(q engine.DBTX) error {
//	    _, err := engine.ClaimNode(ctx, q, …)   // 状态迁移
//	    if err != nil { return err }
//	    return outbox.Append(ctx, q, evt)        // 同事务事件
//	})
//
// 错误分类:ErrInvalidEvent(调用方 bug,不触 DB);其余统一 %w 包装上抛,
// 分类由调用方进行——1213/1205 经 engine.Transient 判定(整 tx 回滚后抖动
// 重试),1062(PK 撞 = 生成器缺陷,通常是跨进程雪花重号)不映射 ErrConflict
// (那会诱发 T2.5 式重决策循环),告警归 T6.4。
func Append(ctx context.Context, q engine.DBTX, evt Event) error {
	if evt.Queue == "" {
		evt.Queue = "default" // 规范化必须发生在 validate 之外:validate 按值收参
	}
	if err := validate(evt); err != nil {
		return err
	}
	var trace any
	if evt.TraceID != "" {
		trace = evt.TraceID
	}
	_, err := q.ExecContext(ctx, `
INSERT INTO outbox
  (id, execution_id, event_type, task_id, queue, payload, status,
   deliver_attempts, available_at, trace_id)
VALUES (?,?,?,?,?,?,'PENDING',0,NOW(3),?)`,
		evt.ID, evt.ExecutionID, evt.EventType, evt.TaskID, evt.Queue, evt.Payload, trace)
	if err != nil {
		return fmt.Errorf("outbox: append %d (task %s): %w", evt.ID, evt.TaskID, err)
	}
	return nil
}

func validate(evt Event) error {
	switch {
	case evt.ID == 0:
		return fmt.Errorf("%w: zero id", ErrInvalidEvent)
	case evt.ExecutionID == 0:
		return fmt.Errorf("%w: zero execution id", ErrInvalidEvent)
	case evt.EventType == "" || len(evt.EventType) > maxEventText:
		return fmt.Errorf("%w: event_type %q", ErrInvalidEvent, evt.EventType)
	case !validTaskID(evt.TaskID):
		return fmt.Errorf("%w: task_id %q not in vocabulary (exec:/resume:/att:/notify:)", ErrInvalidEvent, evt.TaskID)
	}
	if !queueRe.MatchString(evt.Queue) || len(evt.Queue) > maxEventText {
		return fmt.Errorf("%w: queue %q", ErrInvalidEvent, evt.Queue)
	}
	if len(evt.Payload) == 0 || !json.Valid(evt.Payload) {
		return fmt.Errorf("%w: payload not valid JSON", ErrInvalidEvent)
	}
	if len(evt.Payload) > maxPayloadBytes {
		return fmt.Errorf("%w: payload %d bytes > %d", ErrInvalidEvent, len(evt.Payload), maxPayloadBytes)
	}
	if evt.TraceID != "" && !traceIDRe.MatchString(evt.TraceID) {
		return fmt.Errorf("%w: trace_id %q", ErrInvalidEvent, evt.TraceID)
	}
	return nil
}

func validTaskID(id string) bool {
	return taskIDReExec.MatchString(id) || taskIDReResume.MatchString(id) ||
		taskIDReAtt.MatchString(id) || taskIDReNotify.MatchString(id)
}

// clampErrLen 截断错误摘要到 DDL last_error 宽度。按字节拦腰截多字节字符会
// 产出非法 UTF-8,utf8mb4 严格模式拒收(1366)→ 记账失败回滚 → 行永动重试;
// 故回退到最后一个完整 rune 边界。
func clampErrLen(msg string) string {
	if len(msg) <= maxErrText {
		return msg
	}
	cut := maxErrText
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut]
}
