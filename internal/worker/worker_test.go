package worker

// T2.3 验收链(真 asynq server + 真 MySQL/Redis,栈不可达自动 skip):
//
//	验收原文:「重复投递被 ErrTaskIDConflict 收敛,attempt 落库」
//	= TestAttemptRecordedAndDeliverConverged 四断言(在飞冲突 / attempt 落库 /
//	  完成即删 / 删后重投由 uk_attempt 收敛)。
//
// 变异自查(实现错 → 哪条红):
//   - claimAttempt 撞键不读行直接重跑 → 删后重投 executor 计数 2 红;
//   - InsertAttempt 被绕过(不建行)→ 落库断言红(行不存在);
//   - Outcome/error 语义混用(业务失败返回 error)→ TestRetryTakeoverTakeover
//     红(业务失败应收口 FAILED 而非重试);
//   - 心跳没接 → TestHeartbeatDuringExecution 红(负向活性对照);
//   - RetryDelayFunc 未显式钉死 → TestMaxRetryExhaustedArchived 时序漂移
//     (server 默认 15-45s 随机退避,15s 轮询窗口大概率超时)红。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/notify"
	"braid/internal/redistest"
	"braid/internal/scheduler"
)

// TestAttemptRecordedAndDeliverConverged 是验收主链:
//  1. 在飞重投(asynq 任务记录存活期内)→ ErrTaskIDConflict 收敛;
//  2. attempt 落库并收口(RUNNING→SUCCEEDED,幂等键/连接列/worker 归因齐全);
//  3. Retention=0 下完成即删任务记录(执行级幂等锚必须高于 TaskID 的根因);
//  4. 记录删除后同 TaskID 重投(真正的 at-least-once 重复)→ 入队成功,
//     worker 撞 uk_attempt 读到终态行 → 幂等返回,executor 不被二次调。
func TestAttemptRecordedAndDeliverConverged(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	execID, nodeExecID := seedNodeExec(t, d)

	inFlight := make(chan struct{})
	release := make(chan struct{})
	ex := &fakeExec{fn: func(req Request, callNo int) (Outcome, error) {
		if callNo == 1 {
			close(inFlight)
			<-release // 制造在飞窗:任务记录存活,重投必撞 TaskID 唯一性
		}
		return Outcome{Status: engine.AttemptSucceed}, nil
	}}
	queue := startWorker(t, d, ex, DefaultConfig())

	client := asynq.NewClient(redistest.MustOpt(t))
	t.Cleanup(func() { _ = client.Close() })
	taskID := fmt.Sprintf("att:%d:1", nodeExecID)
	p := nodeTaskPayload{ExecutionID: execID, NodeExecutionID: nodeExecID, AttemptNo: 1}
	if _, err := enqueue(t, client, queue, taskID, p); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}

	select {
	case <-inFlight:
	case <-time.After(15 * time.Second):
		t.Fatal("executor never started (task not consumed within 15s)")
	}

	// 断言①:在飞期重投 → asynq 侧 ErrTaskIDConflict(记录存活期内的收敛)。
	if _, err := enqueue(t, client, queue, taskID, p); !errors.Is(err, asynq.ErrTaskIDConflict) {
		t.Errorf("in-flight redelivery: error = %v, want ErrTaskIDConflict", err)
	}
	close(release)
	waitAttemptStatus(t, d, nodeExecID, 1, engine.AttemptSucceed)

	// 断言②:attempt 落库(验收后半句)。
	row := readAttempt(t, d, nodeExecID, 1)
	if row.AsynqTaskID != taskID || row.IdempotencyKey != taskID {
		t.Errorf("task id columns = %q/%q, want %q (queue↔DB 对账连接列 + 外部幂等键)",
			row.AsynqTaskID, row.IdempotencyKey, taskID)
	}
	if row.WorkerID != "test-worker" {
		t.Errorf("worker_id = %q, want test-worker", row.WorkerID)
	}
	if !row.FinishedAt.Valid {
		t.Error("finished_at is NULL after SUCCEEDED")
	}
	if row.ErrorCode.Valid {
		t.Errorf("error_code = %q on success", row.ErrorCode.String)
	}

	// 断言③:Retention=0 → 完成即删任务记录(重投将二次入队的根因)。
	insp := asynq.NewInspector(redistest.MustOpt(t))
	t.Cleanup(func() { _ = insp.Close() })
	if _, err := insp.GetTaskInfo(queue, taskID); !errors.Is(err, asynq.ErrTaskNotFound) {
		t.Errorf("completed task record: error = %v, want ErrTaskNotFound", err)
	}

	// 断言④:删后重投 → 入队成功,worker 撞 uk_attempt 终态幂等返回。
	if _, err := enqueue(t, client, queue, taskID, p); err != nil {
		t.Fatalf("post-completion redelivery enqueue: %v", err)
	}
	waitTaskGone(t, insp, queue, taskID) // 第二发被 ack 后记录又即删 = 处理完毕的证据
	if got := ex.count(); got != 1 {
		t.Errorf("executor calls = %d after redelivery, want 1 (uk_attempt convergence)", got)
	}
	var rows int
	if err := d.QueryRow(`SELECT COUNT(*) FROM node_attempt WHERE node_execution_id=?`,
		nodeExecID).Scan(&rows); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if rows != 1 {
		t.Errorf("attempt rows = %d, want exactly 1", rows)
	}
}

// TestRetryTakeover:executor 首次返回基础设施 error(不收口,行留 RUNNING)
// → asynq 内部重试(RetryDelayFunc 固定延迟)→ 第二次接管同一 attempt 重跑
// → 收口 SUCCEEDED。恰一行,业务重试零叠加。
func TestRetryTakeover(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	execID, nodeExecID := seedNodeExec(t, d)

	ex := &fakeExec{fn: func(req Request, callNo int) (Outcome, error) {
		if callNo == 1 {
			return Outcome{}, errors.New("infra failure (db flash cut)")
		}
		return Outcome{Status: engine.AttemptSucceed}, nil
	}}
	cfg := DefaultConfig()
	cfg.RetryDelay = 50 * time.Millisecond // 测试压缩:生产默认 5s
	queue := startWorker(t, d, ex, cfg)

	client := asynq.NewClient(redistest.MustOpt(t))
	t.Cleanup(func() { _ = client.Close() })
	taskID := fmt.Sprintf("att:%d:1", nodeExecID)
	if _, err := enqueue(t, client, queue, taskID,
		nodeTaskPayload{ExecutionID: execID, NodeExecutionID: nodeExecID, AttemptNo: 1}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitAttemptStatus(t, d, nodeExecID, 1, engine.AttemptSucceed)

	if got := ex.count(); got != 2 {
		t.Errorf("executor calls = %d, want 2 (initial + asynq-internal retry takeover)", got)
	}
	var rows int
	if err := d.QueryRow(`SELECT COUNT(*) FROM node_attempt WHERE node_execution_id=?`,
		nodeExecID).Scan(&rows); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if rows != 1 {
		t.Errorf("attempt rows = %d, want exactly 1 (takeover reuses the row)", rows)
	}
}

// TestMaxRetryExhaustedArchived:基础设施失败两连(MaxRetry=1 预算耗尽)→
// 任务 archived(worker 停手),attempt 行留 RUNNING 归 T2.5 判死——T2.3 边界
// 的显式钉死:asynq 重试穷尽不是业务终态,不收口。
func TestMaxRetryExhaustedArchived(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	execID, nodeExecID := seedNodeExec(t, d)

	ex := &fakeExec{fn: func(req Request, callNo int) (Outcome, error) {
		return Outcome{}, errors.New("infra failure persists")
	}}
	cfg := DefaultConfig()
	cfg.RetryDelay = 50 * time.Millisecond
	queue := startWorker(t, d, ex, cfg)

	client := asynq.NewClient(redistest.MustOpt(t))
	t.Cleanup(func() { _ = client.Close() })
	taskID := fmt.Sprintf("att:%d:1", nodeExecID)
	if _, err := enqueue(t, client, queue, taskID,
		nodeTaskPayload{ExecutionID: execID, NodeExecutionID: nodeExecID, AttemptNo: 1}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	insp := asynq.NewInspector(redistest.MustOpt(t))
	t.Cleanup(func() { _ = insp.Close() })
	waitTaskState(t, insp, queue, taskID, asynq.TaskStateArchived)

	if got := ex.count(); got != 2 {
		t.Errorf("executor calls = %d, want 2 (MaxRetry=1 = initial + one internal retry)", got)
	}
	row := readAttempt(t, d, nodeExecID, 1)
	if row.Status != engine.AttemptRunning {
		t.Errorf("attempt status = %s, want RUNNING (T2.5 reconciler owns the DEAD verdict)", row.Status)
	}
}

// TestBusinessFailureClosesAttempt:Outcome/error 语义分立——业务失败走
// Outcome{FAILED} 收口,不烧 asynq 重试(重试权威在平台节点层)。
func TestBusinessFailureClosesAttempt(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	execID, nodeExecID := seedNodeExec(t, d)

	ex := &fakeExec{fn: func(req Request, callNo int) (Outcome, error) {
		return Outcome{Status: engine.AttemptFailed, Code: "tool_error", Msg: "boom"}, nil
	}}
	queue := startWorker(t, d, ex, DefaultConfig())

	client := asynq.NewClient(redistest.MustOpt(t))
	t.Cleanup(func() { _ = client.Close() })
	taskID := fmt.Sprintf("att:%d:1", nodeExecID)
	if _, err := enqueue(t, client, queue, taskID,
		nodeTaskPayload{ExecutionID: execID, NodeExecutionID: nodeExecID, AttemptNo: 1}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitAttemptStatus(t, d, nodeExecID, 1, engine.AttemptFailed)

	if got := ex.count(); got != 1 {
		t.Errorf("executor calls = %d, want 1 (business failure must not burn asynq retry)", got)
	}
	row := readAttempt(t, d, nodeExecID, 1)
	if !row.ErrorCode.Valid || row.ErrorCode.String != "tool_error" {
		t.Errorf("error_code = %+v, want tool_error", row.ErrorCode)
	}
}

// TestHeartbeatDuringExecution:executor 长跑期间心跳刷新 heartbeat_at
// (初值 = 派发时刻)且同事务刷 workflow_execution.last_activity_at。executor
// 自行轮询到心跳证据才返回成功——心跳未接/未刷则本测试红(负向活性对照)。
func TestHeartbeatDuringExecution(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	execID, nodeExecID := seedNodeExec(t, d)

	seedActivity := readExecActivity(t, d, execID)
	ex := &fakeExec{fn: func(req Request, callNo int) (Outcome, error) {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var hb, started time.Time
			err := d.QueryRow(`SELECT heartbeat_at, started_at FROM node_attempt WHERE id=?`,
				req.AttemptID).Scan(&hb, &started)
			if err == nil && hb.After(started) {
				return Outcome{Status: engine.AttemptSucceed}, nil
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return Outcome{}, fmt.Errorf("poll heartbeat: %w", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		return Outcome{Status: engine.AttemptFailed, Code: "no_heartbeat",
			Msg: "heartbeat_at never advanced beyond started_at within 10s"}, nil
	}}
	cfg := DefaultConfig()
	cfg.HeartbeatInterval = 40 * time.Millisecond // 测试压缩:生产默认 10s
	queue := startWorker(t, d, ex, cfg)

	client := asynq.NewClient(redistest.MustOpt(t))
	t.Cleanup(func() { _ = client.Close() })
	if _, err := enqueue(t, client, queue, fmt.Sprintf("att:%d:1", nodeExecID),
		nodeTaskPayload{ExecutionID: execID, NodeExecutionID: nodeExecID, AttemptNo: 1}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitAttemptStatus(t, d, nodeExecID, 1, engine.AttemptSucceed)

	// 同事务刷新的 execution.last_activity_at 必已越过 seed 时刻。
	if after := readExecActivity(t, d, execID); !after.After(seedActivity) {
		t.Errorf("workflow_execution.last_activity_at = %v, not advanced past seed %v",
			after, seedActivity)
	}
}

// TestBadPayloadArchived:非法 payload(数据缺陷)不触 DB、不调 executor,
// 烧完 asynq 重试预算后 archived 留死信痕迹(T6.4 可见),不静默丢弃。
func TestBadPayloadArchived(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	_, nodeExecID := seedNodeExec(t, d)

	ex := &fakeExec{fn: func(req Request, callNo int) (Outcome, error) {
		return Outcome{Status: engine.AttemptSucceed}, nil
	}}
	cfg := DefaultConfig()
	cfg.RetryDelay = 50 * time.Millisecond
	queue := startWorker(t, d, ex, cfg)

	client := asynq.NewClient(redistest.MustOpt(t))
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	taskID := fmt.Sprintf("att:%d:1", nodeExecID)
	if _, err := client.EnqueueContext(ctx,
		asynq.NewTask(EventTypeNodeTask, []byte("{not json")),
		asynq.TaskID(taskID), asynq.Queue(queue), asynq.MaxRetry(1)); err != nil {
		t.Fatalf("enqueue bad payload: %v", err)
	}

	insp := asynq.NewInspector(redistest.MustOpt(t))
	t.Cleanup(func() { _ = insp.Close() })
	waitTaskState(t, insp, queue, taskID, asynq.TaskStateArchived)

	if got := ex.count(); got != 0 {
		t.Errorf("executor calls = %d, want 0 (payload rejected before claim)", got)
	}
	var rows int
	if err := d.QueryRow(`SELECT COUNT(*) FROM node_attempt WHERE node_execution_id=?`,
		nodeExecID).Scan(&rows); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if rows != 0 {
		t.Errorf("attempt rows = %d, want 0", rows)
	}
}

// TestSuspendOutcomeSuspendNode(T4.1):executor 发 Outcome{Suspend} → handler
// 走 Sched.Suspend 收口路径:attempt SUCCEEDED、节点/执行 WAITING_APPROVAL、
// asynq 任务正常 ack(记录即删 = 无阻塞中的 worker 任务,验收主断言)。
func TestSuspendOutcomeSuspendNode(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	execID, nodeExecID := seedApprovalNodeExec(t, d)

	ex := &fakeExec{fn: func(req Request, callNo int) (Outcome, error) {
		return Outcome{Status: engine.AttemptSucceed, Suspend: true}, nil
	}}
	queue := startWorker(t, d, ex, DefaultConfig())

	client := asynq.NewClient(redistest.MustOpt(t))
	t.Cleanup(func() { _ = client.Close() })
	taskID := fmt.Sprintf("att:%d:1", nodeExecID)
	if _, err := enqueue(t, client, queue, taskID,
		nodeTaskPayload{ExecutionID: execID, NodeExecutionID: nodeExecID, AttemptNo: 1}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	waitAttemptStatus(t, d, nodeExecID, 1, engine.AttemptSucceed)
	var nodeStatus engine.NodeState
	if err := d.QueryRow(`SELECT status FROM node_execution WHERE id=?`, nodeExecID).
		Scan(&nodeStatus); err != nil {
		t.Fatalf("read node: %v", err)
	}
	if nodeStatus != engine.NodeWaitingApproval {
		t.Fatalf("node = %s, want WAITING_APPROVAL", nodeStatus)
	}
	var wfStatus engine.WorkflowState
	if err := d.QueryRow(`SELECT status FROM workflow_execution WHERE id=?`, execID).
		Scan(&wfStatus); err != nil {
		t.Fatalf("read execution: %v", err)
	}
	if wfStatus != engine.WorkflowWaitingApproval {
		t.Fatalf("execution = %s, want WAITING_APPROVAL(静默镜像)", wfStatus)
	}

	// 验收主断言:任务 ack 且记录即删 = worker 槽已释放,无阻塞任务残留。
	// Suspend 提交(DB 已见 SUCCEEDED)与 asynq 删记录间有毫秒级竞速,
	// 轮询收敛(单发检查会抖红——对抗审计 review 修;e2e 同款窗口)。
	insp := asynq.NewInspector(redistest.MustOpt(t))
	t.Cleanup(func() { _ = insp.Close() })
	goneAt := time.Now().Add(15 * time.Second)
	for time.Now().Before(goneAt) {
		_, err := insp.GetTaskInfo(queue, taskID)
		if errors.Is(err, asynq.ErrTaskNotFound) {
			break
		}
		if err != nil {
			t.Fatalf("get task info: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := insp.GetTaskInfo(queue, taskID); !errors.Is(err, asynq.ErrTaskNotFound) {
		t.Errorf("task record after suspend: error = %v, want ErrTaskNotFound (worker released)", err)
	}
	if got := ex.count(); got != 1 {
		t.Errorf("executor calls = %d, want 1", got)
	}
}

// TestSuspendOutcomeMalformedRejected:Suspend 信号畸形(非 SUCCEEDED/携带
// 产出)= 执行器装配缺陷,handler 响亮拒绝(不收口)→ asynq 重试穷尽 archived,
// attempt 留 RUNNING 归 T2.5。
func TestSuspendOutcomeMalformedRejected(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	execID, nodeExecID := seedApprovalNodeExec(t, d)

	ex := &fakeExec{fn: func(req Request, callNo int) (Outcome, error) {
		return Outcome{Status: engine.AttemptFailed, Code: "boom", Suspend: true}, nil
	}}
	cfg := DefaultConfig()
	cfg.RetryDelay = 50 * time.Millisecond
	queue := startWorker(t, d, ex, cfg)

	client := asynq.NewClient(redistest.MustOpt(t))
	t.Cleanup(func() { _ = client.Close() })
	taskID := fmt.Sprintf("att:%d:1", nodeExecID)
	if _, err := enqueue(t, client, queue, taskID,
		nodeTaskPayload{ExecutionID: execID, NodeExecutionID: nodeExecID, AttemptNo: 1}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	insp := asynq.NewInspector(redistest.MustOpt(t))
	t.Cleanup(func() { _ = insp.Close() })
	waitTaskState(t, insp, queue, taskID, asynq.TaskStateArchived)
	if got := readAttempt(t, d, nodeExecID, 1).Status; got != engine.AttemptRunning {
		t.Fatalf("attempt = %s, want RUNNING(畸形挂起不收口)", got)
	}
	var nodeStatus engine.NodeState
	if err := d.QueryRow(`SELECT status FROM node_execution WHERE id=?`, nodeExecID).
		Scan(&nodeStatus); err != nil {
		t.Fatalf("read node: %v", err)
	}
	if nodeStatus != engine.NodeExecuting {
		t.Fatalf("node = %s, want EXECUTING(挂起未发生)", nodeStatus)
	}
}

// TestSuspendOKValidation:Suspend 信号合法形校验(零 DB 表驱动)。
func TestSuspendOKValidation(t *testing.T) {
	cases := []struct {
		name string
		o    Outcome
		want bool // true = 应通过(nil)
	}{
		{"plain succeed", Outcome{Status: engine.AttemptSucceed}, true},
		{"plain failed", Outcome{Status: engine.AttemptFailed, Code: "x"}, true},
		{"valid suspend", Outcome{Status: engine.AttemptSucceed, Suspend: true}, true},
		{"suspend with failed status", Outcome{Status: engine.AttemptFailed, Suspend: true}, false},
		{"suspend with output", Outcome{Status: engine.AttemptSucceed, Suspend: true, Output: json.RawMessage(`{}`)}, false},
		{"suspend with usage", Outcome{Status: engine.AttemptSucceed, Suspend: true, Usage: &engine.AttemptUsage{Model: "m"}}, false},
		{"suspend with toolcall", Outcome{Status: engine.AttemptSucceed, Suspend: true, ToolCall: &scheduler.ToolCallInfo{Name: "t"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := suspendOK(tc.o)
			if tc.want && err != nil {
				t.Fatalf("suspendOK(%+v) = %v, want nil", tc.o, err)
			}
			if !tc.want && err == nil {
				t.Fatalf("suspendOK(%+v) = nil, want error", tc.o)
			}
		})
	}
}

// readExecActivity 读 workflow_execution.last_activity_at。
func readExecActivity(t *testing.T, d *sql.DB, execID uint64) time.Time {
	t.Helper()
	var at time.Time
	if err := d.QueryRow(`SELECT last_activity_at FROM workflow_execution WHERE id=?`,
		execID).Scan(&at); err != nil {
		t.Fatalf("read execution last_activity_at: %v", err)
	}
	return at
}

// waitTaskGone 轮询至任务记录消失(Retention=0 完成即删,= handler 已 ack)。
func waitTaskGone(t *testing.T, insp *asynq.Inspector, queue, taskID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_, err := insp.GetTaskInfo(queue, taskID)
		if errors.Is(err, asynq.ErrTaskNotFound) {
			return
		}
		if err != nil {
			t.Fatalf("get task info: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task %s record still present after 15s", taskID)
}

// waitTaskState 轮询至任务达期望态(超时 fatal 附最后观测;Inspector 瞬时
// 错误不 fatal,与 waitAttemptStatus 同款韧性)。
func waitTaskState(t *testing.T, insp *asynq.Inspector, queue, taskID string, want asynq.TaskState) {
	t.Helper()
	// 45s 上界:archived 路径的墙钟下限 ≈ asynq 调度粒度 5s/跳 ×(首跑+重试);
	// 15s 只有三跳余量,包并行(go test ./...)下 CPU 抖动即假红(T4.3 期间实测
	// 串行全绿、并行偶发超时)。上限只影响失败用例的确诊延迟,不影响绿路径。
	const budget = 45 * time.Second
	deadline := time.Now().Add(budget)
	var lastState = asynq.TaskState(0)
	var lastErr error
	for time.Now().Before(deadline) {
		info, err := insp.GetTaskInfo(queue, taskID)
		if err != nil {
			lastErr = err
			time.Sleep(20 * time.Millisecond)
			continue
		}
		lastErr, lastState = nil, info.State
		if info.State == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach state %d within %s (last state=%d, last err=%v)",
		taskID, want, budget, lastState, lastErr)
}

// TestApprovalNotifyConsumed(T4.3→T4.5):approval.notify 的消费面——
// SMTP 未配置(Notifier nil)时保留占位降级语义:合法 payload 解析校验后
// 日志 ack(零 DB/零执行器触达,通知事实记录在 outbox/audit 行);坏 JSON/
// 零 id/reason 词表外在触任何东西之前被拒,重试穷尽 archived(死信可见,
// 镜像 TestResumeEventBadPayloadArchived 形状)。Retention 让完成态可观测
// (默认完成即删,TaskState 不可断言)。
func TestApprovalNotifyConsumed(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	ex := &fakeExec{fn: func(req Request, callNo int) (Outcome, error) {
		return Outcome{Status: engine.AttemptSucceed}, nil
	}}
	cfg := DefaultConfig()
	cfg.RetryDelay = 50 * time.Millisecond
	queue := startWorker(t, d, ex, cfg)

	client := asynq.NewClient(redistest.MustOpt(t))
	t.Cleanup(func() { _ = client.Close() })
	insp := asynq.NewInspector(redistest.MustOpt(t))
	t.Cleanup(func() { _ = insp.Close() })

	t.Run("valid payload acked (smtp unconfigured)", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		payload, err := json.Marshal(scheduler.ApprovalNotifyPayload{
			ExecutionID: 1, NodeExecutionID: 2, Reason: "escalated",
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := client.EnqueueContext(ctx,
			asynq.NewTask(scheduler.EventTypeApprovalNotify, payload),
			asynq.TaskID("notify:2"), asynq.Queue(queue),
			asynq.MaxRetry(1), asynq.Retention(time.Minute)); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		waitTaskState(t, insp, queue, "notify:2", asynq.TaskStateCompleted)
		if got := ex.count(); got != 0 {
			t.Errorf("executor calls = %d, want 0(通知事件不经节点执行器)", got)
		}
	})
	for _, tc := range []struct {
		name    string
		taskID  string
		payload []byte
	}{
		{"bad json", "notify:3", []byte("{not json")},
		{"zero id fields", "notify:4", []byte(`{"execution_id":0,"node_execution_id":0}`)},
		{"reason outside vocabulary", "notify:5", []byte(`{"execution_id":1,"node_execution_id":2,"reason":"failed"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := client.EnqueueContext(ctx,
				asynq.NewTask(scheduler.EventTypeApprovalNotify, tc.payload),
				asynq.TaskID(tc.taskID), asynq.Queue(queue), asynq.MaxRetry(1)); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			waitTaskState(t, insp, queue, tc.taskID, asynq.TaskStateArchived)
			if got := ex.count(); got != 0 {
				t.Errorf("executor calls = %d, want 0", got)
			}
		})
	}
}

// TestApprovalNotifyDispatch(T4.5 换装面):Notifier 非 nil 时 handler 分派
// 发送——成功 ack;错误上抛(内部重试一次后 archived);去重哨兵
// (notify.ErrAlreadyNotified)映射 ack 不烧重试预算。悬空 id 合法:回表在
// notify.Sender 内,fake 桩不触 DB。
func TestApprovalNotifyDispatch(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	ex := &fakeExec{fn: func(req Request, callNo int) (Outcome, error) {
		return Outcome{Status: engine.AttemptSucceed}, nil
	}}
	boom := errors.New("smtp down")
	cases := []struct {
		name      string
		fn        func(call notifyCall) error
		wantState asynq.TaskState
	}{
		{"delivered", nil, asynq.TaskStateCompleted},
		{"sender error archived", func(call notifyCall) error { return boom }, asynq.TaskStateArchived},
		{"dedup sentinel acked", func(call notifyCall) error { return notify.ErrAlreadyNotified }, asynq.TaskStateCompleted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := &fakeNotifier{fn: tc.fn}
			cfg := DefaultConfig()
			cfg.RetryDelay = 50 * time.Millisecond
			queue := startWorkerOpt(t, d, ex, cfg, n)

			payload, err := json.Marshal(scheduler.ApprovalNotifyPayload{
				ExecutionID: 11, NodeExecutionID: 12, Reason: "escalated",
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			client := asynq.NewClient(redistest.MustOpt(t))
			t.Cleanup(func() { _ = client.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			taskID := fmt.Sprintf("notify:%s", tc.name)
			if _, err := client.EnqueueContext(ctx,
				asynq.NewTask(scheduler.EventTypeApprovalNotify, payload),
				asynq.TaskID(taskID), asynq.Queue(queue),
				asynq.MaxRetry(1), asynq.Retention(time.Minute)); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			insp := asynq.NewInspector(redistest.MustOpt(t))
			t.Cleanup(func() { _ = insp.Close() })
			waitTaskState(t, insp, queue, taskID, tc.wantState)
			calls := n.callsSnapshot()
			if len(calls) == 0 {
				t.Fatal("notifier not called")
			}
			last := calls[len(calls)-1]
			if last.execID != 11 || last.nodeExecID != 12 || last.reason != "escalated" {
				t.Fatalf("notifier call = %+v, want 11/12/escalated", last)
			}
			if got := ex.count(); got != 0 {
				t.Errorf("executor calls = %d, want 0", got)
			}
		})
	}
}

// TestTakeoverYieldsToDeadAttempt:接管让位(D-2026-09-22-t25-reaper-semantics
// #8 接管原子性)——reaper 判 DEAD 后同 TaskID 迟到重投,claimAttempt 见非
// RUNNING 行即放弃接管直接 ack,executor 绝不在 DEAD 行上执行。readAttempt
// 快照读到 RUNNING、reset 锁内复核才见 DEAD 的精确交错由 reaper 包并发对照
// 覆盖;本测钉终态收敛(裁决先落 ⇒ 放弃)。
func TestTakeoverYieldsToDeadAttempt(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	execID, nodeExecID := seedNodeExec(t, d)

	// 上一任 RUNNING attempt(心跳陈旧 60s = worker 已死现场)。
	attID, err := mustTestIDs(t).Next()
	if err != nil {
		t.Fatalf("gen attempt id: %v", err)
	}
	taskID := fmt.Sprintf("att:%d:1", nodeExecID)
	if _, err := d.Exec(`INSERT INTO node_attempt
	  (id, node_execution_id, execution_id, attempt_no, worker_id, status,
	   idempotency_key, asynq_task_id, started_at, heartbeat_at)
	  VALUES (?,?,?,?,?,'RUNNING',?,?,NOW(3),NOW(3) - INTERVAL 60 SECOND)`,
		attID, nodeExecID, execID, 1, "dead-worker", taskID, taskID); err != nil {
		t.Fatalf("seed stale attempt: %v", err)
	}
	// reaper 裁决形状:DEAD 收口 + Redispatch(新 attempt 号待新 claim)。
	if err := engine.WithTx(context.Background(), d, func(q engine.DBTX) error {
		if err := engine.CloseAttemptDead(context.Background(), q, attID, "heartbeat_lost", "stale 60s"); err != nil {
			return err
		}
		_, err := engine.RedispatchNode(context.Background(), q, nodeExecID, 1, engine.RedispatchInput{
			WorkerID:   "reaper:test",
			DeadlineAt: time.Now().Add(30 * time.Minute),
		})
		return err
	}); err != nil {
		t.Fatalf("reaper adjudication: %v", err)
	}

	ex := &fakeExec{fn: func(req Request, callNo int) (Outcome, error) {
		return Outcome{Status: engine.AttemptSucceed}, nil
	}}
	queue := startWorker(t, d, ex, DefaultConfig())
	client := asynq.NewClient(redistest.MustOpt(t))
	t.Cleanup(func() { _ = client.Close() })
	if _, err := enqueue(t, client, queue, taskID,
		nodeTaskPayload{ExecutionID: execID, NodeExecutionID: nodeExecID, AttemptNo: 1}); err != nil {
		t.Fatalf("enqueue late redelivery: %v", err)
	}

	insp := asynq.NewInspector(redistest.MustOpt(t))
	t.Cleanup(func() { _ = insp.Close() })
	waitTaskGone(t, insp, queue, taskID) // ack 且记录即删 = 处理完毕
	if got := ex.count(); got != 0 {
		t.Fatalf("executor calls = %d, want 0(接管必须在 DEAD 行前让位)", got)
	}
	if got := readAttempt(t, d, nodeExecID, 1).Status; got != engine.AttemptDead {
		t.Fatalf("attempt 1 = %s, want DEAD(裁决不被接管回写)", got)
	}
}

// TestResumeEventBadPayloadArchived(T4.2):execution.resume 事件的消费
// 错误面——坏 JSON/零 id 在触 Sched 之前被拒;「fabricated decision」子用例
// 的执行 id 不存在,死于 scheduler.Resume 的 loadExecution(注:决策复核层
// 的真锚在 scheduler 包 TestResumeDecisionMissingFails/AnchorMismatch,此处
// 验证 handler→Sched→error→asynq archived 的链路形状)。三者都:重试穷尽
// archived(死信可见,镜像 TestBadPayloadArchived),零节点执行器触达。
func TestResumeEventBadPayloadArchived(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	ex := &fakeExec{fn: func(req Request, callNo int) (Outcome, error) {
		return Outcome{Status: engine.AttemptSucceed}, nil
	}}
	cfg := DefaultConfig()
	cfg.RetryDelay = 50 * time.Millisecond
	queue := startWorker(t, d, ex, cfg)

	client := asynq.NewClient(redistest.MustOpt(t))
	t.Cleanup(func() { _ = client.Close() })
	insp := asynq.NewInspector(redistest.MustOpt(t))
	t.Cleanup(func() { _ = insp.Close() })

	for _, tc := range []struct {
		name    string
		taskID  string
		payload []byte
	}{
		{"bad json", "resume:1:boss:2", []byte("{not json")},
		{"zero id fields", "resume:2:boss:2", []byte(`{"execution_id":0,"node_execution_id":0,"decision_id":0}`)},
		{"fabricated decision", "resume:3:boss:2",
			[]byte(`{"execution_id":1,"node_execution_id":2,"decision_id":9527,"attempt_no":2}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := client.EnqueueContext(ctx,
				asynq.NewTask(scheduler.EventTypeExecutionResume, tc.payload),
				asynq.TaskID(tc.taskID), asynq.Queue(queue), asynq.MaxRetry(1)); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			waitTaskState(t, insp, queue, tc.taskID, asynq.TaskStateArchived)
			if got := ex.count(); got != 0 {
				t.Errorf("executor calls = %d, want 0(resume 事件不经节点执行器)", got)
			}
		})
	}
}
