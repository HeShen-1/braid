package auth

// AnchorLoginUser(T16.1,外部身份源本地锚定缝)表驱动:词域外 →
// invalid_input;本地无账号 → unknown_user(链 ErrInvalidCredentials);
// 停用 → ErrUserDisabled;健康行 → 读齐角色的 User 视图。企业 OIDC
// Complete 经本缝消费(错误族契约的正面锚;零栈臂注入 nil dbh 只走
// 域外路径)。

import (
	"context"
	"errors"
	"testing"
)

func TestAnchorLoginUser(t *testing.T) {
	t.Run("invalid_username_domain", func(t *testing.T) {
		_, err := AnchorLoginUser(context.Background(), nil, "Alice-With-Caps")
		var creds *CredentialsError
		if !errors.As(err, &creds) || creds.Reason != ReasonInvalidInput {
			t.Fatalf("err = %v, want CredentialsError(invalid_input)", err)
		}
	})
	t.Run("table_faces", func(t *testing.T) {
		d := newDB(t)
		createUser(t, d, "ada", "pw-unused-anchor-1", RoleAuthor)
		createUser(t, d, "bob", "pw-unused-anchor-2", RoleApprover)
		if _, err := d.Exec(`UPDATE users SET status = 'DISABLED' WHERE username = 'bob'`); err != nil {
			t.Fatalf("disable: %v", err)
		}
		cases := []struct {
			name      string
			username  string
			wantErrAs func(error) bool // nil = 期望成功
		}{
			{
				name:     "unknown_user",
				username: "nobody-here",
				wantErrAs: func(err error) bool {
					var creds *CredentialsError
					return errors.As(err, &creds) && creds.Reason == ReasonUnknownUser
				},
			},
			{
				name:     "disabled",
				username: "bob",
				wantErrAs: func(err error) bool {
					return errors.Is(err, ErrUserDisabled)
				},
			},
			{
				name:     "active_user_full_view",
				username: "ada",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				user, err := AnchorLoginUser(context.Background(), d, tc.username)
				if tc.wantErrAs == nil {
					if err != nil {
						t.Fatalf("err = %v, want nil", err)
					}
					if user.Username != tc.username || len(user.Roles) != 1 || user.Roles[0] != RoleAuthor {
						t.Fatalf("user = %+v, want 读齐角色的本地视图", user)
					}
					return
				}
				if err == nil || !tc.wantErrAs(err) {
					t.Fatalf("err = %v, want 错误族命中", err)
				}
			})
		}
		// unknown_user 臂的链上契约:登录面 401 判据。
		_, err := AnchorLoginUser(context.Background(), d, "nobody-here")
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("err 链缺 ErrInvalidCredentials: %v", err)
		}
	})
}

// TestValidUsernameExportedDelegation:T17.6 导出面与 validUsername 同判
// (词域单源;凭证核验型插件的前置门不开第二判据,两套判据必漂移——
// login.go 域校验注释同则)。
func TestValidUsernameExportedDelegation(t *testing.T) {
	cases := map[string]bool{
		"alice": true, "a-b_c9": true, "u12345678901234567890": true,
		"Sir.Mixed": false, "A*B(c)": false, "": false, "system": false,
	}
	for s, want := range cases {
		if got := ValidUsername(s); got != want {
			t.Errorf("ValidUsername(%q) = %v, want %v", s, got, want)
		}
	}
}
