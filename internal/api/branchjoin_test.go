package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"testing"
	"time"

	"braid/internal/auth"
	"braid/internal/scheduler"
)

func exclusiveJoinDoc(name string) []byte {
	return []byte(fmt.Sprintf(`{"dsl":"1.0","name":%q,"inputs":[{"name":"flag","type":"boolean"}],"nodes":[
 {"id":"route","type":"branch","cases":[{"when":{"left":"{{inputs.flag}}","op":"eq","right":true},"next":"left"}],"default":"right"},
 {"id":"left","type":"approval","approvers":["alice"]},
 {"id":"right","type":"approval","approvers":["alice"]},
 {"id":"final_review","type":"approval","approvers":["alice"]}],
 "edges":[{"from":"route","to":"left"},{"from":"route","to":"right"},{"from":"left","to":"final_review"},{"from":"right","to":"final_review"}]}`, name))
}

func TestDraftRejectsExclusiveJoin(t *testing.T) {
	_, err := parseDraft(exclusiveJoinDoc("join-test"))
	if !HasIssueCode(err, "branch_join") {
		t.Fatalf("parseDraft = %v", err)
	}
}

type branchJoinTrigger struct {
	fakeTrigger
	calls int
}

func (f *branchJoinTrigger) Instantiate(ctx context.Context, p scheduler.InstantiateParams) (uint64, error) {
	f.calls++
	return f.fakeTrigger.Instantiate(ctx, p)
}

func TestHTTPBranchJoinAdmission(t *testing.T) {
	trigger := &branchJoinTrigger{fakeTrigger: fakeTrigger{id: 90001}}
	r, dbh, sign := newTestServerWithTrigger(t, trigger)
	author := sign(auth.RoleAuthor)
	name := uniqName("join")
	bad := exclusiveJoinDoc(name)
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/definitions", author, bad)
	if code != 400 || body["error"] != "invalid_dsl" {
		t.Fatalf("create = %d %v", code, body)
	}
	draft := createViaAPI(t, r, author, dslDoc(name))
	id := draft["id"].(string)
	code, body = doJSON(t, r, http.MethodPut, "/api/v1/drafts/"+id, author, bad)
	if code != 400 {
		t.Fatalf("update = %d %v", code, body)
	}
	// 模拟升级前保存的旧草稿；只在隔离测试 schema 写入历史形状。
	if _, err := dbh.Exec(`UPDATE workflow_definition SET dsl=? WHERE id=?`, bad, id); err != nil {
		t.Fatal(err)
	}
	code, body = doJSON(t, r, http.MethodPost, "/api/v1/drafts/"+id+"/publish", author, nil)
	if code != 400 {
		t.Fatalf("publish legacy draft = %d %v", code, body)
	}
	// 旧发布版本也不得新触发；历史定义读取保持可用。
	if _, err := dbh.Exec(`UPDATE workflow_definition SET dsl=?, status='PUBLISHED', version=1, published_by='alice', published_at=NOW(3) WHERE id=?`, bad, id); err != nil {
		t.Fatal(err)
	}
	code, body = doJSON(t, r, http.MethodGet, "/api/v1/definitions/"+name+"/versions/1", author, nil)
	if code != 200 {
		t.Fatalf("read historical version = %d %v", code, body)
	}
	code, body = doJSON(t, r, http.MethodPost, "/api/v1/executions", author, []byte(fmt.Sprintf(`{"definition_name":%q,"version":1,"input":{"flag":true}}`, name)))
	if code != 400 || body["error"] != "branch_join" || trigger.calls != 0 {
		t.Fatalf("trigger = %d %v calls=%d", code, body, trigger.calls)
	}
	issues, ok := body["issues"].([]any)
	if !ok || len(issues) != 1 {
		t.Fatalf("issues = %v", body)
	}
	issue := issues[0].(map[string]any)
	if issue["node"] != "final_review" || issue["ref"] != "route" {
		t.Fatalf("issue = %v", issue)
	}
}

func TestMCPBranchJoinAdmission(t *testing.T) {
	trigger := &branchJoinTrigger{fakeTrigger: fakeTrigger{id: 90002}}
	s, dbh := newMCPServer(t, func(*sql.DB) ExecutionTrigger { return trigger })
	author, err := auth.SignToken(testSecret, time.Hour, "alice", []auth.Role{auth.RoleAuthor}, 0)
	if err != nil {
		t.Fatal(err)
	}
	name := uniqName("mcp-join")
	draft := createViaAPI(t, s.Router(), author, dslDoc(name))
	if _, err := dbh.Exec(`UPDATE workflow_definition SET dsl=?, status='PUBLISHED', version=1, published_by='alice', published_at=NOW(3) WHERE id=?`, exclusiveJoinDoc(name), draft["id"]); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.mcpTriggerWorkflow(mcpClaimsCtx(testClaims("alice", 0, auth.AllScopes()...)), nil, triggerWorkflowIn{DefinitionName: name})
	if err == nil || err.Error() != "branch_join" || trigger.calls != 0 {
		t.Fatalf("MCP trigger err=%v calls=%d", err, trigger.calls)
	}
}
