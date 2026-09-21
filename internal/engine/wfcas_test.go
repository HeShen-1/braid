package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestWorkflowHappyPath(t *testing.T) {
	f := newFixture(t)
	if err := StartExecution(f.ctx(), f.db, f.execID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := SuspendExecution(f.ctx(), f.db, f.execID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if err := ResumeExecution(f.ctx(), f.db, f.execID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	r := scanWf(t, f)
	if r.status != WorkflowRunning || !r.startedAt.Valid {
		t.Fatalf("行=%+v, want RUNNING/started", r)
	}
	out := json.RawMessage(`{"report":"done"}`)
	if err := FinishExecution(f.ctx(), f.db, f.execID, WorkflowSucceeded, out, "", ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	r = scanWf(t, f)
	if r.status != WorkflowSucceeded || !r.finishedAt.Valid || r.errorCode.Valid {
		t.Fatalf("行=%+v, want SUCCEEDED/finished/无 error", r)
	}
	jsonEq(t, r.outputs, out)

	// finished_at 恰好置一次:终态后一切迁移被拒,行逐列不变
	before := scanWf(t, f)
	if err := FinishExecution(f.ctx(), f.db, f.execID, WorkflowCancelled, nil, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("终态再迁移应拒绝,got %v", err)
	}
	if r := scanWf(t, f); r.status != before.status || r.finishedAt != before.finishedAt {
		t.Fatalf("终态行被改动: %+v → %+v", before, r)
	}
}

// PENDING→FAILED(reconciler 判死):未起跑直接终败,started_at 必须保持 NULL。
func TestWorkflowPendingToFailed(t *testing.T) {
	f := newFixture(t)
	if err := FinishExecution(f.ctx(), f.db, f.execID, WorkflowFailed, nil, "reconciler_timeout", "no activity"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	r := scanWf(t, f)
	if r.status != WorkflowFailed || r.startedAt.Valid || !r.finishedAt.Valid {
		t.Fatalf("行=%+v, want FAILED/未起跑/finished", r)
	}
	if r.errorCode.String != "reconciler_timeout" {
		t.Fatalf("error_code=%q", r.errorCode.String)
	}
}

// WAITING_APPROVAL→CANCELLED + 并发双 Finish(SUCCEEDED vs FAILED)先者胜。
func TestWorkflowDoubleFinishRace(t *testing.T) {
	f := newFixture(t)
	if err := StartExecution(f.ctx(), f.db, f.execID); err != nil {
		t.Fatalf("start: %v", err)
	}
	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errs[0] = FinishExecution(f.ctx(), f.db, f.execID, WorkflowSucceeded, json.RawMessage(`{"who":"succ"}`), "", "")
	}()
	go func() {
		defer wg.Done()
		<-start
		errs[1] = FinishExecution(f.ctx(), f.db, f.execID, WorkflowFailed, nil, "x", "y")
	}()
	close(start)
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("恰一胜失败: %v / %v", errs[0], errs[1])
	}
	r := scanWf(t, f)
	if !r.finishedAt.Valid {
		t.Fatalf("竞态后未终局: %+v", r)
	}
	if errs[0] == nil && (r.status != WorkflowSucceeded || !r.outputs.Valid) {
		t.Fatalf("SUCCEEDED 胜者效果不符: %+v", r)
	}
	if errs[1] == nil && (r.status != WorkflowFailed || r.errorCode.String != "x") {
		t.Fatalf("FAILED 胜者效果不符: %+v", r)
	}
}

func TestWorkflowCancelFromWaiting(t *testing.T) {
	f := newFixture(t)
	if err := StartExecution(f.ctx(), f.db, f.execID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := SuspendExecution(f.ctx(), f.db, f.execID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if err := FinishExecution(f.ctx(), f.db, f.execID, WorkflowCancelled, nil, "", ""); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	r := scanWf(t, f)
	if r.status != WorkflowCancelled || r.errorCode.Valid || r.outputs.Valid {
		t.Fatalf("行=%+v, want CANCELLED/无 error/无 outputs(取消理由归 audit)", r)
	}
}

func TestTouchExecution(t *testing.T) {
	f := newFixture(t)
	// 回归:与 INSERT 同毫秒的 touch 是幂等 no-op(changed-rows=0),不得误判 NotFound。
	// 连续两次(无 sleep)最大化命中同毫秒窗口——该路径本质时序相关,无法确定性覆盖。
	if err := TouchExecution(f.ctx(), f.db, f.execID); err != nil {
		t.Fatalf("同毫秒 touch #1: %v", err)
	}
	if err := TouchExecution(f.ctx(), f.db, f.execID); err != nil {
		t.Fatalf("同毫秒 touch #2: %v", err)
	}
	before := scanWf(t, f)
	time.Sleep(20 * time.Millisecond) // 保证 NOW(3) 毫秒推进
	if err := TouchExecution(f.ctx(), f.db, f.execID); err != nil {
		t.Fatalf("touch: %v", err)
	}
	after := scanWf(t, f)
	if !after.lastActivity.After(before.lastActivity) {
		t.Fatalf("last_activity_at 未刷新: %v → %v", before.lastActivity, after.lastActivity)
	}
	if err := TouchExecution(f.ctx(), f.db, newID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

// 判别性故障测试的形状预演(T2.2 正式落地):claim + touch 同事务,注入失败 → 双写同亡。
func TestWithTxRollsBackAtomically(t *testing.T) {
	f := newFixture(t)
	id := f.node()
	boom := errors.New("injected before outbox append")
	err := WithTx(f.ctx(), f.db, func(q DBTX) error {
		if _, err := ClaimNode(f.ctx(), q, id, 1, ClaimInput{WorkerID: "w1", InputSnapshot: json.RawMessage(`{}`)}); err != nil {
			return err
		}
		if err := TouchExecution(f.ctx(), q, f.execID); err != nil {
			return err
		}
		return boom // 模拟 outbox 写入前 panic/失败
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want 注入错误透传", err)
	}
	nr := scanNode(t, f.db, id)
	if nr.Status != NodePending || nr.Version != 1 || nr.Snapshot.Valid {
		t.Fatalf("事务回滚后节点应原封不动: %+v", nr)
	}
	// 回滚后原 version 仍可成功认领(锁未遗留、行未推进)
	if _, err := ClaimNode(f.ctx(), f.db, id, 1, ClaimInput{WorkerID: "w2", InputSnapshot: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("回滚后 re-claim: %v", err)
	}
}

type wfRow struct {
	status       WorkflowState
	startedAt    sql.NullTime
	finishedAt   sql.NullTime
	lastActivity time.Time
	errorCode    sql.NullString
	outputs      sql.NullString
}

func scanWf(t *testing.T, f *fixture) wfRow {
	t.Helper()
	var r wfRow
	err := f.db.QueryRowContext(f.ctx(), `
SELECT status, started_at, finished_at, last_activity_at, error_code, outputs
  FROM workflow_execution WHERE id=?`, f.execID).
		Scan(&r.status, &r.startedAt, &r.finishedAt, &r.lastActivity, &r.errorCode, &r.outputs)
	if err != nil {
		t.Fatalf("scan workflow %d: %v", f.execID, err)
	}
	return r
}
