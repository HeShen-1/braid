package tools

// http 工具测试(T3.5)。验收锚(issue #32):
//   - host 白名单逐跳——重定向跳公网非白名 host 被拒(桩解析器放行回环,
//     证明拦截点在 host 门而非拨号);
//   - 首请求宿主复核、跨白名宿重定向放行、幂等键头传递;
//   - 输入契约 / 响应形状 / body 上限。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// httpReqRecord 记录一次到达请求的观测面(断言锚:方法/查询/头/体/宿主)。
type httpReqRecord struct {
	Method string
	Path   string
	Query  url.Values
	Host   string
	Header http.Header
	Body   string
}

// httpRecorder 是并发安全的请求记录器。
type httpRecorder struct {
	mu  sync.Mutex
	got []httpReqRecord
}

func (r *httpRecorder) record(req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.got = append(r.got, httpReqRecord{
		Method: req.Method,
		Path:   req.URL.Path,
		Query:  req.URL.Query(),
		Host:   req.Host,
		Header: req.Header.Clone(),
		Body:   string(body),
	})
	r.mu.Unlock()
}

func (r *httpRecorder) byPath(path string) []httpReqRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []httpReqRecord
	for _, g := range r.got {
		if g.Path == path {
			out = append(out, g)
		}
	}
	return out
}

func (r *httpRecorder) hosts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.got))
	for _, g := range r.got {
		out = append(out, g.Host)
	}
	return out
}

// newHTTPTestTool 构造指向真实 httptest TLS 桩的工具(回环放行 + 桩证书
// 信任——测试专用 Guard 注入,生产装配恒零值)。
func newHTTPTestTool(t *testing.T, endpoints map[string]string, guard GuardConfig) *HTTPTool {
	t.Helper()
	tool, err := NewHTTPTool(HTTPConfig{Endpoints: endpoints, Guard: guard})
	if err != nil {
		t.Fatalf("NewHTTPTool: %v", err)
	}
	return tool
}

func runHTTP(t *testing.T, tool *HTTPTool, inputs map[string]any) (Result, error) {
	t.Helper()
	return tool.Run(context.Background(), Call{Inputs: inputs, IdempotencyKey: "att:7:1"})
}

func decodeToolOutput(t *testing.T, res Result) map[string]any {
	t.Helper()
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(res.Output)))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode output %s: %v", res.Output, err)
	}
	return m
}

// TestNewHTTPToolConfig 锁定构造校验:base URL 仅 scheme+host[:port]。
func TestNewHTTPToolConfig(t *testing.T) {
	bad := []struct {
		name      string
		endpoints map[string]string
		wantErr   string
	}{
		{"empty endpoints", nil, "empty endpoints"},
		{"alias uppercase", map[string]string{"Docs": "https://a.test"}, "endpoint name"},
		{"alias leading digit", map[string]string{"1api": "https://a.test"}, "endpoint name"},
		{"alias too long", map[string]string{strings.Repeat("a", 65): "https://a.test"}, "endpoint name"},
		{"http scheme", map[string]string{"api": "http://a.test"}, "must be https"},
		{"non-ascii host", map[string]string{"api": "https://évil.test"}, "non-ascii host"},
		{"userinfo", map[string]string{"api": "https://u:p@a.test"}, "userinfo"},
		{"base carries path", map[string]string{"api": "https://a.test/prefix"}, "must not carry path"},
		{"base carries query", map[string]string{"api": "https://a.test/?q=1"}, "must not carry"},
		{"base carries fragment", map[string]string{"api": "https://a.test/#f"}, "must not carry"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewHTTPTool(HTTPConfig{Endpoints: c.endpoints})
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want contains %q", err, c.wantErr)
			}
		})
	}
	for _, raw := range []string{"https://a.test", "https://a.test:8443", "https://a.test/"} {
		if _, err := NewHTTPTool(HTTPConfig{Endpoints: map[string]string{"api": raw}}); err != nil {
			t.Fatalf("base %q: unexpected err %v", raw, err)
		}
	}
}

// TestAllowlistKey 锁定宿主键归一化:小写 hostname、端口不参与。
func TestAllowlistKey(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"https://Example.COM/x", "example.com"},
		{"https://a.test:8443/x", "a.test"},
		{"https://a.test:443/x", "a.test"},
		{"https://1.2.3.4/x", "1.2.3.4"},
	}
	for _, c := range cases {
		u, err := url.Parse(c.raw)
		if err != nil {
			t.Fatalf("parse %q: %v", c.raw, err)
		}
		if got := allowlistKey(u); got != c.want {
			t.Errorf("allowlistKey(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// TestHTTPToolRequestShape 锁定请求观测面:method 归一化、query 强制转换、
// 幂等键头、JSON body、响应形状(status 恒返、body/body_text 二选一)。
func TestHTTPToolRequestShape(t *testing.T) {
	rec := &httpRecorder{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		switch r.URL.Path {
		case "/echo":
			_, _ = io.WriteString(w, `{"echo":true,"n":2}`)
		case "/text":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "plain text")
		case "/arr":
			_, _ = io.WriteString(w, `[1,2,3]`)
		case "/trailing":
			_, _ = io.WriteString(w, `{"a":1}garbage`)
		case "/html":
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<b>bold & <i>raw</i></b>")
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"err":"not found"}`)
		}
	}))
	t.Cleanup(srv.Close)
	tool := newHTTPTestTool(t,
		map[string]string{"docs_api": srv.URL},
		GuardConfig{IPForbidden: allowOnly("127.0.0.1"), TLS: tlsSeam(srv, "")},
	)

	t.Run("get with query coercion", func(t *testing.T) {
		res, err := runHTTP(t, tool, map[string]any{
			"name": "docs_api", "method": "get", "path": "/echo",
			"query": map[string]any{
				"verbose": "true", "n": json.Number("3"), "flag": true,
				"tags": []any{"a", "b"},
			},
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		out := decodeToolOutput(t, res)
		if out["status"] != json.Number("200") {
			t.Fatalf("status = %v, want 200", out["status"])
		}
		if body, ok := out["body"].(map[string]any); !ok || body["echo"] != true || body["n"] != json.Number("2") {
			t.Fatalf("body = %v, want {echo:true n:2}", out["body"])
		}
		reqs := rec.byPath("/echo")
		if len(reqs) != 1 {
			t.Fatalf("/echo reached %d times, want 1", len(reqs))
		}
		got := reqs[0]
		if got.Method != http.MethodGet {
			t.Errorf("method = %q, want GET (lowercase normalized)", got.Method)
		}
		for k, want := range map[string][]string{
			"verbose": {"true"}, "n": {"3"}, "flag": {"true"}, "tags": {"a", "b"},
		} {
			if q := got.Query[k]; strings.Join(q, ",") != strings.Join(want, ",") {
				t.Errorf("query %q = %v, want %v", k, q, want)
			}
		}
		if ik := got.Header.Get("Idempotency-Key"); ik != "att:7:1" {
			t.Errorf("Idempotency-Key = %q, want att:7:1", ik)
		}
	})

	t.Run("post json body", func(t *testing.T) {
		res, err := runHTTP(t, tool, map[string]any{
			"name": "docs_api", "method": "POST", "path": "/echo",
			"body": map[string]any{"doc": json.Number("42")},
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if out := decodeToolOutput(t, res); out["status"] != json.Number("200") {
			t.Fatalf("status = %v", out["status"])
		}
		// POST 记录在 /echo 的第二条。
		reqs := rec.byPath("/echo")
		if len(reqs) != 2 || reqs[1].Method != http.MethodPost {
			t.Fatalf("POST not recorded: %d reqs", len(reqs))
		}
		if ct := reqs[1].Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		if reqs[1].Body != `{"doc":42}` {
			t.Errorf("body = %q", reqs[1].Body)
		}
	})

	t.Run("non-json response yields body_text", func(t *testing.T) {
		res, err := runHTTP(t, tool, map[string]any{"name": "docs_api", "path": "/text"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		out := decodeToolOutput(t, res)
		if out["body_text"] != "plain text" {
			t.Fatalf("body_text = %v", out["body_text"])
		}
		if _, present := out["body"]; present {
			t.Fatalf("body should be absent for non-JSON response, got %v", out["body"])
		}
	})

	t.Run("json array response yields array body", func(t *testing.T) {
		res, err := runHTTP(t, tool, map[string]any{"name": "docs_api", "path": "/arr"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		out := decodeToolOutput(t, res)
		arr, ok := out["body"].([]any)
		if !ok || len(arr) != 3 || arr[0] != json.Number("1") {
			t.Fatalf("body = %v, want [1 2 3]", out["body"])
		}
	})

	t.Run("trailing garbage after json yields body_text", func(t *testing.T) {
		res, err := runHTTP(t, tool, map[string]any{"name": "docs_api", "path": "/trailing"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		out := decodeToolOutput(t, res)
		if _, present := out["body"]; present {
			t.Fatalf("body should be absent, got %v", out["body"])
		}
		if out["body_text"] != `{"a":1}garbage` {
			t.Fatalf("body_text = %v", out["body_text"])
		}
	})

	t.Run("html-ish text not escape-amplified", func(t *testing.T) {
		// T3.5 审计 MEDIUM 修复锚:产出序列化 SetEscapeHTML(false)——
		// <>& 原样(删该行的变异 = 输出含 <,6x 放大回归)。
		res, err := runHTTP(t, tool, map[string]any{"name": "docs_api", "path": "/html"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !strings.Contains(string(res.Output), "<b>bold & <i>raw</i></b>") {
			t.Fatalf("output = %s, want raw unescaped text", res.Output)
		}
	})

	t.Run("non-2xx status is data not error", func(t *testing.T) {
		res, err := runHTTP(t, tool, map[string]any{"name": "docs_api", "path": "/missing"})
		if err != nil {
			t.Fatalf("Run: %v (404 must surface as data)", err)
		}
		out := decodeToolOutput(t, res)
		if out["status"] != json.Number("404") {
			t.Fatalf("status = %v, want 404", out["status"])
		}
		if body := out["body"].(map[string]any); body["err"] != "not found" {
			t.Fatalf("body = %v", out["body"])
		}
	})
}

// TestHTTPToolInputValidation 锁定输入契约(全部业务失败,非白名单哨兵)。
func TestHTTPToolInputValidation(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	tool := newHTTPTestTool(t,
		map[string]string{"docs_api": srv.URL},
		GuardConfig{IPForbidden: allowOnly("127.0.0.1"), TLS: tlsSeam(srv, "")},
	)
	cases := []struct {
		name    string
		inputs  map[string]any
		wantErr string
	}{
		{"missing name", map[string]any{"path": "/"}, `missing required input "name"`},
		{"nil name", map[string]any{"name": nil, "path": "/"}, `missing required input "name"`},
		{"non-string name", map[string]any{"name": 7, "path": "/"}, `must be string`},
		{"unknown endpoint", map[string]any{"name": "ghost", "path": "/"}, "not configured"},
		{"missing path", map[string]any{"name": "docs_api"}, `missing required input "path"`},
		{"path smuggles absolute url", map[string]any{"name": "docs_api", "path": "https://evil.test/x"}, "single slash"},
		{"path smuggles scheme-relative host", map[string]any{"name": "docs_api", "path": "//evil.test/x"}, "single slash"},
		{"path carries query", map[string]any{"name": "docs_api", "path": "/a?b=1"}, "must not contain"},
		{"path carries fragment", map[string]any{"name": "docs_api", "path": "/a#f"}, "must not contain"},
		{"path control byte", map[string]any{"name": "docs_api", "path": "/a\x00b"}, "must not contain"},
		{"unsupported method", map[string]any{"name": "docs_api", "method": "TRACE", "path": "/"}, "unsupported method"},
		{"non-string method", map[string]any{"name": "docs_api", "method": 1, "path": "/"}, "must be string"},
		{"body with GET", map[string]any{"name": "docs_api", "path": "/", "body": map[string]any{"x": 1}}, "body with GET"},
		{"query non-object", map[string]any{"name": "docs_api", "path": "/", "query": "a=1"}, "must be object"},
		{"query object value", map[string]any{"name": "docs_api", "path": "/", "query": map[string]any{"f": map[string]any{"x": 1}}}, "unsupported value type"},
		{"query nested array flattened is rejected", map[string]any{"name": "docs_api", "path": "/", "query": map[string]any{"f": []any{[]any{"a", "b"}}}}, "unsupported value type"},
		{"query mixed array with object element", map[string]any{"name": "docs_api", "path": "/", "query": map[string]any{"f": []any{"ok", map[string]any{"x": 1}}}}, "unsupported value type"},
		{"query null value", map[string]any{"name": "docs_api", "path": "/", "query": map[string]any{"f": nil}}, "unsupported value type"},
		{"query empty key", map[string]any{"name": "docs_api", "path": "/", "query": map[string]any{"": "v"}}, "empty query key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := runHTTP(t, tool, c.inputs)
			if err == nil {
				t.Fatal("err = nil, want rejection")
			}
			if errors.Is(err, ErrHostBlocked) || errors.Is(err, ErrSSRFBlocked) {
				t.Fatalf("input validation must not raise whitelist sentinels: %v", err)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want contains %q", err, c.wantErr)
			}
		})
	}
}

// TestHTTPToolBodyCap 锁定响应体上限(ssrf.go 边界归属:限额归工具实现)。
func TestHTTPToolBodyCap(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 64))
	}))
	t.Cleanup(srv.Close)
	tool, err := NewHTTPTool(HTTPConfig{
		Endpoints:    map[string]string{"docs_api": srv.URL},
		Guard:        GuardConfig{IPForbidden: allowOnly("127.0.0.1"), TLS: tlsSeam(srv, "")},
		MaxBodyBytes: 8,
	})
	if err != nil {
		t.Fatalf("NewHTTPTool: %v", err)
	}
	_, err = runHTTP(t, tool, map[string]any{"name": "docs_api", "path": "/"})
	if err == nil || !strings.Contains(err.Error(), "exceeds 8 bytes") {
		t.Fatalf("err = %v, want body exceeds", err)
	}
}

// TestHTTPToolHostAllowlistRedirects 是 T3.5 核心验收:host 白名单逐跳。
// 现场构造:假域名(docs/cdn/evil.test)经桩解析器全部解析到回环且放行
// 拨号——evil.test 是可拨的"公网"目标;若白名单门只在首请求生效,
// 重定向跳 evil.test 将畅通(现有 T3.3 测试只钉了跳内网禁段 IP)。
func TestHTTPToolHostAllowlistRedirects(t *testing.T) {
	rec := &httpRecorder{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		_, port, _ := net.SplitHostPort(r.Host)
		switch r.URL.Path {
		case "/hop-same":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/hop-cdn":
			http.Redirect(w, r, fmt.Sprintf("https://cdn.test:%s/ok", port), http.StatusFound)
		case "/hop-evil":
			http.Redirect(w, r, fmt.Sprintf("https://evil.test:%s/ok", port), http.StatusFound)
		case "/hop-evil-http":
			// 非白名 host 且非 https:顺序锚——host 门必须先于守卫的
			// https 门拒绝(顺序颠倒变异 = ErrSSRFBlocked 抢先,分桶破坏)。
			http.Redirect(w, r, fmt.Sprintf("http://evil.test:%s/ok", port), http.StatusFound)
		case "/hop-post-307":
			http.Redirect(w, r, "/ok", http.StatusTemporaryRedirect)
		case "/ok":
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}))
	t.Cleanup(srv.Close)
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	resolver := fakeResolver(t, map[string][]string{
		"docs.test": {"127.0.0.1"}, "cdn.test": {"127.0.0.1"}, "evil.test": {"127.0.0.1"},
	})
	tool := newHTTPTestTool(t,
		map[string]string{
			"docs": "https://docs.test:" + port,
			"cdn":  "https://cdn.test:" + port,
		},
		GuardConfig{Resolver: resolver, IPForbidden: allowOnly("127.0.0.1"), TLS: tlsSeam(srv, "127.0.0.1")},
	)

	t.Run("same-host redirect allowed, idempotency key survives", func(t *testing.T) {
		res, err := runHTTP(t, tool, map[string]any{"name": "docs", "path": "/hop-same"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if out := decodeToolOutput(t, res); out["status"] != json.Number("200") {
			t.Fatalf("status = %v", out["status"])
		}
		reqs := rec.byPath("/ok")
		if len(reqs) == 0 || reqs[len(reqs)-1].Header.Get("Idempotency-Key") != "att:7:1" {
			t.Fatalf("idempotency key did not survive redirect: %+v", reqs)
		}
	})

	t.Run("cross-allowlisted-host redirect allowed", func(t *testing.T) {
		res, err := runHTTP(t, tool, map[string]any{"name": "docs", "path": "/hop-cdn"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if out := decodeToolOutput(t, res); out["status"] != json.Number("200") {
			t.Fatalf("status = %v", out["status"])
		}
		reqs := rec.byPath("/ok")
		if len(reqs) == 0 || !strings.HasPrefix(reqs[len(reqs)-1].Host, "cdn.test:") {
			t.Fatalf("final request host = %v, want cdn.test", reqs)
		}
	})

	t.Run("redirect to public non-allowlisted host rejected", func(t *testing.T) {
		_, err := runHTTP(t, tool, map[string]any{"name": "docs", "path": "/hop-evil"})
		if !errors.Is(err, ErrHostBlocked) {
			t.Fatalf("err = %v, want ErrHostBlocked", err)
		}
		if !strings.Contains(err.Error(), "evil.test") {
			t.Fatalf("err = %v, want host in diagnostics", err)
		}
		// 拦截点在 host 门:evil.test 可拨(解析器/拨号都放行),首跳
		// /hop-evil 合法到达 docs.test,但任何 Host=evil.test 的请求都
		// 不得存在——重定向从未被跟随。
		for _, h := range rec.hosts() {
			if strings.HasPrefix(h, "evil.test") {
				t.Fatalf("request leaked to evil target: host %q", h)
			}
		}
	})

	t.Run("redirect order: host gate precedes guard https gate", func(t *testing.T) {
		// T3.5 审计 MEDIUM(测试真空洞)修复锚:Location 同时非白名且
		// 非 https 时,错误必须是 ErrHostBlocked(host 门在前);顺序
		// 颠倒变异会返回 ErrSSRFBlocked,本断言秒红。
		_, err := runHTTP(t, tool, map[string]any{"name": "docs", "path": "/hop-evil-http"})
		if !errors.Is(err, ErrHostBlocked) {
			t.Fatalf("err = %v, want ErrHostBlocked (host gate first)", err)
		}
		if errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("err = %v — guard https gate fired before host gate (order mutation)", err)
		}
	})

	t.Run("POST 307 replays body through allowlisted redirect", func(t *testing.T) {
		// T3.5 审计 LOW(测试真空洞)修复锚:307/308 body 重放依赖
		// bytes.Reader 的 GetBody(stdlib 隐式行为);重放跳仍逐跳过
		// host 门 + 幂等键头存活。
		res, err := tool.Run(context.Background(), Call{
			Inputs: map[string]any{
				"name": "docs", "method": "POST", "path": "/hop-post-307",
				"body": map[string]any{"doc": json.Number("42")},
			},
			IdempotencyKey: "att:7:1",
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if out := decodeToolOutput(t, res); out["status"] != json.Number("200") {
			t.Fatalf("status = %v, want 200", out["status"])
		}
		reqs := rec.byPath("/ok")
		var post *httpReqRecord
		for i := range reqs {
			if reqs[i].Method == http.MethodPost {
				post = &reqs[i]
			}
		}
		if post == nil {
			t.Fatal("POST body not replayed to /ok")
		}
		if post.Body != `{"doc":42}` {
			t.Fatalf("replayed body = %q", post.Body)
		}
		if post.Header.Get("Idempotency-Key") != "att:7:1" {
			t.Fatalf("Idempotency-Key lost across 307: %q", post.Header.Get("Idempotency-Key"))
		}
	})

	t.Run("runner maps rejection to tool_host_blocked", func(t *testing.T) {
		// 全链:client.Do 的 url.Error 包装 → 裸错误 → LocalRunner errors.Is
		// 分类(nodes 消费侧同款链路)。
		r := NewLocalRunner(registryOf(t, tool), NewTokenIssuer(nil, 0, nil))
		_, err := r.Run(context.Background(), RunRequest{
			Tool: "http", Inputs: map[string]any{"name": "docs", "path": "/hop-evil"},
			NodeExecID: 7, AttemptNo: 1, IdempotencyKey: "att:7:1",
		})
		if got := callCodeOf(t, err); got != CodeHostBlocked {
			t.Fatalf("code = %q, want %q (err=%v)", got, CodeHostBlocked, err)
		}
	})
}

// TestCheckHostAllowedNonASCII:T3.5 审计 MEDIUM 修复锚——非 ASCII 宿主
// 一律拒(IDNA 差分:门的 ToLower 折叠与 stdlib 拨号面 UTS#46 归一是
// 两套映射,存在折形差分码位;白名单是 ASCII 域,fail-closed)。
func TestCheckHostAllowedNonASCII(t *testing.T) {
	allow := map[string]struct{}{"k8s.example": {}}
	for _, raw := range []string{
		"https://K8s.example/admin", // U+212A KELVIN SIGN,ToLower 折 'k'
		"https://İ.example/x",       // U+0130,ToLower 折 'i'
		"https://例え.test/x",    // CJK(例え)
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		err = checkHostAllowed(allow, u)
		if !errors.Is(err, ErrHostBlocked) {
			t.Errorf("checkHostAllowed(%q) = %v, want ErrHostBlocked", raw, err)
			continue
		}
		if !strings.Contains(err.Error(), "non-ascii host") {
			t.Errorf("err = %v, want non-ascii diagnostics", err)
		}
	}
	// 对照:ASCII 白名宿主照常放行。
	u, _ := url.Parse("https://K8S.EXAMPLE/x")
	if err := checkHostAllowed(allow, u); err != nil {
		t.Fatalf("ascii foldable host rejected: %v", err)
	}
}

// TestHTTPToolBodyCapStreaming:T3.5 审计 MEDIUM(测试真空洞)修复锚——
// LimitReader 的内存半边(客户端读满 max+1 即返回,不等 EOF):删
// LimitReader 的变异会等整个响应体写完(桩在 max+1 字节后延迟 1s 收尾),
// 用时序差杀变异——正确实现毫秒级返回,变异 ≥1s。
func TestHTTPToolBodyCapStreaming(t *testing.T) {
	const max = 1024
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 首块就超过 max+1 并 flush:LimitReader 实现读满即返回;
		// 删 LimitReader 的变异会等 1s 后的余下字节(EOF)才返回。
		chunk := strings.Repeat("x", 2*max)
		_, _ = io.WriteString(w, chunk)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(time.Second)
		_, _ = io.WriteString(w, chunk)
	}))
	t.Cleanup(srv.Close)
	tool, err := NewHTTPTool(HTTPConfig{
		Endpoints:    map[string]string{"docs_api": srv.URL},
		Guard:        GuardConfig{IPForbidden: allowOnly("127.0.0.1"), TLS: tlsSeam(srv, "")},
		MaxBodyBytes: max,
	})
	if err != nil {
		t.Fatalf("NewHTTPTool: %v", err)
	}
	start := time.Now()
	_, err = runHTTP(t, tool, map[string]any{"name": "docs_api", "path": "/"})
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want body exceeds", err)
	}
	if elapsed >= 700*time.Millisecond {
		t.Fatalf("Run took %v — client waited for EOF instead of stopping at max+1 (LimitReader mutation)", elapsed)
	}
}
