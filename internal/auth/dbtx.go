package auth

import (
	"context"
	"database/sql"
	"fmt"
)

// DBTX 是 auth 持久化原语的最小依赖面(ExecContext + QueryRowContext +
// QueryContext),与 engine.DBTX 同形但独立定义:auth 不 import engine
// (包分层纪律,engine 是单 worktree 持有的受保护包)。roles 经
// JSON_ARRAYAGG 单行聚合读取;多行扫描仅 EmailsByUsernames(T12.2 扇出
// 解析);*sql.DB / *sql.Tx / *sql.Conn 均满足。
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// WithTx 在单事务内执行 fn:Commit/Rollback 只此一处,fn 出错与 panic
// 均回滚(engine.WithTx 镜像,仓内事务纪律);原语永不自管事务边界。
// CreateUser 的「user 行 + N 条角色行」原子性由调用方经本函数包裹。
func WithTx(ctx context.Context, db *sql.DB, fn func(DBTX) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("auth: begin tx: %w", err)
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
		return fmt.Errorf("auth: commit tx: %w", err)
	}
	committed = true
	return nil
}

// IDGen 是新行 Snowflake 主键的最小生成面(*snowflake.Generator 结构性
// 满足);auth 不 import snowflake,测试用计数器换桩。CreateUser 内部
// 经它生成 user 行与全部角色行的 id——角色行 id 是存储内部细节,经
// 参数外泄会迫使调用方理解并生成它们。
type IDGen interface {
	Next() (uint64, error)
}
