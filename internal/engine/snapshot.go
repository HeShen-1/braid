package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// 输入快照契约(验收 2 的真源,DDL node_execution.input_snapshot 列注释):
//
// 何时写:Claim/Resume/Rerun(T9.2,D-2026-09-28-t92-rollback-same-branch
// 决策 2b)三条进入 EXECUTING 的 CAS **同语句**写入/重写——列级天然单写者,
// 恰好由唯一获胜者写一次;败方 0 行,字节不可能落行。Rerun 的输入 =
// ReadNodeInput 读回值(「以 input_snapshot 重跑」的逐字兑现,复现语义锚);
// 闭包重置(ResetNodeToPending)只清列不写(PENDING⇒NULL 不变量),下游
// 重派由求值器对新前驱输出重求值。
//
// 写什么:全部 {{...}} 已对 workflow_execution.input 根 + 前驱 output 求值的
// 已解析 JSON(T2.4 起由求值器填充,引擎不解析、存原文、仅 json.Valid)。
//
// 不变量(单向;CANCELLED 继承来源行,不约束):
//   - status ∈ {PENDING, SKIPPED}            ⇒ input_snapshot IS NULL
//     (PENDING 含 requeue 后:RequeueNode 清列,重认领时重写)
//   - status ∈ {EXECUTING, WAITING_APPROVAL,
//                SUCCEEDED, FAILED}           ⇒ input_snapshot IS NOT NULL
//
// 复现语义:显式数据流无共享黑板 ⇒ 节点有效输入 ≡ snapshot 语义内容,与解析
// 路径/到达顺序/重试次数无关——复现 = ReadNodeInput 读回即用,不是重放解析。
// 证明口径(测试):decode→深比较(防键序侥幸)+ 读回确定性(连读两次字节相同,
// 防驱动/charset 失真)。字节级原文相等**不被承诺**:MySQL JSON 列规范化存储
// (键重排/去空白),故「json.Compact 字节门」不可作为普适判据。

// ReadNodeInput 读回节点输入快照(复现入口)。NULL(从未进入 EXECUTING 的
// PENDING/SKIPPED/CANCELLED 行)返回 nil, nil,由调用方按状态解释。
func ReadNodeInput(ctx context.Context, q DBTX, nodeExecID uint64) (raw []byte, err error) {
	err = q.QueryRowContext(ctx,
		`SELECT input_snapshot FROM node_execution WHERE id=?`, nodeExecID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: node_execution %d", ErrNotFound, nodeExecID)
	}
	if err != nil {
		return nil, fmt.Errorf("engine: read node input %d: %w", nodeExecID, err)
	}
	return raw, nil
}
