package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAuthenticateSuccess(t *testing.T) {
	d := newDB(t)
	createUser(t, d, "alice", "valid-password-01", RoleAuthor, RoleApprover)

	u, err := Authenticate(context.Background(), d, "alice", "valid-password-01")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if u.Username != "alice" || u.ID == 0 || u.Status != StatusActive {
		t.Fatalf("u = %+v", u)
	}
	if len(u.Roles) != 2 || u.Roles[0] != RoleApprover || u.Roles[1] != RoleAuthor {
		t.Fatalf("roles = %v, want [approver author](Go 侧字典序升序,确定性 claims)", u.Roles)
	}
}

func TestAuthenticateWrongPassword(t *testing.T) {
	d := newDB(t)
	createUser(t, d, "alice", "valid-password-01", RoleAuthor)

	_, err := Authenticate(context.Background(), d, "alice", "wrong-password-99")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
	var ce *CredentialsError
	if !errors.As(err, &ce) || ce.Reason != ReasonWrongPassword {
		t.Fatalf("err = %v, want CredentialsError{wrong_password}", err)
	}
	if errors.Is(err, ErrUserDisabled) {
		t.Fatalf("错密码被误报为停用(状态探测面)")
	}
	// 双锁:用户行无副作用。
	if n := countRows(t, d, "users"); n != 1 {
		t.Fatalf("users 行数 = %d, want 1", n)
	}
}

func TestAuthenticateUnknownUser(t *testing.T) {
	d := newDB(t)
	_, err := Authenticate(context.Background(), d, "ghost", "some-password-123")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
	var ce *CredentialsError
	if !errors.As(err, &ce) || ce.Reason != ReasonUnknownUser {
		t.Fatalf("err = %v, want CredentialsError{unknown_user}", err)
	}
	// 防枚举对拍(review 纠正:此前第二探针也是未知用户,同路径自证)。
	// 真「未知用户」vs「已知用户+错密码」——两条路径 errors.Is 同归
	// ErrInvalidCredentials,外部判定面不可区分存在性;三态互异只存在
	// 于 errors.As 的内部归因,仅供日志。
	createUser(t, d, "alice", "valid-password-01", RoleAuthor)
	_, errWrong := Authenticate(context.Background(), d, "alice", "wrong-password-99")
	if !errors.Is(errWrong, ErrInvalidCredentials) {
		t.Fatalf("errWrong = %v, want ErrInvalidCredentials", errWrong)
	}
}

// dummy 时序均衡的存在性锚:未知用户路径必须真实跑一次 bcrypt 比对
// (DefaultCost 数十 ms 量级),删除 dummyCompare 后本路径毫秒级返回
// → 红。下限 15ms 取 ≈3 倍余量:慢机只会更慢,不构成假红来源。
func TestAuthenticateUnknownUserTimingFloor(t *testing.T) {
	d := newDB(t)
	start := time.Now()
	_, err := Authenticate(context.Background(), d, "ghost-user-01", "some-password-123")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
	if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
		t.Fatalf("未知用户路径 %v 返回,时序均衡缺失(dummy 比对未执行?)", elapsed)
	}
}

// 三态互异可观测性:同一 username 在三种失败场景的 errors.As 归因互不
// 相同,且外部 sentinel 判定面统一(convention 嫁接:运维可诊断,
// 客户端不可枚举)。
func TestAuthenticateReasonsDistinct(t *testing.T) {
	d := newDB(t)
	createUser(t, d, "alice", "valid-password-01", RoleAuthor)

	reasonOf := func(password string) string {
		t.Helper()
		_, err := Authenticate(context.Background(), d, "alice", password)
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("password %q: err = %v, want ErrInvalidCredentials", password, err)
		}
		var ce *CredentialsError
		if !errors.As(err, &ce) {
			t.Fatalf("password %q: 无 CredentialsError 归因", password)
		}
		return ce.Reason
	}
	ghostReason := func() string {
		t.Helper()
		_, err := Authenticate(context.Background(), d, "nobody-here-01", "valid-password-01")
		var ce *CredentialsError
		if !errors.As(err, &ce) {
			t.Fatalf("unknown user: 无 CredentialsError 归因")
		}
		return ce.Reason
	}
	rs := map[string]bool{
		reasonOf("wrong-password-99"): true,
		ghostReason():                 true,
		ReasonInvalidInput:            true, // 域外输入路径(下个测试的 panicky 证明)
	}
	if len(rs) != 3 {
		t.Fatalf("三态归因塌缩: %v", rs)
	}
}

// 变异验证:调换 Authenticate 的「先验密码后查状态」顺序(先查
// DISABLED 再比对),本测试红——错密码的停用用户必须仍得到统一
// 凭证错误,防账号状态探测。
func TestAuthenticateDisabledOrdering(t *testing.T) {
	d := newDB(t)
	createUser(t, d, "bob", "valid-password-01", RoleApprover)
	if _, err := d.Exec(`UPDATE users SET status = 'DISABLED' WHERE username = 'bob'`); err != nil {
		t.Fatalf("disable: %v", err)
	}

	_, err := Authenticate(context.Background(), d, "bob", "wrong-password-99")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials(错密码优先,不暴露停用)", err)
	}
	if errors.Is(err, ErrUserDisabled) {
		t.Fatalf("停用状态经错密码路径泄漏")
	}
}

func TestAuthenticateDisabledCorrectPassword(t *testing.T) {
	d := newDB(t)
	createUser(t, d, "bob", "valid-password-01", RoleApprover)
	if _, err := d.Exec(`UPDATE users SET status = 'DISABLED' WHERE username = 'bob'`); err != nil {
		t.Fatalf("disable: %v", err)
	}
	_, err := Authenticate(context.Background(), d, "bob", "valid-password-01")
	if !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("err = %v, want ErrUserDisabled(持正确凭证者可诊断)", err)
	}
	if errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("停用被并入凭证错误,运维不可诊断")
	}
}

// 前置域外拒绝的构造性证明:非法 username / 密码长度越界在任何 DB 调用
// 之前返回(panicky mock:到达即 panic;>72 同时封「仅超长口令可探测」
// 信道与 bcrypt 截断)。
func TestAuthenticatePreDBRejection(t *testing.T) {
	cases := []struct {
		name     string
		username string
		password string
	}{
		{"uppercase username", "Alice", "valid-password-01"},
		{"short password", "alice", "short"},
		{"password over 72", "alice", strings.Repeat("a", 73)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Authenticate(context.Background(), panickyDBTX{}, tc.username, tc.password)
			if !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("err = %v, want ErrInvalidCredentials", err)
			}
			var ce *CredentialsError
			if !errors.As(err, &ce) || ce.Reason != ReasonInvalidInput {
				t.Fatalf("err = %v, want CredentialsError{invalid_input}", err)
			}
		})
	}
}

// 验收三条的端到端复合:登录 → 签发 → 解析 → 角色判定;越权 → 拒绝。
func TestLoginTokenRoleComposite(t *testing.T) {
	d := newDB(t)
	createUser(t, d, "alice", "valid-password-01", RoleAuthor, RoleApprover)

	u, err := Authenticate(context.Background(), d, "alice", "valid-password-01")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	tok, err := SignToken(testSecret, time.Hour, u.Username, u.Roles)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	claims, err := ParseToken(testSecret, tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := RequireRole(claims, RoleApprover); err != nil {
		t.Fatalf("持角色被拒: %v", err)
	}
	if err := RequireRole(claims, RoleAdmin); !errors.Is(err, ErrForbidden) {
		t.Fatalf("角色越权: err = %v, want ErrForbidden", err)
	}
}
