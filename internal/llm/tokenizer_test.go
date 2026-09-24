package llm

// 单测:T7.2 tiktoken 离线 loader + 预构建 + 请求前估算(httptest/毒化传输层,
// 零外部依赖零 DB)。
//
// 时序依赖:TestEstimateLazyOffline 必须是本包首个触达 tiktoken 的测试
// (config/cost/openai 测试文件按字母序在前且不触 tiktoken;本文件内它
// 排第一)——进程级缓存(ranks/实例单例)冷,懒加载路径才真正走 loader,
// 零外网断言才有效;整包跑序变化时该测试退化为冒烟(单测 -run 隔离跑
// 仍有效,审计 LOW 定性的边界)。
//
// 变异自查(实现错 → 哪条红):
//   - loader 走默认下载(未装 embed loader)→ prebuild_offline /
//     estimate_lazy_offline(隔离跑)红(毒化 DefaultTransport 拦下 openaipublic);
//   - SetBpeLoader 晚于首次懒加载 → estimate_lazy_offline(隔离跑)红;
//   - 词表 hash 校验缺失/错 pin → verify_vocab 红;
//   - 未捆绑编码(p50k 等)未拦 → loader_unbundled / estimate_routing 红;
//   - 未知模型 fallback 丢错 → estimate_routing 红(等值断言);
//   - o200k 路由退化成 cl100k → estimate_routing 红(CJK 判别串差值断言);
//   - 实例缓存退化成每调用 GetEncoding(重建 CoreBPE 38-94ms)→
//     estimate_capped 的耗时段 + estimate_concurrent 时长肉眼可见劣化
//     (审计 MEDIUM 锚);
//   - 超长内容不采样(CJK 超线性)→ estimate_capped 超时红;
//   - 并发竞态(实例单例裸写)→ estimate_concurrent 在 -race 下红。

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkoukk/tiktoken-go"
)

// offlineTransport 毒化 http.DefaultTransport 的测试替身:记录全部出站请求;
// 非回环目标直接报错(注入禁网,T7.2 验收语义),回环放行(httptest 假端点
// 是唯一合法通道)。适配器内部 &http.Client{} 无显式 Transport,恒走
// DefaultTransport——毒化它即覆盖全部出站面。
type offlineTransport struct {
	inner   http.RoundTripper
	urls    []string
	failAll bool
	mu      sync.Mutex
}

func (t *offlineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.urls = append(t.urls, req.URL.String())
	t.mu.Unlock()
	if !t.failAll && isLoopbackHost(req.URL.Hostname()) {
		return t.inner.RoundTrip(req)
	}
	return nil, fmt.Errorf("offline test: network disabled for %s", req.URL)
}

func isLoopbackHost(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// poisonDefaultTransport 替换全局 DefaultTransport 并登记还原(t.Cleanup,
// 包内测试串行,无交错)。allowLoopback=false 全禁(prebuild/估算阶段),
// true 只放行回环(带假供应商的在环验收)。
func poisonDefaultTransport(t *testing.T, allowLoopback bool) *offlineTransport {
	t.Helper()
	orig := http.DefaultTransport
	rt := &offlineTransport{inner: orig, failAll: !allowLoopback}
	http.DefaultTransport = rt
	t.Cleanup(func() { http.DefaultTransport = orig })
	return rt
}

func (t *offlineTransport) requestCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.urls)
}

func (t *offlineTransport) nonLoopbackRequests() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for _, u := range t.urls {
		// urls 记录的是完整 URL;host 部分非回环即外网
		if h, err := url.Parse(u); err != nil || !isLoopbackHost(h.Hostname()) {
			out = append(out, u)
		}
	}
	return out
}

// TestEstimateLazyOffline:不经 Prebuild 的懒加载路径同样零外网——SetBpeLoader
// 必须在包 init 即装上,任何首次构建都走 embed loader。【须为包内首个触达
// tiktoken 的测试,见文件头时序注释】
func TestEstimateLazyOffline(t *testing.T) {
	rt := poisonDefaultTransport(t, false)
	n, err := EstimatePromptTokens("glm-4.6", []Message{{Role: "user", Content: "你好世界"}})
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if n <= 0 {
		t.Fatalf("estimate = %d, want > 0", n)
	}
	if got := rt.requestCount(); got != 0 {
		t.Fatalf("lazy estimate made %d network request(s): %v", got, rt.urls)
	}
}

func TestPrebuildOffline(t *testing.T) {
	rt := poisonDefaultTransport(t, false)
	if err := PrebuildTokenizers(); err != nil {
		t.Fatalf("prebuild: %v", err)
	}
	if got := rt.requestCount(); got != 0 {
		t.Fatalf("prebuild made %d network request(s): %v", got, rt.urls)
	}
	// 幂等:二次调用不再触网、不再解析。
	if err := PrebuildTokenizers(); err != nil {
		t.Fatalf("second prebuild: %v", err)
	}
	if got := rt.requestCount(); got != 0 {
		t.Fatalf("second prebuild made %d request(s): %v", got, rt.urls)
	}
}

func TestEstimatePromptTokens(t *testing.T) {
	if err := PrebuildTokenizers(); err != nil {
		t.Fatalf("prebuild: %v", err)
	}
	msgs := []Message{
		{Role: "system", Content: "你是合规审查助手"},
		{Role: "user", Content: "hello world"},
	}
	cases := []struct {
		name  string
		model string
		msgs  []Message
		want  int
	}{
		// cl100k 口径:每消息 4 开销 + content token 数,另加 2 priming
		// (OpenAI cookbook chat 估算启发式,近似非计费)。
		{"empty messages", "gpt-4", nil, 2},
		{"single empty content", "gpt-4", []Message{{Role: "user"}}, 6},
		{"hello world cl100k", "gpt-4", []Message{{Role: "user", Content: "hello world"}}, 8},
		{"two messages", "gpt-4", msgs, 4 + 4 + tokensCl100k(t, "你是合规审查助手") + 2 + 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EstimatePromptTokens(tc.model, tc.msgs)
			if err != nil {
				t.Fatalf("estimate: %v", err)
			}
			if got != tc.want {
				t.Fatalf("estimate(%s) = %d, want %d", tc.model, got, tc.want)
			}
		})
	}
}

// tokensCl100k 用 cl100k 编码数 token(测试期望值的独立构造,直连库 API
// 而非复用被测函数——变异自查:估算公式改错时仍红)。
func tokensCl100k(t *testing.T, s string) int {
	t.Helper()
	tke, err := tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		t.Fatalf("cl100k: %v", err)
	}
	return len(tke.Encode(s, nil, nil))
}

// TestEstimateRouting:模型 → 编码路由的三条性质(值未知,性质可断言):
//  1. 未知模型(glm-4.6)与 cl100k 模型(gpt-4)同值 = fallback 生效;
//  2. 未捆绑编码的旧模型(davinci → r50k)也收敛到 cl100k = 捆绑集过滤生效;
//  3. o200k 模型(gpt-4o)在 CJK 判别串上与 cl100k 路径结果不同 = 路由
//     真的走到了 o200k(非全量 fallback)。
func TestEstimateRouting(t *testing.T) {
	if err := PrebuildTokenizers(); err != nil {
		t.Fatalf("prebuild: %v", err)
	}
	cjk := []Message{{Role: "user", Content: "你好,世界。标点判别串"}}
	base, err := EstimatePromptTokens("gpt-4", cjk)
	if err != nil {
		t.Fatalf("gpt-4: %v", err)
	}
	unknown, err := EstimatePromptTokens("glm-4.6", cjk)
	if err != nil || unknown != base {
		t.Fatalf("unknown model = %d (err %v), want == cl100k path %d", unknown, err, base)
	}
	oldModel, err := EstimatePromptTokens("davinci", cjk)
	if err != nil || oldModel != base {
		t.Fatalf("unbundled-encoding model = %d (err %v), want == cl100k path %d", oldModel, err, base)
	}
	o200k, err := EstimatePromptTokens("gpt-4o", cjk)
	if err != nil {
		t.Fatalf("gpt-4o: %v", err)
	}
	if o200k == base {
		t.Fatalf("gpt-4o estimate == cl100k estimate (%d): o200k routing not exercised", o200k)
	}
}

// TestEstimateCappedExtrapolation:超采样上限的均匀内容,外推 ≈ 精算
// (周期性内容前缀密度 = 全文密度,±10% 容差),且整条估算在秒级内返回
// (审计 MEDIUM 超线性锚:不采样时 12000 CJK rune 精算要数百 ms 起,
// 160k 要分钟级)。
func TestEstimateCappedExtrapolation(t *testing.T) {
	if err := PrebuildTokenizers(); err != nil {
		t.Fatalf("prebuild: %v", err)
	}
	content := strings.Repeat("工作流编排平台断网冷启动", 1000) // 12000 rune > 2048 采样上限
	start := time.Now()
	got, err := EstimatePromptTokens("gpt-4", []Message{{Role: "user", Content: content}})
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("estimate took %s on 12000-rune content, want seconds-bounded (sampling not applied?)", d)
	}
	tke, _ := tiktoken.GetEncoding("cl100k_base")
	exact := len(tke.Encode(content, nil, nil))
	lo, hi := exact*90/100, exact*110/100
	if got < lo || got > hi {
		t.Fatalf("extrapolated estimate = %d, exact = %d, want within ±10%%", got, exact)
	}
}

func TestEstimateConcurrent(t *testing.T) {
	if err := PrebuildTokenizers(); err != nil {
		t.Fatalf("prebuild: %v", err)
	}
	const goroutines, iters = 16, 20
	results := make([]int, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				n, err := EstimatePromptTokens("gpt-4o", []Message{{Role: "user", Content: "并发估算"}})
				if err != nil {
					t.Errorf("goroutine %d: %v", i, err)
					return
				}
				results[i] = n
			}
		}(i)
	}
	wg.Wait()
	for i := 1; i < goroutines; i++ {
		if results[i] != results[0] {
			t.Fatalf("concurrent estimates diverge: goroutine %d = %d, goroutine 0 = %d", i, results[i], results[0])
		}
	}
}

// TestFirstCallOfflineExceptProvider:T7.2 验收的最小在环复现——预构建 +
// 估算 + 一次真实适配器调用(生产 OpenAICompat → httptest 假供应商),
// 毒化传输层断言出站请求只落在供应商端点上,零其它外网。
func TestFirstCallOfflineExceptProvider(t *testing.T) {
	rt := poisonDefaultTransport(t, true)
	if err := PrebuildTokenizers(); err != nil {
		t.Fatalf("prebuild: %v", err)
	}
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, 200, okBody())
	}))
	t.Cleanup(srv.Close)
	cfg := DefaultConfig()
	cfg.BaseURL = srv.URL
	cfg.APIKey = "sk-test"
	cfg.PriceTable = mustTable(t, minPriceTable)
	client, err := NewOpenAICompat(cfg)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if _, err := EstimatePromptTokens("glm-5.3", baseReq().Messages); err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if _, err := client.ChatCompletion(t.Context(), baseReq()); err != nil {
		t.Fatalf("chat completion: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want 1", calls.Load())
	}
	if leak := rt.nonLoopbackRequests(); len(leak) != 0 {
		t.Fatalf("non-provider network requests leaked: %v", leak)
	}
}

func TestVerifyVocab(t *testing.T) {
	if err := verifyVocab("cl100k_base.tiktoken", []byte("tampered payload")); err == nil {
		t.Fatal("tampered vocab: want checksum mismatch error")
	}
	if err := verifyVocab("p50k_base.tiktoken", nil); err == nil {
		t.Fatal("unbundled vocab: want unsupported error")
	}
	for name := range vocabChecksums {
		raw, err := vocabFS.ReadFile("vocab/" + name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		if err := verifyVocab(name, raw); err != nil {
			t.Fatalf("embedded %s should verify: %v", name, err)
		}
	}
}

func TestLoaderRejectsUnbundled(t *testing.T) {
	_, err := embedBpeLoader{}.LoadTiktokenBpe("https://openaipublic.blob.core.windows.net/encodings/p50k_base.tiktoken")
	if err == nil {
		t.Fatal("unbundled encoding: want loud error")
	}
}

func TestParseTiktokenBpeMalformed(t *testing.T) {
	if _, err := parseTiktokenBpe([]byte("not-base64! 7")); err == nil {
		t.Fatal("malformed vocab line: want error")
	}
	if _, err := parseTiktokenBpe([]byte("onlyonefield")); err == nil {
		t.Fatal("single-field line: want error")
	}
}
