package auth

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// T5.3 IdentityProvider 缝的单元面(零栈可跑,CI 无栈不 skip):接口 +
// 内置适配器 + 函数适配器。集成面的替换证明在 internal/api/login_idp_
// test.go(nil dbh 注入,登录全路径不触 DB)。

// 编译锚:内置与函数适配器都结构性满足缝——「桩实现可替换内置认证」
// 的编译验收(删任一类型或方法签名漂移即红)。
var (
	_ IdentityProvider = BuiltinProvider{}
	_ IdentityProvider = ProviderFunc(nil)
)

// TestBuiltinProviderDelegatesPreDB:内置适配器经接口调用,域外输入在
// 任何 DB 访问前被拒(panickyDBTX 到达即 panic)——委托保面的构造性
// 证明:错误面(ErrInvalidCredentials + CredentialsError{invalid_input})
// 与包级 Authenticate 逐字一致,防枚举三态与时序均衡随委托继承。
func TestBuiltinProviderDelegatesPreDB(t *testing.T) {
	var idp IdentityProvider = BuiltinProvider{DB: panickyDBTX{}}
	_, err := idp.Authenticate(context.Background(), "Alice", "valid-password-01")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
	var ce *CredentialsError
	if !errors.As(err, &ce) || ce.Reason != ReasonInvalidInput {
		t.Fatalf("err = %v, want CredentialsError{invalid_input}", err)
	}
}

// TestProviderFuncNilTotal:nil 函数值的调用是 panic;适配器转 ErrInvalidInput
// 让不经 NewServer 装配的消费方也 fail-closed(登录面 500)而非崩溃。
// Server 侧的构造期 typed-nil 拒绝见 api 包 TestNewServerRejectsTypedNilProvider。
func TestProviderFuncNilTotal(t *testing.T) {
	_, err := ProviderFunc(nil).Authenticate(context.Background(), "carol", "some-password-1")
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

// TestProviderFuncPassthrough:函数适配器原样直通——凭据逐字到达、成功
// 视图与错误面(凭证不符 / 停用 / 不透明故障)经接口调用后逐字保留。
// 替换实现的错误族契约就钉在这四种形态上(login.go 的 401/403/500 映射)。
func TestProviderFuncPassthrough(t *testing.T) {
	opaque := errors.New("idp: downstream unavailable")
	cases := []struct {
		name string
		user User
		err  error
	}{
		{
			name: "success",
			user: User{ID: 7, Username: "stub-ada", Roles: []Role{RoleApprover, RoleAuthor}, Status: StatusActive},
		},
		{"credentials", User{}, &CredentialsError{Username: "carol", Reason: ReasonWrongPassword}},
		{"disabled", User{}, fmt.Errorf("%w: %q", ErrUserDisabled, "bob")},
		{"opaque", User{}, opaque},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seenUser, seenPass string
			idp := ProviderFunc(func(_ context.Context, username, password string) (User, error) {
				seenUser, seenPass = username, password
				return tc.user, tc.err
			})
			got, err := idp.Authenticate(context.Background(), "carol", "some-password-1")
			if seenUser != "carol" || seenPass != "some-password-1" {
				t.Fatalf("凭据未逐字到达桩: (%q, %q)", seenUser, seenPass)
			}
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("err = %v, want 原样直通 %v", err, tc.err)
				}
				if !reflect.DeepEqual(got, User{}) {
					t.Fatalf("错误行 user = %+v, want 零值", got)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.user) {
				t.Fatalf("got (%+v, %v), want (%+v, nil)", got, err, tc.user)
			}
		})
	}
}
