package outbox

import (
	"context"
	"fmt"
	"time"

	"braid/internal/engine"
)

// outbox 行状态迁移原语(relay 调用)。outbox 无 version 列 ⇒ status-only CAS
// (WHERE id=? AND status='PENDING',与 workflow_execution 同款裁决形态)。
//
// 前置契约(安全论证的前提,评审 checklist 项):三条原语必须在**已对该行持
// FOR UPDATE 锁的事务内**调用(relay 扫描即如此)。在该前提下 UPDATE 影响 0 行
// 没有任何合法成因——并发者改不了行(锁互斥)、行不会消失(本事务刚锁到,
// T2.2 无 DELETE 路径)、无「同值 no-op」(三条语句 deliver_attempts 恒 +1 ⇒
// 必有列变值)。故 0 行不做复查、直接报错回滚 + 告警。
//
// 对照 T2.1 TouchExecution 教训的正确迁移:该案例 0 行有合法成因(同毫秒幂等
// bump 的 changed-rows=0),故须 FOR UPDATE 复查区分「同值」与「缺行」;教训是
// 「0 行先问有无合法成因」,不是「一律复查」。锁窗内复查只能读到自己,零信息量。
//
// deliver_attempts 计数语义:三条语句都 +1,计数 = **有结论的投递尝试**。
// 被 kill -9 的尝试没有结论(无已提交的事务),不计数——T2.2 验收②断言
// A1(attempts=0)/B2(attempts=1)由此定。
//
// outbox 状态变更只经本文件三原语(AGENTS 铁律「节点状态迁移只经 CAS helper」
// 的延伸;DISPATCHED 行 7 天清理归 T2.5 reaper,不在本任务)。

// MarkDispatched:投递成功(或 ErrTaskIDConflict 冲突收敛)后标记。
// dispatched_at 语义:常规路径 ≈ 实际入队时刻;冲突收敛/崩溃恢复路径是
// **标记时刻**(真实入队可能发生在更早的崩溃前)——T6.4 投递时延度量在
// 该路径会虚高(差 = 停机时长),指标语义见 T6.4。
func MarkDispatched(ctx context.Context, q engine.DBTX, id uint64) error {
	return mark(ctx, q, id, "dispatched", `
UPDATE outbox
   SET status='DISPATCHED', deliver_attempts=deliver_attempts+1, dispatched_at=NOW(3)
 WHERE id=? AND status='PENDING'`, id)
}

// RecordFailure:投递失败未到上限——记失败摘要并把 available_at 推到
// now+backoff(DB 端 NOW(3) 计算,多实例时钟漂移不入库)。backoff 的抖动由
// 调用方算好传入。
func RecordFailure(ctx context.Context, q engine.DBTX, id uint64, errMsg string, backoff time.Duration) error {
	return mark(ctx, q, id, "record failure", `
UPDATE outbox
   SET deliver_attempts=deliver_attempts+1, last_error=?,
       available_at=DATE_ADD(NOW(3), INTERVAL ? MICROSECOND)
 WHERE id=? AND status='PENDING'`, clampErrLen(errMsg), backoff.Microseconds(), id)
}

// MarkDead:尝试穷尽,转 DEAD(T6.4 死信告警源;重放 = 人工重置 PENDING,
// T7.4 runbook)。上限比较在调用方(持锁读到的 deliver_attempts 即当前值,
// 稳定;谓词里多余的条件只会掩盖不一致)。DEAD ≠ 未投递:超时误报路径可造出
// 「DEAD 行 + Redis 仍持有活任务」,T7.4 重置前必须先核验 Redis 无记录
// (idx_outbox_task 反查 + Inspector),否则可能二次执行。
func MarkDead(ctx context.Context, q engine.DBTX, id uint64, errMsg string) error {
	return mark(ctx, q, id, "mark dead", `
UPDATE outbox
   SET deliver_attempts=deliver_attempts+1, last_error=?, status='DEAD'
 WHERE id=? AND status='PENDING'`, clampErrLen(errMsg), id)
}

func mark(ctx context.Context, q engine.DBTX, id uint64, what, query string, args ...any) error {
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("outbox: %s %d: %w", what, id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("outbox: %s %d rows affected: %w", what, id, err)
	}
	if n != 1 {
		// 持锁窗内 0 行无合法成因(见文件头论证)。无锁误用下 0 行另有合法成因
		// (并发迁移获胜),文案保持中性,不妄断成因。
		return fmt.Errorf("outbox: %s %d affected %d rows (row not locked under FOR UPDATE, or already transitioned)", what, id, n)
	}
	return nil
}
