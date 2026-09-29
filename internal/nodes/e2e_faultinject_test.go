package nodes

// T8.1 故障注入套件:worker 中途 kill。链路 = API 触发(root trace X 落
// workflow_execution.trace_id)→ relay → 子进程 worker 消费 execution.start
// 派发 att:1(payload 携 X)→ claim 后 faultGateExecutor 停车(READY 哨兵)
// → 父进程 SIGKILL(真·未收口现场,心跳骤停)→ reaper(grace/scan 压缩)
// DEAD(heartbeat_lost)+ Redispatch(att:2 拷原行 X,reaper 同式拷 trace)
// → relay → 常驻 worker(Mux/fakeRunner)接手 → 无人工干预 SUCCEEDED。
//
// SLA 口径:「扫描间隔×3 内达终态」的可测化 = DeadGrace + 3×ScanInterval
// 量级(压缩配置 grace=1s/scan=200ms ⇒ ~1.6s;T2.5 acceptance 同款),
// 断言总预算 15s(e2e 慢环境余量)。trace 连续性对账三面(T6.2 增补):
//   - root 面:execution.trace_id 非空 = X(API 应用层强制);
//   - 重放面:att:2 重派行 payload.trace_id 与 trace 列双 == X(原事件
//     同一真相,重放不换段);
//   - 全链面:flush 后非空 trace 恒 = X、linked_trace_id 恒 NULL(reaper
//     自生行 trace NULL 属 D-t62 如实化口径,不计为换段)。

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"

	"braid/internal/api"
	"braid/internal/audit"
	"braid/internal/auth"
	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/reaper"
	"braid/internal/redistest"
	"braid/internal/relay"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
	"braid/internal/tools"
	"braid/internal/tracing"
	"braid/internal/worker"
)

// faultChildEnv / faultChildQueueEnv:子进程分支标记与队列注入口。
const (
	faultChildEnv      = "BRAID_NODES_FAULT_CHILD"
	faultChildQueueEnv = "BRAID_NODES_FAULT_QUEUE"
)

// faultChildMain:子进程 = 真 asynq worker,executor 在 claim 落库后打
// READY 哨兵并阻塞(等被 SIGKILL)。tracer = off exporter(trace id 照生成:
// att:1 原行必须携 X,否则 reaper 重派拷不到段链)。
func faultChildMain() {
	d, err := sqltest.OpenAt(nodesSchema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: mysql: %v\n", err)
		os.Exit(3)
	}
	defer d.Close() //nolint:errcheck // kill 路径到不了
	ids, err := snowflake.New(uint16(os.Getpid() & 0x3ff))
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: snowflake: %v\n", err)
		os.Exit(4)
	}
	queue := os.Getenv(faultChildQueueEnv)
	sched, err := scheduler.New(scheduler.Deps{DB: d, IDs: ids, Queue: queue})
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: scheduler: %v\n", err)
		os.Exit(5)
	}
	prov, err := tracing.NewProvider(tracing.Config{
		Exporter: tracing.ExporterOff, ServiceName: "braid-worker-fault-child",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: trace: %v\n", err)
		os.Exit(7)
	}
	cfg := worker.DefaultConfig()
	cfg.HeartbeatInterval = 200 * time.Millisecond // 压缩心跳:grace=1s 可测
	cfg.WorkerID = "fault-child-worker"
	cfg.Queues = map[string]int{queue: 1}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { // stdin EOF(父进程先死)→ 收摊退出,不留孤儿
		_, _ = io.Copy(io.Discard, os.Stdin)
		cancel()
	}()
	if err := worker.Run(ctx, worker.Deps{
		DB: d, IDs: ids, Executor: &faultGateExecutor{stdin: os.Stdin}, Sched: sched,
		Tracer: prov.Tracer("braid/worker"),
	}, cfg, redistest.Opt()); err != nil {
		fmt.Fprintf(os.Stderr, "child: worker: %v\n", err)
		os.Exit(6)
	}
}

// faultGateExecutor:claim 已落库(Request 到手 = attempt 行已 INSERT)→
// READY → 阻塞读 stdin 至 SIGKILL(EOF = 父进程先死,兜底返回)。
type faultGateExecutor struct{ stdin *os.File }

func (g *faultGateExecutor) Execute(ctx context.Context, req worker.Request) (worker.Outcome, error) {
	fmt.Printf("READY %d\n", req.NodeExecutionID) // os.Stdout 无缓冲,直写管道
	buf := make([]byte, 1)
	_, _ = g.stdin.Read(buf)
	return worker.Outcome{Status: engine.AttemptSucceed}, nil
}

// TestFaultWorkerKillMidRunConvergesTraceIntact:worker 中途 kill → 无人工
// 干预收敛 + trace 连续性三面断言。
func TestFaultWorkerKillMidRunConvergesTraceIntact(t *testing.T) {
	d := sqltest.MustOpenAt(t, nodesSchema)
	queue := fmt.Sprintf("t_n%d", atomic.AddUint32(&queueSeq, 1))

	// 定义直插 PUBLISHED(单 llm 节点;触发面走 API 取 root trace——发布
	// 语义在 doc e2e 靶区,不在本臂)。
	defIDs, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("seed snowflake: %v", err)
	}
	defID, err := defIDs.Next()
	if err != nil {
		t.Fatalf("gen def id: %v", err)
	}
	name := fmt.Sprintf("fault-kill-%d", defID)
	dsl := fmt.Sprintf(`{"dsl":"1.0","name":%q,
 "nodes":[{"id":"sum","type":"llm","prompt":{"template":"t"},"outputs":{"text":"string"},
   "retry":{"max_attempts":2}}],
 "edges":[],
 "outputs":{"final":"{{nodes.sum.output.text}}"}}`, name)
	if _, err := d.Exec(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', ?, ?, 'seed', 'seed', 'seed', NOW(3))`,
		defID, name, dsl, string(make([]byte, 64))); err != nil {
		t.Fatalf("seed definition: %v", err)
	}

	// API 面(root trace:off exporter = trace id 照生成零导出)。
	apiIDs, err := snowflake.New(3)
	if err != nil {
		t.Fatalf("api snowflake: %v", err)
	}
	apiSched, err := scheduler.New(scheduler.Deps{DB: d, IDs: apiIDs, Queue: queue})
	if err != nil {
		t.Fatalf("api scheduler: %v", err)
	}
	reg, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	traceProv, err := tracing.NewProvider(tracing.Config{
		Exporter: tracing.ExporterOff, ServiceName: "braid-api-fault",
	})
	if err != nil {
		t.Fatalf("trace provider: %v", err)
	}
	t.Cleanup(func() { _ = traceProv.Shutdown(context.Background()) })
	gin.SetMode(gin.TestMode)
	apiSrv := api.NewServer(api.Config{
		Addr: "127.0.0.1:0", JWTSecret: nodesE2ESecret, SnowflakeNode: 3, ResumeQueue: queue,
		// T6.4 登录防御旋钮放宽(doc e2e 同款;防御行为由 api 包用例锚定)。
		LoginRateLimit: 100000, LoginRateWindow: time.Minute,
		LoginMaxFailures: 100000, LoginLockout: time.Minute,
	}, d, apiIDs, reg, apiSched, auth.BuiltinProvider{DB: d})
	apiSrv.Tracing(traceProv.Tracer("braid/api"))
	router := apiSrv.Router()
	ensureE2EUser(t, d, "t81_author", auth.RoleAuthor)

	startE2ERelay(t, d, context.Background())

	// ---- 阶段一:子进程 worker(唯一消费者)先行,API 触发,claim 后 kill。----
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), faultChildEnv+"=1", faultChildQueueEnv+"="+queue)
	stdin, err := cmd.StdinPipe() // 停车点读它;不预关——EOF 会解除 gate 阻塞污染 kill 现场
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	_ = stdin
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	childDone := make(chan struct{})
	defer func() {
		_ = cmd.Process.Kill()
		<-childDone
	}()
	go func() {
		_, _ = cmd.Process.Wait()
		close(childDone)
	}()

	tok := docLogin(t, router, "t81_author")
	body := fmt.Sprintf(`{"definition_name":%q,"input":{}}`, name)
	code, out := doDocJSON(t, router, http.MethodPost, "/api/v1/executions", tok, []byte(body))
	if code != http.StatusCreated {
		t.Fatalf("trigger = %d %v, want 201", code, out)
	}
	idStr, _ := out["id"].(string)
	execID, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		t.Fatalf("execution id %q: %v", idStr, err)
	}
	t.Cleanup(func() { cleanupNodesE2E(t, d, execID, defID) })

	// READY 哨兵(缺栈/坏栈 = 缺陷不是 flake,响亮失败)。
	readyLine := make(chan string, 1)
	go func() {
		line, rerr := bufio.NewReader(stdout).ReadString('\n')
		if rerr != nil {
			readyLine <- fmt.Sprintf("ERR %v (stderr: %s)", rerr, stderr.String())
			return
		}
		readyLine <- strings.TrimSpace(line)
	}()
	var nodeExecID uint64
	select {
	case line := <-readyLine:
		if _, err := fmt.Sscanf(line, "READY %d", &nodeExecID); err != nil {
			t.Fatalf("child boot: %s", line)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("child never claimed (stderr: %s)", stderr.String())
	}
	// attempt 1 落库 + 心跳已在跑(claim 即写初始 heartbeat_at,kill 后
	// 骤停;心跳龄从 kill 时刻起算)。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := d.QueryRow(
			`SELECT COUNT(*) FROM node_attempt WHERE node_execution_id=? AND status='RUNNING'`,
			nodeExecID).Scan(&n); err == nil && n == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil { // SIGKILL:真·未收口现场
		t.Fatalf("kill: %v", err)
	}
	<-childDone

	// ---- 阶段二:常驻 worker 接管(MustOpt 顺带清掉 kill 残留的 Redis
	// 在飞 att:1 副本,asynq 僵尸重投面由 worker 包 T2.3 验收锚定)。----
	executor := NewMux(mustE2ESched(t, d, queue),
		&fakeRunner{out: worker.Outcome{Status: engine.AttemptSucceed, Output: json.RawMessage(`{"text":"ok"}`)}},
		&fakeToolExec{}, ApprovalExecutor{})
	wcfg := worker.DefaultConfig()
	wcfg.HeartbeatInterval = 40 * time.Millisecond
	startE2EWorker(t, d, executor, wcfg, queue)

	// ---- 阶段三:reaper(压缩配置;RunOnce 循环)。----
	reapIDs, err := snowflake.New(5)
	if err != nil {
		t.Fatalf("reaper snowflake: %v", err)
	}
	r, err := reaper.New(reaper.Deps{
		DB: d, Sched: mustE2ESched(t, d, queue), IDs: reapIDs,
		// D-t121 #4:e2e 侧挂真 tracer——「span 恒不进发射 ctx」的解耦由
		// 本测试全链面断言钉死(spans 在场时非空 trace 仍恒 = X;若解耦
		// 破坏,reaper 自生行携 tick trace ⇒ 断言红)。
		Tracer: e2eReaperTracer(t),
	}, reaper.Config{
		ScanInterval:   200 * time.Millisecond,
		DeadGrace:      time.Second,
		ExecStale:      10 * time.Second, // PENDING 窗口留足(trigger→消费 为毫秒级)
		PendingTTL:     time.Minute,
		DispatchStale:  time.Minute,
		AuditRetention: 24 * time.Hour,
		OutboxBatch:    100,
		Queue:          queue,
		ID:             "fault-reaper",
	})
	if err != nil {
		t.Fatalf("reaper: %v", err)
	}
	rctx, rcancel := context.WithCancel(context.Background())
	t.Cleanup(rcancel)
	go func() {
		for {
			select {
			case <-rctx.Done():
				return
			default:
			}
			if err := r.RunOnce(rctx); err != nil {
				t.Logf("fault reaper RunOnce: %v", err)
			}
			select {
			case <-rctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()

	// ---- 断言①:SLA 内收敛 SUCCEEDED + 双 attempt 形态。----
	if !faultWaitFor(t, 15*time.Second, func() bool {
		var status engine.WorkflowState
		return d.QueryRow(`SELECT status FROM workflow_execution WHERE id=?`, execID).
			Scan(&status) == nil && status == engine.WorkflowSucceeded
	}) {
		t.Fatalf("execution 未收敛(kill 后 15s);child stderr: %s", stderr.String())
	}
	var a1, a2 string
	if err := d.QueryRow(
		`SELECT status FROM node_attempt WHERE node_execution_id=? AND attempt_no=1`,
		nodeExecID).Scan(&a1); err != nil {
		t.Fatalf("read attempt1: %v", err)
	}
	if err := d.QueryRow(
		`SELECT status FROM node_attempt WHERE node_execution_id=? AND attempt_no=2`,
		nodeExecID).Scan(&a2); err != nil {
		t.Fatalf("read attempt2: %v(应存在重派新 attempt)", err)
	}
	if a1 != string(engine.AttemptDead) || a2 != string(engine.AttemptSucceed) {
		t.Fatalf("attempts = %s/%s, want DEAD/SUCCEEDED", a1, a2)
	}

	// ---- 断言②:root 面——trace_id 非空(应用层强制兑现)。----
	var rootTrace sql.NullString
	if err := d.QueryRow(`SELECT trace_id FROM workflow_execution WHERE id=?`, execID).
		Scan(&rootTrace); err != nil {
		t.Fatalf("read exec trace: %v", err)
	}
	if !rootTrace.Valid || rootTrace.String == "" {
		t.Fatalf("workflow_execution.trace_id 空:API root trace 未兑现")
	}
	x := rootTrace.String

	// ---- 断言③:重放面——att:2 重派行 payload/列双 == X(原事件同一真相)。----
	task2 := fmt.Sprintf("att:%d:2", nodeExecID)
	var colTrace sql.NullString
	var payload []byte
	if err := d.QueryRow(`SELECT trace_id, payload FROM outbox WHERE task_id=?`, task2).
		Scan(&colTrace, &payload); err != nil {
		t.Fatalf("read redispatch row: %v", err)
	}
	var np scheduler.NodeTaskPayload
	if err := json.Unmarshal(payload, &np); err != nil {
		t.Fatalf("redispatch payload: %v", err)
	}
	if np.TraceID != x || colTrace.String != x {
		t.Fatalf("重派行 trace = payload %q / 列 %q, want 双 == %q(重放不换段)",
			np.TraceID, colTrace.String, x)
	}

	// ---- 断言④:全链面——flush 后非空 trace 恒 = X、linked 恒 NULL。----
	flushStagedAudit(t, d)
	var offRoot, linked int
	if err := d.QueryRow(`
SELECT SUM(trace_id IS NOT NULL AND trace_id<>?), SUM(linked_trace_id IS NOT NULL)
  FROM audit_log WHERE execution_id=?`, x, execID).Scan(&offRoot, &linked); err != nil {
		t.Fatalf("trace consistency scan: %v", err)
	}
	if offRoot != 0 || linked != 0 {
		t.Fatalf("trace 换段:off-root=%d linked=%d(全链应恒 %s/NULL)", offRoot, linked, x)
	}
	var onRoot int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND trace_id=?`, execID, x).
		Scan(&onRoot); err != nil {
		t.Fatalf("count on-root rows: %v", err)
	}
	if onRoot < 2 {
		t.Fatalf("on-root 审计行 = %d, want ≥2(trigger + 重执行段;断言防空转)", onRoot)
	}
}

// faultWaitFor 轮询至 cond 为真或超时(返回最终 cond 值)。
func faultWaitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cond()
}

// TestFaultApprovalEnqueueFailureTraceIntact:审批 enqueue 失败注入(重投
// 恢复)下的 trace 连续性(T8.1 + T6.2 增补)。与 e2e_resume 的 T4.2 验收臂
// 同注入面(flakyEnqueuer 缝),差异 = 全链经 API 触发取 root X、决定经真
// 路由取段 Z,断言三面:
//   - 段首面:approval.decided 行 trace=Z(≠空)且 linked=X(决策请求新链
//     指回主链,T6.2 trace link);
//   - 重投面:resume 事件行(payload + trace 列)恒 = Z——对账恢复重投同一
//     真相,段链不因 enqueue 失败/恢复换段;
//   - 全链面:flush 后非空 trace ∈ {X, Z},linked 非空恰一行(=decided)= X。
//
// SLA:恢复后 3×(IdleInterval+BackoffMax)+3s 组件预算(e2e_resume 同式)。
func TestFaultApprovalEnqueueFailureTraceIntact(t *testing.T) {
	d := sqltest.MustOpenAt(t, nodesSchema)
	queue := fmt.Sprintf("t_n%d", atomic.AddUint32(&queueSeq, 1))

	// 定义直插:a(llm)→ boss(approval, carol)→ c(llm)。
	defIDs, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("seed snowflake: %v", err)
	}
	defID, err := defIDs.Next()
	if err != nil {
		t.Fatalf("gen def id: %v", err)
	}
	name := fmt.Sprintf("fault-enq-%d", defID)
	dsl := fmt.Sprintf(`{"dsl":"1.0","name":%q,
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"gen"},"outputs":{"text":"string"}},
  {"id":"boss","type":"approval","approvers":["carol"],"mode":"any","timeout_hours":24,
   "on_timeout":"fail","instructions":"请审批","outputs":{"decision":"string"}},
  {"id":"c","type":"llm",
   "prompt":{"template":"定稿","user_data":[{"name":"v","ref":"{{nodes.boss.output.decision}}"}]},
   "outputs":{"text":"string"}}],
 "edges":[{"from":"a","to":"boss"},{"from":"boss","to":"c"}],
 "outputs":{"final":"{{nodes.c.output.text}}"}}`, name)
	if _, err := d.Exec(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', ?, ?, 'seed', 'seed', 'seed', NOW(3))`,
		defID, name, dsl, string(make([]byte, 64))); err != nil {
		t.Fatalf("seed definition: %v", err)
	}

	// API 面(root X;决定事务经真路由取段 Z)。
	apiIDs, err := snowflake.New(3)
	if err != nil {
		t.Fatalf("api snowflake: %v", err)
	}
	apiSched, err := scheduler.New(scheduler.Deps{DB: d, IDs: apiIDs, Queue: queue})
	if err != nil {
		t.Fatalf("api scheduler: %v", err)
	}
	reg, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	traceProv, err := tracing.NewProvider(tracing.Config{
		Exporter: tracing.ExporterOff, ServiceName: "braid-api-fault-enq",
	})
	if err != nil {
		t.Fatalf("trace provider: %v", err)
	}
	t.Cleanup(func() { _ = traceProv.Shutdown(context.Background()) })
	gin.SetMode(gin.TestMode)
	apiSrv := api.NewServer(api.Config{
		Addr: "127.0.0.1:0", JWTSecret: nodesE2ESecret, SnowflakeNode: 3, ResumeQueue: queue,
		LoginRateLimit: 100000, LoginRateWindow: time.Minute,
		LoginMaxFailures: 100000, LoginLockout: time.Minute,
	}, d, apiIDs, reg, apiSched, auth.BuiltinProvider{DB: d})
	apiSrv.Tracing(traceProv.Tracer("braid/api"))
	router := apiSrv.Router()
	ensureE2EUser(t, d, "t81_author", auth.RoleAuthor)
	ensureE2EUser(t, d, "carol", auth.RoleApprover)

	// relay 带 flaky 缝(压缩退避,e2e_resume 同款)。
	flaky := &flakyEnqueuer{inner: relay.NewAsynqEnqueuer(redistest.MustOpt(t))}
	relayCfg := relay.DefaultConfig()
	relayCfg.IdleInterval = 30 * time.Millisecond
	relayCfg.EnqueueTimeout = 5 * time.Second
	relayCfg.BackoffBase = 50 * time.Millisecond
	relayCfg.BackoffFactor = 2
	relayCfg.BackoffMax = 200 * time.Millisecond
	relayCfg.MaxAttempts = 6
	rctx, relayStop := context.WithCancel(context.Background())
	relayDone := make(chan error, 1)
	go func() { relayDone <- relay.Run(rctx, relay.Deps{DB: d, Enqueuer: flaky}, relayCfg) }()
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

	executor := NewMux(mustE2ESched(t, d, queue),
		&fakeRunner{out: worker.Outcome{Status: engine.AttemptSucceed, Output: json.RawMessage(`{"text":"ok"}`)}},
		&fakeToolExec{}, ApprovalExecutor{})
	wcfg := worker.DefaultConfig()
	wcfg.HeartbeatInterval = 40 * time.Millisecond
	startE2EWorker(t, d, executor, wcfg, queue)

	// API 触发(root X)→ 挂起。
	tok := docLogin(t, router, "t81_author")
	code, out := doDocJSON(t, router, http.MethodPost, "/api/v1/executions", tok,
		[]byte(fmt.Sprintf(`{"definition_name":%q,"input":{}}`, name)))
	if code != http.StatusCreated {
		t.Fatalf("trigger = %d %v, want 201", code, out)
	}
	idStr, _ := out["id"].(string)
	execID, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		t.Fatalf("execution id %q: %v", idStr, err)
	}
	t.Cleanup(func() { cleanupNodesE2E(t, d, execID, defID) })
	waitE2EExecWaitingApproval(t, d, execID)

	// 注入:此后一切入队失败(续跑事件是唯一在飞事件);决定事务零 Redis
	// 依赖(201 本身即断言)。
	flaky.broken.Store(true)
	code, body := postDecision(t, router, execID, "approve", "可以")
	if code != http.StatusCreated {
		t.Fatalf("decide = %d %v, want 201(决定事务零 Redis 依赖)", code, body)
	}
	// 注入窗:续跑事件必被记失败(attempts ≥1、行留 PENDING)。
	if !faultWaitFor(t, 10*time.Second, func() bool {
		var attempts int
		return d.QueryRow(
			`SELECT deliver_attempts FROM outbox WHERE execution_id=? AND event_type=?`,
			execID, scheduler.EventTypeExecutionResume).Scan(&attempts) == nil && attempts >= 1
	}) {
		t.Fatalf("resume event deliver_attempts 未记账(失败注入未被记录)")
	}

	// 恢复 → SLA 内 SUCCEEDED。
	release := time.Now()
	flaky.broken.Store(false)
	sla := 3*(relayCfg.IdleInterval+relayCfg.BackoffMax) + 3*time.Second
	if !faultWaitFor(t, sla, func() bool {
		var status engine.WorkflowState
		return d.QueryRow(`SELECT status FROM workflow_execution WHERE id=?`, execID).
			Scan(&status) == nil && status == engine.WorkflowSucceeded
	}) {
		t.Fatalf("execution not SUCCEEDED within %v of enqueue recovery(对账恢复 SLA)", sla)
	}
	t.Logf("enqueue 恢复 → SUCCEEDED 耗时 %v(SLA %v)", time.Since(release), sla)

	// ---- 段首面:decided 行 trace=Z(非空)且 linked=X。----
	var x string
	if err := d.QueryRow(`SELECT trace_id FROM workflow_execution WHERE id=?`, execID).
		Scan(&x); err != nil || x == "" {
		t.Fatalf("workflow_execution.trace_id = %q err=%v, want 非空(API root 强制)", x, err)
	}
	var z sql.NullString
	var linked sql.NullString
	if err := d.QueryRow(`
SELECT trace_id, linked_trace_id FROM audit_log
 WHERE execution_id=? AND event_type=? AND actor_id='carol'`,
		execID, audit.EventApprovalDecided).Scan(&z, &linked); err != nil {
		t.Fatalf("read approval.decided: %v", err)
	}
	if !z.Valid || z.String == "" || z.String == x {
		t.Fatalf("decided 行 trace = %v, want 非空且 ≠ root(段首新链 Z)", z)
	}
	if !linked.Valid || linked.String != x {
		t.Fatalf("decided 行 linked = %v, want = root %q(trace link)", linked, x)
	}

	// ---- 重投面:resume 行 payload/列恒 = Z(重投同一真相,不换段)。----
	var colTrace sql.NullString
	var payload []byte
	if err := d.QueryRow(`
SELECT trace_id, payload FROM outbox
 WHERE execution_id=? AND event_type=?`,
		execID, scheduler.EventTypeExecutionResume).Scan(&colTrace, &payload); err != nil {
		t.Fatalf("read resume event: %v", err)
	}
	var rp scheduler.ResumePayload
	if err := json.Unmarshal(payload, &rp); err != nil {
		t.Fatalf("resume payload: %v", err)
	}
	if rp.TraceID != z.String || colTrace.String != z.String {
		t.Fatalf("resume 行 trace = payload %q / 列 %q, want 双 == Z %q(重投不换段)",
			rp.TraceID, colTrace.String, z.String)
	}

	// ---- 全链面:非空 trace ∈ {X,Z};linked 非空恰一行(=decided)= X。----
	var offChain, linkedRows int
	if err := d.QueryRow(`
SELECT SUM(trace_id IS NOT NULL AND trace_id NOT IN (?, ?)),
       SUM(linked_trace_id IS NOT NULL)
  FROM audit_log WHERE execution_id=?`, x, z.String, execID).Scan(&offChain, &linkedRows); err != nil {
		t.Fatalf("trace consistency scan: %v", err)
	}
	if offChain != 0 {
		t.Fatalf("链外 trace 行 = %d(全链应 ∈ {%s, %s})", offChain, x, z.String)
	}
	if linkedRows != 1 {
		t.Fatalf("linked 非空行 = %d, want 恰 1(decided 段首单点)", linkedRows)
	}
}

// e2eReaperTracer 给 faultinject 的 reaper 挂 off 导出 tracer(span/trace
// id 照生成、真实存在于 ctx 链——全链面断言「spans 在场时非空 trace 仍恒
// = X」的靶,D-t121 #4 解耦钉死)。
func e2eReaperTracer(t *testing.T) trace.Tracer {
	t.Helper()
	prov, err := tracing.NewProvider(tracing.Config{Exporter: tracing.ExporterOff, ServiceName: "braid-reaper-e2e"})
	if err != nil {
		t.Fatalf("trace provider: %v", err)
	}
	t.Cleanup(func() { _ = prov.Shutdown(context.Background()) })
	return prov.Tracer("braid/reaper")
}
