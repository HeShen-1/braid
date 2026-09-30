package audit

// T6.1 异步批量写入的发射半边:高频事件(node.state_changed / branch.evaluated
// / llm.call.completed / tool.call.completed,scheduler 热路径全集)在状态
// 迁移同一事务内改为暂存 audit_pending,由 reaper flush 循环批量搬运至
// audit_log(D-2026-09-24-t61-audit-async-batch)。低频/回读消费事件
// (approval.*、execution.triggered、attempt.reaped、node.redispatched、
// node.deadline_extended、tool.call.denied)仍走 Append 同步直写——
// 分阶段口径:批量收益兑现在热路径,去重锚(approval.notified)与证据级行
// 保持 read-your-writes。
//
// 与 Append 的契约差异(其余逐字同形:校验前置不触 DB、永不自管事务、
// 脱敏先于落库):
//   - 不查 workflow_execution:exec_date 物化与存在性检查都移到 flush 期
//     批量执行(点查出热事务正是异步化的动机;ghost execution 在 flush 期
//     响亮隔离,见 flush.go);
//   - event_at = 暂存事务时刻 NOW(3)(事件时刻=迁移时刻保持——与
//     status_changed_at 同钟系;「事件时刻≠落库时刻」的落库半边 =
//     audit_log.created_at,flush 期默认)。

import (
	"context"
	"encoding/json"
	"fmt"

	"braid/internal/engine"
)

// Enqueue 在调用方事务内暂存一条审计行(audit_pending)。典型形状:
// CAS 迁移 → audit.Enqueue → outbox.Append,同事务同存亡。
//
// 错误分类同 Append:ErrInvalidEvent(调用方 bug,不触 DB);其余 %w 包装
// 上抛,Transient 判定归调用方。
func Enqueue(ctx context.Context, q engine.DBTX, evt Event) error {
	applyCtxTrace(ctx, &evt)
	if len(evt.Payload) == 0 {
		evt.Payload = json.RawMessage(`{}`) // DDL:无正文事件存 {}
	}
	if err := validate(evt); err != nil {
		return err
	}
	payload, piiMasked, err := SanitizePayload(evt.Payload)
	if err != nil {
		return err
	}
	var nodeExecID, attemptID, trace, linked any
	if evt.NodeExecID != 0 {
		nodeExecID = evt.NodeExecID
	}
	if evt.AttemptID != 0 {
		attemptID = evt.AttemptID
	}
	nodeID := any(evt.NodeID)
	if evt.NodeID == "" {
		nodeID = nil // NULL=执行级事件判别锚,同 Append
	}
	if evt.TraceID != "" {
		trace = evt.TraceID
	}
	if evt.LinkedTraceID != "" {
		linked = evt.LinkedTraceID
	}
	// usage 冗余列组(llm.call.completed|discarded 双词,validate 已闸):单一语句形态,
	// NULL 经 any 归一(Billed=false ⇒ cost 两列 NULL,与 Append 同语义)。
	var model, costArg, currencyArg any
	var pt, ct, tt any
	if evt.Usage != nil {
		model, pt, ct, tt = evt.Usage.Model,
			evt.Usage.PromptTokens, evt.Usage.CompletionTokens, evt.Usage.TotalTokens
		if evt.Usage.Billed {
			costArg, currencyArg = evt.Usage.CostAmount, evt.Usage.CostCurrency
		}
	}
	// 串链(T9.3,逐行链写入即算):chain_heads 行锁 → 断链拒 → hash;
	// 暂存行带着 hash 运输,flush 纯搬运(串链与搬运解耦,flush 不取
	// head 锁)。链语义与直写半边(writer.go)同源(appendChain 单点)。
	prevHash, rowHash, err := appendChain(ctx, q, evt, payload, piiMasked)
	if err != nil {
		return err
	}
	// tenant_id 不在暂存 INSERT 列集(D-t112):暂存行按 DDL DEFAULT 0
	// 占位,flush 期随 exec_date 一并物化真实租户——暂存面不可见
	// (D-t63 #1:Enqueue 行 flush 前不进查询),占位无读面代价;
	// 发射点(Event)零扩字段,租户唯一真源 = 执行行。
	_, err = q.ExecContext(ctx, `
INSERT INTO audit_pending
  (id, event_at, event_type, execution_id, node_execution_id, node_id,
   attempt_id, actor_type, actor_id, trace_id, linked_trace_id, pii_masked,
   payload, prev_hash, row_hash,
   model, prompt_tokens, completion_tokens, total_tokens,
   cost_amount, cost_currency, cost_estimated)
VALUES (?, NOW(3), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		evt.ID, evt.EventType, evt.ExecutionID,
		nodeExecID, nodeID, attemptID, evt.ActorType, evt.ActorID, trace,
		linked, piiMasked, payload, prevHash, rowHash,
		model, pt, ct, tt, costArg, currencyArg, usageEstimated(evt.Usage))
	if err != nil {
		return fmt.Errorf("audit: enqueue %d (%s): %w", evt.ID, evt.EventType, err)
	}
	if err := advanceChainHead(ctx, q, evt.ExecutionID, evt.ID, rowHash); err != nil {
		return err
	}
	return nil
}

// usageEstimated 归一 Usage 缺省时的 cost_estimated 落值(nil Usage ⇒ false)。
func usageEstimated(u *engine.AttemptUsage) bool {
	return u != nil && u.Estimated
}
