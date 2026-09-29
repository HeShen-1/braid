package audit

// T9.3 链校验引擎(D-2026-09-29-t93 #5,rev2 对抗审计后重写):**全链
// linkage 复走**——按 prev_hash 链接序从链首('')装配整链再逐行重算,
// 不按 id 序(链序 ≠ id 序:snowflake 同毫秒跨进程倒置是常态,snowflake.go
// 免责条款钦定;rev1 的 id 序走链对诚实倒置误闩,对抗审查 HIGH 实证)。
// 每轮全链复走 ⇒ 历史行篡改不因「已验过」漏检(无 id 水位盲区);轮换
// 制控成本(verifyRerotateInterval 到期或有新活动才复走)。
//
// 校验不取行锁(普通一致读):校验与写路径不争锁,leaderless 重叠幂等
// (verified_* 重写同值,断链闩首见落定 WHERE broken_at IS NULL)。
//
// 断链判据五族:
//  ① 行内容篡改:重算 row_hash ≠ 存储 row_hash;
//  ② 链接断/删行:装配路径在中途失去后继,或某行 prev 指向不存在的前驱
//     (不可达 hash ⇒ 该行成为未装配孤儿);
//  ③ 叉链:同一 prev_hash 有多行(双写竞态产物或篡改注入);
//  ④ 孤儿行:全链装配后仍有未消费行(重复链首/悬空行/伪造行);
//  ⑤ 分区挪移:exec_date re-derivation——行的 exec_date ≠ DATE(wf.
//     created_at)(hash 不含 exec_date,该面由重导比对兜住)。
// 链尾可达性:装配终点 row_hash 必须 == head.last_row_hash;不等时单语句
// UNION 点查 head 指认行(单快照,flush 搬运期恒在两表之一)——在场且
// 吻合 = 写入竞态(绿,下轮复走),缺场或失配 = 尾行丢失/篡改(红)。
//
// 已知边界(登记 D-t93):无密钥链对「全链重写」不可检测(head 锚外发归
// 后续);时间戳列(event_at/created_at)不入链;混合版本窗旧行按
// row_hash NULL 跳过;巨链执行(> chainFetchCap)跳过并响亮,不闩。

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// verifyRerotateInterval 是已验链的复走周期:到期即整链重验(历史行篡改
// 检测面);有新活动(last_id > verified_last_id)不受此限即时触发。
// SQL 谓词由它单源推导(分钟粒度)。
var verifyRerotateMinutes = int((10 * time.Minute).Minutes())

// VerifyOutcome 是单执行单轮校验的结果快照(reaper 观测/测试断言面)。
type VerifyOutcome struct {
	ExecutionID uint64
	Checked     int    // 本轮装配并重算的行数
	Verified    bool   // 全绿(链尾可达或写入竞态下的部分绿)
	Broken      bool   // 断链(含此前已闩)
	NewlyBroken bool   // 本轮落闩(首见;已闩重复校验为 false——计数不重复)
	Skipped     bool   // 巨链跳过(规模事件,非完整性事件;不炸作业不闩)
	Reason      string // 断链归因(首见;已闩时回读存储值)
}

// VerifyChain 全链校验单执行链并执法(落闩/记轮换)。infra 错误(DB 不可
// 达等)以 error 返回且**不落闩**——瞬态故障 ≠ 证据完整性违约。
func VerifyChain(ctx context.Context, db *sql.DB, execID uint64) (VerifyOutcome, error) {
	out := VerifyOutcome{ExecutionID: execID}

	// head 快照(普通读):闩检查 + 链尾指认。
	var brokenAt sql.NullTime
	var brokenReason sql.NullString
	var lastID uint64
	var lastRowHash string
	err := db.QueryRowContext(ctx, `
SELECT last_id, last_row_hash, broken_at, broken_reason
  FROM chain_heads WHERE execution_id=?`, execID).
		Scan(&lastID, &lastRowHash, &brokenAt, &brokenReason)
	if err == sql.ErrNoRows {
		// 无头 = 该执行从未启链(存量执行):无链可校,不是断链。
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("audit: verify head (exec %d): %w", execID, err)
	}
	if brokenAt.Valid {
		out.Broken = true
		out.Reason = brokenReason.String
		return out, nil // 已闩:保持断链,重验归复位后
	}

	// exec_date 重导(单点物化;wf 缺行 = 结构不变量破坏)
	var execDate string
	err = db.QueryRowContext(ctx,
		`SELECT DATE_FORMAT(created_at, '%Y-%m-%d') FROM workflow_execution WHERE id=?`,
		execID).Scan(&execDate)
	if err == sql.ErrNoRows {
		return out, out.breakChain(ctx, db, execID, "execution row vanished (head without execution)")
	}
	if err != nil {
		return out, fmt.Errorf("audit: verify exec_date (exec %d): %w", execID, err)
	}

	// ⑤ 分区挪移:本执行任意行 exec_date ≠ 重导值即违约。
	var moved int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND exec_date<>?`,
		execID, execDate).Scan(&moved); err != nil {
		return out, fmt.Errorf("audit: verify partition probe (exec %d): %w", execID, err)
	}
	if moved > 0 {
		return out, out.breakChain(ctx, db, execID,
			fmt.Sprintf("%d audit rows outside derived exec_date %s", moved, execDate))
	}

	// 全链单快照读取(双表 UNION ALL;见 chain.go readWholeChain)。
	links, err := readWholeChain(ctx, db, execDate, execID)
	if err != nil {
		return out, err
	}
	if len(links) > chainFetchCap {
		// 规模事件 ≠ 完整性事件:跳过该执行但不报错——报错会炸掉整个
		// verify 作业,一条巨链把所有其他头的水位卡死每 tick(对抗审计
		// MEDIUM 修)。Skipped 由 reaper 记 warn,下轮再试;登记 D-t93 已知边界。
		out.Skipped = true
		out.Reason = fmt.Sprintf("chain exceeds fetch cap %d rows; skipped this pass", chainFetchCap)
		return out, nil
	}
	out.Checked = len(links)

	// ①-④ linkage 装配:prev → 行(多行同 prev = 叉链);从链首 '' 顺指针
	// 走,逐行重算比对;终点必须吃掉全部行(孤儿 = 红)。
	byPrev := make(map[string][]*chainLink, len(links))
	for i := range links {
		l := &links[i]
		byPrev[l.storedPrev] = append(byPrev[l.storedPrev], l)
	}
	consumed := make(map[*chainLink]bool, len(links))
	cur := "" // 链首 prev = ''
	var terminal *chainLink
	for {
		cands := byPrev[cur]
		if len(cands) == 0 {
			break // 路径尽头
		}
		if len(cands) > 1 {
			ids := make([]uint64, 0, len(cands))
			for _, c := range cands {
				ids = append(ids, c.id)
			}
			// id 清单截断(broken_reason VARCHAR(255):~10 个 19 位 id 即超长,
			// 1406 会让闩落不了地 = 告警面被压制——对抗审计 MEDIUM 修;
			// orphan 分支同形)。
			ids = firstIDs(ids, 4)
			return out, out.breakChain(ctx, db, execID, fmt.Sprintf(
				"chain fork: %d rows share prev %.8s… (first ids %v)", len(cands), cur, ids))
		}
		l := cands[0]
		recomputed, err := l.row.rowHash(cur)
		if err != nil {
			return out, fmt.Errorf("audit: verify recompute (row %d): %w", l.id, err)
		}
		if l.storedRow != recomputed {
			return out, out.breakChain(ctx, db, execID, fmt.Sprintf(
				"row %d hash mismatch: stored %.8s… recomputed %.8s… (content tampered)",
				l.id, l.storedRow, recomputed))
		}
		consumed[l] = true
		terminal = l
		cur = recomputed
	}
	var orphans []uint64
	for i := range links {
		l := &links[i]
		if !consumed[l] {
			orphans = append(orphans, l.id)
		}
	}
	if len(orphans) > 0 {
		return out, out.breakChain(ctx, db, execID, fmt.Sprintf(
			"%d orphan rows outside the linked chain (first ids %v): dangling prev / forged / duplicate genesis",
			len(orphans), firstIDs(orphans, 4)))
	}

	// 链尾可达性:head 重读(校验期间写者可能推进)。装配终点 ≠ head 时,
	// 单语句 UNION 点查判别 head 指认行的在场性与 hash 吻合——**单快照**
	// 是关键:flush 搬运在任意单快照里行恒在两表之一(未提交搬运行对快照
	// 不可见,原行可见),双探针会把「提交于两探之间」误判双缺(rev1
	// tailPresent 的对抗审查 HIGH 教训不重演)。在场且吻合 = 写入竞态
	//(head 行在快照外,已装配段全绿,下轮复走覆盖);缺场或失配 =
	// 尾丢失/篡改(红)。
	var headNowID uint64
	var headNowHash string
	if err := db.QueryRowContext(ctx,
		`SELECT last_id, last_row_hash FROM chain_heads WHERE execution_id=?`, execID).
		Scan(&headNowID, &headNowHash); err != nil {
		return out, fmt.Errorf("audit: verify head re-read (exec %d): %w", execID, err)
	}
	if terminal != nil && terminal.storedRow == headNowHash {
		out.Verified = true
	} else {
		tailOK, err := tailMatches(ctx, db, execDate, headNowID, headNowHash)
		if err != nil {
			return out, err
		}
		if tailOK {
			out.Verified = true // 写入竞态:head 行在快照外,部分覆盖
		} else {
			return out, out.breakChain(ctx, db, execID, fmt.Sprintf(
				"chain tail row %d vanished or hash mismatch (head %.8s…): not linked and not present in audit_log ∪ audit_pending",
				headNowID, headNowHash))
		}
	}

	// 轮换记录(verified_last_id = 复走时的 head 快照;id 比较仅作下轮
	// 「有新活动」触发启发式,非正确性锚)。
	if _, err := db.ExecContext(ctx,
		`UPDATE chain_heads SET verified_last_id=?, verified_at=NOW(3) WHERE execution_id=?`,
		headNowID, execID); err != nil {
		return out, fmt.Errorf("audit: verify rotation stamp (exec %d): %w", execID, err)
	}
	return out, nil
}

// firstIDs 取前 n 个(id 升序输入时即最小 n 个;诊断字段)。
func firstIDs(ids []uint64, n int) []uint64 {
	if len(ids) > n {
		return ids[:n]
	}
	return ids
}

// tailMatches 单语句 UNION 点查链尾行在场性与 hash 吻合(单快照语义见
// VerifyChain 链尾可达性注释)。存在且 row_hash == wantHash → true。
func tailMatches(ctx context.Context, db *sql.DB, execDate string, id uint64, wantHash string) (bool, error) {
	var got sql.NullString
	err := db.QueryRowContext(ctx, `
SELECT row_hash FROM (
  SELECT row_hash FROM audit_log WHERE exec_date=? AND id=?
  UNION ALL
  SELECT row_hash FROM audit_pending WHERE id=?
) t LIMIT 1`, execDate, id, id).Scan(&got)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("audit: tail probe (id %d): %w", id, err)
	}
	return got.Valid && got.String == wantHash, nil
}

// breakChain 落闩(首见落定,不覆盖既有归因)并回填 outcome;RowsAffected
// 区分首见(计数面)与已闩(幂等重验)。
func (o *VerifyOutcome) breakChain(ctx context.Context, db *sql.DB, execID uint64, reason string) error {
	res, err := db.ExecContext(ctx, `
UPDATE chain_heads SET broken_at=NOW(3), broken_reason=?
 WHERE execution_id=? AND broken_at IS NULL`, reason, execID)
	if err != nil {
		return fmt.Errorf("audit: break chain (exec %d): %w", execID, err)
	}
	n, _ := res.RowsAffected()
	o.Broken = true
	o.NewlyBroken = n > 0
	o.Reason = reason
	return nil
}

// VerifyDueHeads 列出待校验执行:未闩且(从未验过 / 轮换到期 / 有新活动)。
// last_id > verified_last_id 是**触发启发式**而非正确性判断——同毫秒倒置
// 理论上可让新行 id ≤ 旧尾 id,但新活动的行流不可能整段落在历史 id 域
// (snowflake 时间位占高位),触发面完备。按 execution_id 升序(reaper
// 周期作业的分窗输入;升序 = 多 head 操作的钉死锁序,D-t93 #2)。
func VerifyDueHeads(ctx context.Context, db *sql.DB, limit int) ([]uint64, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
SELECT execution_id FROM chain_heads
 WHERE broken_at IS NULL
   AND (verified_at IS NULL
        OR verified_at < NOW(3) - INTERVAL %d MINUTE
        OR last_id > verified_last_id)
 ORDER BY execution_id LIMIT ?`, verifyRerotateMinutes), limit)
	if err != nil {
		return nil, fmt.Errorf("audit: verify due heads: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("audit: scan due head: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: due heads iterate: %w", err)
	}
	return out, nil
}
