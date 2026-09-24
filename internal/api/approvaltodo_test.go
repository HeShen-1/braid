package api

// T4.4 待办列表 HTTP 契约测试(t13 真 MySQL,不可达自动 skip):
// GET /api/v1/approvals/pending。名单真相源 = input_snapshot.approvers
// (JSON_CONTAINS 精确成员判定,勿造第二判据——issue #43 硬约束);
// WAITING_APPROVAL 前缀扫 + 非终态执行过滤(镜像 POST 终态守卫);
// deadline 升序(最紧急优先,索引序)。
//
// 变异自查:名单过滤换成 LIKE → 子串用例红(carol ≠ caroline);
// 终态执行过滤删除 → 孤儿用例红;排序换 deadline DESC → 顺序断言红;
// 状态谓词漏 WAITING_APPROVAL → 已收口节点混入红(total 断言)。

import (
	"database/sql"
	"net/http"
	"strconv"
	"testing"

	"braid/internal/auth"
)

// setApprovalDeadline 直改挂起节点 deadline(排序用例的确定性锚;种子
// 默认 NOW+24h,无相对序)。
func setApprovalDeadline(t *testing.T, dbh *sql.DB, execID uint64, hours int) {
	t.Helper()
	if _, err := dbh.Exec(`UPDATE node_execution SET deadline_at = DATE_ADD(NOW(3), INTERVAL ? HOUR)
	  WHERE execution_id = ?`, hours, execID); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
}

func TestApprovalsPendingList(t *testing.T) {
	r, dbh, _ := newTestServer(t)
	carol := signSubject(t, "carol", auth.RoleApprover)
	dave := signSubject(t, "dave", auth.RoleApprover)
	bob := signSubject(t, "bob", auth.RoleApprover)

	// e1: carol+caroline;e2: carol+dave;e3: 仅 caroline(子串陷阱:
	// carol 不得命中 caroline——JSON_CONTAINS 精确成员,非 LIKE)。
	e1, _ := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol", "caroline")
	e2, _ := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol", "dave")
	e3, _ := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "caroline")
	setApprovalDeadline(t, dbh, e1, 2)
	setApprovalDeadline(t, dbh, e2, 1)
	setApprovalDeadline(t, dbh, e3, 3)
	// 非待办形态:节点已收口 / 孤儿(wf 终态镜像 POST 终态守卫,均不得入列)。
	seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "SUCCEEDED", "carol")
	seedSuspendedApproval(t, dbh, "SUCCEEDED", "WAITING_APPROVAL", "carol")

	list := func(token, query string) (int, map[string]any) {
		t.Helper()
		return doJSON(t, r, http.MethodGet, "/api/v1/approvals/pending"+query, token, nil)
	}

	t.Run("membership_and_order", func(t *testing.T) {
		code, body := list(carol, "")
		if code != http.StatusOK {
			t.Fatalf("code = %d %v", code, body)
		}
		page := body["page"].(map[string]any)
		if page["total"] != float64(2) {
			t.Fatalf("total = %v, want 2(e3 子串不命中 + 收口/孤儿不入列)", page["total"])
		}
		items := body["items"].([]any)
		if len(items) != 2 {
			t.Fatalf("items = %d, want 2", len(items))
		}
		// deadline 升序:e2(+1h)先于 e1(+2h)。
		first := items[0].(map[string]any)
		second := items[1].(map[string]any)
		if first["execution_id"] != strconv.FormatUint(e2, 10) ||
			second["execution_id"] != strconv.FormatUint(e1, 10) {
			t.Fatalf("order = [%v, %v], want [e2, e1](deadline 升序)",
				first["execution_id"], second["execution_id"])
		}
		// 行视图契约:瘦字段,execution_id 字符串化(2^53 精度)。
		if first["node_id"] != "boss" || first["definition_version"] != float64(1) {
			t.Fatalf("item fields = %v", first)
		}
		if name, _ := first["definition_name"].(string); len(name) == 0 {
			t.Fatalf("definition_name missing: %v", first)
		}
		if dl, _ := first["deadline_at"].(string); dl == "" {
			t.Fatalf("deadline_at missing: %v", first)
		}
	})

	t.Run("other_approver", func(t *testing.T) {
		code, body := list(dave, "")
		if code != http.StatusOK {
			t.Fatalf("code = %d", code)
		}
		if body["page"].(map[string]any)["total"] != float64(1) {
			t.Fatalf("dave total = %v, want 1", body["page"])
		}
		if got := body["items"].([]any)[0].(map[string]any)["execution_id"]; got != strconv.FormatUint(e2, 10) {
			t.Fatalf("dave sees %v, want e2", got)
		}
	})

	t.Run("empty_for_outsider", func(t *testing.T) {
		code, body := list(bob, "")
		if code != http.StatusOK {
			t.Fatalf("code = %d", code)
		}
		items, ok := body["items"].([]any)
		if !ok || len(items) != 0 {
			t.Fatalf("bob items = %#v, want 空数组(非 nil)", body["items"])
		}
		if body["page"].(map[string]any)["total"] != float64(0) {
			t.Fatalf("bob total = %v, want 0", body["page"])
		}
	})

	t.Run("pagination", func(t *testing.T) {
		code, body := list(carol, "?limit=1")
		if code != http.StatusOK {
			t.Fatalf("code = %d", code)
		}
		if n := len(body["items"].([]any)); n != 1 {
			t.Fatalf("limit=1 items = %d", n)
		}
		if body["page"].(map[string]any)["total"] != float64(2) {
			t.Fatalf("total = %v, want 2(计数不受 limit 影响)", body["page"])
		}
		for _, q := range []string{"?limit=0", "?limit=101", "?offset=-1", "?offset=10001"} {
			code, _ := list(carol, q)
			if code != http.StatusBadRequest {
				t.Fatalf("query %s code = %d, want 400", q, code)
			}
		}
	})

	t.Run("unauthenticated", func(t *testing.T) {
		code, _ := list("", "")
		if code != http.StatusUnauthorized {
			t.Fatalf("code = %d, want 401", code)
		}
	})

	// WAITING ⇒ deadline NOT NULL 是 engine 不变量,违反 = 数据异常,
	// 读面响亮 500 不静默吞(变异自查:删除 approvaltodo.go 的 !deadline.Valid
	// 检查后本用例红——不变量守卫的锚,对抗审计补)。
	t.Run("null_deadline_fails_loud", func(t *testing.T) {
		execID, _ := seedSuspendedApproval(t, dbh, "WAITING_APPROVAL", "WAITING_APPROVAL", "carol")
		if _, err := dbh.Exec(`UPDATE node_execution SET deadline_at = NULL WHERE execution_id = ?`, execID); err != nil {
			t.Fatalf("null deadline: %v", err)
		}
		code, _ := list(carol, "")
		if code != http.StatusInternalServerError {
			t.Fatalf("code = %d, want 500(数据异常响亮失败)", code)
		}
	})
}
