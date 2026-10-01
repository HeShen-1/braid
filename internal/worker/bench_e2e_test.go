package worker

// T15.5 端到端吞吐/时延基准(D-2026-10-01-t155-bench-baseline):真 relay +
// 真 asynq + 真 worker + t13 MySQL 全链,负载 = 即回执行器(echo,零外部
// 调用)× 线性 llm 链,隔离 engine/管道成本。复跑(槽位显式,本 lane = 2):
//
//	BRAID_TEST_REDIS_DB=2 go test ./internal/worker/ -bench . -benchtime 3x -run '^$' -timeout 1h
//
// 口径注记:
//   - relay 两档:生产缺省(IdleInterval=1s;有行连续排空,空转间隔即稀疏
//     流量的每节点时延地板)+ 压缩对照(20ms,隔离「轮询周期 vs 引擎管道
//     净成本」);relay 单事件单事务是管道天然串行段,吞吐数字含该瓶颈;
//   - 心跳/重试 = 生产缺省(10s/5s),本负载单执行窗口远短于心跳间隔,
//     写放大面不触发(长执行形态的吞吐折价见 docs/CAPACITY.md 外推节);
//   - 每节点时延取 DB 时间戳(finished_at − started_at)/N——不经轮询
//     墙钟(轮询量化会污染时延读数);排空判定走执行器完成计数器。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
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

// benchExec 即回执行器:零外部调用、无内存积累(fakeExec 的 calls 累积在
// 高吞吐基准下无界),只保留完成计数(排空信号)。
type benchExec struct {
	done atomic.Int64
}

func (e *benchExec) Execute(_ context.Context, _ Request) (Outcome, error) {
	e.done.Add(1)
	return Outcome{Status: engine.AttemptSucceed, Output: json.RawMessage(`{"text":"v"}`)}, nil
}

// benchChainDocW 生成 n 节点线性 llm 链 DSL(worker 包本地;scheduler 包
// bench_test.go 同款,测试代码跨包不复用)。
func benchChainDocW(n int) string {
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

// benchW 点位(雪花节点号与 e2e 分账同款:sched=2、worker=0)。
const (
	benchSchedNode  = uint16(2)
	benchWorkerNode = uint16(0)
	benchQueue      = "bench-e2e"
)

// bootBenchPipeline 起全链 rig:同队列的 scheduler + relay + worker;返回
// 排空信号与拆线函数(b.Cleanup 之外手动调用,迭代内复用)。relayIdle 控制
// relay 轮询周期(生产缺省 1s;压缩档隔离「轮询周期 vs 引擎管道净成本」
// ——链式管道每跳经 outbox,稀疏流量下 relay 空转间隔 = 每节点时延地板)。
func bootBenchPipeline(b *testing.B, conc int, relayIdle time.Duration) (*scheduler.Scheduler, *sql.DB, *benchExec, func()) {
	b.Helper()
	d := sqltest.MustOpenAt(b, workerSchema)
	// 上轮 Fatalf 的孤儿清扫(bench 专用段位,不触他人行):滞留 EXECUTING/
	// PENDING 行与残留 outbox 会污染本轮排空判定;TestMain 只清 outbox/
	// node_attempt,节点/执行行归此处。
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
			b.Fatalf("orphan sweep %q: %v", q, err)
		}
	}
	opt := redistest.MustOpt(b)
	const queue = benchQueue

	schedIDs, err := snowflake.New(benchSchedNode)
	if err != nil {
		b.Fatalf("snowflake: %v", err)
	}
	// 丢弃日志:scheduler/relay/worker 的 INFO 会与 go bench 结果行物理
	// 交错污染 ns/op 读数(asynq server 自身的启停日志无法经此消除,量少可容)。
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	sched, err := scheduler.New(scheduler.Deps{DB: d, IDs: schedIDs, Queue: queue, Logger: discard})
	if err != nil {
		b.Fatalf("scheduler: %v", err)
	}

	rcfg := relay.DefaultConfig()
	rcfg.IdleInterval = relayIdle
	rctx, rcancel := context.WithCancel(context.Background())
	renq := relay.NewAsynqEnqueuer(opt)
	relayDone := make(chan error, 1)
	go func() { relayDone <- relay.Run(rctx, relay.Deps{DB: d, Enqueuer: renq, Logger: discard}, rcfg) }()

	wcfg := Config{
		Concurrency: conc, Queues: map[string]int{queue: 1}, WorkerID: "bench-worker",
		HeartbeatInterval: 10 * time.Second, RetryDelay: 5 * time.Second,
		ShutdownTimeout: 10 * time.Second, SnowflakeNode: snowflake.Explicit(benchWorkerNode),
	}
	wctx, wcancel := context.WithCancel(context.Background())
	wids, err := snowflake.New(benchWorkerNode)
	if err != nil {
		b.Fatalf("snowflake: %v", err)
	}
	ex := &benchExec{}
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- Run(wctx, Deps{DB: d, IDs: wids, Executor: ex, Sched: sched, Logger: discard}, wcfg, opt)
	}()
	time.Sleep(200 * time.Millisecond) // asynq server 起听窗(探测失败的教训:立即投递会丢首跳)

	teardown := func() {
		rcancel()
		wcancel()
		select {
		case <-relayDone:
		case <-time.After(15 * time.Second):
			b.Fatal("relay Run 未在 15s 内退出")
		}
		select {
		case <-workerDone:
		case <-time.After(15 * time.Second):
			b.Fatal("worker Run 未在 15s 内退出")
		}
		insp := asynq.NewInspector(opt)
		_ = insp.DeleteQueue(benchQueue, true) // 容 ErrQueueNotFound
	}
	return sched, d, ex, teardown
}

// benchCleanupExec 删单执行产物(镜像 e2e cleanupE2E 的表序)。
func benchCleanupExec(tb testing.TB, d *sql.DB, execID uint64) {
	tb.Helper()
	for _, q := range []string{
		`DELETE FROM audit_log WHERE execution_id=?`,
		`DELETE FROM outbox WHERE execution_id=?`,
		`DELETE FROM node_attempt WHERE execution_id=?`,
		`DELETE FROM audit_pending WHERE execution_id=?`,
		`DELETE FROM approval_decision WHERE execution_id=?`,
		`DELETE FROM node_execution WHERE execution_id=?`,
		`DELETE FROM workflow_execution WHERE id=?`,
	} {
		if _, err := d.Exec(q, execID); err != nil {
			tb.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

// benchSeedDef 直插 PUBLISHED 定义(scheduler bench 同款;dsl_hash 占位 64 字节)。
func benchSeedDef(tb testing.TB, d *sql.DB, ids *snowflake.Generator, doc string) uint64 {
	tb.Helper()
	defID, err := ids.Next()
	if err != nil {
		tb.Fatalf("gen def id: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', ?, ?, 'bench', 'bench', 'bench', NOW(3))`,
		defID, fmt.Sprintf("wf-%d", defID), doc, string(make([]byte, 64))); err != nil {
		tb.Fatalf("seed definition: %v", err)
	}
	tb.Cleanup(func() { _, _ = d.Exec(`DELETE FROM workflow_definition WHERE id=?`, defID) })
	return defID
}

// BenchmarkWorkerE2EChain:端到端链路吞吐/时延。execs/s、nodes/s 由墙时
// 换算;lat-p50/p99 = 每节点时延(finished_at−started_at)/N 的分位,毫秒。
func BenchmarkWorkerE2EChain(b *testing.B) {
	const (
		chainNodes   = 16
		execsPerIter = 8
		drainWait    = 480 * time.Second // 排空护栏:须吃住完整 relay 退避链(5s→20s→80s→320s)与 asynq 单跳重试
	)
	for _, tc := range []struct {
		conc      int
		relayIdle time.Duration
	}{
		{1, time.Second}, {4, time.Second}, {8, time.Second}, // 生产轮询周期
		{1, 20 * time.Millisecond}, {8, 20 * time.Millisecond}, // 压缩对照:隔离引擎管道净成本
	} {
		name := fmt.Sprintf("concurrency=%d,relay-idle=%s", tc.conc, tc.relayIdle)
		b.Run(name, func(b *testing.B) {
			sched, d, ex, teardown := bootBenchPipeline(b, tc.conc, tc.relayIdle)
			defer teardown()
			ids, err := snowflake.New(benchSchedNode)
			if err != nil {
				b.Fatalf("snowflake: %v", err)
			}
			defID := benchSeedDef(b, d, ids, benchChainDocW(chainNodes))
			ctx := context.Background()

			var latMS []float64
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				before := ex.done.Load()
				var execIDs []uint64
				for e := 0; e < execsPerIter; e++ {
					execID, err := sched.Instantiate(ctx, scheduler.InstantiateParams{
						DefinitionID: defID, Input: json.RawMessage(`{}`), TriggeredBy: "bench"})
					if err != nil {
						b.Fatal(err)
					}
					execIDs = append(execIDs, execID)
				}
				deadline := time.Now().Add(drainWait)
				for ex.done.Load()-before < execsPerIter*chainNodes {
					if time.Now().After(deadline) {
						// 现场诊断进失败信息(relay/worker 日志已丢弃):
						// 节点状态直方图 + outbox 积压 + 队列深度。
						var hist strings.Builder
						if h2, err := d.Query(`SELECT status, COUNT(*) FROM node_execution
						  WHERE execution_id IN (SELECT id FROM workflow_execution WHERE triggered_by='bench')
						  GROUP BY status`); err == nil {
							for h2.Next() {
								var st string
								var n int
								if h2.Scan(&st, &n) == nil {
									fmt.Fprintf(&hist, "%s=%d ", st, n)
								}
							}
							_ = h2.Close()
						}
						var pend int
						_ = d.QueryRow(`SELECT COUNT(*) FROM outbox WHERE status='PENDING'`).Scan(&pend)
						qi, qerr := asynq.NewInspector(redistest.Opt()).GetQueueInfo(benchQueue)
						b.Fatalf("drain timeout: done=%d want %d; nodes[%s] outboxPENDING=%d queue=%v(err %v)",
							ex.done.Load()-before, execsPerIter*chainNodes, hist.String(), pend, qi, qerr)
					}
					time.Sleep(200 * time.Microsecond)
				}
				b.StopTimer()
				for _, execID := range execIDs {
					// 排空计数器在执行器返回时 +1,末节点的 Complete 事务可能
					// 仍在飞(RUNNING 瞬态)——有界轮询终态再断言,窗口只吃
					// 收尾事务(本段在 StopTimer 内,时延口径不受污染)。
					deadline := time.Now().Add(30 * time.Second)
					var st engine.WorkflowState
					var started, finished sql.NullTime
					for {
						if err := d.QueryRow(`SELECT status, started_at, finished_at FROM workflow_execution WHERE id=?`,
							execID).Scan(&st, &started, &finished); err != nil {
							b.Fatal(err)
						}
						if st == engine.WorkflowSucceeded || time.Now().After(deadline) {
							break
						}
						time.Sleep(5 * time.Millisecond)
					}
					if st != engine.WorkflowSucceeded {
						b.Fatalf("exec %d = %s, want SUCCEEDED", execID, st)
					}
					if started.Valid && finished.Valid {
						latMS = append(latMS, finished.Time.Sub(started.Time).Seconds()*1000/chainNodes)
					}
					benchCleanupExec(b, d, execID)
				}
				b.StartTimer()
			}
			b.StopTimer() // 迭代循环收尾停表:teardown(cancel/等退出/删队列)不计入 ns/op
			totalNodes := float64(execsPerIter * chainNodes * b.N)
			secs := b.Elapsed().Seconds()
			b.ReportMetric(totalNodes/secs, "nodes/s")
			b.ReportMetric(float64(execsPerIter*b.N)/secs, "execs/s")
			if len(latMS) > 0 {
				sort.Float64s(latMS)
				p := func(q float64) float64 { return latMS[min(int(q*float64(len(latMS))), len(latMS)-1)] }
				b.ReportMetric(p(0.50), "lat-p50-ms/node")
				b.ReportMetric(p(0.99), "lat-p99-ms/node")
			}
		})
	}
}
