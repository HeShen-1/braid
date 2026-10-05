package api

// T19.3 取消端点契约面(D-2026-10-05-t193):真 scheduler 链(rollback_test
// 同基建)——分面 401/403/404/409、成功 200 回显、拒绝审计 cancel.denied、
// SUCCEEDED 执行可回滚的端到端口径(白名单扩列的 api 面锚)。

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"braid/internal/auth"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
)

func TestHTTPCancelEndpoint(t *testing.T) {
	dbh := openAPITestDB(t)
	schedIDs, err := snowflake.New(7)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	sched, err := scheduler.New(scheduler.Deps{DB: dbh, IDs: schedIDs})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	r, _, sign := newTestServerWithTrigger(t, sched)
	author := sign(auth.RoleAuthor)
	viewer := signSubject(t, "eve", auth.RoleApprover) // 无 author/admin → 403 面
	name := uniqName("cx-api")
	rbPublish(t, r, author, name, rbChainDoc(name))

	seedRunning := func(t *testing.T) uint64 {
		t.Helper()
		code, body := doJSON(t, r, http.MethodPost, "/api/v1/executions", author,
			[]byte(fmt.Sprintf(`{"definition_name":%q,"input":{"q":"hi"}}`, name)))
		if code != http.StatusCreated {
			t.Fatalf("trigger: code %d body %v", code, body)
		}
		execID := mustParseUint(t, body["id"].(string))
		if err := sched.Start(context.Background(), execID, "w1"); err != nil {
			t.Fatalf("Start: %v", err)
		}
		return execID
	}

	t.Run("403 非授权角色 + 拒绝审计(载荷分面)", func(t *testing.T) {
		execID := seedRunning(t)
		code, body := doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/cancel", execID), viewer, nil)
		if code != http.StatusForbidden {
			t.Fatalf("code = %d %v, want 403", code, body)
		}
		var denied int
		var reason string
		if err := dbh.QueryRow(`SELECT COUNT(*), COALESCE(
		    (SELECT JSON_UNQUOTE(JSON_EXTRACT(payload,'$.reason')) FROM audit_log
		      WHERE execution_id=? AND event_type='cancel.denied' LIMIT 1), '')
		  FROM audit_log WHERE execution_id=? AND event_type='cancel.denied'`,
			execID, execID).Scan(&denied, &reason); err != nil {
			t.Fatalf("count denied: %v", err)
		}
		if denied != 1 || reason != "role_denied" {
			t.Fatalf("cancel.denied = %d 行 reason=%q, want 1/role_denied", denied, reason)
		}
	})

	t.Run("404 执行不存在", func(t *testing.T) {
		code, _ := doJSON(t, r, http.MethodPost, "/api/v1/executions/1/cancel", author, nil)
		if code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", code)
		}
	})

	t.Run("200 成功 + 重复 409 not_cancellable", func(t *testing.T) {
		execID := seedRunning(t)
		code, body := doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/cancel", execID), author, nil)
		if code != http.StatusOK {
			t.Fatalf("code = %d %v, want 200", code, body)
		}
		if body["from_status"] != "RUNNING" || body["cancelled_count"] != float64(3) {
			t.Fatalf("body = %v, want from=RUNNING count=3", body)
		}
		// 幂等锚:wf 已 CANCELLED → 二次取消 409(重复点击报错非重放)+
		// denied 审计分面(对抗审计 C6:not_cancellable 面须落行且 reason 可辨)。
		code, body = doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/cancel", execID), author, nil)
		if code != http.StatusConflict || body["error"] != "not_cancellable" {
			t.Fatalf("repeat code = %d %v, want 409 not_cancellable", code, body)
		}
		var reason string
		if err := dbh.QueryRow(`SELECT JSON_UNQUOTE(JSON_EXTRACT(payload,'$.reason'))
			FROM audit_log WHERE execution_id=? AND event_type='cancel.denied'`, execID).Scan(&reason); err != nil || reason != "not_cancellable" {
			t.Fatalf("denied reason = %q(%v), want not_cancellable", reason, err)
		}
	})

	t.Run("取消后 SUCCEEDED 回滚口径:cancel → resume 自动定位楔源", func(t *testing.T) {
		// api 面的 T19.3 白名单联动锚:取消产生的 CANCELLED 执行经 resume
		// 端点自动定位(字典序首个 CANCELLED = a)复活——取消/回滚双原语
		// 的端到端闭环。
		execID := seedRunning(t)
		code, _ := doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/cancel", execID), author, nil)
		if code != http.StatusOK {
			t.Fatalf("cancel: code %d", code)
		}
		code, body := doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/resume", execID), author, nil)
		if code != http.StatusCreated {
			t.Fatalf("resume cancelled: code %d %v, want 201", code, body)
		}
		if body["target_node_id"] != "a" {
			t.Fatalf("auto target = %v, want a(楔源)", body["target_node_id"])
		}
	})

	t.Run("409 cancel_too_large + denied 审计(共闸)", func(t *testing.T) {
		// 容量闸与回滚共闸(BRAID_ROLLBACK_MAX_ROWS 单源):api 分面锚。
		cappedIDs, err := snowflake.New(9)
		if err != nil {
			t.Fatalf("snowflake: %v", err)
		}
		capped, err := scheduler.New(scheduler.Deps{DB: dbh, IDs: cappedIDs, MaxRollbackRows: 2})
		if err != nil {
			t.Fatalf("scheduler: %v", err)
		}
		r2, _, sign2 := newTestServerWithTrigger(t, capped)
		a2 := sign2(auth.RoleAuthor)
		name2 := uniqName("cx-cap")
		rbPublish(t, r2, a2, name2, rbChainDoc(name2))
		code, body := doJSON(t, r2, http.MethodPost, "/api/v1/executions", a2,
			[]byte(fmt.Sprintf(`{"definition_name":%q,"input":{"q":"hi"}}`, name2)))
		if code != http.StatusCreated {
			t.Fatalf("trigger: code %d body %v", code, body)
		}
		execID := mustParseUint(t, body["id"].(string))
		if err := capped.Start(context.Background(), execID, "w1"); err != nil {
			t.Fatalf("Start: %v", err)
		}
		code, body = doJSON(t, r2, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/cancel", execID), a2, nil)
		if code != http.StatusConflict || body["error"] != "cancel_too_large" {
			t.Fatalf("cancel cap: code = %d %v, want 409 cancel_too_large", code, body)
		}
		var reason string
		if err := dbh.QueryRow(`SELECT JSON_UNQUOTE(JSON_EXTRACT(payload,'$.reason'))
			FROM audit_log WHERE execution_id=? AND event_type='cancel.denied'`, execID).Scan(&reason); err != nil || reason != "too_large" {
			t.Fatalf("denied reason = %q(%v), want too_large", reason, err)
		}
	})
}
