package reaper

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"braid/internal/engine"
	"braid/internal/scheduler"
)

// TestReapRedispatchCarriesOriginalTrace:心跳判死重派拷原行 trace(T8.1
// trace 连续性维度:重放 = 原事件同一真相,不换段)——原派发行
// att:<id>:1 带主链 X,重派行 att:<id>:2 的 payload.trace_id 与 trace 列
// 都 == X(与 redispatchResume 死信网同式;列/payload 同值双写)。
func TestReapRedispatchCarriesOriginalTrace(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedReap(t, d, 5*time.Minute, 0, 3) // 心跳陈旧判死;deadline 未到;预算内重派
	const x = "4bf92f3577b34da6a3ce929d0e0e4736"
	payload, err := json.Marshal(scheduler.NodeTaskPayload{
		ExecutionID: s.execID, NodeExecutionID: s.nodeExecID, AttemptNo: 1, TraceID: x,
	})
	if err != nil {
		t.Fatalf("marshal node task payload: %v", err)
	}
	origTask := fmt.Sprintf("att:%d:1", s.nodeExecID)
	if _, err := d.Exec(`INSERT INTO outbox
	  (id, execution_id, event_type, task_id, queue, payload, status, trace_id, dispatched_at, available_at)
	  VALUES (?, ?, 'node.task_enqueue', ?, 'reap-test', ?, 'DISPATCHED', ?, NOW(3), NOW(3))`,
		nextID(t), s.execID, origTask, payload, x); err != nil {
		t.Fatalf("seed original dispatch event: %v", err)
	}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	rows := fetchOutbox(t, d, s.execID, "node.task_enqueue")
	if len(rows) != 2 {
		t.Fatalf("node.task_enqueue 行数 = %d, want 2(原行 + 重派)", len(rows))
	}
	want := fmt.Sprintf("att:%d:2", s.nodeExecID)
	if rows[1].taskID != want {
		t.Fatalf("重派 task_id = %q, want %q", rows[1].taskID, want)
	}
	var np scheduler.NodeTaskPayload
	if err := json.Unmarshal(rows[1].payload, &np); err != nil {
		t.Fatalf("redispatch payload: %v", err)
	}
	if np.TraceID != x {
		t.Fatalf("重派 payload trace_id = %q, want 原行 %q(重放不换段)", np.TraceID, x)
	}
	var colTrace sql.NullString
	if err := d.QueryRow(
		`SELECT trace_id FROM outbox WHERE task_id=?`, want).Scan(&colTrace); err != nil {
		t.Fatalf("read redispatched row trace column: %v", err)
	}
	if colTrace.String != x {
		t.Fatalf("重派行 trace 列 = %v, want %q(列/payload 同值双写)", colTrace, x)
	}
}

// TestReapRedispatchNoTraceStaysZeroInjected:原行无 trace(内部测试路径
// 派发)→ 重派零注入:payload 键缺席、列 NULL——不伪造段链。
func TestReapRedispatchNoTraceStaysZeroInjected(t *testing.T) {
	r, d := newReaper(t, testCfg())
	s := seedReap(t, d, 5*time.Minute, 0, 3)
	origTask := fmt.Sprintf("att:%d:1", s.nodeExecID)
	payload, err := json.Marshal(scheduler.NodeTaskPayload{
		ExecutionID: s.execID, NodeExecutionID: s.nodeExecID, AttemptNo: 1,
	})
	if err != nil {
		t.Fatalf("marshal node task payload: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO outbox
	  (id, execution_id, event_type, task_id, queue, payload, status, dispatched_at, available_at)
	  VALUES (?, ?, 'node.task_enqueue', ?, 'reap-test', ?, 'DISPATCHED', NOW(3), NOW(3))`,
		nextID(t), s.execID, origTask, payload); err != nil {
		t.Fatalf("seed original dispatch event: %v", err)
	}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	want := fmt.Sprintf("att:%d:2", s.nodeExecID)
	var colTrace sql.NullString
	if err := d.QueryRow(
		`SELECT trace_id, payload FROM outbox WHERE task_id=?`, want).
		Scan(&colTrace, &payload); err != nil {
		t.Fatalf("read redispatched row: %v", err)
	}
	if colTrace.Valid {
		t.Fatalf("无 trace 原行 → 重派 trace 列 = %q, want NULL(不伪造段链)", colTrace.String)
	}
	var np scheduler.NodeTaskPayload
	if err := json.Unmarshal(payload, &np); err != nil {
		t.Fatalf("redispatch payload: %v", err)
	}
	if np.TraceID != "" {
		t.Fatalf("无 trace 原行 → 重派 payload trace_id = %q, want 空(键缺席零注入)", np.TraceID)
	}
	// D-t121 #3 反向臂:重派 payload 恒不携 span_id(原发射 span 不在 DB,
	// 携陈旧/伪造 span 违「重放同一真相」)——键必须在原文缺席。
	var rawMap map[string]any
	if err := json.Unmarshal(payload, &rawMap); err != nil {
		t.Fatalf("redispatch payload raw: %v", err)
	}
	if _, ok := rawMap["span_id"]; ok {
		t.Fatalf("redispatch payload carries span_id %q, want key absent (D-t121 #3)", rawMap["span_id"])
	}
}

// TestReapBudgetBaseFreshRound:T9.2 预算基线(对抗审计 HIGH 修)——attempts-
// attempts_base < max_attempts 判定:回滚后新一轮满额(旧轮不吃新预算),
// 旧谓词(attempts<max)会把重跑首轮直接判耗尽。分支 B 路径(attempt 行
// 未物化于当前号)。变异锚:漏改本判定点 → 第一臂红(旧轮吃光)或耗尽臂
// 永不触发 → 第二臂红。
func TestReapBudgetBaseFreshRound(t *testing.T) {
	t.Run("新一轮预算内重派", func(t *testing.T) {
		r, d := newReaper(t, testCfg())
		s := seedReap(t, d, 5*time.Minute, 0, 2)
		// 回滚后现场:attempts=2(旧轮烧 1 + rerun claim 1)、base=1;
		// 判死走分支 B(当前号 attempt 行未物化)。2-1=1 < 2 ⇒ 重派。
		if _, err := d.Exec(`UPDATE node_execution SET attempts=2, attempts_base=1 WHERE id=?`, s.nodeExecID); err != nil {
			t.Fatalf("seed rolled-back budget: %v", err)
		}
		// 保留陈旧 RUNNING attempt 行(attempt_no=1 ≠ 当前 attempts):扫描①
		// 以它为候选,[2] 按当前号定位落空 → 分支 B(attemptGone)。

		if err := r.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		var attempts uint16
		var status engine.NodeState
		if err := d.QueryRow(`SELECT attempts, status FROM node_execution WHERE id=?`, s.nodeExecID).
			Scan(&attempts, &status); err != nil {
			t.Fatalf("read node: %v", err)
		}
		if status != engine.NodeExecuting || attempts != 3 {
			t.Fatalf("node = %s/%d, want EXECUTING/3(新一轮 Redispatch)", status, attempts)
		}
	})
	t.Run("新一轮耗尽终败", func(t *testing.T) {
		r, d := newReaper(t, testCfg())
		s := seedReap(t, d, 5*time.Minute, 0, 2)
		// attempts=3、base=1:3-1=2 ≮ 2 ⇒ FailNode + wf FAILED。
		if _, err := d.Exec(`UPDATE node_execution SET attempts=3, attempts_base=1 WHERE id=?`, s.nodeExecID); err != nil {
			t.Fatalf("seed exhausted budget: %v", err)
		}
		// 保留陈旧 RUNNING attempt 行(attempt_no=1 ≠ 当前 attempts):扫描①
		// 以它为候选,[2] 按当前号定位落空 → 分支 B(attemptGone)。

		if err := r.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		var ns engine.NodeState
		var ws engine.WorkflowState
		if err := d.QueryRow(`SELECT n.status, w.status FROM node_execution n
		  JOIN workflow_execution w ON w.id=n.execution_id WHERE n.id=?`, s.nodeExecID).
			Scan(&ns, &ws); err != nil {
			t.Fatalf("read states: %v", err)
		}
		if ns != engine.NodeFailed || ws != engine.WorkflowFailed {
			t.Fatalf("node/wf = %s/%s, want FAILED/FAILED(新一轮耗尽)", ns, ws)
		}
	})
}
