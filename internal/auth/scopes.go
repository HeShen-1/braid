package auth

import "fmt"

// Scope 是 token 作用域(D-2026-09-30-t131-mcp-server):资源:动作形,
// 与角色正交——角色答「你是谁」(Q13 词表锁死三种),scp 答「这枚
// token 能做什么」。首个消费面 = MCP server 面(T13.1);REST 面迁移
// 同词表 = P1 停车场登记,不在本词表外自造第二套。
type Scope string

const (
	ScopeWorkflowsRead   Scope = "workflows:read"   // 定义发现/读
	ScopeExecutionsRead  Scope = "executions:read"  // 执行查询
	ScopeExecutionsWrite Scope = "executions:write" // 执行触发
)

// allScopes 是词表唯一真源:声明序 = DefaultScopesForRoles 映射序。
var allScopes = []Scope{ScopeWorkflowsRead, ScopeExecutionsRead, ScopeExecutionsWrite}

// AllScopes 返回作用域词表快照(防御性拷贝,防调用方改词表)。
func AllScopes() []Scope {
	out := make([]Scope, len(allScopes))
	copy(out, allScopes)
	return out
}

// validScope 报告 s 是否在词表内。
func validScope(s Scope) bool {
	for _, v := range allScopes {
		if v == s {
			return true
		}
	}
	return false
}

// validScopes 校验作用域集逐个在词表内且无重复。空集与 nil 合法——
// 零权限 token 是合法形态(scp 缺席 = 存量 token 兼容面,MCP 面
// fail-closed 零权限),与 roles 非空约束刻意不同:角色是身份下限,
// 作用域是能力上限,上限可为空。
func validScopes(ss []Scope) error {
	seen := make(map[Scope]bool, len(ss))
	for _, s := range ss {
		if !validScope(s) {
			return fmt.Errorf("%w: unknown scope %q", ErrInvalidInput, s)
		}
		if seen[s] {
			return fmt.Errorf("%w: duplicate scope %q", ErrInvalidInput, s)
		}
		seen[s] = true
	}
	return nil
}

// ValidateScopes 是 validScopes 的导出面:请求面(api /auth/tokens)在
// 签发前置校验请求载荷时消费,错误分类走 ErrInvalidInput(与签发原语
// 同判,双判据必漂移——这里只是出口不是第二套逻辑)。
func ValidateScopes(ss []Scope) error {
	return validScopes(ss)
}

// DefaultScopesForRoles 是角色 → 默认作用域映射的单一真源:admin|
// author → 三枚全量(author 与 REST 触发面同权),approver → 两枚
// 只读(镜像 REST 面:审批人只读执行与定义)。多角色并集。login 签发
// 默认与 /api/v1/auth/tokens 自窄化上界同吃这张表——两处各自抄一份
// 映射 = 漂移面,单源使「REST 权限与 token 默认作用域」的等价性可钉死。
func DefaultScopesForRoles(roles []Role) []Scope {
	out := make([]Scope, 0, len(allScopes))
	for _, s := range allScopes {
		for _, r := range roles {
			switch r {
			case RoleAdmin, RoleAuthor:
				// 管理与作者全量
			case RoleApprover:
				if s == ScopeExecutionsWrite {
					continue
				}
			default:
				// 词表外角色不可达(签发面已拒);防御性跳过
				continue
			}
			out = append(out, s)
			break
		}
	}
	return out
}

// RequireScope 作用域判定(MCP 面消费;形态镜像 RequireRole):命中
// 返回 nil,不足返回 ErrForbidden(附所需作用域与现有集,便于诊断),
// want 词表外返回 ErrInvalidInput(调用方 bug 前置拒绝)。
func RequireScope(c Claims, want Scope) error {
	if !validScope(want) {
		return fmt.Errorf("%w: unknown scope %q", ErrInvalidInput, want)
	}
	for _, s := range c.Scopes {
		if s == want {
			return nil
		}
	}
	return fmt.Errorf("%w: scope %q required, token has %v", ErrForbidden, want, c.Scopes)
}
