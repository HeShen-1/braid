package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// panickyDBTX:任何 DB 调用即 panic——「前置校验不触 DB」的构造性
// 证明(engine execRecorder 是录制式,QueryRowContext 返回 *sql.Row
// 无法离线伪造,断言反转成「不得到达」)。
type panickyDBTX struct{}

func (panickyDBTX) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	panic("auth: unexpected ExecContext call")
}

func (panickyDBTX) QueryRowContext(context.Context, string, ...any) *sql.Row {
	panic("auth: unexpected QueryRowContext call")
}

func countRows(t *testing.T, d *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func mysqlErrNum(t *testing.T, err error) uint16 {
	t.Helper()
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		t.Fatalf("err = %v, want *mysql.MySQLError", err)
	}
	return me.Number
}

func TestCreateUserPersists(t *testing.T) {
	d := newDB(t)
	hash, err := HashPassword("valid-password-01", 4)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := WithTx(context.Background(), d, func(q DBTX) error {
		return CreateUser(context.Background(), q, newIDs(), CreateUserParams{
			Username: "alice", PasswordHash: hash, Roles: []Role{RoleAuthor, RoleApprover},
		})
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	var gotHash, gotStatus string
	var gotID uint64
	if err := d.QueryRow(`SELECT id, password_hash, status FROM users WHERE username = 'alice'`).
		Scan(&gotID, &gotHash, &gotStatus); err != nil {
		t.Fatalf("select user: %v", err)
	}
	if gotID == 0 || gotHash != hash || gotStatus != "ACTIVE" {
		t.Fatalf("user row = (%d, %q, %q)", gotID, gotHash, gotStatus)
	}
	var roles string
	if err := d.QueryRow(
		`SELECT GROUP_CONCAT(role ORDER BY role) FROM user_role WHERE user_id = ?`, gotID,
	).Scan(&roles); err != nil {
		t.Fatalf("select roles: %v", err)
	}
	// MySQL ENUM 列排序按声明序(内部索引)而非字典序:author(2) 先于
	// approver(3);Go 侧 slices.Sort 是字典序,两路径各自确定。
	if roles != "author,approver" {
		t.Fatalf("roles = %q, want author,approver", roles)
	}
}

func TestCreateUserDuplicateUsername(t *testing.T) {
	d := newDB(t)
	createUser(t, d, "alice", "valid-password-01", RoleAuthor)

	hash2, _ := HashPassword("another-password-2", 4)
	err := WithTx(context.Background(), d, func(q DBTX) error {
		return CreateUser(context.Background(), q, newIDs(), CreateUserParams{
			Username: "alice", PasswordHash: hash2, Roles: []Role{RoleApprover},
		})
	})
	// 双锁之一:1062 → ErrConflict。
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	// 双锁之二:既有行逐列不变(第二个哈希/角色不得部分落库)。
	var gotHash, gotStatus string
	if err := d.QueryRow(`SELECT password_hash, status FROM users WHERE username = 'alice'`).
		Scan(&gotHash, &gotStatus); err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(gotHash) != 60 || gotStatus != "ACTIVE" || gotHash == hash2 {
		t.Fatalf("回读漂移: hash=%q status=%q(第二个哈希不得落地)", gotHash, gotStatus)
	}
	if n := countRows(t, d, "user_role"); n != 1 {
		t.Fatalf("user_role 行数 = %d, want 1(重复授予不得落地)", n)
	}
}

func TestCreateUserInvalidInput(t *testing.T) {
	// 前置拒绝的构造性证明:校验失败在触 DB(含 ID 生成后写库)之前。
	hash, _ := HashPassword("valid-password-01", 4)
	cases := []struct {
		name string
		p    CreateUserParams
	}{
		{"uppercase username", CreateUserParams{"Alice", hash, []Role{RoleAdmin}}},
		{"reserved username system", CreateUserParams{"system", hash, []Role{RoleAdmin}}},
		{"username with space", CreateUserParams{"a b", hash, []Role{RoleAdmin}}},
		{"username leading digit", CreateUserParams{"1abc", hash, []Role{RoleAdmin}}},
		{"empty username", CreateUserParams{"", hash, []Role{RoleAdmin}}},
		{"username over 128", CreateUserParams{strings.Repeat("a", 129), hash, []Role{RoleAdmin}}},
		{"plaintext hash", CreateUserParams{"alice", strings.Repeat("x", 60), []Role{RoleAdmin}}},
		{"short hash", CreateUserParams{"alice", "$2a$06", []Role{RoleAdmin}}},
		{"empty roles", CreateUserParams{"alice", hash, nil}},
		{"unknown role", CreateUserParams{"alice", hash, []Role{Role("root")}}},
		{"duplicate roles", CreateUserParams{"alice", hash, []Role{RoleAdmin, RoleAdmin}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CreateUser(context.Background(), panickyDBTX{}, newIDs(), tc.p)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput(且不得触 DB)", err)
			}
		})
	}
}

func TestWithTxRollback(t *testing.T) {
	// 组合纪律:fn 失败 → user 行 + 角色行全部回滚(WithTx 是原子性
	// 的唯一承载,原语不自管事务)。
	d := newDB(t)
	hash, _ := HashPassword("valid-password-01", 4)
	boom := errors.New("boom-after-create")
	err := WithTx(context.Background(), d, func(q DBTX) error {
		if err := CreateUser(context.Background(), q, newIDs(), CreateUserParams{
			Username: "alice", PasswordHash: hash, Roles: []Role{RoleAdmin},
		}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom 透传", err)
	}
	// 变异锚(review 实测纠正):删除 WithTx 的 Rollback 后,下面的
	// countRows 断言照绿(未提交行经池内其他连接天然不可见),泄漏的
	// 事务连接只表现为测试变慢(清理 DELETE 撞锁超时)——本断言钉死
	// 「错误路径事务连接归还」:回滚完成才 InUse=0,删 Rollback 恒 1。
	if n := d.Stats().InUse; n != 0 {
		t.Fatalf("InUse = %d, want 0(错误路径事务连接未归还,Rollback 失效?)", n)
	}
	if n := countRows(t, d, "users"); n != 0 {
		t.Fatalf("users 行数 = %d, want 0(事务回滚失效)", n)
	}
	if n := countRows(t, d, "user_role"); n != 0 {
		t.Fatalf("user_role 行数 = %d, want 0", n)
	}
}

// 变异验证:删掉 DDL 的 chk_user_username,本测试红(DB 层域钉死;
// Go 侧 validUsername 拦不住直插旁路)。
func TestUsernameDomainPinned(t *testing.T) {
	d := newDB(t)
	// 哈希走合法形态($2a$ 前缀 + 恰 60 字符),隔离 username CHECK:
	// 列长/前缀错误会先以 1406/3819 在 password_hash 上炸,测不到目标。
	shapeOKHash := "$2a$04$" + strings.Repeat(".", 53)
	_, err := d.Exec(`INSERT INTO users (id, username, password_hash) VALUES (?, ?, ?)`,
		1, "BadName", shapeOKHash)
	if err == nil {
		t.Fatalf("非法 username 直插被放行(chk_user_username 失效?)")
	}
	if num := mysqlErrNum(t, err); num != 3819 {
		t.Fatalf("err no = %d, want 3819 (ER_CHECK_CONSTRAINT_VIOLATED)", num)
	}
	// 保留字 'system'(审计系统触发哨兵)同被 CHECK 拒绝——双闸的
	// 库端半边(Go 侧见 TestCreateUserInvalidInput/reserved username)。
	if _, err := d.Exec(`INSERT INTO users (id, username, password_hash) VALUES (?, ?, ?)`,
		2, "system", shapeOKHash); err == nil {
		t.Fatalf("保留字 username=system 直插被放行")
	} else if num := mysqlErrNum(t, err); num != 3819 {
		t.Fatalf("err no = %d, want 3819", num)
	}
}

// 变异验证:删掉 DDL 的 chk_user_password_hash,本测试红(防降级门闸:
// 明文/裸哈希写入必须 3819 响亮失败,不得静默降级)。
func TestPasswordHashDomainPinned(t *testing.T) {
	d := newDB(t)
	_, err := d.Exec(`INSERT INTO users (id, username, password_hash) VALUES (?, ?, ?)`,
		1, "alice", strings.Repeat("x", 60))
	if err == nil {
		t.Fatalf("明文密码哈希直插被放行(chk_user_password_hash 失效?)")
	}
	if num := mysqlErrNum(t, err); num != 3819 {
		t.Fatalf("err no = %d, want 3819", num)
	}
}

// ENUM 词表含序钉死(engine/enum_test 先例):Go 常量序 = DDL ENUM
// 声明序,任一侧改词表即红。
func TestRoleEnumVocabPinned(t *testing.T) {
	d := newDB(t)
	cases := []struct {
		table, column, want string
	}{
		{"user_role", "role", "enum('admin','author','approver')"},
		{"users", "status", "enum('ACTIVE','DISABLED')"},
	}
	for _, tc := range cases {
		t.Run(tc.table+"."+tc.column, func(t *testing.T) {
			var colType string
			if err := d.QueryRow(
				`SELECT COLUMN_TYPE FROM information_schema.COLUMNS
				 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
				tc.table, tc.column,
			).Scan(&colType); err != nil {
				t.Fatalf("information_schema: %v", err)
			}
			if colType != tc.want {
				t.Fatalf("%s.%s = %q, want %q(含序)", tc.table, tc.column, colType, tc.want)
			}
		})
	}
	// username 二进制排序规则钉死(防大小写折叠归并账号)。
	var collation string
	if err := d.QueryRow(
		`SELECT COLLATION_NAME FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'users' AND COLUMN_NAME = 'username'`,
	).Scan(&collation); err != nil {
		t.Fatalf("information_schema: %v", err)
	}
	if collation != "utf8mb4_bin" {
		t.Fatalf("username collation = %q, want utf8mb4_bin", collation)
	}
}

// FK RESTRICT 语义钉死:持角色行的用户不得被删除(身份不可静默蒸发,
// 审计归因完整性)。
func TestUserRoleFKRestrict(t *testing.T) {
	d := newDB(t)
	createUser(t, d, "alice", "valid-password-01", RoleAuthor)
	var uid uint64
	if err := d.QueryRow(`SELECT id FROM users WHERE username = 'alice'`).Scan(&uid); err != nil {
		t.Fatalf("select id: %v", err)
	}
	if _, err := d.Exec(`DELETE FROM users WHERE id = ?`, uid); err == nil {
		t.Fatalf("持角用户被删除(FK RESTRICT 失效?)")
	} else if num := mysqlErrNum(t, err); num != 1451 {
		t.Fatalf("err no = %d, want 1451 (ER_ROW_IS_REFERENCED)", num)
	}
}

func TestFindByUsernameAggregation(t *testing.T) {
	d := newDB(t)
	createUser(t, d, "alice", "valid-password-01", RoleApprover, RoleAuthor) // 乱序授予

	rec, err := findByUsername(context.Background(), d, "alice")
	if err != nil {
		t.Fatalf("findByUsername: %v", err)
	}
	if len(rec.Roles) != 2 || rec.Roles[0] != RoleApprover || rec.Roles[1] != RoleAuthor {
		t.Fatalf("roles = %v, want [approver author](字典序升序,确定性 claims)", rec.Roles)
	}
	if rec.Status != StatusActive || rec.ID == 0 || rec.passwordHash == "" {
		t.Fatalf("rec = %+v", rec)
	}

	// 未知用户 → 包内 not-found 信号(由 Authenticate 翻译为防枚举错误)。
	if _, err := findByUsername(context.Background(), d, "ghost"); !errors.Is(err, errUserNotFound) {
		t.Fatalf("err = %v, want errUserNotFound", err)
	}

	// 空授权行(直插旁路):COALESCE 路径得空集,不报错。
	hash, _ := HashPassword("valid-password-01", 4)
	if _, err := d.Exec(`INSERT INTO users (id, username, password_hash) VALUES (?, ?, ?)`,
		42, "bob", hash); err != nil {
		t.Fatalf("insert bob: %v", err)
	}
	rec, err = findByUsername(context.Background(), d, "bob")
	if err != nil {
		t.Fatalf("findByUsername(bob): %v", err)
	}
	if len(rec.Roles) != 0 {
		t.Fatalf("roles = %v, want empty", rec.Roles)
	}
}
