package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"braid/internal/audit"
	"braid/internal/auth"
)

// scpPolicy 是 authed 组端点 × 作用域执法矩阵的声明表(T17.1 立,
// D-2026-10-01-t171-rest-scp;T20.2/D-2026-10-07-t202-scp-denied-audit 起
// 执法移 handler 层,本表升格为矩阵真源——handler 内 requireScope 调用
// 必须与表同步旅行)。route = gin 路由模式(枚举完备性比对用),probe =
// 实参化探针路径(行为反证用)。两道断言双向锁死:
//   - 枚举完备:新路由漏挂 scp 闸 = gin 路由集多出表外项即红;
//   - 行为反证:表项漏挂闸 = 交叉积探针不 403 即红。
//
// scope 为空串 = 豁免(仅 POST /auth/tokens:自窄化构造性单调收窄,
// 挂闸反断链式窄化特性,见 server.go 注释)。body 非空 = 探针体覆盖位
// (T20.2:闸在 handler 形状闸之后,rollback/decision 探针须携形状合法体
// 才能触达 scp 闸——D-t202 决策 3 登记的探针合法化)。
type scpPolicy struct {
	method string
	route  string
	probe  string
	scope  auth.Scope
	body   string
}

var scpPolicies = []scpPolicy{
	{http.MethodPost, "/api/v1/auth/tokens", "/api/v1/auth/tokens", "", ""},
	{http.MethodPost, "/api/v1/definitions", "/api/v1/definitions", auth.ScopeWorkflowsWrite, ""},
	{http.MethodGet, "/api/v1/definitions", "/api/v1/definitions", auth.ScopeWorkflowsRead, ""},
	{http.MethodGet, "/api/v1/definitions/:name", "/api/v1/definitions/scp-probe-def", auth.ScopeWorkflowsRead, ""},
	{http.MethodGet, "/api/v1/definitions/:name/versions", "/api/v1/definitions/scp-probe-def/versions", auth.ScopeWorkflowsRead, ""},
	{http.MethodGet, "/api/v1/definitions/:name/versions/:version", "/api/v1/definitions/scp-probe-def/versions/1", auth.ScopeWorkflowsRead, ""},
	{http.MethodPost, "/api/v1/definitions/:name/versions/:version/archive", "/api/v1/definitions/scp-probe-def/versions/1/archive", auth.ScopeWorkflowsWrite, ""},
	{http.MethodGet, "/api/v1/drafts/:id", "/api/v1/drafts/1", auth.ScopeWorkflowsRead, ""},
	{http.MethodPut, "/api/v1/drafts/:id", "/api/v1/drafts/1", auth.ScopeWorkflowsWrite, ""},
	{http.MethodPost, "/api/v1/drafts/:id/publish", "/api/v1/drafts/1/publish", auth.ScopeWorkflowsWrite, ""},
	{http.MethodPost, "/api/v1/executions", "/api/v1/executions", auth.ScopeExecutionsWrite, ""},
	{http.MethodGet, "/api/v1/executions", "/api/v1/executions", auth.ScopeExecutionsRead, ""},
	{http.MethodGet, "/api/v1/executions/:id", "/api/v1/executions/1", auth.ScopeExecutionsRead, ""},
	{http.MethodPost, "/api/v1/executions/:id/rollback", "/api/v1/executions/1/rollback", auth.ScopeExecutionsWrite, `{"node_id":"n1"}`},
	{http.MethodPost, "/api/v1/executions/:id/resume", "/api/v1/executions/1/resume", auth.ScopeExecutionsWrite, ""},
	{http.MethodPost, "/api/v1/executions/:id/cancel", "/api/v1/executions/1/cancel", auth.ScopeExecutionsWrite, ""},
	// T18.4 投影只读三端点(按路径/按轮列表/单轮详情)——executions:read。
	{http.MethodGet, "/api/v1/executions/:id/nodes/:node_id", "/api/v1/executions/1/nodes/n1", auth.ScopeExecutionsRead, ""},
	{http.MethodGet, "/api/v1/executions/:id/nodes/:node_id/attempts", "/api/v1/executions/1/nodes/n1/attempts", auth.ScopeExecutionsRead, ""},
	{http.MethodGet, "/api/v1/executions/:id/nodes/:node_id/attempts/:round", "/api/v1/executions/1/nodes/n1/attempts/1", auth.ScopeExecutionsRead, ""},
	{http.MethodGet, "/api/v1/approvals/pending", "/api/v1/approvals/pending", auth.ScopeExecutionsRead, ""},
	{http.MethodGet, "/api/v1/executions/:id/approvals/:node_id", "/api/v1/executions/1/approvals/n1", auth.ScopeExecutionsRead, ""},
	{http.MethodPost, "/api/v1/executions/:id/approvals/:node_id/decision", "/api/v1/executions/1/approvals/n1/decision", auth.ScopeApprovalsWrite, `{"decision":"approve"}`},
	{http.MethodGet, "/api/v1/audit", "/api/v1/audit", auth.ScopeExecutionsRead, ""},
}

// scpUnauthedAPIRoutes 是 /api/v1 下不挂 Authenticate 的组外面(凭据域
// 各异:口令换 token / HMAC capability / OIDC redirect——scp 无管辖面)。
// OIDC 两路由条件挂载(未配置不注册),在此登记防挂载后漏认知。
// 新增组外 /api/v1 路由 = 有意识的显式扩本表,静默漏挂会被枚举断言拦下。
var scpUnauthedAPIRoutes = map[string]bool{
	"POST /api/v1/auth/login":        true,
	"GET /api/v1/im/approval":        true,
	"POST /api/v1/im/approval":       true, // T17.5 确认页第二跳提交面,同 GET 凭据域(HMAC capability)
	"GET /api/v1/auth/oidc/login":    true,
	"GET /api/v1/auth/oidc/callback": true,
}

// signScopedProbe 签发探针 token:admin 主体(过一切角色闸——探针的
// 403 只能来自 scp 闸,归因无歧义)+ 指定作用域集。
func signScopedProbe(t *testing.T, scopes ...auth.Scope) string {
	t.Helper()
	tok, err := auth.SignTokenScoped(testSecret, time.Hour, "scp-probe", []auth.Role{auth.RoleAdmin}, 0, "", scopes)
	if err != nil {
		t.Fatalf("sign scoped probe: %v", err)
	}
	return tok
}

// signAbsentProbe 签发 scp 缺席探针(存量/手签形态:claim 不携带)。
func signAbsentProbe(t *testing.T) string {
	t.Helper()
	tok, err := auth.SignToken(testSecret, time.Hour, "scp-probe", []auth.Role{auth.RoleAdmin}, 0)
	if err != nil {
		t.Fatalf("sign absent probe: %v", err)
	}
	return tok
}

// probeBody 给探针请求构造载荷:POST 族默认空 JSON,行级 body 覆盖位
// 优先(T20.2:闸在 handler 形状闸之后,形状非法体会在触达 scp 闸前吃
// 400——rollback/decision 两行携形状合法体);GET 用 nil。
func probeBody(p scpPolicy) []byte {
	if p.method == http.MethodGet {
		return nil
	}
	if p.body != "" {
		return []byte(p.body)
	}
	return []byte("{}")
}

// TestSCPPolicyEnumeratesAllAuthedRoutes 枚举完备性(对抗口径①:漏一个
// 端点 = 执法面缺口):gin 实注册的 /api/v1 路由集,扣除显式登记的
// 组外面,必须与策略表集合相等——表外多路由(新端点未挂闸)与表内
// 失路由(删端点忘更表)双向红。
func TestSCPPolicyEnumeratesAllAuthedRoutes(t *testing.T) {
	r, _, _ := newTestServer(t)
	got := map[string]bool{}
	for _, ri := range r.Routes() {
		if !strings.HasPrefix(ri.Path, "/api/v1") {
			continue // /healthz、/ui/*、/mcp 各自凭证域
		}
		key := ri.Method + " " + ri.Path
		if scpUnauthedAPIRoutes[key] {
			continue
		}
		got[key] = true
	}
	want := map[string]bool{}
	for _, p := range scpPolicies {
		key := p.method + " " + p.route
		if want[key] {
			t.Fatalf("duplicate policy entry: %s", key)
		}
		want[key] = true
	}
	for key := range want {
		if !got[key] {
			t.Errorf("policy %s not registered in router (表内失路由)", key)
		}
	}
	for key := range got {
		if !want[key] {
			t.Errorf("authed route %s missing scp policy (新路由漏挂执法——扩策略表或显式豁免)", key)
		}
	}
}

// TestSCPCrossProductDeny 对抗口径②:逐端点全交叉积反证——admin 探针
// 携「词表 ∖ 所需」(非空,恒可构造)访问必 403 forbidden。403 归因:
// admin 过一切角色闸,唯一拦截面 = handler 内 scp 闸(T20.2 起闸在
// 形状闸之后——探针体经 probeBody 合法化后触达)。豁免端点跳过(无可
// 反证的作用域)。
func TestSCPCrossProductDeny(t *testing.T) {
	r, _, _ := newTestServer(t)
	for _, p := range scpPolicies {
		if p.scope == "" {
			continue
		}
		var wrong []auth.Scope
		for _, s := range auth.AllScopes() {
			if s != p.scope {
				wrong = append(wrong, s)
			}
		}
		code, body := doJSON(t, r, p.method, p.probe, signScopedProbe(t, wrong...), probeBody(p))
		if code != http.StatusForbidden {
			t.Errorf("%s %s: wrong-scope probe = %d %v, want 403 (执法缺口)", p.method, p.route, code, body)
		}
	}
}

// TestSCPExactScopePasses 对抗口径③:携恰所需作用域 → 非 403(admin
// 探针过闸后落 handler 域,状态码归 handler 语义:200/400/404 均证
// scp 闸未拦)。豁免端点(对抗审计 MEDIUM 修:单枚 wfr 探针测不出
// 「被挂了 read 闸」的变异)以词表逐枚单作用域探针全扫——给豁免行
// 挂任何一枚作用域闸都至少杀掉四枚探针(持其余作用域者 403),
// 豁免不变量(链式自窄化不得被作用域闸打断)由此钉死。
func TestSCPExactScopePasses(t *testing.T) {
	r, _, _ := newTestServer(t)
	for _, p := range scpPolicies {
		probes := [][]auth.Scope{{p.scope}}
		if p.scope == "" {
			probes = nil
			for _, s := range auth.AllScopes() {
				probes = append(probes, []auth.Scope{s})
			}
		}
		for _, scopes := range probes {
			code, _ := doJSON(t, r, p.method, p.probe, signScopedProbe(t, scopes...), probeBody(p))
			if code == http.StatusForbidden {
				t.Errorf("%s %s: probe scp=%v = 403, want non-403 (过闸/豁免破裂)", p.method, p.route, scopes)
			}
		}
	}
}

// TestSCPAbsentPassThrough 现状兼容钉死(issue #118 裁定):scp 缺席 =
// 全角色凭证,角色闸独挑授权,全端点透传非 403——REST 面 100% 存量
// 兼容(与 MCP 面「缺席 = 零权限 fail-closed」刻意不同面,双面语义
// 见 requireScope 注释)。
func TestSCPAbsentPassThrough(t *testing.T) {
	r, _, _ := newTestServer(t)
	absent := signAbsentProbe(t)
	for _, p := range scpPolicies {
		code, _ := doJSON(t, r, p.method, p.probe, absent, probeBody(p))
		if code == http.StatusForbidden {
			t.Errorf("%s %s: absent-scp probe = 403, want pass-through (现状兼容破裂)", p.method, p.route)
		}
	}
}

// TestSCPApproverLoginShapedZeroRegression login 形态零回退(approver 默认集
// 扩展的刚需面):approver 携新默认集 [workflows:read, executions:read,
// approvals:write] 全端点扫——作用域在其默认集内的端点非 403(读面与
// 审批决定面行为不变),要求 wfw/exw 的端点 403(执法前同为角色闸
// 403,可见面等价)。
func TestSCPApproverLoginShapedZeroRegression(t *testing.T) {
	r, _, _ := newTestServer(t)
	defaults := auth.DefaultScopesForRoles([]auth.Role{auth.RoleApprover})
	tok, err := auth.SignTokenScoped(testSecret, time.Hour, "approver-probe", []auth.Role{auth.RoleApprover}, 0, "", defaults)
	if err != nil {
		t.Fatalf("sign approver probe: %v", err)
	}
	for _, p := range scpPolicies {
		inDefaults := p.scope == ""
		for _, s := range defaults {
			if s == p.scope {
				inDefaults = true
			}
		}
		code, _ := doJSON(t, r, p.method, p.probe, tok, probeBody(p))
		if inDefaults && code == http.StatusForbidden {
			t.Errorf("%s %s: approver login-shaped token = 403, want non-403 (默认集内端点回退)", p.method, p.route)
		}
		if !inDefaults && code != http.StatusForbidden {
			t.Errorf("%s %s: approver on wfw/exw endpoint = %d, want 403 (执法前后可见面等价)", p.method, p.route, code)
		}
	}
}

// TestSCPDeniedAuditedOnActionRoutes 对抗口径⑥(T20.2,D-2026-10-07-t202):
// scoped 拒绝的审计半边——4 条执行锚 action 路由(rollback/resume/cancel/
// decision)上 wrong-scope 探针 403 forbidden 的同时必须落既有 denied 事件族
// 行(reason=scope_denied,零新事件词);幻影 execID(跨租户/不存在同判)
// 403 但零行(execVisibleInTenant 落行闸——不注入受害者证据链)。resume
// 与 rollback 共用 handleRollback → 同落 rollback.denied,**独立种执行**
// (对抗审计 MEDIUM 修:共执行时 resume 臂的 QueryRow 被 rollback 臂
// 已落行顶替,删除 resume 写行的变异全绿——哑绿);decision 锚定
// 要求真实挂起 approval 节点(writeDecisionDeniedAudit 既有防灌水锚)。
func TestSCPDeniedAuditedOnActionRoutes(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	// 种执行:四臂各一棵(落行闸只看租户可见性,状态无关;分种 =
	// 逐臂断言互不遮蔽);decision 种真挂起 approval 节点(快照在册)。
	rbExecID, _ := seedSuspendedApproval(t, dbh, "RUNNING", "PENDING", "carol")
	rsExecID, _ := seedSuspendedApproval(t, dbh, "RUNNING", "PENDING", "carol")
	cxExecID, _ := seedSuspendedApproval(t, dbh, "RUNNING", "PENDING", "carol")
	decExecID, _ := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol")
	// admin 探针携 [wfr](缺 exw/apw):403 归因唯一 = scp 闸。
	noWrite := signScopedProbe(t, auth.ScopeWorkflowsRead)

	probes := []struct {
		name      string
		path      string
		body      []byte
		eventType string
		execID    uint64
	}{
		{"rollback", fmt.Sprintf("/api/v1/executions/%d/rollback", rbExecID),
			[]byte(`{"node_id":"n1"}`), audit.EventRollbackDenied, rbExecID},
		{"resume", fmt.Sprintf("/api/v1/executions/%d/resume", rsExecID),
			[]byte(`{}`), audit.EventRollbackDenied, rsExecID},
		{"cancel", fmt.Sprintf("/api/v1/executions/%d/cancel", cxExecID),
			nil, audit.EventCancelDenied, cxExecID},
		{"decision", decisionPath(decExecID),
			[]byte(`{"decision":"approve"}`), audit.EventApprovalDecisionDenied, decExecID},
	}
	for _, pr := range probes {
		code, body := doJSON(t, r, http.MethodPost, pr.path, noWrite, pr.body)
		if code != http.StatusForbidden || body["error"] != "forbidden" {
			t.Fatalf("%s: wrong-scope probe = %d %v, want 403 forbidden", pr.name, code, body)
		}
		if n := countRows(t, dbh, `SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND event_type=? AND actor_id='scp-probe'`,
			pr.execID, pr.eventType); n != 1 {
			t.Fatalf("%s: scope_denied audit rows = %d, want exactly 1", pr.name, n)
		}
		var reason string
		if err := dbh.QueryRow(`SELECT JSON_UNQUOTE(JSON_EXTRACT(payload,'$.reason'))
			FROM audit_log WHERE execution_id=? AND event_type=? AND actor_id='scp-probe'`,
			pr.execID, pr.eventType).Scan(&reason); err != nil || reason != "scope_denied" {
			t.Fatalf("%s: scope_denied audit row reason = %q (%v)", pr.name, reason, err)
		}
	}

	// 闸位反证锚(对抗审计 MEDIUM 修:D-t202 决策 3 的让步方向——形状闸
	// 先于 scp 闸——此前零锚,把闸挪回首行的变异六道断言全绿)。形状
	// 非法体 × wrong-scope:400 形状分面先裁,不 403、零审计行。
	code, body := doJSON(t, r, http.MethodPost,
		fmt.Sprintf("/api/v1/executions/%d/rollback", rbExecID), noWrite, []byte(`{}`))
	if code != http.StatusBadRequest || body["error"] != "node_id_required" {
		t.Fatalf("shape-invalid × wrong-scope = %d %v, want 400 node_id_required (形状闸先于 scp 闸)", code, body)
	}
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND actor_id='scp-probe' AND event_type='rollback.denied'`,
		rbExecID); n != 1 {
		t.Fatalf("shape-invalid denial rows = %d, want 1(400 面不落 scope_denied 行;恰 1 = 前臂 rollback 行)", n)
	}

	// 幻影执行:403 裁决照常,零行(跨租户/不存在同判,D-t112 落行闸)。
	code, body = doJSON(t, r, http.MethodPost,
		"/api/v1/executions/4242424242/rollback", noWrite, []byte(`{"node_id":"n1"}`))
	if code != http.StatusForbidden {
		t.Fatalf("phantom rollback = %d %v, want 403", code, body)
	}
	if n := countRows(t, dbh, `SELECT COUNT(*) FROM audit_log WHERE execution_id=4242424242`); n != 0 {
		t.Fatalf("phantom rollback audit rows = %d, want 0", n)
	}
}
