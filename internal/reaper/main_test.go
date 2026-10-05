package reaper

// 集成测试基建:t13 真 MySQL(+ Redis,leader/验收链用),不可达自动 skip。
// TestMain 组合 Redis 槽位(AGENTS.md 分配表:reaper 基号 2),模式与
// internal/worker/main_test.go 同源(T2.7 契约:漏写组合器 = 槽位号被当
// 绝对号,并行 worktree 下静默互踩)。

import (
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"braid/internal/db"
	"braid/internal/db/sqltest"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
)

// reaperRedisBase:reaper 包的 Redis 基号(AGENTS.md:relay=0、worker=1、
// reaper=2;绝对 DB = worktree 槽位×4 + 基号)。
const reaperRedisBase = 2

// reaperSchema:reaper 包独占 schema(跨包隔离 go test ./... 并发)。
const reaperSchema = "braid_reaper_test"

var testIDs *snowflake.Generator

func TestMain(m *testing.M) {
	if os.Getenv("BRAID_REAPER_ACC_CHILD") == "1" {
		// kill 子进程:BRAID_TEST_REDIS_DB 已是父进程组合好的绝对号,直接用
		// (再跑组合会把绝对号当槽位解析而越界——T2.7 契约)。
		accChildMain()
		return
	}
	if os.Getenv(flushChildEnv) == "1" {
		// T8.1 flush-kill 子进程:同款绝对号直用契约(flush 臂纯 MySQL,
		// 不触 Redis,分支形式与 acc 子进程对齐)。
		flushChildMain()
		return
	}
	redisDB, err := db.TestRedisDB(reaperRedisBase)
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
	// 父进程清扫:残留行会以更小 id 抢占游标分页扫描的「第一批」,污染
	// 用例前提;栈不可达则留给各测试自行 skip。
	if d, err := sqltest.OpenAt(reaperSchema); err == nil {
		_, _ = d.Exec(`DELETE FROM audit_log`)
		_, _ = d.Exec(`DELETE FROM outbox`)
		_, _ = d.Exec(`DELETE FROM audit_pending`) // T6.1 暂存面(全清 schema 级助手)
		_, _ = d.Exec(`DELETE FROM chain_heads`)   // T9.3 链锚(同上;残头会被第七作业判破污染断言)
		_, _ = d.Exec(`DELETE FROM login_audit`)   // T7.1 保留期清理面(清扫卫生对称)
		_, _ = d.Exec(`DELETE FROM node_attempt`)
		_, _ = d.Exec(`DELETE FROM node_execution`)
		_, _ = d.Exec(`DELETE FROM workflow_execution WHERE triggered_by='reap-seed'`)
		_ = d.Close()
	}
	os.Exit(m.Run())
}

func mustTestIDs(t *testing.T) *snowflake.Generator {
	t.Helper()
	if testIDs == nil {
		t.Fatal("seed snowflake generator not initialized")
	}
	return testIDs
}

// nextID 生成一个测试雪花 id(种子行/事件行)。
func nextID(t *testing.T) uint64 {
	t.Helper()
	id, err := mustTestIDs(t).Next()
	if err != nil {
		t.Fatalf("gen id: %v", err)
	}
	return id
}

// newReaper 构造被测实例(Locker=nil leaderless;毫秒级阈值由用例注入)。
func newReaper(t *testing.T, cfg Config) (*Reaper, *sql.DB) {
	t.Helper()
	return newReaperDeps(t, cfg, nil)
}

// newReaperDeps = newReaper + Deps 修改器(T17.4:Metrics/AsynqProbe 注入)。
func newReaperDeps(t *testing.T, cfg Config, mod func(*Deps)) (*Reaper, *sql.DB) {
	t.Helper()
	d := sqltest.MustOpenAt(t, reaperSchema)
	s, err := scheduler.New(scheduler.Deps{DB: d, IDs: mustTestIDs(t), Queue: "reap-test"})
	if err != nil {
		t.Fatalf("scheduler New: %v", err)
	}
	deps := Deps{DB: d, Sched: s, IDs: mustTestIDs(t), Logger: testLogger(t)}
	if mod != nil {
		mod(&deps)
	}
	r, err := New(deps, cfg)
	if err != nil {
		t.Fatalf("reaper New: %v", err)
	}
	return r, d
}

// seeded 是一次种子链:definition → execution(RUNNING)→ node(EXECUTING)
// → attempt(RUNNING,心跳年龄可控)。
type seeded struct {
	execID, nodeExecID, attID uint64
}

// seedReap 种一行可判死现场:node EXECUTING(attempts/max 可配),attempt
// RUNNING 且 heartbeat_at 回拨 stale(默认远超 grace)。
func seedReap(t *testing.T, d *sql.DB, heartbeatStale, deadlinePast time.Duration, maxA uint16) seeded {
	t.Helper()
	ids := mustTestIDs(t)
	defID, _ := ids.Next()
	execID, _ := ids.Next()
	nodeID, _ := ids.Next()
	attID, _ := ids.Next()
	name := fmt.Sprintf("reap-wf-%d", defID)
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
	  VALUES (?, ?, ?, 1, ?, 'RUNNING', 'reap-seed', '{}')`,
		execID, defID, name, string(make([]byte, 64)))
	var deadline any
	if deadlinePast > 0 {
		deadline = fmt.Sprintf("NOW(3) - INTERVAL %d MICROSECOND", deadlinePast.Microseconds())
	} else {
		deadline = "NOW(3) + INTERVAL 30 MINUTE"
	}
	ins(fmt.Sprintf(`INSERT INTO node_execution
	  (id, execution_id, node_id, type, status, attempts, max_attempts, worker_id, version, deadline_at)
	  VALUES (?, ?, 'n1', 'llm', 'EXECUTING', 1, ?, 'seed-dispatch', 1, %s)`, deadline),
		nodeID, execID, maxA)
	taskID := fmt.Sprintf("att:%d:1", nodeID)
	ins(fmt.Sprintf(`INSERT INTO node_attempt
	  (id, node_execution_id, execution_id, attempt_no, worker_id, status,
	   idempotency_key, asynq_task_id, started_at, heartbeat_at)
	  VALUES (?,?,?,?,?,'RUNNING',?,?,NOW(3),NOW(3) - INTERVAL %d MICROSECOND)`,
		heartbeatStale.Microseconds()),
		attID, nodeID, execID, 1, "w-first", taskID, taskID)
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM audit_log WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM outbox WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM audit_pending WHERE execution_id=?`, execID) // T6.1 暂存面
		_, _ = d.Exec(`DELETE FROM chain_heads WHERE execution_id=?`, execID)   // T9.3 链锚
		_, _ = d.Exec(`DELETE FROM node_attempt WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM node_execution WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM workflow_execution WHERE id=?`, execID)
		_, _ = d.Exec(`DELETE FROM workflow_definition WHERE id=?`, defID)
	})
	return seeded{execID: execID, nodeExecID: nodeID, attID: attID}
}

// seedExecRow 种一行执行(状态/静默时长可控)+ 最小 definition,返回
// (execID, defID)。
func seedExecRow(t *testing.T, d *sql.DB, status string, stale time.Duration) (uint64, uint64) {
	t.Helper()
	defID := nextID(t)
	execID := nextID(t)
	name := fmt.Sprintf("exec-wf-%d", defID)
	ins := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	ins(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 1, 'PUBLISHED', '1.0', '{}', ?, 'seed', 'seed', 'seed', NOW(3))`,
		defID, name, string(make([]byte, 64)))
	ins(fmt.Sprintf(`INSERT INTO workflow_execution
	  (id, definition_id, definition_name, definition_version, definition_hash, status, triggered_by, input, last_activity_at)
	  VALUES (?, ?, ?, 1, ?, ?, 'reap-seed', '{}', NOW(3) - INTERVAL %d MICROSECOND)`,
		stale.Microseconds()),
		execID, defID, name, string(make([]byte, 64)), status)
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM outbox WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM audit_pending WHERE execution_id=?`, execID) // T6.1 暂存面
		_, _ = d.Exec(`DELETE FROM chain_heads WHERE execution_id=?`, execID)   // T9.3 链锚
		_, _ = d.Exec(`DELETE FROM node_attempt WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM node_execution WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM workflow_execution WHERE id=?`, execID)
		_, _ = d.Exec(`DELETE FROM workflow_definition WHERE id=?`, defID)
	})
	return execID, defID
}

// testCfg 返回测试压缩配置(毫秒级;Validate 故意不跑——启动路径专用)。
func testCfg() Config {
	return Config{
		ScanInterval:         50 * time.Millisecond,
		DeadGrace:            200 * time.Millisecond,
		ExecStale:            200 * time.Millisecond,
		PendingTTL:           time.Minute,
		DispatchStale:        time.Minute,
		AuditRetention:       24 * time.Hour,
		LoginAuditRetention:  24 * time.Hour,
		NodeAttemptRetention: 24 * time.Hour,
		JanitorInterval:      time.Hour,
		OutboxBatch:          100,
		Queue:                "reap-test",
		ID:                   "test-reaper",
	}
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
