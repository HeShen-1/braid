package nodes

// T9.2 回滚/断点续跑 e2e(D-2026-09-28-t92-rollback-same-branch,验收原文
// 「回滚→重跑→终态 + 审计链完整」):真 relay + 真 worker + 真 asynq。
//   - 旅程 A(收编):菱形 a→(b,c)→d,b 首轮毒化终败 → wf FAILED;回滚目标
//     = 汇聚点 d ⇒ 失败源 b 被闭包收编重置重派(线性形状结构性测不到的
//     CRITICAL 面Unit 已锚,此处真链路复验)→ b/c/d 终态 → wf SUCCEEDED;
//   - 旅程 B(目标重跑):线性 a→b,b 首轮毒化终败 → 回滚目标 = b 本身 ⇒
//     RerunNode 快照读回重跑(e2e 级「以 input_snapshot 重跑」逐字兑现:
//     attempt2 台账快照 == attempt1)+ 新一轮满额预算。
// 审计链断言:execution.rolled_back 直写行 + 按轮次分组的 node.state_changed
// 全序(暂存面 flush 后),两条路径各有完整 {EXECUTING→终态} 链。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// rollbackE2EDSL:菱形 a→(b,c)→d(b 模板带 boom 毒标,c/d 正常;d 引 b+c)。
const rollbackE2EDSL = `{
 "dsl":"1.0","name":"rb-e2e-diamond",
 "edges":[{"from":"a","to":"b"},{"from":"a","to":"c"},{"from":"b","to":"d"},{"from":"c","to":"d"}],
 "nodes":[
  {"id":"a","type":"llm","model":"glm-5.3","prompt":{"template":"阶段A"},"outputs":{"text":"string"}},
  {"id":"b","type":"llm","model":"glm-5.3","prompt":{"template":"boom-毒标节点"},"outputs":{"text":"string"}},
  {"id":"c","type":"llm","model":"glm-5.3","prompt":{"template":"旁路节点"},"outputs":{"text":"string"}},
  {"id":"d","type":"llm","model":"glm-5.3",
   "prompt":{"template":"汇聚","user_data":[{"name":"v","ref":"{{nodes.b.output.text}}"},{"name":"w","ref":"{{nodes.c.output.text}}"}]},
   "outputs":{"text":"string"}}],
 "outputs":{"done":"{{nodes.d.output.text}}"}}`

// rollbackE2EChainDSL:线性 a→b(b 毒标;RerunNode 读回旅程)。
const rollbackE2EChainDSL = `{
 "dsl":"1.0","name":"rb-e2e-chain",
 "edges":[{"from":"a","to":"b"}],
 "nodes":[
  {"id":"a","type":"llm","model":"glm-5.3","prompt":{"template":"阶段A"},"outputs":{"text":"string"}},
  {"id":"b","type":"llm","model":"glm-5.3","retry":{"max_attempts":2},
   "prompt":{"template":"boom-毒标节点","user_data":[{"name":"v","ref":"{{nodes.a.output.text}}"}]},
   "outputs":{"text":"string"}}],
 "outputs":{"done":"{{nodes.b.output.text}}"}}`

// quiesceE2EDSL:T19.6 quiesce 旅程——菱形,b 毒标终败(fail-fast),c 带
// 慢标(桩延迟应答 = wf FAILED 下 c 稳定在飞的残窗)。
const quiesceE2EDSL = `{
 "dsl":"1.0","name":"rb-e2e-quiesce",
 "edges":[{"from":"a","to":"b"},{"from":"a","to":"c"},{"from":"b","to":"d"},{"from":"c","to":"d"}],
 "nodes":[
  {"id":"a","type":"llm","model":"glm-5.3","prompt":{"template":"阶段A"},"outputs":{"text":"string"}},
  {"id":"b","type":"llm","model":"glm-5.3","prompt":{"template":"boom-毒标节点"},"outputs":{"text":"string"}},
  {"id":"c","type":"llm","model":"glm-5.3","prompt":{"template":"旁路慢标节点"},"outputs":{"text":"string"}},
  {"id":"d","type":"llm","model":"glm-5.3",
   "prompt":{"template":"汇聚","user_data":[{"name":"v","ref":"{{nodes.b.output.text}}"},{"name":"w","ref":"{{nodes.c.output.text}}"}]},
   "outputs":{"text":"string"}}],
 "outputs":{"done":"{{nodes.d.output.text}}"}}`

// rbE2EStack 装配毒化可翻转的桩 + 真适配器/执行器/relay/worker。
// block/release(T19.3):挂起全部 LLM 调用的闸——在飞取消旅程的稳定时序面
// (worker 真执行中触发取消,不受桩响应速度竞争)。slowGate(T19.6):模板含
// 「慢标」的调用阻塞至显式放行——fail-fast 后残留兄弟的确定性在飞窗
// (固定 sleep 在满载下会与毒化重试链竞速:失败侧被拉长、残窗同时耗尽)。
type rbE2EStack struct {
	d        *sql.DB
	sched    *scheduler.Scheduler
	execID   uint64
	defID    uint64
	poison   atomic.Bool
	block    atomic.Bool
	slow     atomic.Bool
	slowOnce sync.Once
	slowGate chan struct{}
	release  chan struct{}
}

func rbE2EStart(t *testing.T, doc, input string) *rbE2EStack {
	t.Helper()
	d := sqltest.MustOpenAt(t, nodesSchema)
	queue := fmt.Sprintf("t_n%d", atomic.AddUint32(&queueSeq, 1))
	st := &rbE2EStack{d: d, release: make(chan struct{}), slowGate: make(chan struct{})}

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if st.block.Load() {
			<-st.release // 挂住直至放行(迟到收口旅程的时序闸)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := json.Marshal(body)
		if st.slow.Load() && strings.Contains(string(raw), "慢标") {
			<-st.slowGate // T19.6:确定性在飞残窗——楔停必发生在应答前,放行才迟到
		}
		if st.poison.Load() && strings.Contains(string(raw), "boom") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"poisoned"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp-rb", "model": "glm-5.3",
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": `{"text":"ok"}`},
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
	t.Cleanup(stub.Close)
	// 兜底放行(须后于 stub.Close 注册:LIFO 先执行——否则失败路径上
	// stub.Close 等待挂起在闸上的请求,测试二进制挂到包超时;对抗审计 M5)。
	t.Cleanup(func() { st.releaseSlow() })

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

	st.defID, st.execID = rbE2ESeed(t, d, st.sched, doc, input)
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

func rbE2ESeed(t *testing.T, d *sql.DB, sched *scheduler.Scheduler, doc, input string) (defID, execID uint64) {
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
		defID, fmt.Sprintf("rb-e2e-%d", defID), doc, string(make([]byte, 64))); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	execID, err = sched.Instantiate(context.Background(), scheduler.InstantiateParams{
		DefinitionID: defID, Input: json.RawMessage(input), TriggeredBy: "e2e", TriggerType: "TEST",
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	if err := sched.Start(context.Background(), execID, "rb-e2e"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return defID, execID
}

func (st *rbE2EStack) waitExecStatus(t *testing.T, want engine.WorkflowState) {
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

// releaseSlow 放行慢标闸(sync.Once:测试体与 t.Cleanup 双路径幂等)。
func (st *rbE2EStack) releaseSlow() {
	st.slowOnce.Do(func() { close(st.slowGate) })
}

// TestE2ERollbackAnnexJourney:旅程 A——菱形,b 首轮毒化终败 → 回滚目标 d
// (汇聚点)⇒ 失败源 b 收编重派 → 全链终态 SUCCEEDED + 审计链完整。
func TestE2ERollbackAnnexJourney(t *testing.T) {
	st := rbE2EStart(t, rollbackE2EDSL, `{}`)
	st.poison.Store(true)

	// 第一轮:b 毒化终败(max=1)→ wf FAILED;c 迟到成功;d 冻结 PENDING。
	st.waitExecStatus(t, engine.WorkflowFailed)
	waitInstanceState(t, st.d, st.execID, "c", 0, engine.NodeSucceeded)
	assertE2ENodeState(t, st.d, st.execID, "d", engine.NodePending)

	// 翻转毒化(修复动作的等价物)→ 回滚目标 = d(b 作为失败源被收编)。
	st.poison.Store(false)
	res, err := st.sched.RollbackTo(context.Background(), scheduler.RollbackParams{
		ExecID: st.execID, TargetNodeID: "d", WorkerID: "rb-e2e", ActorID: "e2e-op"})
	if err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	if res.RerunTarget {
		t.Fatalf("d 为冻结 PENDING,应走重评估路径")
	}

	// 第二轮:b 收编重派成功 → d 派发 → wf SUCCEEDED(终态可达的验收本体)。
	waitE2EExecSucceeded(t, st.d, st.execID)
	assertE2ENodeState(t, st.d, st.execID, "b", engine.NodeSucceeded)
	assertE2ENodeState(t, st.d, st.execID, "d", engine.NodeSucceeded)

	// 台账:b 两轮 attempt 行——旧轮 FAILED(output NULL)、新轮 SUCCEEDED
	//(input/output 双留档;worker 认领抄档的真链路形态)。
	type attRow struct {
		no     uint16
		status string
		input  sql.NullString
		output sql.NullString
	}
	atts := []attRow{}
	rs, err := st.d.Query(`SELECT na.attempt_no, na.status, na.input_snapshot, na.output
	  FROM node_attempt na JOIN node_execution ne ON na.node_execution_id=ne.id
	  WHERE ne.execution_id=? AND ne.node_id='b' ORDER BY na.attempt_no`, st.execID)
	if err != nil {
		t.Fatalf("query attempts: %v", err)
	}
	for rs.Next() {
		var a attRow
		if err := rs.Scan(&a.no, &a.status, &a.input, &a.output); err != nil {
			t.Fatalf("scan attempt: %v", err)
		}
		atts = append(atts, a)
	}
	_ = rs.Close()
	if len(atts) != 2 {
		t.Fatalf("b attempts = %d, want 2(两轮)", len(atts))
	}
	if atts[0].status != "FAILED" || atts[0].output.Valid {
		t.Fatalf("旧轮 = %s/%v, want FAILED/NULL-output", atts[0].status, atts[0].output)
	}
	if atts[1].status != "SUCCEEDED" || !atts[1].input.Valid || !atts[1].output.Valid {
		t.Fatalf("新轮 = %s(in=%v out=%v), want SUCCEEDED 双留档", atts[1].status, atts[1].input, atts[1].output)
	}

	// 审计链:rolled_back 直写行 + b 的四段 state_changed(两轮各
	// EXECUTING→终态 + 重置 FAILED→PENDING + 重派 PENDING→EXECUTING)。
	var rolled int
	if err := st.d.QueryRow(`SELECT COUNT(*) FROM audit_log
	  WHERE execution_id=? AND event_type='execution.rolled_back'`, st.execID).Scan(&rolled); err != nil {
		t.Fatalf("count rolled_back: %v", err)
	}
	if rolled != 1 {
		t.Fatalf("execution.rolled_back = %d, want 1", rolled)
	}
	flushStagedAudit(t, st.d)
	var bStates int
	if err := st.d.QueryRow(`SELECT COUNT(*) FROM audit_log
	  WHERE execution_id=? AND event_type='node.state_changed' AND node_id='b'`, st.execID).
		Scan(&bStates); err != nil {
		t.Fatalf("count b states: %v", err)
	}
	// 两轮全链:R1 {PENDING→EXECUTING, EXECUTING→FAILED};回滚 reset
	// {FAILED→PENDING};R2 {PENDING→EXECUTING, EXECUTING→SUCCEEDED} = 5 行
	//(两条路径按 attempt_no 分组可追溯的机器判据)。
	if bStates != 5 {
		t.Fatalf("b state_changed 行 = %d, want 5(两条路径全链)", bStates)
	}
}

// TestE2ERollbackRerunJourney:旅程 B——线性,b 毒化耗尽(含平台重试,
// max=2)→ 回滚目标 = b 本身 ⇒ RerunNode 快照读回重跑(attempt2 台账快照
// == attempt1,e2e 级「以 input_snapshot 重跑」)+ 新一轮满额预算。
func TestE2ERollbackRerunJourney(t *testing.T) {
	st := rbE2EStart(t, rollbackE2EChainDSL, `{}`)
	st.poison.Store(true)

	st.waitExecStatus(t, engine.WorkflowFailed)
	assertE2ENodeState(t, st.d, st.execID, "b", engine.NodeFailed)
	var bAttempts uint16
	if err := st.d.QueryRow(`SELECT attempts FROM node_execution
	  WHERE execution_id=? AND node_id='b'`, st.execID).Scan(&bAttempts); err != nil || bAttempts != 2 {
		t.Fatalf("b attempts = %d(%v), want 2(首轮烧满)", bAttempts, err)
	}

	// 旧轮快照(读回断言的锚)。
	var oldSnap sql.NullString
	if err := st.d.QueryRow(`SELECT input_snapshot FROM node_execution
	  WHERE execution_id=? AND node_id='b'`, st.execID).Scan(&oldSnap); err != nil || !oldSnap.Valid {
		t.Fatalf("旧轮快照: %v(%v)", oldSnap, err)
	}

	st.poison.Store(false)
	res, err := st.sched.RollbackTo(context.Background(), scheduler.RollbackParams{
		ExecID: st.execID, TargetNodeID: "b", WorkerID: "rb-e2e", ActorID: "e2e-op"})
	if err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	if !res.RerunTarget {
		t.Fatalf("目标 b(FAILED,快照在)应走 RerunNode 读回重跑")
	}

	waitE2EExecSucceeded(t, st.d, st.execID)
	// 新一轮:attempts=3(读回 claim);行快照逐字读回;满额预算未被旧轮吃掉
	//(本旅程翻转后一次成功,预算断言在 scheduler 单测锚定)。
	var b struct {
		attempts uint16
		snap     sql.NullString
	}
	if err := st.d.QueryRow(`SELECT attempts, input_snapshot FROM node_execution
	  WHERE execution_id=? AND node_id='b'`, st.execID).Scan(&b.attempts, &b.snap); err != nil {
		t.Fatalf("read b: %v", err)
	}
	if b.attempts != 3 {
		t.Fatalf("b attempts = %d, want 3(Rerun claim)", b.attempts)
	}
	if b.snap.String != oldSnap.String {
		t.Fatalf("重跑快照未逐字读回:\n got %s\nwant %s", b.snap.String, oldSnap.String)
	}
	// 台账:attempt3 的 input == 旧轮快照(worker 认领抄档);attempt2(平台
	// 重试)与 attempt1(首轮)同快照(前驱终态不可变 ⇒ 求值恒同)。
	var a3Input sql.NullString
	if err := st.d.QueryRow(`SELECT na.input_snapshot FROM node_attempt na
	  JOIN node_execution ne ON na.node_execution_id=ne.id
	  WHERE ne.execution_id=? AND ne.node_id='b' AND na.attempt_no=3`, st.execID).
		Scan(&a3Input); err != nil || !a3Input.Valid {
		t.Fatalf("attempt3 台账快照: %v(%v)", a3Input, err)
	}
	if a3Input.String != oldSnap.String {
		t.Fatalf("attempt3 台账快照 ≠ 旧轮行快照(读回抄档断裂)")
	}
}

// TestE2ERollbackQuiesceJourney:T19.6(D-2026-10-06-t196)quiesce 真链路
// 旅程——fail-fast 残留(c 的 LLM 调用阻塞在桩闸上 = 确定性在飞残窗):
// 无旗标 409(协议观测态)→ quiesce=true 单事务楔停收敛(不等 reaper
// orphanQuiet 2min)→ 重置重跑 → wf SUCCEEDED;迟到收口经 attempt DEAD
// 幂等哨兵吸收(D-t193 机制继承);审计链 rolled_back.quiesced_count +
// c 的 from=EXECUTING→CANCELLED 楔停行。
func TestE2ERollbackQuiesceJourney(t *testing.T) {
	st := rbE2EStart(t, quiesceE2EDSL, `{}`)
	st.poison.Store(true)
	st.slow.Store(true)

	// 第一轮:b 毒化终败(max=1)→ wf FAILED;c 的调用睡在桩里(在飞残留,
	// 场景本体:现行为须等 reaper orphanQuiet 2min 才 settle)。
	st.waitExecStatus(t, engine.WorkflowFailed)
	waitInstanceState(t, st.d, st.execID, "c", 0, engine.NodeExecuting)
	// 认领钉死(对抗审计 M4):node 行 EXECUTING 在 dispatch 即置位,attempt
	// 行 worker 认领时才物化——未认领形态下楔停收口环零对象、台账恰 1 行,
	// 下游 atts==2 断言假红(合法形态的两分法见 TestE2ECancelJourney)。本
	// 旅程按已认领形态锚:轮询 c 的 RUNNING attempt 出现才继续。
	claimDeadline := time.Now().Add(30 * time.Second)
	claimed := false
	for time.Now().Before(claimDeadline) {
		var n int
		if err := st.d.QueryRow(`SELECT COUNT(*) FROM node_attempt na
		  JOIN node_execution ne ON na.node_execution_id=ne.id
		  WHERE ne.execution_id=? AND ne.node_id='c' AND na.status='RUNNING'`, st.execID).Scan(&n); err == nil && n > 0 {
			claimed = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !claimed {
		t.Fatalf("c 未被 worker 认领(30s)——旅程前提(已认领形态)未成立")
	}

	// 无旗标 = 409 blocked_inflight(协议观测态;Blocked 清单引导)。
	_, err := st.sched.RollbackTo(context.Background(), scheduler.RollbackParams{
		ExecID: st.execID, TargetNodeID: "d", WorkerID: "rb-e2e", ActorID: "e2e-op"})
	var re *scheduler.RollbackError
	if !errors.As(err, &re) || re.Kind != scheduler.RollbackBlockedInflight {
		t.Fatalf("无旗标 err = %v, want blocked_inflight(观测态)", err)
	}

	// quiesce=true:单事务楔停 c + 回滚(b 收编;c 重置 PENDING → advance
	// 重派)。楔停时 c 的调用仍阻塞在闸上 = 残留在场;回滚返回后放行(关
	// slow + 开 gate):旧调用迟到走哨兵、重派轮即时应答。
	st.poison.Store(false)
	res, err := st.sched.RollbackTo(context.Background(), scheduler.RollbackParams{
		ExecID: st.execID, TargetNodeID: "d", WorkerID: "rb-e2e", ActorID: "e2e-op", Quiesce: true})
	if err != nil {
		st.slow.Store(false)
		st.releaseSlow()
		t.Fatalf("RollbackTo(quiesce): %v", err)
	}
	if res.QuiescedCount != 1 {
		st.slow.Store(false)
		st.releaseSlow()
		t.Fatalf("quiesced_count = %d, want 1(c 残行恰一)", res.QuiescedCount)
	}
	st.slow.Store(false)
	st.releaseSlow()

	// 重跑轮:b/c 重派成功(slow 已关,新调用即时应答)→ d 汇聚 → wf
	// SUCCEEDED。放行后 c 的旧调用迟到达 → attempt DEAD 哨兵吸收。
	waitE2EExecSucceeded(t, st.d, st.execID)
	assertE2ENodeState(t, st.d, st.execID, "b", engine.NodeSucceeded)
	assertE2ENodeState(t, st.d, st.execID, "c", engine.NodeSucceeded)
	assertE2ENodeState(t, st.d, st.execID, "d", engine.NodeSucceeded)

	// 台账:c 恰两 attempt——旧轮 DEAD/rollback_quiesce(楔停收口 + 归因
	// code,区别 rollback_reset),重跑轮 SUCCEEDED。
	type attRow struct {
		no     uint16
		status string
		code   sql.NullString
	}
	atts := []attRow{}
	rs, err := st.d.Query(`SELECT na.attempt_no, na.status, na.error_code
	  FROM node_attempt na JOIN node_execution ne ON na.node_execution_id=ne.id
	  WHERE ne.execution_id=? AND ne.node_id='c' ORDER BY na.attempt_no`, st.execID)
	if err != nil {
		t.Fatalf("query c attempts: %v", err)
	}
	for rs.Next() {
		var a attRow
		if err := rs.Scan(&a.no, &a.status, &a.code); err != nil {
			t.Fatalf("scan attempt: %v", err)
		}
		atts = append(atts, a)
	}
	_ = rs.Close()
	if len(atts) != 2 {
		t.Fatalf("c attempts = %d, want 2(旧轮楔停 + 重跑轮)", len(atts))
	}
	if atts[0].status != "DEAD" || atts[0].code.String != "rollback_quiesce" {
		t.Fatalf("旧轮 = %s/%v, want DEAD/rollback_quiesce", atts[0].status, atts[0].code)
	}
	if atts[1].status != "SUCCEEDED" {
		t.Fatalf("重跑轮 = %s, want SUCCEEDED", atts[1].status)
	}

	// 审计链:rolled_back payload 携 quiesced_count;c 的 from=EXECUTING
	//→CANCELLED 楔停行(暂存 flush 后)。
	var payload []byte
	if err := st.d.QueryRow(`SELECT payload FROM audit_log
	  WHERE execution_id=? AND event_type='execution.rolled_back'`, st.execID).Scan(&payload); err != nil {
		t.Fatalf("read rolled_back: %v", err)
	}
	var p struct {
		QuiescedCount int `json:"quiesced_count"`
	}
	if err := json.Unmarshal(payload, &p); err != nil || p.QuiescedCount != 1 {
		t.Fatalf("rolled_back.quiesced_count = %+v(%v), want 1", p, err)
	}
	flushStagedAudit(t, st.d)
	var wedged int
	if err := st.d.QueryRow(`SELECT COUNT(*) FROM audit_log
	  WHERE execution_id=? AND event_type='node.state_changed' AND node_id='c'
	    AND JSON_UNQUOTE(JSON_EXTRACT(payload,'$.from'))='EXECUTING'
	    AND JSON_UNQUOTE(JSON_EXTRACT(payload,'$.to'))='CANCELLED'`, st.execID).Scan(&wedged); err != nil || wedged != 1 {
		t.Fatalf("c 的 EXECUTING→CANCELLED 楔停行 = %d(%v), want 1", wedged, err)
	}

	// 迟到收敛稳定性:旧调用放行后收口,状态面不动——哨兵吸收零痕迹
	//(有界等待窗过,断言终态稳定;D-t193 取消旅程同款收敛机制的复验)。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var attCnt int
		if err := st.d.QueryRow(`SELECT COUNT(*) FROM node_attempt na
		  JOIN node_execution ne ON na.node_execution_id=ne.id
		  WHERE ne.execution_id=? AND ne.node_id='c'`, st.execID).Scan(&attCnt); err == nil && attCnt == 2 {
			// attempt 面已定型;再等一拍让迟到收口事务跑完。
			time.Sleep(500 * time.Millisecond)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	assertE2ENodeState(t, st.d, st.execID, "c", engine.NodeSucceeded)
	var wfLate engine.WorkflowState
	if err := st.d.QueryRow(`SELECT status FROM workflow_execution WHERE id=?`,
		st.execID).Scan(&wfLate); err != nil || wfLate != engine.WorkflowSucceeded {
		t.Fatalf("迟到收口后 wf = %v(%v), want SUCCEEDED(哨兵吸收)", wfLate, err)
	}
}

// TestE2ECancelJourney:T19.3 取消真链路旅程(D-2026-10-05-t193)——worker
// 真执行中(LLM 桩挂起)触发取消:全行楔停 + wf CANCELLED 原子提交;放行后
// 迟到收口经幂等哨兵收敛(attempt DEAD + 终态守卫,节点保持 CANCELLED);
// resume 自动定位楔源复活重跑 → wf SUCCEEDED;审计链 execution.cancelled
// 直写行 + 逐行 CANCELLED state_changed(暂存 flush 后)。
func TestE2ECancelJourney(t *testing.T) {
	st := rbE2EStart(t, rollbackE2EChainDSL, `{}`)
	st.block.Store(true) // a 的 LLM 调用挂住:EXECUTING 滞留的稳定在飞面

	// 在飞:a EXECUTING(worker 阻塞在桩上)。
	waitInstanceState(t, st.d, st.execID, "a", 0, engine.NodeExecuting)

	res, err := st.sched.CancelExecution(context.Background(), scheduler.CancelParams{
		ExecID: st.execID, WorkerID: "e2e:cancel", ActorID: "e2e-op"})
	if err != nil {
		t.Fatalf("CancelExecution: %v", err)
	}
	if res.FromStatus != engine.WorkflowRunning {
		t.Fatalf("from_status = %s, want RUNNING", res.FromStatus)
	}
	assertE2ENodeState(t, st.d, st.execID, "a", engine.NodeCancelled)
	assertE2ENodeState(t, st.d, st.execID, "b", engine.NodeCancelled)
	var wfSt engine.WorkflowState
	if err := st.d.QueryRow(
		`SELECT status FROM workflow_execution WHERE id=?`, st.execID).Scan(&wfSt); err != nil || wfSt != engine.WorkflowCancelled {
		t.Fatalf("wf = %v(%v), want CANCELLED", wfSt, err)
	}

	// 放行 → worker 迟到收口撞 attempt DEAD 幂等哨兵;节点/wf 保持 CANCELLED
	//(真链路的竞态收敛面,scheduler 层已锚,此处复验)。有界排空而非猜测
	// sleep:轮询 attempt 终态翻定(哨兵路径的 observable——收口事务必已跑)
	// 或 5s 超时(仍可断言状态面,死信重试面归 asynq)。
	st.block.Store(false)
	close(st.release)
	deadline := time.Now().Add(5 * time.Second)
	sawAttemptFinal := false
	for time.Now().Before(deadline) {
		var cnt int
		if err := st.d.QueryRow(`SELECT COUNT(*) FROM node_attempt na
		  JOIN node_execution ne ON na.node_execution_id=ne.id
		  WHERE ne.execution_id=? AND ne.node_id='a' AND na.status IN ('DEAD','FAILED')`,
			st.execID).Scan(&cnt); err == nil && cnt > 0 {
			sawAttemptFinal = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	assertE2ENodeState(t, st.d, st.execID, "a", engine.NodeCancelled)
	wfSt = engine.WorkflowState("")
	if err := st.d.QueryRow(
		`SELECT status FROM workflow_execution WHERE id=?`, st.execID).Scan(&wfSt); err != nil || wfSt != engine.WorkflowCancelled {
		t.Fatalf("迟到收口后 wf = %v(%v), want CANCELLED(终态守卫零推进)", wfSt, err)
	}
	// 正向观测(对抗审计 C8):状态断言对「哨兵链整体回归」无牙
	//(completeOnce 的 CAS 也会自守卫);attempt 面与审计面才是专属
	// observable。取消时刻存在两个合法收敛形态:worker 已认领(attempt
	// RUNNING → 取消事务收口 DEAD)或未认领(asynq 副本被活态闸拦,
	// attempt 零物化)——两者都满足「RUNNING attempt 不残留、成功永不落链」。
	var attDead, attOther int
	if err := st.d.QueryRow(`SELECT
	    COALESCE(SUM(na.status='DEAD'), 0), COALESCE(SUM(na.status NOT IN ('DEAD')), 0)
	  FROM node_attempt na JOIN node_execution ne ON na.node_execution_id=ne.id
	  WHERE ne.execution_id=? AND ne.node_id='a'`, st.execID).Scan(&attDead, &attOther); err != nil {
		t.Fatalf("count a attempts: %v", err)
	}
	if attOther != 0 {
		t.Fatalf("a 的非 DEAD attempt = %d, want 0(取消后 RUNNING 不残留)", attOther)
	}
	flushStagedAudit(t, st.d)
	var succeededAfter int
	if err := st.d.QueryRow(`SELECT COUNT(*) FROM audit_log
	  WHERE execution_id=? AND event_type='node.state_changed' AND node_id='a'
	    AND JSON_UNQUOTE(JSON_EXTRACT(payload,'$.to'))='SUCCEEDED'`, st.execID).Scan(&succeededAfter); err != nil || succeededAfter != 0 {
		t.Fatalf("取消后 a 的 →SUCCEEDED 审计行 = %d(%v), want 0(迟到成功永不落链)", succeededAfter, err)
	}
	_ = sawAttemptFinal

	// resume(空目标)→ 楔源 a(EXECUTING 取消行快照在 ⇒ RerunNode)→
	// 重跑成功 → b 派发成功 → wf SUCCEEDED。
	rbRes, err := st.sched.RollbackTo(context.Background(), scheduler.RollbackParams{
		ExecID: st.execID, WorkerID: "e2e-rb", ActorID: "e2e-op"})
	if err != nil {
		t.Fatalf("resume cancelled: %v", err)
	}
	if rbRes.TargetNodeID != "a" || !rbRes.RerunTarget {
		t.Fatalf("res = %+v, want target=a rerun=true", rbRes)
	}
	waitE2EExecSucceeded(t, st.d, st.execID)
	assertE2ENodeState(t, st.d, st.execID, "a", engine.NodeSucceeded)
	assertE2ENodeState(t, st.d, st.execID, "b", engine.NodeSucceeded)

	// 审计链:execution.cancelled 直写行 + 逐行 CANCELLED state_changed。
	var cancelled int
	if err := st.d.QueryRow(`SELECT COUNT(*) FROM audit_log
	  WHERE execution_id=? AND event_type='execution.cancelled'`, st.execID).Scan(&cancelled); err != nil || cancelled != 1 {
		t.Fatalf("execution.cancelled = %d(%v), want 1", cancelled, err)
	}
	flushStagedAudit(t, st.d)
	var wedged int
	if err := st.d.QueryRow(`SELECT COUNT(*) FROM audit_log
	  WHERE execution_id=? AND event_type='node.state_changed'
	    AND JSON_UNQUOTE(JSON_EXTRACT(payload,'$.to'))='CANCELLED'`, st.execID).Scan(&wedged); err != nil || wedged != 2 {
		t.Fatalf("CANCELLED state_changed = %d(%v), want 2(a+b)", wedged, err)
	}
}
