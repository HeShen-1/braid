package api

// IM 鉴权短链端点测试(T14.3,D-t143 决策 2/4):authed 组外路由(零
// Authorization)、capability 验证(过期/篡改/词面外)、决定事务单点消费
//(via=im_link 审计、any/all 定局语义、resume 事件、重放/冲突/已决/
// 轮次过)、通道关闭 404、结果页安全头。t13 真 MySQL,不可达自动 skip。
//
// 变异自查:via 归因丢失 → 审计断言红;轮绑定守卫移除 → 轮次过用例红;
// 短链绕过名单门 → mallory 用例红;定局守卫对短链入口失效 → 已决用例红。

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"braid/internal/auth"
	"braid/internal/db/sqltest"
	"braid/internal/snowflake"
	"braid/internal/tools"
)

const imLinkAPISecret = "braid-im-link-api-secret-0123456789"

// newIMTestServer:IM 短链面专用 server(密钥注入;登录防御旋钮放宽同
// newTestServer——本面不经登录,旋钮纯防串扰)。
func newIMTestServer(t *testing.T) *gin.Engine {
	t.Helper()
	return newIMTestServerOpt(t, imLinkAPISecret)
}

func newIMTestServerOpt(t *testing.T, linkSecret string) *gin.Engine {
	t.Helper()
	dbh := sqltest.MustOpenAt(t, "braid_api_test")
	cleanExecutionChain(t, dbh)
	ids, err := snowflake.New(11)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	cfg := Config{Addr: DefaultAddr, JWTSecret: testSecret, SnowflakeNode: 11,
		LoginRateLimit: 100000, LoginRateWindow: time.Minute,
		LoginMaxFailures: 100000, LoginLockout: time.Minute,
		IMLinkSecret: linkSecret}
	reg, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	gin.SetMode(gin.TestMode)
	return NewServer(cfg, dbh, ids, reg, fakeTrigger{id: 1}, auth.BuiltinProvider{DB: dbh}).Router()
}

// imLinkPath 签发一张按钮短链(seed 恒 node_id=boss、round=1;exp 不可自定
// ——过期链接用 imLinkExpiredAt 直改轮次/时间由 SignIMLink 语义出)。
func imLinkPath(t *testing.T, execID, nodeExecID uint64, approver, action string) string {
	t.Helper()
	d, s, err := auth.SignIMLink(imLinkAPISecret, auth.IMLinkPayload{
		ExecutionID: execID, NodeExecID: nodeExecID, NodeID: "boss",
		Approver: approver, Round: 1, Action: action,
	}, time.Now())
	if err != nil {
		t.Fatalf("sign im link: %v", err)
	}
	return "/api/v1/im/approval?d=" + d + "&s=" + s
}

// imLinkAt 是时间可控签发(过期臂:now 回拨 TTL+1m)。
func imLinkAt(t *testing.T, execID, nodeExecID uint64, approver, action string, now time.Time) string {
	t.Helper()
	d, s, err := auth.SignIMLink(imLinkAPISecret, auth.IMLinkPayload{
		ExecutionID: execID, NodeExecID: nodeExecID, NodeID: "boss",
		Approver: approver, Round: 1, Action: action,
	}, now)
	if err != nil {
		t.Fatalf("sign im link: %v", err)
	}
	return "/api/v1/im/approval?d=" + d + "&s=" + s
}

// doIMGet 发无 Authorization 的 GET(短链凭据 = URL 本身)。
func doIMGet(t *testing.T, r *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func imBodyWant(t *testing.T, w *httptest.ResponseRecorder, status int, want ...string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("code = %d want %d; body: %s", w.Code, status, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q", ct)
	}
	for _, s := range want {
		if !strings.Contains(w.Body.String(), s) {
			t.Fatalf("body missing %q: %s", s, w.Body.String())
		}
	}
}

// imDecidedRows 数本执行 approval.decided 行的 via/decision 词面分布。
func imDecidedRows(t *testing.T, dbh *sql.DB, execID uint64) []map[string]any {
	t.Helper()
	rows, err := dbh.Query(`
SELECT JSON_UNQUOTE(JSON_EXTRACT(payload, '$.via')), JSON_UNQUOTE(JSON_EXTRACT(payload, '$.decision'))
  FROM audit_log WHERE execution_id=? AND event_type='approval.decided' ORDER BY id`, execID)
	if err != nil {
		t.Fatalf("query decided: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var via, decision sql.NullString
		if err := rows.Scan(&via, &decision); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, map[string]any{"via": via.String, "decision": decision.String})
	}
	return out
}

func imDecisionCount(t *testing.T, dbh *sql.DB, execID uint64) int {
	var n int
	if err := dbh.QueryRow(`SELECT COUNT(*) FROM approval_decision WHERE execution_id=?`, execID).Scan(&n); err != nil {
		t.Fatalf("count decisions: %v", err)
	}
	return n
}

func imResumeCount(t *testing.T, dbh *sql.DB, execID uint64) int {
	var n int
	if err := dbh.QueryRow(`SELECT COUNT(*) FROM outbox WHERE execution_id=? AND event_type='execution.resume'`, execID).Scan(&n); err != nil {
		t.Fatalf("count resume events: %v", err)
	}
	return n
}

// TestIMLinkApprovalAppliesDecision:any 模式首票 = 定局:短链 GET 落票 +
// via=im_link 审计 + resume 事件 + 结果页安全头。
func TestIMLinkApprovalAppliesDecision(t *testing.T) {
	r := newIMTestServer(t)
	dbh := sqltest.MustOpenAt(t, "braid_api_test")
	execID, nodeExecID := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol")

	w := doIMGet(t, r, imLinkPath(t, execID, nodeExecID, "carol", "approve"))
	imBodyWant(t, w, http.StatusOK, "投票已记录", "carol 已批准", "已定局")

	if got := w.Header().Get("Content-Security-Policy"); got != "default-src 'none'" {
		t.Fatalf("csp = %q", got)
	}
	if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("xfo = %q", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache-control = %q", got)
	}
	if n := imDecisionCount(t, dbh, execID); n != 1 {
		t.Fatalf("decision rows = %d", n)
	}
	rows := imDecidedRows(t, dbh, execID)
	if len(rows) != 1 || rows[0]["via"] != "im_link" || rows[0]["decision"] != "approve" {
		t.Fatalf("decided audit rows = %+v", rows)
	}
	if n := imResumeCount(t, dbh, execID); n != 1 {
		t.Fatalf("resume events = %d(定局票必须产生)", n)
	}
	var decidedBy, dec string
	if err := dbh.QueryRow(`SELECT decided_by, decision FROM approval_decision WHERE execution_id=?`, execID).
		Scan(&decidedBy, &dec); err != nil {
		t.Fatalf("read decision: %v", err)
	}
	if decidedBy != "carol" || dec != "APPROVED" {
		t.Fatalf("decision = %s/%s", decidedBy, dec)
	}
}

// TestIMLinkAllModeIntermediateThenFinal:会签中间态票零续跑意图,末票
// 定局才 resume(D-t142 契约经短链入口不变形)。
func TestIMLinkAllModeIntermediateThenFinal(t *testing.T) {
	r := newIMTestServer(t)
	dbh := sqltest.MustOpenAt(t, "braid_api_test")
	execID, nodeExecID := seedSuspendedApprovalMode(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "all", "carol", "dave")

	w := doIMGet(t, r, imLinkPath(t, execID, nodeExecID, "carol", "approve"))
	imBodyWant(t, w, http.StatusOK, "等待其余审批人投票")
	if n := imResumeCount(t, dbh, execID); n != 0 {
		t.Fatalf("intermediate vote must not resume: %d", n)
	}

	w = doIMGet(t, r, imLinkPath(t, execID, nodeExecID, "dave", "approve"))
	imBodyWant(t, w, http.StatusOK, "本轮已定局")
	if n := imResumeCount(t, dbh, execID); n != 1 {
		t.Fatalf("final vote must resume: %d", n)
	}
	if n := imDecisionCount(t, dbh, execID); n != 2 {
		t.Fatalf("decisions = %d", n)
	}
}

// TestIMLinkReplayAndConflict:同票重放 = 200 零新写;异票 = 409 改判面。
func TestIMLinkReplayAndConflict(t *testing.T) {
	r := newIMTestServer(t)
	dbh := sqltest.MustOpenAt(t, "braid_api_test")
	execID, nodeExecID := seedSuspendedApprovalMode(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "all", "carol", "dave")

	approve := imLinkPath(t, execID, nodeExecID, "carol", "approve")
	doIMGet(t, r, approve)
	w := doIMGet(t, r, approve)
	imBodyWant(t, w, http.StatusOK, "已投过票", "未产生新写入")
	if n := imDecisionCount(t, dbh, execID); n != 1 {
		t.Fatalf("replay must not write: %d", n)
	}

	w = doIMGet(t, r, imLinkPath(t, execID, nodeExecID, "carol", "reject"))
	imBodyWant(t, w, http.StatusConflict, "投票冲突", "不可改判")
	if n := imDecisionCount(t, dbh, execID); n != 1 {
		t.Fatalf("conflict must not write: %d", n)
	}
}

// TestIMLinkAlreadyDecided:any 首票定局后,第二人短链 → 409 已定局。
func TestIMLinkAlreadyDecided(t *testing.T) {
	r := newIMTestServer(t)
	dbh := sqltest.MustOpenAt(t, "braid_api_test")
	execID, nodeExecID := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol", "dave")

	doIMGet(t, r, imLinkPath(t, execID, nodeExecID, "carol", "approve"))
	w := doIMGet(t, r, imLinkPath(t, execID, nodeExecID, "dave", "approve"))
	imBodyWant(t, w, http.StatusConflict, "已定局")
	if n := imDecisionCount(t, dbh, execID); n != 1 {
		t.Fatalf("post-final vote must not write: %d", n)
	}
}

// TestIMLinkRoundStale:链接签发于第 1 轮、节点已进第 2 轮(回滚重挂)
// ⇒ 409 轮次过,零落票(撤销语义细粒度半边)。
func TestIMLinkRoundStale(t *testing.T) {
	r := newIMTestServer(t)
	dbh := sqltest.MustOpenAt(t, "braid_api_test")
	execID, nodeExecID := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol")
	if _, err := dbh.Exec(`UPDATE node_execution SET attempts=2 WHERE id=?`, nodeExecID); err != nil {
		t.Fatalf("bump attempts: %v", err)
	}
	w := doIMGet(t, r, imLinkPath(t, execID, nodeExecID, "carol", "approve"))
	imBodyWant(t, w, http.StatusConflict, "链接已失效", "轮次已过")
	if n := imDecisionCount(t, dbh, execID); n != 0 {
		t.Fatalf("stale link must not vote: %d", n)
	}
}

// TestIMLinkGuards:过期(410)/篡改(403)/词面外 action(403)/名单外
// approver(403 无权)/节点已收口(409)/node_exec 错配(500)。
func TestIMLinkGuards(t *testing.T) {
	r := newIMTestServer(t)
	dbh := sqltest.MustOpenAt(t, "braid_api_test")
	execID, nodeExecID := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol")

	t.Run("expired", func(t *testing.T) {
		p := imLinkAt(t, execID, nodeExecID, "carol", "approve", time.Now().Add(-auth.IMLinkTTL-time.Minute))
		imBodyWant(t, doIMGet(t, r, p), http.StatusGone, "链接已过期")
	})
	t.Run("tampered signature", func(t *testing.T) {
		p := imLinkPath(t, execID, nodeExecID, "carol", "approve")
		b := []byte(p)
		b[len(b)-2]++ // 签名尾段变异
		imBodyWant(t, doIMGet(t, r, string(b)), http.StatusForbidden, "链接无效")
	})
	t.Run("action outside vocabulary", func(t *testing.T) {
		// 签名有效但 action 词面外 = 签发面漂移/伪造,不进决定事务。
		d, s, err := auth.SignIMLink(imLinkAPISecret, auth.IMLinkPayload{
			ExecutionID: execID, NodeExecID: nodeExecID, NodeID: "boss",
			Approver: "carol", Round: 1, Action: "APPROVE",
		}, time.Now())
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		imBodyWant(t, doIMGet(t, r, "/api/v1/im/approval?d="+d+"&s="+s), http.StatusForbidden, "链接无效")
	})
	t.Run("approver outside list", func(t *testing.T) {
		imBodyWant(t, doIMGet(t, r, imLinkPath(t, execID, nodeExecID, "mallory", "approve")),
			http.StatusForbidden, "无权投票")
		if n := imDecisionCount(t, dbh, execID); n != 0 {
			t.Fatalf("membership gate must hold: %d", n)
		}
		// issue #33 硬约束对称面(orch-review LOW 修):名单门拒绝落
		// approval.decision_denied,actor = 短链所指 approver。
		var reason string
		err := dbh.QueryRow(`
SELECT JSON_UNQUOTE(JSON_EXTRACT(payload, '$.reason')) FROM audit_log
 WHERE execution_id=? AND event_type='approval.decision_denied' AND actor_id='mallory'`, execID).Scan(&reason)
		if err != nil || reason != "membership" {
			t.Fatalf("denied audit row: err=%v reason=%q", err, reason)
		}
	})
	t.Run("node not waiting", func(t *testing.T) {
		execID2, nodeExecID2 := seedSuspendedApproval(t, dbh, "RUNNING", "SUCCEEDED", "carol")
		imBodyWant(t, doIMGet(t, r, imLinkPath(t, execID2, nodeExecID2, "carol", "approve")),
			http.StatusConflict, "已收口")
	})
	t.Run("node_exec mismatch", func(t *testing.T) {
		imBodyWant(t, doIMGet(t, r, imLinkPath(t, execID, nodeExecID+1, "carol", "approve")),
			http.StatusInternalServerError, "暂时无法处理")
		if n := imDecisionCount(t, dbh, execID); n != 0 {
			t.Fatalf("mismatch must not vote: %d", n)
		}
	})
}

// TestIMLinkDisabledWithoutSecret:api 未配密钥 = 面关闭(404,路由恒挂载)。
func TestIMLinkDisabledWithoutSecret(t *testing.T) {
	r := newIMTestServerOpt(t, "")
	dbh := sqltest.MustOpenAt(t, "braid_api_test")
	execID, nodeExecID := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol")
	d, s, err := auth.SignIMLink(imLinkAPISecret, auth.IMLinkPayload{
		ExecutionID: execID, NodeExecID: nodeExecID, NodeID: "boss",
		Approver: "carol", Round: 1, Action: "approve",
	}, time.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	w := doIMGet(t, r, "/api/v1/im/approval?d="+d+"&s="+s)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d want 404; body: %s", w.Code, w.Body.String())
	}
}

// TestIMLinkDeciderRaceBranch:并发双投竞速败方的读回裁决臂(对抗审计
// MEDIUM 修:卡片同审批人并排两按钮,快速连点正是竞速典型触发,该臂
// 此前零覆盖)。直调 renderIMDecisionError —— errDeciderRace 只在
// InsertDecision 撞 uk 的真并发下产生,顺序路径恒走重放面,单测直入
// 分支消除断言的并发不确定性。
func TestIMLinkDeciderRaceBranch(t *testing.T) {
	dbh := sqltest.MustOpenAt(t, "braid_api_test")
	ids, err := snowflake.New(12)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	reg, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	cfg := Config{Addr: DefaultAddr, JWTSecret: testSecret, SnowflakeNode: 12, IMLinkSecret: imLinkAPISecret}
	s := NewServer(cfg, dbh, ids, reg, fakeTrigger{id: 1}, auth.BuiltinProvider{DB: dbh})

	for _, tc := range []struct {
		name    string
		prior   string // 竞速胜方已落的票(APPROVED/REJECTED)
		action  string // 本链接(败方)的方向
		want    int
		wantTxt string
	}{
		{"same direction replays 200", "APPROVED", "approve", http.StatusOK, "已投过票"},
		{"opposite direction conflicts 409", "APPROVED", "reject", http.StatusConflict, "投票冲突"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			execID, nodeExecID := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol")
			// 胜方票已提交(读回路径读当前轮 attempts 命中)。
			insVote(t, dbh, execID, nodeExecID, "carol", tc.prior)
			gin.SetMode(gin.TestMode)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
			s.renderIMDecisionError(c, errDeciderRace, auth.IMLinkPayload{
				ExecutionID: execID, NodeExecID: nodeExecID, NodeID: "boss",
				Approver: "carol", Round: 1, Action: tc.action,
			})
			if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.wantTxt) {
				t.Fatalf("code=%d want %d; body missing %q: %s", rec.Code, tc.want, tc.wantTxt, rec.Body.String())
			}
		})
	}
}

// insVote 直插一轮已投决策行(竞速胜方形态;uk_decision_approver
// (node,round,approver) 单票)。
func insVote(t *testing.T, dbh *sql.DB, execID, nodeExecID uint64, by, decision string) {
	t.Helper()
	ids := approvalTestIDs
	vid, err := ids.Next()
	if err != nil {
		t.Fatalf("gen vote id: %v", err)
	}
	if _, err := dbh.Exec(`INSERT INTO approval_decision
	  (id, execution_id, node_execution_id, node_id, round, decided_by, decision, comment)
	  VALUES (?, ?, ?, 'boss', 1, ?, ?, NULL)`, vid, execID, nodeExecID, by, decision); err != nil {
		t.Fatalf("seed vote: %v", err)
	}
}

// TestConfigIMLinkSecretEntropyGate:api 侧熵地板闸(对抗审计 MEDIUM
// 变异实证:`if false` 存活——闸此前零覆盖)。
func TestConfigIMLinkSecretEntropyGate(t *testing.T) {
	ok := DefaultConfig()
	ok.JWTSecret = testSecret
	for _, tc := range []struct {
		name string
		v    string
		bad  bool
	}{
		{"empty disables im face (legal)", "", false},
		{"32-byte secret passes", strings.Repeat("a", 32), false},
		{"31-byte secret rejected", strings.Repeat("a", 31), true},
	} {
		c := ok
		c.IMLinkSecret = tc.v
		err := c.Validate()
		if tc.bad && err == nil {
			t.Fatalf("%s: want error", tc.name)
		}
		if !tc.bad && err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
}
