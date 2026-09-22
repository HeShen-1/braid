package worker

// 集成测试基建:T2.3 验收链 = 真 asynq server + 真 MySQL(t13 栈,不可达自动
// skip)。TestMain 组合 Redis 槽位(AGENTS.md 分配表:worker 基号 1),模式与
// internal/relay/kill_test.go 同源(T2.7 契约:漏写组合器 = 槽位号被当绝对号,
// 并行 worktree 下静默互踩)。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"braid/internal/db"
	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/redistest"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
)

// workerRedisBase:worker 包的 Redis 基号(AGENTS.md:relay=0、worker=1、
// reaper=2;绝对 DB = worktree 槽位×4 + 基号)。
const workerRedisBase = 1

// workerSchema:worker 包独占 schema(relay 用 braid_relay_test 同款理由:
// 跨包隔离 go test ./... 并发)。
const workerSchema = "braid_worker_test"

// testIDs 供 seed 行生成应用侧雪花 id(nodeID=1;worker Deps 用 nodeID=0,
// 错开以便对账归因)。理论不可达错误在 TestMain 响亮退出。
var testIDs *snowflake.Generator

func TestMain(m *testing.M) {
	// T2.7:BRAID_TEST_REDIS_DB 语义 = worktree 槽位,组合为绝对 DB 号
	// (槽位×4 + worker 基号)写回同名 env;非法槽位响亮退出。
	redisDB, err := db.TestRedisDB(workerRedisBase)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := os.Setenv("BRAID_TEST_REDIS_DB", strconv.Itoa(redisDB)); err != nil {
		fmt.Fprintf(os.Stderr, "set BRAID_TEST_REDIS_DB: %v\n", err)
		os.Exit(2)
	}
	testIDs, err = snowflake.New(1)
	if err != nil {
		fmt.Fprintf(os.Stderr, "seed snowflake: %v\n", err)
		os.Exit(2)
	}
	// 父进程清扫:残留 PENDING 行会以更小 id 抢先被 gap 锁测试的 SKIP LOCKED
	// 扫描锁住,污染「第一行」前提;node_attempt 残留会撞跨轮唯一键
	// (uk_attempt_idem)。栈不可达则留给各测试自行 skip。
	if d, err := sqltest.OpenAt(workerSchema); err == nil {
		_, _ = d.Exec(`DELETE FROM outbox`)
		_, _ = d.Exec(`DELETE FROM node_attempt`)
		_ = d.Close()
	}
	os.Exit(m.Run())
}

// mustTestIDs 返回 seed 雪花生成器(测试内惰性断言初始化成功)。
func mustTestIDs(t *testing.T) *snowflake.Generator {
	t.Helper()
	if testIDs == nil {
		t.Fatal("seed snowflake generator not initialized")
	}
	return testIDs
}

// seedNodeExec 造最小业务链:definition → execution → node_execution
// (EXECUTING,模拟调度器 ClaimNode 后的状态)。T2.4 起 Complete 消费真实 DSL
// 拓扑与行内 worker_id 回传,种子必须与调度器产物同构:单节点合法 DSL、
// definition 带版本号(loadDefinition 拒 NULL version)、node 行带派发方归因。
func seedNodeExec(t *testing.T, d *sql.DB) (execID, nodeExecID uint64) {
	t.Helper()
	ids := mustTestIDs(t)
	defID, err := ids.Next()
	if err != nil {
		t.Fatalf("gen definition id: %v", err)
	}
	execID, err = ids.Next()
	if err != nil {
		t.Fatalf("gen execution id: %v", err)
	}
	nodeExecID, err = ids.Next()
	if err != nil {
		t.Fatalf("gen node execution id: %v", err)
	}
	name := fmt.Sprintf("seed-wf-%d", defID)
	seedDSL := fmt.Sprintf(`{"dsl":"1.0","name":%q,"description":"","inputs":[],"edges":[],`+
		`"nodes":[{"id":"seed_node","type":"llm","title":"seed","description":"",`+
		`"prompt":{"template":"seed"},"outputs":{}}],"outputs":{}}`, name)
	ins := func(query string, args ...any) {
		t.Helper()
		if _, err := d.Exec(query, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	ins(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', ?, ?, 'seed', 'seed', 'seed', NOW(3))`,
		defID, name, seedDSL, string(make([]byte, 64)))
	ins(`INSERT INTO workflow_execution
	  (id, definition_id, definition_name, definition_version, definition_hash, triggered_by, input)
	  VALUES (?, ?, ?, 1, ?, 'seed', '{}')`,
		execID, defID, name, string(make([]byte, 64)))
	ins(`INSERT INTO node_execution
	  (id, execution_id, node_id, type, status, attempts, worker_id, version)
	  VALUES (?, ?, 'seed_node', 'llm', 'EXECUTING', 1, 'seed-dispatcher', 1)`,
		nodeExecID, execID)
	return execID, nodeExecID
}

// fakeExec 是 Executor 测试桩:记录全部调用,fn 决定每次返回(可按调用序
// 编排失败/接管/心跳观察)。
type fakeExec struct {
	mu    sync.Mutex
	calls []Request
	fn    func(req Request, callNo int) (Outcome, error)
}

func (f *fakeExec) Execute(ctx context.Context, req Request) (Outcome, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	n := len(f.calls)
	f.mu.Unlock()
	return f.fn(req, n)
}

func (f *fakeExec) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// queueSeq 生成 per-process 唯一队列名(redistest 键级隔离契约)。
var queueSeq atomic.Uint64

func nextQueue(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("t_w%d", queueSeq.Add(1))
}

// startWorker 以生产路径 worker.Run 起一个 worker(goroutine),返回停止函数
// (cancel + 等 Run 返回)。调度器与 worker 共用队列名(事件必须投到被 serve
// 的队列,否则永挂 pending)。
func startWorker(t *testing.T, d *sql.DB, ex Executor, cfg Config) (queue string) {
	t.Helper()
	queue = nextQueue(t)
	startWorkerOnQueue(t, d, ex, cfg, queue)
	return queue
}

// startWorkerOnQueue 在指定队列上起 worker(e2e:Instantiate 的调度器先于
// worker 存在,需同一队列名)。
func startWorkerOnQueue(t *testing.T, d *sql.DB, ex Executor, cfg Config, queue string) {
	t.Helper()
	opt := redistest.MustOpt(t)
	cfg.Queues = map[string]int{queue: 1}
	if cfg.WorkerID == "" {
		cfg.WorkerID = "test-worker"
	}
	ids, err := snowflake.New(0)
	if err != nil {
		t.Fatalf("worker snowflake: %v", err)
	}
	sched, err := scheduler.New(scheduler.Deps{DB: d, IDs: ids, Queue: queue})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	wctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(wctx, Deps{DB: d, IDs: ids, Executor: ex, Sched: sched}, cfg, opt)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("worker.Run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("worker.Run did not stop within 15s after cancel")
		}
	})
}

// enqueue 投一个 node.task_enqueue 任务(与 relay 同款 option 面:确定性
// TaskID + MaxRetry=1),返回入队结果。client 由调用方管理生命周期。
func enqueue(t *testing.T, client *asynq.Client, queue, taskID string, p nodeTaskPayload) (*asynq.TaskInfo, error) {
	t.Helper()
	payload, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return client.EnqueueContext(ctx,
		asynq.NewTask(EventTypeNodeTask, payload),
		asynq.TaskID(taskID), asynq.Queue(queue), asynq.MaxRetry(1))
}

// attemptRow 是断言用的 attempt 行快照。
type attemptRow struct {
	ID             uint64
	Status         engine.AttemptState
	WorkerID       string
	AsynqTaskID    string
	IdempotencyKey string
	ErrorCode      sql.NullString
	FinishedAt     sql.NullTime
	HeartbeatAt    time.Time
	StartedAt      time.Time
}

// readAttempt 按 uk 读行(断言用;行不存在 t.Fatal)。
func readAttempt(t *testing.T, d *sql.DB, nodeExecID uint64, attemptNo uint16) attemptRow {
	t.Helper()
	var r attemptRow
	err := d.QueryRow(`
SELECT id, status, worker_id, asynq_task_id, idempotency_key, error_code, finished_at, heartbeat_at, started_at
  FROM node_attempt WHERE node_execution_id=? AND attempt_no=?`,
		nodeExecID, attemptNo).
		Scan(&r.ID, &r.Status, &r.WorkerID, &r.AsynqTaskID, &r.IdempotencyKey,
			&r.ErrorCode, &r.FinishedAt, &r.HeartbeatAt, &r.StartedAt)
	if err != nil {
		t.Fatalf("read attempt node_exec=%d no=%d: %v", nodeExecID, attemptNo, err)
	}
	return r
}

// waitAttemptStatus 轮询至 attempt 行达期望态(超时 fatal 附最后观测)。
func waitAttemptStatus(t *testing.T, d *sql.DB, nodeExecID uint64, attemptNo uint16, want engine.AttemptState) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last = "(no row)"
	for time.Now().Before(deadline) {
		var st engine.AttemptState
		err := d.QueryRow(`SELECT status FROM node_attempt WHERE node_execution_id=? AND attempt_no=?`,
			nodeExecID, attemptNo).Scan(&st)
		if err == nil {
			last = string(st)
			if st == want {
				return
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("poll attempt status: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("attempt node_exec=%d no=%d did not reach %s within 15s (last=%s)",
		nodeExecID, attemptNo, want, last)
}
