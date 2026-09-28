package nodes

// T9.1 端到端验收(D-2026-09-28-t91):真 relay + 真 asynq + 真 worker 全链 ——
// parallel fan-out 多实例并发执行(多 attempt 同时 RUNNING 的验收断言)→
// gather 恰一次汇合(输入序)→ 后继派发与工作流 outputs 物化;on_failure
// 两语义的收敛时机差异在真时序上钉死(fail_fast 门控释放前即 FAILED /
// wait_all 收齐后才 FAILED)。
//
// 变异自查(实现错 → 哪条红):
//   - fan-out 实际串行派发 → 并发窗断言红(第三路 attempt 永 RUNNING 不了);
//   - gather 双收口 → post 双派发,post 恰一次调用断言红;
//   - fail_fast 误实现为 wait_all → 门控释放前执行态断言红(RUNNING≠FAILED);
//   - wait_all 误实现为 fail_fast → bad 实例失败后执行态断言红(FAILED≠RUNNING)。

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
	"braid/internal/redistest"
	"braid/internal/relay"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
	"braid/internal/worker"
)

// parallelE2EDoc:e2e 专用 parallel DSL(fan → post 双 llm;body user_data
// {{item}},post user_data {{nodes.fan.output.results}})。onFailure 经 %s 注入。
const parallelE2EDoc = `{"dsl":"1.0","name":"e2e-par-%d",
 "inputs":[{"name":"items","type":"array"}],
 "nodes":[
  {"id":"fan","type":"parallel","parallel":{
    "foreach":"{{inputs.items}}",
    %s
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
 "edges":[{"from":"fan","to":"post"}],
 "outputs":{"all":"{{nodes.fan.output.results}}","done":"{{nodes.post.output.done}}"}}`

// seedParallelExec 物化 parallel e2e 执行(onFailureSlot 为空串 = fail_fast 缺省)。
func seedParallelExec(t *testing.T, d *sql.DB, sched *scheduler.Scheduler,
	onFailureSlot string, input string) (defID, execID uint64) {
	t.Helper()
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("seed snowflake: %v", err)
	}
	defID, err = ids.Next()
	if err != nil {
		t.Fatalf("gen def id: %v", err)
	}
	doc := fmt.Sprintf(parallelE2EDoc, defID, onFailureSlot)
	if _, err := d.Exec(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', ?, ?, 'e2e', 'e2e', 'e2e', NOW(3))`,
		defID, fmt.Sprintf("e2e-par-%d", defID), doc, string(make([]byte, 64))); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	execID, err = sched.Instantiate(context.Background(), scheduler.InstantiateParams{
		DefinitionID: defID, Input: json.RawMessage(input), TriggeredBy: "e2e", TriggerType: "TEST",
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	return defID, execID
}

// parallelStub 是 OpenAI 兼容桩:实例调用按 user 消息里「entry: <item>」识别,
// 正常项回 {"verdict":"ok-<item>"}(经 gate 门控,制造并发在飞窗);bad 项回
// 非 JSON content(bad_llm_output 终败,不门控);post 调用按「all: 」识别回
// {"done":true}。记录幂等键集与各路调用计数。
type parallelStub struct {
	srv       *httptest.Server
	release   chan struct{} // 关闭 = 放行全部门控实例调用(nil = 不门控)
	gate      bool
	arrived3  chan struct{} // 首次达到 3 路并发在飞时关闭一次
	inflight  atomic.Int64
	mu        sync.Mutex
	idemKeys  map[string]bool
	instCalls atomic.Int64
	postCalls atomic.Int64
	badFailed atomic.Bool
}

func newParallelStub(t *testing.T, gate bool) *parallelStub {
	t.Helper()
	st := &parallelStub{
		release:  make(chan struct{}),
		gate:     gate,
		arrived3: make(chan struct{}),
		idemKeys: map[string]bool{},
	}
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
		case strings.Contains(content, "entry: "):
			st.instCalls.Add(1)
			m := entryRe.FindStringSubmatch(content)
			item := ""
			if m != nil {
				item = m[1]
			}
			if item == "bad" {
				// 非 JSON content + response_format json ⇒ bad_llm_output 终败。
				st.badFailed.Store(true)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "resp-bad", "model": "glm-5.3",
					"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
						"message": map[string]any{"role": "assistant", "content": "not-json"}}},
					"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
				})
				return
			}
			if st.gate {
				if n := st.inflight.Add(1); n >= 3 {
					st.closeArrived3()
				}
				select {
				case <-st.release:
				case <-r.Context().Done():
					return
				}
				st.inflight.Add(-1)
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

var arrivedOnce sync.Once

func (st *parallelStub) closeArrived3() { arrivedOnce.Do(func() { close(st.arrived3) }) }

// startParallelRelay 起 e2e relay(生产路径)。
func startParallelRelay(t *testing.T, d *sql.DB) {
	t.Helper()
	cfg := relay.DefaultConfig()
	cfg.IdleInterval = 20 * time.Millisecond
	cfg.EnqueueTimeout = 5 * time.Second
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- relay.Run(ctx, relay.Deps{DB: d, Enqueuer: relay.NewAsynqEnqueuer(redistest.MustOpt(t))}, cfg)
	}()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("relay.Run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("relay.Run did not stop within 15s after cancel")
		}
	})
}

// parallelMux 装配生产 Mux(llm 真适配器指向桩)。
func parallelMux(t *testing.T, d *sql.DB, queue, stubURL string) *Mux {
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

// instanceState 轮询等某实例行到达目标态。
func waitInstanceState(t *testing.T, d *sql.DB, execID uint64, nodeID string, instanceNo uint16,
	want engine.NodeState) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var got engine.NodeState
		err := d.QueryRow(`SELECT status FROM node_execution WHERE execution_id=? AND node_id=? AND instance_no=?`,
			execID, nodeID, instanceNo).Scan(&got)
		if err == nil && got == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("node %s[%d] not %s within 30s", nodeID, instanceNo, want)
}

// execStatus 轮询执行态(带目标态与期望不符时的显式快照断言用)。
func execStatus(t *testing.T, d *sql.DB, execID uint64) engine.WorkflowState {
	t.Helper()
	var st engine.WorkflowState
	if err := d.QueryRow(`SELECT status FROM workflow_execution WHERE id=?`, execID).Scan(&st); err != nil {
		t.Fatalf("read execution: %v", err)
	}
	return st
}

// countRunningAttempts 数该执行当前 RUNNING 的 attempt 行(并发窗断言)。
func countRunningAttempts(t *testing.T, d *sql.DB, execID uint64) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM node_attempt WHERE execution_id=? AND status='RUNNING'`,
		execID).Scan(&n); err != nil {
		t.Fatalf("count running attempts: %v", err)
	}
	return n
}

func TestE2EParallelScatterGather(t *testing.T) {
	d := sqltest.MustOpenAt(t, nodesSchema)
	queue := fmt.Sprintf("t_n%d", atomic.AddUint32(&queueSeq, 1))
	arrivedOnce = sync.Once{} // 每 case 独立一次(包级 var 复用纪律)

	stub := newParallelStub(t, true)
	executor := parallelMux(t, d, queue, stub.srv.URL)
	sched := mustE2ESched(t, d, queue)
	defID, execID := seedParallelExec(t, d, sched, "", `{"items": ["alpha","beta","gamma","delta"]}`)
	t.Cleanup(func() { cleanupNodesE2E(t, d, execID, defID) })

	startParallelRelay(t, d)
	wcfg := worker.DefaultConfig()
	wcfg.HeartbeatInterval = 40 * time.Millisecond
	startE2EWorker(t, d, executor, wcfg, queue)

	// 并发窗:3 路实例调用同时门控在飞 → DB 必见 ≥3 个 RUNNING attempt
	// (验收「多 attempt 同时 RUNNING」的真链路形态)。
	select {
	case <-stub.arrived3:
	case <-time.After(20 * time.Second):
		t.Fatalf("3 concurrent instance calls never arrived (fan-out not parallel?)")
	}
	if n := countRunningAttempts(t, d, execID); n < 3 {
		t.Fatalf("running attempts = %d at 3-way gate, want >= 3", n)
	}
	close(stub.release)

	waitE2EExecSucceeded(t, d, execID)

	// 恰一次汇合:post 恰一次调用;控制器 SUCCEEDED;results 输入序。
	if got := stub.postCalls.Load(); got != 1 {
		t.Fatalf("post calls = %d, want 1 (gather exactly-once)", got)
	}
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
	if len(results) != 4 {
		t.Fatalf("results len = %d, want 4", len(results))
	}
	wantOrder := []string{"ok-alpha", "ok-beta", "ok-gamma", "ok-delta"}
	for i, w := range wantOrder {
		m, _ := results[i].(map[string]any)
		if m == nil || m["verdict"] != w {
			t.Fatalf("results[%d] = %v, want verdict %q (输入序,与完成顺序无关)", i, results[i], w)
		}
	}
	// 幂等键:4 实例 + post 各一,互异且形态精确(att:<row>:1)。
	if got := len(stub.idemKeys); got != 5 {
		t.Fatalf("distinct idempotency keys = %d, want 5 (4 instances + post)", got)
	}
	var wfOut sql.NullString
	if err := d.QueryRow(`SELECT outputs FROM workflow_execution WHERE id=?`, execID).Scan(&wfOut); err != nil {
		t.Fatalf("read workflow outputs: %v", err)
	}
	if !strings.Contains(wfOut.String, "ok-delta") {
		t.Fatalf("workflow outputs missing gathered results: %s", wfOut.String)
	}
	// 实例行收口全 SUCCEEDED;attempt 台账各一行(4 路真实调用)。
	for i := uint16(1); i <= 4; i++ {
		waitInstanceState(t, d, execID, "fan", i, engine.NodeSucceeded)
	}
	var attempts int
	if err := d.QueryRow(`SELECT COUNT(*) FROM node_attempt WHERE execution_id=?`, execID).Scan(&attempts); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attempts != 5 { // 4 实例 + post
		t.Fatalf("attempts = %d, want 5", attempts)
	}
}

func TestE2EParallelFailureSemantics(t *testing.T) {
	input := `{"items": ["good1", "bad", "good2", "good3"]}`

	t.Run("fail_fast_converges_before_siblings_finish", func(t *testing.T) {
		d := sqltest.MustOpenAt(t, nodesSchema)
		queue := fmt.Sprintf("t_n%d", atomic.AddUint32(&queueSeq, 1))
		arrivedOnce = sync.Once{}
		stub := newParallelStub(t, true)
		executor := parallelMux(t, d, queue, stub.srv.URL)
		sched := mustE2ESched(t, d, queue)
		defID, execID := seedParallelExec(t, d, sched, "", input)
		t.Cleanup(func() { cleanupNodesE2E(t, d, execID, defID) })
		startParallelRelay(t, d)
		wcfg := worker.DefaultConfig()
		wcfg.HeartbeatInterval = 40 * time.Millisecond
		startE2EWorker(t, d, executor, wcfg, queue)

		// bad 立即终败(不门控)→ 控制器 FAILED、执行 FAILED,此时 good 三路
		// 仍门控在飞(fail_fast = 任一实例终败即收敛,不等待兄弟)。
		waitInstanceState(t, d, execID, "fan", 2, engine.NodeFailed)
		waitInstanceState(t, d, execID, "fan", 0, engine.NodeFailed)
		if st := execStatus(t, d, execID); st != engine.WorkflowFailed {
			t.Fatalf("execution = %s while siblings still gated, want FAILED (fail_fast)", st)
		}
		if n := countRunningAttempts(t, d, execID); n < 3 {
			t.Fatalf("running sibling attempts = %d, want >= 3 still in flight", n)
		}
		close(stub.release)
		// 在飞兄弟自然跑完(FAILED 执行内迟到收口),post 永不派发。
		for i := uint16(1); i <= 4; i++ {
			if i == 2 {
				continue
			}
			waitInstanceState(t, d, execID, "fan", i, engine.NodeSucceeded)
		}
		assertE2ENodeState(t, d, execID, "post", engine.NodePending)
	})

	t.Run("wait_all_collects_before_failing", func(t *testing.T) {
		d := sqltest.MustOpenAt(t, nodesSchema)
		queue := fmt.Sprintf("t_n%d", atomic.AddUint32(&queueSeq, 1))
		arrivedOnce = sync.Once{}
		stub := newParallelStub(t, true)
		executor := parallelMux(t, d, queue, stub.srv.URL)
		sched := mustE2ESched(t, d, queue)
		defID, execID := seedParallelExec(t, d, sched, `"on_failure": "wait_all",`, input)
		t.Cleanup(func() { cleanupNodesE2E(t, d, execID, defID) })
		startParallelRelay(t, d)
		wcfg := worker.DefaultConfig()
		wcfg.HeartbeatInterval = 40 * time.Millisecond
		startE2EWorker(t, d, executor, wcfg, queue)

		// bad 终败已处理,good 三路仍门控在飞 → 执行必须仍 RUNNING(收齐再判)。
		waitInstanceState(t, d, execID, "fan", 2, engine.NodeFailed)
		if st := execStatus(t, d, execID); st != engine.WorkflowRunning {
			t.Fatalf("execution = %s after one instance failed but siblings in flight, want RUNNING (wait_all)", st)
		}
		if st := instanceStateNow(t, d, execID, "fan", 0); st != engine.NodeExecuting {
			t.Fatalf("controller = %s, want EXECUTING (wait_all 未收齐)", st)
		}
		close(stub.release)
		// 收齐:控制器 FAILED、执行 FAILED、post 滞留 PENDING。
		waitInstanceState(t, d, execID, "fan", 0, engine.NodeFailed)
		deadline := time.Now().Add(30 * time.Second)
		for execStatus(t, d, execID) != engine.WorkflowFailed && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if st := execStatus(t, d, execID); st != engine.WorkflowFailed {
			t.Fatalf("execution = %s after gather, want FAILED", st)
		}
		assertE2ENodeState(t, d, execID, "post", engine.NodePending)
		if got := stub.postCalls.Load(); got != 0 {
			t.Fatalf("post calls = %d, want 0", got)
		}
	})
}

func instanceStateNow(t *testing.T, d *sql.DB, execID uint64, nodeID string, instanceNo uint16) engine.NodeState {
	t.Helper()
	var st engine.NodeState
	if err := d.QueryRow(`SELECT status FROM node_execution WHERE execution_id=? AND node_id=? AND instance_no=?`,
		execID, nodeID, instanceNo).Scan(&st); err != nil {
		t.Fatalf("read instance: %v", err)
	}
	return st
}
