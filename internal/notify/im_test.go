package notify

// IMSender 测试(T14.3,D-t143):钉钉加签金向量(独立 Python 预计算)、
// 卡片渲染(按钮对 cap/降级/信息卡/票向文案)、发送面集成(t13 真 MySQL
// + httptest 钉钉桩:送达审计行、按轮去重、errcode 响亮失败、传输错误
// 脱壳不带 webhook 凭证、短链 roundtrip)。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"braid/internal/audit"
	"braid/internal/auth"
	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
)

// 金向量:官方口径 key=secret、msg=`timestamp+"\n"+secret`(官方文档
// Java/Python 样例逐字),本机 Python hmac/hashlib/base64 独立实算。
// 初版向量是 key/msg 交换序的错误值(orch-review HIGH:预计算复刻了
// 同一颠倒,自证测试锁死 bug)——本向量按官方序钉死,回退即红。
func TestDingSignGolden(t *testing.T) {
	got := dingSign(1760000000123, "SEC0123456789abcdef")
	const want = "o9JPuPFX6fUtUNM1+M3wQbDKfG+7kNbz77ubMxH6nYw="
	if got != want {
		t.Fatalf("ding sign: got %s want %s", got, want)
	}
	// 反序钉死(变异锚:若实现回退到交换序,得到的是这个值——结构性
	// 区分官方序与颠倒序,而非仅靠单一向量)。
	const swapped = "IXNWYbw116qZJyeb1S8REBXp19r9CYy6Rb/vkqQA55k="
	if got == swapped {
		t.Fatal("ding sign matches the swapped-operand formula (regression of orch-review HIGH)")
	}
}

// newRenderSender 构造免 DB 的渲染单测面(同包直取,绕过构造闸——构造闸
// 在 NewIMSender / 集成测试覆盖)。
func newRenderSender() *IMSender {
	return &IMSender{
		cfg: IMConfig{
			WebhookURL: "https://oapi.dingtalk.example/robot/send?access_token=x",
			LinkSecret: "braid-im-link-secret-0123456789ab",
			BaseURL:    "https://braid.test.example",
		},
		now: func() time.Time { return time.Unix(1760000000, 0) },
	}
}

func TestIMCardRenderButtons(t *testing.T) {
	s := newRenderSender()
	deadline := sql.NullTime{Time: time.Unix(1760003600, 0).UTC(), Valid: true}
	baseSnap := scheduler.ApprovalInput{
		Approvers:    []string{"carol", "dave"},
		Mode:         scheduler.ApprovalModeAll,
		TimeoutHours: 24, OnTimeout: "escalate",
		Instructions: "请审批\n这笔报销",
	}

	t.Run("pending pairs up to cap, voted shown as text", func(t *testing.T) {
		snap := baseSnap
		body, err := s.renderCard(9, 7, "ap_node", deadline, "wf", 3, snap, "escalated", 1,
			map[string]string{"carol": scheduler.DecisionApproved}, []string{"dave"})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		var card dingActionCard
		if err := json.Unmarshal(body, &card); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if card.MsgType != "actionCard" || len(card.Card.Btns) != 2 {
			t.Fatalf("want 2 buttons for 1 pending, got %+v", card.Card.Btns)
		}
		if card.Card.Btns[0].Title != "dave·批准" || card.Card.Btns[1].Title != "dave·驳回" {
			t.Fatalf("button titles: %+v", card.Card.Btns)
		}
		for _, want := range []string{"carol(已批准)", "dave(待投)", "会签", "截止时刻", "> 请审批", "> 这笔报销"} {
			if !strings.Contains(card.Card.Text, want) {
				t.Fatalf("text missing %q:\n%s", want, card.Card.Text)
			}
		}
	})

	t.Run("deadline invalid omits line without crash", func(t *testing.T) {
		snap := baseSnap
		body, err := s.renderCard(9, 7, "ap_node", sql.NullTime{Valid: false}, "wf", 3, snap,
			"escalated", 1, nil, []string{"carol"})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		var card dingActionCard
		if err := json.Unmarshal(body, &card); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if strings.Contains(card.Card.Text, "截止时刻") {
			t.Fatalf("invalid deadline must omit the line:\n%s", card.Card.Text)
		}
	})

	t.Run("beyond cap degrades to page button", func(t *testing.T) {
		snap := baseSnap
		snap.Approvers = []string{"a", "b", "c", "d"}
		body, err := s.renderCard(9, 7, "ap_node", deadline, "wf", 3, snap, "escalated", 1, nil,
			[]string{"a", "b", "c", "d"})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		var card dingActionCard
		if err := json.Unmarshal(body, &card); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(card.Card.Btns) != 1 || card.Card.Btns[0].Title != "打开审批页" {
			t.Fatalf("want single page button, got %+v", card.Card.Btns)
		}
	})

	t.Run("zero pending renders informational card", func(t *testing.T) {
		snap := baseSnap
		body, err := s.renderCard(9, 7, "ap_node", deadline, "wf", 3, snap, "escalated", 1,
			map[string]string{"carol": scheduler.DecisionApproved, "dave": scheduler.DecisionApproved}, nil)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		var card dingActionCard
		if err := json.Unmarshal(body, &card); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(card.Card.Btns) != 1 || card.Card.Btns[0].Title != "打开审批页" {
			t.Fatalf("want single page button, got %+v", card.Card.Btns)
		}
		if !strings.Contains(card.Card.Text, "投票已齐") {
			t.Fatalf("text missing informational note:\n%s", card.Card.Text)
		}
	})

	t.Run("any mode face and reject vote text", func(t *testing.T) {
		snap := baseSnap
		snap.Mode = ""
		body, err := s.renderCard(9, 7, "ap_node", deadline, "wf", 3, snap, "escalated", 2,
			map[string]string{"carol": scheduler.DecisionRejected}, []string{"dave"})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		var card dingActionCard
		if err := json.Unmarshal(body, &card); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !strings.Contains(card.Card.Text, "或签") || !strings.Contains(card.Card.Text, "carol(已驳回)") {
			t.Fatalf("mode/vote face wrong:\n%s", card.Card.Text)
		}
	})

	t.Run("button action url roundtrips through capability", func(t *testing.T) {
		snap := baseSnap
		body, err := s.renderCard(9, 7, "ap_node", deadline, "wf", 3, snap, "escalated", 1, nil, []string{"carol"})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		var card dingActionCard
		if err := json.Unmarshal(body, &card); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for i, want := range []struct {
			action   string
			approver string
		}{{"approve", "carol"}, {"reject", "carol"}} {
			u, err := url.Parse(card.Card.Btns[i].ActionURL)
			if err != nil {
				t.Fatalf("parse btn url: %v", err)
			}
			if u.Path != "/api/v1/im/approval" {
				t.Fatalf("btn path: %s", u.Path)
			}
			p, err := auth.VerifyIMLink(s.cfg.LinkSecret, u.Query().Get("d"), u.Query().Get("s"),
				s.now().Add(time.Hour))
			if err != nil {
				t.Fatalf("verify btn %d: %v", i, err)
			}
			if p.Action != want.action || p.Approver != want.approver || p.NodeID != "ap_node" ||
				p.ExecutionID != 9 || p.Round != 1 {
				t.Fatalf("btn %d payload: %+v", i, p)
			}
			// 签发面真实形态锚(对抗审计 MEDIUM 修):NodeExecID 必填——
			// api 侧错配守卫依赖它,漏填回退即红。
			if p.NodeExecID != 7 {
				t.Fatalf("btn %d NodeExecID = %d, want 7 (mint-side binding)", i, p.NodeExecID)
			}
		}
	})
}

// imSeed 造 IM 消费面种子(镜像 seedApprovalEscalated,参数化 approvers/
// mode/已投票;node_id 恒 im_node,round 恒 1)。
func imSeed(t *testing.T, d *sql.DB, approvers []string, mode string,
	votes map[string]string) (execID, nodeExecID uint64) {
	t.Helper()
	ids, err := snowflake.New(3)
	if err != nil {
		t.Fatalf("seed snowflake: %v", err)
	}
	next := func() uint64 {
		t.Helper()
		v, err := ids.Next()
		if err != nil {
			t.Fatalf("gen id: %v", err)
		}
		return v
	}
	defID, eid, nid := next(), next(), next()
	name := fmt.Sprintf("im-wf-%d", defID)
	snapJSON, err := json.Marshal(scheduler.ApprovalInput{
		Approvers: approvers, Mode: mode,
		TimeoutHours: 24, OnTimeout: "escalate", Instructions: "请审批这笔报销",
	})
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	ins := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	ins(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 3, 'PUBLISHED', '1.0', '{}', ?, 'seed', 'seed', 'seed', NOW(3))`,
		defID, name, string(make([]byte, 64)))
	ins(`INSERT INTO workflow_execution
	  (id, definition_id, definition_name, definition_version, definition_hash, status, triggered_by, input)
	  VALUES (?, ?, ?, 3, ?, 'WAITING_APPROVAL', 'seed', '{}')`,
		eid, defID, name, string(make([]byte, 64)))
	ins(`INSERT INTO node_execution
	  (id, execution_id, node_id, type, status, attempts, worker_id, version, input_snapshot,
	   deadline_at, escalated_at)
	  VALUES (?, ?, 'im_node', 'approval', 'WAITING_APPROVAL', 1, 'seed-dispatcher', 2, ?,
	          NOW(3) + INTERVAL 48 HOUR, NOW(3))`,
		nid, eid, snapJSON)
	ins(`INSERT INTO audit_log
	  (id, exec_date, event_at, event_type, execution_id, node_execution_id, node_id, actor_type, actor_id, payload)
	  SELECT ?, DATE_FORMAT(created_at, '%Y-%m-%d'), NOW(3), ?, id, ?, 'im_node', 'system', 'reaper-seed', '{}'
	  FROM workflow_execution WHERE id=?`,
		next(), audit.EventApprovalEscalated, nid, eid)
	for by, dec := range votes {
		ins(`INSERT INTO approval_decision
		  (id, execution_id, node_execution_id, node_id, round, decided_by, decision, comment)
		  VALUES (?, ?, ?, 'im_node', 1, ?, ?, NULL)`, next(), eid, nid, by, dec)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM approval_decision WHERE node_execution_id=?`, nid)
		_, _ = d.Exec(`DELETE FROM audit_log WHERE execution_id=?`, eid)
		_, _ = d.Exec(`DELETE FROM node_execution WHERE id=?`, nid)
		_, _ = d.Exec(`DELETE FROM workflow_execution WHERE id=?`, eid)
		_, _ = d.Exec(`DELETE FROM workflow_definition WHERE id=?`, defID)
	})
	return eid, nid
}

// dingStub 起钉钉 webhook 桩:捕获 URL 查询与请求体,按配置回 errcode;
// errcode < 0 = 回无 errcode 键的信封(非钉钉端点形态,误配面)。
type dingStub struct {
	*httptest.Server
	mu      sync.Mutex
	bodies  []dingActionCard
	queries []url.Values
	errcode int
}

func newDingStub(t *testing.T, errcode int) *dingStub {
	t.Helper()
	st := &dingStub{errcode: errcode}
	st.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var card dingActionCard
		_ = json.NewDecoder(r.Body).Decode(&card)
		st.mu.Lock()
		st.bodies = append(st.bodies, card)
		st.queries = append(st.queries, r.URL.Query())
		st.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if errcode < 0 {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"errcode": errcode, "errmsg": "stub"})
	}))
	t.Cleanup(st.Close)
	return st
}

func (st *dingStub) snapshot() (bodies []dingActionCard, queries []url.Values) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]dingActionCard{}, st.bodies...), append([]url.Values{}, st.queries...)
}

func newTestIMSender(t *testing.T, d *sql.DB, webhook, secret string) *IMSender {
	t.Helper()
	ids, err := snowflake.New(4)
	if err != nil {
		t.Fatalf("sender snowflake: %v", err)
	}
	cfg := DefaultIMConfig()
	cfg.WebhookURL = webhook
	cfg.Secret = secret
	cfg.LinkSecret = "braid-im-link-secret-0123456789ab"
	cfg.BaseURL = "https://braid.test.example"
	s, err := NewIMSender(Deps{DB: d, IDs: ids, WorkerID: "test-worker"}, cfg)
	if err != nil {
		t.Fatalf("NewIMSender: %v", err)
	}
	return s
}

func imNotifiedCount(t *testing.T, d *sql.DB, execID uint64) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log
	   WHERE execution_id=? AND event_type=?`, execID, audit.EventApprovalIMNotified).Scan(&n); err != nil {
		t.Fatalf("count im_notified: %v", err)
	}
	return n
}

func TestIMSenderDeliversAndDedups(t *testing.T) {
	d := sqltest.MustOpenAt(t, notifySchema)
	st := newDingStub(t, 0)
	s := newTestIMSender(t, d, st.URL, "")
	execID, nodeExecID := imSeed(t, d, []string{"carol", "dave"}, "all", nil)

	if err := s.NotifyApproval(context.Background(), execID, nodeExecID, "escalated", 1); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	bodies, queries := st.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("want 1 card, got %d", len(bodies))
	}
	if got := queries[0].Get("sign"); got != "" {
		t.Fatalf("secret unset must not sign, got sign=%q", got)
	}
	card := bodies[0]
	if card.MsgType != "actionCard" || len(card.Card.Btns) != 4 {
		t.Fatalf("want 2 pending approvers × 2 buttons, got %+v", card.Card.Btns)
	}
	if n := imNotifiedCount(t, d, execID); n != 1 {
		t.Fatalf("im_notified rows: %d", n)
	}

	// 重投:去重锚命中 ⇒ 幂等哨兵,零新卡。
	if err := s.NotifyApproval(context.Background(), execID, nodeExecID, "escalated", 1); !errors.Is(err, ErrAlreadyNotified) {
		t.Fatalf("want ErrAlreadyNotified, got %v", err)
	}
	if bodies, _ = st.snapshot(); len(bodies) != 1 {
		t.Fatalf("dedup must not resend: %d cards", len(bodies))
	}
	if n := imNotifiedCount(t, d, execID); n != 1 {
		t.Fatalf("im_notified rows after retry: %d", n)
	}

	// 新轮(round 2)= 新通知意图,不被第一轮锚吞。
	if err := s.NotifyApproval(context.Background(), execID, nodeExecID, "escalated", 2); err != nil {
		t.Fatalf("deliver round 2: %v", err)
	}
	if bodies, _ = st.snapshot(); len(bodies) != 2 {
		t.Fatalf("round 2 must deliver: %d cards", len(bodies))
	}
	if n := imNotifiedCount(t, d, execID); n != 2 {
		t.Fatalf("im_notified rows after round 2: %d", n)
	}
}

func TestIMSenderSignedRequest(t *testing.T) {
	d := sqltest.MustOpenAt(t, notifySchema)
	st := newDingStub(t, 0)
	s := newTestIMSender(t, d, st.URL, "SEC0123456789abcdef")
	execID, nodeExecID := imSeed(t, d, []string{"carol"}, "any", nil)
	if err := s.NotifyApproval(context.Background(), execID, nodeExecID, "escalated", 1); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	_, queries := st.snapshot()
	if len(queries) != 1 {
		t.Fatalf("want 1 request, got %d", len(queries))
	}
	q := queries[0]
	ts, sign := q.Get("timestamp"), q.Get("sign")
	if ts == "" || sign == "" {
		t.Fatalf("signed request missing timestamp/sign: %v", q)
	}
	// sign 是 URL 解码后的 Base64(dingSign 输出原样);timestamp 与签名
	// 自洽(用桩捕获的 timestamp 复算)。
	if dingSign(mustParseI64(t, ts), "SEC0123456789abcdef") != sign {
		t.Fatalf("sign mismatch for captured timestamp %s", ts)
	}
}

func mustParseI64(t *testing.T, v string) int64 {
	t.Helper()
	var n int64
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		t.Fatalf("parse %q: %v", v, err)
	}
	return n
}

func TestIMSenderErrcodeFailsLoud(t *testing.T) {
	d := sqltest.MustOpenAt(t, notifySchema)
	st := newDingStub(t, 310000)
	s := newTestIMSender(t, d, st.URL, "")
	execID, nodeExecID := imSeed(t, d, []string{"carol"}, "any", nil)
	err := s.NotifyApproval(context.Background(), execID, nodeExecID, "escalated", 1)
	if err == nil || !strings.Contains(err.Error(), "errcode=310000") {
		t.Fatalf("want errcode error, got %v", err)
	}
	// 失败不落审计行(重投可重试,D-t45 同纪律)。
	if n := imNotifiedCount(t, d, execID); n != 0 {
		t.Fatalf("failed delivery must not write anchor: %d", n)
	}
}

func TestIMSenderTransportErrorSanitized(t *testing.T) {
	d := sqltest.MustOpenAt(t, notifySchema)
	// 已关闭端口 + webhook 携带 access_token 凭证段:错误面不得带出 URL。
	st := newDingStub(t, 0)
	webhook := st.URL + "/robot/send?access_token=SECRETOKEN123"
	st.Close()
	s := newTestIMSender(t, d, webhook, "")
	execID, nodeExecID := imSeed(t, d, []string{"carol"}, "any", nil)
	err := s.NotifyApproval(context.Background(), execID, nodeExecID, "escalated", 1)
	if err == nil {
		t.Fatal("want transport error")
	}
	if strings.Contains(err.Error(), "SECRETOKEN123") || strings.Contains(err.Error(), "access_token") {
		t.Fatalf("error leaks webhook credential: %v", err)
	}
}

// TestIMSenderChannelDedupIsolation:per-channel 锚隔离负例(对抗审计
// MEDIUM 变异实证:dedup 谓词去 event_type 过滤全绿)——SMTP 的
// approval.notified 行在场不得遮蔽 IM 通道(反之亦然,D-t143 决策 5)。
func TestIMSenderChannelDedupIsolation(t *testing.T) {
	d := sqltest.MustOpenAt(t, notifySchema)
	st := newDingStub(t, 0)
	s := newTestIMSender(t, d, st.URL, "")
	execID, nodeExecID := imSeed(t, d, []string{"carol"}, "any", nil)

	// 模拟 SMTP 通道已送达:落一行 approval.notified(同 exec/node/round)。
	ids2, err := snowflake.New(5)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	auditID, err := ids2.Next()
	if err != nil {
		t.Fatalf("gen audit id: %v", err)
	}
	raw, _ := json.Marshal(audit.ApprovalNotifiedPayload{NodeID: "im_node", Reason: "escalated", Round: 1})
	if err := engine.WithTx(context.Background(), d, func(q engine.DBTX) error {
		return audit.Append(context.Background(), q, audit.Event{
			ID: auditID, EventType: audit.EventApprovalNotified,
			ExecutionID: execID, NodeExecID: nodeExecID, NodeID: "im_node",
			ActorType: audit.ActorWorker, ActorID: "test-worker", Payload: raw,
		})
	}); err != nil {
		t.Fatalf("seed smtp notified row: %v", err)
	}

	// IM 通道照常送达(不被 SMTP 锚短路)+ 落自家 im_notified 锚。
	if err := s.NotifyApproval(context.Background(), execID, nodeExecID, "escalated", 1); err != nil {
		t.Fatalf("im must deliver despite smtp anchor: %v", err)
	}
	if bodies, _ := st.snapshot(); len(bodies) != 1 {
		t.Fatalf("want 1 card, got %d", len(bodies))
	}
	if n := imNotifiedCount(t, d, execID); n != 1 {
		t.Fatalf("im_notified rows = %d", n)
	}
}

// TestIMSenderMissingErrcodeEnvelope:200 + 无 errcode 键 = 非钉钉信封
// (误配面),响亮失败不落锚(对抗审计 LOW 修)。
func TestIMSenderMissingErrcodeEnvelope(t *testing.T) {
	d := sqltest.MustOpenAt(t, notifySchema)
	st := newDingStub(t, -1) // <0 = 无 errcode 键信封
	s := newTestIMSender(t, d, st.URL, "")
	execID, nodeExecID := imSeed(t, d, []string{"carol"}, "any", nil)
	err := s.NotifyApproval(context.Background(), execID, nodeExecID, "escalated", 1)
	if err == nil || !strings.Contains(err.Error(), "missing errcode") {
		t.Fatalf("want missing-errcode error, got %v", err)
	}
	if n := imNotifiedCount(t, d, execID); n != 0 {
		t.Fatalf("non-dingtalk envelope must not write anchor: %d", n)
	}
}

// TestIMSenderTenantGuard:租户闸(对抗审计 HIGH 修)——非平台租户执行
// 响亮拒发,零卡零审计行;多租户部署的 IM 通道 = 契约禁用(D-t143 #6)。
func TestIMSenderTenantGuard(t *testing.T) {
	d := sqltest.MustOpenAt(t, notifySchema)
	st := newDingStub(t, 0)
	s := newTestIMSender(t, d, st.URL, "")
	execID, nodeExecID := imSeed(t, d, []string{"carol"}, "any", nil)
	// tenant 行先种(FK fk_exec_tenant RESTRICT;镜像 api 租户测试种子)。
	if _, err := d.Exec(`INSERT INTO tenants (id, slug, name) VALUES (5, 'im-t5', 'IM T5')
		ON DUPLICATE KEY UPDATE id = id`); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM tenants WHERE id=5`) })
	if _, err := d.Exec(`UPDATE workflow_execution SET tenant_id=5 WHERE id=?`, execID); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	err := s.NotifyApproval(context.Background(), execID, nodeExecID, "escalated", 1)
	if err == nil || !strings.Contains(err.Error(), "single-tenant") {
		t.Fatalf("want single-tenant loud error, got %v", err)
	}
	if bodies, _ := st.snapshot(); len(bodies) != 0 {
		t.Fatalf("tenant guard must not deliver: %d cards", len(bodies))
	}
	if n := imNotifiedCount(t, d, execID); n != 0 {
		t.Fatalf("tenant guard must not write anchor: %d", n)
	}
}

func TestIMSenderPartiallyVotedAndZeroPending(t *testing.T) {
	d := sqltest.MustOpenAt(t, notifySchema)
	st := newDingStub(t, 0)
	s := newTestIMSender(t, d, st.URL, "")

	// 会签部分批准(D-t142 中间态):卡片只给未投者按钮对,已投者文本化。
	execID, nodeExecID := imSeed(t, d, []string{"carol", "dave", "erin"}, "all",
		map[string]string{"carol": scheduler.DecisionApproved})
	if err := s.NotifyApproval(context.Background(), execID, nodeExecID, "escalated", 1); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	bodies, _ := st.snapshot()
	if len(bodies) != 1 || len(bodies[0].Card.Btns) != 4 {
		t.Fatalf("want 2 pending × 2 buttons, got %+v", bodies)
	}
	if !strings.Contains(bodies[0].Card.Text, "carol(已批准)") {
		t.Fatalf("text missing voted face:\n%s", bodies[0].Card.Text)
	}

	// 零 pending(窗内票齐):信息卡,单按钮,非错误。
	execID2, nodeExecID2 := imSeed(t, d, []string{"carol", "dave"}, "all",
		map[string]string{"carol": scheduler.DecisionApproved, "dave": scheduler.DecisionApproved})
	if err := s.NotifyApproval(context.Background(), execID2, nodeExecID2, "escalated", 1); err != nil {
		t.Fatalf("zero-pending must be informational, got %v", err)
	}
	bodies, _ = st.snapshot()
	if len(bodies) != 2 || len(bodies[1].Card.Btns) != 1 || bodies[1].Card.Btns[0].Title != "打开审批页" {
		t.Fatalf("want informational single-button card, got %+v", bodies)
	}
	if n := imNotifiedCount(t, d, execID2); n != 1 {
		t.Fatalf("informational card must still write anchor: %d", n)
	}
}
