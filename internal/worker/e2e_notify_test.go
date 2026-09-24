package worker

// T4.5 验收链(真 asynq server + 真 MySQL/Redis + 真 notify.Sender →
// notifytest fake SMTP,栈不可达自动 skip):
//
//	验收原文:「fake SMTP 集成测试断言邮件含审批链接与工作流上下文」
//	= TestApprovalNotifySMTPDelivery 三段断言(worker.Run 生产路径全链):
//	  ① 邮件 DATA 含审批链接(BaseURL + /executions/<id>/approvals/<node>)
//	     与工作流上下文(定义名/版本、审批人、说明);
//	  ② 发送成功后 approval.notified 审计行落库;
//	  ③ 同 TaskID 完成即删后重投(模拟 relay 重投/asynq at-least-once)
//	     ⇒ 去重锚收敛:零新邮件、零新审计行、任务正常 ack。
//
// 变异自查(实现错 → 哪条红):
//   - handler 不分派 Notifier → fake SMTP 零捕获,①红;
//   - 去重查缺(只发不查)→ 重投后 Count=2,③红;
//   - 审计行不落(发送后跳过)→ ②红(且 ③ 的去重锚失效连坐);
//   - 链接拼错/上下文漏渲染 → ① 红。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"braid/internal/audit"
	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/notify"
	"braid/internal/notify/notifytest"
	"braid/internal/redistest"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
)

// seedWaitingApprovalEscalated 造 reaper 升级事务后的业务链种子(worker 包
// schema):definition → execution(WAITING_APPROVAL)→ node_execution
// (WAITING_APPROVAL + 快照 + deadline + escalated_at)。
func seedWaitingApprovalEscalated(t *testing.T, d *sql.DB) (execID, nodeExecID uint64, defName string) {
	t.Helper()
	ids := mustTestIDs(t)
	defID, err := ids.Next()
	if err != nil {
		t.Fatalf("gen definition id: %v", err)
	}
	execIDNum, err := ids.Next()
	if err != nil {
		t.Fatalf("gen execution id: %v", err)
	}
	nodeExecIDNum, err := ids.Next()
	if err != nil {
		t.Fatalf("gen node execution id: %v", err)
	}
	name := fmt.Sprintf("e2e-notify-wf-%d", defID)
	snapshot := `{"approvers":["carol","dave"],"mode":"any","timeout_hours":24,` +
		`"on_timeout":"escalate","instructions":"请审批这笔采购单"}`
	ins := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	ins(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 2, 'PUBLISHED', '1.0', '{}', ?, 'seed', 'seed', 'seed', NOW(3))`,
		defID, name, string(make([]byte, 64)))
	ins(`INSERT INTO workflow_execution
	  (id, definition_id, definition_name, definition_version, definition_hash, status, triggered_by, input)
	  VALUES (?, ?, ?, 2, ?, 'WAITING_APPROVAL', 'seed', '{}')`,
		execIDNum, defID, name, string(make([]byte, 64)))
	ins(`INSERT INTO node_execution
	  (id, execution_id, node_id, type, status, attempts, worker_id, version, input_snapshot,
	   deadline_at, escalated_at)
	  VALUES (?, ?, 'pay_approval', 'approval', 'WAITING_APPROVAL', 1, 'seed-dispatcher', 2, ?,
	          NOW(3) + INTERVAL 48 HOUR, NOW(3))`,
		nodeExecIDNum, execIDNum, snapshot)
	// reaper 升级事务同款 approval.escalated 审计行(对抗审计 MEDIUM⑥:
	// event_type 是去重查询唯一判别位,seed 不造则该谓词变异假绿)。
	auditID, err := ids.Next()
	if err != nil {
		t.Fatalf("gen audit id: %v", err)
	}
	ins(`INSERT INTO audit_log
	  (id, exec_date, event_at, event_type, execution_id, node_execution_id, node_id, actor_type, actor_id, payload)
	  SELECT ?, DATE_FORMAT(created_at, '%Y-%m-%d'), NOW(3), ?, id, ?, 'pay_approval', 'system', 'reaper-seed', '{}'
	  FROM workflow_execution WHERE id=?`,
		auditID, audit.EventApprovalEscalated, nodeExecIDNum, execIDNum)
	// 共享 schema(braid_worker_test)跨运行累积防污染:按 id 清四表
	// (sender_test seed 同款卫生,对抗审计 LOW⑨)。
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM audit_log WHERE execution_id=?`, execIDNum)
		_, _ = d.Exec(`DELETE FROM node_execution WHERE id=?`, nodeExecIDNum)
		_, _ = d.Exec(`DELETE FROM workflow_execution WHERE id=?`, execIDNum)
		_, _ = d.Exec(`DELETE FROM workflow_definition WHERE id=?`, defID)
	})
	return execIDNum, nodeExecIDNum, name
}

// e2eNotifier 起指向 fake SMTP 的生产 Sender(T4.5 换装体的真实现)。
func e2eNotifier(t *testing.T, d *sql.DB, f *notifytest.SMTP) *notify.Sender {
	t.Helper()
	ids, err := snowflake.New(3)
	if err != nil {
		t.Fatalf("notify snowflake: %v", err)
	}
	cfg := notify.DefaultConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = f.Port()
	cfg.From = "braid@test.example"
	cfg.NotifyTo = []string{"ops@test.example"}
	cfg.ApprovalBaseURL = "https://braid.test.example"
	cfg.StartTLS = false
	cfg.Timeout = 5 * time.Second
	s, err := notify.NewSender(notify.Deps{DB: d, IDs: ids, WorkerID: "e2e-worker"}, cfg)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	return s
}

// enqueueNotify 以 relay 同款 option 面投 approval.notify(确定性 TaskID +
// MaxRetry=1;零 Retention——完成即删,给重投腾出 TaskID)。
func enqueueNotify(t *testing.T, queue, taskID string, execID, nodeExecID uint64) {
	t.Helper()
	payload, err := json.Marshal(scheduler.ApprovalNotifyPayload{
		ExecutionID: execID, NodeExecutionID: nodeExecID, Reason: "escalated",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	client := asynq.NewClient(redistest.MustOpt(t))
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := client.EnqueueContext(ctx,
		asynq.NewTask(scheduler.EventTypeApprovalNotify, payload),
		asynq.TaskID(taskID), asynq.Queue(queue), asynq.MaxRetry(1)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
}

func TestApprovalNotifySMTPDelivery(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	f := notifytest.Start(t)
	execID, nodeExecID, defName := seedWaitingApprovalEscalated(t, d)
	taskID := "notify:" + strconv.FormatUint(nodeExecID, 10)

	ex := &fakeExec{fn: func(req Request, callNo int) (Outcome, error) {
		return Outcome{Status: engine.AttemptSucceed}, nil
	}}
	cfg := DefaultConfig()
	cfg.RetryDelay = 50 * time.Millisecond
	queue := startWorkerOpt(t, d, ex, cfg, e2eNotifier(t, d, f))
	insp := asynq.NewInspector(redistest.MustOpt(t))
	t.Cleanup(func() { _ = insp.Close() })

	// ① 首投:worker 生产路径消费 → Sender → fake SMTP。
	enqueueNotify(t, queue, taskID, execID, nodeExecID)
	waitTaskGone(t, insp, queue, taskID)

	if got := f.Count(); got != 1 {
		t.Fatalf("messages = %d, want 1", got)
	}
	msg := f.LastMessage(t)
	wantLink := "https://braid.test.example/executions/" +
		strconv.FormatUint(execID, 10) + "/approvals/pay_approval"
	for _, want := range []string{
		wantLink,
		defName,
		"carol", "dave",
		"请审批这笔采购单",
		"escalated",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q\n--- message ---\n%s", want, msg)
		}
	}

	// ② 送达审计行落库(去重锚的写半边)。
	var notified int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log
		WHERE execution_id=? AND node_execution_id=? AND event_type=?`,
		execID, nodeExecID, audit.EventApprovalNotified).Scan(&notified); err != nil {
		t.Fatalf("count notified rows: %v", err)
	}
	if notified != 1 {
		t.Fatalf("approval.notified rows = %d, want 1", notified)
	}

	// ③ 同 TaskID 重投(完成即删后的新投递 = asynq at-least-once 重复面):
	// 去重锚收敛——零新邮件、零新审计行、任务正常 ack(记录消失)。
	enqueueNotify(t, queue, taskID, execID, nodeExecID)
	waitTaskGone(t, insp, queue, taskID)
	if got := f.Count(); got != 1 {
		t.Fatalf("messages after redelivery = %d, want 1 (deduped)", got)
	}
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log
		WHERE execution_id=? AND node_execution_id=? AND event_type=?`,
		execID, nodeExecID, audit.EventApprovalNotified).Scan(&notified); err != nil {
		t.Fatalf("recount notified rows: %v", err)
	}
	if notified != 1 {
		t.Fatalf("approval.notified rows after redelivery = %d, want 1", notified)
	}
	if got := ex.count(); got != 0 {
		t.Errorf("executor calls = %d, want 0(通知事件不经节点执行器)", got)
	}
}
