package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestResolveAPIBase 三层解析:flag > $BRAID_API_ADDR > dev 默认;尾斜杠归一;
// 裸 host:port(cmd/api 的 BRAID_API_ADDR 监听值)垫 http:// 前缀。
func TestResolveAPIBase(t *testing.T) {
	tests := []struct {
		name     string
		flagAddr string
		envAddr  string
		want     string
	}{
		{"flag wins", "http://flag:1", "http://env:2", "http://flag:1"},
		{"env fallback", "", "http://env:2/", "http://env:2"},
		{"default", "", "", "http://127.0.0.1:8081"},
		{"hostport gets scheme", "", "127.0.0.1:8081", "http://127.0.0.1:8081"},
		{"hostport from flag", "127.0.0.1:9090/", "", "http://127.0.0.1:9090"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveAPIBase(tt.flagAddr, tt.envAddr); got != tt.want {
				t.Fatalf("resolveAPIBase(%q,%q) = %q, want %q", tt.flagAddr, tt.envAddr, got, tt.want)
			}
		})
	}
}

// TestResolveToken 解析链:BRAID_TOKEN 直用零请求;凭据走 login(路径/
// body/提取);双缺报 env 名;login 非 200 带状态与 body。
func TestResolveToken(t *testing.T) {
	t.Run("token env short-circuits", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
		}))
		defer srv.Close()
		env := map[string]string{"BRAID_TOKEN": "tok-1"}
		tok, err := resolveToken(context.Background(), srv.Client(), srv.URL, func(k string) string { return env[k] })
		if err != nil || tok != "tok-1" {
			t.Fatalf("tok=%q err=%v, want tok-1 nil", tok, err)
		}
		if n := hits.Load(); n != 0 {
			t.Fatalf("token env must not hit API, got %d requests", n)
		}
	})
	t.Run("credentials login", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/api/v1/auth/login" {
				t.Errorf("request %s %s, want POST /api/v1/auth/login", r.Method, r.URL.Path)
			}
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["username"] != "alice" || body["password"] != "pw-secret" {
				t.Errorf("login body = %v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"token":"tok-2","token_type":"Bearer","expires_at":"2036-01-01T00:00:00Z"}`)
		}))
		defer srv.Close()
		env := map[string]string{"BRAID_USERNAME": "alice", "BRAID_PASSWORD": "pw-secret"}
		tok, err := resolveToken(context.Background(), srv.Client(), srv.URL, func(k string) string { return env[k] })
		if err != nil || tok != "tok-2" {
			t.Fatalf("tok=%q err=%v, want tok-2 nil", tok, err)
		}
	})
	t.Run("no credentials", func(t *testing.T) {
		_, err := resolveToken(context.Background(), http.DefaultClient, "http://127.0.0.1:1", func(string) string { return "" })
		if err == nil {
			t.Fatal("want error")
		}
		for _, want := range []string{"BRAID_TOKEN", "BRAID_USERNAME", "BRAID_PASSWORD"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error missing %q: %v", want, err)
			}
		}
	})
	t.Run("login rejected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid_credentials"}`)
		}))
		defer srv.Close()
		env := map[string]string{"BRAID_USERNAME": "alice", "BRAID_PASSWORD": "wrong"}
		_, err := resolveToken(context.Background(), srv.Client(), srv.URL, func(k string) string { return env[k] })
		if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid_credentials") {
			t.Fatalf("err = %v, want 401 + body excerpt", err)
		}
	})
}

// TestPostJSON 契约:Bearer 头、Content-Type、Idempotency-Key 首请求必
// 在(逐请求断言,不恒真)、期望状态集外 → 错误带状态与 body 摘录、
// 成功体全量返回(>8KiB 不截断,detail 内嵌全量 DSL)。
func TestPostJSON(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer tok-3" {
			t.Errorf("req %d Authorization = %q", n, got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("req %d Content-Type = %q", n, got)
		}
		switch n {
		case 1: // 携幂等键的调用:透传断言必须为真(头缺失即红)
			if got := r.Header.Get("Idempotency-Key"); got != "idem-9" {
				t.Errorf("req 1 Idempotency-Key = %q, want idem-9", got)
			}
			_, _ = io.WriteString(w, `{"id":"42"}`)
		default: // 后续调用不带键:不得残留头
			if got := r.Header.Get("Idempotency-Key"); got != "" {
				t.Errorf("req %d Idempotency-Key = %q, want empty", n, got)
			}
			_, _ = io.WriteString(w, `{"id":"43"}`)
		}
	}))
	defer srv.Close()
	status, body, err := postJSON(context.Background(), srv.Client(), srv.URL+"/api/v1/executions", "tok-3",
		[]byte(`{"definition_name":"x"}`), "idem-9", http.StatusOK)
	if err != nil {
		t.Fatalf("postJSON: %v", err)
	}
	if status != http.StatusOK || strings.TrimSpace(string(body)) != `{"id":"42"}` {
		t.Errorf("status=%d body=%q", status, body)
	}

	// 状态集双接受:200 与 201 都在集内 → 同一响应两态调用皆成功。
	if _, _, err := postJSON(context.Background(), srv.Client(), srv.URL+"/api/v1/executions", "tok-3",
		nil, "", http.StatusOK, http.StatusCreated); err != nil {
		t.Fatalf("status set {200,201} must accept 200: %v", err)
	}

	// 状态集外:错误带实际与期望状态。
	_, _, err = postJSON(context.Background(), srv.Client(), srv.URL+"/api/v1/executions", "tok-3",
		nil, "", http.StatusCreated)
	if err == nil {
		t.Fatal("status mismatch must fail")
	}
	if !strings.Contains(err.Error(), "200") || !strings.Contains(err.Error(), "201") {
		t.Errorf("error should name both statuses: %v", err)
	}
}

// TestPostJSONLargeBody 成功体全量:>8KiB 的 detail(服务端 DSL 上限
// 1MiB,detail 内嵌全量 DSL)一字节不截——摘录上界只属错误路径
// (对抗审计 HIGH 回归锚)。
func TestPostJSONLargeBody(t *testing.T) {
	big := `{"id":"d-big","dsl":"` + strings.Repeat("x", 12<<10) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, big)
	}))
	defer srv.Close()
	_, body, err := postJSON(context.Background(), srv.Client(), srv.URL+"/x", "t", nil, "", http.StatusOK)
	if err != nil {
		t.Fatalf("postJSON: %v", err)
	}
	if len(body) != len(big) {
		t.Fatalf("body len = %d, want %d (full read, no truncation)", len(body), len(big))
	}
}
