// Package ldap 是 LDAP bind 插件(T11.1 留桩,T16.1 切入企业增值面,
// D-2026-10-01-t161-engine-enterprise-split;实现归 T17.6)。
package ldap

import (
	"context"
	"fmt"

	"braid/internal/auth"
)

// LDAPBindProvider 是 LDAP bind 插件的留桩(T11.1 验收「LDAP bind 留桩」,
// 实现归 P1/后续任务)。桩的职责:把「凭证核验型插件挂点 = IdentityProvider
// (ProviderFunc 即最小挂点,identity.go)」的形状钉成可编译、可测试的事实
// ——后续任务只换 Authenticate 实现,缝面零改动。
//
// 刻意不返回 CredentialsError(凭据判定语义随实现定形)也不 panic:显式
// not-implemented 错误,装配后登录面表现为可诊断的 500,不误成功不误 401。
type LDAPBindProvider struct {
	// Addr 是 LDAP 服务器 URL(ldaps://host:port 或 ldap://host:port);
	// 字段面预登记,校验与装配随实现任务定形。
	Addr string
}

// Authenticate 恒定失败:LDAP bind 实现归后续任务(T11.1 留桩)。
func (p LDAPBindProvider) Authenticate(_ context.Context, _, _ string) (auth.User, error) {
	return auth.User{}, fmt.Errorf("auth: ldap bind provider not implemented (stub, addr %q)", p.Addr)
}
