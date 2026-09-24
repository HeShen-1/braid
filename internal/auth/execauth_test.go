package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"braid/internal/audit"
	"braid/internal/tools"
)

// execauth 测试的引擎侧种子:auth schema 跑全量迁移(0001+0002+0003),
// workflow_definition / workflow_execution / node_execution 可直插。清理按
// FK 逆序(禁 TRUNCATE 纪律;注册序在 newDB 之后 → 先于 users 清理执行)。
type execSeed struct {
	db     *sql.DB
	defID  uint64
	execID uint64
}

func seedExec(t *testing.T, d *sql.DB, triggeredBy string) execSeed {
	t.Helper()
	ids := newIDs()
	defID, err := ids.Next()
	if err != nil {
		t.Fatalf("def id: %v", err)
	}
	execID, err := ids.Next()
	if err != nil {
		t.Fatalf("exec id: %v", err)
	}
	nodeID := func() uint64 { n, _ := ids.Next(); return n }() // 第一个 node_exec id
	_, err = d.Exec(`INSERT INTO workflow_definition
		(id, name, dsl_version, dsl, dsl_hash, created_by, updated_by)
		VALUES (?, 'execauth_test', '1.0', '{}', ?, 'e2e', 'e2e')`,
		defID, strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	_, err = d.Exec(`INSERT INTO workflow_execution
		(id, definition_id, definition_name, definition_version, definition_hash,
		 trigger_type, triggered_by, input)
		VALUES (?, ?, 'execauth_test', 1, ?, 'TEST', ?, '{}')`,
		execID, defID, strings.Repeat("a", 64), triggeredBy)
	if err != nil {
		t.Fatalf("seed execution: %v", err)
	}
	_, err = d.Exec(`INSERT INTO node_execution (id, execution_id, node_id, type)
		VALUES (?, ?, 'fetch', 'tool')`, nodeID, execID)
	if err != nil {
		t.Fatalf("seed node_execution: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM audit_pending`) // T6.1 暂存面(防御:auth 面零暂存,保持清理面对称)
		_, _ = d.Exec(`DELETE FROM audit_log`)
		_, _ = d.Exec(`DELETE FROM node_execution WHERE execution_id = ?`, execID)
		_, _ = d.Exec(`DELETE FROM workflow_execution WHERE id = ?`, execID)
		_, _ = d.Exec(`DELETE FROM workflow_definition WHERE id = ?`, defID)
	})
	return execSeed{db: d, defID: defID, execID: execID}
}

func newAuthorizer(t *testing.T, d *sql.DB, pol ToolPolicy) *ExecutionAuthorizer {
	t.Helper()
	return NewExecutionAuthorizer(d, newIDs(), pol)
}

// TestAuthorizeCallPolicy:执行时 RBAC 判定表(T5.2 验收面)。放行 = nil;
// 越权 = 包装 tools.ErrCallDenied 的错误(runner 侧映射 tool_rbac_denied)。
func TestAuthorizeCallPolicy(t *testing.T) {
	d := newDB(t)
	createUser(t, d, "auth_author", "pw-author-words-placeholder", RoleAuthor)
	createUser(t, d, "auth_admin", "pw-admin-words-placeholder", RoleAdmin)
	createUser(t, d, "auth_approver", "pw-approver-words-placeholder", RoleApprover)

	perTool := map[string][]Role{"admin_tool": {RoleAdmin}}
	pol, err := NewToolPolicy([]Role{RoleAuthor, RoleAdmin}, perTool)
	if err != nil {
		t.Fatalf("NewToolPolicy: %v", err)
	}
	authz := newAuthorizer(t, d, pol)

	cases := []struct {
		name    string
		actor   string
		tool    string
		wantErr bool // true = 越权拒绝(ErrCallDenied);false = 放行
	}{
		{"author may call default tool", "auth_author", "http", false},
		{"admin may call default tool", "auth_admin", "http", false},
		{"approver-only denied on default tool", "auth_approver", "http", true},
		{"author denied on admin-only tool", "auth_author", "admin_tool", true},
		{"admin passes admin-only tool", "auth_admin", "admin_tool", false},
		{"unknown subject fail-closed", "auth_ghost", "http", true},
		{"system sentinel passes", "system", "http", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seed := seedExec(t, d, tc.actor)
			var nodeExecID uint64
			if err := d.QueryRow(`SELECT id FROM node_execution WHERE execution_id = ?`, seed.execID).Scan(&nodeExecID); err != nil {
				t.Fatalf("read node_exec id: %v", err)
			}
			err := authz.AuthorizeCall(context.Background(), nodeExecID, tc.tool)
			if tc.wantErr {
				if !errors.Is(err, tools.ErrCallDenied) {
					t.Fatalf("err = %v, want ErrCallDenied", err)
				}
			} else if err != nil {
				t.Fatalf("AuthorizeCall: %v", err)
			}
		})
	}
}

// TestAuthorizeCallDisabledUser:DISABLED 主体拒绝(角色在册也拒——执行时
// 校验取当前账号态,停权即失效,不等 token 过期)。
func TestAuthorizeCallDisabledUser(t *testing.T) {
	d := newDB(t)
	createUser(t, d, "auth_disabled", "pw-disabled-words-placeholder", RoleAuthor)
	if _, err := d.Exec(`UPDATE users SET status='DISABLED' WHERE username='auth_disabled'`); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	authz := newAuthorizer(t, d, DefaultToolPolicy())
	seed := seedExec(t, d, "auth_disabled")
	var nodeExecID uint64
	if err := d.QueryRow(`SELECT id FROM node_execution WHERE execution_id = ?`, seed.execID).Scan(&nodeExecID); err != nil {
		t.Fatalf("read node_exec id: %v", err)
	}
	err := authz.AuthorizeCall(context.Background(), nodeExecID, "http")
	if !errors.Is(err, tools.ErrCallDenied) {
		t.Fatalf("err = %v, want ErrCallDenied", err)
	}
	// 消息静态化(sec 评审 LOW):账号态细节只进审计 payload,不进会
	// 落 node error_message 公开读面的错误串。
	if strings.Contains(err.Error(), "auth_disabled") || strings.Contains(err.Error(), "DISABLED") {
		t.Fatalf("denial message leaks account state: %v", err)
	}
	// reason 变异锚(对抗审计):disabled 分因进 payload。
	var payload []byte
	if err := d.QueryRow(`SELECT payload FROM audit_log WHERE node_execution_id=?`, nodeExecID).Scan(&payload); err != nil {
		t.Fatalf("audit row missing: %v", err)
	}
	var p audit.ToolCallDeniedPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("payload unmarshal: %v (raw=%s)", err, payload)
	}
	if p.Reason != "disabled" {
		t.Fatalf("reason = %q, want disabled", p.Reason)
	}
}

// failingIDs 恒败 IDGen(best-effort 纪律的故障注入面)。
type failingIDs struct{}

func (failingIDs) Next() (uint64, error) { return 0, errors.New("idgen boom") }

// TestAuthorizeCallDenialIndependentOfAuditWrite:best-effort 契约的失败
// 注入锚(对抗审计 #15)——审计写链路死(雪花源故障)时拒绝裁决不受
// 影响:越权照拒 ErrCallDenied。
func TestAuthorizeCallDenialIndependentOfAuditWrite(t *testing.T) {
	d := newDB(t)
	createUser(t, d, "auth_approver3", "pw-approver3-words-placeholder", RoleApprover)
	authz := NewExecutionAuthorizer(d, failingIDs{}, DefaultToolPolicy())
	seed := seedExec(t, d, "auth_approver3")
	var nodeExecID uint64
	if err := d.QueryRow(`SELECT id FROM node_execution WHERE execution_id = ?`, seed.execID).Scan(&nodeExecID); err != nil {
		t.Fatalf("read node_exec id: %v", err)
	}
	if err := authz.AuthorizeCall(context.Background(), nodeExecID, "http"); !errors.Is(err, tools.ErrCallDenied) {
		t.Fatalf("err = %v, want ErrCallDenied despite audit-write failure", err)
	}
}

// TestAuthorizeCallDeniedAudited:越权拒绝必须落 tool.call.denied 审计行
// (T5.2 验收:越权工具调用被拒并落审计)。actor=被拒主体、payload 携工具名
// 与实持角色;attempt_id NULL(拒绝先于 attempt 收口)。
func TestAuthorizeCallDeniedAudited(t *testing.T) {
	d := newDB(t)
	createUser(t, d, "auth_approver2", "pw-approver2-words-placeholder", RoleApprover)
	authz := newAuthorizer(t, d, DefaultToolPolicy())
	seed := seedExec(t, d, "auth_approver2")
	var nodeExecID uint64
	if err := d.QueryRow(`SELECT id FROM node_execution WHERE execution_id = ?`, seed.execID).Scan(&nodeExecID); err != nil {
		t.Fatalf("read node_exec id: %v", err)
	}
	if err := authz.AuthorizeCall(context.Background(), nodeExecID, "http"); !errors.Is(err, tools.ErrCallDenied) {
		t.Fatalf("err = %v, want ErrCallDenied", err)
	}
	var (
		eventType, actorType, actorID, nodeID string
		attemptID                             sql.NullInt64
		payload                               []byte
	)
	err := d.QueryRow(`SELECT event_type, actor_type, actor_id, node_id, attempt_id, payload
		FROM audit_log WHERE node_execution_id = ?`, nodeExecID).
		Scan(&eventType, &actorType, &actorID, &nodeID, &attemptID, &payload)
	if err != nil {
		t.Fatalf("audit row missing: %v", err)
	}
	if eventType != "tool.call.denied" {
		t.Fatalf("event_type = %q", eventType)
	}
	if actorType != "user" || actorID != "auth_approver2" {
		t.Fatalf("actor = %s/%s", actorType, actorID)
	}
	if nodeID != "fetch" {
		t.Fatalf("node_id = %q", nodeID)
	}
	if attemptID.Valid {
		t.Fatalf("attempt_id = %v, want NULL", attemptID.Int64)
	}
	// JSON 列经 MySQL 规范化(键序/空白不定),解码后判而非子串匹配。
	var p audit.ToolCallDeniedPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("payload unmarshal: %v (raw=%s)", err, payload)
	}
	if p.Tool != "http" || len(p.ActorRoles) != 1 || p.ActorRoles[0] != "approver" {
		t.Fatalf("payload = %+v, want tool=http roles=[approver]", p)
	}
	if p.Reason != "role_mismatch" {
		t.Fatalf("reason = %q, want role_mismatch", p.Reason)
	}
}

// TestAuthorizeCallUnknownSubjectAuditedRolesEmpty:无用户行的主体被拒时
// actor_roles 为空数组(非 null——运维对账面确定性)。
func TestAuthorizeCallUnknownSubjectAuditedRolesEmpty(t *testing.T) {
	d := newDB(t)
	authz := newAuthorizer(t, d, DefaultToolPolicy())
	seed := seedExec(t, d, "auth_ghost")
	var nodeExecID uint64
	if err := d.QueryRow(`SELECT id FROM node_execution WHERE execution_id = ?`, seed.execID).Scan(&nodeExecID); err != nil {
		t.Fatalf("read node_exec id: %v", err)
	}
	if err := authz.AuthorizeCall(context.Background(), nodeExecID, "http"); !errors.Is(err, tools.ErrCallDenied) {
		t.Fatalf("err = %v, want ErrCallDenied", err)
	}
	var payload []byte
	if err := d.QueryRow(`SELECT payload FROM audit_log WHERE node_execution_id = ?`, nodeExecID).Scan(&payload); err != nil {
		t.Fatalf("audit row missing: %v", err)
	}
	var p audit.ToolCallDeniedPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("payload unmarshal: %v (raw=%s)", err, payload)
	}
	if p.ActorRoles == nil || len(p.ActorRoles) != 0 {
		t.Fatalf("actor_roles = %#v, want empty non-nil array", p.ActorRoles)
	}
	if p.Reason != "unknown_subject" {
		t.Fatalf("reason = %q, want unknown_subject", p.Reason)
	}
}

// TestAuthorizeCallInfraErrors:锚行缺失与 DB 故障都是基础设施失败(裸
// error,无 ErrCallDenied 哨兵)——不伪装成越权、不放行,attempt 留
// RUNNING 由重试接管。
func TestAuthorizeCallInfraErrors(t *testing.T) {
	d := newDB(t)
	authz := newAuthorizer(t, d, DefaultToolPolicy())

	t.Run("missing node_exec row", func(t *testing.T) {
		err := authz.AuthorizeCall(context.Background(), 999999, "http")
		if err == nil || errors.Is(err, tools.ErrCallDenied) {
			t.Fatalf("err = %v, want non-denial infra error", err)
		}
	})
	t.Run("db closed", func(t *testing.T) {
		closed, err := sql.Open("mysql", "root:invalid@tcp(127.0.0.1:1)/none")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = closed.Close() })
		if err := closed.Ping(); err == nil {
			t.Skip("unexpectedly reachable")
		}
		a2 := NewExecutionAuthorizer(closed, newIDs(), DefaultToolPolicy())
		err = a2.AuthorizeCall(context.Background(), 1, "http")
		if err == nil || errors.Is(err, tools.ErrCallDenied) {
			t.Fatalf("err = %v, want infra error", err)
		}
	})
}

// TestNewToolPolicyRejectsInvalid:策略构造防御——空默认集/未知角色/
// 空工具名/空角色集 = 构造期响亮失败(词表纪律:判定面变形即启动即现)。
func TestNewToolPolicyRejectsInvalid(t *testing.T) {
	cases := []struct {
		name     string
		defaults []Role
		perTool  map[string][]Role
	}{
		{"empty defaults", nil, nil},
		{"unknown default role", []Role{"superuser"}, nil},
		{"empty tool name", []Role{RoleAdmin}, map[string][]Role{"": {RoleAdmin}}},
		{"empty per-tool roles", []Role{RoleAdmin}, map[string][]Role{"http": {}}},
		{"unknown per-tool role", []Role{RoleAdmin}, map[string][]Role{"http": {"root"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewToolPolicy(tc.defaults, tc.perTool); err == nil {
				t.Fatal("NewToolPolicy accepted invalid policy")
			}
		})
	}
}

// TestNewExecutionAuthorizerNilPanics:nil 依赖 = 装配缺陷启动即现。
func TestNewExecutionAuthorizerNilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("nil db did not panic")
		}
	}()
	NewExecutionAuthorizer(nil, newIDs(), DefaultToolPolicy())
}

// 编译期接口满足锚(实现漂移即编译红)。
var _ tools.CallAuthorizer = (*ExecutionAuthorizer)(nil)
