package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// publishSrv 起两跳桩:hop1 POST /definitions 回 201 {"id"};hop2
// POST /drafts/:id/publish 回 200 detail。hop 返回码可覆写。
func publishSrv(t *testing.T, hop1Status, hop2Status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/definitions":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"name"`) {
				t.Errorf("hop1 body missing DSL doc: %q", body)
			}
			w.WriteHeader(hop1Status)
			_, _ = io.WriteString(w, `{"id":"d-7","name":"wf"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/drafts/d-7/publish":
			w.WriteHeader(hop2Status)
			_, _ = io.WriteString(w, `{"id":"d-7","status":"PUBLISHED","version":3}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func envWith(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// TestPublishRunHappy 两跳成功:stdout = 发布 detail 原文,退出 0。
func TestPublishRunHappy(t *testing.T) {
	srv := publishSrv(t, http.StatusCreated, http.StatusOK)
	defer srv.Close()
	dir := t.TempDir()
	file := filepath.Join(dir, "wf.json")
	if err := os.WriteFile(file, []byte(`{"name":"wf","nodes":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := publishRun(context.Background(), envWith(map[string]string{"BRAID_TOKEN": "tok"}), publishParams{
		addr: srv.URL, dslFile: file,
	}, &out, &errb)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %q", code, errb.String())
	}
	if got := strings.TrimSpace(out.String()); got != `{"id":"d-7","status":"PUBLISHED","version":3}` {
		t.Errorf("stdout = %q", got)
	}
}

// TestPublishRunLargeDetail 发布 detail 内嵌全量 DSL,常态可达 1MiB 量级
// (服务端 maxDSLBody):>8KiB 的 detail 必须全量落 stdout,摘录上界只属
// 错误路径(对抗审计 HIGH 回归锚)。
func TestPublishRunLargeDetail(t *testing.T) {
	big := `{"id":"d-7","name":"wf","dsl":"` + strings.Repeat("y", 12<<10) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/definitions":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"d-7"}`)
		case "/api/v1/drafts/d-7/publish":
			_, _ = io.WriteString(w, big)
		}
	}))
	defer srv.Close()
	var out, errb bytes.Buffer
	code := publishRun(context.Background(), envWith(map[string]string{"BRAID_TOKEN": "tok"}), publishParams{
		addr: srv.URL, dslFile: writeTemp(t, `{"name":"wf"}`),
	}, &out, &errb)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %q", code, errb.String())
	}
	if got := len(strings.TrimSpace(out.String())); got != len(big) {
		t.Fatalf("stdout len = %d, want %d (full detail, no truncation)", got, len(big))
	}
}

// writeTemp 写临时文件(路径由调用方清理树)。
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "wf.json")
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// TestPublishRunFailures 错误面:文件不可读 / hop1 拒 / hop2 拒(归因带
// 草稿 id)/ 无凭证。
func TestPublishRunFailures(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "wf.json")
	if err := os.WriteFile(file, []byte(`{"name":"wf"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		srv     func(t *testing.T) *httptest.Server // nil = 不起服务
		env     map[string]string
		file    string
		wantSub []string
	}{
		{
			name:    "missing file",
			srv:     nil,
			env:     map[string]string{"BRAID_TOKEN": "tok"},
			file:    filepath.Join(dir, "nope.json"),
			wantSub: []string{"nope.json"},
		},
		{
			name:    "hop1 rejected (DSL invalid)",
			srv:     func(t *testing.T) *httptest.Server { return publishSrv(t, http.StatusBadRequest, http.StatusOK) },
			env:     map[string]string{"BRAID_TOKEN": "tok"},
			file:    file,
			wantSub: []string{"400"},
		},
		{
			name:    "hop2 rejected",
			srv:     func(t *testing.T) *httptest.Server { return publishSrv(t, http.StatusCreated, http.StatusConflict) },
			env:     map[string]string{"BRAID_TOKEN": "tok"},
			file:    file,
			wantSub: []string{"409", "d-7"},
		},
		{
			name:    "no credentials",
			srv:     func(t *testing.T) *httptest.Server { return publishSrv(t, http.StatusCreated, http.StatusOK) },
			env:     map[string]string{},
			file:    file,
			wantSub: []string{"BRAID_TOKEN"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := "http://127.0.0.1:1"
			if tt.srv != nil {
				srv := tt.srv(t)
				defer srv.Close()
				addr = srv.URL
			}
			var out, errb bytes.Buffer
			code := publishRun(context.Background(), envWith(tt.env), publishParams{addr: addr, dslFile: tt.file}, &out, &errb)
			if code != ExitError {
				t.Fatalf("exit = %d, want 1; stderr: %q", code, errb.String())
			}
			for _, want := range tt.wantSub {
				if !strings.Contains(errb.String(), want) {
					t.Errorf("stderr missing %q; got: %q", want, errb.String())
				}
			}
		})
	}
}
