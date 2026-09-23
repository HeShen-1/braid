package nodes

// T3.2 端到端验收:tool 节点全链 = Instantiate 真调度器 → outbox → 真 relay
// → 真 asynq server → Mux → ToolExecutor → tools.LocalRunner 真件(token 门
// + registry)→ 进程内工具。断言面:
//   - inputs 求值({{inputs.doc}} → 值)在真路径到达工具;
//   - scoped token per-call 签发并到达工具,且不出现在任何审计列(铁律:
//     审计行不含凭证——短时凭证本体同样不留痕);
//   - attempt 幂等键透传;
//   - outputs 声明抽取落 node output 与 workflow outputs;
//   - tool.call.completed 审计行:actor=tool/工具名、attempt_id 锚定、
//     payload 携时长观测、失败路径零发射。
//
// 沙箱环境白名单(主凭证不可达)的直证在 tools/command_test.go;此处链路
// 用进程内工具(载体无关:token 门/registry/收口对两类载体同构)。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"braid/internal/audit"
	"time"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/redistest"
	"braid/internal/relay"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
	"braid/internal/tools"
	"braid/internal/worker"
)

// e2eSpyTool:进程内工具真件形态(T3.5 内置工具的同构前身)——回显固定
// 声明友好输出,记录收到的完整 Call(token/幂等键传递面的断言锚)。
type e2eSpyTool struct {
	mu   sync.Mutex
	got  tools.Call
	seen bool
}

func (e *e2eSpyTool) Name() string { return "e2e_spy" }
func (e *e2eSpyTool) Run(_ context.Context, call tools.Call) (tools.Result, error) {
	e.mu.Lock()
	e.got, e.seen = call, true
	e.mu.Unlock()
	return tools.Result{Output: json.RawMessage(`{"summary":"spy 摘要","count":3}`)}, nil
}

func TestE2EToolNodeFullChain(t *testing.T) {
	d := sqltest.MustOpenAt(t, nodesSchema)
	ctx := context.Background()
	queue := fmt.Sprintf("t_n%d", queueSeq+100)

	spy := &e2eSpyTool{}
	reg, err := tools.NewRegistry(spy)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	runner := tools.NewLocalRunner(reg, tools.NewTokenIssuer(nil, 0, nil))
	executor := NewMux(mustE2ESched(t, d, queue), &fakeRunner{}, &ToolExecutor{
		Runner: runner, DB: d,
	}, ApprovalExecutor{}) // tool e2e 不触 approval 路由,装配完整即可

	sched := mustE2ESched(t, d, queue)
	defID, execID := seedToolExec(t, d, sched)
	t.Cleanup(func() { cleanupNodesE2E(t, d, execID, defID) })

	startE2ERelay(t, d, ctx)
	workerCfg := worker.DefaultConfig()
	workerCfg.HeartbeatInterval = 40 * time.Millisecond // 对齐 llm e2e 节奏
	startE2EWorker(t, d, executor, workerCfg, queue)

	waitE2EExecSucceeded(t, d, execID)
	assertE2ENodeState(t, d, execID, "read", engine.NodeSucceeded)
	var nodeExecID, attemptID uint64
	if err := d.QueryRow(`SELECT id FROM node_execution WHERE execution_id=? AND node_id=?`,
		execID, "read").Scan(&nodeExecID); err != nil {
		t.Fatalf("read node_execution id: %v", err)
	}

	// 1) 工具收到的调用面:inputs 求值、token、幂等键。
	spy.mu.Lock()
	gotCall, seen := spy.got, spy.seen
	spy.mu.Unlock()
	if !seen {
		t.Fatal("tool never dispatched")
	}
	if gotCall.Inputs["path"] != "/tmp/report.md" {
		t.Fatalf("tool inputs = %v, want evaluated path(引用求值真链)", gotCall.Inputs)
	}
	if gotCall.Token == "" {
		t.Fatal("scoped token not delivered to tool")
	}
	if want := fmt.Sprintf("att:%d:1", nodeExecID); gotCall.IdempotencyKey != want {
		t.Fatalf("idempotency key = %q, want %q", gotCall.IdempotencyKey, want)
	}

	// 2) outputs 声明抽取:node output + workflow outputs 同源。
	var rawOut []byte
	if err := d.QueryRow(`SELECT output FROM node_execution WHERE execution_id=? AND node_id=?`,
		execID, "read").Scan(&rawOut); err != nil {
		t.Fatalf("read node output: %v", err)
	}
	var nodeOut map[string]any
	if err := json.Unmarshal(rawOut, &nodeOut); err != nil {
		t.Fatalf("node output: %v", err)
	}
	if nodeOut["summary"] != "spy 摘要" || nodeOut["count"] != float64(3) {
		t.Fatalf("node output = %v", nodeOut)
	}

	// 3) tool.call.completed 审计行:词表/actor/attempt 锚/payload;且整行
	//    任何文本列不含 token 本体(凭证纪律端到端)。
	if err := d.QueryRow(`SELECT id FROM node_attempt WHERE node_execution_id=? AND attempt_no=1`,
		nodeExecID).Scan(&attemptID); err != nil {
		t.Fatalf("read attempt id: %v", err)
	}
	var actorType, actorID, payload string
	if err := d.QueryRow(
		`SELECT actor_type, actor_id, payload FROM audit_log
		  WHERE event_type=? AND attempt_id=?`, audit.EventToolCallCompleted, attemptID).
		Scan(&actorType, &actorID, &payload); err != nil {
		t.Fatalf("read tool.call.completed row: %v", err)
	}
	if actorType != audit.ActorTool || actorID != "e2e_spy" {
		t.Fatalf("actor = %s/%s, want tool/e2e_spy", actorType, actorID)
	}
	// MySQL JSON 列规范化(冒号后带空格),解码断言而非子串匹配。
	// 负向断言(审计 LOW):payload 键集恰为三键——给 payload 加 Inputs/
	// Output 等自由文本字段的「可观测性增强」变异必须红(D-t26 §4:入参
	// 真相源在 input_snapshot,审计不复制)。
	var pl map[string]any
	if err := json.Unmarshal([]byte(payload), &pl); err != nil {
		t.Fatalf("payload not json: %v (%s)", err, payload)
	}
	if len(pl) != 3 {
		t.Fatalf("payload keys = %v, want exactly {node_id, tool, duration_ms}", pl)
	}
	if pl["tool"] != "e2e_spy" || pl["node_id"] != "read" {
		t.Fatalf("payload = %v, want tool=e2e_spy node_id=read", pl)
	}
	if _, ok := pl["duration_ms"]; !ok {
		t.Fatalf("payload missing duration_ms: %v", pl)
	}
	if strings.Contains(payload, gotCall.Token) {
		t.Fatal("audit payload contains scoped token body")
	}
	var leaked int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND (payload LIKE ? OR actor_id LIKE ?)`,
		execID, "%"+gotCall.Token+"%", "%"+gotCall.Token+"%").Scan(&leaked); err != nil {
		t.Fatalf("scan leaks: %v", err)
	}
	if leaked != 0 {
		t.Fatalf("token leaked into %d audit rows", leaked)
	}
}

// TestE2EToolNodeFailurePath:工具失败(tool_failed)真链路——error_code 落列、
// 无 tool.call.completed 审计行(仅 SUCCEEDED 发射)、执行 fail-fast。
func TestE2EToolNodeFailurePath(t *testing.T) {
	d := sqltest.MustOpenAt(t, nodesSchema)
	ctx := context.Background()
	queue := fmt.Sprintf("t_n%d", queueSeq+101)

	boom := &boomTool{}
	reg, err := tools.NewRegistry(boom)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	runner := tools.NewLocalRunner(reg, tools.NewTokenIssuer(nil, 0, nil))
	executor := NewMux(mustE2ESched(t, d, queue), &fakeRunner{}, &ToolExecutor{
		Runner: runner, DB: d,
	}, ApprovalExecutor{}) // tool e2e 不触 approval 路由,装配完整即可

	sched := mustE2ESched(t, d, queue)
	defID, execID := seedToolExec(t, d, sched)
	t.Cleanup(func() { cleanupNodesE2E(t, d, execID, defID) })

	startE2ERelay(t, d, ctx)
	workerCfg := worker.DefaultConfig()
	workerCfg.HeartbeatInterval = 40 * time.Millisecond
	startE2EWorker(t, d, executor, workerCfg, queue)

	// fail-fast 到 FAILED(单节点无 retry)。
	waitE2EExecFailed(t, d, execID)
	assertE2ENodeState(t, d, execID, "read", engine.NodeFailed)

	var nodeCode, attCode sql.NullString
	var attempts uint16
	if err := d.QueryRow(`
SELECT ne.error_code, na.error_code, ne.attempts
  FROM node_execution ne JOIN node_attempt na ON na.node_execution_id = ne.id
 WHERE ne.execution_id=? AND ne.node_id='read'`, execID).
		Scan(&nodeCode, &attCode, &attempts); err != nil {
		t.Fatalf("read failure rows: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if !nodeCode.Valid || nodeCode.String != tools.CodeToolFailed || !attCode.Valid || attCode.String != tools.CodeToolFailed {
		t.Fatalf("codes = %v/%v, want tool_failed both", nodeCode, attCode)
	}
	var audited int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND event_type=?`,
		execID, audit.EventToolCallCompleted).Scan(&audited); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if audited != 0 {
		t.Fatalf("tool.call.completed rows = %d, want 0(仅 SUCCEEDED 发射)", audited)
	}
}

// boomTool 总失败。
type boomTool struct{}

func (boomTool) Name() string { return "e2e_spy" } // 同名占位:复用 seedToolExec 的 DSL
func (boomTool) Run(context.Context, tools.Call) (tools.Result, error) {
	return tools.Result{}, fmt.Errorf("e2e boom: simulated tool failure")
}

// ---- 基建补充 ----

func startE2ERelay(t *testing.T, d *sql.DB, ctx context.Context) {
	t.Helper()
	relayCfg := relay.DefaultConfig()
	relayCfg.IdleInterval = 20 * time.Millisecond
	relayCfg.EnqueueTimeout = 5 * time.Second
	rctx, relayStop := context.WithCancel(ctx)
	relayDone := make(chan error, 1)
	go func() {
		relayDone <- relay.Run(rctx, relay.Deps{
			DB:       d,
			Enqueuer: relay.NewAsynqEnqueuer(redistest.MustOpt(t)),
		}, relayCfg)
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
}

func waitE2EExecFailed(t *testing.T, d *sql.DB, execID uint64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var status engine.WorkflowState
		if err := d.QueryRow(`SELECT status FROM workflow_execution WHERE id=?`, execID).
			Scan(&status); err == nil && status == engine.WorkflowFailed {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("execution %d not FAILED within 15s", execID)
}

func seedToolExec(t *testing.T, d *sql.DB, sched *scheduler.Scheduler) (defID, execID uint64) {
	t.Helper()
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("seed snowflake: %v", err)
	}
	defID, err = ids.Next()
	if err != nil {
		t.Fatalf("gen def id: %v", err)
	}
	dslDoc := fmt.Sprintf(`{"dsl":"1.0","name":"e2e-tool-%d",
 "inputs":[{"name":"doc","type":"string"}],
 "nodes":[
  {"id":"read","type":"tool","tool":"e2e_spy",
   "inputs":{"path":"{{inputs.doc}}"},
   "outputs":{"summary":"string","count":"number"}}],
 "outputs":{"s":"{{nodes.read.output.summary}}","n":"{{nodes.read.output.count}}"}}`, defID)
	if _, err := d.Exec(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', ?, ?, 'e2e', 'e2e', 'e2e', NOW(3))`,
		defID, fmt.Sprintf("e2e-tool-%d", defID), dslDoc, string(make([]byte, 64))); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	execID, err = sched.Instantiate(context.Background(), scheduler.InstantiateParams{
		DefinitionID: defID, Input: json.RawMessage(`{"doc":"/tmp/report.md"}`),
		TriggeredBy: "e2e", TriggerType: "TEST",
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	return defID, execID
}
