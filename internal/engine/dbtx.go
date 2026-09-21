package engine

import (
	"context"
	"database/sql"
	"fmt"
)

// DBTX 是 CAS helper 的事务组合面。*sql.DB、*sql.Tx、*sql.Conn 均满足。
//
// 最小两方法是有意的:helper 永不自管事务边界(不 Begin/不 Commit),事务时长与
// 锁持有窗口归调用方。T2.2 判别性故障测试的形状因此成立:
//
//	WithTx(ctx, db, func(q engine.DBTX) error {
//	    _, err := engine.ClaimNode(ctx, q, …)  // 状态迁移
//	    err  = engine.TouchExecution(ctx, q, …) // 活动时刻同事务
//	    err  = outbox.Append(ctx, q, evt)        // T2.2;panic/断连注入点 → 整体回滚
//	    return err
//	})
//
// 未来需要 Prepare/批量接口时,以新小接口嵌入扩展,不动本签名(锁死防漂移)。
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// WithTx 在单事务内执行 fn,Commit/Rollback 只在此一处集中。
// fn 返回错误即整体回滚;panic 也会回滚(defer 路径),这是 T2.2 判别性故障
// 测试「状态写入后、outbox 写入前注入失败 → 双写同亡」的依赖。
func WithTx(ctx context.Context, db *sql.DB, fn func(DBTX) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("engine: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() // Commit 后/已回滚返回 ErrTxDone,忽略
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("engine: commit: %w", err)
	}
	committed = true
	return nil
}
