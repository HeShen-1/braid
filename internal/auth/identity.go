package auth

import (
	"context"
	"errors"
	"fmt"
)

// IdentityProvider 是认证身份源的插件缝(ARCHITECTURE §3.6 / T5.3):
// 凭据 → 读齐角色的用户视图。方法面刻意不带 DBTX——provider 自持其
// 存储(内置实现持连接池;替换实现可以无存储,api 包的替换验收以
// nil dbh 注入即证明)。本缝挂载的是凭证核验型身份源:内置 bcrypt、
// LDAP bind、测试桩。OIDC 属 redirect 流,形状装不进本接口(无口令
// 可传、302 非 (User, error)),P1 须另立回调路由面——但错误族与
// User→SignToken 契约可原样复用。
//
// 替换契约(internal/api/login.go 的错误映射依赖,违反即登录面行为
// 漂移):
//   - 凭证不符 → *CredentialsError(Unwrap ErrInvalidCredentials → 401;
//     三态 Reason 词表见 errors.go);
//   - 凭证验证通过但账号停用 → ErrUserDisabled(403);
//   - 其余错误原样上抛(500)。
//
// 成功返回的 User 必须与 SignToken 兼容:Username 过 validUsername 域
// (users.go,与 DDL chk_user_username 双闸)、Roles 非空/词表内/无重复
// (token.go validRoles)——不满足时登录面签发即 500(响亮拒绝,不产出
// 坏 token)。
type IdentityProvider interface {
	Authenticate(ctx context.Context, username, password string) (User, error)
}

// BuiltinProvider 内置身份源(默认):一行委托包级 Authenticate,防枚举
// 三态统一、dummy 时序均衡、DISABLED 密码后判定全部继承,逻辑零改动。
// 值类型:DB 为 nil DBTX 时仅域外输入路径可完整走完,触库调用会 panic
// ——装配方负责注入真实连接池(生产装配见 cmd/api)。
type BuiltinProvider struct {
	DB DBTX
}

// Authenticate 委托包级原语;签名差异只有存储持有方式(接口面自持)。
func (p BuiltinProvider) Authenticate(ctx context.Context, username, password string) (User, error) {
	return Authenticate(ctx, p.DB, username, password)
}

// ProviderFunc 是 IdentityProvider 的函数适配器(http.HandlerFunc 同款):
// 测试桩的一行换装面,也是 P1 凭证核验型插件(如 LDAP bind)的最小
// 挂点。nil 值调用不 panic 而是返回 ErrInvalidInput(fail-closed 500),
// 给不经 NewServer 装配的消费方兜底;Server 侧另有构造期 typed-nil 拒绝。
type ProviderFunc func(ctx context.Context, username, password string) (User, error)

// Authenticate 直通 f,零加工——直通性由 TestProviderFuncPassthrough 钉死。
func (f ProviderFunc) Authenticate(ctx context.Context, username, password string) (User, error) {
	if f == nil {
		return User{}, fmt.Errorf("%w: nil ProviderFunc", ErrInvalidInput)
	}
	return f(ctx, username, password)
}

// AnchorLoginUser 把外部身份源判定通过的主体锚定到本地账号(T16.1,
// D-2026-10-01-t161:enterprise OIDC 完成面的核心侧缝——本地账号锚定是
// 平台数据契约,角色/状态自本地库,D-t111):词域外/缺失 →
// *CredentialsError(ReasonInvalidInput);本地无账号 → *CredentialsError
// (ReasonUnknownUser);停用 → ErrUserDisabled;库故障原样上抛。错误族 =
// 本文件 IdentityProvider 契约同款,api 登录面(401/403/500)按既有
// 映射消费,不新开分类面。
func AnchorLoginUser(ctx context.Context, dbh DBTX, username string) (User, error) {
	if !validUsername(username) {
		return User{}, &CredentialsError{Username: username, Reason: ReasonInvalidInput}
	}
	rec, err := findByUsername(ctx, dbh, username)
	if errors.Is(err, errUserNotFound) {
		return User{}, &CredentialsError{Username: username, Reason: ReasonUnknownUser}
	}
	if err != nil {
		return User{}, err // 查询故障原样上抛,不吞不降级
	}
	if rec.Status == StatusDisabled {
		return User{}, fmt.Errorf("%w: %q", ErrUserDisabled, username)
	}
	return rec.User, nil
}
