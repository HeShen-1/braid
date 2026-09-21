package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"braid/internal/db/sqltest"
)

// idGen:进程唯一假 Snowflake。基座 = 启动时刻(右移腾位)异或 PID + 单调
// 计数:进程内不撞(计数唯一);跨测试**包**并行(go test ./... 多二进制共享
// braid_engine_test)不撞——纯时间基座在两二进制同 ~1ms 桶启动时会同基座,
// PID 熵消除该碰撞;跨测试运行不撞(启动时刻不同)。
var (
	idBase = uint64(time.Now().UnixNano()>>20)<<16 ^ uint64(os.Getpid())<<36
	idCtr  atomic.Uint64
)

func newID() uint64 { return idBase + idCtr.Add(1) }

// fixture:一次测试的全部种子行与其清理(FK RESTRICT ⇒ 叶先根后;禁 TRUNCATE,
// t.Parallel 下会互踩)。执行按 (execution_id, node_id) 每测唯一 ⇒ 并行安全。
type fixture struct {
	t      *testing.T
	db     *sql.DB
	execID uint64
	nodes  uint64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d := sqltest.MustOpen(t)
	f := &fixture{t: t, db: d, execID: newID()}
	defID := newID()
	name := fmt.Sprintf("wf-%d", defID)
	mustExec(f, `INSERT INTO workflow_definition
		(id, name, dsl_version, status, description, dsl, dsl_hash, created_by, updated_by)
		VALUES (?,?, '1.0', 'DRAFT', '', '{}', '', 'test', 'test')`, defID, name)
	mustExec(f, `INSERT INTO workflow_execution
		(id, definition_id, definition_name, definition_version, definition_hash, status, triggered_by, input)
		VALUES (?,?,?, 1, '', 'PENDING', 'test', '{}')`, f.execID, defID, name)
	t.Cleanup(func() {
		mustExec(f, `DELETE FROM node_attempt WHERE execution_id=?`, f.execID)
		mustExec(f, `DELETE FROM node_execution WHERE execution_id=?`, f.execID)
		mustExec(f, `DELETE FROM workflow_execution WHERE id=?`, f.execID)
		mustExec(f, `DELETE FROM workflow_definition WHERE id=?`, defID)
	})
	return f
}

// node 种一行 node_execution(PENDING, version=1)并返回行 id。
func (f *fixture) node() uint64 {
	f.t.Helper()
	f.nodes++
	id := newID()
	mustExec(f, `INSERT INTO node_execution
		(id, execution_id, node_id, type, status, max_attempts, version)
		VALUES (?,?,?, 'llm', 'PENDING', 1, 1)`, id, f.execID, fmt.Sprintf("n%d", f.nodes))
	return id
}

func (f *fixture) ctx() context.Context { return context.Background() }

func mustExec(f *fixture, query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.ExecContext(context.Background(), query, args...); err != nil {
		f.t.Fatalf("seed/exec %q: %v", query, err)
	}
}

// jsonEq 断言 DB 读回 JSON 与期望语义相等(decode→深比较;MySQL JSON 列会重排
// 键序/去空白,字节级相等从不被承诺)。want 为 nil 时断言 NULL。
func jsonEq(t *testing.T, got sql.NullString, want json.RawMessage) {
	t.Helper()
	if len(want) == 0 {
		if got.Valid {
			t.Fatalf("want NULL, got %s", got.String)
		}
		return
	}
	if !got.Valid {
		t.Fatalf("want JSON %s, got NULL", want)
	}
	jsonEqStr(t, got.String, want)
}

func jsonEqStr(t *testing.T, got string, want json.RawMessage) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("got 非法 JSON %q: %v", got, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("want 非法 JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("JSON 语义不等: got %s want %s", got, want)
	}
}

// nodeRow 是断言用回读快照。
type nodeRow struct {
	Status     NodeState
	Version    uint32
	Attempts   uint16
	WorkerID   sql.NullString
	Snapshot   sql.NullString // JSON 列按字符串读,MySQL 规范化后字节可预测
	Deadline   sql.NullTime
	StartedAt  sql.NullTime
	FinishedAt sql.NullTime
	ErrorCode  sql.NullString
	Output     sql.NullString
}

func scanNode(t *testing.T, d *sql.DB, id uint64) nodeRow {
	t.Helper()
	var r nodeRow
	err := d.QueryRowContext(context.Background(), `
SELECT status, version, attempts, worker_id, input_snapshot, deadline_at,
       started_at, finished_at, error_code, output
  FROM node_execution WHERE id=?`, id).
		Scan(&r.Status, &r.Version, &r.Attempts, &r.WorkerID, &r.Snapshot, &r.Deadline,
			&r.StartedAt, &r.FinishedAt, &r.ErrorCode, &r.Output)
	if err != nil {
		t.Fatalf("scan node %d: %v", id, err)
	}
	return r
}
