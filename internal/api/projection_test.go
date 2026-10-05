package api

// T18.4 投影只读端点(D-2026-10-05-t184-projection-read-endpoints)契约面:
// 按路径(node_execution 投影列)与按轮(node_attempt 台账)三端点——
// 真 scheduler 链(rb* 先例)+ 直插种子(approval 面先例):
//   - 投影列语义:output = 最新成功轮投影(D-t92 决策 9)、input_snapshot
//     NULL 不伪造、attempts/attempts_base 轮区间锚(旧轮 [1,base]);
//   - round ≡ attempt_no(D-t92 决策 6,与 decisionDTO 词表同轴):回滚
//     重跑后旧轮 FAILED + 新轮 SUCCEEDED 全时间线升序;
//   - 保留期变薄(D-t186 决策 1/5③):janitor 清走的旧轮 = 列表 200 变薄、
//     单轮 404,成因同面不报错;
//   - 租户锚三臂(node 系无租户列,GetExecution 前置闸)+ 形状闸
//     (bad_node_id / bad_instance_no / bad_round;round 0 是 approval
//     存量哨兵语义,不映射 attempt——attempt_no CHECK >= 1);
//   - usage/cost 列组(auditquery 同形)与 DEAD 行(rollback_reset 收口)
//     的 NULL 语义;attempt_id 字符串化(2^53)。
// scp 枚举面由 scope_enforce_test 策略表双向锁死(三行同 PR 旅行)。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"braid/internal/auth"
	"braid/internal/engine"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
)

// prjDriveMultiRound:触发 → Start → a 成 → b 终败 → resume(b 重挂第二轮)
// → b 第二轮成。b 终态:attempts=2、attempts_base=1、output=第二轮投影;
// 台账:round1 FAILED + round2 SUCCEEDED(rbSeedAttempt 直插,无快照列)。
func prjDriveMultiRound(t *testing.T, r *gin.Engine, sched *scheduler.Scheduler,
	dbh *sql.DB, token, name string) (execID, bNodeID uint64) {
	t.Helper()
	execID, bNodeID = rbDriveToFailed(t, r, sched, dbh, token, name)
	// resume(无 body 自动定位字典序首个 FAILED = b)→ b 重挂第二轮。
	code, body := doJSON(t, r, http.MethodPost,
		fmt.Sprintf("/api/v1/executions/%d/resume", execID), token, nil)
	if code != http.StatusCreated {
		t.Fatalf("resume: code %d body %v", code, body)
	}
	rbComplete(t, sched, execID, bNodeID, rbSeedAttempt(t, dbh, execID, bNodeID, 2),
		engine.AttemptSucceed, json.RawMessage(`{"text":"vb"}`))
	return execID, bNodeID
}

// TestHTTPProjectionByPath:按路径端点 GET /executions/:id/nodes/:node_id
// (?instance_no= 缺省 0=控制行)。
func TestHTTPProjectionByPath(t *testing.T) {
	dbh := openAPITestDB(t)
	schedIDs, err := snowflake.New(5)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	sched, err := scheduler.New(scheduler.Deps{DB: dbh, IDs: schedIDs})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	r, _, sign := newTestServerWithTrigger(t, sched)
	author := sign(auth.RoleAuthor)
	name := uniqName("prj-path")
	rbPublish(t, r, author, name, rbChainDoc(name))
	execID, b := rbDriveToFailed(t, r, sched, dbh, author, name) // b FAILED attempts=1;c PENDING 冻结

	t.Run("200 未派发节点:投影列 null 不伪造", func(t *testing.T) {
		code, body := doJSON(t, r, http.MethodGet,
			fmt.Sprintf("/api/v1/executions/%d/nodes/c", execID), author, nil)
		if code != http.StatusOK {
			t.Fatalf("code = %d %v, want 200", code, body)
		}
		if body["status"] != "PENDING" || body["attempts"] != float64(0) {
			t.Fatalf("行视图 = %v, want PENDING attempts=0", body)
		}
		// PENDING ⇒ input_snapshot 恒 NULL(engine 不变量);无成功轮 ⇒
		// output null——缺席不伪造(audit trace_id 同纪律)。
		if body["input_snapshot"] != nil || body["output"] != nil || body["started_at"] != nil {
			t.Fatalf("投影列 = %v/%v/%v, want null/null/null",
				body["input_snapshot"], body["output"], body["started_at"])
		}
		if body["finished_at"] != nil {
			t.Fatalf("PENDING finished_at = %v, want null(未终态)", body["finished_at"])
		}
	})

	t.Run("200 失败节点 round-1 投影", func(t *testing.T) {
		code, body := doJSON(t, r, http.MethodGet,
			fmt.Sprintf("/api/v1/executions/%d/nodes/b", execID), author, nil)
		if code != http.StatusOK {
			t.Fatalf("code = %d %v, want 200", code, body)
		}
		if body["status"] != "FAILED" || body["attempts"] != float64(1) || body["attempts_base"] != float64(0) {
			t.Fatalf("行视图 = %v, want FAILED attempts=1 base=0", body)
		}
		// 无成功轮 ⇒ output null;失败码在场;快照在场(已入 EXECUTING 过)。
		if body["output"] != nil {
			t.Fatalf("output = %v, want null(无成功轮)", body["output"])
		}
		if body["error_code"] == nil || body["input_snapshot"] == nil {
			t.Fatalf("error/snapshot = %v/%v, want 双在场", body["error_code"], body["input_snapshot"])
		}
		// 值锚(对抗审计 MEDIUM 修:error_code↔error_message 同型单侧交换
		// 在 presence 断言下逃逸——引擎实落 attempts_exhausted/空串)。
		if body["error_code"] != "attempts_exhausted" {
			t.Fatalf("error_code = %v, want attempts_exhausted(值锚)", body["error_code"])
		}
		// 时间对值锚:started↔finished 同型交换逃逸(对抗审计 MEDIUM 修;
		// 毫秒精度下两时刻可同 ms,用 !Before 而非严格 After 防抖)。
		st, _ := body["started_at"].(string)
		ft, _ := body["finished_at"].(string)
		sa, errSa := time.Parse(time.RFC3339, st)
		fa, errFa := time.Parse(time.RFC3339, ft)
		if errSa != nil || errFa != nil || fa.Before(sa) {
			t.Fatalf("started/finished = %q/%q(%v/%v), want 同序时间对", st, ft, errSa, errFa)
		}
	})

	// resume(b 重挂第二轮)→ b 第二轮成;c 随推进被派发(EXECUTING)。
	code, body := doJSON(t, r, http.MethodPost,
		fmt.Sprintf("/api/v1/executions/%d/resume", execID), author, nil)
	if code != http.StatusCreated {
		t.Fatalf("resume: code %d body %v", code, body)
	}
	rbComplete(t, sched, execID, b, rbSeedAttempt(t, dbh, execID, b, 2),
		engine.AttemptSucceed, json.RawMessage(`{"text":"vb"}`))

	t.Run("200 回滚后投影与轮区间锚", func(t *testing.T) {
		code, body := doJSON(t, r, http.MethodGet,
			fmt.Sprintf("/api/v1/executions/%d/nodes/b", execID), author, nil)
		if code != http.StatusOK {
			t.Fatalf("code = %d %v, want 200", code, body)
		}
		if body["status"] != "SUCCEEDED" || body["attempts"] != float64(2) ||
			body["attempts_base"] != float64(1) || body["max_attempts"] != float64(1) {
			t.Fatalf("行视图 = %v, want SUCCEEDED attempts=2 base=1", body)
		}
		// 最新成功轮投影:第二轮输出(第一轮 FAILED 从未上投影)。
		out, _ := body["output"].(map[string]any)
		if out["text"] != "vb" {
			t.Fatalf("output = %v, want 第二轮投影 {text:vb}", body["output"])
		}
		if body["input_snapshot"] == nil {
			t.Fatalf("input_snapshot = nil, want 非空(EXECUTING 后不变量)")
		}
		if body["error_code"] != nil || body["finished_at"] == nil {
			t.Fatalf("error/finished = %v/%v, want 重跑清列后 error 空、finished 在场",
				body["error_code"], body["finished_at"])
		}
		if id, ok := body["node_execution_id"].(string); !ok || id != fmt.Sprint(b) {
			t.Fatalf("node_execution_id = %v(%T), want 字符串化 %d", body["node_execution_id"], body["node_execution_id"], b)
		}
		if body["instance_no"] != float64(0) || body["type"] != "llm" {
			t.Fatalf("instance_no/type = %v/%v, want 0/llm", body["instance_no"], body["type"])
		}
	})

	t.Run("200 parallel 实例行寻址", func(t *testing.T) {
		// 直插 a 的实例行(instance_no=1,0006 uk 三列;output 区分控制行)。
		instID := nextRBID()
		if _, err := dbh.Exec(`INSERT INTO node_execution
		  (id, execution_id, node_id, instance_no, type, status, attempts, worker_id, version, input_snapshot, output)
		  VALUES (?, ?, 'a', 1, 'llm', 'SUCCEEDED', 1, 'w', 1, '{}', '{"text":"i1"}')`,
			instID, execID); err != nil {
			t.Fatalf("seed instance row: %v", err)
		}
		code, body := doJSON(t, r, http.MethodGet,
			fmt.Sprintf("/api/v1/executions/%d/nodes/a?instance_no=1", execID), author, nil)
		if code != http.StatusOK {
			t.Fatalf("instance probe = %d %v, want 200", code, body)
		}
		if out, _ := body["output"].(map[string]any); out["text"] != "i1" {
			t.Fatalf("instance output = %v, want i1", body["output"])
		}
		if id, _ := body["node_execution_id"].(string); id != fmt.Sprint(instID) {
			t.Fatalf("instance node_execution_id = %v, want %d", body["node_execution_id"], instID)
		}
		// 缺省 = 控制行(a 真链行,输出 va)。
		code, body = doJSON(t, r, http.MethodGet,
			fmt.Sprintf("/api/v1/executions/%d/nodes/a", execID), author, nil)
		if code != http.StatusOK {
			t.Fatalf("control probe = %d %v, want 200", code, body)
		}
		if out, _ := body["output"].(map[string]any); out["text"] != "va" {
			t.Fatalf("control output = %v, want va", body["output"])
		}
		// 缺席实例行:404(与「不存在」同面)。
		code, _ = doJSON(t, r, http.MethodGet,
			fmt.Sprintf("/api/v1/executions/%d/nodes/a?instance_no=9", execID), author, nil)
		if code != http.StatusNotFound {
			t.Fatalf("absent instance = %d, want 404", code)
		}
	})

	t.Run("400 形状闸", func(t *testing.T) {
		for _, tc := range []struct{ path, wantErr string }{
			{fmt.Sprintf("/api/v1/executions/%d/nodes/Bad", execID), "bad_node_id"},
			{fmt.Sprintf("/api/v1/executions/%d/nodes/a?instance_no=abc", execID), "bad_instance_no"},
			{fmt.Sprintf("/api/v1/executions/%d/nodes/a?instance_no=70000", execID), "bad_instance_no"},
		} {
			code, body := doJSON(t, r, http.MethodGet, tc.path, author, nil)
			if code != http.StatusBadRequest || body["error"] != tc.wantErr {
				t.Fatalf("%s = %d %v, want 400 %s", tc.path, code, body, tc.wantErr)
			}
		}
	})

	t.Run("404 节点行不存在", func(t *testing.T) {
		code, body := doJSON(t, r, http.MethodGet,
			fmt.Sprintf("/api/v1/executions/%d/nodes/zzz", execID), author, nil)
		if code != http.StatusNotFound || body["error"] != "not_found" {
			t.Fatalf("ghost node = %d %v, want 404 not_found", code, body)
		}
	})
}

// TestHTTPProjectionAttempts:按轮两端点(瘦列表 + 单轮四件套详情)。
func TestHTTPProjectionAttempts(t *testing.T) {
	dbh := openAPITestDB(t)
	schedIDs, err := snowflake.New(6)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	sched, err := scheduler.New(scheduler.Deps{DB: dbh, IDs: schedIDs})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	r, _, sign := newTestServerWithTrigger(t, sched)
	author := sign(auth.RoleAuthor)
	name := uniqName("prj-round")
	rbPublish(t, r, author, name, rbChainDoc(name))
	execID, b := prjDriveMultiRound(t, r, sched, dbh, author, name)

	t.Run("200 按轮全时间线升序(round≡attempt_no)", func(t *testing.T) {
		code, body := doJSON(t, r, http.MethodGet,
			fmt.Sprintf("/api/v1/executions/%d/nodes/b/attempts", execID), author, nil)
		if code != http.StatusOK {
			t.Fatalf("code = %d %v, want 200", code, body)
		}
		items, _ := body["items"].([]any)
		if len(items) != 2 {
			t.Fatalf("items = %d, want 2(旧轮 FAILED + 新轮 SUCCEEDED)", len(items))
		}
		first, _ := items[0].(map[string]any)
		second, _ := items[1].(map[string]any)
		if first["round"] != float64(1) || first["status"] != "FAILED" {
			t.Fatalf("round1 = %v, want FAILED", first)
		}
		if second["round"] != float64(2) || second["status"] != "SUCCEEDED" {
			t.Fatalf("round2 = %v, want SUCCEEDED", second)
		}
		// 瘦列纪律:列表行不携 JSON 大列(缺席而非 null)。
		if _, has := first["output"]; has {
			t.Fatalf("list row carries output, want 瘦列缺席")
		}
		if _, has := first["input_snapshot"]; has {
			t.Fatalf("list row carries input_snapshot, want 瘦列缺席")
		}
		if _, ok := first["attempt_id"].(string); !ok {
			t.Fatalf("attempt_id = %v(%T), want 字符串化", first["attempt_id"], first["attempt_id"])
		}
		if first["finished_at"] == nil {
			t.Fatalf("FAILED round finished_at = nil, want 收口时刻在场")
		}
	})

	t.Run("200 空台账:items 非缺席", func(t *testing.T) {
		code, body := doJSON(t, r, http.MethodGet,
			fmt.Sprintf("/api/v1/executions/%d/nodes/c/attempts", execID), author, nil)
		if code != http.StatusOK {
			t.Fatalf("code = %d %v, want 200", code, body)
		}
		items, ok := body["items"].([]any)
		if !ok || items == nil || len(items) != 0 {
			t.Fatalf("items = %#v, want 非缺席空数组(未派发 ≠ 报错)", body["items"])
		}
	})

	t.Run("200 单轮四件套 + NULL 三义", func(t *testing.T) {
		// round1(直插种子):快照缺席(派发链外直插)、FAILED ⇒ output 恒 null。
		code, body := doJSON(t, r, http.MethodGet,
			fmt.Sprintf("/api/v1/executions/%d/nodes/b/attempts/1", execID), author, nil)
		if code != http.StatusOK {
			t.Fatalf("round1 = %d %v, want 200", code, body)
		}
		if body["round"] != float64(1) || body["status"] != "FAILED" {
			t.Fatalf("round1 = %v, want round=1 FAILED", body)
		}
		if body["input_snapshot"] != nil || body["output"] != nil {
			t.Fatalf("round1 四件套 = %v/%v, want null/null(未物化 + FAILED 未产出)",
				body["input_snapshot"], body["output"])
		}
		// usage/cost 列组 NULL 侧值锚(rb 链失败轮无用量语义,tool 轮同形)。
		if body["model"] != nil || body["prompt_tokens"] != nil || body["cost_amount"] != nil {
			t.Fatalf("round1 usage 列组 = %v/%v/%v, want null 侧(无用量语义)",
				body["model"], body["prompt_tokens"], body["cost_amount"])
		}
		// round2:SUCCEEDED 轮 output = 该轮真实产出。
		code, body = doJSON(t, r, http.MethodGet,
			fmt.Sprintf("/api/v1/executions/%d/nodes/b/attempts/2", execID), author, nil)
		if code != http.StatusOK {
			t.Fatalf("round2 = %d %v, want 200", code, body)
		}
		if out, _ := body["output"].(map[string]any); out["text"] != "vb" {
			t.Fatalf("round2 output = %v, want {text:vb}", body["output"])
		}
	})

	t.Run("200 usage/cost 列组 + DEAD 收口", func(t *testing.T) {
		// 直插 no=3(usage 全载 llm 轮)与 no=4(DEAD/rollback_reset,
		// finished_at 恒 NULL——D-t92 回滚收口形态)。
		prjSeedLLMAttempt(t, dbh, execID, b, 3)
		prjSeedDeadAttempt(t, dbh, execID, b, 4)

		code, body := doJSON(t, r, http.MethodGet,
			fmt.Sprintf("/api/v1/executions/%d/nodes/b/attempts/3", execID), author, nil)
		if code != http.StatusOK {
			t.Fatalf("round3 = %d %v, want 200", code, body)
		}
		if body["model"] != "glm-5.3" || body["prompt_tokens"] != float64(100) ||
			body["total_tokens"] != float64(150) {
			t.Fatalf("usage 列组 = %v, want model/tokens 在场", body)
		}
		if body["cost_amount"] != 0.001234 || body["cost_currency"] != "CNY" || body["estimated"] != true {
			t.Fatalf("cost 列组 = %v/%v/%v, want 0.001234/CNY/true",
				body["cost_amount"], body["cost_currency"], body["estimated"])
		}
		if in, _ := body["input_snapshot"].(map[string]any); in["tpl"] != "b" {
			t.Fatalf("round3 input_snapshot = %v, want {tpl:b}", body["input_snapshot"])
		}

		code, body = doJSON(t, r, http.MethodGet,
			fmt.Sprintf("/api/v1/executions/%d/nodes/b/attempts/4", execID), author, nil)
		if code != http.StatusOK {
			t.Fatalf("round4 = %d %v, want 200", code, body)
		}
		if body["status"] != "DEAD" || body["finished_at"] != nil ||
			body["output"] != nil || body["error_code"] != "rollback_reset" {
			t.Fatalf("DEAD 行 = %v, want finished/output null + rollback_reset", body)
		}

		// 列表随之四行——全位次断言(对抗审计 MEDIUM 修:升序契约此前
		// 仅 2 行序对拍,ref 计划下索引自然序与期望同向,删 ORDER BY 变异
		// 逃逸;位次断言使任何序回归在 DTO 层可见)。
		code, body = doJSON(t, r, http.MethodGet,
			fmt.Sprintf("/api/v1/executions/%d/nodes/b/attempts", execID), author, nil)
		if code != http.StatusOK {
			t.Fatalf("list = %d %v, want 200", code, body)
		}
		items, _ := body["items"].([]any)
		if len(items) != 4 {
			t.Fatalf("items = %d, want 4", len(items))
		}
		wantSeq := []struct {
			round  float64
			status string
		}{{1, "FAILED"}, {2, "SUCCEEDED"}, {3, "SUCCEEDED"}, {4, "DEAD"}}
		for i, w := range wantSeq {
			it, _ := items[i].(map[string]any)
			if it["round"] != w.round || it["status"] != w.status {
				t.Fatalf("items[%d] = round %v/%v, want %v/%v(升序全时间线)", i, it["round"], it["status"], w.round, w.status)
			}
		}
	})

	t.Run("保留期变薄 D-t186:列表变薄、单轮 404、不报错", func(t *testing.T) {
		if _, err := dbh.Exec(`DELETE FROM node_attempt WHERE node_execution_id=? AND attempt_no=1`, b); err != nil {
			t.Fatalf("simulate janitor: %v", err)
		}
		code, body := doJSON(t, r, http.MethodGet,
			fmt.Sprintf("/api/v1/executions/%d/nodes/b/attempts", execID), author, nil)
		if code != http.StatusOK {
			t.Fatalf("thinned list = %d %v, want 200(半边消失不报错)", code, body)
		}
		items, _ := body["items"].([]any)
		if len(items) != 3 {
			t.Fatalf("thinned items = %d, want 3", len(items))
		}
		// 变薄后剩余位次(序锚随变薄面同步,对抗审计 MEDIUM 修)。
		for i, w := range []float64{2, 3, 4} {
			it, _ := items[i].(map[string]any)
			if it["round"] != w {
				t.Fatalf("thinned items[%d].round = %v, want %v", i, it["round"], w)
			}
		}
		code, body = doJSON(t, r, http.MethodGet,
			fmt.Sprintf("/api/v1/executions/%d/nodes/b/attempts/1", execID), author, nil)
		if code != http.StatusNotFound || body["error"] != "not_found" {
			t.Fatalf("cleaned round = %d %v, want 404 not_found(成因同面)", code, body)
		}
	})

	t.Run("400 bad_round", func(t *testing.T) {
		for _, seg := range []string{"0", "abc", "70000"} {
			code, body := doJSON(t, r, http.MethodGet,
				fmt.Sprintf("/api/v1/executions/%d/nodes/b/attempts/%s", execID, seg), author, nil)
			if code != http.StatusBadRequest || body["error"] != "bad_round" {
				t.Fatalf("round %q = %d %v, want 400 bad_round", seg, code, body)
			}
		}
	})
}

// TestHTTPProjectionTenantGate:租户锚三臂 × 三端点——node 系无租户列,
// 唯一合法形状 = GetExecution(tenant,id) 前置后经 execution 锚派生
// (execstore 钦定;跨租户与不存在同面 404 不泄露存在性)。
func TestHTTPProjectionTenantGate(t *testing.T) {
	dbh := openAPITestDB(t)
	seedAdversarialTenant(t, dbh)
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
	name := uniqName("prj-ten")
	rbPublish(t, r, author, name, rbChainDoc(name))
	execID, _ := rbDriveToFailed(t, r, sched, dbh, author, name)
	outsider := signTenant(t, "mallory", adversarialTenantID, auth.RoleAuthor)

	paths := []string{
		fmt.Sprintf("/api/v1/executions/%d/nodes/b", execID),
		fmt.Sprintf("/api/v1/executions/%d/nodes/b/attempts", execID),
		fmt.Sprintf("/api/v1/executions/%d/nodes/b/attempts/1", execID),
	}
	for _, p := range paths {
		code, body := doJSON(t, r, http.MethodGet, p, outsider, nil)
		if code != http.StatusNotFound || body["error"] != "not_found" {
			t.Fatalf("cross-tenant %s = %d %v, want 404 not_found(同面)", p, code, body)
		}
	}
	for _, p := range paths {
		ghost := fmt.Sprintf("/api/v1/executions/1%s", p[len(fmt.Sprintf("/api/v1/executions/%d", execID)):])
		code, _ := doJSON(t, r, http.MethodGet, ghost, author, nil)
		if code != http.StatusNotFound {
			t.Fatalf("ghost %s = %d, want 404", ghost, code)
		}
		code, _ = doJSON(t, r, http.MethodGet, p, author, nil)
		if code == http.StatusNotFound {
			t.Fatalf("own-tenant %s = 404, want 200(控制臂)", p)
		}
	}
}

// prjSeedLLMAttempt 直插 usage/cost 全载 + 快照/输出在场的 SUCCEEDED 轮
// (rbSeedAttempt 只覆盖 RUNNING 裸行;本 helper 钉列组语义)。
func prjSeedLLMAttempt(t *testing.T, dbh *sql.DB, execID, nodeExecID uint64, no uint16) {
	t.Helper()
	id := nextRBID()
	taskID := fmt.Sprintf("att:%d:%d", nodeExecID, no)
	if _, err := dbh.Exec(`INSERT INTO node_attempt
	  (id, node_execution_id, execution_id, attempt_no, worker_id, status,
	   idempotency_key, asynq_task_id, model, prompt_tokens, completion_tokens,
	   total_tokens, cost_amount, cost_currency, estimated, input_snapshot, output,
	   started_at, finished_at)
	  VALUES (?,?,?,?,?,'SUCCEEDED',?,?,'glm-5.3',100,50,150,0.001234,'CNY',1,
	   '{"tpl":"b"}','{"text":"x3"}',NOW(3),NOW(3))`,
		id, nodeExecID, execID, no, "w1", taskID, taskID); err != nil {
		t.Fatalf("seed llm attempt: %v", err)
	}
}

// prjSeedDeadAttempt 直插 DEAD/rollback_reset 收口行(finished_at 恒 NULL,
// D-t92 决策 4⑤ 回滚闭包收口形态)。
func prjSeedDeadAttempt(t *testing.T, dbh *sql.DB, execID, nodeExecID uint64, no uint16) {
	t.Helper()
	id := nextRBID()
	taskID := fmt.Sprintf("att:%d:%d", nodeExecID, no)
	if _, err := dbh.Exec(`INSERT INTO node_attempt
	  (id, node_execution_id, execution_id, attempt_no, worker_id, status,
	   idempotency_key, asynq_task_id, error_code, started_at)
	  VALUES (?,?,?,?,?,'DEAD',?,?,'rollback_reset',NOW(3))`,
		id, nodeExecID, execID, no, "w1", taskID, taskID); err != nil {
		t.Fatalf("seed dead attempt: %v", err)
	}
}
