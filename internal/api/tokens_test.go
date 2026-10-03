package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"braid/internal/auth"
	"braid/internal/metrics"
	"braid/internal/snowflake"
	"braid/internal/tools"
)

// issueTokenBody 构造 POST /api/v1/auth/tokens 请求体。
func issueTokenBody(t *testing.T, scopes []auth.Scope, ttlSecs any) []byte {
	t.Helper()
	m := map[string]any{}
	if scopes != nil {
		m["scopes"] = scopes
	}
	if ttlSecs != nil {
		m["ttl_seconds"] = ttlSecs
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// mintViaEndpoint 经端点签发并断言 200,返回响应体(失败即 Fatal)。
func mintViaEndpoint(t *testing.T, r *gin.Engine, token string, scopes []auth.Scope) map[string]any {
	t.Helper()
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/auth/tokens", token, issueTokenBody(t, scopes, nil))
	if code != http.StatusOK {
		t.Fatalf("issue token: code %d, body %v", code, body)
	}
	return body
}

// TestIssueTokenSubsetRoundTrip:author 请求单枚子集 → 200;签出的 token
// 经 ParseToken 回读:作用域恰为请求集、主体/租户透传(D-t131 决策 4)。
func TestIssueTokenSubsetRoundTrip(t *testing.T) {
	r, _, sign := newTestServer(t)
	body := mintViaEndpoint(t, r, sign(auth.RoleAuthor), []auth.Scope{auth.ScopeWorkflowsRead})

	tok, _ := body["token"].(string)
	if tok == "" || body["token_type"] != "Bearer" {
		t.Fatalf("token/token_type = %q/%v", tok, body["token_type"])
	}
	scopes, _ := body["scopes"].([]any)
	if len(scopes) != 1 || scopes[0] != string(auth.ScopeWorkflowsRead) {
		t.Fatalf("scopes echo = %v, want [workflows:read]", body["scopes"])
	}
	if body["expires_at"] == "" {
		t.Fatalf("expires_at missing: %v", body)
	}
	c, err := auth.ParseToken(testSecret, tok)
	if err != nil {
		t.Fatalf("ParseToken(minted): %v", err)
	}
	if len(c.Scopes) != 1 || c.Scopes[0] != auth.ScopeWorkflowsRead {
		t.Fatalf("minted scopes = %v, want [workflows:read]", c.Scopes)
	}
	if c.Subject != "alice" || c.Tenant != 0 {
		t.Fatalf("minted sub/tnt = %q/%d, want alice/0", c.Subject, c.Tenant)
	}
}

// TestIssueTokenAdminFull:admin 请求词表全量 → 200(默认集上界即全量)。
func TestIssueTokenAdminFull(t *testing.T) {
	r, _, sign := newTestServer(t)
	mintViaEndpoint(t, r, sign(auth.RoleAdmin), auth.AllScopes())
}

// TestIssueTokenNewVocabScopes:T17.1 词表 3→5 后的自窄化面——author
// 可窄化 workflows:write、approver 可窄化 approvals:write(其决定面
// 刚需);approver 请求 workflows:write = 超角色默认 → 400;旧窄 token
// (3 词表时代签发)不得经再签发获得新词表项(链式封堵对新枚同判)。
func TestIssueTokenNewVocabScopes(t *testing.T) {
	r, _, sign := newTestServer(t)
	mintViaEndpoint(t, r, sign(auth.RoleAuthor), []auth.Scope{auth.ScopeWorkflowsWrite})
	mintViaEndpoint(t, r, sign(auth.RoleApprover), []auth.Scope{auth.ScopeApprovalsWrite})

	code, body := doJSON(t, r, http.MethodPost, "/api/v1/auth/tokens", sign(auth.RoleApprover),
		issueTokenBody(t, []auth.Scope{auth.ScopeWorkflowsWrite}, nil))
	if code != http.StatusBadRequest || body["error"] != "invalid_scope" {
		t.Fatalf("approver widen to workflows:write = %d %v, want 400 invalid_scope", code, body)
	}

	parent, err := auth.SignTokenScoped(testSecret, time.Hour, "alice", []auth.Role{auth.RoleAuthor}, 0, "", []auth.Scope{auth.ScopeWorkflowsRead})
	if err != nil {
		t.Fatalf("sign parent: %v", err)
	}
	code, body = doJSON(t, r, http.MethodPost, "/api/v1/auth/tokens", parent,
		issueTokenBody(t, []auth.Scope{auth.ScopeApprovalsWrite}, nil))
	if code != http.StatusBadRequest || body["error"] != "invalid_scope" {
		t.Fatalf("old narrow token gains new scope = %d %v, want 400 invalid_scope", code, body)
	}
}

// TestIssueTokenScopeValidation:自窄化语义的拒绝面——超集(approver
// 请求 executions:write)/空集/缺席/词表外/重复一律 400 invalid_scope
// (只窄不宽 fail-closed,D-t131 决策 4)。
func TestIssueTokenScopeValidation(t *testing.T) {
	r, _, sign := newTestServer(t)
	approver := sign(auth.RoleApprover)
	cases := []struct {
		name   string
		token  string
		scopes []auth.Scope
	}{
		{"escalation beyond role defaults", approver, []auth.Scope{auth.ScopeExecutionsWrite}},
		{"empty set", approver, []auth.Scope{}},
		{"field absent", approver, nil},
		{"unknown scope", approver, []auth.Scope{"bogus:read"}},
		{"duplicate scope", approver, []auth.Scope{auth.ScopeWorkflowsRead, auth.ScopeWorkflowsRead}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doJSON(t, r, http.MethodPost, "/api/v1/auth/tokens", tc.token, issueTokenBody(t, tc.scopes, nil))
			if code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400", code)
			}
			if body["error"] != "invalid_scope" {
				t.Fatalf("error = %v, want invalid_scope", body["error"])
			}
		})
	}
}

// TestIssueTokenTTL:ttl_seconds 非正/超 MaxTokenTTL → 400 invalid_ttl;
// 合法值 3600 → 200 且 minted exp ≈ now+1h(分钟级容差)。
func TestIssueTokenTTL(t *testing.T) {
	r, _, sign := newTestServer(t)
	author := sign(auth.RoleAuthor)
	// 1<<33 秒:秒→Duration 换算溢出域(int64 纳秒上界 ≈ 2^63/1e9),旧守卫
	// 形态(先换算后比较)会被溢出负值绕过(评审 LOW)。
	for _, bad := range []any{0, -1, int64(auth.MaxTokenTTL/time.Second) + 1, int64(1) << 33} {
		code, body := doJSON(t, r, http.MethodPost, "/api/v1/auth/tokens", author, issueTokenBody(t, []auth.Scope{auth.ScopeWorkflowsRead}, bad))
		if code != http.StatusBadRequest {
			t.Fatalf("ttl %v: code = %d, want 400", bad, code)
		}
		if body["error"] != "invalid_ttl" {
			t.Fatalf("ttl %v: error = %v, want invalid_ttl", bad, body["error"])
		}
	}
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/auth/tokens", author, issueTokenBody(t, []auth.Scope{auth.ScopeWorkflowsRead}, 3600))
	if code != http.StatusOK {
		t.Fatalf("ttl 3600: code = %d, body %v", code, body)
	}
	c, err := auth.ParseToken(testSecret, body["token"].(string))
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	want := time.Now().Add(time.Hour)
	if c.ExpiresAt == nil || c.ExpiresAt.Sub(want) > time.Minute {
		t.Fatalf("exp = %v, want ~%v", c.ExpiresAt, want)
	}
}

// TestIssueTokenRequiresAuth:未认证/坏 token → 401(Authenticate 面,
// 端点自身零豁免)。
func TestIssueTokenRequiresAuth(t *testing.T) {
	r, _, _ := newTestServer(t)
	for name, tok := range map[string]string{"missing": "", "garbage": "not-a-token"} {
		t.Run(name, func(t *testing.T) {
			code, _ := doJSON(t, r, http.MethodPost, "/api/v1/auth/tokens", tok, issueTokenBody(t, []auth.Scope{auth.ScopeWorkflowsRead}, nil))
			if code != http.StatusUnauthorized {
				t.Fatalf("code = %d, want 401", code)
			}
		})
	}
}

// TestIssueTokenTenantPassthrough:签发者租户快照透传(tnt=7 的 token
// 签出子 token 仍携 7——租户维度不可经自窄化漂移,D-t131 决策 5)。
func TestIssueTokenTenantPassthrough(t *testing.T) {
	r, _, _ := newTestServer(t)
	tok, err := auth.SignTokenScoped(testSecret, time.Hour, "t7user", []auth.Role{auth.RoleAuthor}, 7, "", []auth.Scope{auth.ScopeWorkflowsRead})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	body := mintViaEndpoint(t, r, tok, []auth.Scope{auth.ScopeWorkflowsRead})
	c, err := auth.ParseToken(testSecret, body["token"].(string))
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	if c.Tenant != 7 || c.Subject != "t7user" {
		t.Fatalf("sub/tnt = %q/%d, want t7user/7", c.Subject, c.Tenant)
	}
}

// TestIssueTokenBadJSON:坏 JSON → 400 bad_request(readBodyLimited 单源
// 分界,超限 413 同源)。
func TestIssueTokenBadJSON(t *testing.T) {
	r, _, sign := newTestServer(t)
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/auth/tokens", sign(auth.RoleAuthor), []byte("{not-json"))
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", code)
	}
	if body["error"] != "bad_request" {
		t.Fatalf("error = %v, want bad_request", body["error"])
	}
}

// TestIssueTokenNarrowParentCannotWiden:链式扩权封堵(评审 HIGH)——
// 窄 token(scp ⊂ 角色默认)再签发只能取自身 scp 子集;请求落在角色默认
// 集内但超出父 scp → 400(泄露窄 token 经两跳重获写能力的链在此断)。
func TestIssueTokenNarrowParentCannotWiden(t *testing.T) {
	r, _, _ := newTestServer(t)
	parent, err := auth.SignTokenScoped(testSecret, time.Hour, "alice", []auth.Role{auth.RoleAuthor}, 0, "", []auth.Scope{auth.ScopeWorkflowsRead})
	if err != nil {
		t.Fatalf("sign parent: %v", err)
	}

	code, body := doJSON(t, r, http.MethodPost, "/api/v1/auth/tokens", parent,
		issueTokenBody(t, []auth.Scope{auth.ScopeExecutionsWrite}, nil))
	if code != http.StatusBadRequest || body["error"] != "invalid_scope" {
		t.Fatalf("widen = %d %v, want 400 invalid_scope", code, body)
	}

	code, body = doJSON(t, r, http.MethodPost, "/api/v1/auth/tokens", parent,
		issueTokenBody(t, []auth.Scope{auth.ScopeWorkflowsRead}, nil))
	if code != http.StatusOK {
		t.Fatalf("narrow ok = %d %v, want 200", code, body)
	}
	child, err := auth.ParseToken(testSecret, body["token"].(string))
	if err != nil {
		t.Fatalf("parse child: %v", err)
	}
	if len(child.Scopes) != 1 || child.Scopes[0] != auth.ScopeWorkflowsRead {
		t.Fatalf("child scopes = %v, want [workflows:read]", child.Scopes)
	}
}

// TestIssueTokenTTLCappedByParent:寿命外扩封堵(评审 HIGH)——子 TTL ≤
// 父剩余寿命(缺省 24h 与显式 3600 同被钳):无状态 JWT 无吊销面,TTL
// 即停用后暴露窗上界,再签发链不得外扩寿命。
func TestIssueTokenTTLCappedByParent(t *testing.T) {
	r, _, _ := newTestServer(t)
	parent, err := auth.SignTokenScoped(testSecret, 90*time.Second, "alice", []auth.Role{auth.RoleAuthor}, 0, "", []auth.Scope{auth.ScopeWorkflowsRead})
	if err != nil {
		t.Fatalf("sign parent: %v", err)
	}
	pc, err := auth.ParseToken(testSecret, parent)
	if err != nil || pc.ExpiresAt == nil {
		t.Fatalf("parse parent: %v", err)
	}
	for _, ttlReq := range []any{nil, 3600} {
		code, body := doJSON(t, r, http.MethodPost, "/api/v1/auth/tokens", parent,
			issueTokenBody(t, []auth.Scope{auth.ScopeWorkflowsRead}, ttlReq))
		if code != http.StatusOK {
			t.Fatalf("ttl %v: code %d body %v, want 200", ttlReq, code, body)
		}
		child, err := auth.ParseToken(testSecret, body["token"].(string))
		if err != nil || child.ExpiresAt == nil {
			t.Fatalf("parse child: %v", err)
		}
		// 2s 宽容:签发流水线的 now 漂移。
		if child.ExpiresAt.After(pc.ExpiresAt.Add(2 * time.Second)) {
			t.Fatalf("ttl %v: child exp %v outlives parent exp %v", ttlReq, child.ExpiresAt, pc.ExpiresAt)
		}
	}
}

// TestIssueTokenAuditRowAndMetricPurity(T17.2 验收面):签发成功落
// scoped_token_issued 审计行——主体(username 列)/作用域(comma-join,
// 请求序保持)/TTL(clamp 后实际授予值:显式 60 原样、超父剩余回收到
// 父剩余秒)/token 哈希(SHA-256 hex,对返回 token 可复算——原值绝不
// 入行,铁律);method/reason 恒 NULL(chk_login_audit_method 的 Go 侧
// 同判)。指标纯度:登录四 label 计数全零——登录指标是登录判定的观测
// 半边,签发计数混入会把 label 词面污染成不可归因混合体。
func TestIssueTokenAuditRowAndMetricPurity(t *testing.T) {
	dbh := newLoginDefenseDB(t)
	cfg := Config{Addr: DefaultAddr, JWTSecret: testSecret, SnowflakeNode: snowflake.Explicit(1),
		LoginRateLimit: 100000, LoginRateWindow: time.Minute,
		LoginMaxFailures: 100000, LoginLockout: time.Minute}
	set, _ := metrics.NewSet()
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	reg, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	gin.SetMode(gin.TestMode)
	srv := NewServer(cfg, dbh, ids, reg, fakeTrigger{id: 1}, auth.BuiltinProvider{DB: dbh})
	srv.loginMetrics = set.Login
	r := srv.Router()

	parent, err := auth.SignTokenScoped(testSecret, time.Hour, "alice",
		[]auth.Role{auth.RoleAuthor}, 0, "", []auth.Scope{auth.ScopeWorkflowsRead, auth.ScopeExecutionsWrite})
	if err != nil {
		t.Fatalf("sign parent: %v", err)
	}

	// 第一枚:显式 ttl=60(远小于父剩余,不被 clamp)。
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/auth/tokens", parent,
		issueTokenBody(t, []auth.Scope{auth.ScopeWorkflowsRead, auth.ScopeExecutionsWrite}, 60))
	if code != http.StatusOK {
		t.Fatalf("issue #1: code %d body %v", code, body)
	}
	tok1, _ := body["token"].(string)
	sum := sha256.Sum256([]byte(tok1))

	// 第二枚:ttl = MaxTokenTTL(604800,守卫上界)且超父剩余(1h)→
	// 行里记 clamp 后实际授予值(≈3600,秒截断容 3599)。
	code, body = doJSON(t, r, http.MethodPost, "/api/v1/auth/tokens", parent,
		issueTokenBody(t, []auth.Scope{auth.ScopeExecutionsWrite}, int64(auth.MaxTokenTTL/time.Second)))
	if code != http.StatusOK {
		t.Fatalf("issue #2: code %d body %v", code, body)
	}

	rows, err := dbh.Query(`SELECT event_type, IFNULL(method,''), username, client_ip,
	    scopes, ttl_seconds, token_hash, IFNULL(reason,'')
	  FROM login_audit WHERE event_type = 'scoped_token_issued' ORDER BY id`)
	if err != nil {
		t.Fatalf("query issuance rows: %v", err)
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // 测试收尾
	var got [][]any
	for rows.Next() {
		var ev, method, user, ip, scopes, hash, reason string
		var ttl uint32
		if err := rows.Scan(&ev, &method, &user, &ip, &scopes, &ttl, &hash, &reason); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, []any{ev, method, user, ip, scopes, ttl, hash, reason})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("scoped_token_issued rows = %d, want 2(拒绝路径零行): %v", len(got), got)
	}
	// 行 1:显式 TTL 原样 + 双作用域请求序保持 + 哈希可复算 + method/reason 恒空。
	r1 := got[0]
	if r1[0] != "scoped_token_issued" || r1[1] != "" || r1[2] != "alice" || r1[3] == "" {
		t.Fatalf("row1 形状: %v", r1)
	}
	if r1[4] != "workflows:read,executions:write" {
		t.Fatalf("row1 scopes = %v, want 请求序 comma-join", r1[4])
	}
	if r1[5] != uint32(60) {
		t.Fatalf("row1 ttl = %v, want 60(显式值不被 clamp)", r1[5])
	}
	if r1[6] != hex.EncodeToString(sum[:]) {
		t.Fatalf("row1 token_hash 不可复算: %v", r1[6])
	}
	if r1[7] != "" {
		t.Fatalf("row1 reason = %v, want 恒 NULL", r1[7])
	}
	// 行 2:clamp 后实际授予值(父剩余 ≈3600,秒截断容 3599)。
	if ttl := got[1][5].(uint32); ttl < 3599 || ttl > 3600 {
		t.Fatalf("row2 ttl = %d, want clamp 至父剩余 [3599,3600]", ttl)
	}

	// 指标纯度:签发两枚后登录四 label 全零。
	for _, label := range []string{metrics.ResultSucceeded, metrics.ResultFailed,
		metrics.ResultLocked, metrics.ResultRateLimited} {
		if c := testutil.ToFloat64(set.Login.Attempts.WithLabelValues(label)); c != 0 {
			t.Errorf("login_attempts{%s} = %v, want 0(签发不进登录指标)", label, c)
		}
	}
}

// TestIssueTokenSubSecondParentRejected(评审 LOW 修的防回退锚):父 token
// 剩余 < 1s 时签发面前置拒绝 401 token_expired——若不拦,亚秒剩余经
// clamp 会签出亚秒 token,其审计行 ttl_seconds 秒截断为 0 被
// writeScopedTokenAudit 防御闸拒收,「每次签发必落行」契约出现确定性
// 缺口(200 签出零审计痕迹)。与 Authenticate 过期路径同映射,断言
// 确定性不受请求处理时序影响(两路皆 401 token_expired)。
func TestIssueTokenSubSecondParentRejected(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	if _, err := dbh.Exec(`DELETE FROM login_audit WHERE event_type = 'scoped_token_issued'`); err != nil {
		t.Fatalf("pre-clean: %v", err)
	}
	t.Cleanup(func() {
		_, _ = dbh.Exec(`DELETE FROM login_audit WHERE event_type = 'scoped_token_issued'`)
	})
	parent, err := auth.SignTokenScoped(testSecret, 300*time.Millisecond, "alice",
		[]auth.Role{auth.RoleAuthor}, 0, "", []auth.Scope{auth.ScopeWorkflowsRead})
	if err != nil {
		t.Fatalf("sign parent: %v", err)
	}
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/auth/tokens", parent,
		issueTokenBody(t, []auth.Scope{auth.ScopeWorkflowsRead}, nil))
	if code != http.StatusUnauthorized || body["error"] != "token_expired" {
		t.Fatalf("code = %d body %v, want 401 token_expired(亚秒剩余 = 事实过期)", code, body)
	}
	if n := loginAuditRows(t, dbh); n != 0 {
		t.Fatalf("login_audit rows = %d, want 0(拒绝路径零签发行)", n)
	}
}
