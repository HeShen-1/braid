package nodes

// T3.6 端到端验收:e2e 经 OpenAI 兼容端点(httptest 桩)跑通 llm 节点全链。
// 本包直测(nodes 产线 import worker,同包测试无环):Instantiate 真调度器
// → outbox → 真 relay → 真 asynq server → Mux → LLMExecutor → llm.OpenAICompat
// 生产适配器 → 桩端点。请求形态断言(json_object 方言翻译、幂等键、prompt
// 标签块渲染)与 outputs 声明抽取都在真路径验证;台账双录逐字核对。
//
// 变异自查:适配器不翻译词表 → 桩断言 response_format.type=json_object 红;
// 渲染丢标签块 → messages 断言红;outputs 抽取不校验类型 → 节点 output 红;
// 台账桥断链 → attempt/audit 两半边断言红;Mux 路由失灵 → 执行卡 RUNNING
// 轮询超时红。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"braid/internal/audit"
	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/llm"
	"braid/internal/redistest"
	"braid/internal/relay"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
	"braid/internal/worker"
)

func TestE2ELLMNodeFullChain(t *testing.T) {
	d := sqltest.MustOpenAt(t, nodesSchema)
	ctx := context.Background()
	queue := fmt.Sprintf("t_n%d", atomic.AddUint32(&queueSeq, 1))

	const (
		docText    = "braid 是一个工作流编排平台"
		summary    = "braid 是编排平台"
		wantAmount = "0.003200" // T3.1 手算锚:(1000×2+150×8)/1e6 CNY
	)

	// 1) 桩 OpenAI 兼容端点:真适配器的对手方。请求形态逐项断言。
	var calls atomic.Int64
	var gotBody map[string]any
	var gotAuth, gotIdem, gotPath string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotIdem = r.Header.Get("Idempotency-Key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp-e2e", "model": "glm-5.3",
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant",
					"content": fmt.Sprintf(`{"summary":%q,"count":3}`, summary)},
			}},
			"usage": map[string]any{"prompt_tokens": 1000, "completion_tokens": 150, "total_tokens": 1150},
		})
	}))
	t.Cleanup(stub.Close)

	// 2) 真生产件:适配器 + Mux + LLMExecutor。
	llmCfg := llm.DefaultConfig()
	llmCfg.BaseURL = stub.URL
	llmCfg.APIKey = "sk-e2e"
	table, err := llm.ParsePriceTable(
		`{"glm-5.3":{"input_per_mtok":"2","output_per_mtok":"8","currency":"CNY"}}`)
	if err != nil {
		t.Fatalf("price table: %v", err)
	}
	llmCfg.PriceTable = table
	client, err := llm.NewOpenAICompat(llmCfg)
	if err != nil {
		t.Fatalf("llm client: %v", err)
	}
	executor := NewMux(mustE2ESched(t, d, queue), &LLMExecutor{
		Client: client, Prices: table, DB: d,
	}, &fakeToolExec{}, ApprovalExecutor{}) // llm e2e 不触 tool/approval 路由,装配完整即可

	// 3) 定义 + 物化(调度器与 worker 同队列)。
	sched := mustE2ESched(t, d, queue)
	defID, execID := seedLLMExec(t, d, sched, docText)
	t.Cleanup(func() { cleanupNodesE2E(t, d, execID, defID) })

	// 4) 真 relay + 真 worker(生产路径)。
	relayCfg := relay.DefaultConfig()
	relayCfg.IdleInterval = 20 * time.Millisecond
	relayCfg.EnqueueTimeout = 5 * time.Second
	rctx, relayStop := context.WithCancel(ctx)
	relayDone := make(chan error, 1)
	go func() {
		relayDone <- relay.Run(rctx, relay.Deps{
			DB:       d,
			Enqueuer: relay.NewAsynqEnqueuer(redistest.MustOpt(t)),
		}, relayCfg)
	}()
	t.Cleanup(func() {
		relayStop()
		select {
		case err := <-relayDone:
			if err != nil {
				t.Errorf("relay.Run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("relay.Run did not stop within 15s after cancel")
		}
	})
	workerCfg := worker.DefaultConfig()
	workerCfg.HeartbeatInterval = 40 * time.Millisecond
	startE2EWorker(t, d, executor, workerCfg, queue)

	// 5) 全链推进到终态 + 取 node_execution id(幂等键精确断言用)。
	waitE2EExecSucceeded(t, d, execID)
	assertE2ENodeState(t, d, execID, "sum", engine.NodeSucceeded)
	var nodeExecID uint64
	if err := d.QueryRow(`SELECT id FROM node_execution WHERE execution_id=? AND node_id=?`,
		execID, "sum").Scan(&nodeExecID); err != nil {
		t.Fatalf("read node_execution id: %v", err)
	}

	// 6) 桩请求形态:方言翻译、幂等键、渲染、协议字段全在真路径上核。
	if got := calls.Load(); got != 1 {
		t.Fatalf("stub calls = %d, want 1(单 attempt 单调用)", got)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("stub path = %q", gotPath)
	}
	if gotAuth != "Bearer sk-e2e" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if want := fmt.Sprintf("att:%d:1", nodeExecID); gotIdem != want {
		t.Errorf("Idempotency-Key = %q, want %q(attempt 幂等键精确形态)", gotIdem, want)
	}
	if rf, ok := gotBody["response_format"].(map[string]any); !ok || rf["type"] != "json_object" {
		t.Errorf("response_format = %v, want type=json_object(HIGH② 适配器翻译)", gotBody["response_format"])
	}
	if gotBody["model"] != "glm-5.3" || gotBody["temperature"] != 0.3 || gotBody["max_tokens"] != float64(256) {
		t.Errorf("protocol fields = %v", gotBody)
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v, want system+user", msgs)
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "你是助手" {
		t.Errorf("system message = %v", msgs[0])
	}
	usr, _ := msgs[1].(map[string]any)
	wantUser := "总结以下文档\n\ndoc: " + docText
	if usr["role"] != "user" || usr["content"] != wantUser {
		t.Errorf("user message = %q, want %q(prompt 标签块渲染)", usr["content"], wantUser)
	}

	// 7) outputs 声明抽取:节点 output 与工作流 outputs 物化同源。
	var rawOut []byte
	if err := d.QueryRow(`SELECT output FROM node_execution WHERE execution_id=? AND node_id=?`,
		execID, "sum").Scan(&rawOut); err != nil {
		t.Fatalf("read node output: %v", err)
	}
	var nodeOut map[string]any
	if err := json.Unmarshal(rawOut, &nodeOut); err != nil {
		t.Fatalf("node output not json: %v", err)
	}
	if nodeOut["summary"] != summary || nodeOut["count"] != float64(3) {
		t.Errorf("node output = %v, want summary=%q count=3", nodeOut, summary)
	}
	var wfOutRaw sql.NullString
	if err := d.QueryRow(`SELECT outputs FROM workflow_execution WHERE id=?`, execID).
		Scan(&wfOutRaw); err != nil {
		t.Fatalf("read workflow outputs: %v", err)
	}
	var wfOut map[string]any
	if err := json.Unmarshal([]byte(wfOutRaw.String), &wfOut); err != nil {
		t.Fatalf("workflow outputs: %v", err)
	}
	if wfOut["s"] != summary || wfOut["n"] != float64(3) {
		t.Errorf("workflow outputs = %v", wfOut)
	}

	// 8) 台账双录:真实调用路径产出,两半边逐字核对。
	assertLLMLedger(t, d, nodeExecID, wantAmount)
}

// TestE2ELLMNodeFailurePath:失败半边真链路——桩返回非 JSON content,声明
// outputs 的节点 FAILED(bad_llm_output)收口:错误码落列、usage 列恒 NULL
// (失败不计费)、无 llm.call.completed 审计行、执行 fail-fast 终态。
func TestE2ELLMNodeFailurePath(t *testing.T) {
	d := sqltest.MustOpenAt(t, nodesSchema)
	ctx := context.Background()
	queue := fmt.Sprintf("t_n%d", atomic.AddUint32(&queueSeq, 1))

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp-bad", "model": "glm-5.3",
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "definitely not json"},
			}},
			"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120},
		})
	}))
	t.Cleanup(stub.Close)

	llmCfg := llm.DefaultConfig()
	llmCfg.BaseURL = stub.URL
	llmCfg.APIKey = "sk-e2e"
	table, err := llm.ParsePriceTable(
		`{"glm-5.3":{"input_per_mtok":"2","output_per_mtok":"8","currency":"CNY"}}`)
	if err != nil {
		t.Fatalf("price table: %v", err)
	}
	llmCfg.PriceTable = table
	client, err := llm.NewOpenAICompat(llmCfg)
	if err != nil {
		t.Fatalf("llm client: %v", err)
	}
	executor := NewMux(mustE2ESched(t, d, queue), &LLMExecutor{
		Client: client, Prices: table, DB: d,
	}, &fakeToolExec{}, ApprovalExecutor{}) // llm e2e 不触 tool/approval 路由,装配完整即可

	sched := mustE2ESched(t, d, queue)
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("seed snowflake: %v", err)
	}
	defID, err := ids.Next()
	if err != nil {
		t.Fatalf("gen def id: %v", err)
	}
	dslDoc := fmt.Sprintf(`{"dsl":"1.0","name":"e2e-llm-bad-%d",
 "inputs":[{"name":"doc","type":"string"}],
 "nodes":[
  {"id":"gen","type":"llm","model":"glm-5.3",
   "prompt":{"template":"输出 JSON","user_data":[{"name":"doc","ref":"{{inputs.doc}}"}]},
   "outputs":{"s":"string"}}]}`, defID)
	if _, err := d.Exec(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', ?, ?, 'e2e', 'e2e', 'e2e', NOW(3))`,
		defID, fmt.Sprintf("e2e-llm-bad-%d", defID), dslDoc, string(make([]byte, 64))); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	execID, err := sched.Instantiate(ctx, scheduler.InstantiateParams{
		DefinitionID: defID, Input: json.RawMessage(`{"doc":"x"}`),
		TriggeredBy: "e2e", TriggerType: "TEST",
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	t.Cleanup(func() { cleanupNodesE2E(t, d, execID, defID) })

	relayCfg := relay.DefaultConfig()
	relayCfg.IdleInterval = 20 * time.Millisecond
	relayCfg.EnqueueTimeout = 5 * time.Second
	rctx, relayStop := context.WithCancel(ctx)
	relayDone := make(chan error, 1)
	go func() {
		relayDone <- relay.Run(rctx, relay.Deps{
			DB:       d,
			Enqueuer: relay.NewAsynqEnqueuer(redistest.MustOpt(t)),
		}, relayCfg)
	}()
	t.Cleanup(func() {
		relayStop()
		select {
		case err := <-relayDone:
			if err != nil {
				t.Errorf("relay.Run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("relay.Run did not stop within 15s after cancel")
		}
	})
	workerCfg := worker.DefaultConfig()
	workerCfg.HeartbeatInterval = 40 * time.Millisecond
	startE2EWorker(t, d, executor, workerCfg, queue)

	// 执行 fail-fast 到 FAILED 终态(单节点无 retry,attempt 1 即终)。
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var status engine.WorkflowState
		if err := d.QueryRow(`SELECT status FROM workflow_execution WHERE id=?`, execID).
			Scan(&status); err == nil && status == engine.WorkflowFailed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	assertE2ENodeState(t, d, execID, "gen", engine.NodeFailed)

	var nodeCode, attCode sql.NullString
	var usageNull sql.NullInt64 // prompt_tokens NULL 性质即"未计费"
	var attempts uint16
	if err := d.QueryRow(`
SELECT ne.error_code, na.error_code, na.prompt_tokens, ne.attempts
  FROM node_execution ne JOIN node_attempt na ON na.node_execution_id = ne.id
 WHERE ne.execution_id=? AND ne.node_id='gen'`, execID).
		Scan(&nodeCode, &attCode, &usageNull, &attempts); err != nil {
		t.Fatalf("read failure rows: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1(无 retry 声明,失败即终)", attempts)
	}
	if !nodeCode.Valid || nodeCode.String != "bad_llm_output" || !attCode.Valid || attCode.String != "bad_llm_output" {
		t.Fatalf("error codes = node %v / attempt %v, want bad_llm_output both", nodeCode, attCode)
	}
	if usageNull.Valid {
		t.Fatalf("prompt_tokens = %v on FAILED attempt, want NULL(失败不计费)", usageNull)
	}
	var audited int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND event_type=?`,
		execID, audit.EventLLMCallCompleted).Scan(&audited); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if audited != 0 {
		t.Fatalf("llm.call.completed rows = %d, want 0(仅 SUCCEEDED 发射)", audited)
	}
}

// ---- e2e 基建(nodes 包精简版;worker 包同款语义) ----

var queueSeq uint32

func mustE2ESched(t *testing.T, d *sql.DB, queue string) *scheduler.Scheduler {
	t.Helper()
	ids, err := snowflake.New(2)
	if err != nil {
		t.Fatalf("scheduler snowflake: %v", err)
	}
	s, err := scheduler.New(scheduler.Deps{DB: d, IDs: ids, Queue: queue})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	return s
}

func seedLLMExec(t *testing.T, d *sql.DB, sched *scheduler.Scheduler, docText string) (defID, execID uint64) {
	t.Helper()
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("seed snowflake: %v", err)
	}
	defID, err = ids.Next()
	if err != nil {
		t.Fatalf("gen def id: %v", err)
	}
	dslDoc := fmt.Sprintf(`{"dsl":"1.0","name":"e2e-llm-%d",
 "inputs":[{"name":"doc","type":"string"}],
 "nodes":[
  {"id":"sum","type":"llm","model":"glm-5.3","temperature":0.3,"max_tokens":256,
   "response_format":"json",
   "prompt":{"system":"你是助手","template":"总结以下文档",
             "user_data":[{"name":"doc","ref":"{{inputs.doc}}"}]},
   "outputs":{"summary":"string","count":"number"}}],
 "outputs":{"s":"{{nodes.sum.output.summary}}","n":"{{nodes.sum.output.count}}"}}`, defID)
	if _, err := d.Exec(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', ?, ?, 'e2e', 'e2e', 'e2e', NOW(3))`,
		defID, fmt.Sprintf("e2e-llm-%d", defID), dslDoc, string(make([]byte, 64))); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	execID, err = sched.Instantiate(context.Background(), scheduler.InstantiateParams{
		DefinitionID: defID, Input: json.RawMessage(`{"doc":"` + docText + `"}`),
		TriggeredBy: "e2e", TriggerType: "TEST",
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	return defID, execID
}

func cleanupNodesE2E(t *testing.T, d *sql.DB, execID, defID uint64) {
	t.Helper()
	for _, q := range []string{
		`DELETE FROM outbox WHERE execution_id=?`,
		`DELETE FROM node_attempt WHERE execution_id=?`,
		`DELETE FROM approval_decision WHERE execution_id=?`,
		`DELETE FROM node_execution WHERE execution_id=?`,
		`DELETE FROM workflow_execution WHERE id=?`,
		`DELETE FROM audit_log WHERE execution_id=?`,
	} {
		if _, err := d.Exec(q, execID); err != nil {
			t.Errorf("cleanup %q: %v", q, err)
		}
	}
	if _, err := d.Exec(`DELETE FROM workflow_definition WHERE id=?`, defID); err != nil {
		t.Errorf("cleanup definition: %v", err)
	}
}

func startE2EWorker(t *testing.T, d *sql.DB, ex worker.Executor, cfg worker.Config, queue string) {
	t.Helper()
	cfg.Queues = map[string]int{queue: 1}
	if cfg.WorkerID == "" {
		cfg.WorkerID = "nodes-e2e-worker"
	}
	ids, err := snowflake.New(0)
	if err != nil {
		t.Fatalf("worker snowflake: %v", err)
	}
	sched, err := scheduler.New(scheduler.Deps{DB: d, IDs: ids, Queue: queue})
	if err != nil {
		t.Fatalf("worker scheduler: %v", err)
	}
	wctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- worker.Run(wctx, worker.Deps{DB: d, IDs: ids, Executor: ex, Sched: sched}, cfg, redistest.MustOpt(t))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("worker.Run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("worker.Run did not stop within 15s after cancel")
		}
	})
}

func waitE2EExecSucceeded(t *testing.T, d *sql.DB, execID uint64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var status engine.WorkflowState
		if err := d.QueryRow(`SELECT status FROM workflow_execution WHERE id=?`, execID).
			Scan(&status); err == nil && status == engine.WorkflowSucceeded {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("execution %d not SUCCEEDED within 30s", execID)
}

func assertE2ENodeState(t *testing.T, d *sql.DB, execID uint64, nodeID string, want engine.NodeState) {
	t.Helper()
	var got engine.NodeState
	if err := d.QueryRow(`SELECT status FROM node_execution WHERE execution_id=? AND node_id=?`,
		execID, nodeID).Scan(&got); err != nil {
		t.Fatalf("read node %s: %v", nodeID, err)
	}
	if got != want {
		t.Fatalf("node %s status = %s, want %s", nodeID, got, want)
	}
}

// assertLLMLedger:attempt 台账列与 llm.call.completed 审计行两半边逐字一致
// (T3.1 双录契约,真实调用路径产出)。
func assertLLMLedger(t *testing.T, d *sql.DB, nodeExecID uint64, wantAmount string) {
	t.Helper()
	var attemptID uint64
	var model string
	var pt, ct, tt int64
	var amount, currency sql.NullString
	var estimated bool
	if err := d.QueryRow(`
SELECT id, model, prompt_tokens, completion_tokens, total_tokens,
       cost_amount, cost_currency, estimated
  FROM node_attempt WHERE node_execution_id=? AND attempt_no=?`, nodeExecID, 1).
		Scan(&attemptID, &model, &pt, &ct, &tt, &amount, &currency, &estimated); err != nil {
		t.Fatalf("read attempt usage: %v", err)
	}
	if model != "glm-5.3" || pt != 1000 || ct != 150 || tt != 1150 {
		t.Fatalf("attempt usage = %s %d/%d/%d, want glm-5.3 1000/150/1150(API 原值)", model, pt, ct, tt)
	}
	if !amount.Valid || amount.String != wantAmount || !currency.Valid || currency.String != "CNY" || estimated {
		t.Fatalf("attempt cost = %s %s estimated=%v, want %s CNY estimated=false", amount.String, currency.String, estimated, wantAmount)
	}

	var aModel string
	var apt, act, att int64
	var aAmount, aCurrency sql.NullString
	var aEstimated bool
	if err := d.QueryRow(`
SELECT model, prompt_tokens, completion_tokens, total_tokens,
       cost_amount, cost_currency, cost_estimated
  FROM audit_log WHERE event_type=? AND attempt_id=?`,
		audit.EventLLMCallCompleted, attemptID).
		Scan(&aModel, &apt, &act, &att, &aAmount, &aCurrency, &aEstimated); err != nil {
		t.Fatalf("read audit llm row: %v", err)
	}
	if aModel != model || apt != pt || act != ct || att != tt ||
		aAmount.String != amount.String || aCurrency.String != currency.String || aEstimated != estimated {
		t.Fatalf("double-write diverged: node_attempt=(%s %d/%d/%d %s %s %v) audit=(%s %d/%d/%d %s %s %v)",
			model, pt, ct, tt, amount.String, currency.String, estimated,
			aModel, apt, act, att, aAmount.String, aCurrency.String, aEstimated)
	}
}
