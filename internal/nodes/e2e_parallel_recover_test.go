package nodes

// T19.5(D-2026-10-06-t195-parallel-incremental-recovery)增量恢复真链路 e2e:
// 真 relay + 真 worker + 真 asynq 下复验 scheduler 面单测锚定的保活语义。
//   - 旅程 A(部分失败 → 增量恢复):wait_all 三实例,boom 元素终败 →
//     wf FAILED;回滚自动定位 fan → SUCCEEDED 实例保活(零新 attempt、
//     version 零写)、终败实例重跑(新轮 attempt 台账)→ results 合并新旧 →
//     wf SUCCEEDED。kept 实例的 state_changed 恒 2 行(R1 全链)、重跑实例
//     5 行(两轮全链 + 重置 + 重派)是「零重烧」的审计机器判据。
//   - 旅程 B(取消楔源 → 增量恢复,D-t193 联动):hang-marker 时序栈保确定性
//     ——ok 实例先收口、hang 实例挂住 worker,取消楔停(hang 实例 + 控制器 +
//     后继 CANCELLED,ok 实例非楔源 untouched);放行迟到收口经 DEAD 哨兵
//     收敛后回滚 → ok 实例保活、楔源实例重跑 → wf SUCCEEDED。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/llm"
	"braid/internal/redistest"
	"braid/internal/relay"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
	"braid/internal/worker"
)

// parallelRecoverE2EDSL:pre → fan(wait_all,body 单次,{{item}} 注入)→ post。
// 元素字符串带 "boom"(毒化)/"hang"(挂起)标记即分别驱动失败/在飞时序。
const parallelRecoverE2EDSL = `{
 "dsl":"1.0","name":"pr-e2e-fan",
 "inputs":[{"name":"items","type":"array","required":true}],
 "edges":[{"from":"pre","to":"fan"},{"from":"fan","to":"post"}],
 "nodes":[
  {"id":"pre","type":"llm","model":"glm-5.3","prompt":{"template":"阶段A"},"outputs":{"text":"string"}},
  {"id":"fan","type":"parallel","parallel":{
    "foreach":"{{inputs.items}}","on_failure":"wait_all",
    "body":{"type":"llm","model":"glm-5.3","retry":{"max_attempts":1},
      "prompt":{"template":"评审","user_data":[{"name":"item","ref":"{{item}}"}]},
      "outputs":{"text":"string"}}}},
  {"id":"post","type":"llm","model":"glm-5.3",
   "prompt":{"template":"汇聚","user_data":[{"name":"v","ref":"{{nodes.fan.output.results}}"}]},
   "outputs":{"text":"string"}}],
 "outputs":{"all":"{{nodes.fan.output.results}}"}
}`

// prE2EStack:毒化(boom 500)+ 挂起(hang 阻塞至 release)双标记栈。
// 挂起按请求体逐实例判定——ok 实例即时收口、hang 实例稳定在飞,取消旅程
// 的确定性时序面(无「block 全局开/关与完成竞速」的窗口)。
type prE2EStack struct {
	d         *sql.DB
	sched     *scheduler.Scheduler
	execID    uint64
	defID     uint64
	poison    atomic.Bool
	release   chan struct{}
	released  atomic.Bool
	blockHang atomic.Bool
}

func prE2EStart(t *testing.T, doc, input string) *prE2EStack {
	t.Helper()
	d := sqltest.MustOpenAt(t, nodesSchema)
	queue := fmt.Sprintf("t_n%d", atomic.AddUint32(&queueSeq, 1))
	st := &prE2EStack{d: d, release: make(chan struct{})}
	// 失败路径防死锁:LIFO 使本 cleanup 先于 httptest 的 stub.Close()(后者
	// 等待在飞句柄)——测试体任何红点退出前先放行 hang 句柄。
	t.Cleanup(func() {
		if st.released.CompareAndSwap(false, true) {
			close(st.release)
		}
	})

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := json.Marshal(body)
		s := string(raw)
		if st.blockHang.Load() && strings.Contains(s, "hang") {
			<-st.release // hang 实例挂住直至放行(迟到收口旅程的时序闸)
		}
		if st.poison.Load() && strings.Contains(s, "boom") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"poisoned"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp-pr", "model": "glm-5.3",
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": `{"text":"ok"}`},
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
	t.Cleanup(stub.Close)

	llmCfg := llm.DefaultConfig()
	llmCfg.BaseURL = stub.URL
	llmCfg.APIKey = "sk-e2e"
	table, err := llm.ParsePriceTable(
		`{"glm-5.3":{"input_per_mtok":"2","output_per_mtok":"8","currency":"CNY"}}`)
	if err != nil {
		t.Fatalf("price table: %v", err)
	}
	llmCfg.PriceTable = table
	client, err := llm.NewOpenAICompat(llmCfg, nil)
	if err != nil {
		t.Fatalf("llm client: %v", err)
	}
	st.sched = mustE2ESched(t, d, queue)
	executor := NewMux(st.sched, &LLMExecutor{Client: client, Prices: table, DB: d},
		&fakeToolExec{}, ApprovalExecutor{})

	st.defID, st.execID = prE2ESeed(t, d, st.sched, doc, input)
	t.Cleanup(func() { cleanupNodesE2E(t, d, st.execID, st.defID) })

	relayCfg := relay.DefaultConfig()
	relayCfg.IdleInterval = 20 * time.Millisecond
	relayCfg.EnqueueTimeout = 5 * time.Second
	rctx, relayStop := context.WithCancel(context.Background())
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
	wcfg := worker.DefaultConfig()
	wcfg.HeartbeatInterval = 40 * time.Millisecond
	startE2EWorker(t, d, executor, wcfg, queue)
	return st
}

func prE2ESeed(t *testing.T, d *sql.DB, sched *scheduler.Scheduler, doc, input string) (defID, execID uint64) {
	t.Helper()
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("seed snowflake: %v", err)
	}
	defID, err = ids.Next()
	if err != nil {
		t.Fatalf("gen def id: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', ?, ?, 'e2e', 'e2e', 'e2e', NOW(3))`,
		defID, fmt.Sprintf("pr-e2e-%d", defID), doc, string(make([]byte, 64))); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	execID, err = sched.Instantiate(context.Background(), scheduler.InstantiateParams{
		DefinitionID: defID, Input: json.RawMessage(input), TriggeredBy: "e2e", TriggerType: "TEST",
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	if err := sched.Start(context.Background(), execID, "pr-e2e"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return defID, execID
}

func (st *prE2EStack) waitExecStatus(t *testing.T, want engine.WorkflowState) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var got engine.WorkflowState
		if err := st.d.QueryRow(`SELECT status FROM workflow_execution WHERE id=?`,
			st.execID).Scan(&got); err == nil && got == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("execution 未达 %s(60s)", want)
}

// prInstanceRow 读实例行(含 version/attempts,保活零写断言的锚)。
func prInstanceRow(t *testing.T, d *sql.DB, execID uint64, instanceNo uint16) (version uint32, attempts uint16, status engine.NodeState) {
	t.Helper()
	err := d.QueryRow(`SELECT version, attempts, status FROM node_execution
	  WHERE execution_id=? AND node_id='fan' AND instance_no=?`, execID, instanceNo).
		Scan(&version, &attempts, &status)
	if err != nil {
		t.Fatalf("read fan[%d]: %v", instanceNo, err)
	}
	return version, attempts, status
}

// prAttemptCount 数实例行的 attempt 台账行数(保活 = 零新 attempt)。
func prAttemptCount(t *testing.T, d *sql.DB, execID uint64, instanceNo uint16) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM node_attempt na
	  JOIN node_execution ne ON na.node_execution_id=ne.id
	  WHERE ne.execution_id=? AND ne.node_id='fan' AND ne.instance_no=?`,
		execID, instanceNo).Scan(&n); err != nil {
		t.Fatalf("count fan[%d] attempts: %v", instanceNo, err)
	}
	return n
}

// prStateChangedCount 数实例行的 node.state_changed 审计行(暂存 flush 后):
// 保活 = R1 恒 2;重跑 = 两轮全链 + 重置 + 重派。
func prStateChangedCount(t *testing.T, d *sql.DB, execID uint64, instanceNo uint16) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log
	  WHERE execution_id=? AND event_type='node.state_changed' AND node_id='fan'
	    AND CAST(JSON_UNQUOTE(JSON_EXTRACT(payload,'$.instance_no')) AS UNSIGNED)=?`,
		execID, instanceNo).Scan(&n); err != nil {
		t.Fatalf("count fan[%d] state_changed: %v", instanceNo, err)
	}
	return n
}

// TestE2EParallelIncrementalRecoveryJourney:旅程 A——wait_all 部分失败 →
// 回滚 → SUCCEEDED 实例零重烧(零新 attempt / version 零写 / 审计恒 2 行),
// 终败实例重跑新轮 → results 合并 → wf SUCCEEDED。
func TestE2EParallelIncrementalRecoveryJourney(t *testing.T) {
	st := prE2EStart(t, parallelRecoverE2EDSL, `{"items":["ok-a","boom-b","ok-c"]}`)
	st.poison.Store(true)

	// 第一轮:boom 实例终败(max=1),wait_all 收齐 → wf FAILED;1/3 成功。
	st.waitExecStatus(t, engine.WorkflowFailed)
	waitInstanceState(t, st.d, st.execID, "fan", 1, engine.NodeSucceeded)
	waitInstanceState(t, st.d, st.execID, "fan", 3, engine.NodeSucceeded)
	waitInstanceState(t, st.d, st.execID, "fan", 2, engine.NodeFailed)
	v1, a1, _ := prInstanceRow(t, st.d, st.execID, 1)
	v3, a3, _ := prInstanceRow(t, st.d, st.execID, 3)
	if a1 != 1 || a3 != 1 {
		t.Fatalf("R1 kept attempts = %d/%d, want 1/1", a1, a3)
	}

	// 修复等价物:毒化翻转 → 回滚自动定位 fan。
	st.poison.Store(false)
	res, err := st.sched.RollbackTo(context.Background(), scheduler.RollbackParams{
		ExecID: st.execID, WorkerID: "pr-e2e", ActorID: "e2e-op"})
	if err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	if res.TargetNodeID != "fan" || res.ResetCount != 2 {
		t.Fatalf("res = %+v, want target=fan reset=2(controller+failed instance)", res)
	}

	// 第二轮:重跑实例成功 → 收敛 → 后继 → wf SUCCEEDED。
	st.waitExecStatus(t, engine.WorkflowSucceeded)
	waitInstanceState(t, st.d, st.execID, "fan", 2, engine.NodeSucceeded)

	// 保活机器判据:1/3 零新 attempt、version 零写;重跑实例两轮台账。
	nv1, na1, st1 := prInstanceRow(t, st.d, st.execID, 1)
	nv3, na3, st3 := prInstanceRow(t, st.d, st.execID, 3)
	if st1 != engine.NodeSucceeded || st3 != engine.NodeSucceeded {
		t.Fatalf("kept = %s/%s, want SUCCEEDED", st1, st3)
	}
	if nv1 != v1 || nv3 != v3 || na1 != 1 || na3 != 1 {
		t.Fatalf("kept rows drifted: v %d→%d %d→%d, a %d %d (want 零写/零新 attempt)",
			v1, nv1, v3, nv3, na1, na3)
	}
	if prAttemptCount(t, st.d, st.execID, 1) != 1 || prAttemptCount(t, st.d, st.execID, 3) != 1 {
		t.Fatalf("kept attempt 台账 != 1(重烧了)")
	}
	_, ra2, _ := prInstanceRow(t, st.d, st.execID, 2)
	if ra2 != 2 || prAttemptCount(t, st.d, st.execID, 2) != 2 {
		t.Fatalf("rerun attempts/台账 = %d, want 2/2", ra2)
	}
	// 审计:kept 恒 2 行(R1 全链,回滚零行);重跑 5 行(两轮 + reset + claim)。
	flushStagedAudit(t, st.d)
	if n := prStateChangedCount(t, st.d, st.execID, 1); n != 2 {
		t.Fatalf("kept fan[1] state_changed = %d, want 2(R1 全链,保活零审计)", n)
	}
	if n := prStateChangedCount(t, st.d, st.execID, 2); n != 5 {
		t.Fatalf("rerun fan[2] state_changed = %d, want 5(两轮全链)", n)
	}
	// 收敛:results = 3 位(新旧合并),控制器 output 形状。
	var ctrlOut sql.NullString
	if err := st.d.QueryRow(`SELECT output FROM node_execution
	  WHERE execution_id=? AND node_id='fan' AND instance_no=0`, st.execID).Scan(&ctrlOut); err != nil || !ctrlOut.Valid {
		t.Fatalf("controller output: %v(%v)", ctrlOut, err)
	}
	var decoded struct {
		Results []any `json:"results"`
	}
	if err := json.Unmarshal([]byte(ctrlOut.String), &decoded); err != nil || len(decoded.Results) != 3 {
		t.Fatalf("results = %v(%v), want 3 位合并", decoded.Results, err)
	}
}

// TestE2EParallelCancelWedgeRecoveryJourney:旅程 B(D-t193 联动)——ok 实例
// 先收口、hang 实例挂住 worker → 取消楔停(hang 实例/控制器/后继 CANCELLED,
// ok 实例非楔源)→ 放行迟到收口 DEAD 哨兵收敛 → 回滚 → ok 实例保活、楔源
// 实例重跑 → wf SUCCEEDED。
func TestE2EParallelCancelWedgeRecoveryJourney(t *testing.T) {
	st := prE2EStart(t, parallelRecoverE2EDSL, `{"items":["ok-a","hang-b","hang-c"]}`)
	st.blockHang.Store(true)

	// 确定性时序:ok 实例收口 SUCCEEDED,hang 实例稳定 EXECUTING(挂桩上)。
	waitInstanceState(t, st.d, st.execID, "fan", 1, engine.NodeSucceeded)
	waitInstanceState(t, st.d, st.execID, "fan", 2, engine.NodeExecuting)
	waitInstanceState(t, st.d, st.execID, "fan", 3, engine.NodeExecuting)
	v1, a1, _ := prInstanceRow(t, st.d, st.execID, 1)

	if _, err := st.sched.CancelExecution(context.Background(), scheduler.CancelParams{
		ExecID: st.execID, WorkerID: "e2e:cancel", ActorID: "e2e-op"}); err != nil {
		t.Fatalf("CancelExecution: %v", err)
	}
	st.waitExecStatus(t, engine.WorkflowCancelled)
	waitInstanceState(t, st.d, st.execID, "fan", 1, engine.NodeSucceeded) // 非楔源 untouched
	waitInstanceState(t, st.d, st.execID, "fan", 2, engine.NodeCancelled)
	waitInstanceState(t, st.d, st.execID, "fan", 3, engine.NodeCancelled)

	// 放行 → 迟到收口撞取消事务已收口的 DEAD attempt(幂等哨兵),节点保持
	// CANCELLED;有界排空后回滚(T19.3 旅程同款口径)。
	st.blockHang.Store(false)
	if st.released.CompareAndSwap(false, true) {
		close(st.release)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n := prAttemptCount(t, st.d, st.execID, 2); n >= 1 {
			var dead int
			if err := st.d.QueryRow(`SELECT COUNT(*) FROM node_attempt na
			  JOIN node_execution ne ON na.node_execution_id=ne.id
			  WHERE ne.execution_id=? AND ne.node_id='fan' AND ne.instance_no=2
			    AND na.status='DEAD'`, st.execID).Scan(&dead); err == nil && dead >= 1 {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	waitInstanceState(t, st.d, st.execID, "fan", 2, engine.NodeCancelled)

	res, err := st.sched.RollbackTo(context.Background(), scheduler.RollbackParams{
		ExecID: st.execID, WorkerID: "pr-e2e", ActorID: "e2e-op"})
	if err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	if res.TargetNodeID != "fan" || res.ResetCount != 4 {
		t.Fatalf("res = %+v, want target=fan reset=4(controller+2 楔源实例+post)", res)
	}

	st.waitExecStatus(t, engine.WorkflowSucceeded)
	waitInstanceState(t, st.d, st.execID, "fan", 2, engine.NodeSucceeded)
	waitInstanceState(t, st.d, st.execID, "fan", 3, engine.NodeSucceeded)

	// 保活判据:ok 实例零新 attempt + version 零写;楔源实例两轮。
	nv1, na1, st1 := prInstanceRow(t, st.d, st.execID, 1)
	if st1 != engine.NodeSucceeded || nv1 != v1 || na1 != a1 || prAttemptCount(t, st.d, st.execID, 1) != 1 {
		t.Fatalf("kept fan[1] drifted: v %d→%d a %d→%d(非楔源保活)", v1, nv1, a1, na1)
	}
	for _, ino := range []uint16{2, 3} {
		if _, a, _ := prInstanceRow(t, st.d, st.execID, ino); a != 2 {
			t.Fatalf("wedge fan[%d] attempts = %d, want 2(重跑)", ino, a)
		}
	}
	var ctrlOut sql.NullString
	if err := st.d.QueryRow(`SELECT output FROM node_execution
	  WHERE execution_id=? AND node_id='fan' AND instance_no=0`, st.execID).Scan(&ctrlOut); err != nil || !ctrlOut.Valid {
		t.Fatalf("controller output: %v(%v)", ctrlOut, err)
	}
	var decoded struct {
		Results []any `json:"results"`
	}
	if err := json.Unmarshal([]byte(ctrlOut.String), &decoded); err != nil || len(decoded.Results) != 3 {
		t.Fatalf("results = %v(%v), want 3 位合并", decoded.Results, err)
	}
}
