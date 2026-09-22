package auth

import "fmt"

// Role 角色:词表被 ARCHITECTURE Q13 锁死三种,不得自创第四种(追加
// 角色 = DDL ENUM 末尾追加 + 本文件常量 + RBAC 判定面同 PR 旅行)。
// 小写对齐 node_execution.type「词表名词」先例;与 user_role.role 的
// ENUM 含序一致(TestRoleEnumVocabPinned 对拍钉死)。
type Role string

const (
	RoleAdmin    Role = "admin"    // 平台管理
	RoleAuthor   Role = "author"   // workflow 作者
	RoleApprover Role = "approver" // 审批人
)

// allRoles 是词表唯一真源:声明序 = DDL ENUM 声明序。
var allRoles = []Role{RoleAdmin, RoleAuthor, RoleApprover}

// AllRoles 返回角色词表快照(防御性拷贝,防调用方改词表)。
func AllRoles() []Role {
	out := make([]Role, len(allRoles))
	copy(out, allRoles)
	return out
}

// validRole 报告 r 是否在词表内(含序无关;含序钉死在 ENUM 对拍测试)。
func validRole(r Role) bool {
	for _, v := range allRoles {
		if r == v {
			return true
		}
	}
	return false
}

// validRoles 校验角色集非空、逐个在词表内且无重复(重复授予由 uk_user_role
// 兜底,但前置拒绝调用方 bug 不触 DB)。
func validRoles(rs []Role) error {
	if len(rs) == 0 {
		return fmt.Errorf("%w: empty roles", ErrInvalidInput)
	}
	seen := make(map[Role]bool, len(rs))
	for _, r := range rs {
		if !validRole(r) {
			return fmt.Errorf("%w: unknown role %q", ErrInvalidInput, r)
		}
		if seen[r] {
			return fmt.Errorf("%w: duplicate role %q", ErrInvalidInput, r)
		}
		seen[r] = true
	}
	return nil
}
