package nodes

// T7.2 断网验收 e2e:毒化全局 HTTP 传输层(非回环一律报错并记录),跑
// 「预构建 tokenizer → 请求前估算 → 首个 LLM 节点全链(调度 → outbox →
// relay → asynq → Mux → LLMExecutor → 生产 OpenAICompat)」,断言全程唯一
// 出站 HTTP = 桩供应商端点(回环)。DB/Redis 走原生 TCP 协议不经
// http.DefaultTransport,不受毒化影响。
//
// 变异自查:
//   - tokenizer loader 走默认下载 → 预构建即被拦,non-loopback 断言红;
//   - 适配器触网兜底(如 SDK 遥测)→ non-loopback 断言红;
//   - 词表损坏/篡改 → PrebuildTokenizers 启动即败,测试红。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/llm"
	"braid/internal/redistest"
	"braid/internal/relay"
	"braid/internal/worker"
)

// offlineTransport:记录全部出站 HTTP;非回环直接报错(注入禁网)。
type offlineTransport struct {
	inner http.RoundTripper
	urls  []string
	mu    sync.Mutex
}

func (t *offlineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.urls = append(t.urls, req.URL.String())
	t.mu.Unlock()
	if req.URL.Hostname() == "127.0.0.1" || req.URL.Hostname() == "localhost" || req.URL.Hostname() == "::1" {
		return t.inner.RoundTrip(req)
	}
	return nil, fmt.Errorf("offline e2e: network disabled for %s", req.URL)
}

func (t *offlineTransport) nonLoopbackRequests() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for _, u := range t.urls {
		if h, err := url.Parse(u); err != nil || h.Hostname() != "127.0.0.1" && h.Hostname() != "localhost" && h.Hostname() != "::1" {
			out = append(out, u)
		}
	}
	return out
}

func TestE2ELLMNodeOfflineColdStart(t *testing.T) {
	d := sqltest.MustOpenAt(t, nodesSchema)
	ctx := context.Background()
	queue := fmt.Sprintf("t_n%d", atomic.AddUint32(&queueSeq, 1))

	orig := http.DefaultTransport
	rt := &offlineTransport{inner: orig}
	http.DefaultTransport = rt
	t.Cleanup(func() { http.DefaultTransport = orig })

	// 冷启动半边:断网下预构建 + 请求前估算(未来预算消费方的调用位)。
	if err := llm.PrebuildTokenizers(); err != nil {
		t.Fatalf("prebuild tokenizers offline: %v", err)
	}
	if _, err := llm.EstimatePromptTokens("glm-5.3",
		[]llm.Message{{Role: "user", Content: "断网冷启动估算"}}); err != nil {
		t.Fatalf("estimate offline: %v", err)
	}
	if leak := rt.nonLoopbackRequests(); len(leak) != 0 {
		t.Fatalf("startup phase leaked network requests: %v", leak)
	}

	// 首个 LLM 节点全链:唯一合法出站 = 桩供应商(回环)。
	var calls atomic.Int64
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp-offline", "model": "glm-5.3",
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant",
					"content": `{"summary":"离线","count":1}`},
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
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
	client, err := llm.NewOpenAICompat(llmCfg, nil)
	if err != nil {
		t.Fatalf("llm client: %v", err)
	}
	executor := NewMux(mustE2ESched(t, d, queue), &LLMExecutor{
		Client: client, Prices: table, DB: d,
	}, &fakeToolExec{}, ApprovalExecutor{})

	sched := mustE2ESched(t, d, queue)
	defID, execID := seedLLMExec(t, d, sched, "断网冷启动文档")
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

	waitE2EExecSucceeded(t, d, execID)
	assertE2ENodeState(t, d, execID, "sum", engine.NodeSucceeded)
	if got := calls.Load(); got != 1 {
		t.Fatalf("stub calls = %d, want 1", got)
	}
	if leak := rt.nonLoopbackRequests(); len(leak) != 0 {
		t.Fatalf("offline cold start leaked non-provider network requests: %v", leak)
	}
}
