package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTriggerRunHappy 触发成功:body 形状(definition_name/version/
// input 原文嵌入)、幂等键透传、stdout = 执行 detail、退出 0。
func TestTriggerRunHappy(t *testing.T) {
	var gotBody map[string]any
	var gotIdem string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/executions" {
			t.Errorf("request %s %s, want POST /api/v1/executions", r.Method, r.URL.Path)
		}
		gotIdem = r.Header.Get("Idempotency-Key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"e-1","status":"PENDING"}`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	file := filepath.Join(dir, "input.json")
	if err := os.WriteFile(file, []byte(`{"doc_path":"/tmp/a.txt"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := triggerRun(context.Background(), envWith(map[string]string{"BRAID_TOKEN": "tok"}), strings.NewReader(""),
		triggerParams{addr: srv.URL, definition: "doc-compliance-review", inputFile: file, idempotencyKey: "idem-1"}, &out, &errb)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %q", code, errb.String())
	}
	if got := strings.TrimSpace(out.String()); got != `{"id":"e-1","status":"PENDING"}` {
		t.Errorf("stdout = %q", got)
	}
	if gotIdem != "idem-1" {
		t.Errorf("Idempotency-Key = %q", gotIdem)
	}
	if gotBody["definition_name"] != "doc-compliance-review" {
		t.Errorf("definition_name = %v", gotBody["definition_name"])
	}
	in, _ := gotBody["input"].(map[string]any)
	if in == nil || in["doc_path"] != "/tmp/a.txt" {
		t.Errorf("input = %v, want embedded object", gotBody["input"])
	}
	if _, ok := gotBody["version"]; ok {
		t.Errorf("version must be omitted when 0, body = %v", gotBody)
	}
}

// TestTriggerRunStdinAndVersion -input - 走 stdin;-version > 0 进 body。
func TestTriggerRunStdinAndVersion(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"e-2"}`)
	}))
	defer srv.Close()
	var out, errb bytes.Buffer
	code := triggerRun(context.Background(), envWith(map[string]string{"BRAID_TOKEN": "tok"}),
		strings.NewReader(`{"proposal":"text"}`),
		triggerParams{addr: srv.URL, definition: "agent-council", version: 3, inputFile: "-"}, &out, &errb)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %q", code, errb.String())
	}
	if gotBody["version"] != float64(3) {
		t.Errorf("version = %v, want 3", gotBody["version"])
	}
	in, _ := gotBody["input"].(map[string]any)
	if in == nil || in["proposal"] != "text" {
		t.Errorf("input = %v", gotBody["input"])
	}
}

// TestTriggerRunReplay 幂等重放契约:服务端同键重放回 200 + 原执行回显
// (internal/api/execution.go 钉死的行为),CLI 状态集 {200,201} 双接受
// ——重放 = 机器语义成功,退出 0,stdout 出回显原文,stderr 带 replayed
// 提示;无幂等键的 200 同样放行且无提示(对抗审计 HIGH 回归锚)。
func TestTriggerRunReplay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // 非 201:重放路径
		_, _ = io.WriteString(w, `{"id":"e-1","status":"RUNNING"}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	file := filepath.Join(dir, "in.json")
	if err := os.WriteFile(file, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(idem string) (int, string, string) {
		var out, errb bytes.Buffer
		code := triggerRun(context.Background(), envWith(map[string]string{"BRAID_TOKEN": "tok"}), strings.NewReader(""),
			triggerParams{addr: srv.URL, definition: "wf", inputFile: file, idempotencyKey: idem}, &out, &errb)
		return code, out.String(), errb.String()
	}
	code, out, errb := run("k-1")
	if code != ExitOK {
		t.Fatalf("replay exit = %d, want 0; stderr: %q", code, errb)
	}
	if got := strings.TrimSpace(out); got != `{"id":"e-1","status":"RUNNING"}` {
		t.Errorf("stdout = %q", got)
	}
	if !strings.Contains(errb, "replayed") {
		t.Errorf("stderr missing replayed hint: %q", errb)
	}
	code, _, errb = run("") // 无键 200:同放行,无提示
	if code != ExitOK {
		t.Fatalf("keyless 200 exit = %d, want 0; stderr: %q", code, errb)
	}
	if strings.Contains(errb, "replayed") {
		t.Errorf("keyless 200 must not claim replay: %q", errb)
	}
}

// TestTriggerRunInputTooLarge 输入 > 1MiB(服务端请求体闸,handlers.go
// readBodyLimited):客户端先响亮拒绝,不空转上传后被拒。
func TestTriggerRunInputTooLarge(t *testing.T) {
	big := make([]byte, (1<<20)+1)
	for i := range big {
		big[i] = 'a'
	}
	var out, errb bytes.Buffer
	code := triggerRun(context.Background(), envWith(map[string]string{"BRAID_TOKEN": "tok"}), bytes.NewReader(big),
		triggerParams{addr: "http://127.0.0.1:1", definition: "wf", inputFile: "-"}, &out, &errb)
	if code != ExitError {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "exceeds") {
		t.Errorf("stderr missing cap message: %q", errb.String())
	}
}

// TestTriggerRunFailures 错误面:输入非 JSON / 文件缺失 / API 拒(503
// fleet gate 面)/ 无凭证。
func TestTriggerRunFailures(t *testing.T) {
	dir := t.TempDir()
	goodFile := filepath.Join(dir, "in.json")
	if err := os.WriteFile(goodFile, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	badFile := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badFile, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"fleet_capability_gate"}`)
	}))
	defer srv.Close()
	tests := []struct {
		name    string
		input   string
		wantSub []string
	}{
		{name: "input not json", input: badFile, wantSub: []string{"JSON"}},
		{name: "input file missing", input: filepath.Join(dir, "nope.json"), wantSub: []string{"nope.json"}},
		{name: "api rejected", input: goodFile, wantSub: []string{"503", "fleet_capability_gate"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := triggerRun(context.Background(), envWith(map[string]string{"BRAID_TOKEN": "tok"}), strings.NewReader(""),
				triggerParams{addr: srv.URL, definition: "wf", inputFile: tt.input}, &out, &errb)
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
	t.Run("no credentials", func(t *testing.T) {
		var out, errb bytes.Buffer
		code := triggerRun(context.Background(), envWith(map[string]string{}), strings.NewReader(""),
			triggerParams{addr: "http://127.0.0.1:1", definition: "wf", inputFile: goodFile}, &out, &errb)
		if code != ExitError || !strings.Contains(errb.String(), "BRAID_TOKEN") {
			t.Fatalf("exit=%d stderr=%q, want 1 + credential guidance", code, errb.String())
		}
	})
}
