package auth

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"braid/internal/db/sqltest"
)

// authSchema:本包自开测试 base(relay 先例),与共享的 braid_engine_test
// 隔离;schema 名经 TestSchema 自动拼 worktree 后缀(T2.7),栈不可达
// 自动 skip(CI 无栈只跑单元面)。
const authSchema = "braid_auth_test"

// newDB 返回迁移就绪的连接池,t.Cleanup 叶先根后清空本包表(engine
// fixtures 同款纪律:禁 TRUNCATE,DELETE 按 FK 逆序)。
func newDB(t *testing.T) *sql.DB {
	t.Helper()
	d := sqltest.MustOpenAt(t, authSchema)
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM user_role`)
		_, _ = d.Exec(`DELETE FROM users`)
	})
	return d
}

// seqIDs 计数器 IDGen 桩:base 取当前纳秒(跨运行唯一),步进 +1;
// 唯一性只需在测试 schema 内成立(engine fixtures newID 同思路)。
type seqIDs struct {
	base uint64
	cur  atomic.Uint64
}

func (s *seqIDs) Next() (uint64, error) { return s.base + s.cur.Add(1), nil }

func newIDs() *seqIDs { return &seqIDs{base: uint64(time.Now().UnixNano())} }

// createUser 是集成测试的种子捷径:MinCost 哈希(速度)+ WithTx 包裹
// (原子性示范,即生产调用形态)。
func createUser(t *testing.T, d *sql.DB, username, password string, roles ...Role) {
	t.Helper()
	hash, err := HashPassword(password, 4) // bcrypt.MinCost:测试速度,生严成本见 config_test
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := WithTx(context.Background(), d, func(q DBTX) error {
		return CreateUser(context.Background(), q, newIDs(), CreateUserParams{
			Username: username, PasswordHash: hash, Roles: roles,
		})
	}); err != nil {
		t.Fatalf("create user %q: %v", username, err)
	}
}

// 测试密钥纪律(gitleaks 实测):低熵词组占位,≥32 字节;token 一律
// 测试内签发,不硬编码 eyJ 串。
const (
	testSecret = "unit-test-jwt-signing-secret-words-only-placeholder"
	altSecret  = "another-jwt-secret-placeholder-made-of-plain-words"
)
