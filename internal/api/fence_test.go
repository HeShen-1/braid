package api

// fence_test.go = T20.1 fence 面(api 五写面之 publish)集成测试(t13 栈
// MySQL;栈不可达自动 skip)。口径真源 = D-2026-10-06-t201 决策 4:zombie
// api 的发布事务结构性回滚——草稿不得被失冠进程翻成新发布版本。

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"braid/internal/auth"
	"braid/internal/db/sqltest"
	"braid/internal/metrics"
	"braid/internal/snowflake"
	"braid/internal/tools"
)

const apiFenceNode = 1 // schema 内自洽(api 内建默认号语义,测试库隔离)

// fence 种子生成器:包级单实例(issue #210)——seedApprovalFenceChain 被
// 同一测试连发四次,per-call New(21) 的首号恒为 (当前毫秒, 21, seq=0),
// 任意两次落同毫秒即 workflow_definition PK 1062(CI 实证)。单实例下 seq
// 跨调用单调递增,撞号结构性消失(同 approval_test.go:82 的包级先例)。
// 族内其余 per-call New(节点 1/2/3…)分属不同测试函数,函数间有建栈
// 成本(建 schema/租约),跨函数同毫秒不可达,不扫。
var fenceTestIDs, _ = snowflake.New(21)

// newLeasedTestServer:newTestServer 的带租约变体(T20.1):Server 经
// SnowflakeLease 注入真实租约 + FenceMetrics 观测面,模拟 cmd/api 生产装配。
// 返回 Server 本体(直接方法调用锚需要);Router() 按需派生。
func newLeasedTestServer(t *testing.T) (*Server, *snowflake.Lease, *sql.DB) {
	t.Helper()
	dbh := sqltest.MustOpenAt(t, "braid_api_test")
	cleanExecutionChain(t, dbh)
	if _, err := dbh.Exec(`DELETE FROM snowflake_node_lease`); err != nil {
		t.Fatalf("clear leases: %v", err)
	}
	lease, err := snowflake.StartLease(context.Background(), dbh, snowflake.LeaseConfig{
		Kind:   snowflake.KindAPI,
		Spec:   snowflake.Explicit(apiFenceNode),
		Holder: "api-fence",
	})
	if err != nil {
		t.Fatalf("StartLease: %v", err)
	}
	t.Cleanup(func() { _ = lease.Release(context.Background()) })
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	cfg := Config{Addr: DefaultAddr, JWTSecret: testSecret, SnowflakeNode: snowflake.Explicit(1),
		LoginRateLimit: 100000, LoginRateWindow: time.Minute,
		LoginMaxFailures: 100000, LoginLockout: time.Minute}
	reg, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	gin.SetMode(gin.TestMode)
	srv := NewServer(cfg, dbh, ids, reg, fakeTrigger{id: 1}, auth.BuiltinProvider{DB: dbh})
	srv.SnowflakeLease(lease)
	mset, _ := metrics.NewSet()
	srv.FenceMetrics(mset.Fence)
	return srv, lease, dbh
}

// TestPublishFenceRejectsSupersededLease:冠内发布照常(200 + DRAFT→
// PUBLISHED);易主后发布被拒,草稿停留 DRAFT(zombie api 不得翻新版本)。
func TestPublishFenceRejectsSupersededLease(t *testing.T) {
	srv, _, dbh := newLeasedTestServer(t)
	r := srv.Router()
	token, err := auth.SignToken(testSecret, time.Hour, "alice", []auth.Role{auth.RoleAuthor}, 0)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	// 冠内:发布成功(fence 零行为扰动)。
	okName := uniqName("fence-ok")
	created := createViaAPI(t, r, token, dslDoc(okName))
	id1 := created["id"].(string)
	if code, _ := doJSON(t, r, http.MethodPost, "/api/v1/drafts/"+id1+"/publish", token, nil); code != http.StatusOK {
		t.Fatalf("crowned publish: code %d, want 200", code)
	}

	// 易主(zombie api 形态:代际失配)。
	if _, err := dbh.Exec(
		`UPDATE snowflake_node_lease SET fencing_epoch = fencing_epoch + 1 WHERE node_id = ?`,
		apiFenceNode); err != nil {
		t.Fatalf("supersede lease: %v", err)
	}

	zombieName := uniqName("fence-zombie")
	created2 := createViaAPI(t, r, token, dslDoc(zombieName))
	id2 := created2["id"].(string)
	if code, _ := doJSON(t, r, http.MethodPost, "/api/v1/drafts/"+id2+"/publish", token, nil); code == http.StatusOK {
		t.Fatalf("superseded publish: code 200, want non-200(zombie api 不得发布)")
	}
	var pub int
	if err := dbh.QueryRow(
		`SELECT COUNT(*) FROM workflow_definition WHERE name=? AND status='PUBLISHED'`,
		zombieName).Scan(&pub); err != nil {
		t.Fatalf("count published: %v", err)
	}
	if pub != 0 {
		t.Fatalf("superseded publish landed %d PUBLISHED rows, want 0", pub)
	}
}

// seedApprovalFenceChain 落一套审批 decide 消费链(definition PUBLISHED +
// execution RUNNING + approval 节点 WAITING_APPROVAL 带 approvers=["bob"]
// 快照),返回 (execID, nodeExecID)。
func seedApprovalFenceChain(t *testing.T, dbh *sql.DB, tag string) (uint64, uint64) {
	t.Helper()
	ids := fenceTestIDs
	defID, _ := ids.Next()
	execID, _ := ids.Next()
	nodeExecID, _ := ids.Next()
	name := uniqName(tag)
	dsl := `{"dsl":"1.0","name":"` + name + `","inputs":[],"edges":[],"nodes":[{"id":"a","type":"approval","approvers":["bob"]}],"outputs":{}}`
	ins := func(q string, args ...any) {
		t.Helper()
		if _, err := dbh.Exec(q, args...); err != nil {
			t.Fatalf("seed approval chain: %v", err)
		}
	}
	ins(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', ?, ?, 'seed', 'seed', 'seed', NOW(3))`,
		defID, name, dsl, string(make([]byte, 64)))
	ins(`INSERT INTO workflow_execution
	  (id, definition_id, definition_name, definition_version, definition_hash, status, triggered_by, input)
	  VALUES (?, ?, ?, 1, ?, 'RUNNING', 'seed', '{}')`,
		execID, defID, name, string(make([]byte, 64)))
	ins(`INSERT INTO node_execution
	  (id, execution_id, node_id, type, status, attempts, version, deadline_at, input_snapshot)
	  VALUES (?, ?, 'a', 'approval', 'WAITING_APPROVAL', 1, 2, NOW(3) + INTERVAL 30 MINUTE, ?)`,
		nodeExecID, execID, `{"approvers":["bob"],"mode":"any","timeout_hours":24,"on_timeout":"escalate","instructions":""}`)
	t.Cleanup(func() {
		cleanExecutionChain(t, dbh) // 全清版 helper(隔离 schema 内互不影响;LIFO 下首清即净)
		_, _ = dbh.Exec(`DELETE FROM workflow_definition WHERE id=?`, defID)
	})
	return execID, nodeExecID
}

// auditRowCount 数指定执行的事件行数(fence 面证据事务的落行判据)。
func auditRowCount(t *testing.T, dbh *sql.DB, execID uint64, event string) int {
	t.Helper()
	var n int
	if err := dbh.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND event_type=?`, execID, event,
	).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// fakeGinC 构造 denied writer 面直接方法调用的最小 gin ctx。
func fakeGinC() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	return c
}

// TestFenceSeedDistinctIDs:同测试连发种子的 id 唯一性锚(issue #210)。
// 修复前 = per-call New(21) 四发同毫秒即 workflow_definition PK 1062
// (flake 常态路径,撞号在 seed 内 Fatal);修复后包级单实例 seq 单调,
// 恒绿——本锚同时拦住「改回 per-call New」的回归。
func TestFenceSeedDistinctIDs(t *testing.T) {
	_, _, dbh := newLeasedTestServer(t)
	seen := make(map[uint64]bool)
	for i := 0; i < 4; i++ {
		defID, _ := seedApprovalFenceChain(t, dbh, "fence-seed-distinct")
		if seen[defID] {
			t.Fatalf("defID %d repeated on rapid seed #%d", defID, i)
		}
		seen[defID] = true
	}
}

// TestAPIFenceAllWriteFaces:对抗审计 HIGH 锚——api 六写面(publish 见
// TestPublishFenceRejectsSupersededLease)其余五面逐一非 vacuous:冠内
// 落行/成功 → 失冠 ErrFenced/零行。denied 三 writer 与触发证据事务走
// 直接方法调用(事务级锚),审批 decide 走种子链全旅程。
func TestAPIFenceAllWriteFaces(t *testing.T) {
	srv, lease, dbh := newLeasedTestServer(t)
	ctx := context.Background()

	// 审计 Append 按执行行推 exec_date(分区键),四类写面各需真实种子链。
	trigExec1, _ := seedApprovalFenceChain(t, dbh, "fence-trig-ok")
	deniedExec1, _ := seedApprovalFenceChain(t, dbh, "fence-denied-ok")
	trigExec2, _ := seedApprovalFenceChain(t, dbh, "fence-trig-zombie")
	deniedExec2, _ := seedApprovalFenceChain(t, dbh, "fence-denied-zombie")

	// --- 冠内控制:五面全部落行/成功。
	srv.writeTriggerAudit(ctx, trigExec1, 1, "alice")
	if n := auditRowCount(t, dbh, trigExec1, "execution.triggered"); n != 1 {
		t.Fatalf("crowned trigger audit rows = %d, want 1", n)
	}
	srv.writeCancelDeniedAudit(fakeGinC(), deniedExec1, "alice", "not_cancellable")
	if n := auditRowCount(t, dbh, deniedExec1, "cancel.denied"); n != 1 {
		t.Fatalf("crowned cancel.denied rows = %d, want 1", n)
	}
	srv.writeRollbackDeniedAudit(fakeGinC(), deniedExec1, "", "alice", "role_denied", 0, false)
	if n := auditRowCount(t, dbh, deniedExec1, "rollback.denied"); n != 1 {
		t.Fatalf("crowned rollback.denied rows = %d, want 1", n)
	}
	srv.writeDecisionDeniedAudit(fakeGinC(), deniedExec1, "a", "alice", "api", "role_denied")
	if n := auditRowCount(t, dbh, deniedExec1, "approval.decision_denied"); n != 1 {
		t.Fatalf("crowned approval.decision_denied rows = %d, want 1", n)
	}
	decExec1, _ := seedApprovalFenceChain(t, dbh, "fence-decide-ok")
	if _, _, err := srv.applyApprovalDecision(ctx, decisionCall{ // LinkRound=-1 = Bearer 路径哨兵(短链恒 ≥0)
		ExecID: decExec1, NodeID: "a", Actor: "bob", Decision: "APPROVED", LinkRound: -1,
	}); err != nil {
		t.Fatalf("crowned applyApprovalDecision: %v", err)
	}

	// --- 失冠(zombie api 形态:代际失配)。
	if _, err := dbh.Exec(
		`UPDATE snowflake_node_lease SET fencing_epoch = fencing_epoch + 1 WHERE node_id = ?`,
		apiFenceNode); err != nil {
		t.Fatalf("supersede lease: %v", err)
	}
	if err := lease.Fence(ctx, dbh); !errors.Is(err, snowflake.ErrFenced) {
		t.Fatalf("precondition: fence after supersede = %v", err)
	}

	srv.writeTriggerAudit(ctx, trigExec2, 1, "alice")
	if n := auditRowCount(t, dbh, trigExec2, "execution.triggered"); n != 0 {
		t.Fatalf("superseded trigger audit rows = %d, want 0", n)
	}
	srv.writeCancelDeniedAudit(fakeGinC(), deniedExec2, "alice", "not_cancellable")
	if n := auditRowCount(t, dbh, deniedExec2, "cancel.denied"); n != 0 {
		t.Fatalf("superseded cancel.denied rows = %d, want 0", n)
	}
	srv.writeRollbackDeniedAudit(fakeGinC(), deniedExec2, "", "alice", "role_denied", 0, false)
	if n := auditRowCount(t, dbh, deniedExec2, "rollback.denied"); n != 0 {
		t.Fatalf("superseded rollback.denied rows = %d, want 0", n)
	}
	srv.writeDecisionDeniedAudit(fakeGinC(), deniedExec2, "a", "alice", "api", "role_denied")
	if n := auditRowCount(t, dbh, deniedExec2, "approval.decision_denied"); n != 0 {
		t.Fatalf("superseded approval.decision_denied rows = %d, want 0", n)
	}
	decExec2, _ := seedApprovalFenceChain(t, dbh, "fence-decide-zombie")
	if _, _, err := srv.applyApprovalDecision(ctx, decisionCall{
		ExecID: decExec2, NodeID: "a", Actor: "bob", Decision: "APPROVED", LinkRound: -1,
	}); !errors.Is(err, snowflake.ErrFenced) {
		t.Fatalf("superseded applyApprovalDecision: %v, want ErrFenced", err)
	}
}
