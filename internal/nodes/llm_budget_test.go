package nodes

// T18.2 每执行 token 预算闸门消费点测试(D-2026-10-05-t182-token-budget-gate)。
// 直打 LLMExecutor.Run(读快照 → 闸门 → 调用),无需 relay/asynq 全链:
// 闸门语义(拒绝/边界/关闭/审计行)在本层闭合,台账双录的调用后半边归
// e2e 全链。栈缺席自动 skip(t13 契约)。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"braid/internal/audit"
	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/engine/dsl"
	"braid/internal/llm"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
	"braid/internal/worker"
)

// budgetFakeClient 计数调用;被拒路径断言零触达(调用未发出 = 预算拒绝词
// 的存在前提,D-t182 决策 4)。
type budgetFakeClient struct {
	calls int
}

func (c *budgetFakeClient) ChatCompletion(_ context.Context, _ llm.Request) (llm.Response, error) {
	c.calls++
	return llm.Response{
		Model: "glm-5.3", FinishReason: "stop", Content: `{"ok":true}`,
		Usage: llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}, nil
}

// seedBudgetExec 手种最小链:definition(FK RESTRICT)→ execution →
// node_execution(EXECUTING + input_snapshot)→ 若 spent>0 再种一条带
// usage 的 SUCCEEDED attempt。返回 (db, nodeExecID)。
func seedBudgetExec(t *testing.T, idg *snowflake.Generator, template string, spent uint32) (db *sql.DB, execID, nodeExecID uint64) {
	t.Helper()
	db = sqltest.MustOpenAt(t, nodesSchema)
	var err error
	defID, err := idg.Next()
	if err != nil {
		t.Fatalf("gen def id: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', '{}', ?, 't', 't', 't', NOW(3))`,
		defID, fmt.Sprintf("budget-t-%d", defID), string(make([]byte, 64))); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	execID, err = idg.Next()
	if err != nil {
		t.Fatalf("gen exec id: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO workflow_execution
	  (id, definition_id, definition_name, definition_version, definition_hash, status, trigger_type, triggered_by, input)
	  VALUES (?, ?, 'budget-t', 1, ?, 'RUNNING', 'TEST', 't', '{}')`,
		execID, defID, string(make([]byte, 64))); err != nil {
		t.Fatalf("seed execution: %v", err)
	}
	nodeExecID, err = idg.Next()
	if err != nil {
		t.Fatalf("gen node exec id: %v", err)
	}
	snap, err := json.Marshal(scheduler.LLMInput{
		Template: template, Model: "glm-5.3", MaxTokens: 100,
	})
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO node_execution
	  (id, execution_id, node_id, type, status, input_snapshot)
	  VALUES (?, ?, 'sum', 'llm', 'EXECUTING', ?)`,
		nodeExecID, execID, snap); err != nil {
		t.Fatalf("seed node execution: %v", err)
	}
	if spent > 0 {
		attID, err := idg.Next()
		if err != nil {
			t.Fatalf("gen attempt id: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO node_attempt
		  (id, node_execution_id, execution_id, attempt_no, status, idempotency_key,
		   asynq_task_id, model, prompt_tokens, completion_tokens, total_tokens, started_at)
		  VALUES (?, ?, ?, 1, 'SUCCEEDED', ?, ?, 'glm-5.3', ?, ?, ?, NOW(3))`,
			attID, nodeExecID, execID,
			fmt.Sprintf("att:%d:1", nodeExecID+1000), fmt.Sprintf("att:%d:1", nodeExecID+1000),
			spent-30, 30, spent); err != nil {
			t.Fatalf("seed attempt usage: %v", err)
		}
	}
	return db, execID, nodeExecID
}

func budgetExecutor(t *testing.T, db *sql.DB, budget llm.TokenBudget, idg *snowflake.Generator, client *budgetFakeClient) *LLMExecutor {
	t.Helper()
	table, err := llm.ParsePriceTable(
		`{"glm-5.3":{"input_per_mtok":"2","output_per_mtok":"8","currency":"CNY"}}`)
	if err != nil {
		t.Fatalf("price table: %v", err)
	}
	return &LLMExecutor{
		Client: client, Prices: table, DB: db, Budget: budget, IDs: idg,
	}
}

func TestClampActorID(t *testing.T) {
	// 对抗审计 LOW①:DSL model ≤100 码点合法但 CJK 可 >128 字节,actor_id
	// 列宽字节语义——rune 边界裁,产物恒合法 UTF-8 且 ≤128 字节。
	cases := []struct {
		name  string
		in    string
		want  string
		wantN int // 期望输出字节数(0 = 与 want 字符串比对即可)
	}{
		{"ascii fits untouched", "glm-5.3", "glm-5.3", 0},
		{"ascii exact boundary passes", strings.Repeat("a", 128), strings.Repeat("a", 128), 0},
		{"cjk 50 chars cut at rune boundary", strings.Repeat("模", 50), strings.Repeat("模", 42), 0},
		{"cut never splits a rune", "a" + strings.Repeat("模", 50), "a" + strings.Repeat("模", 42), 127},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clampActorID(tc.in)
			if got != tc.want {
				t.Fatalf("clampActorID() = %q (len %d), want %q", got, len(got), tc.want)
			}
			if n := len(got); n > 128 || (tc.wantN != 0 && n != tc.wantN) {
				t.Fatalf("len = %d, want <=128 (expected %d)", n, tc.wantN)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("output not valid UTF-8: %q", got)
			}
		})
	}
}

func TestLLMBudgetGate(t *testing.T) {
	// 估算值测试内独立重导(同 API 重算 = 规格,非实现内窥):template
	// "hello" + max_tokens 100 ⇒ est = priming 2 + overhead 4 + tokens("hello")。
	const template = "hello"
	est, err := llm.EstimatePromptTokens("glm-5.3", renderMessages(scheduler.LLMInput{
		Template: template, Model: "glm-5.3", MaxTokens: 100,
	}))
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	const spent = uint64(400)
	const reserve = uint64(100) // = MaxTokens 声明(D-t182 决策 1:完成预留)
	estU := uint64(est)         // 估算返回 int,与 uint64 常量混算处统一转换

	t.Run("denied over budget, loud, zero provider touch, audited", func(t *testing.T) {
		idg, _ := snowflake.New(1)
		db, execID, nodeExecID := seedBudgetExec(t, idg, template, uint32(spent))
		client := &budgetFakeClient{}
		e := budgetExecutor(t, db, llm.TokenBudget(spent+estU+reserve-1), idg, client)
		out, runErr := e.Run(context.Background(), worker.Request{
			ExecutionID: execID, NodeExecutionID: nodeExecID,
			AttemptID: 777, AttemptNo: 1, TaskID: "att:x:1", WorkerID: "w",
		}, &dsl.LLMNode{Model: "glm-5.3"})
		if runErr != nil {
			t.Fatalf("Run() err = %v, want business failure (nil)", runErr)
		}
		if out.Status != engine.AttemptFailed || out.Code != "llm_budget_exceeded" {
			t.Fatalf("outcome = %+v, want FAILED/llm_budget_exceeded", out)
		}
		if client.calls != 0 {
			t.Fatalf("provider calls = %d, want 0 (pre-call rejection, 零触达)", client.calls)
		}
		var nodeID string
		var attemptID uint64
		var payload json.RawMessage
		var actorType, actorID string
		err := db.QueryRow(`SELECT node_id, attempt_id, actor_type, actor_id, payload
		  FROM audit_log WHERE event_type = ? AND execution_id = ?`,
			audit.EventLLMBudgetDenied, execID).Scan(&nodeID, &attemptID, &actorType, &actorID, &payload)
		if err != nil {
			t.Fatalf("audit row missing: %v", err)
		}
		if nodeID != "sum" || attemptID != 777 || actorType != audit.ActorLLM || actorID != "glm-5.3" {
			t.Fatalf("audit row anchor = (%q,%d,%q,%q), want (sum,777,llm,glm-5.3)",
				nodeID, attemptID, actorType, actorID)
		}
		var p audit.LLMCallBudgetDeniedPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			t.Fatalf("payload unmarshal: %v", err)
		}
		if p.Budget != spent+estU+reserve-1 || p.Spent != spent ||
			p.EstimatedPromptTokens != estU || p.CompletionReserve != reserve ||
			p.Model != "glm-5.3" || p.NodeID != "sum" {
			t.Fatalf("payload = %+v, want budget=%d spent=%d est=%d reserve=%d",
				p, spent+estU+reserve-1, spent, est, reserve)
		}
	})

	t.Run("exact boundary passes", func(t *testing.T) {
		idg, _ := snowflake.New(1)
		db, execID, nodeExecID := seedBudgetExec(t, idg, template, uint32(spent))
		client := &budgetFakeClient{}
		e := budgetExecutor(t, db, llm.TokenBudget(spent+estU+reserve), idg, client)
		out, runErr := e.Run(context.Background(), worker.Request{
			ExecutionID: execID, NodeExecutionID: nodeExecID,
			AttemptID: 778, AttemptNo: 1, TaskID: "att:y:1", WorkerID: "w",
		}, &dsl.LLMNode{Model: "glm-5.3"})
		if runErr != nil {
			t.Fatalf("Run() err = %v, want nil", runErr)
		}
		if out.Status != engine.AttemptSucceed {
			t.Fatalf("outcome = %+v, want SUCCEEDED at boundary (== 含)", out)
		}
		if client.calls != 1 {
			t.Fatalf("provider calls = %d, want 1", client.calls)
		}
	})

	t.Run("gate off by default regardless of spend", func(t *testing.T) {
		idg, _ := snowflake.New(1)
		db, execID, nodeExecID := seedBudgetExec(t, idg, template, uint32(spent))
		client := &budgetFakeClient{}
		e := budgetExecutor(t, db, 0, idg, client) // 缺省关:零查询零估算直通
		out, runErr := e.Run(context.Background(), worker.Request{
			ExecutionID: execID, NodeExecutionID: nodeExecID,
			AttemptID: 779, AttemptNo: 1, TaskID: "att:z:1", WorkerID: "w",
		}, &dsl.LLMNode{Model: "glm-5.3"})
		if runErr != nil {
			t.Fatalf("Run() err = %v, want nil", runErr)
		}
		if out.Status != engine.AttemptSucceed || client.calls != 1 {
			t.Fatalf("outcome = %+v calls = %d, want SUCCEEDED/1 (gate off)", out, client.calls)
		}
	})
}
