package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"braid/internal/auth"
	"braid/internal/db/sqltest"
	"braid/internal/metrics"
	"braid/internal/snowflake"
	"braid/internal/tools"
)

// TestKnownLoginEventsClosedVocabulary:词表闭集锚——DDL ENUM(0005)与
// Go 侧四词同 PR 旅行,漂移在此红。
func TestKnownLoginEventsClosedVocabulary(t *testing.T) {
	want := map[string]bool{
		"login.succeeded": true, "login.failed": true,
		"login.locked": true, "login.rate_limited": true,
	}
	if len(knownLoginEvents) != len(want) {
		t.Fatalf("knownLoginEvents = %v, want exactly %v", knownLoginEvents, want)
	}
	for w := range want {
		if !knownLoginEvents[w] {
			t.Errorf("word %q missing from Go-side vocabulary", w)
		}
	}
}

// TestLoginDefenseAndAudit(T6.4 验收面):独立紧旋钮 server 上的完整登录
// 防御 + 审计旅程——成功/失败/锁定/限流四分支各自落 login_audit 行与
// braid_login_attempts_total 计数;锁定与限流对外同形 429(防枚举)。
func TestLoginDefenseAndAudit(t *testing.T) {
	dbh := newLoginDefenseDB(t)
	seedLoginUser(t, dbh, "bob", "bob-pass-123", "ACTIVE")
	seedLoginUser(t, dbh, "alice", "alice-pass-123", "ACTIVE")

	cfg := Config{Addr: DefaultAddr, JWTSecret: testSecret, SnowflakeNode: 1,
		LoginRateLimit: 3, LoginRateWindow: time.Minute,
		LoginMaxFailures: 2, LoginLockout: 10 * time.Minute}
	set, _ := metrics.NewSet()
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	reg, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	gin.SetMode(gin.TestMode)
	srv := NewServer(cfg, dbh, ids, reg, fakeTrigger{id: 1}, auth.BuiltinProvider{DB: dbh})
	srv.loginMetrics = set.Login
	r := srv.Router()
	login := func(t *testing.T, body string) (int, map[string]any) {
		t.Helper()
		return doJSON(t, r, http.MethodPost, "/api/v1/auth/login", "", []byte(body))
	}

	// wantEvent 空 = 防御拒绝路径(review 修:metric-only 不落 DB)。
	steps := []struct {
		name       string
		body       string
		wantCode   int
		wantErr    string
		wantEvent  string
		wantReason any // nil = NULL
	}{
		{"bob success", `{"username":"bob","password":"bob-pass-123"}`, 200, "", "login.succeeded", nil},
		{"alice wrong pw 1", `{"username":"alice","password":"wrong-password-1"}`, 401, "invalid_credentials", "login.failed", "wrong_password"},
		{"alice wrong pw 2 (locks)", `{"username":"alice","password":"wrong-password-2"}`, 401, "invalid_credentials", "login.failed", "wrong_password"},
		{"alice correct pw but locked", `{"username":"alice","password":"alice-pass-123"}`, 429, "too_many_attempts", "", nil},
		{"bob collides with ip window", `{"username":"bob","password":"bob-pass-123"}`, 429, "too_many_attempts", "", nil},
		// 窗口(rate=3)此时已被前 5 次耗尽:坏 body 也吃 429——它同样
		// 计入 IP 窗口(review HIGH 修复面:malformed 不再绕过限流)。
		{"malformed body consumes ip window", `{not-json`, 429, "too_many_attempts", "", nil},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			before := loginAuditRows(t, dbh)
			code, body := login(t, st.body)
			if code != st.wantCode {
				t.Fatalf("code = %d body %v, want %d", code, body, st.wantCode)
			}
			if st.wantErr != "" {
				if body["error"] != st.wantErr {
					t.Fatalf("error = %v, want %q", body["error"], st.wantErr)
				}
			}
			if st.wantEvent == "" {
				// 防御拒绝/坏 body:零新行(写放大防线)。
				if after := loginAuditRows(t, dbh); after != before {
					t.Fatalf("login_audit rows %d → %d on rejected path, want no durable write", before, after)
				}
				return
			}
			var username string
			_ = json.Unmarshal([]byte(st.body), &struct {
				Username *string `json:"username"`
			}{Username: &username})
			assertLoginAuditRow(t, dbh, st.wantEvent, username, st.wantReason)
		})
	}

	// 指标半边:四 label 计数(成功 1 / 判定失败 2 / 锁定 1 / 限流 2 =
	// 步骤4 bob + 步骤5 坏 body;防御拒绝与坏 body 皆零 DB 行)。
	for label, want := range map[string]float64{
		metrics.ResultSucceeded: 1, metrics.ResultFailed: 2,
		metrics.ResultLocked: 1, metrics.ResultRateLimited: 2,
	} {
		if got := testutil.ToFloat64(set.Login.Attempts.WithLabelValues(label)); got != want {
			t.Errorf("login_attempts{%s} = %v, want %v", label, got, want)
		}
	}
	// 审计行总数恰 3(判定行 only:成功 1 + 失败 2;防御拒绝零行)。
	assertLoginAuditCount(t, dbh, 3)
}

// TestLoginAuditNoCredentialLeak:铁律锚——审计行永不携密码/token。
func TestLoginAuditNoCredentialLeak(t *testing.T) {
	dbh := newLoginDefenseDB(t)
	seedLoginUser(t, dbh, "eve", "eve-secret-999", "ACTIVE")
	cfg := Config{Addr: DefaultAddr, JWTSecret: testSecret, SnowflakeNode: 1,
		LoginRateLimit: 100000, LoginRateWindow: time.Minute,
		LoginMaxFailures: 100000, LoginLockout: time.Minute}
	ids, _ := snowflake.New(1)
	reg, _ := tools.NewRegistry()
	gin.SetMode(gin.TestMode)
	srv := NewServer(cfg, dbh, ids, reg, fakeTrigger{id: 1}, auth.BuiltinProvider{DB: dbh})
	r := srv.Router()
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/auth/login", "",
		[]byte(`{"username":"eve","password":"eve-secret-999"}`))
	if code != 200 {
		t.Fatalf("code = %d %v", code, body)
	}
	token, _ := body["token"].(string)
	assertLoginAuditRow(t, dbh, "login.succeeded", "eve", nil)
	// 凭证扫描:全表任何列不得含密码或签发 token 片段。
	var hits int
	if err := dbh.QueryRow(`SELECT COUNT(*) FROM login_audit
	  WHERE CONCAT_WS('|', event_type, username, client_ip, IFNULL(reason,'')) LIKE ?`,
		"%eve-secret-999%").Scan(&hits); err != nil {
		t.Fatalf("scan leak: %v", err)
	}
	if hits != 0 {
		t.Fatalf("password leaked into login_audit: %d rows", hits)
	}
	if token != "" {
		var tok int
		if err := dbh.QueryRow(`SELECT COUNT(*) FROM login_audit WHERE client_ip = ?`,
			token[:min(20, len(token))]).Scan(&tok); err == nil && tok > 0 {
			// client_ip 列理论上装不下 token;防御性再查一列足矣。
			t.Fatalf("token fragment landed in client_ip")
		}
	}
}

// newLoginDefenseDB:登录防御用例的独立 DB 面(t13;缺栈自动 skip)。
// users 表消费方自清纪律(login_test.go 注释):先清后种。
func newLoginDefenseDB(t *testing.T) *sql.DB {
	t.Helper()
	dbh := sqltest.MustOpenAt(t, "braid_api_test")
	for _, q := range []string{
		"DELETE FROM login_audit",
		// T11.3 DB 锚定:防御行随测试共享 schema,httptest 同源 IP 的
		// 窗口残留会让紧旋钮断言(恰第 N 次 429)全体漂移。
		"DELETE FROM login_throttle",
		"DELETE FROM user_role", "DELETE FROM users",
	} {
		if _, err := dbh.Exec(q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
	t.Cleanup(func() {
		_, _ = dbh.Exec("DELETE FROM login_audit")
		_, _ = dbh.Exec("DELETE FROM login_throttle")
	})
	return dbh
}

// seedLoginUser:可指定密码与状态的种子(独立于 seedUser 的固定密码形态)。
func seedLoginUser(t *testing.T, dbh *sql.DB, username, password, status string) {
	t.Helper()
	hash, err := auth.HashPassword(password, bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := auth.CreateUser(t.Context(), dbh, loginTestIDs,
		auth.CreateUserParams{Username: username, PasswordHash: hash,
			Roles: []auth.Role{auth.RoleApprover}}); err != nil {
		t.Fatalf("create user %s: %v", username, err)
	}
	if status != "ACTIVE" {
		if _, err := dbh.Exec("UPDATE users SET status=? WHERE username=?", status, username); err != nil {
			t.Fatalf("set status %s: %v", status, err)
		}
	}
}

// assertLoginAuditRow:最新一行的形状锚(event/username/reason;client_ip
// 由 httptest RemoteAddr 非空即可)。
func assertLoginAuditRow(t *testing.T, dbh *sql.DB, event, username string, wantReason any) {
	t.Helper()
	var gotEvent, gotUser, gotIP string
	var gotReason sql.NullString
	err := dbh.QueryRow(`SELECT event_type, username, client_ip, reason
	  FROM login_audit ORDER BY id DESC LIMIT 1`).
		Scan(&gotEvent, &gotUser, &gotIP, &gotReason)
	if err != nil {
		t.Fatalf("read latest login_audit row: %v", err)
	}
	if gotEvent != event {
		t.Errorf("event = %s, want %s", gotEvent, event)
	}
	if wantUser := auditUsername(username); gotUser != wantUser {
		t.Errorf("username = %s, want %s", gotUser, wantUser)
	}
	if gotIP == "" {
		t.Errorf("client_ip empty")
	}
	if wantReason == nil && gotReason.Valid {
		t.Errorf("reason = %q, want NULL", gotReason.String)
	}
	if wantReason != nil {
		if w, ok := wantReason.(string); !ok || !gotReason.Valid || gotReason.String != w {
			t.Errorf("reason = %v, want %v", gotReason.String, wantReason)
		}
	}
}

func assertLoginAuditCount(t *testing.T, dbh *sql.DB, want int) {
	t.Helper()
	if got := loginAuditRows(t, dbh); got != want {
		t.Fatalf("login_audit rows = %d, want %d", got, want)
	}
}

func loginAuditRows(t *testing.T, dbh *sql.DB) int {
	t.Helper()
	var got int
	if err := dbh.QueryRow(`SELECT COUNT(*) FROM login_audit`).Scan(&got); err != nil {
		t.Fatalf("count: %v", err)
	}
	return got
}

// TestAuditUsernameMultibyteBounded:auditUsername 的 byte 截断 + UTF-8
// 边界回退锚(审计 LOW:0010 注释引用的「同式」截断路径此前零单测)。
func TestAuditUsernameMultibyteBounded(t *testing.T) {
	in := strings.Repeat("张", 50) // 150 bytes > 128:截断点落「张」中段
	got := auditUsername(in)
	if len(got) != 126 { // 128 回退到 rune 边界 = 42×3
		t.Fatalf("截断长度 = %d, want 126(rune 边界回退 2 bytes)", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("截断产物非合法 UTF-8: %q", got)
	}
	if !strings.HasPrefix(got, strings.Repeat("张", 42)) {
		t.Fatalf("截断丢前缀: %q", got)
	}
	if auditUsername("carol") != "carol" {
		t.Fatal("短输入须原样")
	}
}
