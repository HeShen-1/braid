package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// T16.3(issue #140):braid seed 一键 demo——迁移 + 建号 + 示例定义发布 +
// 触发,幂等先行(迁移 no-op / 建号 ErrConflict 跳过 / 定义按名判存 /
// 触发固定幂等键重放)。REST 面用 httptest 假 API;DB 面走 sqltest 真库
// (与 usercreate 测试同 schema base,槽位不变)。

// fakeAPI 是 rest 段的假 API:状态机可拨(登录成败/是否已发布/执行状态),
// 记录 Idempotency-Key 与登录名供断言。seed 是同步单 goroutine 调用方,
// 仍加锁防御未来并行化。
type fakeAPI struct {
	t *testing.T

	mu          sync.Mutex
	srv         *httptest.Server
	loginOK     bool // 登录面开关(false = 401)
	published   bool // GET /definitions/:name → 200/404
	probeFail   bool // 定义探针 → 500(非 200/404 失败面)
	triggered   bool // 首次 POST /executions → 201,其后同幂等键 → 200
	execStatus  string
	idemKeySeen string
	polls       int
	logins      []string
}

// execPolls 返回 GET /executions/:id 轮询次数(等待环确实跑过的证据)。
func (f *fakeAPI) execPolls(t *testing.T) int {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.polls
}

// wantAuthorPass 是假登录面认的 alice 口令(seed env 传入什么就比对什么;
// 缺省与 dev 默认一致)。
const wantAuthorPass = "dev-author-1"

func newFakeAPI(t *testing.T, published bool, execStatus string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{t: t, loginOK: true, published: published, execStatus: execStatus}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		// handler goroutine 里只能 Errorf(FailNow/Fatalf 须在测试 goroutine),
		// 错误仍响亮可见,由用例在 runSeed 返回后断言副作用。
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Errorf("fakeAPI: login body decode: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.logins = append(f.logins, body.Username)
		ok := f.loginOK && body.Username == "alice" && body.Password == wantAuthorPass
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_credentials"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token":"tok-alice","token_type":"Bearer"}`))
	})
	mux.HandleFunc("GET /api/v1/definitions/agent-council", func(w http.ResponseWriter, r *http.Request) {
		if !f.checkAuth(w, r) {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.probeFail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		if !f.published {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"definition not found"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"agent-council","version":1,"status":"published"}`))
	})
	mux.HandleFunc("POST /api/v1/definitions", func(w http.ResponseWriter, r *http.Request) {
		if !f.checkAuth(w, r) {
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"draft-1"}`))
	})
	mux.HandleFunc("POST /api/v1/drafts/draft-1/publish", func(w http.ResponseWriter, r *http.Request) {
		if !f.checkAuth(w, r) {
			return
		}
		f.mu.Lock()
		f.published = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"agent-council","version":1,"status":"published"}`))
	})
	mux.HandleFunc("POST /api/v1/executions", func(w http.ResponseWriter, r *http.Request) {
		if !f.checkAuth(w, r) {
			return
		}
		key := r.Header.Get("Idempotency-Key")
		f.mu.Lock()
		defer f.mu.Unlock()
		f.idemKeySeen = key
		if !f.triggered {
			f.triggered = true
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"exec-7","status":"RUNNING"}`))
			return
		}
		// 服务端幂等契约:同键重放 200 + 原执行回显(execution.go)。
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"exec-7","status":"` + f.execStatus + `"}`))
	})
	mux.HandleFunc("GET /api/v1/executions/exec-7", func(w http.ResponseWriter, r *http.Request) {
		if !f.checkAuth(w, r) {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.polls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"exec-7","status":"` + f.execStatus + `"}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// checkAuth 断言请求带 seed 自建 alice 凭据换来的 Bearer(ambient
// BRAID_TOKEN 不得被 seed 采用);false 时已写 401,调用方必须立即 return
// (不截流会出现 superfluous WriteHeader)。
func (f *fakeAPI) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	if got := r.Header.Get("Authorization"); got != "Bearer tok-alice" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return false
	}
	return true
}

// writeDSL 落一个最小 DSL 文件(seed 只解析 name,校验归服务端)。
func writeDSL(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent-council.json")
	if err := os.WriteFile(p, []byte(`{"dsl":"1.0","name":"agent-council","description":"seed test"}`), 0o644); err != nil {
		t.Fatalf("write dsl: %v", err)
	}
	return p
}

// TestSeedFlagLayer 钉 argv 层:坏 -stage / 坏 -wait / 负 -wait / 多余
// 位置参 / 未知 flag → 退出 2(不触 DB/网络)。
func TestSeedFlagLayer(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{"bad stage", []string{"seed", "-stage", "db+rest"}, "-stage must be one of db, rest, all"},
		{"bad duration", []string{"seed", "-wait", "notadur"}, "invalid value"},
		{"negative wait", []string{"seed", "-wait", "-1s"}, "-wait must be >= 0"},
		{"extra positional", []string{"seed", "junk"}, "unexpected positional"},
		{"unknown flag", []string{"seed", "-nope"}, "not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := Run(context.Background(), tt.args, func(string) string { return "" },
				strings.NewReader(""), &out, &errb)
			if code != ExitUsage {
				t.Fatalf("Run(%q) exit = %d, want %d; stderr: %q", tt.args, code, ExitUsage, errb.String())
			}
			if !strings.Contains(errb.String(), tt.wantSub) {
				t.Errorf("stderr missing %q; got: %q", tt.wantSub, errb.String())
			}
		})
	}
}

// TestSeedDBStageIntegration 真库:首轮建出 alice/bob 双行 + 租约用后即删;
// 重轮全跳过(ErrConflict 幂等);DSN 缺失响亮失败。
func TestSeedDBStageIntegration(t *testing.T) {
	env := newDBEnv(t, nil)
	dsl := writeDSL(t)

	run := func() (int, string, string) {
		var out, errb bytes.Buffer
		code := runSeed(context.Background(), []string{"-stage", "db", "-definition", dsl}, env, &out, &errb)
		return code, out.String(), errb.String()
	}

	code, out, errb := run()
	if code != ExitOK {
		t.Fatalf("first run exit = %d, want 0; stderr: %q", code, errb)
	}
	for _, want := range []string{
		"① 迁移", "schema 已就绪",
		"② 建号:alice(author, approver)已创建",
		"② 建号:bob(approver)已创建",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q; got: %q", want, out)
		}
	}

	code, out, errb = run()
	if code != ExitOK {
		t.Fatalf("rerun exit = %d, want 0; stderr: %q", code, errb)
	}
	if n := strings.Count(out, "已存在,跳过"); n != 2 {
		t.Errorf("rerun skip lines = %d, want 2; stdout: %q", n, out)
	}

	// DSN 缺失 = 运行失败(退出 1),指引 env 名。
	var out2, errb2 bytes.Buffer
	code = runSeed(context.Background(), []string{"-stage", "db", "-definition", dsl},
		func(string) string { return "" }, &out2, &errb2)
	if code != ExitError {
		t.Fatalf("no-DSN exit = %d, want %d; stderr: %q", code, ExitError, errb2.String())
	}
	if !strings.Contains(errb2.String(), "BRAID_MYSQL_DSN") {
		t.Errorf("stderr missing BRAID_MYSQL_DSN hint; got: %q", errb2.String())
	}
}

// TestSeedRESTStageFlow httptest 全链:首轮探针 404 → 两跳发布 → 触发 201
// (幂等键形状钉死)→ 轮询至 WAITING_APPROVAL 的指路叙事;重轮探针 200
// 跳过发布 + 幂等键重放 200 仍退出 0;ambient BRAID_TOKEN 被 seed 无视。
func TestSeedRESTStageFlow(t *testing.T) {
	f := newFakeAPI(t, false, "WAITING_APPROVAL")
	dsl := writeDSL(t)
	env := func(k string) string {
		if k == "BRAID_TOKEN" {
			return "ambient-token-must-be-ignored" // seed 只用自建 alice 凭据
		}
		return ""
	}

	args := []string{"-stage", "rest", "-definition", dsl, "-addr", f.srv.URL, "-wait", "2s"}
	var out, errb bytes.Buffer
	if code := runSeed(context.Background(), args, env, &out, &errb); code != ExitOK {
		t.Fatalf("first run exit = %d, want 0; stderr: %q", code, errb.String())
	}
	for _, want := range []string{
		"③ 定义:agent-council 已发布(v1)",
		"④ 触发:执行 exec-7 已创建",
		"⑤ 执行 exec-7:WAITING_APPROVAL",
		// T21.2 路由硬切:审批台入口 = /approvals/(旧前缀已 404),
		// 画布 = /workflows/;seed 引导文案必须指向活路由。
		"/approvals/",
		"/workflows/",
		"Asynqmon",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout missing %q; got: %q", want, out.String())
		}
	}
	if ok, _ := regexp.MatchString(`^seed-agent-council-[0-9a-f]{8}$`, f.idemKeySeen); !ok {
		t.Errorf("idempotency key = %q, want seed-agent-council-<8 hex>", f.idemKeySeen)
	}
	if len(f.logins) == 0 || f.logins[0] != "alice" {
		t.Errorf("logins = %v, want first login as alice", f.logins)
	}
	if f.execPolls(t) == 0 {
		t.Error("no execution polls recorded; wait loop never ran")
	}

	// 重轮:定义已发布 → 跳过两跳;同幂等键 → 200 重放回显,退出 0。
	var out2, errb2 bytes.Buffer
	if code := runSeed(context.Background(), args, env, &out2, &errb2); code != ExitOK {
		t.Fatalf("rerun exit = %d, want 0; stderr: %q", code, errb2.String())
	}
	if !strings.Contains(out2.String(), "③ 定义:agent-council 已发布,跳过") {
		t.Errorf("rerun stdout missing publish-skip line; got: %q", out2.String())
	}
	if !strings.Contains(out2.String(), "④ 触发:幂等键命中,回显既有执行 exec-7") {
		t.Errorf("rerun stdout missing replay line; got: %q", out2.String())
	}
}

// TestSeedRESTStageFailures 失败面:登录 401(口令不符,含 env 指引)与
// 探针非 200/404(5xx)→ 退出 1。
func TestSeedRESTStageFailures(t *testing.T) {
	dsl := writeDSL(t)

	f := newFakeAPI(t, false, "RUNNING")
	f.loginOK = false
	var out, errb bytes.Buffer
	code := runSeed(context.Background(),
		[]string{"-stage", "rest", "-definition", dsl, "-addr", f.srv.URL}, func(string) string { return "" }, &out, &errb)
	if code != ExitError {
		t.Fatalf("login-401 exit = %d, want %d; stderr: %q", code, ExitError, errb.String())
	}
	if !strings.Contains(errb.String(), "BRAID_SEED_AUTHOR_PASSWORD") {
		t.Errorf("stderr missing password env hint; got: %q", errb.String())
	}

	f2 := newFakeAPI(t, false, "RUNNING")
	f2.probeFail = true
	var out2, errb2 bytes.Buffer
	code = runSeed(context.Background(),
		[]string{"-stage", "rest", "-definition", dsl, "-addr", f2.srv.URL}, func(string) string { return "" }, &out2, &errb2)
	if code != ExitError {
		t.Fatalf("probe-500 exit = %d, want %d; stderr: %q", code, ExitError, errb2.String())
	}
}

// TestSeedRESTStageWaitBudget 轮询预算耗尽不算失败:退出 0,报当前状态。
func TestSeedRESTStageWaitBudget(t *testing.T) {
	f := newFakeAPI(t, true, "RUNNING") // 已发布 + 永远 RUNNING
	dsl := writeDSL(t)
	var out, errb bytes.Buffer
	code := runSeed(context.Background(),
		[]string{"-stage", "rest", "-definition", dsl, "-addr", f.srv.URL, "-wait", "150ms"},
		func(string) string { return "" }, &out, &errb)
	if code != ExitOK {
		t.Fatalf("wait-budget exit = %d, want 0; stderr: %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "预算耗尽") || !strings.Contains(out.String(), "RUNNING") {
		t.Errorf("stdout missing budget-exhausted status line; got: %q", out.String())
	}
}
