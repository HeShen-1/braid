package nodes

// T19.2 端到端验收(D-2026-10-05-t192-foreach-dynamic-fanout):真 relay + 真
// asynq + 真 worker 全链——gen 产数组(输出 = 扇出集真相源)→ fan 在派发位
// 展开(Instantiate 后 fan 仅控制行)→ gather 输入序汇合 → post 消费 results
// → 工作流 outputs 物化。回滚 reconcile 面已由 scheduler 层表驱动族钉死
//(parallel_dynamic_test.go),本文件钉真链路时序。
//
// 变异自查(实现错 → 哪条红):
//   - Instantiate 预落实例行 → instantiate 阶段实例行计数断言红;
//   - 派发位不展开 → 执行永 RUNNING(gen 收口后控制器滞留 EXECUTING),成功
//     断言红;
//   - 动态 item 绑定断链(快照不带元素)→ 桩按 entry 识别失败,实例终败。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/llm"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
	"braid/internal/worker"
)

// parallelDynE2EDoc:gen(user_data topic)→ fan(foreach = {{nodes.gen.output.list}},
// body user_data {{item}})→ post(user_data results)。gen 输出 = 动态扇出集。
const parallelDynE2EDoc = `{"dsl":"1.0","name":"e2e-pardyn-%d",
 "inputs":[{"name":"topic","type":"string"}],
 "nodes":[
  {"id":"gen","type":"llm","model":"glm-5.3","temperature":0.3,"max_tokens":256,
   "response_format":"json",
   "prompt":{"system":"你是生成器","template":"列出条目",
             "user_data":[{"name":"topic","ref":"{{inputs.topic}}"}]},
   "outputs":{"list":"array"}},
  {"id":"fan","type":"parallel","parallel":{
    "foreach":"{{nodes.gen.output.list}}",
    "body":{"type":"llm","model":"glm-5.3","temperature":0.3,"max_tokens":256,
      "response_format":"json",
      "prompt":{"system":"你是评审","template":"评审以下条目",
                "user_data":[{"name":"entry","ref":"{{item}}"}]},
      "outputs":{"verdict":"string"}}}},
  {"id":"post","type":"llm","model":"glm-5.3","temperature":0.3,"max_tokens":256,
   "response_format":"json",
   "prompt":{"system":"你是汇总","template":"汇总评审结果",
             "user_data":[{"name":"all","ref":"{{nodes.fan.output.results}}"}]},
   "outputs":{"done":"boolean"}}],
 "edges":[{"from":"gen","to":"fan"},{"from":"fan","to":"post"}],
 "outputs":{"all":"{{nodes.fan.output.results}}","done":"{{nodes.post.output.done}}"}}`

// dynFanStub:OpenAI 兼容桩。gen 调用按「topic: 」识别回 {"list":[...]}(扇出集
// 真相源);实例调用按「entry: <item>」识别回 {"verdict":"ok-<item>"};post
// 按「all: 」识别回 {"done":true}。记录幂等键集与各路调用计数。
type dynFanStub struct {
	srv       *httptest.Server
	mu        sync.Mutex
	idemKeys  map[string]bool
	genCalls  atomic.Int64
	instCalls atomic.Int64
	postCalls atomic.Int64
}

func newDynFanStub(t *testing.T, list string) *dynFanStub {
	t.Helper()
	st := &dynFanStub{idemKeys: map[string]bool{}}
	entryRe := regexp.MustCompile(`entry: (\S+)`)
	st.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		msgs, _ := body["messages"].([]any)
		content := ""
		if len(msgs) > 1 {
			if usr, ok := msgs[1].(map[string]any); ok {
				content, _ = usr["content"].(string)
			}
		}
		st.mu.Lock()
		st.idemKeys[r.Header.Get("Idempotency-Key")] = true
		st.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(content, "topic: "):
			st.genCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "resp-gen", "model": "glm-5.3",
				"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
					"message": map[string]any{"role": "assistant",
						"content": fmt.Sprintf(`{"list":%s}`, list)}}},
				"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 5, "total_tokens": 10},
			})
		case strings.Contains(content, "entry: "):
			st.instCalls.Add(1)
			m := entryRe.FindStringSubmatch(content)
			item := ""
			if m != nil {
				item = m[1]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "resp-ok", "model": "glm-5.3",
				"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
					"message": map[string]any{"role": "assistant",
						"content": fmt.Sprintf(`{"verdict":"ok-%s"}`, item)}}},
				"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
			})
		default: // post:汇总
			st.postCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "resp-post", "model": "glm-5.3",
				"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
					"message": map[string]any{"role": "assistant", "content": `{"done":true}`}}},
				"usage": map[string]any{"prompt_tokens": 20, "completion_tokens": 5, "total_tokens": 25},
			})
		}
	}))
	t.Cleanup(st.srv.Close)
	return st
}

// dynFanMux 装配生产 Mux(llm 真适配器指向桩;无工具面)。
func dynFanMux(t *testing.T, d *sql.DB, queue, stubURL string) *Mux {
	t.Helper()
	cfg := llm.DefaultConfig()
	cfg.BaseURL = stubURL
	cfg.APIKey = "sk-e2e"
	table, err := llm.ParsePriceTable(
		`{"glm-5.3":{"input_per_mtok":"2","output_per_mtok":"8","currency":"CNY"}}`)
	if err != nil {
		t.Fatalf("price table: %v", err)
	}
	cfg.PriceTable = table
	client, err := llm.NewOpenAICompat(cfg, nil)
	if err != nil {
		t.Fatalf("llm client: %v", err)
	}
	return NewMux(mustE2ESched(t, d, queue), &LLMExecutor{Client: client, Prices: table, DB: d},
		&fakeToolExec{}, ApprovalExecutor{})
}

// fanInstanceRowCount 数 fan 节点实例行(instance_no > 0)。
func fanInstanceRowCount(t *testing.T, d *sql.DB, execID uint64) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM node_execution
	  WHERE execution_id=? AND node_id='fan' AND instance_no>0`, execID).Scan(&n); err != nil {
		t.Fatalf("count fan instance rows: %v", err)
	}
	return n
}

// TestE2EParallelDynamicFanout:gen 输出驱动的动态扇出全旅程——Instantiate 时
// fan 仅控制行(实例行零预落)→ gen 收口后派发位展开 3 实例 → gather 输入序
// → post 恰一次 → 执行 SUCCEEDED、outputs 物化。
func TestE2EParallelDynamicFanout(t *testing.T) {
	d := sqltest.MustOpenAt(t, nodesSchema)
	// T23.2:触发、投递、动态展开/worker CAS 均用各自真实组件账号;
	// admin 只建 fixture、读取断言与清理,不能替组件代写业务行。
	apiDB := sqltest.MustComponentDB(t, d, "api")
	workerDB := sqltest.MustComponentDB(t, d, "worker")
	relayDB := sqltest.MustComponentDB(t, d, "relay")
	queue := fmt.Sprintf("t_n%d", atomic.AddUint32(&queueSeq, 1))

	stub := newDynFanStub(t, `["alpha","beta","gamma"]`)
	executor := dynFanMux(t, workerDB, queue, stub.srv.URL)
	sched := mustE2ESched(t, apiDB, queue)

	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("seed snowflake: %v", err)
	}
	defID, err := ids.Next()
	if err != nil {
		t.Fatalf("gen def id: %v", err)
	}
	doc := fmt.Sprintf(parallelDynE2EDoc, defID)
	if _, err := d.Exec(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', ?, ?, 'e2e', 'e2e', 'e2e', NOW(3))`,
		defID, fmt.Sprintf("e2e-pardyn-%d", defID), doc, string(make([]byte, 64))); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	execID, err := sched.Instantiate(context.Background(), scheduler.InstantiateParams{
		DefinitionID: defID, Input: json.RawMessage(`{"topic":"标题"}`), TriggeredBy: "e2e", TriggerType: "TEST",
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	t.Cleanup(func() { cleanupNodesE2E(t, d, execID, defID) })

	// T19.2 锚:Instantiate 后 fan 仅控制行(动态扇出零预落)。
	if n := fanInstanceRowCount(t, d, execID); n != 0 {
		t.Fatalf("fan instance rows at Instantiate = %d, want 0 (dispatch-time 展开)", n)
	}

	startParallelRelay(t, relayDB)
	wcfg := worker.DefaultConfig()
	wcfg.HeartbeatInterval = 40 * time.Millisecond
	startE2EWorker(t, workerDB, executor, wcfg, queue)

	waitE2EExecSucceeded(t, d, execID)

	// 展开事实:3 实例全 SUCCEEDED;gen/post 各恰一次调用;实例恰 3 次调用。
	if n := fanInstanceRowCount(t, d, execID); n != 3 {
		t.Fatalf("fan instance rows = %d, want 3 (gen 输出长度)", n)
	}
	for i := uint16(1); i <= 3; i++ {
		waitInstanceState(t, d, execID, "fan", i, engine.NodeSucceeded)
	}
	if got := stub.genCalls.Load(); got != 1 {
		t.Fatalf("gen calls = %d, want 1", got)
	}
	if got := stub.instCalls.Load(); got != 3 {
		t.Fatalf("instance calls = %d, want 3", got)
	}
	if got := stub.postCalls.Load(); got != 1 {
		t.Fatalf("post calls = %d, want 1 (gather exactly-once)", got)
	}
	// gather 输入序 + 工作流 outputs 物化。
	var ctrlOut sql.NullString
	if err := d.QueryRow(`SELECT output FROM node_execution
	  WHERE execution_id=? AND node_id='fan' AND instance_no=0`, execID).Scan(&ctrlOut); err != nil {
		t.Fatalf("read controller: %v", err)
	}
	var ctrl map[string]any
	if err := json.Unmarshal([]byte(ctrlOut.String), &ctrl); err != nil {
		t.Fatalf("controller output: %v", err)
	}
	results, _ := ctrl["results"].([]any)
	if len(results) != 3 {
		t.Fatalf("results len = %d, want 3", len(results))
	}
	for i, w := range []string{"ok-alpha", "ok-beta", "ok-gamma"} {
		m, _ := results[i].(map[string]any)
		if m == nil || m["verdict"] != w {
			t.Fatalf("results[%d] = %v, want verdict %q (输入序)", i, results[i], w)
		}
	}
	var wfOut sql.NullString
	if err := d.QueryRow(`SELECT outputs FROM workflow_execution WHERE id=?`, execID).Scan(&wfOut); err != nil {
		t.Fatalf("read workflow outputs: %v", err)
	}
	if !strings.Contains(wfOut.String, "ok-gamma") {
		t.Fatalf("workflow outputs missing gathered results: %s", wfOut.String)
	}
	// 幂等键互异:gen + 3 实例 + post = 5。
	if got := len(stub.idemKeys); got != 5 {
		t.Fatalf("distinct idempotency keys = %d, want 5", got)
	}
}
