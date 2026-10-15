package sqltest

// 真 MySQL 交错测试只在驱动边界暂停，SQL、事务和行锁仍由 InnoDB 执行。
// 避免向生产 scheduler/worker 注入测试钩子。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"

	"github.com/go-sql-driver/mysql"

	"braid/internal/db"
)

// Intercepts 在事务开始前或结果集关闭后设置屏障；nil 表示原样透传。
type Intercepts struct {
	BeforeBegin func(context.Context) error
	AfterRows   func(context.Context, string, []driver.NamedValue) error
}

// OpenIntercepted 连接已由 MustOpenAt 初始化的同一测试 schema。
func OpenIntercepted(t TB, original *sql.DB, hooks Intercepts) *sql.DB {
	t.Helper()
	var schema string
	if err := original.QueryRow("SELECT DATABASE()").Scan(&schema); err != nil {
		t.Fatalf("read test schema: %v", err)
	}
	cfg, err := mysql.ParseDSN(db.CanonicalDSN(db.TestServerDSN() + schema))
	if err != nil {
		t.Fatalf("parse intercepted DSN: %v", err)
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatalf("intercepted connector: %v", err)
	}
	d := sql.OpenDB(&interceptConnector{Connector: connector, hooks: hooks})
	t.Cleanup(func() { _ = d.Close() })
	return d
}

type interceptConnector struct {
	driver.Connector
	hooks Intercepts
}

func (c *interceptConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &interceptConn{Conn: conn, hooks: c.hooks}, nil
}

type interceptConn struct {
	driver.Conn
	hooks Intercepts
}

func (c *interceptConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.hooks.BeforeBegin != nil {
		if err := c.hooks.BeforeBegin(ctx); err != nil {
			return nil, err
		}
	}
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c *interceptConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *interceptConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	var stmt driver.Stmt
	if errors.Is(err, driver.ErrSkip) && c.hooks.AfterRows != nil {
		// 保留生产默认的预备语句路径，同时包住结果集，避免屏障被回落绕开。
		stmt, err = c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
		if err != nil {
			return nil, err
		}
		rows, err = stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
		if err != nil {
			_ = stmt.Close()
			return nil, err
		}
	}
	if err != nil || c.hooks.AfterRows == nil {
		return rows, err
	}
	return &interceptRows{Rows: rows, stmt: stmt, after: func() error { return c.hooks.AfterRows(ctx, query, args) }}, nil
}

type interceptRows struct {
	driver.Rows
	stmt  driver.Stmt
	after func() error
}

func (r *interceptRows) Close() error {
	if err := r.Rows.Close(); err != nil {
		if r.stmt != nil {
			_ = r.stmt.Close()
		}
		return err
	}
	if r.stmt != nil {
		if err := r.stmt.Close(); err != nil {
			return err
		}
		r.stmt = nil
	}
	if r.after == nil {
		return nil
	}
	after := r.after
	r.after = nil
	return after()
}
