package audit

// T6.1 异步批量写入的搬运半边:audit_pending → audit_log 的批量 flush。
// 消费方 = reaper flush 循环(独立 goroutine,同 janitorLoop 形态;leaderless
// 重叠下并发安全,见下)与测试。
//
// 批量契约(D-2026-09-24-t61,铁律面;GATE 2 修订:预过滤 + 裸 INSERT,
// 否决 ON DUPLICATE KEY UPDATE——它要求 reaper 持 audit_log UPDATE 权限,
// 与 T7.3 CheckAuditAppendOnly 硬拒及 INSERT/SELECT-only 部署契约正面冲突):
//   - **禁 INSERT IGNORE**——IGNORE 把分区越界 1526/截断等错误静默降级为
//     warning,行无声丢失(T1.3 审计确认);裸 INSERT 让它们保持 ERROR(响亮)。
//   - 重放幂等 = 预过滤:搬运前查本批 (exec_date,id) 已在 audit_log 的行
//     (重放残行:audit 行已落、暂存 DELETE 未及的窗口产物)直接核销暂存,
//     不参与 INSERT。同事务 {INSERT, DELETE} 原子性使该窗口结构性不可达,
//     预过滤是防御臂(历史残行/未来非原子路径)。
//   - 并发重叠(leaderless 双 flusher 同批):后到方 INSERT 撞先到方未提交
//     行的 PK 锁 → 先到方提交后 1062 → 后到方整批回滚 + 响亮错误 → 下轮
//     预过滤看到先到方终行 → 收敛。错误是信号,不是损失。
//   - 对账:同事务内 COUNT 校验本批新行全部在场,缺失 = 真丢失,rollback +
//     响亮错误(affected-rows 作重放观测,见 FlushResult)。
//
// 队头饿死防护:批选取按游标翻页(WHERE id > afterID),隔离毒行也推进
// 游标——全毒批(Flushed=0)不再堵死其后健康行;FlushAll 以 Staged<batch
// (扫到队尾)为终止判据。毒行留驻 + 每 tick 响亮记日志,运维介入。
//
// 毒行隔离:ghost execution(暂存行的 execution 无对应行 = 结构不变量破坏,
// 正常路径不可达——execution 无删除面)不阻断同批健康行。exec_date 批量
// 解析(点查摊销)只在 flush 期发生——发射半边零 workflow_execution 触达
// (D-t26-F1 锁面结论延续)。另一类语句级毒行(分区缺失 1526)整批失败
// 响亮、下轮重试,不静默;可达性 = janitor 停摆 ≥ retention 且暂存滞留同存,
// 登记为已知边界(D-t61)。

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	"braid/internal/engine"
)

// flushColsPerRow 是 INSERT 每行占位符数。batch 上界由 prepared statement
// 占位符预算推出:MySQL 上限 65535,21×3100 = 65100 留余量(T6.2 加
// linked_trace_id 列后 20→21,3200 会超限;reaper Validate 同步收口;
// COUNT/DELETE 语句各 2/1 占位符每行,远低)。
const flushColsPerRow = 21

// FlushResult 是单批搬运的可观测快照(reaper 循环聚合计数/测试断言)。
type FlushResult struct {
	Staged   int    // 本批取到的暂存行数(含被隔离毒行与重放残行)
	Flushed  int    // 成功搬运行数(新行 INSERT + DELETE 落定)
	Inserted int64  // INSERT affected-rows(裸 INSERT 恒 = 新行数;留作对账旁证)
	Skipped  int    // ghost execution 隔离行数(留驻暂存)
	Replayed int    // 预过滤核销的重放残行(audit 行已在,仅 DELETE 暂存)
	LastID   uint64 // 本批最大暂存 id(含毒行;游标推进锚。空批 = afterID 原样)
}

// flushRow 是 audit_pending 读形状。event_at 经 CAST(AS CHAR) 直出字符串:
// 不经 driver time.Location 解析往返(D-t26-F1 同判;字符串回插由服务端解析,
// 毫秒精度原样)。cost_amount 走 NullString:DECIMAL 精度不经浮点。
type flushRow struct {
	id          uint64
	eventAt     string
	eventType   string
	executionID uint64
	nodeExecID  sql.NullInt64
	nodeID      sql.NullString
	attemptID   sql.NullInt64
	actorType   string
	actorID     string
	traceID     sql.NullString
	linkedTrace sql.NullString
	piiMasked   bool
	model       sql.NullString
	promptTok   sql.NullInt64
	complTok    sql.NullInt64
	totalTok    sql.NullInt64
	costAmount  sql.NullString
	costCurr    sql.NullString
	costEst     bool
	payload     []byte
}

const flushSelect = `
SELECT id, CAST(event_at AS CHAR), event_type, execution_id,
       node_execution_id, node_id, attempt_id, actor_type, actor_id, trace_id,
       linked_trace_id, pii_masked, model, prompt_tokens, completion_tokens,
       total_tokens, cost_amount, cost_currency, cost_estimated, payload
  FROM audit_pending`

// linked_trace_id 已随 T6.2 接线进搬运面(Enqueue/Append/flush 三处同步
// 加列,0004 建列防漏的兑现;D-2026-09-27-t62-otel-trace)。

// Flush 搬运 afterID 之后的一批暂存审计行到 audit_log(单批语义:LIMIT batch,
// 余量留驻,调用方 FlushAll 循环翻页至排空)。afterID=0 即从队头。步骤:
// [1] 无事务读批(WHERE id > afterID ORDER BY id = snowflake 时序翻页)
// → [2] DISTINCT execution 批量解析 exec_date(毒行隔离)
// → [2.5] 预过滤重放残行(audit 行已在 → 仅核销暂存)
// → [3] 事务内 {裸多行 INSERT, COUNT 对账, DELETE 暂存}。
func Flush(ctx context.Context, db *sql.DB, afterID uint64, batch int) (FlushResult, error) {
	if batch < 1 {
		return FlushResult{LastID: afterID}, fmt.Errorf("audit: flush batch %d < 1", batch)
	}
	res := FlushResult{LastID: afterID}

	// [1] 读批:普通一致读,无锁;与并发 flusher 的 DELETE 竞态下最坏丢行
	// (对方已搬),本批少搬、下轮补齐——搬运是收敛性作业。
	rows, err := db.QueryContext(ctx, flushSelect+` WHERE id > ? ORDER BY id LIMIT ?`, afterID, batch)
	if err != nil {
		return res, fmt.Errorf("audit: flush select: %w", err)
	}
	var batchRows []flushRow
	err = func() error {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r flushRow
			if err := rows.Scan(&r.id, &r.eventAt, &r.eventType, &r.executionID,
				&r.nodeExecID, &r.nodeID, &r.attemptID, &r.actorType, &r.actorID,
				&r.traceID, &r.linkedTrace, &r.piiMasked, &r.model, &r.promptTok, &r.complTok,
				&r.totalTok, &r.costAmount, &r.costCurr, &r.costEst, &r.payload); err != nil {
				return fmt.Errorf("audit: flush scan: %w", err)
			}
			batchRows = append(batchRows, r)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("audit: flush rows: %w", err)
		}
		return nil
	}()
	if err != nil {
		return res, err
	}
	res.Staged = len(batchRows)
	if len(batchRows) == 0 {
		return res, nil
	}
	res.LastID = batchRows[len(batchRows)-1].id

	// [2] 批量解析 exec_date:一次 IN 点查覆盖本批全部 DISTINCT execution
	// (发射半边点查出热事务的摊销兑现)。缺行 = 毒行,隔离留驻。
	execIDs := make([]uint64, 0, len(batchRows))
	seen := map[uint64]bool{}
	for _, r := range batchRows {
		if !seen[r.executionID] {
			seen[r.executionID] = true
			execIDs = append(execIDs, r.executionID)
		}
	}
	execDates, err := resolveExecDates(ctx, db, execIDs)
	if err != nil {
		return res, fmt.Errorf("audit: flush resolve exec_date: %w", err)
	}
	var missingIDs []uint64
	flushable := make([]flushRow, 0, len(batchRows))
	for _, r := range batchRows {
		if _, ok := execDates[r.executionID]; ok {
			flushable = append(flushable, r)
		} else {
			missingIDs = append(missingIDs, r.id)
		}
	}
	res.Skipped = len(missingIDs)
	if len(missingIDs) > 0 {
		// 每 tick 响亮(毒行不清则持续重记,有意为之:不可达面的结构性违约
		// 需要运维介入,静默 = 审计缺口不可见)。游标翻页保证重扫只耗扫描
		// 不堵健康行。
		slog.Error("audit.flush_missing_execution_quarantined",
			"execution_ids", missingExecIDs(batchRows, execDates),
			"staged_ids", missingIDs, "hint", "execution row vanished; invariant break requires manual surgery")
	}
	if len(flushable) == 0 {
		return res, nil
	}

	// [2.5] 预过滤重放残行:本批 (exec_date,id) 已在 audit_log 的行不参与
	// INSERT,仅核销暂存(重放幂等的裸 INSERT 形态;同事务 {INSERT,DELETE}
	// 下该窗口结构性不可达,此臂防历史/未来非原子路径)。
	fDateOrder, fIDsByDate := groupIDsByDate(flushable, execDates)
	present, err := selectPresentIDs(ctx, db, fDateOrder, fIDsByDate)
	if err != nil {
		return res, fmt.Errorf("audit: flush prefilter: %w", err)
	}
	replaySet := make(map[uint64]bool, len(present))
	for _, id := range present {
		replaySet[id] = true
	}
	pending := make([]flushRow, 0, len(flushable))
	for _, r := range flushable {
		if !replaySet[r.id] {
			pending = append(pending, r)
		}
	}
	res.Replayed = len(replaySet)
	if res.Replayed > 0 {
		slog.Info("audit.flush_replay_converged", "replayed", res.Replayed)
		if err := deleteStaged(ctx, db, present); err != nil {
			return res, fmt.Errorf("audit: flush replay cleanup: %w", err)
		}
	}
	if len(pending) == 0 {
		res.Flushed = 0
		return res, nil
	}

	// [3] 搬运事务:裸 INSERT → COUNT 对账 → DELETE,任一失败整体回滚
	// (回滚后下一轮安全:行未落 ⇒ 重试;并发撞车 1062 ⇒ 预过滤收敛)。
	err = engine.WithTx(ctx, db, func(tx engine.DBTX) error {
		var ph strings.Builder
		args := make([]any, 0, len(pending)*flushColsPerRow)
		for i, r := range pending {
			if i > 0 {
				ph.WriteByte(',')
			}
			ph.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
			args = append(args,
				r.id, execDates[r.executionID], r.eventAt, r.eventType, r.executionID,
				r.nodeExecID, r.nodeID, r.attemptID, r.actorType, r.actorID, r.traceID,
				r.linkedTrace, r.piiMasked, r.payload,
				r.model, r.promptTok, r.complTok, r.totalTok, r.costAmount, r.costCurr,
				r.costEst)
		}
		execRes, err := tx.ExecContext(ctx, `
INSERT INTO audit_log
  (id, exec_date, event_at, event_type, execution_id, node_execution_id,
   node_id, attempt_id, actor_type, actor_id, trace_id, linked_trace_id,
   pii_masked, payload,
   model, prompt_tokens, completion_tokens, total_tokens,
   cost_amount, cost_currency, cost_estimated)
VALUES `+ph.String(), args...)
		if err != nil {
			return fmt.Errorf("audit: flush insert %d rows: %w", len(pending), err)
		}
		affected, _ := execRes.RowsAffected() // 错误有意忽略:COUNT 对账是权威裁决
		res.Inserted = affected

		// COUNT 对账:本批新行必须全部在场;缺失 = 批量契约破坏,回滚响亮。
		// 形态 = 按 exec_date 分组的 (exec_date=? AND id IN (...)) OR 串——
		// 多对 row-constructor `(a,b) IN ((?,?),(?,?))` 在 MySQL 8.0.46
		// prepared statement 下实测返回 0 行(t13 复现;字面量形态正确),
		// 本对账臂首次上岗即抓住该静默错误,故钉死此形态并在此登记。
		pDateOrder, pIDsByDate := groupIDsByDate(pending, execDates)
		got, err := countPresent(ctx, tx, pDateOrder, pIDsByDate)
		if err != nil {
			return fmt.Errorf("audit: flush reconcile: %w", err)
		}
		if got != len(pending) {
			return fmt.Errorf("audit: flush reconcile: %d/%d rows present in audit_log (batch contract violated; rolled back)",
				got, len(pending))
		}

		if err := deleteStaged(ctx, tx, idsOf(pending)); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		res.Flushed = 0
		res.Inserted = 0
		return res, err
	}
	res.Flushed = len(pending)
	return res, nil
}

// FlushAllResult 是整轮搬运的可观测聚合(T6.4:reaper flush 观测面,
// D-t61 的 AuditFlushInterval 无上界观测字源)。
type FlushAllResult struct {
	Batches  int // 完成批数(不含失败批)
	Staged   int
	Flushed  int
	Inserted int64
	Skipped  int
	Replayed int
}

// FlushAll 循环搬运至排空或 roundCap 上限(单轮上限语义同 janitor 的
// outboxRoundCap:有界 tick,余量下轮)。游标在轮内推进(毒行也推进)——
// 全毒批不堵其后健康行;Staged < batch = 扫到队尾即终止。
func FlushAll(ctx context.Context, db *sql.DB, batch, roundCap int) (FlushAllResult, error) {
	var agg FlushAllResult
	if batch < 1 || roundCap < 1 {
		return agg, fmt.Errorf("audit: flush all batch %d / cap %d < 1", batch, roundCap)
	}
	total := 0
	var cursor uint64
	for total < roundCap {
		res, err := Flush(ctx, db, cursor, batch)
		if err != nil {
			return agg, err
		}
		total += res.Flushed
		agg.Batches++
		agg.Staged += res.Staged
		agg.Flushed += res.Flushed
		agg.Inserted += res.Inserted
		agg.Skipped += res.Skipped
		agg.Replayed += res.Replayed
		cursor = res.LastID
		if res.Staged < batch {
			return agg, nil // 队尾(空批或尾批不满)
		}
	}
	return agg, nil
}

// groupIDsByDate 把行按 exec_date 分组(对账/预过滤查询的 OR 串输入;
// dateOrder 保持首现序,日志与断言稳定)。
func groupIDsByDate(rows []flushRow, execDates map[uint64]string) (dateOrder []string, idsByDate map[string][]uint64) {
	idsByDate = map[string][]uint64{}
	for _, r := range rows {
		d := execDates[r.executionID]
		if _, ok := idsByDate[d]; !ok {
			dateOrder = append(dateOrder, d)
		}
		idsByDate[d] = append(idsByDate[d], r.id)
	}
	return dateOrder, idsByDate
}

// selectPresentIDs 返回 idsByDate 中已在 audit_log 在场的 id 集(预过滤
// 臂;事务外一致读——audit 行 append-only 不可变,读后新出现的行只会走
// INSERT 撞车 1062 路径)。单点可测:OR 串漏日期臂在此红。
func selectPresentIDs(ctx context.Context, db *sql.DB, dateOrder []string, idsByDate map[string][]uint64) ([]uint64, error) {
	if len(dateOrder) == 0 {
		return nil, nil
	}
	var where strings.Builder
	total := 0
	for _, ids := range idsByDate {
		total += len(ids)
	}
	args := make([]any, 0, total+len(dateOrder))
	for di, d := range dateOrder {
		if di > 0 {
			where.WriteString(" OR ")
		}
		where.WriteString("(exec_date=? AND id IN (")
		args = append(args, d)
		for i, id := range idsByDate[d] {
			if i > 0 {
				where.WriteByte(',')
			}
			where.WriteByte('?')
			args = append(args, id)
		}
		where.WriteString("))")
	}
	rows, err := db.QueryContext(ctx, `SELECT id FROM audit_log WHERE `+where.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return out, nil
}

// countPresent 是对账臂:按日期组 COUNT 求和(DBTX 兼容——组内 id 唯一,
// 和即精确在场数;单点可测:漏组/漏臂在和上红)。
func countPresent(ctx context.Context, q engine.DBTX, dateOrder []string, idsByDate map[string][]uint64) (int, error) {
	total := 0
	for _, d := range dateOrder {
		var ph strings.Builder
		args := make([]any, 0, len(idsByDate[d])+1)
		args = append(args, d)
		for i, id := range idsByDate[d] {
			if i > 0 {
				ph.WriteByte(',')
			}
			ph.WriteByte('?')
			args = append(args, id)
		}
		var n int
		if err := q.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM audit_log WHERE exec_date=? AND id IN (`+ph.String()+`)`,
			args...).Scan(&n); err != nil {
			return 0, fmt.Errorf("count %s: %w", d, err)
		}
		total += n
	}
	return total, nil
}

// deleteStaged 核销暂存行(搬运与重放清理共用)。
func deleteStaged(ctx context.Context, q engine.DBTX, ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}
	var ph strings.Builder
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			ph.WriteByte(',')
		}
		ph.WriteByte('?')
		args = append(args, id)
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM audit_pending WHERE id IN (`+ph.String()+`)`, args...); err != nil {
		return fmt.Errorf("audit: flush delete staged: %w", err)
	}
	return nil
}

func idsOf(rows []flushRow) []uint64 {
	out := make([]uint64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.id)
	}
	return out
}

// resolveExecDates 批量物化分区键(镜像 Append 的 DATE_FORMAT 表达式,
// created_at 不可变 ⇒ 两处恒同值)。缺行(理论不可达:execution 无删除面)
// 不进返回 map,由调用方隔离。
func resolveExecDates(ctx context.Context, db *sql.DB, execIDs []uint64) (map[uint64]string, error) {
	var ph strings.Builder
	args := make([]any, 0, len(execIDs))
	for i, id := range execIDs {
		if i > 0 {
			ph.WriteByte(',')
		}
		ph.WriteByte('?')
		args = append(args, id)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, DATE_FORMAT(created_at, '%Y-%m-%d') FROM workflow_execution WHERE id IN (`+ph.String()+`)`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[uint64]string, len(execIDs))
	for rows.Next() {
		var id uint64
		var date string
		if err := rows.Scan(&id, &date); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out[id] = date
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return out, nil
}

// missingExecIDs 收集隔离行的 execution 去重清单(日志字段)。
func missingExecIDs(rows []flushRow, execDates map[uint64]string) []uint64 {
	seen := map[uint64]bool{}
	var out []uint64
	for _, r := range rows {
		if _, ok := execDates[r.executionID]; !ok && !seen[r.executionID] {
			seen[r.executionID] = true
			out = append(out, r.executionID)
		}
	}
	return out
}
