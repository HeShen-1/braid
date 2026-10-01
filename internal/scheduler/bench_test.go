package scheduler

// T15.5 压测基线(全仓首个 benchmark 面;D-2026-10-01-t155-bench-baseline):
// 引擎核心两族——scheduler 吞吐(Instantiate 物化 / Complete 收口)与大
// DAG 回滚锁窗(RollbackTo 墙时 = 锁窗上界;前置仅 loadExecution/
// loadWorkflow 两点查,提交后 finishIfDone 独立事务,口径如实)。
// 本包零 Redis(队列往返面归 internal/worker bench);栈不可达自动 skip。
// 复跑口径见 docs/CAPACITY.md:
//
//	go test ./internal/scheduler/ -bench . -benchtime 5x -run '^$' -timeout 2h
//
// 口径注记:
//   - 执行器成本不在测量面(Complete 直调,无 asynq/worker 往返);Complete
//     档的 attempt 行为种子预插(计时圈只含 Complete 事务 = 生产 worker 收
//     口事务,claim 成本另见 worker bench);
//   - 大 DAG 网格用 fixture 直插终态行(与 seedDef 同类的构造式 INSERT,
//     非 UPDATE 状态迁移):真路径种子的每笔 Complete 都全行锁(D-t24#6),
//     10k 档种子成本平方级不可行;TestRollbackFixtureEquivalence 在 ~300
//     行档对拍真路径与 fixture 的回滚结果,钉死 fixture 与生产可达形态
//     不漂移;
//   - 真路径 FAILED 种子序 = 全实例先 SUCCEEDED、fan_001 末实例终败收口
//     (wait_all 语义:控制器在末实例收口事务收敛 FAILED,零 EXECUTING
//     残留 = 回滚守卫②可过;先失败后补完会撞终态守卫跳过 gather 的形状)。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
)

// benchChainDoc 生成 n 节点线性 llm 链 DSL(节点 id n001..,字典序 = 链序)。
func benchChainDoc(n int) string {
	var b strings.Builder
	b.WriteString(`{"dsl":"1.0","name":"bench-chain","nodes":[`)
	for i := 1; i <= n; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":"n%03d","type":"llm","prompt":{"template":"t"`, i)
		if i > 1 {
			fmt.Fprintf(&b, `,"user_data":[{"name":"v","ref":"{{nodes.n%03d.output.text}}"}]`, i-1)
		}
		b.WriteString(`},"outputs":{"text":"string"}}`)
	}
	b.WriteString(`],"edges":[`)
	for i := 1; i < n; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"from":"n%03d","to":"n%03d"}`, i, i+1)
	}
	fmt.Fprintf(&b, `],"outputs":{"done":"{{nodes.n%03d.output.text}}"}}`, n)
	return b.String()
}

// benchFanGridDoc 生成 pre → fan_001..fan_k(star)→ post 的 fan-out DSL:
// 每 fan = parallel foreach {{inputs.a<i>}}(wait_all)+ llm body。行集 =
// 1(pre)+ k×(1+per)(fan 控制+实例)+ 1(post)。
func benchFanGridDoc(k, per int) string {
	var inputs, nodes, edges strings.Builder
	for i := 1; i <= k; i++ {
		if i > 1 {
			inputs.WriteByte(',')
		}
		fmt.Fprintf(&inputs, `{"name":"a%03d","type":"array","required":true}`, i)
	}
	nodes.WriteString(`{"id":"pre","type":"llm","prompt":{"template":"t"},"outputs":{"text":"string"}}`)
	for i := 1; i <= k; i++ {
		fmt.Fprintf(&nodes, `,{"id":"fan_%03d","type":"parallel","parallel":{"foreach":"{{inputs.a%03d}}","on_failure":"wait_all",`+
			`"body":{"type":"llm","prompt":{"template":"评审","user_data":[{"name":"review","ref":"{{item}}"},`+
			`{"name":"ctx","ref":"{{nodes.pre.output.text}}"}]},"outputs":{"verdict":"string"}}}}`, i, i)
	}
	nodes.WriteString(`,{"id":"post","type":"tool","tool":"http","inputs":{"payload":"{{nodes.fan_001.output.results}}"},"outputs":{"done":"boolean"}}`)
	edges.WriteString(`{"from":"pre","to":"fan_001"}`)
	for i := 2; i <= k; i++ {
		fmt.Fprintf(&edges, `,{"from":"pre","to":"fan_%03d"}`, i)
	}
	for i := 1; i <= k; i++ {
		fmt.Fprintf(&edges, `,{"from":"fan_%03d","to":"post"}`, i)
	}
	return fmt.Sprintf(`{"dsl":"1.0","name":"bench-fan","inputs":[%s],"nodes":[%s],"edges":[%s],`+
		`"outputs":{"all":"{{nodes.fan_001.output.results}}"}}`, inputs.String(), nodes.String(), edges.String())
}

// benchFanInput 生成与 fan 网格一致的触发入参(k 个 per 长数组)。
func benchFanInput(k, per int) json.RawMessage {
	var b strings.Builder
	b.WriteByte('{')
	for i := 1; i <= k; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"a%03d":[`, i)
		for j := 0; j < per; j++ {
			if j > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"s%d"`, j)
		}
		b.WriteByte(']')
	}
	b.WriteByte('}')
	return json.RawMessage(b.String())
}

// fanGridShape 把目标行数解析为 (k, per):≤1000 行单 fan 自恰(per =
// target-3);更大档 per 钉 997(≤1000 上限留余量)、k = 覆盖目标的最小
// fan 数(≤32;512 节点帽内 star 形状 k+2 ≤ 34 节点)。实际行数由
// fanGridRows 计算,网格档位报实际值。
func fanGridShape(target int) (k, per int) {
	if target <= 1000 {
		return 1, target - 3 // rows = 2 + (per+1)
	}
	per = 997
	k = (target - 2 + per) / (per + 1)
	if k > 32 {
		k = 32
	}
	return k, per
}

func fanGridRows(k, per int) int { return 2 + k*(per+1) }

func newBenchSched(tb testing.TB) (*Scheduler, *sql.DB) {
	tb.Helper()
	d := sqltest.MustOpenAt(tb, testSchema)
	// 上轮 Fatalf 的孤儿清扫(bench 段位 triggered_by='bench';测试行
	// TriggeredBy='test' 不受触)。滞留行会抬升共享 schema 的行计数与
	// 后续档位的锁面。
	for _, q := range []string{
		`DELETE FROM audit_log WHERE execution_id IN (SELECT id FROM workflow_execution WHERE triggered_by='bench')`,
		`DELETE FROM outbox WHERE execution_id IN (SELECT id FROM workflow_execution WHERE triggered_by='bench')`,
		`DELETE FROM node_attempt WHERE execution_id IN (SELECT id FROM workflow_execution WHERE triggered_by='bench')`,
		`DELETE FROM audit_pending WHERE execution_id IN (SELECT id FROM workflow_execution WHERE triggered_by='bench')`,
		`DELETE FROM approval_decision WHERE execution_id IN (SELECT id FROM workflow_execution WHERE triggered_by='bench')`,
		`DELETE FROM node_execution WHERE execution_id IN (SELECT id FROM workflow_execution WHERE triggered_by='bench')`,
		`DELETE FROM workflow_execution WHERE triggered_by='bench'`,
	} {
		if _, err := d.Exec(q); err != nil {
			tb.Fatalf("bench orphan sweep: %v", err)
		}
	}
	// 丢弃日志:调度器 INFO(fanout/instantiated)会与 go bench 的结果行
	// 物理交错,污染 ns/op 读数——基准的观测噪音面零价值,直接消源。
	s, err := New(Deps{DB: d, IDs: mustTestIDs(tb), Queue: "bench",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		tb.Fatalf("New: %v", err)
	}
	return s, d
}

// seedFailedFanFixture 直插一把 FAILED 终态执行(构造式 fixture,与 seedDef
// 同类;零 UPDATE——行按目标态一次成型):pre SUCCEEDED、fan_001 控制行
// FAILED + 其实例 1 FAILED(其余全 SUCCEEDED)、其余 fan 全 SUCCEEDED、
// post PENDING 冻结 = wait_all 真路径可达终态的同构像。
func seedFailedFanFixture(tb testing.TB, d *sql.DB, defID uint64, k, per int, input json.RawMessage) uint64 {
	tb.Helper()
	ids := mustTestIDs(tb)
	execID, err := ids.Next()
	if err != nil {
		tb.Fatalf("gen exec id: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO workflow_execution
	  (id, definition_id, definition_name, definition_version, definition_hash,
	   status, trigger_type, triggered_by, input, error_code, started_at, finished_at)
	  VALUES (?,?,?,1,?, 'FAILED','TEST','bench',?,'node_failed',NOW(3),NOW(3))`,
		execID, defID, "bench-fan", string(make([]byte, 64)), string(input)); err != nil {
		tb.Fatalf("seed execution: %v", err)
	}

	type fanRow struct {
		id     uint64
		nodeID string
		ino    int
		typ    string
		status string
		att    int
		worker sql.NullString
		snap   sql.NullString
		out    sql.NullString
		ecode  sql.NullString
		termTS bool // 终态行补 started_at/finished_at(真路径永不留 NULL)
	}
	var rows []fanRow
	mk := func(nodeID string, ino int, typ, status string, att int, worker, snap, out, ecode sql.NullString) {
		id, err := ids.Next()
		if err != nil {
			tb.Fatalf("gen node id: %v", err)
		}
		rows = append(rows, fanRow{id, nodeID, ino, typ, status, att, worker, snap, out, ecode,
			status != "PENDING"})
	}
	succ := sql.NullString{String: `{"text":"ctx-out"}`, Valid: true}
	verdict := sql.NullString{String: `{"verdict":"v"}`, Valid: true}
	results := sql.NullString{String: `{"results":["v"]}`, Valid: true}
	snapUD := sql.NullString{String: `{"ud":"s0"}`, Valid: true}
	w := sql.NullString{String: "bench", Valid: true}
	null := sql.NullString{}
	// 失败码对齐真路径:实例终败 = FailNode 缺省 attempts_exhausted;
	// 控制行 gather 失败 = parallel_instances_failed(failExecution 同源)。
	instFailed := sql.NullString{String: "attempts_exhausted", Valid: true}
	ctrlFailed := sql.NullString{String: "parallel_instances_failed", Valid: true}
	mk("pre", 0, "llm", "SUCCEEDED", 1, w, snapUD, succ, null)
	for i := 1; i <= k; i++ {
		fan := fmt.Sprintf("fan_%03d", i)
		// 控制行无 attempt/worker(gather 锚);post 冻结未跑同判。
		if i == 1 {
			mk(fan, 0, "parallel", "FAILED", 0, null, null, null, ctrlFailed)
		} else {
			mk(fan, 0, "parallel", "SUCCEEDED", 0, null, null, results, null)
		}
		for j := 1; j <= per; j++ {
			if i == 1 && j == 1 {
				mk(fan, j, "llm", "FAILED", 1, w, snapUD, null, instFailed)
			} else {
				mk(fan, j, "llm", "SUCCEEDED", 1, w, snapUD, verdict, null)
			}
		}
	}
	mk("post", 0, "tool", "PENDING", 0, null, null, null, null)

	const batchSize = 500
	for start := 0; start < len(rows); start += batchSize {
		end := min(start+batchSize, len(rows))
		var vb strings.Builder
		args := make([]any, 0, (end-start)*14)
		for _, r := range rows[start:end] {
			vb.WriteString("(?,?,?,?,?,?,?,?,?,?,?,?,IF(?,NOW(3),NULL),IF(?,NOW(3),NULL)),")
			args = append(args, r.id, execID, r.nodeID, r.ino, r.typ, r.status, r.att, 1,
				r.worker, r.snap, r.out, r.ecode, r.termTS, r.termTS)
		}
		vbStr := vb.String()
		if _, err := d.Exec(`INSERT INTO node_execution
		  (id, execution_id, node_id, instance_no, type, status, attempts, max_attempts,
		   worker_id, input_snapshot, output, error_code, started_at, finished_at)
		  VALUES `+vbStr[:len(vbStr)-1], args...); err != nil {
			tb.Fatalf("seed node rows: %v", err)
		}
	}
	return execID
}

// driveFanExecToFailed 真路径把 fan 网格执行推进到 FAILED(种子成本 O(N)
// Complete——见文件头口径注记):pre 成功 → 全实例 SUCCEEDED(fan_001
// 实例 1 除外)→ fan_001 实例 1 终败(wait_all:末实例收口收敛控制器
// FAILED + fail-fast wf FAILED)。终态 = 零 EXECUTING 残留。
func driveFanExecToFailed(tb testing.TB, s *Scheduler, d *sql.DB, defID uint64,
	input json.RawMessage, k, per int) uint64 {
	tb.Helper()
	ctx := context.Background()
	execID, err := s.Instantiate(ctx, InstantiateParams{
		DefinitionID: defID, Input: input, TriggeredBy: "bench"})
	if err != nil {
		tb.Fatalf("Instantiate: %v", err)
	}
	if err := s.Start(ctx, execID, "bench"); err != nil {
		tb.Fatalf("Start: %v", err)
	}
	if err := completeNode(tb, s, d, execID, "pre", engine.AttemptSucceed,
		json.RawMessage(`{"text":"ctx-out"}`)); err != nil {
		tb.Fatalf("complete pre: %v", err)
	}
	completeInst := func(fan string, ino int, failed bool) {
		tb.Helper()
		row := instanceRowOf(tb, d, execID, fan, uint16(ino))
		att := seedAttempt(tb, d, execID, row.id, 1)
		st := engine.AttemptSucceed
		out := json.RawMessage(`{"verdict":"v"}`)
		if failed {
			st = engine.AttemptFailed
			out = nil
		}
		mustComplete(tb, s, execID, row.id, att, st, out)
	}
	for i := 1; i <= k; i++ {
		fan := fmt.Sprintf("fan_%03d", i)
		for j := 1; j <= per; j++ {
			if i == 1 && j == 1 {
				continue // 失败源最后收口(见文件头口径注记)
			}
			completeInst(fan, j, false)
		}
	}
	completeInst("fan_001", 1, true)

	if e := readExec(tb, d, execID); e.status != engine.WorkflowFailed {
		tb.Fatalf("种子 wf = %s, want FAILED", e.status)
	}
	ctrl := instanceRowOf(tb, d, execID, "fan_001", 0)
	if ctrl.status != engine.NodeFailed {
		tb.Fatalf("种子 fan_001 控制 = %s, want FAILED", ctrl.status)
	}
	return execID
}

// TestRollbackFixtureEquivalence:fixture 与真路径 FAILED 终态在 ~300 行档
// 的回滚等价性——结果三件套(TargetNodeID/RerunTarget/ResetCount)与
// 回滚后 wf/关键行状态逐项一致。fixture 漂移(列形状/状态组合偏差)在
// 此红,而不是在压测数字里静默失真。
func TestRollbackFixtureEquivalence(t *testing.T) {
	const k, per = 3, 97 // 行集 = 2 + 3×98 = 296
	s, d := newBenchSched(t)
	defID := seedDef(t, d, benchFanGridDoc(k, per))
	input := benchFanInput(k, per)

	realExec := driveFanExecToFailed(t, s, d, defID, input, k, per)
	t.Cleanup(func() { cleanupExec(t, d, realExec) })
	realRes, err := s.RollbackTo(context.Background(), RollbackParams{
		ExecID: realExec, WorkerID: "bench", ActorID: "bench"})
	if err != nil {
		t.Fatalf("rollback real-path: %v", err)
	}

	fixExec := seedFailedFanFixture(t, d, defID, k, per, input)
	t.Cleanup(func() { cleanupExec(t, d, fixExec) })
	fixRes, err := s.RollbackTo(context.Background(), RollbackParams{
		ExecID: fixExec, WorkerID: "bench", ActorID: "bench"})
	if err != nil {
		t.Fatalf("rollback fixture: %v", err)
	}

	if realRes.TargetNodeID != fixRes.TargetNodeID || realRes.RerunTarget != fixRes.RerunTarget ||
		realRes.ResetCount != fixRes.ResetCount {
		t.Fatalf("result drift: real %+v, fixture %+v", *realRes, *fixRes)
	}
	if fixRes.TargetNodeID != "fan_001" || fixRes.RerunTarget {
		t.Fatalf("target = %s rerun=%v, want fan_001/false", fixRes.TargetNodeID, fixRes.RerunTarget)
	}
	for _, exec := range []uint64{realExec, fixExec} {
		if e := readExec(t, d, exec); e.status != engine.WorkflowRunning {
			t.Fatalf("exec %d 回滚后 wf = %s, want RUNNING", exec, e.status)
		}
		ctrl := instanceRowOf(t, d, exec, "fan_001", 0)
		if ctrl.status != engine.NodeExecuting {
			t.Fatalf("exec %d fan_001 控制回滚后 = %s, want EXECUTING(重派)", exec, ctrl.status)
		}
		other := instanceRowOf(t, d, exec, "fan_002", 0)
		if other.status != engine.NodeSucceeded {
			t.Fatalf("exec %d fan_002 = %s, want SUCCEEDED(闭包外不动)", exec, other.status)
		}
	}
}

// cleanupBenchExecs 按定义清掉 bench 产物(Instantiate 档的执行未逐个注册
// 清理,b.N 不可知)。
func cleanupBenchExecs(tb testing.TB, d *sql.DB, defID uint64) {
	tb.Helper()
	rs, err := d.Query(`SELECT id FROM workflow_execution WHERE definition_id=?`, defID)
	if err != nil {
		tb.Fatalf("query bench execs: %v", err)
	}
	var ids []uint64
	for rs.Next() {
		var id uint64
		if err := rs.Scan(&id); err != nil {
			_ = rs.Close()
			tb.Fatalf("scan exec id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rs.Err(); err != nil {
		_ = rs.Close()
		tb.Fatalf("iter execs: %v", err)
	}
	_ = rs.Close()
	for _, id := range ids {
		cleanupExec(tb, d, id)
	}
}

// BenchmarkInstantiateChain:触发物化吞吐(单事务 execution + 全节点行 +
// outbox execution.start)。rows/op = 链节点数。
func BenchmarkInstantiateChain(b *testing.B) {
	for _, n := range []int{16, 64, 256} {
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			s, d := newBenchSched(b)
			defID := seedDef(b, d, benchChainDoc(n))
			ctx := context.Background()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.Instantiate(ctx, InstantiateParams{
					DefinitionID: defID, Input: json.RawMessage(`{}`), TriggeredBy: "bench"}); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(n), "rows/op")
			cleanupBenchExecs(b, d, defID)
		})
	}
}

// BenchmarkCompleteChain:节点收口事务吞吐(计时圈只含 Complete = 生产
// worker 收口事务:全行锁 + CloseAttempt + 节点 CAS + advance 派发后继 +
// TouchExecution;attempt 行种子预插在圈外)。nodes/s 由墙时换算。
func BenchmarkCompleteChain(b *testing.B) {
	for _, n := range []int{16, 64} {
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			s, d := newBenchSched(b)
			defID := seedDef(b, d, benchChainDoc(n))
			ctx := context.Background()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				execID, err := s.Instantiate(ctx, InstantiateParams{
					DefinitionID: defID, Input: json.RawMessage(`{}`), TriggeredBy: "bench"})
				if err != nil {
					b.Fatal(err)
				}
				if err := s.Start(ctx, execID, "bench"); err != nil {
					b.Fatal(err)
				}
				var nodeIDs []uint64
				rs, err := d.Query(`SELECT id FROM node_execution WHERE execution_id=? ORDER BY node_id`, execID)
				if err != nil {
					b.Fatal(err)
				}
				for rs.Next() {
					var id uint64
					if err := rs.Scan(&id); err != nil {
						_ = rs.Close()
						b.Fatal(err)
					}
					nodeIDs = append(nodeIDs, id)
				}
				if err := rs.Err(); err != nil {
					_ = rs.Close()
					b.Fatal(err)
				}
				_ = rs.Close()
				atts := make([]uint64, len(nodeIDs))
				for idx, nid := range nodeIDs {
					atts[idx] = seedAttempt(b, d, execID, nid, 1)
				}
				b.StartTimer()
				for idx, nid := range nodeIDs {
					if err := s.Complete(ctx, CompletionRequest{
						ExecID: execID, NodeExecID: nid, AttemptID: atts[idx],
						Status: engine.AttemptSucceed, Output: json.RawMessage(`{"text":"v"}`),
						WorkerID: "bench"}); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				cleanupExec(b, d, execID)
				b.StartTimer()
			}
			b.ReportMetric(float64(n)*float64(b.N)/b.Elapsed().Seconds(), "nodes/s")
		})
	}
}

// BenchmarkRollbackLockWindow:大 DAG 回滚锁窗(RollbackTo 墙时;前置两点
// 查 + 提交后 finishIfDone,锁窗 ⊆ 墙时,上界口径)。fixture 网格种子在
// 计时圈外;rows/op = 实际行集(控制行 + parallel 实例行)。
func BenchmarkRollbackLockWindow(b *testing.B) {
	for _, target := range []int{100, 300, 1000, 3000, 10000} {
		k, per := fanGridShape(target)
		b.Run(fmt.Sprintf("rows=%d", fanGridRows(k, per)), func(b *testing.B) {
			s, d := newBenchSched(b)
			defID := seedDef(b, d, benchFanGridDoc(k, per))
			input := benchFanInput(k, per)
			ctx := context.Background()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				execID := seedFailedFanFixture(b, d, defID, k, per, input)
				b.StartTimer()
				if _, err := s.RollbackTo(ctx, RollbackParams{
					ExecID: execID, WorkerID: "bench", ActorID: "bench"}); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				cleanupExec(b, d, execID)
				b.StartTimer()
			}
			b.ReportMetric(float64(fanGridRows(k, per)), "rows/op")
		})
	}
}

// BenchmarkRollbackGuardReject:容量闸拒绝成本(生产配置下每笔回滚都付的
// COUNT 索引范围扫 + 拒绝面)——主网格(MarkRows=0)量的是闸外曲线,正是
// 定档输入;本臂补生产可达路径的闸内开销面。
func BenchmarkRollbackGuardReject(b *testing.B) {
	const k, per = 1, 97 // 100 行,cap=2 恒拒
	d := sqltest.MustOpenAt(b, testSchema)
	s, err := New(Deps{DB: d, IDs: mustTestIDs(b), Queue: "bench", MaxRollbackRows: 2,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defID := seedDef(b, d, benchFanGridDoc(k, per))
	input := benchFanInput(k, per)
	ctx := context.Background()
	execID := seedFailedFanFixture(b, d, defID, k, per, input)
	b.Cleanup(func() { cleanupExec(b, d, execID) })
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.RollbackTo(ctx, RollbackParams{
			ExecID: execID, WorkerID: "bench", ActorID: "bench"}); err == nil {
			b.Fatal("guard did not reject")
		}
	}
	b.ReportMetric(100, "rows/op")
}

// BenchmarkRollbackLockWindowRealPath:回滚锁窗真路径锚(与 fixture 档
// 可比性校验;种子成本 O(N) Complete,档位钉 ~300 行)。正确性等价归
// TestRollbackFixtureEquivalence,本档只出墙时对照。
func BenchmarkRollbackLockWindowRealPath(b *testing.B) {
	const k, per = 3, 97 // 296 行
	s, d := newBenchSched(b)
	defID := seedDef(b, d, benchFanGridDoc(k, per))
	input := benchFanInput(k, per)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		execID := driveFanExecToFailed(b, s, d, defID, input, k, per)
		b.StartTimer()
		if _, err := s.RollbackTo(ctx, RollbackParams{
			ExecID: execID, WorkerID: "bench", ActorID: "bench"}); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		cleanupExec(b, d, execID)
		b.StartTimer()
	}
	b.ReportMetric(float64(fanGridRows(k, per)), "rows/op")
}
