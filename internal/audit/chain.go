package audit

// T9.3 哈希链的链原语(D-2026-09-29-t93 #2):per-execution 串行化锚 =
// chain_heads 行锁。锁面自持于本表——不触 workflow_execution(D-t26
// lockfree-read 哲学延续:当年 FOR UPDATE 即与 reaper 扫描 1213),不进
// 全局锁序;head 行锁恒为写事务的**末段**获取锁(CAS → audit → outbox
// 形状下与全部调用方同序,无环)。
//
// 串行化协议(两语句,无 miss 分支)——**必须在调用方事务内**:
//  1. anchor:INSERT ... ON DUPLICATE KEY UPDATE last_id=last_id(自赋值
//     no-op)——缺行时建占位头(last_id=0, ''),在场时仅取行锁;两个
//     首写者的竞态被压成串行(后者阻塞至前者提交,随后 SELECT 见真头)。
//  2. SELECT ... FOR UPDATE 读链尾(last_row_hash 即下一行 prev_hash 的
//     唯一真源——同步行经它拿到在途暂存行 hash)。
//
// 占位头(last_id=0)只在写事务内存在:行落库 + head 推进同事务,回滚
// 整体消失(原子性锚见 chain_test TestChainRollbackAtomicity)。
//
// ⚠ 事务契约是硬边界(对抗审查 HIGH,非注记):裸 *sql.DB(autocommit)
// 下,FOR UPDATE 在其语句自身提交即释放——锚定/读尾/落行/推头四语句散落
// 在不同隐式事务,同执行并发写者读到同一链尾 ⇒ **链叉**(两行同 prev,
// 诚实战况被误闩断链)+ 占位头永久残留。因此 appendChain 对池句柄
// fail-closed(ErrTxRequired),调用方必须经 engine.WithTx(或既有大事务)
// 传入 *sql.Tx/*sql.Conn。
//
// 链序 = prev_hash 链接序(链首 prev=''),**不是 id 序**:Event.ID 由
// 调用方在上锁前分配,snowflake 同毫秒跨进程次序由 node 位决定
// (snowflake.go 免责条款:「id 序只是公平调度序,不是因果序」)——先链
// 后写者的 id 可以更小。校验(verify.go)按链接序走链,id 序仅作展示序
// 与轮换触发启发式。
//
// 断链闩:broken_at 非 NULL ⇒ 双写路径响亮拒绝(ErrChainBroken,
// fail-closed;该执行业务停摆待人工,其余执行不受累)。闩由校验作业
// (verify.go,T9.3 #5)落定,人工复位。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"braid/internal/engine"
)

// ErrChainBroken:断链闩已落定(校验作业检出的证据完整性违约)。写路径
// fail-closed 拒绝新 append;非 transient——重试不消,人工复位才放行。
var ErrChainBroken = errors.New("audit: chain broken")

// ErrTxRequired:写路径收到裸池句柄(*sql.DB)——串链协议的四语句必须
// 同事务原子,autocommit 散语句会产生链叉与残留占位头(见文件头 ⚠)。
// 调用方 bug,fail-closed 在触 DB 之前。
var ErrTxRequired = errors.New("audit: caller transaction required (pool handle cannot serialize the chain)")

// chainHead 是 chain_heads 的读形状(锁内快照)。
type chainHead struct {
	lastID       uint64
	lastRowHash  string // '' = 链未延伸(占位头);否则 = 下一行 prev_hash
	broken       bool
	brokenReason string
}

// requireTx 是串链写路径的事务契约闸(ErrTxRequired 语义见文件头 ⚠)。
// **只放行 *sql.Tx**(白名单而非黑名单:裸 *sql.Conn 同样是 autocommit,
// 黑名单式「拒 *sql.DB」会漏过它——对抗审计 LOW 修;conn.BeginTx 返回的
// 本就是 *sql.Tx,不受影响)。
func requireTx(q engine.DBTX) error {
	if _, isTx := q.(*sql.Tx); !isTx {
		return ErrTxRequired
	}
	return nil
}

// lockChainHead 在调用方事务内锚定并锁定执行链头(见文件头协议)。
// ODKU 自赋值形态避免 VALUES()(8.0.20 弃用潮),no-op 语义 = 仅取行锁。
func lockChainHead(ctx context.Context, q engine.DBTX, executionID uint64) (chainHead, error) {
	var h chainHead
	if err := requireTx(q); err != nil {
		return h, err
	}
	if _, err := q.ExecContext(ctx, `
INSERT INTO chain_heads (execution_id, last_id, last_row_hash)
VALUES (?, 0, '')
ON DUPLICATE KEY UPDATE last_id = last_id`, executionID); err != nil {
		return h, fmt.Errorf("audit: anchor chain head (exec %d): %w", executionID, err)
	}
	var brokenAt sql.NullTime
	var brokenReason sql.NullString
	err := q.QueryRowContext(ctx, `
SELECT last_id, last_row_hash, broken_at, broken_reason
  FROM chain_heads WHERE execution_id=? FOR UPDATE`, executionID).
		Scan(&h.lastID, &h.lastRowHash, &brokenAt, &brokenReason)
	if err != nil {
		return h, fmt.Errorf("audit: lock chain head (exec %d): %w", executionID, err)
	}
	h.broken = brokenAt.Valid
	if brokenReason.Valid {
		h.brokenReason = brokenReason.String
	}
	return h, nil
}

// advanceChainHead 推链尾(行已落库;行锁由本事务 lockChainHead 持有)。
// **无条件 UPDATE**——串行化由锁承载,不由 id 承载:链序 ≠ id 序(先链
// 后写者 id 可以更小,见文件头),`last_id < ?` 式 id 单调 guard 在诚实
// 倒置下静默匹配 0 行 ⇒ 头滞留旧链尾 ⇒ 下一次 append 从旧尾接链 = 系统
// 自造叉链(rev2 对抗审查 HIGH,guard 本身就是 bug;rev1 建议与「id 序
// ≠ 链序」互斥,以链序为准)。last_id 列语义 = 链尾指认启发式(0008
// DDL 钦定),随真链尾走。verified_* 不在此推进——校验状态只归校验作业。
func advanceChainHead(ctx context.Context, q engine.DBTX, executionID, lastID uint64, rowHash string) error {
	if _, err := q.ExecContext(ctx,
		`UPDATE chain_heads SET last_id=?, last_row_hash=? WHERE execution_id=?`,
		lastID, rowHash, executionID); err != nil {
		return fmt.Errorf("audit: advance chain head (exec %d): %w", executionID, err)
	}
	return nil
}

// chainLink 是校验读回形状:行 id(展示序/诊断)+ 存储链列(比对面)+
// 19 列规范化输入(重算面)。row_hash IS NOT NULL 谓词保证 storedRow 恒非
// NULL(存量未启链行跳过)。链序 = storedPrev 链接序,id 只是近似展示序
// (同毫秒跨进程倒置,见文件头)。
type chainLink struct {
	id         uint64
	storedPrev string
	storedRow  string
	row        chainRow
}

// chainFetchCap 是单执行全链读取的行数硬上界(防御臂:巨链执行的校验
// 耗时/内存有界化)。命中 = 跳过本轮该执行并响亮记日志(不闩——是规模
// 事件不是完整性事件);登记 D-t93 已知边界,演化路径 = 分页 + 外部锚。
const chainFetchCap = 20000

// readWholeChain 读回执行的全链行(校验半边;RowsDBTX 多行读面——DBTX
// 单行面表达不了多行读,engine.T9.1 嵌入扩展先例)。**单语句 UNION ALL
// 双表** = 单一致读快照:链跨 audit_log ∪ audit_pending 延伸(同步行可
// 先于其在途暂存前驱落地),分读两表会把「flush 提交于两读之间」的行
// 漏出 → 误闩(fault e2e 实证)。全链单读(无游标):链接序装配必须见到
// 全部行,id 游标会把倒置对的另一翼切在窗外。cost 走 CAST(AS CHAR)
// (DECIMAL 精度不经浮点,读回恒六位定标 = costScale6 形态,flush.go
// 同判);payload 直读字节(MySQL 规范化形,canonicalizer 归一)。
func readWholeChain(ctx context.Context, q engine.RowsDBTX, execDate string, executionID uint64) ([]chainLink, error) {
	rows, err := q.QueryContext(ctx, `
SELECT id, prev_hash, row_hash, event_type, execution_id,
       node_execution_id, node_id, attempt_id, actor_type, actor_id, trace_id,
       linked_trace_id, pii_masked, model, prompt_tokens, completion_tokens,
       total_tokens, CAST(cost_amount AS CHAR), cost_currency, cost_estimated, payload
  FROM audit_log
 WHERE exec_date=? AND execution_id=? AND row_hash IS NOT NULL
UNION ALL
SELECT id, prev_hash, row_hash, event_type, execution_id,
       node_execution_id, node_id, attempt_id, actor_type, actor_id, trace_id,
       linked_trace_id, pii_masked, model, prompt_tokens, completion_tokens,
       total_tokens, CAST(cost_amount AS CHAR), cost_currency, cost_estimated, payload
  FROM audit_pending
 WHERE execution_id=? AND row_hash IS NOT NULL
ORDER BY id
LIMIT ?`, execDate, executionID, executionID, chainFetchCap+1)
	if err != nil {
		return nil, fmt.Errorf("audit: read whole chain (exec %d): %w", executionID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []chainLink
	for rows.Next() {
		var (
			l             chainLink
			nodeExecID    sql.NullInt64
			nodeID        sql.NullString
			attemptID     sql.NullInt64
			traceID       sql.NullString
			linkedTrace   sql.NullString
			model         sql.NullString
			promptTok     sql.NullInt64
			complTok      sql.NullInt64
			totalTok      sql.NullInt64
			costAmount    sql.NullString
			costCurrency  sql.NullString
			costEstimated bool
		)
		if err := rows.Scan(&l.id, &l.storedPrev, &l.storedRow, &l.row.EventType, &l.row.ExecutionID,
			&nodeExecID, &nodeID, &attemptID, &l.row.ActorType, &l.row.ActorID,
			&traceID, &linkedTrace, &l.row.PIIMasked,
			&model, &promptTok, &complTok, &totalTok,
			&costAmount, &costCurrency, &costEstimated, &l.row.Payload); err != nil {
			return nil, fmt.Errorf("audit: scan chain row: %w", err)
		}
		l.row.ID = l.id
		if nodeExecID.Valid {
			l.row.NodeExecID = uint64(nodeExecID.Int64)
		}
		l.row.NodeID = nodeID.String // Invalid ⇒ "" = NULL 归一
		if attemptID.Valid {
			l.row.AttemptID = uint64(attemptID.Int64)
		}
		l.row.TraceID = traceID.String
		l.row.LinkedTraceID = linkedTrace.String
		l.row.Model = model.String
		if promptTok.Valid {
			l.row.PromptTokens = uint64(promptTok.Int64)
		}
		if complTok.Valid {
			l.row.CompletionTokens = uint64(complTok.Int64)
		}
		if totalTok.Valid {
			l.row.TotalTokens = uint64(totalTok.Int64)
		}
		l.row.CostAmount = costAmount.String
		l.row.CostCurrency = costCurrency.String
		l.row.CostEstimated = costEstimated
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: chain rows iterate: %w", err)
	}
	return out, nil
}

// chainRowFromEvent 把写入参数(已脱敏 payload)映射为链输入(19 列,
// 落库归一同形:零值 ⇒ NULL 臂)。cost 传引擎原串(rowHash 内部六位定标,
// 与 DB 读回形态等价)。
func chainRowFromEvent(evt Event, payload []byte, piiMasked bool) chainRow {
	r := chainRow{
		ID:            evt.ID,
		EventType:     evt.EventType,
		ExecutionID:   evt.ExecutionID,
		NodeExecID:    evt.NodeExecID,
		NodeID:        evt.NodeID,
		AttemptID:     evt.AttemptID,
		ActorType:     evt.ActorType,
		ActorID:       evt.ActorID,
		TraceID:       evt.TraceID,
		LinkedTraceID: evt.LinkedTraceID,
		PIIMasked:     piiMasked,
		Payload:       payload,
	}
	if evt.Usage != nil {
		r.Model = evt.Usage.Model
		r.PromptTokens = uint64(evt.Usage.PromptTokens)
		r.CompletionTokens = uint64(evt.Usage.CompletionTokens)
		r.TotalTokens = uint64(evt.Usage.TotalTokens)
		if evt.Usage.Billed {
			r.CostAmount = evt.Usage.CostAmount
			r.CostCurrency = evt.Usage.CostCurrency
		}
		r.CostEstimated = evt.Usage.Estimated
	}
	return r
}

// appendChain 是双写路径共享的串链段(锁 → 断链拒 → hash);行落库与
// head 推进由调用方语句形状各自承载(INSERT 列集不同,链语义相同)。
func appendChain(ctx context.Context, q engine.DBTX, evt Event, payload []byte, piiMasked bool) (prev string, rowHash string, err error) {
	head, err := lockChainHead(ctx, q, evt.ExecutionID)
	if err != nil {
		return "", "", err
	}
	if head.broken {
		return "", "", fmt.Errorf("%w: execution %d: %s", ErrChainBroken, evt.ExecutionID, head.brokenReason)
	}
	rowHash, err = chainRowFromEvent(evt, payload, piiMasked).rowHash(head.lastRowHash)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrInvalidEvent, err)
	}
	return head.lastRowHash, rowHash, nil
}
