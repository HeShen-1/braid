package api

import (
	"context"
	"database/sql"
	"fmt"
)

// DBTX 是 api 持久化原语的最小依赖面,与 auth.DBTX 同形但独立定义
// (仓内事务纪律:各包不互相 import 对方的 tx 面)。*sql.DB / *sql.Tx /
// *sql.Conn 均满足;QueryContext 供 List 多行扫描。
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// WithTx 在单事务内执行 fn:Commit/Rollback 只此一处,fn 出错与 panic 均
// 回滚(auth.WithTx / engine.WithTx 镜像,仓内事务纪律);原语永不自管
// 事务边界。Publish 的「读版本号 + 状态翻转」原子性由调用方经本函数包裹。
func WithTx(ctx context.Context, db *sql.DB, fn func(DBTX) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("api: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() // fn 失败/panic/Commit 失败路径;ErrTxDone 时无害
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("api: commit tx: %w", err)
	}
	committed = true
	return nil
}
