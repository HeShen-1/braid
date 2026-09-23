package reaper

// 验收主链(issue #4):kill -9 worker 中途 + 断 Redis 30s(reaper 侧结构
// 等价口径,D-t25 #2:reaper Locker 指向不可达端口 = leaderless 降级)+ 真
// relay 投递,扫描间隔×3 内 execution 无人工干预达终态。
//
// 链路:子进程真 worker claim attempt 1 → READY 哨兵 → 父进程 SIGKILL(真·
// 未收口 RUNNING 现场心跳骤停)→ 父进程 reaper(grace/scan 压缩)检出 →
// DEAD(heartbeat_lost)+ Redispatch(att:2 新 TaskID)→ 真 relay 投递 →
// 父进程常驻 worker 接手 attempt 2 → SUCCEEDED → wf SUCCEEDED。
//
// 断言分解(攻击 5/11 吸收):终态 eventually(压缩时限);检出界 = 死因码
// + 双 attempt 形态;3×scan 字面恒等绑定生产默认口径(设计文档 B 节)。

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/redistest"
	"braid/internal/relay"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
	"braid/internal/worker"
)

const accChildEnv = "BRAID_REAPER_ACC_CHILD=1"

// accChildMain:子进程 = 真 asynq worker,executor 在 claim 落库后打 READY
// 哨兵并阻塞(等被 SIGKILL)。队列经 BRAID_REAPER_ACC_QUEUE 传入。
func accChildMain() {
	d, err := sqltest.OpenAt(reaperSchema)
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
	queue := os.Getenv("BRAID_REAPER_ACC_QUEUE")
	sched, err := scheduler.New(scheduler.Deps{DB: d, IDs: ids, Queue: queue})
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: scheduler: %v\n", err)
		os.Exit(5)
	}
	cfg := worker.DefaultConfig()
	cfg.HeartbeatInterval = 200 * time.Millisecond // 压缩心跳:grace=1s 可测
	cfg.WorkerID = "child-worker"
	cfg.Queues = map[string]int{queue: 1}
	if err := worker.Run(context.Background(), worker.Deps{
		DB: d, IDs: ids, Executor: &gateExecutor{stdin: os.Stdin}, Sched: sched,
	}, cfg, redistest.Opt()); err != nil {
		fmt.Fprintf(os.Stderr, "child: worker: %v\n", err)
		os.Exit(6)
	}
}

// gateExecutor:claim 已落库(Request 到手 = attempt 行已 INSERT)→ READY →
// 阻塞读 stdin 至 SIGKILL(EOF = 父进程先死,兜底返回)。
type gateExecutor struct{ stdin *os.File }

func (g *gateExecutor) Execute(ctx context.Context, req worker.Request) (worker.Outcome, error) {
	fmt.Printf("READY %d\n", req.NodeExecutionID) // os.Stdout 无缓冲,直写管道
	buf := make([]byte, 1)
	_, _ = g.stdin.Read(buf)
	return worker.Outcome{Status: engine.AttemptSucceed}, nil
}

// TestAcceptanceKill9ConvergesWithoutIntervention:验收主链。
func TestAcceptanceKill9ConvergesWithoutIntervention(t *testing.T) {
	d := sqltest.MustOpenAt(t, reaperSchema)
	opt := redistest.MustOpt(t)

	// ---- 种子:max_attempts=2 的单节点执行(RUNNING + EXECUTING)。----
	ids := mustTestIDs(t)
	defID, _ := ids.Next()
	execID, _ := ids.Next()
	nodeID, _ := ids.Next()
	name := fmt.Sprintf("acc-wf-%d", defID)
	dsl := fmt.Sprintf(`{"dsl":"1.0","name":%q,"description":"","inputs":[],"edges":[],`+
		`"nodes":[{"id":"n1","type":"llm","title":"n","description":"","prompt":{"template":"t"},"outputs":{}}],"outputs":{}}`, name)
	ins := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	ins(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', ?, ?, 'seed', 'seed', 'seed', NOW(3))`,
		defID, name, dsl, string(make([]byte, 64)))
	ins(`INSERT INTO workflow_execution
	  (id, definition_id, definition_name, definition_version, definition_hash, status, triggered_by, input)
	  VALUES (?, ?, ?, 1, ?, 'RUNNING', 'acc', '{}')`,
		execID, defID, name, string(make([]byte, 64)))
	ins(`INSERT INTO node_execution
	  (id, execution_id, node_id, type, status, attempts, max_attempts, worker_id, version, deadline_at)
	  VALUES (?, ?, 'n1', 'llm', 'EXECUTING', 1, 2, 'acc-dispatch', 1, NOW(3) + INTERVAL 30 MINUTE)`,
		nodeID, execID)
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM outbox WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM node_attempt WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM node_execution WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM workflow_execution WHERE id=?`, execID)
		_, _ = d.Exec(`DELETE FROM workflow_definition WHERE id=?`, defID)
	})

	queue := fmt.Sprintf("accq%d", nodeID%99991)
	taskID1 := fmt.Sprintf("att:%d:1", nodeID)

	// ---- 阶段一:子进程 worker 消费 att:1,claim 后阻塞 → SIGKILL。----
	client := asynq.NewClient(opt)
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.EnqueueContext(context.Background(),
		asynq.NewTask(scheduler.EventTypeNodeTask,
			[]byte(fmt.Sprintf(`{"execution_id":%d,"node_execution_id":%d,"attempt_no":1}`, execID, nodeID))),
		asynq.TaskID(taskID1), asynq.Queue(queue), asynq.MaxRetry(1)); err != nil {
		t.Fatalf("enqueue att:1: %v", err)
	}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), accChildEnv, "BRAID_REAPER_ACC_QUEUE="+queue)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
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
	ready := make(chan string, 1)
	go func() {
		line, rerr := bufio.NewReader(stdout).ReadString('\n')
		if rerr != nil {
			ready <- fmt.Sprintf("ERR %v (stderr: %s)", rerr, stderr.String())
			return
		}
		ready <- strings.TrimSpace(line)
	}()
	select {
	case line := <-ready:
		if !strings.HasPrefix(line, "READY") {
			t.Fatalf("child boot: %s", line)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("child never claimed (stderr: %s)", stderr.String())
	}
	// 等 attempt 1 落库 + 至少一次心跳(kill 前最后心跳龄 ≤ 200ms)。
	waitFor(t, 5*time.Second, func() bool {
		var n int
		return d.QueryRow(`SELECT COUNT(*) FROM node_attempt WHERE node_execution_id=? AND status='RUNNING'`, nodeID).
			Scan(&n) == nil && n == 1
	})
	_ = stdin.Close()
	if err := cmd.Process.Kill(); err != nil { // SIGKILL:真·未收口现场
		t.Fatalf("kill: %v", err)
	}
	<-childDone

	// ---- 阶段二:reaper(断 Redis 结构等价:Locker 指向不可达端口)。----
	sched, err := scheduler.New(scheduler.Deps{DB: d, IDs: ids, Queue: queue})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	cfg := Config{
		ScanInterval:   200 * time.Millisecond,
		DeadGrace:      time.Second,
		ExecStale:      time.Second,
		PendingTTL:     time.Minute,
		DispatchStale:  time.Minute,
		AuditRetention: 24 * time.Hour,
		OutboxBatch:    100,
		Queue:          queue,
		ID:             "acc-reaper",
	}
	r, err := New(Deps{
		DB: d, Sched: sched, IDs: ids,
		// 断 Redis(结构等价):leaderless 降级仍全速对账(fail-open 验证)。
		Locker: RedisLocker(redis.NewClient(&redis.Options{
			Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond,
		})),
	}, cfg)
	if err != nil {
		t.Fatalf("reaper: %v", err)
	}
	rctx, rcancel := context.WithCancel(context.Background())
	defer rcancel()
	go func() {
		for {
			select {
			case <-rctx.Done():
				return
			default:
			}
			if err := r.RunOnce(rctx); err != nil {
				r.log.Warn("acc.reap_once", "err", err)
			}
			select {
			case <-rctx.Done():
				return
			case <-time.After(cfg.ScanInterval):
			}
		}
	}()

	// ---- 阶段三:真 relay 投递重派事件;父进程常驻 worker 接手。----
	wctx, wcancel := context.WithCancel(context.Background())
	defer wcancel()
	rcfg := relay.DefaultConfig()
	rcfg.IdleInterval = 20 * time.Millisecond
	rcfg.EnqueueTimeout = time.Second
	go func() {
		_ = relay.Run(wctx, relay.Deps{DB: d, Enqueuer: asynq.NewClient(opt)}, rcfg)
	}()
	wcfg := worker.DefaultConfig()
	wcfg.HeartbeatInterval = 200 * time.Millisecond
	wcfg.WorkerID = "takeover-worker"
	wcfg.Queues = map[string]int{queue: 1}
	go func() {
		_ = worker.Run(wctx, worker.Deps{DB: d, IDs: ids, Executor: &okExecutor{}, Sched: sched}, wcfg, opt)
	}()

	// ---- 断言:无人工干预终态 + 双 attempt 形态。----
	if !waitFor(t, 15*time.Second, func() bool {
		return readExecRow(t, d, execID) == engine.WorkflowSucceeded
	}) {
		t.Fatalf("execution 未收敛(kill 后 15s);child stderr: %s", stderr.String())
	}
	var a1, a2 string
	if err := d.QueryRow(`SELECT status FROM node_attempt WHERE node_execution_id=? AND attempt_no=1`, nodeID).Scan(&a1); err != nil {
		t.Fatalf("read attempt1: %v", err)
	}
	if err := d.QueryRow(`SELECT status FROM node_attempt WHERE node_execution_id=? AND attempt_no=2`, nodeID).Scan(&a2); err != nil {
		t.Fatalf("read attempt2: %v(应存在重派新 attempt)", err)
	}
	if a1 != string(engine.AttemptDead) || a2 != string(engine.AttemptSucceed) {
		t.Fatalf("attempts = %s/%s, want DEAD/SUCCEEDED", a1, a2)
	}
	var c1 sql.NullString
	if err := d.QueryRow(`SELECT error_code FROM node_attempt WHERE node_execution_id=? AND attempt_no=1`, nodeID).Scan(&c1); err != nil {
		t.Fatalf("read code: %v", err)
	}
	if c1.String != "heartbeat_lost" {
		t.Fatalf("attempt1 code = %q, want heartbeat_lost", c1.String)
	}
}

// okExecutor:正常收口成功(接管方)。
type okExecutor struct{ calls atomic.Int64 }

func (e *okExecutor) Execute(ctx context.Context, req worker.Request) (worker.Outcome, error) {
	e.calls.Add(1)
	return worker.Outcome{Status: engine.AttemptSucceed}, nil
}

// waitFor 轮询至 cond 为真或超时(返回最终 cond 值)。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
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
