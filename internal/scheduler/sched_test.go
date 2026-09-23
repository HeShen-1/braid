package scheduler

// 集成测试(t13 真 MySQL,不可达自动 skip;本包零 Redis 面——队列交互归
// worker 包 e2e)。验收锚:入度推进单测(多前驱、跳过分支)+ branch 显式
// 布尔路由 + 执行完成回写 + 重试/耗尽/fail-fast/幂等重放/终态守卫。
//
// 变异自查(实现错 → 哪条红):
//   - 前驱计数用快照读而非锁读 → 菱形并发子测试红(d 不被派发或双派发);
//   - 全行锁改部分锁 → 菱形并发子测试出现死锁错误上抛;
//   - 终态判定留在事务内 → 不相交臂并发子测试红(执行卡 RUNNING);
//   - 路由在 claim 之后 → 路由错误子测试红(branch 留 EXECUTING 而非 PENDING);
//   - env 在节点收口前构建 → 链式推进子测试红(b 的 user_data 未解析)。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/snowflake"
)

const testSchema = "braid_scheduler_test"

var (
	testIDsOnce sync.Once
	testIDs     *snowflake.Generator
)

func mustTestIDs(t *testing.T) *snowflake.Generator {
	t.Helper()
	testIDsOnce.Do(func() {
		g, err := snowflake.New(uint16(os.Getpid() & 0x3ff))
		if err != nil {
			t.Fatalf("test snowflake: %v", err)
		}
		testIDs = g
	})
	return testIDs
}

// newSched 返回调度器 + 连接(栈不可达自动 skip)。Queue 任意合法名:本包
// 不投队列,仅落 outbox 行。
func newSched(t *testing.T) (*Scheduler, *sql.DB) {
	t.Helper()
	d := sqltest.MustOpenAt(t, testSchema)
	s, err := New(Deps{DB: d, IDs: mustTestIDs(t), Queue: "sched-test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, d
}

// seedDef 落一行带版本的 definition(合法 DSL 文档)并注册清理。
func seedDef(t *testing.T, d *sql.DB, doc string) uint64 {
	t.Helper()
	defID, err := mustTestIDs(t).Next()
	if err != nil {
		t.Fatalf("gen def id: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', ?, ?, 'test', 'test', 'test', NOW(3))`,
		defID, fmt.Sprintf("wf-%d", defID), doc, string(make([]byte, 64))); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM workflow_definition WHERE id=?`, defID)
	})
	return defID
}

// inst 走生产入口物化(同事务含 execution.start 事件)并注册清理。
func inst(t *testing.T, s *Scheduler, d *sql.DB, doc string, input string) (defID, execID uint64) {
	t.Helper()
	defID = seedDef(t, d, doc)
	execID, err := s.Instantiate(context.Background(), InstantiateParams{
		DefinitionID: defID, Input: json.RawMessage(input), TriggeredBy: "test",
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	t.Cleanup(func() { cleanupExec(t, d, execID) })
	return defID, execID
}

func cleanupExec(t *testing.T, d *sql.DB, execID uint64) {
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
}

// nodeSnapshotRow 是断言用节点行快照。
type nodeSnapshotRow struct {
	id         uint64
	status     engine.NodeState
	version    uint32
	attempts   uint16
	maxA       uint16
	workerID   sql.NullString
	snapshot   sql.NullString
	output     sql.NullString
	deadlineOK bool
}

func readNode(t *testing.T, d *sql.DB, execID uint64, nodeID string) nodeSnapshotRow {
	t.Helper()
	var r nodeSnapshotRow
	var deadline sql.NullTime
	err := d.QueryRow(`
SELECT id, status, version, attempts, max_attempts, worker_id, input_snapshot, output, deadline_at
  FROM node_execution WHERE execution_id=? AND node_id=?`, execID, nodeID).
		Scan(&r.id, &r.status, &r.version, &r.attempts, &r.maxA, &r.workerID, &r.snapshot, &r.output, &deadline)
	if err != nil {
		t.Fatalf("read node %s: %v", nodeID, err)
	}
	r.deadlineOK = deadline.Valid
	return r
}

type execSnapshot struct {
	status    engine.WorkflowState
	outputs   sql.NullString
	errorCode sql.NullString
}

func readExec(t *testing.T, d *sql.DB, execID uint64) execSnapshot {
	t.Helper()
	var r execSnapshot
	err := d.QueryRow(`SELECT status, outputs, error_code FROM workflow_execution WHERE id=?`, execID).
		Scan(&r.status, &r.outputs, &r.errorCode)
	if err != nil {
		t.Fatalf("read execution: %v", err)
	}
	return r
}

// seedAttempt 落一行 RUNNING attempt(模拟 T2.3 worker 的 claimAttempt 产物),
// 返回 attempt id。幂等键/连接列与派发 TaskID 同值。
func seedAttempt(t *testing.T, d *sql.DB, execID, nodeExecID uint64, attemptNo uint16) uint64 {
	t.Helper()
	id, err := mustTestIDs(t).Next()
	if err != nil {
		t.Fatalf("gen attempt id: %v", err)
	}
	taskID := fmt.Sprintf("att:%d:%d", nodeExecID, attemptNo)
	if _, err := d.Exec(`INSERT INTO node_attempt
	  (id, node_execution_id, execution_id, attempt_no, worker_id, status,
	   idempotency_key, asynq_task_id, started_at)
	  VALUES (?,?,?,?,?,'RUNNING',?,?,NOW(3))`,
		id, nodeExecID, execID, attemptNo, "sched-test", taskID, taskID); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
	return id
}

// complete 是测试面的 Complete 快捷(成功/失败 + 输出)。
func complete(t *testing.T, s *Scheduler, execID, nodeExecID, attemptID uint64,
	status engine.AttemptState, output json.RawMessage) error {
	t.Helper()
	return s.Complete(context.Background(), CompletionRequest{
		ExecID: execID, NodeExecID: nodeExecID, AttemptID: attemptID,
		Status: status, Output: output, WorkerID: "sched-test",
	})
}

func mustComplete(t *testing.T, s *Scheduler, execID, nodeExecID, attemptID uint64,
	status engine.AttemptState, output json.RawMessage) {
	t.Helper()
	if err := complete(t, s, execID, nodeExecID, attemptID, status, output); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

// completeNode 按节点 id 完成其当前 attempt(测试语义:每个节点顺序完成)。
func completeNode(t *testing.T, s *Scheduler, d *sql.DB, execID uint64, nodeID string,
	status engine.AttemptState, output json.RawMessage) error {
	t.Helper()
	row := readNode(t, d, execID, nodeID)
	attemptID := seedAttempt(t, d, execID, row.id, row.attempts)
	return complete(t, s, execID, row.id, attemptID, status, output)
}

// taskEvents 返回该执行的全部 outbox task_id(顺序)。
func taskEvents(t *testing.T, d *sql.DB, execID uint64) []string {
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

func waitNodeStatus(t *testing.T, d *sql.DB, execID uint64, nodeID string, want engine.NodeState) nodeSnapshotRow {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last nodeSnapshotRow
	for time.Now().Before(deadline) {
		last = readNode(t, d, execID, nodeID)
		if last.status == want {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("node %s did not reach %s within 10s (last=%s)", nodeID, want, last.status)
	return last
}

func waitExecStatus(t *testing.T, d *sql.DB, execID uint64, want engine.WorkflowState) execSnapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last execSnapshot
	for time.Now().Before(deadline) {
		last = readExec(t, d, execID)
		if last.status == want {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("execution did not reach %s within 10s (last=%s)", want, last.status)
	return last
}

// ---- 测试用 DSL 文档(全部经 dsl.Parse 双层校验)----

const chainDSL = `{"dsl":"1.0","name":"chain-t","inputs":[{"name":"q","type":"string"}],
 "edges":[{"from":"a","to":"b"},{"from":"b","to":"c"}],
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"gen","user_data":[{"name":"q","ref":"{{inputs.q}}"}]},"outputs":{"text":"string"}},
  {"id":"b","type":"llm","prompt":{"template":"mid","user_data":[{"name":"v","ref":"{{nodes.a.output.text}}"}]},"outputs":{"text":"string"}},
  {"id":"c","type":"llm","prompt":{"template":"end","user_data":[{"name":"v","ref":"{{nodes.b.output.text}}"}]},"outputs":{"text":"string"}}],
 "outputs":{"final":"{{nodes.c.output.text}}"}}`

const diamondDSL = `{"dsl":"1.0","name":"diamond-t",
 "edges":[{"from":"a","to":"b"},{"from":"a","to":"c"},{"from":"b","to":"d"},{"from":"c","to":"d"}],
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"gen"},"outputs":{"x":"number"}},
  {"id":"b","type":"llm","prompt":{"template":"b","user_data":[{"name":"v","ref":"{{nodes.a.output.x}}"}]},"outputs":{"y":"number"}},
  {"id":"c","type":"llm","prompt":{"template":"c","user_data":[{"name":"v","ref":"{{nodes.a.output.x}}"}]},"outputs":{"y":"number"}},
  {"id":"d","type":"llm","prompt":{"template":"d","user_data":[{"name":"p","ref":"{{nodes.b.output.y}}"},{"name":"q","ref":"{{nodes.c.output.y}}"}]},"outputs":{"z":"number"}}],
 "outputs":{"sum":"{{nodes.d.output.z}}"}}`

const disjointDSL = `{"dsl":"1.0","name":"disjoint-t",
 "edges":[{"from":"e1","to":"x"},{"from":"e2","to":"y"},{"from":"y","to":"z"}],
 "nodes":[
  {"id":"e1","type":"llm","prompt":{"template":"1"},"outputs":{"t":"string"}},
  {"id":"e2","type":"llm","prompt":{"template":"2"},"outputs":{"t":"string"}},
  {"id":"x","type":"llm","prompt":{"template":"x","user_data":[{"name":"v","ref":"{{nodes.e1.output.t}}"}]},"outputs":{"t":"string"}},
  {"id":"y","type":"llm","prompt":{"template":"y","user_data":[{"name":"v","ref":"{{nodes.e2.output.t}}"}]},"outputs":{"t":"string"}},
  {"id":"z","type":"llm","prompt":{"template":"z","user_data":[{"name":"v","ref":"{{nodes.y.output.t}}"}]},"outputs":{"t":"string"}}]}`

const branchDSL = `{"dsl":"1.0","name":"branch-t",
 "edges":[{"from":"a","to":"b"},{"from":"b","to":"c"},{"from":"b","to":"d"},
          {"from":"c","to":"e"},{"from":"d","to":"e"},{"from":"d","to":"f"}],
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"gen"},"outputs":{"flag":"boolean","text":"string"}},
  {"id":"b","type":"branch","cases":[{"when":{"left":"{{nodes.a.output.flag}}","op":"eq","right":true},"next":"c"}],"default":"d"},
  {"id":"c","type":"llm","prompt":{"template":"c","user_data":[{"name":"v","ref":"{{nodes.a.output.text}}"}]},"outputs":{"text":"string"}},
  {"id":"d","type":"llm","prompt":{"template":"d"},"outputs":{"text":"string"}},
  {"id":"e","type":"llm","prompt":{"template":"e","user_data":[{"name":"v","ref":"{{nodes.c.output.text}}"}]},"outputs":{"text":"string"}},
  {"id":"f","type":"llm","prompt":{"template":"f","user_data":[{"name":"v","ref":"{{nodes.d.output.text}}"}]},"outputs":{"text":"string"}}],
 "outputs":{"taken":"{{nodes.c.output.text}}","cut":"{{nodes.d.output.text}}"}}`

const retryDSL = `{"dsl":"1.0","name":"retry-t",
 "nodes":[{"id":"a","type":"llm","retry":{"max_attempts":2},"prompt":{"template":"x"},"outputs":{"text":"string"}}]}`

// evalFailDSL:a 声明 text 但运行期 output 不含它(执行期数据缺陷,Parse 不可见)。
const evalFailDSL = `{"dsl":"1.0","name":"evalfail-t",
 "edges":[{"from":"a","to":"b"}],
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"gen"},"outputs":{"text":"string"}},
  {"id":"b","type":"llm","prompt":{"template":"b","user_data":[{"name":"v","ref":"{{nodes.a.output.text}}"}]},"outputs":{"text":"string"}}]}`

// branchErrDSL:a 声明 flag:boolean 但运行期产出字符串(桩路由类型错误)。
const branchErrDSL = `{"dsl":"1.0","name":"brancherr-t",
 "edges":[{"from":"a","to":"b"},{"from":"b","to":"c"},{"from":"b","to":"d"}],
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"gen"},"outputs":{"flag":"boolean"}},
  {"id":"b","type":"branch","cases":[{"when":{"left":"{{nodes.a.output.flag}}","op":"eq","right":true},"next":"c"}],"default":"d"},
  {"id":"c","type":"llm","prompt":{"template":"c"},"outputs":{"text":"string"}},
  {"id":"d","type":"llm","prompt":{"template":"d"},"outputs":{"text":"string"}}]}`

// ---- 集成测试 ----

func TestInstantiateAndStart(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, chainDSL, `{"q":"hi"}`)

	// 物化:PENDING 执行 + 全部节点行 + exec: 事件(同事务)。
	e := readExec(t, d, execID)
	if e.status != engine.WorkflowPending {
		t.Fatalf("execution status = %s, want PENDING", e.status)
	}
	events := taskEvents(t, d, execID)
	if len(events) != 1 || events[0] != fmt.Sprintf("exec:%d", execID) {
		t.Fatalf("outbox events = %v, want [exec:%d]", events, execID)
	}
	for _, id := range []string{"a", "b", "c"} {
		r := readNode(t, d, execID, id)
		if r.status != engine.NodePending || r.version != 1 || r.attempts != 0 {
			t.Fatalf("node %s = %+v, want PENDING/v1/attempts0", id, r)
		}
	}

	// Start:RUNNING + 入口 a 派发(EXECUTING、attempts=1、deadline、快照、att 事件)。
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if e := readExec(t, d, execID); e.status != engine.WorkflowRunning {
		t.Fatalf("execution status = %s, want RUNNING", e.status)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	if a.attempts != 1 || !a.deadlineOK || a.workerID.String != "sched-test" {
		t.Fatalf("node a = %+v, want claimed(attempts1/deadline/worker)", a)
	}
	var snap map[string]any
	if err := json.Unmarshal([]byte(a.snapshot.String), &snap); err != nil {
		t.Fatalf("a snapshot: %v", err)
	}
	ud := snap["user_data"].([]any)[0].(map[string]any)
	if ud["value"] != "hi" {
		t.Fatalf("a snapshot user_data = %v(input 未求值)", ud)
	}
	events = taskEvents(t, d, execID)
	want := fmt.Sprintf("att:%d:1", a.id)
	if len(events) != 2 || events[1] != want {
		t.Fatalf("outbox events = %v, want +[%s]", events, want)
	}
	var payload NodeTaskPayload
	if err := json.Unmarshal(mustPayload(t, d, events[1]), &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.ExecutionID != execID || payload.NodeExecutionID != a.id || payload.AttemptNo != 1 {
		t.Fatalf("payload = %+v", payload)
	}

	// Start 幂等(双投递 exec:):不重复派发。
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if a2 := readNode(t, d, execID, "a"); a2.attempts != 1 {
		t.Fatalf("node a attempts = %d after double Start, want 1", a2.attempts)
	}
}

func mustPayload(t *testing.T, d *sql.DB, taskID string) []byte {
	t.Helper()
	var raw []byte
	if err := d.QueryRow(`SELECT payload FROM outbox WHERE task_id=? ORDER BY id DESC LIMIT 1`, taskID).
		Scan(&raw); err != nil {
		t.Fatalf("read payload %s: %v", taskID, err)
	}
	return raw
}

func TestCompleteAdvancesChain(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, chainDSL, `{"q":"hi"}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	attemptA := seedAttempt(t, d, execID, a.id, 1)
	mustComplete(t, s, execID, a.id, attemptA, engine.AttemptSucceed, json.RawMessage(`{"text":"第一跳"}`))

	aDone := readNode(t, d, execID, "a")
	if aDone.status != engine.NodeSucceeded || aDone.output.String != `{"text": "第一跳"}` {
		// MySQL JSON 列规范化(键序/空白),语义比较即可。
		var got, want map[string]any
		_ = json.Unmarshal([]byte(aDone.output.String), &got)
		_ = json.Unmarshal([]byte(`{"text":"第一跳"}`), &want)
		if aDone.status != engine.NodeSucceeded || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("node a = %+v output=%s", aDone, aDone.output.String)
		}
	}

	// b 被推进:EXECUTING + 快照含前驱 output 解析值。
	b := waitNodeStatus(t, d, execID, "b", engine.NodeExecuting)
	if b.attempts != 1 {
		t.Fatalf("b attempts = %d, want 1", b.attempts)
	}
	var bsnap map[string]any
	if err := json.Unmarshal([]byte(b.snapshot.String), &bsnap); err != nil {
		t.Fatalf("b snapshot: %v", err)
	}
	if ud := bsnap["user_data"].([]any)[0].(map[string]any); ud["value"] != "第一跳" {
		t.Fatalf("b snapshot user_data = %v(前驱 output 未入 env)", ud)
	}

	attemptB := seedAttempt(t, d, execID, b.id, 1)
	mustComplete(t, s, execID, b.id, attemptB, engine.AttemptSucceed, json.RawMessage(`{"text":"第二跳"}`))
	c := waitNodeStatus(t, d, execID, "c", engine.NodeExecuting)
	attemptC := seedAttempt(t, d, execID, c.id, 1)
	mustComplete(t, s, execID, c.id, attemptC, engine.AttemptSucceed, json.RawMessage(`{"text":"终值"}`))

	// 执行终态回写:SUCCEEDED + outputs 物化(提交后新事务,轮询)。
	e := waitExecStatus(t, d, execID, engine.WorkflowSucceeded)
	var outs map[string]any
	if err := json.Unmarshal([]byte(e.outputs.String), &outs); err != nil {
		t.Fatalf("outputs: %v", err)
	}
	if outs["final"] != "终值" {
		t.Fatalf("outputs = %v", outs)
	}
}

// TestConcurrentMultiPredDiamond 是验收核心:两前驱并发完成,汇合节点恰派发一次。
func TestConcurrentMultiPredDiamond(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, diamondDSL, `{}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	attemptA := seedAttempt(t, d, execID, a.id, 1)
	mustComplete(t, s, execID, a.id, attemptA, engine.AttemptSucceed, json.RawMessage(`{"x":1}`))
	b := waitNodeStatus(t, d, execID, "b", engine.NodeExecuting)
	c := waitNodeStatus(t, d, execID, "c", engine.NodeExecuting)

	// 并发完成 b 与 c(真事务并发;任何死锁未收敛都会以错误浮出)。
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, n := range []struct {
		row    nodeSnapshotRow
		output string
	}{{b, `{"y":1}`}, {c, `{"y":2}`}} {
		wg.Add(1)
		go func(n struct {
			row    nodeSnapshotRow
			output string
		}) {
			defer wg.Done()
			attempt := seedAttempt(t, d, execID, n.row.id, 1)
			errs <- complete(t, s, execID, n.row.id, attempt, engine.AttemptSucceed, json.RawMessage(n.output))
		}(n)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Complete: %v(死锁/竞态未收敛)", err)
		}
	}

	// d 恰派发一次:attempts=1 且 att 事件恰一条。
	dd := waitNodeStatus(t, d, execID, "d", engine.NodeExecuting)
	if dd.attempts != 1 {
		t.Fatalf("d attempts = %d, want 1(双派发即红)", dd.attempts)
	}
	count := 0
	for _, e := range taskEvents(t, d, execID) {
		if e == fmt.Sprintf("att:%d:1", dd.id) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("d att 事件 = %d, want 1", count)
	}
	// d 的快照引用了两个前驱的 output(多前驱数据汇合)。
	var dsnap map[string]any
	if err := json.Unmarshal([]byte(dd.snapshot.String), &dsnap); err != nil {
		t.Fatalf("d snapshot: %v", err)
	}
	uds := dsnap["user_data"].([]any)
	if uds[0].(map[string]any)["value"].(float64) != 1 || uds[1].(map[string]any)["value"].(float64) != 2 {
		t.Fatalf("d snapshot user_data = %v(双前驱输出应齐全)", uds)
	}
}

// TestConcurrentDisjointArmsFinish 回归 P1:不相交节点集并发完成,执行终态
// 不漏判(事务内判定会因各自快照见对方在飞而双双跳过)。
func TestConcurrentDisjointArmsFinish(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, disjointDSL, `{}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	e1 := waitNodeStatus(t, d, execID, "e1", engine.NodeExecuting)
	e2 := waitNodeStatus(t, d, execID, "e2", engine.NodeExecuting)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, n := range []nodeSnapshotRow{e1, e2} {
		wg.Add(1)
		go func(n nodeSnapshotRow) {
			defer wg.Done()
			attempt := seedAttempt(t, d, execID, n.id, 1)
			errs <- complete(t, s, execID, n.id, attempt, engine.AttemptSucceed, json.RawMessage(`{"t":"v"}`))
		}(n)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Complete: %v", err)
		}
	}
	// x/y/z 随后正常推进到终态(测试侧顺序完成剩余节点)。
	for _, id := range []string{"x", "y", "z"} {
		r := waitNodeStatus(t, d, execID, id, engine.NodeExecuting)
		if err := completeNode(t, s, d, execID, id, engine.AttemptSucceed, json.RawMessage(`{"t":"v"}`)); err != nil {
			t.Fatalf("complete %s: %v", id, err)
		}
		_ = r
	}
	waitExecStatus(t, d, execID, engine.WorkflowSucceeded)
}

// TestBranchRoutingAndSkip:取路臂派发、未取臂剪枝、传递跳过、臂汇合节点恒剪枝、
// outputs 引用剪枝臂 → null。
func TestBranchRoutingAndSkip(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, branchDSL, `{}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	attemptA := seedAttempt(t, d, execID, a.id, 1)
	// flag=true → 路由 c;d 剪枝;e(汇合 c/d)恒剪枝;f(d 下游)传递剪枝。
	mustComplete(t, s, execID, a.id, attemptA, engine.AttemptSucceed, json.RawMessage(`{"flag":true,"text":"路上"}`))

	b := readNode(t, d, execID, "b")
	if b.status != engine.NodeSucceeded || b.attempts != 1 {
		t.Fatalf("branch b = %+v, want SUCCEEDED/attempts1(内联求值)", b)
	}
	if b.output.Valid {
		t.Fatalf("branch output = %s, want NULL(DDL:branch 恒 NULL)", b.output.String)
	}
	if b.snapshot.Valid {
		var op map[string]any
		if err := json.Unmarshal([]byte(b.snapshot.String), &op); err != nil || len(op["operands"].(map[string]any)) != 1 {
			t.Fatalf("branch snapshot = %s, want operands 单键(flag)", b.snapshot.String)
		}
	}
	c := waitNodeStatus(t, d, execID, "c", engine.NodeExecuting)
	if got := readNode(t, d, execID, "d").status; got != engine.NodeSkipped {
		t.Fatalf("cut arm d = %s, want SKIPPED", got)
	}
	for _, id := range []string{"e", "f"} {
		if got := readNode(t, d, execID, id).status; got != engine.NodeSkipped {
			t.Fatalf("downstream %s = %s, want SKIPPED(传递剪枝/臂不可汇合)", id, got)
		}
	}
	_ = c

	// c 完成 → 执行 SUCCEEDED;outputs:taken=值、cut=null。
	attemptC := seedAttempt(t, d, execID, c.id, 1)
	mustComplete(t, s, execID, c.id, attemptC, engine.AttemptSucceed, json.RawMessage(`{"text":"取路值"}`))
	e := waitExecStatus(t, d, execID, engine.WorkflowSucceeded)
	var outs map[string]any
	if err := json.Unmarshal([]byte(e.outputs.String), &outs); err != nil {
		t.Fatalf("outputs: %v", err)
	}
	if outs["taken"] != "取路值" {
		t.Fatalf("outputs[taken] = %v", outs["taken"])
	}
	if v, ok := outs["cut"]; !ok || v != nil {
		t.Fatalf("outputs[cut] = %#v(ok=%v), want 显式 null(键保留)", v, ok)
	}
}

// TestBranchDefaultRoute:flag=false → default 臸 d 派发、c 剪枝。
func TestBranchDefaultRoute(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, branchDSL, `{}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	attemptA := seedAttempt(t, d, execID, a.id, 1)
	mustComplete(t, s, execID, a.id, attemptA, engine.AttemptSucceed, json.RawMessage(`{"flag":false,"text":"默认"}`))

	waitNodeStatus(t, d, execID, "d", engine.NodeExecuting)
	if got := readNode(t, d, execID, "c").status; got != engine.NodeSkipped {
		t.Fatalf("cut arm c = %s, want SKIPPED", got)
	}
}

func TestRetryRedispatchThenSucceed(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, retryDSL, `{}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)

	// attempt 1 业务失败:max_attempts=2 > attempts=1 → 再派发,节点留 EXECUTING。
	attempt1 := seedAttempt(t, d, execID, a.id, 1)
	mustComplete(t, s, execID, a.id, attempt1, engine.AttemptFailed, nil)
	a2 := readNode(t, d, execID, "a")
	if a2.status != engine.NodeExecuting || a2.attempts != 2 {
		t.Fatalf("node a = %+v, want EXECUTING/attempts2(平台重试)", a2)
	}
	events := taskEvents(t, d, execID)
	want := fmt.Sprintf("att:%d:2", a.id)
	found := false
	for _, e := range events {
		if e == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("outbox events = %v, want 含 %s(重试新 attempt = 新 TaskID)", events, want)
	}
	if e := readExec(t, d, execID); e.status != engine.WorkflowRunning {
		t.Fatalf("execution = %s, want RUNNING(重试不是失败)", e.status)
	}

	// attempt 2 成功 → 节点/执行终态。
	attempt2 := seedAttempt(t, d, execID, a.id, 2)
	mustComplete(t, s, execID, a.id, attempt2, engine.AttemptSucceed, json.RawMessage(`{"text":"ok"}`))
	waitExecStatus(t, d, execID, engine.WorkflowSucceeded)
}

func TestRetryExhaustedFailsFast(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, retryDSL, `{}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	attempt1 := seedAttempt(t, d, execID, a.id, 1)
	mustComplete(t, s, execID, a.id, attempt1, engine.AttemptFailed, nil)
	a2 := readNode(t, d, execID, "a")

	attempt2 := seedAttempt(t, d, execID, a2.id, 2)
	mustComplete(t, s, execID, a2.id, attempt2, engine.AttemptFailed, nil)

	if got := readNode(t, d, execID, "a").status; got != engine.NodeFailed {
		t.Fatalf("node a = %s, want FAILED(耗尽)", got)
	}
	e := readExec(t, d, execID)
	if e.status != engine.WorkflowFailed || !e.errorCode.Valid || e.errorCode.String != "node_failed" {
		t.Fatalf("execution = %+v, want FAILED/node_failed(fail-fast)", e)
	}
}

func TestEvalFailureFailsExecution(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, evalFailDSL, `{}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	attemptA := seedAttempt(t, d, execID, a.id, 1)
	// a 声明了 text 但产出缺该字段 → b 求值失败 → 执行级 input_eval,b 留 PENDING。
	mustComplete(t, s, execID, a.id, attemptA, engine.AttemptSucceed, json.RawMessage(`{"other":1}`))

	e := readExec(t, d, execID)
	if e.status != engine.WorkflowFailed || !e.errorCode.Valid || e.errorCode.String != "input_eval" {
		t.Fatalf("execution = %+v, want FAILED/input_eval", e)
	}
	if got := readNode(t, d, execID, "b").status; got != engine.NodePending {
		t.Fatalf("node b = %s, want PENDING(不可派发即不 claim)", got)
	}
}

func TestBranchRouterTypeError(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, branchErrDSL, `{}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	attemptA := seedAttempt(t, d, execID, a.id, 1)
	// flag 声明 boolean 但产出字符串 → 桩路由类型错误。
	mustComplete(t, s, execID, a.id, attemptA, engine.AttemptSucceed, json.RawMessage(`{"flag":"yes"}`))

	e := readExec(t, d, execID)
	if e.status != engine.WorkflowFailed || !e.errorCode.Valid || e.errorCode.String != "branch_eval" {
		t.Fatalf("execution = %+v, want FAILED/branch_eval", e)
	}
	// branch 留 PENDING(路由先于 claim,不留不可扫描 EXECUTING 行)。
	for _, id := range []string{"b", "c", "d"} {
		if got := readNode(t, d, execID, id).status; got != engine.NodePending {
			t.Fatalf("node %s = %s, want PENDING", id, got)
		}
	}
}

func TestCompleteIdempotentReplay(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, chainDSL, `{"q":"hi"}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	attemptA := seedAttempt(t, d, execID, a.id, 1)
	mustComplete(t, s, execID, a.id, attemptA, engine.AttemptSucceed, json.RawMessage(`{"text":"v"}`))
	b := waitNodeStatus(t, d, execID, "b", engine.NodeExecuting)

	// 同一 attempt 重放:幂等返回,不重复推进。
	if err := complete(t, s, execID, a.id, attemptA, engine.AttemptSucceed, json.RawMessage(`{"text":"v"}`)); err != nil {
		t.Fatalf("replay Complete: %v(应幂等 nil)", err)
	}
	b2 := readNode(t, d, execID, "b")
	if b2.attempts != 1 || b2.version != b.version {
		t.Fatalf("node b = %+v(重放引发重复派发)", b2)
	}
}

func TestTerminalGuardStopsAdvancement(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, disjointDSL, `{}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	e1 := waitNodeStatus(t, d, execID, "e1", engine.NodeExecuting)
	e2 := waitNodeStatus(t, d, execID, "e2", engine.NodeExecuting)

	// e1 耗尽失败(max=1)→ fail-fast;e2 迟到成功:节点收口但不推进 z。
	attemptE1 := seedAttempt(t, d, execID, e1.id, 1)
	mustComplete(t, s, execID, e1.id, attemptE1, engine.AttemptFailed, nil)
	waitExecStatus(t, d, execID, engine.WorkflowFailed)

	attemptE2 := seedAttempt(t, d, execID, e2.id, 1)
	mustComplete(t, s, execID, e2.id, attemptE2, engine.AttemptSucceed, json.RawMessage(`{"t":"v"}`))
	if got := readNode(t, d, execID, "y").status; got != engine.NodePending {
		t.Fatalf("node y = %s, want PENDING(终态守卫阻断派发)", got)
	}
	if got := readNode(t, d, execID, "e2").status; got != engine.NodeSucceeded {
		t.Fatalf("node e2 = %s, want SUCCEEDED(迟到收口仍执行)", got)
	}
}

// TestInstantiateRejectsNonPublishedAndBadInput:review M1/M2 回归——ARCHIVED
// 定义禁新触发(DDL 生命周期契约);非对象输入在触发时拒绝(不放行到
// Start 才确定性失败)。
func TestInstantiateRejectsNonPublishedAndBadInput(t *testing.T) {
	s, d := newSched(t)
	seed := func(status string, version any) uint64 {
		t.Helper()
		defID, err := mustTestIDs(t).Next()
		if err != nil {
			t.Fatalf("gen def id: %v", err)
		}
		if _, err := d.Exec(`INSERT INTO workflow_definition
		  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
		  VALUES (?, ?, ?, ?, '1.0', ?, ?, 'test', 'test', 'test', NOW(3))`,
			defID, fmt.Sprintf("wf-%d", defID), version, status,
			`{"dsl":"1.0","name":"x","nodes":[{"id":"a","type":"llm","prompt":{"template":"t"}}]}`,
			string(make([]byte, 64))); err != nil {
			t.Fatalf("seed: %v", err)
		}
		t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM workflow_definition WHERE id=?`, defID) })
		return defID
	}
	tests := []struct {
		name  string
		defID uint64
		input string
	}{
		{"archived", seed("ARCHIVED", 1), `{}`},
		{"draft_null_version", seed("DRAFT", nil), `{}`},
		{"array_input", seed("PUBLISHED", 1), `[1,2]`},
		{"scalar_input", seed("PUBLISHED", 1), `"str"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.Instantiate(context.Background(), InstantiateParams{
				DefinitionID: tt.defID, Input: json.RawMessage(tt.input), TriggeredBy: "test",
			})
			if err == nil {
				t.Fatal("Instantiate 应拒绝(非 PUBLISHED / 非对象输入)")
			}
		})
	}
}

// warnSink 是最小 slog Handler:并发安全地收集 Warn 级消息(W1 断言用)。
type warnSink struct {
	mu     sync.Mutex
	events []string
}

func (w *warnSink) Enabled(_ context.Context, l slog.Level) bool { return l == slog.LevelWarn }
func (w *warnSink) Handle(_ context.Context, r slog.Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, r.Message)
	return nil
}
func (w *warnSink) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *warnSink) WithGroup(string) slog.Handler      { return w }

// TestCompleteDiscardedByReaper:W1 哨兵 pin(D-2026-09-22-t25-reaper-semantics
// #7「接受等价」)——reaper 判 DEAD + Redispatch 后,老 worker 的迟到 Complete
// 撞 DEAD attempt:幂等返回 nil、节点/推进不被二次改写、落 discarded_by_reaper
// 结构化 warn(判死双执行的观测信号)。
//
// 变异自查(实现错 → 哪条红):
//   - 哨兵区分来源后不再 return nil → 迟到 Complete 返回 err;
//   - Observed==DEAD 不 warn → 日志断言红;
//   - 哨兵放行推进 → 节点 a 被 SucceedNode 收口/执行误判终态(行断言红)。
func TestCompleteDiscardedByReaper(t *testing.T) {
	s, d := newSched(t)
	sink := &warnSink{}
	s.log = slog.New(sink)
	_, execID := inst(t, s, d, chainDSL, `{"q":"hi"}`)
	if err := s.Start(context.Background(), execID, "sched-test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := waitNodeStatus(t, d, execID, "a", engine.NodeExecuting)
	attemptA := seedAttempt(t, d, execID, a.id, 1)

	// 模拟 reaper 裁决事务:DEAD 收口 + RedispatchNode(新 attempt 号 2 待新 claim)。
	if err := engine.WithTx(context.Background(), d, func(q engine.DBTX) error {
		if err := engine.CloseAttemptDead(context.Background(), q, attemptA, "heartbeat_lost", "stale"); err != nil {
			return err
		}
		_, err := engine.RedispatchNode(context.Background(), q, a.id, a.version, engine.RedispatchInput{
			WorkerID:   "reaper:test",
			DeadlineAt: time.Now().Add(NodeTimeout("llm")),
		})
		return err
	}); err != nil {
		t.Fatalf("reaper adjudication: %v", err)
	}

	// 迟到 Complete:哨兵幂等 nil。
	if err := complete(t, s, execID, a.id, attemptA, engine.AttemptSucceed, json.RawMessage(`{"text":"late"}`)); err != nil {
		t.Fatalf("迟到 Complete 应幂等 nil,got %v", err)
	}
	// 节点不被迟到结果收口:仍 EXECUTING、attempts=2(重派链路权威)。
	a2 := readNode(t, d, execID, "a")
	if a2.status != engine.NodeExecuting || a2.attempts != 2 {
		t.Fatalf("node a = %s/attempts=%d, want EXECUTING/2(迟到推进被哨兵阻断)", a2.status, a2.attempts)
	}
	// b 不被派发(推进让位),执行仍 RUNNING。
	if got := readNode(t, d, execID, "b").status; got != engine.NodePending {
		t.Fatalf("node b = %s, want PENDING", got)
	}
	if got := readExec(t, d, execID).status; got != engine.WorkflowRunning {
		t.Fatalf("execution = %s, want RUNNING", got)
	}
	sink.mu.Lock()
	found := false
	for _, e := range sink.events {
		if e == "complete.discarded_by_reaper" {
			found = true
		}
	}
	sink.mu.Unlock()
	if !found {
		t.Fatalf("缺 discarded_by_reaper warn(判死双执行观测信号),got %v", sink.events)
	}
}
