package cli

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"braid/internal/db"
	"braid/internal/db/sqltest"
)

// cliSchema:本包自开测试 base(auth 先例),栈不可达自动 skip
// (CI REQUIRE_STACK=1 下红)。清理纪律 = DELETE 按 FK 逆序 + 本包
// 触的租约行(CLI 建号 acquire/release;残留过期行对 auto 池无害,
// 清掉是防树跨运行无限增长)。
const cliSchema = "braid_cli_test"

// newDBEnv 返回注入迁移就绪 DSN 的 env 桩(kv 优先;BRAID_MYSQL_DSN
// 默认指向本包 schema)。
func newDBEnv(t *testing.T, kv map[string]string) func(string) string {
	t.Helper()
	dbh := sqltest.MustOpenAt(t, cliSchema)
	t.Cleanup(func() {
		_, _ = dbh.Exec(`DELETE FROM user_role`)
		_, _ = dbh.Exec(`DELETE FROM users`)
		_, _ = dbh.Exec(`DELETE FROM snowflake_node_lease`)
	})
	dsn := db.TestServerDSN() + db.TestSchema(cliSchema)
	return func(k string) string {
		if v, ok := kv[k]; ok {
			return v
		}
		if k == "BRAID_MYSQL_DSN" {
			return dsn
		}
		return ""
	}
}

// uniqName 生成跨运行唯一的合法 username(域 ^[a-z][a-z0-9_-]{0,127}$,
// 无点号:HHMMSS + 纳秒全数字拼接)。
func uniqName(prefix string) string {
	return prefix + "u" + time.Now().UTC().Format("150405000000000")
}

// TestUserCreateIntegration 真库集成:happy path 落 users+user_role 行、
// 哈希 bcrypt 形态、租约用后即删;重复用户名走 ErrConflict 面。
func TestUserCreateIntegration(t *testing.T) {
	env := newDBEnv(t, map[string]string{"BRAID_CLI_PASSWORD": "correct horse battery"})
	name := uniqName("alice")
	var out, errb bytes.Buffer
	code := userCreate(context.Background(), env, userCreateParams{
		username: name,
		rolesRaw: "author,approver",
	}, &out, &errb)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %q", code, errb.String())
	}
	if !strings.Contains(out.String(), name) {
		t.Errorf("stdout missing username: %q", out.String())
	}

	// 直接核库:行落位 + 哈希形态 + 角色两行 + 租约表清空(Release 删行)。
	dbh := sqltest.MustOpenAt(t, cliSchema)
	var hash, email sql.NullString
	var tenant uint64
	if err := dbh.QueryRow(`SELECT password_hash, email, tenant_id FROM users WHERE username = ?`, name).
		Scan(&hash, &email, &tenant); err != nil {
		t.Fatalf("users row: %v", err)
	}
	if !hash.Valid || len(hash.String) != 60 || !strings.HasPrefix(hash.String, "$2") {
		t.Errorf("password_hash not bcrypt-shaped: %+v", hash)
	}
	if email.Valid {
		t.Errorf("email = %q, want NULL", email.String)
	}
	if tenant != 0 {
		t.Errorf("tenant = %d, want 0", tenant)
	}
	var roles int
	if err := dbh.QueryRow(`SELECT COUNT(*) FROM user_role ur JOIN users u ON u.id = ur.user_id WHERE u.username = ?`, name).Scan(&roles); err != nil {
		t.Fatalf("user_role rows: %v", err)
	}
	if roles != 2 {
		t.Errorf("role rows = %d, want 2", roles)
	}
	var leases int
	if err := dbh.QueryRow(`SELECT COUNT(*) FROM snowflake_node_lease`).Scan(&leases); err != nil {
		t.Fatalf("lease rows: %v", err)
	}
	if leases != 0 {
		t.Errorf("lease rows = %d after run, want 0 (must release)", leases)
	}

	// 重复用户名 → 1062 → ErrConflict 面,退出 1。
	var out2, errb2 bytes.Buffer
	code = userCreate(context.Background(), env, userCreateParams{
		username: name,
		rolesRaw: "author",
	}, &out2, &errb2)
	if code != ExitError {
		t.Fatalf("duplicate exit = %d, want 1; stderr: %q", code, errb2.String())
	}
	if !strings.Contains(errb2.String(), "auth: conflict") {
		t.Errorf("duplicate stderr missing conflict sentinel: %q", errb2.String())
	}
}

// TestUserCreateValidation 前置拒绝面(env 缺失/口令短/坏角色/保留名),
// 退出码 1 + stderr 指引。词表/词域真源在 auth(不做 CLI 侧复制),
// 坏角色与保留名走真库到 CreateUser 校验(无写路径,回滚收尾)。
func TestUserCreateValidation(t *testing.T) {
	const pw = "correct horse battery"
	tests := []struct {
		name    string
		env     map[string]string // 叠在默认 DSN 之上的覆盖(空值可抹掉默认)
		p       userCreateParams
		wantSub string
	}{
		{
			name:    "missing DSN",
			env:     map[string]string{"BRAID_MYSQL_DSN": "", "BRAID_CLI_PASSWORD": pw},
			p:       userCreateParams{username: "alice", rolesRaw: "author"},
			wantSub: "BRAID_MYSQL_DSN",
		},
		{
			name:    "missing password env",
			env:     map[string]string{},
			p:       userCreateParams{username: "alice", rolesRaw: "author"},
			wantSub: "BRAID_CLI_PASSWORD",
		},
		{
			name:    "password too short",
			env:     map[string]string{"BRAID_CLI_PASSWORD": "short"},
			p:       userCreateParams{username: "alice", rolesRaw: "author"},
			wantSub: "shorter than 8",
		},
		{
			name:    "unknown role",
			env:     map[string]string{"BRAID_CLI_PASSWORD": pw},
			p:       userCreateParams{username: "alice", rolesRaw: "root"},
			wantSub: "unknown role",
		},
		{
			name:    "reserved username",
			env:     map[string]string{"BRAID_CLI_PASSWORD": pw},
			p:       userCreateParams{username: "system", rolesRaw: "author"},
			wantSub: "username",
		},
		{
			name:    "empty role segment",
			env:     map[string]string{"BRAID_CLI_PASSWORD": pw},
			p:       userCreateParams{username: "alice", rolesRaw: "author,"},
			wantSub: "role",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newDBEnv(t, tt.env)
			var out, errb bytes.Buffer
			code := userCreate(context.Background(), env, tt.p, &out, &errb)
			if code != ExitError {
				t.Fatalf("exit = %d, want 1; stderr: %q", code, errb.String())
			}
			if !strings.Contains(errb.String(), tt.wantSub) {
				t.Errorf("stderr missing %q; got: %q", tt.wantSub, errb.String())
			}
		})
	}
}
