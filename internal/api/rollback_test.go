package api

// T9.2 回滚/续跑端点契约面(D-2026-09-28-t92):真 scheduler 链(审批面
// newTestServerWithTrigger(sched) 先例)——分面 401/403/400/404/409、
// 成功 201 回显、拒绝审计 rollback.denied、resume 自动定位语法糖。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"braid/internal/auth"
	"braid/internal/engine"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
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

	t.Run("201 成功 + 重复 409", func(t *testing.T) {
		code, body := doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/rollback", execID), author,
			[]byte(`{"node_id":"b"}`))
		if code != http.StatusCreated {
			t.Fatalf("code = %d %v, want 201", code, body)
		}
		if body["target_node_id"] != "b" || body["rerun_target"] != true {
			t.Fatalf("body = %v, want target b rerun", body)
		}
		// 幂等锚:wf 已 RUNNING → 二次回滚 409(wf CAS 败者收敛面)。
		code, body = doJSON(t, r, http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/rollback", execID), author,
			[]byte(`{"node_id":"b"}`))
		if code != http.StatusConflict {
			t.Fatalf("repeat code = %d %v, want 409", code, body)
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
