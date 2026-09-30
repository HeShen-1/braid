package notify

// Sender 集成测试:t13 真 MySQL(不可达自动 skip)+ 包内 fake SMTP 服务器
// (最小对话桩,捕获 DATA 原文)。验收锚 = 邮件含审批链接与工作流上下文、
// 去重、发送失败不落审计行(重投可重试)、上下文缺行/坏快照响亮失败。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"braid/internal/audit"
	"braid/internal/auth"
	"braid/internal/db/sqltest"
	"braid/internal/notify/notifytest"
	"braid/internal/snowflake"
)

const notifySchema = "braid_notify_test"

// seedApprovalEscalated 造 T4.5 消费面种子:definition → execution
// (WAITING_APPROVAL 执行级镜像)→ node_execution(WAITING_APPROVAL +
// input_snapshot + deadline + escalated_at,reaper 升级事务后的形态)。
// 返回 (execID, nodeExecID, definitionName)。
func seedApprovalEscalated(t *testing.T, d *sql.DB) (uint64, uint64, string) {
	t.Helper()
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("seed snowflake: %v", err)
	}
	defID, err := ids.Next()
	if err != nil {
		t.Fatalf("gen definition id: %v", err)
	}
	execID, err := ids.Next()
	if err != nil {
		t.Fatalf("gen execution id: %v", err)
	}
	nodeExecID, err := ids.Next()
	if err != nil {
		t.Fatalf("gen node execution id: %v", err)
	}
	name := fmt.Sprintf("notify-wf-%d", defID)
	snapshot := `{"approvers":["carol","dave"],"mode":"any","timeout_hours":24,` +
		`"on_timeout":"escalate","instructions":"请审批这笔报销"}`
	auditID, err := ids.Next()
	if err != nil {
		t.Fatalf("gen audit id: %v", err)
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
		execID, defID, name, string(make([]byte, 64)))
	ins(`INSERT INTO node_execution
	  (id, execution_id, node_id, type, status, attempts, worker_id, version, input_snapshot,
	   deadline_at, escalated_at)
	  VALUES (?, ?, 'seed_node', 'approval', 'WAITING_APPROVAL', 1, 'seed-dispatcher', 2, ?,
	          NOW(3) + INTERVAL 48 HOUR, NOW(3))`,
		nodeExecID, execID, snapshot)
	// reaper 升级事务同款 approval.escalated 审计行(对抗审计 MEDIUM⑥:生产里
	// 消费 approval.notify 时该行必然在场——event_type 是去重查询的唯一判别位,
	// seed 不造则该谓词变异全套假绿)。exec_date 物化与 audit.Append 同式。
	ins(`INSERT INTO audit_log
	  (id, exec_date, event_at, event_type, execution_id, node_execution_id, node_id, actor_type, actor_id, payload)
	  SELECT ?, DATE_FORMAT(created_at, '%Y-%m-%d'), NOW(3), ?, id, ?, 'seed_node', 'system', 'reaper-seed', '{}'
	  FROM workflow_execution WHERE id=?`,
		auditID, audit.EventApprovalEscalated, nodeExecID, execID)
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM audit_log WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM node_execution WHERE id=?`, nodeExecID)
		_, _ = d.Exec(`DELETE FROM workflow_execution WHERE id=?`, execID)
		_, _ = d.Exec(`DELETE FROM workflow_definition WHERE id=?`, defID)
	})
	return execID, nodeExecID, name
}

// newTestSender 起一个指向 fake SMTP 的 Sender(明文、无认证——测试形态)。
func newTestSender(t *testing.T, d *sql.DB, f *notifytest.SMTP) *Sender {
	t.Helper()
	ids, err := snowflake.New(2)
	if err != nil {
		t.Fatalf("sender snowflake: %v", err)
	}
	cfg := DefaultConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = f.Port()
	cfg.From = "braid@test.example"
	cfg.NotifyTo = []string{"ops@test.example", "approval@test.example"}
	cfg.ApprovalBaseURL = "https://braid.test.example"
	cfg.StartTLS = false
	cfg.Timeout = 5 * time.Second
	s, err := NewSender(Deps{DB: d, IDs: ids, WorkerID: "test-worker"}, cfg)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	return s
}

// notifiedRows 数 approval.notified 审计行(去重锚断言)。
func notifiedRows(t *testing.T, d *sql.DB, execID uint64) []auditRowSnapshot {
	t.Helper()
	rows, err := d.Query(`
SELECT id, node_id, payload FROM audit_log
 WHERE execution_id=? AND event_type=? ORDER BY id`, execID, audit.EventApprovalNotified)
	if err != nil {
		t.Fatalf("query notified rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditRowSnapshot
	for rows.Next() {
		var r auditRowSnapshot
		if err := rows.Scan(&r.id, &r.nodeID, &r.payload); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

type auditRowSnapshot struct {
	id      uint64
	nodeID  sql.NullString
	payload []byte
}

// TestNotifyApprovalDeliversContextAndLink(T4.5 验收核心):合法升级事件 →
// 一封邮件,DATA 含审批链接(BaseURL + /executions/<id>/approvals/<node_id>)
// 与工作流上下文(定义名/版本、审批人、说明、成因);信封收件人 = 配置
// NotifyTo;发送成功后 approval.notified 审计行落库(payload 携 node_id/reason)。
func TestNotifyApprovalDeliversContextAndLink(t *testing.T) {
	d := sqltest.MustOpenAt(t, notifySchema)
	f := notifytest.Start(t)
	execID, nodeExecID, defName := seedApprovalEscalated(t, d)
	s := newTestSender(t, d, f)

	if err := s.NotifyApproval(context.Background(), execID, nodeExecID, "escalated", 1); err != nil {
		t.Fatalf("NotifyApproval: %v", err)
	}

	if got := f.Count(); got != 1 {
		t.Fatalf("messages = %d, want 1", got)
	}
	msg := f.LastMessage(t)
	wantLink := "https://braid.test.example/executions/" +
		strconv.FormatUint(execID, 10) + "/approvals/seed_node"
	for _, want := range []string{
		wantLink,
		defName,
		"carol", "dave",
		"请审批这笔报销",
		"escalated",
		"From: braid@test.example",
		"To: ops@test.example, approval@test.example",
		"Subject: =?UTF-8?B?", // 中文主题走 B 编码字
		"Content-Type: text/plain; charset=UTF-8",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q\n--- message ---\n%s", want, msg)
		}
	}
	rcpts := f.LastRcpts(t)
	if len(rcpts) != 2 || rcpts[0] != "ops@test.example" || rcpts[1] != "approval@test.example" {
		t.Fatalf("rcpts = %v, want configured NotifyTo", rcpts)
	}

	rows := notifiedRows(t, d, execID)
	if len(rows) != 1 {
		t.Fatalf("approval.notified rows = %d, want 1", len(rows))
	}
	// 判别位承重锚(对抗审计 MEDIUM⑥):escalated 行在场(seed 复刻 reaper
	// 升级事务)而首投照发——去重查询丢 event_type 谓词在此红。
	var escalatedRows int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log
		WHERE execution_id=? AND event_type=?`, execID, audit.EventApprovalEscalated).
		Scan(&escalatedRows); err != nil {
		t.Fatalf("count escalated rows: %v", err)
	}
	if escalatedRows != 1 {
		t.Fatalf("approval.escalated seed rows = %d, want 1", escalatedRows)
	}
	if rows[0].nodeID.String != "seed_node" {
		t.Fatalf("notified node_id = %q, want seed_node", rows[0].nodeID.String)
	}
	var p audit.ApprovalNotifiedPayload
	if err := json.Unmarshal(rows[0].payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.NodeID != "seed_node" || p.Reason != "escalated" {
		t.Fatalf("payload = %+v, want seed_node/escalated", p)
	}
}

// TestNotifyApprovalDedupOnRedelivery(D-t43「按 task_id 去重」锚):已有
// approval.notified 行(发送成功后落)⇒ 重投幂等跳过——不重发邮件、不落
// 第二行,返回 ErrAlreadyNotified 供 worker 侧映射 ack。
func TestNotifyApprovalDedupOnRedelivery(t *testing.T) {
	d := sqltest.MustOpenAt(t, notifySchema)
	f := notifytest.Start(t)
	execID, nodeExecID, _ := seedApprovalEscalated(t, d)
	s := newTestSender(t, d, f)

	if err := s.NotifyApproval(context.Background(), execID, nodeExecID, "escalated", 1); err != nil {
		t.Fatalf("first NotifyApproval: %v", err)
	}
	err := s.NotifyApproval(context.Background(), execID, nodeExecID, "escalated", 1)
	if !errors.Is(err, ErrAlreadyNotified) {
		t.Fatalf("second NotifyApproval = %v, want ErrAlreadyNotified", err)
	}
	if got := f.Count(); got != 1 {
		t.Fatalf("messages after redelivery = %d, want 1", got)
	}
	if got := len(notifiedRows(t, d, execID)); got != 1 {
		t.Fatalf("approval.notified rows = %d, want 1", got)
	}
}

// TestEncodeHeaderWord:头值编码纪律——非 ASCII 整串 B 编码;ASCII 直通;
// CR/LF(潜伏注入面,sec 评审 LOW①)即使全 ASCII 也必须被编码中和,输出
// 恒无裸控制字符。
func TestEncodeHeaderWord(t *testing.T) {
	cases := []struct {
		in   string
		want string // "raw" = 原样直通;"b64" = =?UTF-8?B? 前缀
	}{
		{"[braid] plain ascii subject", "raw"},
		{"[braid] 审批升级提醒", "b64"},
		{"line1\r\nBcc: victim@example.com", "b64"}, // 注入形:ASCII 但含 CRLF
		{"plain\rwith bare CR", "b64"},
	}
	for _, tc := range cases {
		got := encodeHeaderWord(tc.in)
		switch tc.want {
		case "raw":
			if got != tc.in {
				t.Errorf("encodeHeaderWord(%q) = %q, want raw passthrough", tc.in, got)
			}
		case "b64":
			const p = "=?UTF-8?B?"
			if !strings.HasPrefix(got, p) || !strings.HasSuffix(got, "?=") {
				t.Errorf("encodeHeaderWord(%q) = %q, want %s…?=", tc.in, got, p)
			}
			if strings.ContainsAny(got, "\r\n") {
				t.Errorf("encodeHeaderWord(%q) output carries control chars: %q", tc.in, got)
			}
		}
	}
}

// TestNotifyApprovalSendFailureLeavesNoAudit:发送失败(SMTP 不可达)→
// error 上抛(worker 侧 asynq 重试接管),审计行不落——锚在发送后,失败
// 重投可完整重试。
func TestNotifyApprovalSendFailureLeavesNoAudit(t *testing.T) {
	d := sqltest.MustOpenAt(t, notifySchema)
	// 拿一个确定无人的端口:短暂占位后释放(拨号必拒),避免碰真实服务。
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	deadPort := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	f := notifytest.Start(t) // 消息计数用(应恒 0)
	execID, nodeExecID, _ := seedApprovalEscalated(t, d)
	s := newTestSender(t, d, f)
	s.cfg.Port = deadPort

	if err := s.NotifyApproval(context.Background(), execID, nodeExecID, "escalated", 1); err == nil {
		t.Fatal("NotifyApproval(dead smtp) = nil, want error")
	}
	if got := f.Count(); got != 0 {
		t.Fatalf("messages = %d, want 0", got)
	}
	if got := len(notifiedRows(t, d, execID)); got != 0 {
		t.Fatalf("approval.notified rows = %d, want 0 (no audit on send failure)", got)
	}
}

// TestNotifyApprovalQuitFailureAfterDelivery(review MEDIUM②):DATA 已被
// 服务器接受(w.Close 成功)后 QUIT 失败(421)不构成发送失败——邮件已
// 送达、审计行照落;置之成 error 会必然重发一封且去重锚缺席。
func TestNotifyApprovalQuitFailureAfterDelivery(t *testing.T) {
	d := sqltest.MustOpenAt(t, notifySchema)
	f := notifytest.Start(t)
	f.QuitReply = "421 temporary failure, try again"
	execID, nodeExecID, _ := seedApprovalEscalated(t, d)
	s := newTestSender(t, d, f)

	if err := s.NotifyApproval(context.Background(), execID, nodeExecID, "escalated", 1); err != nil {
		t.Fatalf("NotifyApproval = %v, want nil (quit failure after delivery is non-fatal)", err)
	}
	if got := f.Count(); got != 1 {
		t.Fatalf("messages = %d, want 1", got)
	}
	if got := len(notifiedRows(t, d, execID)); got != 1 {
		t.Fatalf("approval.notified rows = %d, want 1", got)
	}
}

// TestNotifyApprovalStartTLSAndAuth(对抗审计 MEDIUM⑦:生产默认路径锚):
// StartTLS=true(生产默认)+ Username/Password 认证全握手送达。隐式锚:
// 若 STARTTLS 未真升级,net/smtp PlainAuth 对明文非 localhost 连接拒发,
// 本测红;AUTH PLAIN 命令被桩记录显式断言。
func TestNotifyApprovalStartTLSAndAuth(t *testing.T) {
	d := sqltest.MustOpenAt(t, notifySchema)
	f := notifytest.Start(t)
	f.AdvertiseTLS = true
	f.AdvertiseAuth = true
	execID, nodeExecID, _ := seedApprovalEscalated(t, d)

	ids, err := snowflake.New(4)
	if err != nil {
		t.Fatalf("sender snowflake: %v", err)
	}
	cfg := DefaultConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = f.Port()
	cfg.Username = "braid"
	cfg.Password = "secret"
	cfg.From = "braid@test.example"
	cfg.NotifyTo = []string{"ops@test.example"}
	cfg.ApprovalBaseURL = "https://braid.test.example"
	cfg.StartTLS = true // 生产默认路径
	cfg.TLSRootCAs = f.CertPool()
	cfg.Timeout = 5 * time.Second
	s, err := NewSender(Deps{DB: d, IDs: ids, WorkerID: "test-worker"}, cfg)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	if err := s.NotifyApproval(context.Background(), execID, nodeExecID, "escalated", 1); err != nil {
		t.Fatalf("NotifyApproval(starttls+auth): %v", err)
	}
	if got := f.Count(); got != 1 {
		t.Fatalf("messages = %d, want 1", got)
	}
	msg := f.LastMessage(t)
	wantLink := "https://braid.test.example/executions/" +
		strconv.FormatUint(execID, 10) + "/approvals/seed_node"
	if !strings.Contains(msg, wantLink) {
		t.Errorf("message missing %q\n--- message ---\n%s", wantLink, msg)
	}
	if auth := f.LastAuth(t); !strings.HasPrefix(auth, "AUTH PLAIN") {
		t.Fatalf("auth command = %q, want AUTH PLAIN", auth)
	}
}

// TestNotifyApprovalContextDefects:回表缺行 / exec 不匹配 / 坏快照 → 响亮
// error,零邮件零审计(数据缺陷走 archived 死信可见)。
func TestNotifyApprovalContextDefects(t *testing.T) {
	d := sqltest.MustOpenAt(t, notifySchema)
	f := notifytest.Start(t)
	execID, nodeExecID, _ := seedApprovalEscalated(t, d)
	s := newTestSender(t, d, f)

	ctx := context.Background()
	cases := []struct {
		name               string
		execID, nodeExecID uint64
	}{
		{"node vanished", execID, nodeExecID + 404},
		{"execution mismatch", execID + 404, nodeExecID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.NotifyApproval(ctx, tc.execID, tc.nodeExecID, "escalated", 1); err == nil {
				t.Fatal("NotifyApproval = nil, want error")
			}
		})
	}

	// 坏快照:合法 JSON 非对象(DB 层 JSON 校验拦掉语法级坏形,这里破坏
	// 的是 ApprovalInput 形状契约——engine 不变量 WAITING ⇒ 可解码被破坏)。
	t.Run("bad snapshot", func(t *testing.T) {
		if _, err := d.Exec(`UPDATE node_execution SET input_snapshot='[]' WHERE id=?`, nodeExecID); err != nil {
			t.Fatalf("corrupt snapshot: %v", err)
		}
		defer func() {
			_, _ = d.Exec(`UPDATE node_execution SET input_snapshot=? WHERE id=?`,
				`{"approvers":["carol","dave"],"mode":"any","timeout_hours":24,"on_timeout":"escalate","instructions":"请审批这笔报销"}`,
				nodeExecID)
		}()
		if err := s.NotifyApproval(ctx, execID, nodeExecID, "escalated", 1); err == nil {
			t.Fatal("NotifyApproval(bad snapshot) = nil, want error")
		}
	})

	if got := f.Count(); got != 0 {
		t.Fatalf("messages = %d, want 0", got)
	}
	if got := len(notifiedRows(t, d, execID)); got != 0 {
		t.Fatalf("approval.notified rows = %d, want 0", got)
	}
}

// seedFanoutUser 造扇出解析面种子账号(email 为 nil = 未配置)。
func seedFanoutUser(t *testing.T, d *sql.DB, name string, email *string) {
	t.Helper()
	hash, err := auth.HashPassword("fanout-password-1", 4)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	ids, err := snowflake.New(3)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	if err := auth.WithTx(context.Background(), d, func(q auth.DBTX) error {
		return auth.CreateUser(context.Background(), q, ids, auth.CreateUserParams{
			Username: name, PasswordHash: hash, Email: email, Roles: []auth.Role{auth.RoleApprover},
		})
	}); err != nil {
		t.Fatalf("seed user %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE ur FROM user_role ur JOIN users u ON u.id=ur.user_id WHERE u.username LIKE 'fanout-%'`)
		_, _ = d.Exec(`DELETE FROM users WHERE username LIKE 'fanout-%'`)
	})
}

// newFanoutSender:newTestSender 的可变体(扇出用例需覆写 NotifyTo)。
func newFanoutSender(t *testing.T, d *sql.DB, f *notifytest.SMTP, mutate func(*Config)) *Sender {
	t.Helper()
	ids, err := snowflake.New(4)
	if err != nil {
		t.Fatalf("sender snowflake: %v", err)
	}
	cfg := DefaultConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = f.Port()
	cfg.From = "braid@test.example"
	cfg.NotifyTo = []string{"ops@test.example"}
	cfg.ApprovalBaseURL = "https://braid.test.example"
	cfg.StartTLS = false
	cfg.Timeout = 5 * time.Second
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := NewSender(Deps{DB: d, IDs: ids, WorkerID: "test-worker"}, cfg)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	return s
}

// TestNotifyApprovalFanOutPerApprover(T12.2,D-t45 #2 兑现):逐人收件——
// carol(有邮箱)得一封本人 To 的邮件,dave(未配邮箱)不单独发信但名字
// 保留正文;ops 副本(NOTIFY_TO)追加一封;approval.notified 锚恒一行且
// payload 不含任何地址(D-t45 #1 铁律)。变异自查:逐人换单封多 RCPT →
// Count/To 断言红;地址进 payload → payload 断言红;ops 副本丢 → Count 红。
func TestNotifyApprovalFanOutPerApprover(t *testing.T) {
	d := sqltest.MustOpenAt(t, notifySchema)
	f := notifytest.Start(t)
	execID, nodeExecID, _ := seedApprovalEscalated(t, d)
	// seedApprovalEscalated 快照 approvers = ["carol","dave"]:改名入
	// fanout- 前缀(seedFanoutUser 清理域)。
	if _, err := d.Exec(`UPDATE node_execution SET input_snapshot = JSON_SET(input_snapshot,
		'$.approvers', JSON_ARRAY('fanout-carol','fanout-dave')) WHERE id=?`, nodeExecID); err != nil {
		t.Fatalf("rewire approvers: %v", err)
	}
	carolMail := "fanout-carol@example.com"
	seedFanoutUser(t, d, "fanout-carol", &carolMail)
	seedFanoutUser(t, d, "fanout-dave", nil)
	s := newFanoutSender(t, d, f, nil)

	if err := s.NotifyApproval(context.Background(), execID, nodeExecID, "escalated", 1); err != nil {
		t.Fatalf("NotifyApproval: %v", err)
	}

	msgs := f.Messages()
	rcpts := f.RcptsAll()
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2(carol 逐人一封 + ops 副本一封)", len(msgs))
	}
	// 逐人封:To = 本人,信封恰一收件人。
	if !strings.Contains(msgs[0], "To: fanout-carol@example.com") {
		t.Errorf("per-approver To header missing: %s", msgs[0])
	}
	if len(rcpts[0]) != 1 || rcpts[0][0] != carolMail {
		t.Errorf("per-approver envelope = %v, want [%s]", rcpts[0], carolMail)
	}
	// ops 副本:To = NOTIFY_TO;dave 名字保留正文(兜底可见)。
	if !strings.Contains(msgs[1], "To: ops@test.example") {
		t.Errorf("ops copy To header missing: %s", msgs[1])
	}
	for i, m := range msgs {
		if !strings.Contains(m, "fanout-dave") {
			t.Errorf("message %d lacks unresolved approver name in body", i)
		}
	}
	// 送达锚:恰一行,payload 键集 = {node_id, reason, round}——零地址。
	rows := notifiedRows(t, d, execID)
	if len(rows) != 1 {
		t.Fatalf("approval.notified rows = %d, want 1", len(rows))
	}
	var keys map[string]any
	if err := json.Unmarshal(rows[0].payload, &keys); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("payload keys = %v, want exactly node_id/reason/round (no addresses)", keys)
	}
	if strings.Contains(string(rows[0].payload), "@") {
		t.Fatalf("payload carries an address: %s", rows[0].payload)
	}
}

// TestNotifyApprovalNoRecipientsResolvable:纯扇出形态(NOTIFY_TO 空)+
// 零 approver 可解析邮箱(无账号或未配邮箱皆同) = 响亮失败(不静默吞、
// 不落 notified 行——worker 侧 asynq 重试一次后死信,T6.4 告警接住)。
// 不种任何账号:approvers(carol/dave)无账号 = 缺席语义的正路。
func TestNotifyApprovalNoRecipientsResolvable(t *testing.T) {
	d := sqltest.MustOpenAt(t, notifySchema)
	f := notifytest.Start(t)
	execID, nodeExecID, _ := seedApprovalEscalated(t, d)
	s := newFanoutSender(t, d, f, func(c *Config) { c.NotifyTo = nil })

	err := s.NotifyApproval(context.Background(), execID, nodeExecID, "escalated", 1)
	if err == nil || !strings.Contains(err.Error(), "no recipients resolvable") {
		t.Fatalf("err = %v, want no-recipients error", err)
	}
	if got := f.Count(); got != 0 {
		t.Fatalf("messages = %d, want 0", got)
	}
	if got := len(notifiedRows(t, d, execID)); got != 0 {
		t.Fatalf("approval.notified rows = %d, want 0(失败不落锚,重投可完整重试)", got)
	}
}

// TestNotifyApprovalCrossTenantApproverSuppressed(D-t112 审计收口):
// 执行属租户 7、快照名单指向平台用户(全局唯一 username 下结构性可达)
// ——解析携执行租户,平台用户缺席收件面,跨租户邮件推送闭合;ops 副本
// 兜底可见性。变异自查:EmailsByUsernames 去掉 tenant 谓词 → Count 断言
// 红(多出一封发往平台用户的逐人封)。
func TestNotifyApprovalCrossTenantApproverSuppressed(t *testing.T) {
	d := sqltest.MustOpenAt(t, notifySchema)
	f := notifytest.Start(t)
	if _, err := d.Exec(
		`INSERT INTO tenants (id, slug, name) VALUES (7, 't-seven', 'T Seven') ON DUPLICATE KEY UPDATE id = id`); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	execID, nodeExecID, _ := seedApprovalEscalated(t, d)
	// 执行迁入租户 7 + 名单指向平台用户(种子默认租户 0)。
	if _, err := d.Exec(`UPDATE workflow_execution SET tenant_id = 7 WHERE id = ?`, execID); err != nil {
		t.Fatalf("move exec to tenant 7: %v", err)
	}
	if _, err := d.Exec(`UPDATE node_execution SET input_snapshot = JSON_SET(input_snapshot,
		'$.approvers', JSON_ARRAY('fanout-carol')) WHERE id=?`, nodeExecID); err != nil {
		t.Fatalf("rewire approvers: %v", err)
	}
	carolMail := "fanout-carol@example.com"
	seedFanoutUser(t, d, "fanout-carol", &carolMail) // 平台租户(0)
	s := newFanoutSender(t, d, f, nil)

	if err := s.NotifyApproval(context.Background(), execID, nodeExecID, "escalated", 1); err != nil {
		t.Fatalf("NotifyApproval: %v", err)
	}

	msgs := f.Messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1(仅 ops 副本;跨租户名单用户被租户谓词抑制)", len(msgs))
	}
	for _, m := range msgs {
		if strings.Contains(m, carolMail) {
			t.Fatalf("跨租户用户收到推送:%s", carolMail)
		}
	}
}
