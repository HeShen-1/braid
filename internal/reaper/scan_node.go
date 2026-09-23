package reaper

import (
	"context"
	"fmt"
	"time"
)

// orphanQuiet 是扫描③的执行静默窗(常量,不设 env):避开 Complete 提交与
// 提交后 finishIfDone 的在飞窗口,防误收口刚终态执行的残留 EXECUTING。
const orphanQuiet = 2 * time.Minute

// scanNodes 扫描②(deadline)+ ③(孤儿),共用 reapAttempt 处置事务:
//
//	② EXECUTING 且 deadline 过期(skew margin = SCAN_INTERVAL,兑现 nodecas.go
//	  「deadline 是唯一应用侧时间,偏差由 reconciler 阈值×N 吸收」契约;NULL
//	  deadline 谓词恒假,branch 内联天然排除;WAITING_APPROVAL 半边归 T4.3)
//	  → 分支 B 补洞:EXECUTING+无活 attempt 的卡死执行在此收敛(攻击 6/10)。
//	③ EXECUTING 且 wf 已终态(fail-fast 遗留,扫描靶 a;node 侧驱动,成本
//	  上界 = EXECUTING 行数,孤儿任意年龄可检出——攻击 14 吸收)。
func (r *Reaper) scanNodes(ctx context.Context) error {
	if err := r.scanNodeDeadline(ctx); err != nil {
		return err
	}
	return r.scanNodeOrphans(ctx)
}

func (r *Reaper) scanNodeDeadline(ctx context.Context) error {
	var lastID uint64
	for batch := 0; batch < scanMaxBatch; batch++ {
		rows, err := r.deps.DB.QueryContext(ctx, `
SELECT id, execution_id
  FROM node_execution
 WHERE status='EXECUTING' AND deadline_at < NOW(3) - INTERVAL ? MICROSECOND
   AND id > ?
 ORDER BY id
 LIMIT ?`, r.cfg.ScanInterval.Microseconds(), lastID, scanBatchSize)
		if err != nil {
			return fmt.Errorf("reaper: scan node deadline: %w", err)
		}
		cands, err := collectNodeCands(rows, &lastID)
		if err != nil {
			return err
		}
		if len(cands) == 0 {
			return nil
		}
		for _, c := range cands {
			if err := r.reapAttempt(ctx, c.id, c.execID, entryDeadline); err != nil {
				r.log.Error("reap.node_failed",
					"node_execution_id", c.id, "err", err)
			}
		}
		if len(cands) < scanBatchSize {
			return nil
		}
	}
	r.log.Warn("scan.backlog", "scanner", "node_deadline",
		"hint", "candidates exceeded per-tick cap; retrying next tick")
	return nil
}

func (r *Reaper) scanNodeOrphans(ctx context.Context) error {
	var lastID uint64
	for batch := 0; batch < scanMaxBatch; batch++ {
		rows, err := r.deps.DB.QueryContext(ctx, `
SELECT n.id, n.execution_id
  FROM node_execution n
  JOIN workflow_execution w ON w.id = n.execution_id
 WHERE n.status = 'EXECUTING'
   AND w.status IN ('SUCCEEDED','FAILED','CANCELLED')
   AND w.last_activity_at < NOW(3) - INTERVAL ? MICROSECOND
   AND n.id > ?
 ORDER BY n.id
 LIMIT ?`, orphanQuiet.Microseconds(), lastID, scanBatchSize)
		if err != nil {
			return fmt.Errorf("reaper: scan node orphans: %w", err)
		}
		cands, err := collectNodeCands(rows, &lastID)
		if err != nil {
			return err
		}
		if len(cands) == 0 {
			return nil
		}
		for _, c := range cands {
			if err := r.reapAttempt(ctx, c.id, c.execID, entryOrphan); err != nil {
				r.log.Error("reap.orphan_failed",
					"node_execution_id", c.id, "err", err)
			}
		}
		if len(cands) < scanBatchSize {
			return nil
		}
	}
	r.log.Warn("scan.backlog", "scanner", "node_orphans",
		"hint", "candidates exceeded per-tick cap; retrying next tick")
	return nil
}

// nodeCand / collectNodeCands 是②③共用的候选收集(游标推进 + 关闭)。
type nodeCand struct {
	id     uint64
	execID uint64
}

func collectNodeCands(rows nodeRows, lastID *uint64) ([]nodeCand, error) {
	defer func() { _ = rows.Close() }()
	var cands []nodeCand
	for rows.Next() {
		var c nodeCand
		if err := rows.Scan(&c.id, &c.execID); err != nil {
			return nil, fmt.Errorf("reaper: scan nodes row: %w", err)
		}
		*lastID = c.id
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reaper: scan nodes rows: %w", err)
	}
	return cands, nil
}

// nodeRows 收窄 *sql.Rows(测试换桩面;同 redistest.TB 思路)。
type nodeRows interface {
	Next() bool
	Scan(dest ...any) error
	Close() error
	Err() error
}
