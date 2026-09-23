package worker

// T2.4 端到端 = M2 里程碑场景「桩 LLM + 桩谓词跑通 LLM → branch → LLM」:
// Instantiate(real DSL)→ outbox(exec:)→ 真 relay 投递 → 真 asynq server +
// 桩 executor(fakeExec,首跳产出布尔字段)→ scheduler 推进/路由/剪枝 →
// 执行 SUCCEEDED + outputs 物化。审计行断言归 T2.6(里程碑验收的另一半)。
//
// 变异自查:调度器没接 Complete → 执行卡 RUNNING(轮询超时红);relay 未起 →
// 同样卡(exec: 事件无人投);路由谓词失真 → d/c 状态断言红。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/redistest"
	"braid/internal/relay"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
)

func TestE2EBranchPipeline(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	ctx := context.Background()

	// 1) 定义 + 物化(调度器与 worker 同队列)。
	queue := nextQueue(t)
	defID, err := mustTestIDs(t).Next()
	if err != nil {
		t.Fatalf("gen def id: %v", err)
	}
	dslDoc := fmt.Sprintf(`{"dsl":"1.0","name":"e2e-%d",
 "edges":[{"from":"a","to":"b"},{"from":"b","to":"c"},{"from":"b","to":"d"}],
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"gen"},"outputs":{"flag":"boolean","text":"string"}},
  {"id":"b","type":"branch","cases":[{"when":{"left":"{{nodes.a.output.flag}}","op":"eq","right":true},"next":"c"}],"default":"d"},
  {"id":"c","type":"llm","prompt":{"template":"c","user_data":[{"name":"v","ref":"{{nodes.a.output.text}}"}]},"outputs":{"text":"string"}},
  {"id":"d","type":"llm","prompt":{"template":"d"},"outputs":{"text":"string"}}],
 "outputs":{"taken":"{{nodes.c.output.text}}","cut":"{{nodes.d.output.text}}"}}`, defID)
	if _, err := d.Exec(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', ?, ?, 'e2e', 'e2e', 'e2e', NOW(3))`,
		defID, fmt.Sprintf("e2e-%d", defID), dslDoc, string(make([]byte, 64))); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	schedIDs, err := snowflake.New(2)
	if err != nil {
		t.Fatalf("scheduler snowflake: %v", err)
	}
	sched, err := scheduler.New(scheduler.Deps{DB: d, IDs: schedIDs, Queue: queue})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	execID, err := sched.Instantiate(ctx, scheduler.InstantiateParams{
		DefinitionID: defID, Input: json.RawMessage(`{}`), TriggeredBy: "e2e", TriggerType: "TEST",
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	t.Cleanup(func() { cleanupE2E(t, d, execID, defID) })

	// 2) 真 relay(压缩 IdleInterval;生产投递路径零改动)。
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

	// 3) 真 worker + 桩 executor:首跳 a 出布尔,后续按路由落点出文本。
	ex := &fakeExec{fn: func(req Request, callNo int) (Outcome, error) {
		if callNo == 1 {
			return Outcome{Status: engine.AttemptSucceed,
				Output: json.RawMessage(`{"flag":true,"text":"路上"}`)}, nil
		}
		return Outcome{Status: engine.AttemptSucceed,
			Output: json.RawMessage(`{"text":"取路值"}`)}, nil
	}}
	cfg := DefaultConfig()
	cfg.HeartbeatInterval = 40 * time.Millisecond
	startWorkerOnQueue(t, d, ex, cfg, queue)

	// 4) 全链自动推进(exec: 事件已在 outbox,relay 即刻投递),轮询终态。
	e := waitE2EExec(t, d, execID, engine.WorkflowSucceeded)

	// 5) 断言:节点终态 + 剪枝 + attempt 行数 + 事件链 + outputs。
	assertE2ENode(t, d, execID, "a", engine.NodeSucceeded)
	assertE2ENode(t, d, execID, "b", engine.NodeSucceeded) // branch 内联,无 attempt 行
	assertE2ENode(t, d, execID, "c", engine.NodeSucceeded) // 取路臂被执行
	assertE2ENode(t, d, execID, "d", engine.NodeSkipped)   // 未取臂被剪枝
	for _, id := range []string{"a", "c"} {
		var rows int
		if err := d.QueryRow(`SELECT COUNT(*) FROM node_attempt na
		  JOIN node_execution ne ON na.node_execution_id = ne.id
		  WHERE na.execution_id=? AND ne.node_id=?`, execID, id).Scan(&rows); err != nil {
			t.Fatalf("count attempts %s: %v", id, err)
		}
		if rows != 1 {
			t.Errorf("node %s attempt rows = %d, want 1", id, rows)
		}
	}
	var branchAttempts int
	if err := d.QueryRow(`SELECT COUNT(*) FROM node_attempt na
	  JOIN node_execution ne ON na.node_execution_id = ne.id
	  WHERE na.execution_id=? AND ne.node_id='b'`, execID).Scan(&branchAttempts); err != nil {
		t.Fatalf("count branch attempts: %v", err)
	}
	if branchAttempts != 0 {
		t.Errorf("branch attempt rows = %d, want 0(内联求值不入队)", branchAttempts)
	}
	var outs map[string]any
	if err := json.Unmarshal([]byte(e.outputs.String), &outs); err != nil {
		t.Fatalf("outputs: %v", err)
	}
	if outs["taken"] != "取路值" {
		t.Errorf("outputs[taken] = %v", outs["taken"])
	}
	if v, ok := outs["cut"]; !ok || v != nil {
		t.Errorf("outputs[cut] = %#v(ok=%v), want null(剪枝臂)", v, ok)
	}
	if got := ex.count(); got != 2 {
		t.Errorf("executor calls = %d, want 2(a + c;d 被剪枝)", got)
	}
	// 事件链:exec: + att:a:1 + att:c:1(无 att:d)。
	events := e2eEvents(t, d, execID)
	wantSet := map[string]bool{
		fmt.Sprintf("exec:%d", execID): false,
	}
	for _, ev := range events {
		wantSet[ev] = true
	}
	if !wantSet[fmt.Sprintf("exec:%d", execID)] {
		t.Errorf("events = %v, want 含 exec:%d", events, execID)
	}
	attCount := 0
	for _, ev := range events {
		if len(ev) > 4 && ev[:4] == "att:" {
			attCount++
		}
	}
	if attCount != 2 {
		t.Errorf("att 事件数 = %d, want 2(a/c 各一;无 d)", attCount)
	}
}

// TestE2EDoubleStartConverges:exec: 双投递(首任务完成后记录即删,第二发
// 真实入队)→ Start 幂等,入口不重复派发。
func TestE2EDoubleStartConverges(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	ctx := context.Background()
	queue := nextQueue(t)
	defID, err := mustTestIDs(t).Next()
	if err != nil {
		t.Fatalf("gen def id: %v", err)
	}
	dslDoc := fmt.Sprintf(`{"dsl":"1.0","name":"e2e2-%d",
 "nodes":[{"id":"a","type":"llm","prompt":{"template":"x"},"outputs":{"text":"string"}}]}`, defID)
	if _, err := d.Exec(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', ?, ?, 'e2e', 'e2e', 'e2e', NOW(3))`,
		defID, fmt.Sprintf("e2e2-%d", defID), dslDoc, string(make([]byte, 64))); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	schedIDs, err := snowflake.New(2)
	if err != nil {
		t.Fatalf("scheduler snowflake: %v", err)
	}
	sched, err := scheduler.New(scheduler.Deps{DB: d, IDs: schedIDs, Queue: queue})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	execID, err := sched.Instantiate(ctx, scheduler.InstantiateParams{
		DefinitionID: defID, Input: json.RawMessage(`{}`), TriggeredBy: "e2e", TriggerType: "TEST",
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	t.Cleanup(func() { cleanupE2E(t, d, execID, defID) })

	relayCfg := relay.DefaultConfig()
	relayCfg.IdleInterval = 20 * time.Millisecond
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
		<-relayDone
	})

	ex := &fakeExec{fn: func(req Request, callNo int) (Outcome, error) {
		return Outcome{Status: engine.AttemptSucceed, Output: json.RawMessage(`{"text":"v"}`)}, nil
	}}
	cfg := DefaultConfig()
	cfg.HeartbeatInterval = 40 * time.Millisecond
	startWorkerOnQueue(t, d, ex, cfg, queue)
	waitE2EExec(t, d, execID, engine.WorkflowSucceeded)

	// 首任务已完成(Retention=0 记录即删)→ 同 TaskID 二发真实入队 → Start
	// 幂等(nil),不重复派发(executor 计数仍 1)。
	client := asynq.NewClient(redistest.MustOpt(t))
	t.Cleanup(func() { _ = client.Close() })
	payload, err := json.Marshal(scheduler.ExecStartPayload{ExecutionID: execID})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if _, err := client.EnqueueContext(ctx,
		asynq.NewTask(scheduler.EventTypeExecutionStart, payload),
		asynq.TaskID(fmt.Sprintf("exec:%d", execID)), asynq.Queue(queue), asynq.MaxRetry(1)); err != nil {
		t.Fatalf("second exec enqueue: %v", err)
	}
	// 记录消失(Retention=0 完成即删)= 第二发已被 handler ack(Start 幂等返回)。
	insp := asynq.NewInspector(redistest.MustOpt(t))
	t.Cleanup(func() { _ = insp.Close() })
	waitTaskGone(t, insp, queue, fmt.Sprintf("exec:%d", execID))
	if got := ex.count(); got != 1 {
		t.Errorf("executor calls = %d after exec: redelivery, want 1(Start 幂等)", got)
	}
	var rows int
	if err := d.QueryRow(`SELECT COUNT(*) FROM node_attempt WHERE execution_id=?`, execID).Scan(&rows); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if rows != 1 {
		t.Errorf("attempt rows = %d, want 1", rows)
	}
}

// ---- e2e 辅助 ----

type e2eExecRow struct {
	status  engine.WorkflowState
	outputs sql.NullString
}

func waitE2EExec(t *testing.T, d *sql.DB, execID uint64, want engine.WorkflowState) e2eExecRow {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last e2eExecRow
	for time.Now().Before(deadline) {
		err := d.QueryRow(`SELECT status, outputs FROM workflow_execution WHERE id=?`, execID).
			Scan(&last.status, &last.outputs)
		if err != nil {
			t.Fatalf("read execution: %v", err)
		}
		if last.status == want {
			return last
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("execution did not reach %s within 30s (last=%s)——链路断裂在 relay/worker/scheduler 某环",
		want, last.status)
	return last
}

func assertE2ENode(t *testing.T, d *sql.DB, execID uint64, nodeID string, want engine.NodeState) {
	t.Helper()
	var got engine.NodeState
	if err := d.QueryRow(`SELECT status FROM node_execution WHERE execution_id=? AND node_id=?`,
		execID, nodeID).Scan(&got); err != nil {
		t.Fatalf("read node %s: %v", nodeID, err)
	}
	if got != want {
		t.Errorf("node %s = %s, want %s", nodeID, got, want)
	}
}

func e2eEvents(t *testing.T, d *sql.DB, execID uint64) []string {
	t.Helper()
	rows, err := d.Query(`SELECT task_id FROM outbox WHERE execution_id=? ORDER BY id`, execID)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		out = append(out, id)
	}
	return out
}

func cleanupE2E(t *testing.T, d *sql.DB, execID, defID uint64) {
	t.Helper()
	for _, q := range []string{
		`DELETE FROM outbox WHERE execution_id=?`,
		`DELETE FROM node_attempt WHERE execution_id=?`,
		`DELETE FROM node_execution WHERE execution_id=?`,
		`DELETE FROM workflow_execution WHERE id=?`,
	} {
		if _, err := d.Exec(q, execID); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
	if _, err := d.Exec(`DELETE FROM workflow_definition WHERE id=?`, defID); err != nil {
		t.Fatalf("cleanup definition: %v", err)
	}
}
