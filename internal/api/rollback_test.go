package api

// T9.2 回滚/续跑端点契约面(D-2026-09-28-t92):真 scheduler 链(审批面
// newTestServerWithTrigger(sched) 先例)——分面 401/403/400/404/409、
// 成功 201 回显、拒绝审计 rollback.denied、resume 自动定位语法糖。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"braid/internal/auth"
	"braid/internal/engine"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
	"braid/internal/tools"
	"braid/internal/tracing"
)

// rbChainDoc:a→b→c llm 链(b 的 user_data 引 a 输出;max 缺省 1)。
func rbChainDoc(name string) []byte {
	return fmt.Appendf(nil, `{"dsl":"1.0","name":%q,
"edges":[{"from":"a","to":"b"},{"from":"b","to":"c"}],
"nodes":[
 {"id":"a","type":"llm","prompt":{"template":"a"},"outputs":{"text":"string"}},
 {"id":"b","type":"llm","prompt":{"template":"b","user_data":[{"name":"v","ref":"{{nodes.a.output.text}}"}]},"outputs":{"text":"string"}},
 {"id":"c","type":"llm","prompt":{"template":"c","user_data":[{"name":"v","ref":"{{nodes.b.output.text}}"}]},"outputs":{"text":"string"}}],
"outputs":{"done":"{{nodes.c.output.text}}"}}`, name)
}

// rbPublish 发布自定义 DSL 文档。
func rbPublish(t *testing.T, r *gin.Engine, token, name string, doc []byte) {
	t.Helper()
	created := createViaAPI(t, r, token, doc)
	id := created["id"].(string)
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/drafts/"+id+"/publish", token, nil)
	if code != http.StatusOK {
		t.Fatalf("publish %s: code %d body %v", name, code, body)
	}
}

// rbNodeID 读执行内节点行 id。
func rbNodeID(t *testing.T, dbh *sql.DB, execID uint64, node string) uint64 {
	t.Helper()
	var id uint64
	if err := dbh.QueryRow(`SELECT id FROM node_execution WHERE execution_id=? AND node_id=?`,
		execID, node).Scan(&id); err != nil {
		t.Fatalf("read node %s: %v", node, err)
	}
	return id
}

// rbSeedAttempt 直插 RUNNING attempt 行(worker 面不在链上,镜像 scheduler
// 测试 seedAttempt)并返回行 id。
func rbSeedAttempt(t *testing.T, dbh *sql.DB, execID, nodeExecID uint64, no uint16) uint64 {
	t.Helper()
	id := uint64(time.Now().UnixNano())&0x7FFFFFFFFFFF<<10 | uint64(nodeExecID&0x3FF)
	taskID := fmt.Sprintf("att:%d:%d", nodeExecID, no)
	if _, err := dbh.Exec(`INSERT INTO node_attempt
	  (id, node_execution_id, execution_id, attempt_no, worker_id, status,
	   idempotency_key, asynq_task_id, started_at)
	  VALUES (?,?,?,?,?,'RUNNING',?,?,NOW(3))`,
		id, nodeExecID, execID, no, "w1", taskID, taskID); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
	return id
}

// rbComplete 走真链收口。
func rbComplete(t *testing.T, sched *scheduler.Scheduler, execID, nodeExecID, attemptID uint64,
	status engine.AttemptState, output json.RawMessage) {
	t.Helper()
	if err := sched.Complete(context.Background(), scheduler.CompletionRequest{
		ExecID: execID, NodeExecID: nodeExecID, AttemptID: attemptID,
		Status: status, Output: output, WorkerID: "w1",
	}); err != nil {
		t.Fatalf("Complete %d: %v", nodeExecID, err)
	}
}

// rbDriveToFailed:触发 → Start → a 成功 → b 终败 → wf FAILED。
func rbDriveToFailed(t *testing.T, r *gin.Engine, sched *scheduler.Scheduler, dbh *sql.DB,
	token, name string) (execID uint64, bNodeID uint64) {
	t.Helper()
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/executions", token,
		[]byte(fmt.Sprintf(`{"definition_name":%q,"input":{"q":"hi"}}`, name)))
	if code != http.StatusCreated {
		t.Fatalf("trigger: code %d body %v", code, body)
	}
	execID = uint64(mustParseUint(t, body["id"].(string)))
	if err := sched.Start(context.Background(), execID, "w1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := rbNodeID(t, dbh, execID, "a")
	rbComplete(t, sched, execID, a, rbSeedAttempt(t, dbh, execID, a, 1),
		engine.AttemptSucceed, json.RawMessage(`{"text":"va"}`))
	b := rbNodeID(t, dbh, execID, "b")
	rbComplete(t, sched, execID, b, rbSeedAttempt(t, dbh, execID, b, 1),
		engine.AttemptFailed, nil)
	var st engine.WorkflowState
	if err := dbh.QueryRow(`SELECT status FROM workflow_execution WHERE id=?`, execID).Scan(&st); err != nil || st != engine.WorkflowFailed {
		t.Fatalf("种子 wf = %v(%v), want FAILED", st, err)
	}
	return execID, b
}

// rbDiamondDoc:a→(b,c)→d 菱形(fail-fast 残留形状:b 终败时 c 在飞)。
func rbDiamondDoc(name string) []byte {
	return fmt.Appendf(nil, `{"dsl":"1.0","name":%q,
"edges":[{"from":"a","to":"b"},{"from":"a","to":"c"},{"from":"b","to":"d"},{"from":"c","to":"d"}],
"nodes":[
 {"id":"a","type":"llm","prompt":{"template":"a"},"outputs":{"text":"string"}},
 {"id":"b","type":"llm","prompt":{"template":"b"},"outputs":{"text":"string"}},
 {"id":"c","type":"llm","prompt":{"template":"c"},"outputs":{"text":"string"}},
 {"id":"d","type":"llm","prompt":{"template":"d","user_data":[{"name":"v","ref":"{{nodes.b.output.text}}"},{"name":"w","ref":"{{nodes.c.output.text}}"}]},"outputs":{"text":"string"}}],
"outputs":{"done":"{{nodes.d.output.text}}"}}`, name)
}

// TestHTTPRollbackQuiesce:T19.6(D-t196)HTTP 契约面——无旗标 409 携
// quiesce 出路提示(detail 自描述 + Retry-After 保持);旗标 201 回显
// quiesced_count;重放 200 镜像 quiesced_count(台账整结构体回读)。
func TestHTTPRollbackQuiesce(t *testing.T) {
	dbh := openAPITestDB(t)
	schedIDs, err := snowflake.New(2)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	sched, err := scheduler.New(scheduler.Deps{DB: dbh, IDs: schedIDs})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	r, _, sign := newTestServerWithTrigger(t, sched)
	author := sign(auth.RoleAuthor)
	name := uniqName("rb-quiesce")
	rbPublish(t, r, author, name, rbDiamondDoc(name))

	// 种子:a 成功 → b 终败(fail-fast,wf FAILED、c EXECUTING 残留)。
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/executions", author,
		[]byte(fmt.Sprintf(`{"definition_name":%q,"input":{"q":"hi"}}`, name)))
	if code != http.StatusCreated {
		t.Fatalf("trigger: code %d body %v", code, body)
	}
	execID := uint64(mustParseUint(t, body["id"].(string)))
	if err := sched.Start(context.Background(), execID, "w1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := rbNodeID(t, dbh, execID, "a")
	rbComplete(t, sched, execID, a, rbSeedAttempt(t, dbh, execID, a, 1),
		engine.AttemptSucceed, json.RawMessage(`{"text":"va"}`))
	b := rbNodeID(t, dbh, execID, "b")
	rbComplete(t, sched, execID, b, rbSeedAttempt(t, dbh, execID, b, 1),
		engine.AttemptFailed, nil)
	c := rbNodeID(t, dbh, execID, "c")
	attC := rbSeedAttempt(t, dbh, execID, c, 1)

	t.Run("无旗标 409 携 quiesce 出路", func(t *testing.T) {
		// 原生请求(doJSON 不回 header):Retry-After 与 blocked 清单同锚
		//(观测态契约面,对抗审计 LOW 修——注释声称锚过的面必须真锚)。
		req := httptest.NewRequest(http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/rollback", execID),
			bytes.NewReader([]byte(`{"node_id":"b"}`)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+author)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusConflict {
			t.Fatalf("code = %d body %s, want 409", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Retry-After"); got != "120" {
			t.Fatalf("Retry-After = %q, want 120(reaper settle 量级)", got)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("body: %v", err)
		}
		if body["error"] != "rollback_blocked_inflight" {
			t.Fatalf("error = %v, want rollback_blocked_inflight", body["error"])
		}
		detail, _ := body["detail"].(string)
		if !strings.Contains(detail, "quiesce=true") {
			t.Fatalf("detail = %q, want quiesce=true 出路提示(协议自描述)", detail)
		}
		blocked, _ := body["blocked"].([]any)
		foundC := false
		for _, x := range blocked {
			if s, ok := x.(string); ok && strings.HasPrefix(s, "c:") {
				foundC = true
			}
		}
		if !foundC {
			t.Fatalf("blocked = %v, want 含 c 残行", blocked)
		}
		// denied 行 payload:quiesce=false 镜像(拒绝面的旗标证据锚)。
		var deniedQ sql.NullString
		if err := dbh.QueryRow(`SELECT JSON_UNQUOTE(JSON_EXTRACT(payload,'$.quiesce'))
		  FROM audit_log WHERE execution_id=? AND event_type='rollback.denied'`, execID).
			Scan(&deniedQ); err != nil {
			t.Fatalf("read denied payload: %v", err)
		}
		if deniedQ.String != "false" {
			t.Fatalf("denied quiesce = %q, want false(旗标镜像)", deniedQ.String)
		}
	})

	t.Run("旗标 201 楔停收敛 + 重放 200 镜像", func(t *testing.T) {
		code, body := doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/rollback", execID), author,
			[]byte(`{"node_id":"b","quiesce":true}`))
		if code != http.StatusCreated {
			t.Fatalf("code = %d %v, want 201", code, body)
		}
		if body["quiesced_count"] != float64(1) {
			t.Fatalf("quiesced_count = %v, want 1", body["quiesced_count"])
		}
		// 楔停收口锚:残留 attempt DEAD + code=rollback_quiesce。
		var status, errCode string
		if err := dbh.QueryRow(`SELECT status, error_code FROM node_attempt WHERE id=?`, attC).
			Scan(&status, &errCode); err != nil {
			t.Fatalf("read attempt: %v", err)
		}
		if status != "DEAD" || errCode != "rollback_quiesce" {
			t.Fatalf("attempt = %s/%q, want DEAD/rollback_quiesce", status, errCode)
		}
		// 重放 200:台账 payload 的 quiesced_count 镜像回显。
		code, body = doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/rollback", execID), author,
			[]byte(`{"node_id":"b","quiesce":true}`))
		if code != http.StatusOK {
			t.Fatalf("replay code = %d %v, want 200", code, body)
		}
		if body["quiesced_count"] != float64(1) {
			t.Fatalf("replay quiesced_count = %v, want 1(台账镜像)", body["quiesced_count"])
		}
	})
}

func mustParseUint(t *testing.T, s string) uint64 {
	t.Helper()
	var v uint64
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		t.Fatalf("parse id %q: %v", s, err)
	}
	return v
}

func TestHTTPRollbackEndpoint(t *testing.T) {
	dbh := openAPITestDB(t)
	schedIDs, err := snowflake.New(2)
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
	name := uniqName("rb-api")
	rbPublish(t, r, author, name, rbChainDoc(name))

	execID, _ := rbDriveToFailed(t, r, sched, dbh, author, name)

	t.Run("403 非授权角色 + 拒绝审计", func(t *testing.T) {
		code, body := doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/rollback", execID), viewer,
			[]byte(`{"node_id":"b"}`))
		if code != http.StatusForbidden {
			t.Fatalf("code = %d %v, want 403", code, body)
		}
		var denied int
		if err := dbh.QueryRow(`SELECT COUNT(*) FROM audit_log
			WHERE execution_id=? AND event_type='rollback.denied'`, execID).Scan(&denied); err != nil {
			t.Fatalf("count denied: %v", err)
		}
		if denied != 1 {
			t.Fatalf("rollback.denied 行 = %d, want 1", denied)
		}
	})

	t.Run("400 缺 node_id", func(t *testing.T) {
		code, _ := doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/rollback", execID), author, []byte(`{}`))
		if code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", code)
		}
	})

	t.Run("404 执行不存在", func(t *testing.T) {
		code, _ := doJSON(t, r, http.MethodPost, "/api/v1/executions/1/rollback", author,
			[]byte(`{"node_id":"b"}`))
		if code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", code)
		}
	})

	t.Run("201 成功 + 重复 200 重放", func(t *testing.T) {
		code, body := doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/rollback", execID), author,
			[]byte(`{"node_id":"b"}`))
		if code != http.StatusCreated {
			t.Fatalf("code = %d %v, want 201", code, body)
		}
		if body["target_node_id"] != "b" || body["rerun_target"] != true {
			t.Fatalf("body = %v, want target b rerun", body)
		}
		// T18.5 重放幂等:wf 已 RUNNING → 二次同 target 回滚 200 回显首答
		//(审批同轮重放 200 先例;201-vs-200 状态码即重放信号,零新写)。
		code, body = doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/rollback", execID), author,
			[]byte(`{"node_id":"b"}`))
		if code != http.StatusOK {
			t.Fatalf("repeat code = %d %v, want 200 replay", code, body)
		}
		if body["target_node_id"] != "b" || body["rerun_target"] != true ||
			body["reset_count"] == nil {
			t.Fatalf("replay body = %v, want target b rerun + reset echo", body)
		}
		// 零新写:rolled_back 恰 1 行;重放不灌 denied(not_rollbackable
		// 拒绝行 0——否则 T6.4 denied 计数器/告警把无害重放当拒绝证据)。
		var rolled, denied int
		if err := dbh.QueryRow(`SELECT
			SUM(event_type='execution.rolled_back'),
			COALESCE(SUM(event_type='rollback.denied'
				AND JSON_UNQUOTE(JSON_EXTRACT(payload,'$.reason'))='not_rollbackable'),0)
			FROM audit_log WHERE execution_id=?`, execID).Scan(&rolled, &denied); err != nil {
			t.Fatalf("count audit: %v", err)
		}
		if rolled != 1 || denied != 0 {
			t.Fatalf("rolled_back=%d not_rollbackable_denied=%d, want 1/0", rolled, denied)
		}
	})
}

// TestHTTPRollbackReplayFaces:T18.5 重放裁决分面——重放不是 RUNNING 的
// 后门:无台账(全新 RUNNING,从未回滚)仍 409;同 target 才重放,异
// target 409 落 denied;resume(auto target 无法重解析)重试采信台账
// target 同享重放。
func TestHTTPRollbackReplayFaces(t *testing.T) {
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

	t.Run("无台账 RUNNING 仍 409", func(t *testing.T) {
		name := uniqName("rb-rep0")
		rbPublish(t, r, author, name, rbChainDoc(name))
		code, body := doJSON(t, r, http.MethodPost, "/api/v1/executions", author,
			[]byte(fmt.Sprintf(`{"definition_name":%q,"input":{"q":"hi"}}`, name)))
		if code != http.StatusCreated {
			t.Fatalf("trigger: code %d body %v", code, body)
		}
		execID := uint64(mustParseUint(t, body["id"].(string)))
		if err := sched.Start(context.Background(), execID, "w1"); err != nil {
			t.Fatalf("Start: %v", err)
		}
		// T18.5 ⑦:种 root X 后 denied 行应携 linked 指回(镜像
		// approval.decision_denied 点查先例)。
		const rootX = "0123456789abcdef0123456789abcdef"
		if _, err := dbh.Exec(`UPDATE workflow_execution SET trace_id=? WHERE id=?`, rootX, execID); err != nil {
			t.Fatalf("seed root trace: %v", err)
		}
		code, body = doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/rollback", execID), author,
			[]byte(`{"node_id":"b"}`))
		if code != http.StatusConflict || body["error"] != "not_rollbackable" {
			t.Fatalf("code = %d %v, want 409 not_rollbackable(无重放台账)", code, body)
		}
		var denied int
		var linked sql.NullString
		if err := dbh.QueryRow(`SELECT COUNT(*), MAX(linked_trace_id) FROM audit_log
			WHERE execution_id=? AND event_type='rollback.denied'
			AND JSON_UNQUOTE(JSON_EXTRACT(payload,'$.reason'))='not_rollbackable'`,
			execID).Scan(&denied, &linked); err != nil || denied != 1 {
			t.Fatalf("not_rollbackable denied = %d(%v), want 1", denied, err)
		}
		if !linked.Valid || linked.String != rootX {
			t.Fatalf("denied linked_trace_id = %+v, want root X", linked)
		}
	})

	t.Run("异 target 仍 409 落 denied", func(t *testing.T) {
		name := uniqName("rb-rep1")
		rbPublish(t, r, author, name, rbChainDoc(name))
		execID, _ := rbDriveToFailed(t, r, sched, dbh, author, name)
		code, _ := doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/rollback", execID), author,
			[]byte(`{"node_id":"b"}`))
		if code != http.StatusCreated {
			t.Fatalf("first rollback code = %d, want 201", code)
		}
		// 台账 target=b ≠ 请求 a:非重放,原 409 语义 + denied 证据。
		code, body := doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/rollback", execID), author,
			[]byte(`{"node_id":"a"}`))
		if code != http.StatusConflict || body["error"] != "not_rollbackable" {
			t.Fatalf("code = %d %v, want 409 not_rollbackable(异 target 非重放)", code, body)
		}
		var denied int
		if err := dbh.QueryRow(`SELECT COUNT(*) FROM audit_log
			WHERE execution_id=? AND event_type='rollback.denied'
			AND JSON_UNQUOTE(JSON_EXTRACT(payload,'$.reason'))='not_rollbackable'`,
			execID).Scan(&denied); err != nil || denied != 1 {
			t.Fatalf("not_rollbackable denied = %d(%v), want 1", denied, err)
		}
	})

	t.Run("resume 重试重放", func(t *testing.T) {
		name := uniqName("rb-rep2")
		rbPublish(t, r, author, name, rbChainDoc(name))
		execID, _ := rbDriveToFailed(t, r, sched, dbh, author, name)
		code, _ := doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/resume", execID), author, nil)
		if code != http.StatusCreated {
			t.Fatalf("resume code = %d, want 201", code)
		}
		// 重试的 resume 无法重解析 auto target(wf 已 RUNNING,FAILED 行
		// 已被首答收编)→ 采信台账 target 重放。
		code, body := doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/resume", execID), author, nil)
		if code != http.StatusOK {
			t.Fatalf("resume retry code = %d %v, want 200 replay", code, body)
		}
		if body["target_node_id"] != "b" {
			t.Fatalf("replay target = %v, want b(台账回显)", body["target_node_id"])
		}
		var rolled int
		if err := dbh.QueryRow(`SELECT COUNT(*) FROM audit_log
			WHERE execution_id=? AND event_type='execution.rolled_back'`,
			execID).Scan(&rolled); err != nil || rolled != 1 {
			t.Fatalf("rolled_back = %d(%v), want 1(重放零新写)", rolled, err)
		}
	})

	t.Run("无台账 RUNNING 的 resume 仍 409", func(t *testing.T) {
		// auto target 面的 !found 闸:台账缺席时重放不得凭空回 200 +
		// 空 target(变异自查:删 !found 早退/ErrNoRows 误报 found → 红)。
		name := uniqName("rb-rep3")
		rbPublish(t, r, author, name, rbChainDoc(name))
		code, body := doJSON(t, r, http.MethodPost, "/api/v1/executions", author,
			[]byte(fmt.Sprintf(`{"definition_name":%q,"input":{"q":"hi"}}`, name)))
		if code != http.StatusCreated {
			t.Fatalf("trigger: code %d body %v", code, body)
		}
		execID := uint64(mustParseUint(t, body["id"].(string)))
		if err := sched.Start(context.Background(), execID, "w1"); err != nil {
			t.Fatalf("Start: %v", err)
		}
		code, body = doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/resume", execID), author, nil)
		if code != http.StatusConflict || body["error"] != "not_rollbackable" {
			t.Fatalf("resume code = %d %v, want 409 not_rollbackable(无台账)", code, body)
		}
	})

	t.Run("重选取最新台账", func(t *testing.T) {
		// 双纪元:回滚 b → 重跑再终败 → 回滚 a。台账两行,重放必须重建
		// 最新纪元(ORDER BY 最新锚;变异自查:序倒转 → 取到 b ≠ a → 409 红)。
		name := uniqName("rb-rep4")
		rbPublish(t, r, author, name, rbChainDoc(name))
		execID, bNode := rbDriveToFailed(t, r, sched, dbh, author, name)
		code, _ := doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/rollback", execID), author,
			[]byte(`{"node_id":"b"}`))
		if code != http.StatusCreated {
			t.Fatalf("epoch1 rollback code = %d, want 201", code)
		}
		rbComplete(t, sched, execID, bNode, rbSeedAttempt(t, dbh, execID, bNode, 2),
			engine.AttemptFailed, nil)
		code, _ = doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/rollback", execID), author,
			[]byte(`{"node_id":"a"}`))
		if code != http.StatusCreated {
			t.Fatalf("epoch2 rollback code = %d, want 201", code)
		}
		code, body := doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/rollback", execID), author,
			[]byte(`{"node_id":"a"}`))
		if code != http.StatusOK || body["target_node_id"] != "a" {
			t.Fatalf("latest-ledger replay = %d %v, want 200 target a(最新纪元)", code, body)
		}
	})
}

func TestHTTPResumeEndpoint(t *testing.T) {
	dbh := openAPITestDB(t)
	schedIDs, _ := snowflake.New(3)
	sched, err := scheduler.New(scheduler.Deps{DB: dbh, IDs: schedIDs})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	r, _, sign := newTestServerWithTrigger(t, sched)
	author := sign(auth.RoleAuthor)
	name := uniqName("rb-resume")
	rbPublish(t, r, author, name, rbChainDoc(name))
	execID, _ := rbDriveToFailed(t, r, sched, dbh, author, name)

	// 无 body 自动定位(字典序首个 FAILED = b)→ 201 回显 target。
	code, body := doJSON(t, r, http.MethodPost,
		fmt.Sprintf("/api/v1/executions/%d/resume", execID), author, nil)
	if code != http.StatusCreated {
		t.Fatalf("code = %d %v, want 201", code, body)
	}
	if body["target_node_id"] != "b" {
		t.Fatalf("auto target = %v, want b", body["target_node_id"])
	}
}

// TestHTTPDecideCrossRoundNewVote:H5 锚(对抗审计 HIGH 修)——回滚重挂后
// (挂起轮 attempts=2),旧轮决定行(round=1)对新轮判定面不可见:同审批人
// 再投 = 新票 201(非重放 200 / 非改判 409),或签首票定局按本轮计数。
func TestHTTPDecideCrossRoundNewVote(t *testing.T) {
	dbh := openAPITestDB(t)
	schedIDs, _ := snowflake.New(4)
	sched, err := scheduler.New(scheduler.Deps{DB: dbh, IDs: schedIDs})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	r, _, _ := newTestServerWithTrigger(t, sched)
	publisher := signSubject(t, "author1", auth.RoleAuthor)
	author := signSubject(t, "alice", auth.RoleApprover)

	// 种子:RUNNING 执行 + WA 审批节点(attempts=2 = 回滚重挂后的第二轮
	// 挂起)+ 旧轮票(round=1,alice 已批)。
	name := fmt.Sprintf("rb-round-%d", nextRBID())
	rbPublish(t, r, publisher, name, fmt.Appendf(nil, `{"dsl":"1.0","name":%q,
 "nodes":[{"id":"boss","type":"approval","approvers":["alice"],"mode":"any"}],"outputs":{}}`, name))
	var defID uint64
	if err := dbh.QueryRow(`SELECT id FROM workflow_definition WHERE name=?`, name).Scan(&defID); err != nil {
		t.Fatalf("read definition: %v", err)
	}
	execID := nextRBID()
	if _, err := dbh.Exec(`INSERT INTO workflow_execution
	  (id, definition_id, definition_name, definition_version, definition_hash, status, triggered_by, input)
	  SELECT ?, id, name, version, dsl_hash, 'RUNNING', 'seed', '{}' FROM workflow_definition WHERE id=?`,
		execID, defID); err != nil {
		t.Fatalf("seed execution: %v", err)
	}
	nodeExecID := nextRBID()
	if _, err := dbh.Exec(`INSERT INTO node_execution
	  (id, execution_id, node_id, type, status, attempts, worker_id, version, deadline_at, input_snapshot)
	  VALUES (?, ?, 'boss', 'approval', 'WAITING_APPROVAL', 2, 'w', 1, NOW(3) + INTERVAL 72 HOUR, ?)`,
		nodeExecID, execID, `{"approvers":["alice"],"mode":"any","timeout_hours":72}`); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	decID := nextRBID()
	if _, err := dbh.Exec(`INSERT INTO approval_decision
	  (id, execution_id, node_execution_id, node_id, round, decided_by, decision, decided_at)
	  VALUES (?,?,?, 'boss', 1, 'alice', 'APPROVED', NOW(3))`,
		decID, execID, nodeExecID); err != nil {
		t.Fatalf("seed old-round decision: %v", err)
	}

	code, body := doJSON(t, r, http.MethodPost,
		fmt.Sprintf("/api/v1/executions/%d/approvals/boss/decision", execID), author,
		[]byte(`{"decision":"approve"}`))
	if code != http.StatusCreated {
		t.Fatalf("cross-round vote = %d %v, want 201 新票(旧轮不可见)", code, body)
	}
	if body["round"] != float64(2) {
		t.Fatalf("round = %v, want 2(锚定当轮挂起)", body["round"])
	}
	// 同轮重放语义保持:同 (approver, round=2) 再投 → 200 重放。
	code, body = doJSON(t, r, http.MethodPost,
		fmt.Sprintf("/api/v1/executions/%d/approvals/boss/decision", execID), author,
		[]byte(`{"decision":"approve"}`))
	if code != http.StatusOK {
		t.Fatalf("same-round replay = %d %v, want 200", code, body)
	}
}

func nextRBID() uint64 {
	return uint64(time.Now().UnixNano())&0x7FFFFFFFFFFF<<10 | uint64(atomic.AddUint32(&rbIDSeq, 1))
}

var rbIDSeq uint32

// TestHTTPRollbackTooLarge:T15.5 容量闸 api 面——行集超 MaxRollbackRows 时
// 409 rollback_too_large + denied 审计(reason=too_large);resume 同漏斗继承。
func TestHTTPRollbackTooLarge(t *testing.T) {
	dbh := openAPITestDB(t)
	schedIDs, err := snowflake.New(2)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	sched, err := scheduler.New(scheduler.Deps{DB: dbh, IDs: schedIDs, MaxRollbackRows: 2})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	r, _, sign := newTestServerWithTrigger(t, sched)
	author := sign(auth.RoleAuthor)
	name := uniqName("rb-cap")
	rbPublish(t, r, author, name, rbChainDoc(name))
	execID, _ := rbDriveToFailed(t, r, sched, dbh, author, name)

	code, body := doJSON(t, r, http.MethodPost,
		fmt.Sprintf("/api/v1/executions/%d/rollback", execID), author, []byte(`{"node_id":"b"}`))
	if code != http.StatusConflict {
		t.Fatalf("rollback: code %d body %v, want 409", code, body)
	}
	if body["error"] != "rollback_too_large" {
		t.Fatalf("error = %v, want rollback_too_large", body["error"])
	}
	var denied int
	if err := dbh.QueryRow(`SELECT COUNT(*) FROM audit_log
		WHERE execution_id=? AND event_type='rollback.denied' AND actor_id=?`,
		execID, "alice").Scan(&denied); err != nil || denied != 1 {
		t.Fatalf("rollback.denied rows = %d(%v), want 1", denied, err)
	}
	var reason string
	if err := dbh.QueryRow(`SELECT JSON_UNQUOTE(JSON_EXTRACT(payload,'$.reason'))
		FROM audit_log WHERE execution_id=? AND event_type='rollback.denied'`, execID).Scan(&reason); err != nil || reason != "too_large" {
		t.Fatalf("denied reason = %q(%v), want too_large", reason, err)
	}

	// resume 同漏斗:无 body 自动定位,同样落容量闸。
	code, body = doJSON(t, r, http.MethodPost,
		fmt.Sprintf("/api/v1/executions/%d/resume", execID), author, nil)
	if code != http.StatusConflict || body["error"] != "rollback_too_large" {
		t.Fatalf("resume: code %d body %v, want 409 rollback_too_large", code, body)
	}
}

// TestHTTPRollbackTraceLineage:T18.5 ⑦ 世系——真请求 span 两端:触发请求
// root X(经 InstantiateParams 落 workflow_execution.trace_id)、回滚请求
// 段链 Z(rolled_back 行自身 trace);linked=X 指回主链(审批 decided 段首
// 单点先例);重跑任务 outbox 行 trace=Z(重跑段与回滚请求同链,审批
// resume 事件同形)。Provider = ExporterOff(trace id 照生成零导出,
// doc-example e2e 同式)。
func TestHTTPRollbackTraceLineage(t *testing.T) {
	dbh := openAPITestDB(t)
	cleanExecutionChain(t, dbh)
	schedIDs, err := snowflake.New(6)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	sched, err := scheduler.New(scheduler.Deps{DB: dbh, IDs: schedIDs})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	prov, err := tracing.NewProvider(tracing.Config{
		Exporter: tracing.ExporterOff, ServiceName: "braid-api-test"})
	if err != nil {
		t.Fatalf("trace provider: %v", err)
	}
	t.Cleanup(func() { _ = prov.Shutdown(context.Background()) })
	// 本地装配(镜像 newTestServerWithTrigger,唯一差异 = Tracing 面;
	// 该 helper 不暴露 tracer 注入口)。
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	reg, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	cfg := Config{Addr: DefaultAddr, JWTSecret: testSecret, SnowflakeNode: snowflake.Explicit(1),
		LoginRateLimit: 100000, LoginRateWindow: time.Minute,
		LoginMaxFailures: 100000, LoginLockout: time.Minute}
	srv := NewServer(cfg, dbh, ids, reg, sched, auth.BuiltinProvider{DB: dbh})
	srv.Tracing(prov.Tracer("braid/api"))
	r := srv.Router()
	author, err := auth.SignToken(testSecret, time.Hour, "alice", []auth.Role{auth.RoleAuthor}, 0)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	name := uniqName("rb-trace")
	rbPublish(t, r, author, name, rbChainDoc(name))
	execID, bNode := rbDriveToFailed(t, r, sched, dbh, author, name)

	// X = 触发请求 root trace(应用层强制;api 触发路径非空)。
	var x string
	if err := dbh.QueryRow(`SELECT trace_id FROM workflow_execution WHERE id=?`, execID).
		Scan(&x); err != nil || x == "" {
		t.Fatalf("workflow_execution.trace_id = %q err=%v, want 非空(API 触发 root)", x, err)
	}

	code, _ := doJSON(t, r, http.MethodPost,
		fmt.Sprintf("/api/v1/executions/%d/rollback", execID), author,
		[]byte(`{"node_id":"b"}`))
	if code != http.StatusCreated {
		t.Fatalf("rollback code = %d, want 201", code)
	}

	// 段首面:rolled_back 行 trace=Z(非空、≠X)+ linked=X。
	var z, linked sql.NullString
	if err := dbh.QueryRow(`SELECT trace_id, linked_trace_id FROM audit_log
	  WHERE execution_id=? AND event_type='execution.rolled_back'`, execID).Scan(&z, &linked); err != nil {
		t.Fatalf("read rolled_back: %v", err)
	}
	if !z.Valid || z.String == "" || z.String == x {
		t.Fatalf("rolled_back trace = %v, want 非空且 ≠ root(回滚请求段链 Z)", z)
	}
	if !linked.Valid || linked.String != x {
		t.Fatalf("rolled_back linked = %v, want root X(段首指回主链)", linked)
	}

	// 重跑任务面:RerunNode 派发的 att 任务 outbox 行 trace=Z——重跑段与
	// 回滚请求同链(worker 消费侧 remote parent 续 Z,审批 resume 同形)。
	var taskTrace sql.NullString
	if err := dbh.QueryRow(`SELECT trace_id FROM outbox
	  WHERE execution_id=? AND task_id=?`, execID, fmt.Sprintf("att:%d:%d", bNode, 2)).Scan(&taskTrace); err != nil {
		t.Fatalf("read rerun outbox row: %v", err)
	}
	if !taskTrace.Valid || taskTrace.String != z.String {
		t.Fatalf("rerun outbox trace = %v, want Z=%v(重跑段同链)", taskTrace, z.String)
	}
}

// TestHTTPRollbackConflictReplay:T18.5 重放收敛第二形态——裸 ErrConflict
// (并发双回滚败者 CAS 形态)经 fakeTrigger 注入钉 handler 接线;终态
// 复核闸用「台账在场但 wf 已终态」的执行锚定(变异自查:删 ErrConflict
// 分支 → 200 臂红;删终态复核 → conflict 臂红为 200)。种子走真 scheduler
// 链,server B(fakeTrigger)只读不写——两 server 共享 dbh。
func TestHTTPRollbackConflictReplay(t *testing.T) {
	dbh := openAPITestDB(t)
	schedIDs, err := snowflake.New(8)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	sched, err := scheduler.New(scheduler.Deps{DB: dbh, IDs: schedIDs})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	reg, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	cfg := Config{Addr: DefaultAddr, JWTSecret: testSecret, SnowflakeNode: snowflake.Explicit(1),
		LoginRateLimit: 100000, LoginRateWindow: time.Minute,
		LoginMaxFailures: 100000, LoginLockout: time.Minute}
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	// 种子 server(真 trigger:发布 + 触发 + 真回滚落台账)。
	seedSrv := NewServer(cfg, dbh, ids, reg, sched, auth.BuiltinProvider{DB: dbh})
	seedR := seedSrv.Router()
	author, err := auth.SignToken(testSecret, time.Hour, "alice", []auth.Role{auth.RoleAuthor}, 0)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	// 执行一:FAILED → 真回滚 → RUNNING + 台账(重放成功臂的种子)。
	name := uniqName("rb-cfl1")
	rbPublish(t, seedR, author, name, rbChainDoc(name))
	execRunning, _ := rbDriveToFailed(t, seedR, sched, dbh, author, name)
	if _, err := sched.RollbackTo(context.Background(), scheduler.RollbackParams{
		ExecID: execRunning, TargetNodeID: "b", WorkerID: "api-rb", ActorID: "op"}); err != nil {
		t.Fatalf("seed rollback: %v", err)
	}

	// 执行二:FAILED → 真回滚 → 重跑再终败 → 台账在场但 wf FAILED
	//(终态复核臂的种子:终态时 ErrConflict 保持冲突语义不重放)。
	name2 := uniqName("rb-cfl2")
	rbPublish(t, seedR, author, name2, rbChainDoc(name2))
	execFailed, b2 := rbDriveToFailed(t, seedR, sched, dbh, author, name2)
	if _, err := sched.RollbackTo(context.Background(), scheduler.RollbackParams{
		ExecID: execFailed, TargetNodeID: "b", WorkerID: "api-rb", ActorID: "op"}); err != nil {
		t.Fatalf("seed rollback: %v", err)
	}
	rbComplete(t, sched, execFailed, b2, rbSeedAttempt(t, dbh, execFailed, b2, 2),
		engine.AttemptFailed, nil)
	var st engine.WorkflowState
	if err := dbh.QueryRow(`SELECT status FROM workflow_execution WHERE id=?`, execFailed).
		Scan(&st); err != nil || st != engine.WorkflowFailed {
		t.Fatalf("种子二 wf = %v(%v), want FAILED(台账在场+终态)", st, err)
	}

	// 被测 server:fakeTrigger 注入裸 ErrConflict(并发败者 CAS 形态)。
	conflictSrv := NewServer(cfg, dbh, ids, reg,
		fakeTrigger{err: engine.ErrConflict}, auth.BuiltinProvider{DB: dbh})
	cr := conflictSrv.Router()

	code, body := doJSON(t, cr, http.MethodPost,
		fmt.Sprintf("/api/v1/executions/%d/rollback", execRunning), author,
		[]byte(`{"node_id":"b"}`))
	if code != http.StatusOK {
		t.Fatalf("conflict-loser replay code = %d %v, want 200(RUNNING+台账匹配)", code, body)
	}
	if body["target_node_id"] != "b" {
		t.Fatalf("replay body = %v, want target b", body)
	}

	code, body = doJSON(t, cr, http.MethodPost,
		fmt.Sprintf("/api/v1/executions/%d/rollback", execFailed), author,
		[]byte(`{"node_id":"b"}`))
	if code != http.StatusConflict || body["error"] != "conflict" {
		t.Fatalf("terminal code = %d %v, want 409 conflict(终态复核拒重放)", code, body)
	}
}
