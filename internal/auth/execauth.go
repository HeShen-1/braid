// execauth 是执行时工具 RBAC(T5.2):tools.CallAuthorizer 的生产实现。
// ARCHITECTURE §3.4「双重校验:定义时白名单 × 执行时 RBAC」的执行侧半边
// ——发布期白名单(registry.Names(),T5.4)校验定义引用了注册工具;本面
// 在每次工具调用发生的现场,按 node_exec 锚定执行主体再校验:调用发生时
// 该主体(workflow_execution.triggered_by)仍持有该工具所需角色。
//
// 主体语义:
//   - triggered_by = "system"(DDL 钦定系统触发哨兵,users 侧保留字不可
//     被人工账号占据)→ 平台自身触发放行;
//   - 其余主体经 findByUsername 现查:无用户行 / DISABLED / 角色不足 →
//     拒绝(fail-closed;执行时校验取当前账号态,停权即生效,不等 token
//     过期——触发者 token 只在 API 入口存在,worker 链路本就无凭)。
//
// 分层:本包(企业层)实现核心层接口 tools.CallAuthorizer——依赖方向
// 企业→核心合法(tools 不反向 import auth)。
//
// 错误语义(与 runner 的映射契约严格对齐):
//   - 越权拒绝 → 包装 tools.ErrCallDenied 的错误(runner 映射
//     CallError{tool_rbac_denied},节点 FAILED 收口);
//   - 锚行缺失 / DB 故障 → 裸错误(基础设施失败,attempt 留 RUNNING,
//     asynq 重试接管)——授权面故障不得伪装成越权,也不得静默放行。
package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"braid/internal/audit"
	"braid/internal/tools"
)

// ToolPolicy 是工具→所需角色的执行时策略。改判定面(默认集或逐工具
// 覆盖)= 同 PR 旅行(roles.go 词表纪律);P0 默认集与 API 触发面写权限
// 一致(author|admin),逐工具覆盖表留作收紧扩展点(如危险工具 admin-only)。
type ToolPolicy struct {
	defaults []Role
	perTool  map[string][]Role
}

// NewToolPolicy 校验并构造策略:默认集非空且词表内、逐工具键非空且角色
// 集非空词表内。畸形策略构造期响亮失败(启动即现,不带病运行)。
func NewToolPolicy(defaults []Role, perTool map[string][]Role) (ToolPolicy, error) {
	if err := validRoles(defaults); err != nil {
		return ToolPolicy{}, fmt.Errorf("auth: tool policy defaults: %w", err)
	}
	for name, roles := range perTool {
		if name == "" {
			return ToolPolicy{}, fmt.Errorf("%w: empty tool name in policy", ErrInvalidInput)
		}
		if err := validRoles(roles); err != nil {
			return ToolPolicy{}, fmt.Errorf("%w: policy for tool %q: %v", ErrInvalidInput, name, err)
		}
	}
	return ToolPolicy{defaults: defaults, perTool: perTool}, nil
}

// DefaultToolPolicy:P0 默认策略——全部工具要求 author|admin(与 definition
// 写面 / execution 触发面的角色闸同集,执行时强制 = 该闸在调用现场的
// 防御纵深,不信任上游 API 门)。
func DefaultToolPolicy() ToolPolicy {
	return ToolPolicy{defaults: []Role{RoleAuthor, RoleAdmin}}
}

// required 返回工具所需角色(逐工具覆盖优先,缺省回默认集)。
func (p ToolPolicy) required(tool string) []Role {
	if roles, ok := p.perTool[tool]; ok {
		return roles
	}
	return p.defaults
}

// systemActor 是系统触发哨兵(users 保留字同源;独立常量避免测试直接
// 依赖 reservedUsernames 私有面)。
const systemActor = "system"

// ExecutionAuthorizer 是 tools.CallAuthorizer 的执行时实现:两段读
// (node_exec→execution 锚行;主体→用户+角色)+ 策略判定 + 拒绝落审计。
type ExecutionAuthorizer struct {
	db  *sql.DB
	ids IDGen
	pol ToolPolicy
}

// NewExecutionAuthorizer 构造执行时授权器;nil 依赖 panic(装配缺陷
// 启动即现)。
func NewExecutionAuthorizer(db *sql.DB, ids IDGen, pol ToolPolicy) *ExecutionAuthorizer {
	if db == nil || ids == nil {
		panic("auth: nil db or IDGen for ExecutionAuthorizer")
	}
	return &ExecutionAuthorizer{db: db, ids: ids, pol: pol}
}

// execAnchor 是调用现场的执行锚:被校验主体 + 审计行定位四件套。
type execAnchor struct {
	execID      uint64
	triggeredBy string
	nodeID      string
}

// anchor 从 node_execution 锚定执行行(单 JOIN;找不到 = 调用方传入的
// node_exec id 不存在,基础设施面错误)。
func (a *ExecutionAuthorizer) anchor(ctx context.Context, nodeExecID uint64) (execAnchor, error) {
	var an execAnchor
	err := a.db.QueryRowContext(ctx,
		`SELECT we.id, we.triggered_by, ne.node_id
		 FROM node_execution ne JOIN workflow_execution we ON we.id = ne.execution_id
		 WHERE ne.id = ?`, nodeExecID,
	).Scan(&an.execID, &an.triggeredBy, &an.nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return an, fmt.Errorf("auth: authorize call: node_execution %d not found", nodeExecID)
	}
	if err != nil {
		return an, fmt.Errorf("auth: anchor node_execution %d: %w", nodeExecID, err)
	}
	return an, nil
}

// AuthorizeCall 实现 tools.CallAuthorizer:见文件头语义说明。
func (a *ExecutionAuthorizer) AuthorizeCall(ctx context.Context, nodeExecID uint64, tool string) error {
	an, err := a.anchor(ctx, nodeExecID)
	if err != nil {
		return err // 基础设施失败(裸 error,无哨兵)
	}
	if an.triggeredBy == systemActor {
		return nil // 平台哨兵:系统触发的执行不经用户角色面
	}
	rec, err := findByUsername(ctx, a.db, an.triggeredBy)
	if err != nil {
		if errors.Is(err, errUserNotFound) {
			// 主体无用户行(用户被清理/外部 IdP 换源残留)= fail-closed 拒绝,
			// 不是基础设施失败——确定性事实,可安全落 FAILED 收口。
			return a.deny(ctx, nodeExecID, an, tool, nil, "unknown_subject")
		}
		return fmt.Errorf("auth: load subject %q: %w", an.triggeredBy, err)
	}
	if rec.Status != StatusActive {
		return a.deny(ctx, nodeExecID, an, tool, rec.Roles, "disabled")
	}
	for _, want := range a.pol.required(tool) {
		for _, have := range rec.Roles {
			if want == have {
				return nil
			}
		}
	}
	return a.deny(ctx, nodeExecID, an, tool, rec.Roles, "role_mismatch")
}

// deny 落拒绝审计行(best-effort,失败仅 slog——拒绝裁决不受旁路写入
// 失败影响,先例 = api.writeDecisionDeniedAudit)并返回哨兵包装错误。
// 错误消息刻意静态(sec 评审 LOW):区分维度走 payload.reason 只进审计
// ——error_message 列在 GET /executions/:id 公开读面,主体账号态/角色
// 差额细节不落公开面。
func (a *ExecutionAuthorizer) deny(ctx context.Context, nodeExecID uint64, an execAnchor, tool string, roles []Role, reason string) error {
	a.appendDeniedAudit(ctx, nodeExecID, an, tool, roles, reason)
	return fmt.Errorf("auth: execution subject not authorized for tool %q (node_execution %d): %w",
		tool, nodeExecID, tools.ErrCallDenied)
}

// appendDeniedAudit 写 tool.call.denied 审计行(独立小事务)。actor=
// user/被拒主体;attempt_id NULL(拒绝发生在 attempt 收口之前,工具
// 零触达)。actor_roles 恒为数组(make 构造非 nil——nil slice 会序列化
// 成 null),空集即 [],运维对账面确定性。
func (a *ExecutionAuthorizer) appendDeniedAudit(ctx context.Context, nodeExecID uint64, an execAnchor, tool string, roles []Role, reason string) {
	id, err := a.ids.Next()
	if err != nil {
		slog.Error("auth: audit id gen for tool.call.denied", "err", err, "node_exec_id", nodeExecID)
		return
	}
	roleStrs := make([]string, len(roles))
	for i, r := range roles {
		roleStrs[i] = string(r)
	}
	raw, err := json.Marshal(audit.ToolCallDeniedPayload{
		NodeID: an.nodeID, Tool: tool, ActorRoles: roleStrs, Reason: reason,
	})
	if err != nil {
		slog.Error("auth: marshal tool.call.denied payload", "err", err, "node_exec_id", nodeExecID)
		return
	}
	err = WithTx(ctx, a.db, func(tx DBTX) error {
		return audit.Append(ctx, tx, audit.Event{
			ID: id, EventType: audit.EventToolCallDenied,
			ExecutionID: an.execID, NodeExecID: nodeExecID, NodeID: an.nodeID,
			ActorType: audit.ActorUser, ActorID: an.triggeredBy, Payload: raw,
		})
	})
	if err != nil {
		slog.Error("auth: audit tool.call.denied", "err", err, "node_exec_id", nodeExecID)
	}
}
