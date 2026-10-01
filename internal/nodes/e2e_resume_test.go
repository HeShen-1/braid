package nodes

// T4.2 端到端验收:审批决定 → 经 outbox 续跑 → 执行终态,全链 =
// api 决定事务(gin 真路由)→ outbox execution.resume → 真 relay → 真
// asynq → worker handleNodeResume → scheduler.Resume → 下游推进。
// 验收断言面(issue #33):
//   - 批准:执行在扫描间隔×3 量级内达 SUCCEEDED,下游节点真实推进
//     (boss.output.decision 引用穿透),approval.decided 审计行在链;
//   - 驳回:执行 FAILED(approval_rejected),fail-fast;
//   - enqueue 失败注入:relay 入队持续失败(outbox RecordFailure 退避)
//     → 恢复后同一事件行重投成功(对账恢复 = outbox PENDING 重试语义,
//     TaskID 同 ID ⇒ ErrTaskIDConflict 幂等收敛面不触发),终态时延
//     ≤ 3×(IdleInterval+BackoffMax) + 3s 执行链组件预算(「扫描间隔×3
//     内达终态」的可测化口径:relay 轮询与重试退避是 SLA 公式项,3s 常数
//     界住 worker 派发/收口与下游 attempt 的队列往返——实测余量主要在此)。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"

	"braid/internal/api"
	"braid/internal/audit"
	"braid/internal/auth"
	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/redistest"
	"braid/internal/relay"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
	"braid/internal/tools"
	"braid/internal/worker"
)

// nodesE2ESecret 是审批 e2e 的 JWT 密钥(测试面;≥ auth.MinSecretLen)。
const nodesE2ESecret = "nodes-e2e-approval-secret-0123456789"

// flakyEnqueuer 在 release 前对所有入队返回基础设施错误(relay 记失败退避、
// 行留 PENDING);release 后透传真 Enqueuer。验收①「enqueue 失败注入」的
// 注入点——对账恢复 = outbox RecordFailure + available_at 退避重扫。
type flakyEnqueuer struct {
	inner  relay.Enqueuer
	broken atomic.Bool
}

func (f *flakyEnqueuer) EnqueueContext(ctx context.Context, task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	if f.broken.Load() {
		return nil, errors.New("flaky: enqueue down (injected)")
	}
	return f.inner.EnqueueContext(ctx, task, opts...)
}

// seedApprovalChain 种 a(llm)→boss(approval)→c(llm) 链并物化执行。
func seedApprovalChain(t *testing.T, d *sql.DB, sched *scheduler.Scheduler) (defID, execID uint64) {
	return seedApprovalChainMode(t, d, sched, "any", "carol")
}

// seedApprovalChainMode 是 seedApprovalChain 的 mode/名单参数化变体
// (T14.2 会签 e2e:approval 快照由 DSL 物化,mode=all + 多人名单)。
func seedApprovalChainMode(t *testing.T, d *sql.DB, sched *scheduler.Scheduler, mode string, approvers ...string) (defID, execID uint64) {
	t.Helper()
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("seed snowflake: %v", err)
	}
	defID, err = ids.Next()
	if err != nil {
		t.Fatalf("gen def id: %v", err)
	}
	name := fmt.Sprintf("e2e-resume-%d", defID)
	approverList, err := json.Marshal(approvers)
	if err != nil {
		t.Fatalf("marshal approvers: %v", err)
	}
	dslDoc := fmt.Sprintf(`{"dsl":"1.0","name":%q,
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"gen"},"outputs":{"text":"string"}},
  {"id":"boss","type":"approval","approvers":%s,"mode":%q,"timeout_hours":24,
   "on_timeout":"fail","instructions":"请审批","outputs":{"decision":"string"}},
  {"id":"c","type":"llm",
   "prompt":{"template":"定稿","user_data":[{"name":"v","ref":"{{nodes.boss.output.decision}}"}]},
   "outputs":{"text":"string"}}],
 "edges":[{"from":"a","to":"boss"},{"from":"boss","to":"c"}],
 "outputs":{"final":"{{nodes.c.output.text}}"}}`, name, approverList, mode)
	if _, err := d.Exec(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', ?, ?, 'e2e', 'e2e', 'e2e', NOW(3))`,
		defID, name, dslDoc, string(make([]byte, 64))); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	execID, err = sched.Instantiate(context.Background(), scheduler.InstantiateParams{
		DefinitionID: defID, Input: json.RawMessage(`{}`), TriggeredBy: "e2e", TriggerType: "TEST",
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	return defID, execID
}

// newApprovalAPIServer 在 nodes e2e 内装配真 api 路由(决定 POST 的真
// 对手方;ResumeQueue 指向本测试队列,事件与 worker 同队列)。
func newApprovalAPIServer(t *testing.T, d *sql.DB, queue string) *gin.Engine {
	t.Helper()
	ids, err := snowflake.New(3)
	if err != nil {
		t.Fatalf("api snowflake: %v", err)
	}
	reg, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	sched, err := scheduler.New(scheduler.Deps{DB: d, IDs: ids, Queue: queue})
	if err != nil {
		t.Fatalf("api scheduler: %v", err)
	}
	gin.SetMode(gin.TestMode)
	return api.NewServer(api.Config{
		Addr: "127.0.0.1:0", JWTSecret: nodesE2ESecret, SnowflakeNode: snowflake.Explicit(3), ResumeQueue: queue,
	}, d, ids, reg, sched, auth.BuiltinProvider{DB: d}).Router()
}

// postDecision 经真 gin 路由发决定(carol 主体)。
func postDecision(t *testing.T, r *gin.Engine, execID uint64, decision, comment string) (int, map[string]any) {
	return postDecisionBy(t, r, execID, "carol", decision, comment)
}

// postDecisionBy 是 postDecision 的主体参数化变体(T14.2 会签多投票人)。
func postDecisionBy(t *testing.T, r *gin.Engine, execID uint64, subject, decision, comment string) (int, map[string]any) {
	t.Helper()
	token, err := auth.SignToken(nodesE2ESecret, time.Hour, subject, []auth.Role{auth.RoleApprover}, 0)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/executions/%d/approvals/boss/decision", execID),
		strings.NewReader(fmt.Sprintf(`{"decision":%q,"comment":%q}`, decision, comment)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("non-JSON body (code %d): %q", w.Code, w.Body.String())
		}
	}
	return w.Code, out
}

// TestE2EApprovalApproveResumesFlow:批准全链 → 下游推进 → SUCCEEDED;
// approval.decided 审计行与 boss.output 形状在真路径核。
func TestE2EApprovalApproveResumesFlow(t *testing.T) {
	d := sqltest.MustOpenAt(t, nodesSchema)
	ctx := context.Background()
	queue := fmt.Sprintf("t_n%d", atomic.AddUint32(&queueSeq, 1))

	executor := NewMux(mustE2ESched(t, d, queue),
		&fakeRunner{out: worker.Outcome{Status: engine.AttemptSucceed, Output: json.RawMessage(`{"text":"ok"}`)}},
		&fakeToolExec{}, ApprovalExecutor{})
	sched := mustE2ESched(t, d, queue)
	defID, execID := seedApprovalChain(t, d, sched)
	t.Cleanup(func() { cleanupNodesE2E(t, d, execID, defID) })

	startE2ERelay(t, d, ctx)
	workerCfg := worker.DefaultConfig()
	workerCfg.HeartbeatInterval = 40 * time.Millisecond
	startE2EWorker(t, d, executor, workerCfg, queue)
	router := newApprovalAPIServer(t, d, queue)

	waitE2EExecWaitingApproval(t, d, execID)

	code, body := postDecision(t, router, execID, "approve", "可以")
	if code != http.StatusCreated {
		t.Fatalf("decide = %d %v, want 201", code, body)
	}
	waitE2EExecSucceeded(t, d, execID)
	assertE2ENodeState(t, d, execID, "boss", engine.NodeSucceeded)
	assertE2ENodeState(t, d, execID, "c", engine.NodeSucceeded)

	// boss.output 形状(真路径产出):decision/decided_by/comment 三键。
	var rawOut []byte
	if err := d.QueryRow(`SELECT output FROM node_execution WHERE execution_id=? AND node_id=?`,
		execID, "boss").Scan(&rawOut); err != nil {
		t.Fatalf("read boss output: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(rawOut, &out); err != nil {
		t.Fatalf("boss output json: %v", err)
	}
	if out["decision"] != "approved" || out["decided_by"] != "carol" || out["comment"] != "可以" {
		t.Fatalf("boss output = %v, want approval face", out)
	}
	// 审计行:approval.decided(actor=carol)在真链。
	var n int
	flushStagedAudit(t, d) // T6.1 异步暂存排空
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND event_type=? AND actor_id='carol'`,
		execID, audit.EventApprovalDecided).Scan(&n); err != nil {
		t.Fatalf("count approval.decided: %v", err)
	}
	if n != 1 {
		t.Fatalf("approval.decided rows = %d, want 1", n)
	}
	// 续跑事件已投递(对账面:DISPATCHED)。
	var resumeStatus string
	if err := d.QueryRow(`SELECT status FROM outbox WHERE execution_id=? AND event_type=?`,
		execID, scheduler.EventTypeExecutionResume).Scan(&resumeStatus); err != nil {
		t.Fatalf("read resume event: %v", err)
	}
	if resumeStatus != "DISPATCHED" {
		t.Fatalf("resume event status = %s, want DISPATCHED", resumeStatus)
	}
}

// TestE2EApprovalRejectFailsFlow:驳回全链 → boss FAILED(approval_rejected)
// → 执行 fail-fast FAILED,c 不推进。
func TestE2EApprovalRejectFailsFlow(t *testing.T) {
	d := sqltest.MustOpenAt(t, nodesSchema)
	ctx := context.Background()
	queue := fmt.Sprintf("t_n%d", atomic.AddUint32(&queueSeq, 1))

	executor := NewMux(mustE2ESched(t, d, queue),
		&fakeRunner{out: worker.Outcome{Status: engine.AttemptSucceed, Output: json.RawMessage(`{"text":"ok"}`)}},
		&fakeToolExec{}, ApprovalExecutor{})
	sched := mustE2ESched(t, d, queue)
	defID, execID := seedApprovalChain(t, d, sched)
	t.Cleanup(func() { cleanupNodesE2E(t, d, execID, defID) })

	startE2ERelay(t, d, ctx)
	workerCfg := worker.DefaultConfig()
	workerCfg.HeartbeatInterval = 40 * time.Millisecond
	startE2EWorker(t, d, executor, workerCfg, queue)
	router := newApprovalAPIServer(t, d, queue)

	waitE2EExecWaitingApproval(t, d, execID)

	code, body := postDecision(t, router, execID, "reject", "不行")
	if code != http.StatusCreated {
		t.Fatalf("decide = %d %v, want 201", code, body)
	}
	waitE2EExecFailed(t, d, execID)
	assertE2ENodeState(t, d, execID, "boss", engine.NodeFailed)
	var codeCol sql.NullString
	if err := d.QueryRow(`SELECT error_code FROM node_execution WHERE execution_id=? AND node_id=?`,
		execID, "boss").Scan(&codeCol); err != nil {
		t.Fatalf("read boss error_code: %v", err)
	}
	if !codeCol.Valid || codeCol.String != "approval_rejected" {
		t.Fatalf("boss error_code = %v, want approval_rejected", codeCol)
	}
	assertE2ENodeState(t, d, execID, "c", engine.NodePending)
}

// TestE2EApprovalResumeAfterEnqueueFailure:验收①——enqueue 失败注入下经
// 对账恢复(outbox PENDING 重试),恢复后 3×(轮询+退避上界)内达终态。
func TestE2EApprovalResumeAfterEnqueueFailure(t *testing.T) {
	d := sqltest.MustOpenAt(t, nodesSchema)
	ctx := context.Background()
	queue := fmt.Sprintf("t_n%d", atomic.AddUint32(&queueSeq, 1))

	executor := NewMux(mustE2ESched(t, d, queue),
		&fakeRunner{out: worker.Outcome{Status: engine.AttemptSucceed, Output: json.RawMessage(`{"text":"ok"}`)}},
		&fakeToolExec{}, ApprovalExecutor{})
	sched := mustE2ESched(t, d, queue)
	defID, execID := seedApprovalChain(t, d, sched)
	t.Cleanup(func() { cleanupNodesE2E(t, d, execID, defID) })

	// relay 带 flaky 缝:先健康跑挂起链(与生产同款 relay 配置收紧),后注入。
	flaky := &flakyEnqueuer{inner: relay.NewAsynqEnqueuer(redistest.MustOpt(t))}
	relayCfg := relay.DefaultConfig()
	relayCfg.IdleInterval = 30 * time.Millisecond
	relayCfg.EnqueueTimeout = 5 * time.Second
	relayCfg.BackoffBase = 50 * time.Millisecond
	relayCfg.BackoffFactor = 2
	relayCfg.BackoffMax = 200 * time.Millisecond
	relayCfg.MaxAttempts = 6
	rctx, relayStop := context.WithCancel(ctx)
	relayDone := make(chan error, 1)
	go func() {
		relayDone <- relay.Run(rctx, relay.Deps{DB: d, Enqueuer: flaky}, relayCfg)
	}()
	t.Cleanup(func() {
		relayStop()
		select {
		case err := <-relayDone:
			if err != nil {
				t.Errorf("relay.Run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("relay.Run did not stop within 15s after cancel")
		}
	})

	workerCfg := worker.DefaultConfig()
	workerCfg.HeartbeatInterval = 40 * time.Millisecond
	startE2EWorker(t, d, executor, workerCfg, queue)
	router := newApprovalAPIServer(t, d, queue)

	waitE2EExecWaitingApproval(t, d, execID)

	// 注入:此后一切入队失败(续跑事件是唯一在飞事件)。
	flaky.broken.Store(true)
	code, body := postDecision(t, router, execID, "approve", "可以")
	if code != http.StatusCreated {
		t.Fatalf("decide = %d %v, want 201(决定事务零 Redis 依赖)", code, body)
	}

	// 注入窗:续跑事件必被记失败(attempts ≥1、行留 PENDING)。
	failed := time.Now().Add(10 * time.Second)
	var attempts int
	for time.Now().Before(failed) {
		if err := d.QueryRow(`SELECT deliver_attempts FROM outbox WHERE execution_id=? AND event_type=?`,
			execID, scheduler.EventTypeExecutionResume).Scan(&attempts); err == nil && attempts >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if attempts < 1 {
		t.Fatalf("resume event deliver_attempts = %d, want ≥1(失败注入被记录)", attempts)
	}

	// 恢复:同事件行重投(同 TaskID,at-least-once),对账收敛到终态。
	release := time.Now()
	flaky.broken.Store(false)
	sla := 3 * (relayCfg.IdleInterval + relayCfg.BackoffMax)
	// + 执行链组件预算:worker 派发/收口与下游 attempt 的队列往返。
	sla += 3 * time.Second
	for time.Since(release) < sla {
		var status engine.WorkflowState
		if err := d.QueryRow(`SELECT status FROM workflow_execution WHERE id=?`, execID).
			Scan(&status); err == nil && status == engine.WorkflowSucceeded {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("execution not SUCCEEDED within %v of enqueue recovery(对账恢复 SLA)", sla)
}

// TestE2EApprovalAllModeQuorumApproves(T14.2 会签全链):部分批准 = 真中间
// 态——首票后零 resume 事件、执行保持 WAITING_APPROVAL;票齐(末票)才产
// 生续跑意图 → 下游推进 → SUCCEEDED;boss.output 锚末票行(decided_by=dave)。
func TestE2EApprovalAllModeQuorumApproves(t *testing.T) {
	d := sqltest.MustOpenAt(t, nodesSchema)
	ctx := context.Background()
	queue := fmt.Sprintf("t_n%d", atomic.AddUint32(&queueSeq, 1))

	executor := NewMux(mustE2ESched(t, d, queue),
		&fakeRunner{out: worker.Outcome{Status: engine.AttemptSucceed, Output: json.RawMessage(`{"text":"ok"}`)}},
		&fakeToolExec{}, ApprovalExecutor{})
	sched := mustE2ESched(t, d, queue)
	defID, execID := seedApprovalChainMode(t, d, sched, "all", "carol", "dave")
	t.Cleanup(func() { cleanupNodesE2E(t, d, execID, defID) })

	startE2ERelay(t, d, ctx)
	workerCfg := worker.DefaultConfig()
	workerCfg.HeartbeatInterval = 40 * time.Millisecond
	startE2EWorker(t, d, executor, workerCfg, queue)
	router := newApprovalAPIServer(t, d, queue)

	waitE2EExecWaitingApproval(t, d, execID)

	// 首票:中间态(201,无 final 字段),零续跑意图。
	code, body := postDecisionBy(t, router, execID, "carol", "approve", "第一票")
	if code != http.StatusCreated {
		t.Fatalf("partial decide = %d %v, want 201", code, body)
	}
	if _, has := body["final"]; has {
		t.Fatalf("partial vote must omit final, got %v", body["final"])
	}
	var resumeEvents int
	if err := d.QueryRow(`SELECT COUNT(*) FROM outbox WHERE execution_id=? AND event_type=?`,
		execID, scheduler.EventTypeExecutionResume).Scan(&resumeEvents); err != nil {
		t.Fatalf("count resume events: %v", err)
	}
	if resumeEvents != 0 {
		t.Fatalf("resume events after partial vote = %d, want 0", resumeEvents)
	}
	assertE2ENodeState(t, d, execID, "boss", engine.NodeWaitingApproval)

	// 末票:定局(final=true)→ 全链续跑。
	code, body = postDecisionBy(t, router, execID, "dave", "approve", "第二票")
	if code != http.StatusCreated || body["final"] != true {
		t.Fatalf("final decide = %d %v, want 201 final=true", code, body)
	}
	waitE2EExecSucceeded(t, d, execID)
	assertE2ENodeState(t, d, execID, "boss", engine.NodeSucceeded)
	assertE2ENodeState(t, d, execID, "c", engine.NodeSucceeded)

	var rawOut []byte
	if err := d.QueryRow(`SELECT output FROM node_execution WHERE execution_id=? AND node_id=?`,
		execID, "boss").Scan(&rawOut); err != nil {
		t.Fatalf("read boss output: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(rawOut, &out); err != nil {
		t.Fatalf("boss output json: %v", err)
	}
	if out["decision"] != "approved" || out["decided_by"] != "dave" {
		t.Fatalf("boss output = %v, want approved by dave(事件锚定末票行)", out)
	}
	// 审计:每票一行(部分批准票同样是决定事实)。
	flushStagedAudit(t, d)
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND event_type=?`,
		execID, audit.EventApprovalDecided).Scan(&n); err != nil {
		t.Fatalf("count approval.decided: %v", err)
	}
	if n != 2 {
		t.Fatalf("approval.decided rows = %d, want 2(carol+dave)", n)
	}
}

// TestE2EApprovalAllModeRejectFails(T14.2 会签驳回全链):部分批准后任一
// 驳回即定局 → resume 事件 → boss FAILED(approval_rejected)→ 执行
// fail-fast,c 不推进。
func TestE2EApprovalAllModeRejectFails(t *testing.T) {
	d := sqltest.MustOpenAt(t, nodesSchema)
	ctx := context.Background()
	queue := fmt.Sprintf("t_n%d", atomic.AddUint32(&queueSeq, 1))

	executor := NewMux(mustE2ESched(t, d, queue),
		&fakeRunner{out: worker.Outcome{Status: engine.AttemptSucceed, Output: json.RawMessage(`{"text":"ok"}`)}},
		&fakeToolExec{}, ApprovalExecutor{})
	sched := mustE2ESched(t, d, queue)
	defID, execID := seedApprovalChainMode(t, d, sched, "all", "carol", "dave")
	t.Cleanup(func() { cleanupNodesE2E(t, d, execID, defID) })

	startE2ERelay(t, d, ctx)
	workerCfg := worker.DefaultConfig()
	workerCfg.HeartbeatInterval = 40 * time.Millisecond
	startE2EWorker(t, d, executor, workerCfg, queue)
	router := newApprovalAPIServer(t, d, queue)

	waitE2EExecWaitingApproval(t, d, execID)

	if code, body := postDecisionBy(t, router, execID, "carol", "approve", "先批"); code != http.StatusCreated {
		t.Fatalf("partial decide = %d %v, want 201", code, body)
	}
	code, body := postDecisionBy(t, router, execID, "dave", "reject", "否")
	if code != http.StatusCreated || body["final"] != true {
		t.Fatalf("reject = %d %v, want 201 final=true(任一驳回即定局)", code, body)
	}
	waitE2EExecFailed(t, d, execID)
	assertE2ENodeState(t, d, execID, "boss", engine.NodeFailed)
	assertE2ENodeState(t, d, execID, "c", engine.NodePending)
}
