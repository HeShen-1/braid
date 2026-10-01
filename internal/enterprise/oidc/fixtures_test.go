package oidc

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"braid/internal/auth"
	"braid/internal/db/sqltest"
)

// entSchema:T16.1 切出后本包自开测试 base(auth fixtures 同款纪律),
// 与共享的 braid_auth_test 隔离;schema 名经 TestSchema 自动拼 worktree
// 后缀(T2.7),栈不可达自动 skip(CI 无栈只跑单元面)。
const entSchema = "braid_ent_oidc_test"

// newDB 返回迁移就绪的连接池,t.Cleanup 先根后清空本包面(auth fixtures
// 同款纪律:禁 TRUNCATE,DELETE 按 FK 逆序)。
func newDB(t *testing.T) *sql.DB {
	t.Helper()
	d := sqltest.MustOpenAt(t, entSchema)
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM user_role`)
		_, _ = d.Exec(`DELETE FROM users`)
	})
	return d
}

// seqIDs 计数器 IDGen 桩:base 取当前纳秒(跨运行唯一),步进 +1。
type seqIDs struct {
	base uint64
	cur  atomic.Uint64
}

func (s *seqIDs) Next() (uint64, error) { return s.base + s.cur.Add(1), nil }

func newIDs() *seqIDs { return &seqIDs{base: uint64(time.Now().UnixNano())} }

// createUser 是集成测试的种子捷径(auth fixtures 同形,经导出面装配:
// HashPassword/WithTx/CreateUser 全导出,零核心面开挖)。
func createUser(t *testing.T, d *sql.DB, username, password string, roles ...auth.Role) {
	t.Helper()
	hash, err := auth.HashPassword(password, 4) // bcrypt.MinCost:测试速度
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := auth.WithTx(context.Background(), d, func(q auth.DBTX) error {
		return auth.CreateUser(context.Background(), q, newIDs(), auth.CreateUserParams{
			Username: username, PasswordHash: hash, Roles: roles,
		})
	}); err != nil {
		t.Fatalf("create user %q: %v", username, err)
	}
}
