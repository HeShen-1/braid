package main

// cmd/worker 装配决策面单测(T4.5,对抗审计 LOW⑧):buildNotifier 的三分支
// ——整组缺省 → nil(降级);半配置(正向/反向)→ error;全配置 → 非 nil。
// sql.Open 惰性零连接(成功路径仅构造 Sender,不触网不触 DB)。

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"braid/internal/db/sqltest"
	"braid/internal/llm"
	"braid/internal/metrics"
	"braid/internal/notify"
	"braid/internal/snowflake"
	"braid/internal/worker"
)

func TestBuildNotifier(t *testing.T) {
	// 惰性连接池:NewSender 只校验非 nil,不拨号。
	db, err := sql.Open("mysql", "unused:unused@tcp(127.0.0.1:1)/")
	if err != nil {
		t.Fatalf("open stub pool: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ids, err := snowflake.New(9)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	smtpFull := map[string]string{
		"BRAID_SMTP_HOST":         "127.0.0.1",
		"BRAID_SMTP_PORT":         "2525",
		"BRAID_SMTP_STARTTLS":     "false",
		"BRAID_SMTP_FROM":         "braid@test.example",
		"BRAID_SMTP_NOTIFY_TO":    "ops@test.example",
		"BRAID_APPROVAL_BASE_URL": "https://braid.test.example",
	}
	imFull := map[string]string{
		"BRAID_IM_WEBHOOK_URL":    "https://oapi.dingtalk.example/robot/send?access_token=x",
		"BRAID_IM_LINK_SECRET":    "braid-im-link-secret-0123456789ab",
		"BRAID_APPROVAL_BASE_URL": "https://braid.test.example",
	}
	both := map[string]string{}
	for k, v := range smtpFull {
		both[k] = v
	}
	for k, v := range imFull {
		both[k] = v
	}
	cases := []struct {
		name       string
		env        map[string]string
		wantNil    bool
		wantFanout bool
		wantSMTP   bool
		wantIM     bool
		wantError  bool
	}{
		{"unconfigured group degrades to nil", nil, true, false, false, false, false},
		{"knob-only orphans still nil", map[string]string{
			"BRAID_SMTP_PORT": "2525",
		}, true, false, false, false, false},
		{"im link-secret orphan tolerated (dual-end key, api side may own it)", map[string]string{
			"BRAID_IM_LINK_SECRET": "braid-im-link-secret-0123456789ab",
		}, true, false, false, false, false},
		{"smtp-only builds sender", smtpFull, false, false, true, false, false},
		{"im-only builds im sender", imFull, false, false, false, true, false},
		{"both channels build fanout", both, false, true, true, true, false},
		{"smtp forward half-config fails", map[string]string{
			"BRAID_SMTP_HOST":      "127.0.0.1",
			"BRAID_SMTP_FROM":      "braid@test.example",
			"BRAID_SMTP_NOTIFY_TO": "ops@test.example",
			// 缺 BRAID_APPROVAL_BASE_URL
		}, false, false, false, false, true},
		{"smtp reverse half-config fails", map[string]string{
			"BRAID_SMTP_FROM":         "braid@test.example",
			"BRAID_SMTP_NOTIFY_TO":    "ops@test.example",
			"BRAID_APPROVAL_BASE_URL": "https://braid.test.example",
			// 缺 BRAID_SMTP_HOST
		}, false, false, false, false, true},
		{"im half-config fails (webhook without link secret)", map[string]string{
			"BRAID_IM_WEBHOOK_URL":    "https://oapi.dingtalk.example/robot/send?access_token=x",
			"BRAID_APPROVAL_BASE_URL": "https://braid.test.example",
		}, false, false, false, false, true},
		{"shared base url orphan with no channel fails (joint gate)", map[string]string{
			"BRAID_APPROVAL_BASE_URL": "https://braid.test.example",
		}, false, false, false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, smtpOK, imOK, err := buildNotifier(func(k string) string { return tc.env[k] }, db, ids, "test-worker")
			if tc.wantError {
				if err == nil {
					t.Fatalf("buildNotifier(%s) = nil error, want error", tc.name)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildNotifier(%s): %v", tc.name, err)
			}
			if smtpOK != tc.wantSMTP || imOK != tc.wantIM {
				t.Fatalf("buildNotifier(%s) channel flags = (%v,%v), want (%v,%v)",
					tc.name, smtpOK, imOK, tc.wantSMTP, tc.wantIM)
			}
			if tc.wantNil && n != nil {
				t.Fatalf("buildNotifier(%s) = %T, want nil (degraded)", tc.name, n)
			}
			if !tc.wantNil && n == nil {
				t.Fatalf("buildNotifier(%s) = nil, want sender", tc.name)
			}
			if tc.wantFanout && smtpOK && imOK {
				if _, ok := n.(*notify.Fanout); !ok {
					t.Fatalf("buildNotifier(%s) = %T, want *notify.Fanout", tc.name, n)
				}
			}
		})
	}
}

// TestRunRequiresDSN:启动即败路径锚(T7.1 cmd 直读 env 收编后,DSN 必填
// 断言的新位置读 cfg.MySQLDSN):不触网不触 DB——断言在 sql.Open 前返回。
func TestRunRequiresDSN(t *testing.T) {
	t.Setenv("BRAID_MYSQL_DSN", "")
	if err := run(context.Background()); err == nil {
		t.Fatalf("missing DSN accepted")
	}
}

// TestWorkerLeaseConfigHolderAlignment(T20.4 锚,D-t204 决策 3):租约
// Holder 与 cfg.WorkerID 同串——一行回退 DefaultHolder 会让显式
// BRAID_WORKER_ID 形态下 sandboxSweepHoldoff 复核恒空转(误杀面复活)
// 且三件套不红,本锚钉死;能力宣告面(T15.6)同锚。
func TestWorkerLeaseConfigHolderAlignment(t *testing.T) {
	cases := []struct{ name, workerID string }{
		{"explicit id", "braid-worker-a"},
		{"hostname default shape", "host-12345"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := worker.Config{WorkerID: tc.workerID, SnowflakeNode: snowflake.Auto()}
			got := workerLeaseConfig(cfg)
			if got.Kind != snowflake.KindWorker || got.Holder != tc.workerID {
				t.Fatalf("lease config = (%s, %q), want (worker, %q)(Holder 对齐 cfg.WorkerID,D-t204 决策 3)", got.Kind, got.Holder, tc.workerID)
			}
			if got.Spec != cfg.SnowflakeNode {
				t.Fatalf("spec = %+v, want %+v(三层解析产物直传)", got.Spec, cfg.SnowflakeNode)
			}
			want := snowflake.DeclaredCapabilities()
			if len(got.Capabilities) != len(want) {
				t.Fatalf("capabilities = %v, want %v", got.Capabilities, want)
			}
			for i := range want {
				if got.Capabilities[i] != want[i] {
					t.Fatalf("capabilities[%d] = %q, want %q(T15.6 宣告面)", i, got.Capabilities[i], want[i])
				}
			}
		})
	}
}

// TestSandboxSweepHoldoffDecision(T20.4 锚,t13 栈 MySQL;栈不可达自动
// skip):复核决策三态——活体 peer ⇒ 非空(带 peer 详情,供日志/断言);
// 无行/过期 ⇒ ""(清扫照走);复核不可达 ⇒ 非空(fail-safe)。
// run() 内「StartLease 之前」的调用时点契约由 D-t204 决策 2 + 演练
// arm ③ 承载(全 run() 集成测试需整套 env,不在单测面)。
func TestSandboxSweepHoldoffDecision(t *testing.T) {
	dbh := sqltest.MustOpenAt(t, "braid_worker_holdoff_test")
	t.Cleanup(func() { _ = dbh.Close() })
	ctx := context.Background()
	clear := func() {
		t.Helper()
		if _, err := dbh.Exec(`DELETE FROM snowflake_node_lease`); err != nil {
			t.Fatalf("clear leases: %v", err)
		}
	}
	seed := func(holderID string, expiresIn time.Duration) {
		t.Helper()
		if _, err := dbh.Exec(
			`INSERT INTO snowflake_node_lease (node_id, kind, holder_id, expires_at)
			 VALUES (7, 'worker', ?, DATE_ADD(NOW(3), INTERVAL ? MICROSECOND))`,
			holderID, expiresIn.Microseconds()); err != nil {
			t.Fatalf("seed lease: %v", err)
		}
	}
	t.Run("live peer yields holdoff", func(t *testing.T) {
		clear()
		seed("worker:w1", 20*time.Second)
		if h := sandboxSweepHoldoff(ctx, dbh, "w1"); !strings.Contains(h, "worker:w1") {
			t.Fatalf("holdoff = %q, want peer detail(活体 ⇒ 跳过清扫)", h)
		}
	})
	t.Run("expired peer clears", func(t *testing.T) {
		clear()
		seed("worker:w1", -time.Second)
		if h := sandboxSweepHoldoff(ctx, dbh, "w1"); h != "" {
			t.Fatalf("holdoff = %q, want empty(死 owner:清扫照走)", h)
		}
	})
	t.Run("no rows clears", func(t *testing.T) {
		clear()
		if h := sandboxSweepHoldoff(ctx, dbh, "w1"); h != "" {
			t.Fatalf("holdoff = %q, want empty", h)
		}
	})
	t.Run("probe failure fail-safes to holdoff", func(t *testing.T) {
		closed := sqltest.MustOpenAt(t, "braid_worker_holdoff_test")
		if err := closed.Close(); err != nil {
			t.Fatalf("close fixture: %v", err)
		}
		if h := sandboxSweepHoldoff(ctx, closed, "w1"); h == "" {
			t.Fatalf("holdoff = empty, want non-empty(复核不可达 ⇒ 不清)")
		}
	})
}

// TestMetricsVocabularyPinnedToLLM 锚定 llm 失败类/结局词面与 metrics
// 词表常量的字面一致(T6.4):*metrics.LLMSet 隐式满足 llm.Observability,
// 两侧字符串各自独立演进——漂移在这里红,不在告警规则里静默失配。
func TestMetricsVocabularyPinnedToLLM(t *testing.T) {
	pairs := []struct {
		llmStr    string
		metricStr string
	}{
		{llm.HTTPClass429, metrics.CodeRateLimited},
		{llm.HTTPClass5xx, metrics.CodeServerError},
		{llm.HTTPClass4xx, metrics.CodeClientError},
		{llm.HTTPClassNetwork, metrics.CodeNetworkErr},
		{llm.HTTPClassTimeout, metrics.CodeTimeout},
		{llm.HTTPClassProtocol, metrics.CodeProtocolErr},
		{llm.CallOutcomeSucceeded, metrics.CallSucceeded},
		{llm.CallOutcomeFailed, metrics.CallFailed},
	}
	for _, p := range pairs {
		if p.llmStr != p.metricStr {
			t.Errorf("vocabulary drift: llm %q vs metrics %q", p.llmStr, p.metricStr)
		}
	}
	// 隐式接口满足的编译期证明(装配面使用的同一事实)。
	var _ llm.Observability = (*metrics.LLMSet)(nil)
}
