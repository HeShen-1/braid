package api

// T11.2 跨租户对抗测试(D-2026-09-30-t112 #4;ROADMAP 验收「跨租户访问
// 被拒的对抗测试」)。口径 = 语句→caller 反证(T7.4 教训:演练只测过严
// 方向会漏过松,须从语句出发反证每个 caller 携租户谓词,包可达 ≠ 运行
// 时可达):
//
//   [S1] workflow_definition 全部 SELECT/INSERT/UPDATE(除 scheduler
//        loadDefinition/loadPublishedDefinition)→ api Store 十法
//        (GetByID/GetVersion/LatestPublished/ListVersions/List/UpdateDraft/
//        Publish/Archive/CreateDraft/getByIDForUpdate)签名强制携 tenant,
//        调用点唯一取值面 tenantFrom(c) = claims.Tenant(编译器执法:
//        漏带的调用编译不过,非靠评审自觉);
//   [S2] workflow_execution 读面(GetExecution/ListExecutions/
//        GetExecutionByIdempotencyKey)→ 同上;node_execution 读面
//        (ListNodeExecutions/lockApprovalNode)无租户列,前置 GetExecution
//        租户闸后经 execution 锚派生(tenant_id 行不可变 ⇒ 无 TOCTOU);
//   [S3] approvaltodo pendingWhere(we.tenant_id = ?)→ 本文件
//        TestCrossTenantAdversarialMatrix/pending 闭合「跨租户 DSL 命名
//        他租户主体」面:名单命中 + 执行属读者租户才可见;
//   [S4] auditPredicate(tenant_id = ? 恒携)→ TestHTTPAuditTenant
//        ScopingAndDeniedMasking(三维度含模型名 actor);
//   [S5] engine/reaper/relay 内部写面(nodecas/wfcas/attempt/outbox/
//        flush/reaper 扫描)无租户上下文 = 内部信任边界(D-t62 同判:
//        队列/DB 写权者本就有全量写权),不在 api 读面反证范围;其产出的
//        行经 [S2] 的 execution 锚对读者闭合。
//
// 本文件 = [S2]/[S3] 的执行面 + rollback/decideApproval 的写面闸。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"braid/internal/auth"
	"braid/internal/scheduler"
)

// adversarialTenantID 是对抗矩阵的第二租户(平台 = 0)。种行镜像 0011
// 种子形态(幂等重放)。
const adversarialTenantID = uint64(7)

// seedAdversarialTenant 确保第二租户行在场(各用例共享)。
func seedAdversarialTenant(t *testing.T, dbh *sql.DB) {
	t.Helper()
	if _, err := dbh.Exec(
		`INSERT INTO tenants (id, slug, name) VALUES (?, 't-seven', 'T Seven') ON DUPLICATE KEY UPDATE id = id`,
		adversarialTenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
}

// signTenant 签发指定租户 token(signSubject 的租户变体)。
func signTenant(t *testing.T, subject string, tenant uint64, roles ...auth.Role) string {
	t.Helper()
	s, err := auth.SignToken(testSecret, time.Hour, subject, roles, tenant)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return s
}

// seedSuspendedApprovalTenant 是 seedSuspendedApproval 的租户变体:
// execution 行显式落 tenant_id——构造「租户 7 的工作流把平台用户写进
// 审批名单」的对抗形态(该形态经 [S3] 闭合:平台用户的待办不见此行)。
func seedSuspendedApprovalTenant(t *testing.T, dbh *sql.DB, tenant uint64, approvers ...string) uint64 {
	t.Helper()
	defID, err := approvalTestIDs.Next()
	if err != nil {
		t.Fatalf("gen def id: %v", err)
	}
	execID, err := approvalTestIDs.Next()
	if err != nil {
		t.Fatalf("gen exec id: %v", err)
	}
	nodeID, err := approvalTestIDs.Next()
	if err != nil {
		t.Fatalf("gen node id: %v", err)
	}
	name := fmt.Sprintf("approval-tenant-seed-%d", defID)
	snap, err := json.Marshal(scheduler.ApprovalInput{
		Approvers: approvers, Mode: "any", TimeoutHours: 24, OnTimeout: "fail", Instructions: "请审批",
	})
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if _, err := dbh.Exec(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', '{}', ?, 'seed', 'seed', 'seed', NOW(3))`,
		defID, name, strings.Repeat("0", 64)); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	if _, err := dbh.Exec(`INSERT INTO workflow_execution
	  (id, tenant_id, definition_id, definition_name, definition_version, definition_hash, status,
	   trigger_type, triggered_by, input)
	  VALUES (?,?,?,?,1,?,?,'API','seed','{}')`,
		execID, tenant, defID, name, strings.Repeat("0", 64), "WAITING_APPROVAL"); err != nil {
		t.Fatalf("seed execution: %v", err)
	}
	if _, err := dbh.Exec(`INSERT INTO node_execution
	  (id, execution_id, node_id, type, status, attempts, max_attempts, version,
	   worker_id, input_snapshot, deadline_at)
	  VALUES (?,?, 'boss', 'approval', ?, 1, 1, 2, 'seed', ?, DATE_ADD(NOW(3), INTERVAL 24 HOUR))`,
		nodeID, execID, "WAITING_APPROVAL", snap); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	return execID
}

// TestCrossTenantAdversarialMatrix:[S2]/[S3] 执行面 + 写面闸(rollback/
// decision 的 GetExecution 前置)。跨租户直取 = 404,与「不存在」同面
// (不泄露存在性)。
func TestCrossTenantAdversarialMatrix(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	seedAdversarialTenant(t, dbh)

	// 平台执行(名单 carol)+ 租户 7 执行(名单同为 carol——跨租户命名)。
	platformExec, _ := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol")
	tenantExec := seedSuspendedApprovalTenant(t, dbh, adversarialTenantID, "carol")

	carol := signSubject(t, "carol", auth.RoleApprover) // 平台
	author7 := signTenant(t, "root7", adversarialTenantID, auth.RoleAuthor, auth.RoleAdmin)

	t.Run("pending_closes_cross_tenant_named_approver", func(t *testing.T) {
		// [S3]:carol 名单命中两行,但租户谓词只放行平台执行——「租户 7
		// 的 DSL 命名平台用户」不产生可见待办(闭合计时的 notify 扇出面
		// 也因执行不可达而阻断在读者侧;发布期 approver 租户校验 = P1)。
		code, body := doJSON(t, r, http.MethodGet, "/api/v1/approvals/pending", carol, nil)
		if code != http.StatusOK {
			t.Fatalf("pending = %d %v", code, body)
		}
		page := body["page"].(map[string]any)
		if page["total"] != float64(1) {
			t.Fatalf("total = %v, want 1(租户 7 行不得入列)", page["total"])
		}
		items := body["items"].([]any)
		if items[0].(map[string]any)["execution_id"] != strconv.FormatUint(platformExec, 10) {
			t.Fatalf("可见行 = %v, want 平台执行", items[0])
		}
	})

	t.Run("decision_cross_tenant_404", func(t *testing.T) {
		// [S2]:carol 是 DSL 名单成员(角色+membership 本应全过),但执行
		// 属租户 7 → GetExecution 租户闸先行 404(越权面与「不存在」同形)。
		code, body := doJSON(t, r, http.MethodPost, decisionPath(tenantExec), carol,
			[]byte(`{"decision":"approve"}`))
		if code != http.StatusNotFound {
			t.Fatalf("cross decision = %d %v, want 404", code, body)
		}
		// 控制臂:同租户执行的决定面可达(角色/名单通过后进事务闸)。
		code, _ = doJSON(t, r, http.MethodPost, decisionPath(platformExec), carol,
			[]byte(`{"decision":"approve"}`))
		if code == http.StatusNotFound {
			t.Fatalf("own-tenant decision 不可达(控制臂失败,检查种子)")
		}
	})

	t.Run("rollback_cross_tenant_404", func(t *testing.T) {
		// [S2] 写面闸:租户 7 的 author|admin 对平台执行回滚 → 404;
		// ghost id 同面(不泄露存在性)。
		code, body := doJSON(t, r, http.MethodPost,
			"/api/v1/executions/"+strconv.FormatUint(platformExec, 10)+"/rollback", author7,
			[]byte(`{"node_id":"boss"}`))
		if code != http.StatusNotFound {
			t.Fatalf("cross rollback = %d %v, want 404", code, body)
		}
		code, _ = doJSON(t, r, http.MethodPost,
			"/api/v1/executions/999999/rollback", author7, []byte(`{"node_id":"boss"}`))
		if code != http.StatusNotFound {
			t.Fatalf("ghost rollback = %d, want 404(同面)", code)
		}
	})

	t.Run("approval_detail_cross_tenant_404", func(t *testing.T) {
		// [S2] 第三调用面:getApproval 详情端点(READ /executions/:id/
		// approvals/:node_id)经 GetExecution 前置闸——对抗审计 MEDIUM 补:
		// [S2] 注释锚声称三调用面闭合,矩阵此前只钉两处。
		code, _ := doJSON(t, r, http.MethodGet,
			"/api/v1/executions/"+strconv.FormatUint(tenantExec, 10)+"/approvals/boss", carol, nil)
		if code != http.StatusNotFound {
			t.Fatalf("cross approval detail = %d, want 404", code)
		}
		// 控制臂:同租户读者可见(名单收窄面在 handler 内,404/200 均可达)。
		code, _ = doJSON(t, r, http.MethodGet,
			"/api/v1/executions/"+strconv.FormatUint(platformExec, 10)+"/approvals/boss", carol, nil)
		if code == http.StatusNotFound {
			t.Fatalf("own approval detail 不可达(控制臂失败,检查种子)")
		}
	})

	t.Run("execution_list_and_detail_scoped", func(t *testing.T) {
		// [S2]:列表级排他(非点查 404——列表谓词的残差过滤面)。
		code, body := doJSON(t, r, http.MethodGet, "/api/v1/executions?limit=100", carol, nil)
		if code != http.StatusOK {
			t.Fatalf("list = %d", code)
		}
		for _, it := range body["items"].([]any) {
			if it.(map[string]any)["execution_id"] == strconv.FormatUint(tenantExec, 10) ||
				it.(map[string]any)["id"] == strconv.FormatUint(tenantExec, 10) {
				t.Fatalf("平台列表泄露租户 7 执行 %d", tenantExec)
			}
		}
		code, _ = doJSON(t, r, http.MethodGet,
			"/api/v1/executions/"+strconv.FormatUint(tenantExec, 10), carol, nil)
		if code != http.StatusNotFound {
			t.Fatalf("cross detail = %d, want 404", code)
		}
		// 对侧控制臂:租户 7 读者自见。
		code, _ = doJSON(t, r, http.MethodGet,
			"/api/v1/executions/"+strconv.FormatUint(tenantExec, 10), author7, nil)
		if code != http.StatusOK {
			t.Fatalf("own detail = %d, want 200", code)
		}
	})
}
