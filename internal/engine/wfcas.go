package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// workflow_execution 状态迁移:status-only CAS(DDL:低频,目标态互斥收敛,无 version 列)。
// 并发双 Finish(SUCCEEDED vs FAILED)竞态 → 先者胜,败者 ErrConflict;
// WAITING_APPROVAL⇄RUNNING 振荡靠谓词互斥收敛。
//
// 锁顺序铁律(防 AB-BA,T2.4 修正为实际不变量):node_execution 全行集
// (node_id 升序,单语句序列 FOR UPDATE)→ node_attempt → workflow_execution(最后)。
// wf 最后是 T2.3 以来的实际不变量:worker 心跳/收口均为 attempt→wf,先拿 wf 会与
// 之成环。例外:Start(T2.4 调度器)先拿 wf 行锁——PENDING 执行不可能有任何
// claim/attempt 存在(派发先于 Start 提交不可能发生),无并发写者,安全;该假设
// 是 T2.5 reconciler 压测点之一。

// TransitionWorkflow 执行工作流状态机的单条边 from→to(status-only CAS)。
// 矩阵外的组合在进程内拒绝(ErrIllegalTransition,零 DB 往返)。
// started_at 仅当 to==RUNNING 时落定(IFNULL 语义=首次起跑),否则不触碰——
// PENDING→CANCELLED 不得给从未起跑的执行伪造 started_at(列注释:NULL=未起跑)。
func TransitionWorkflow(ctx context.Context, q DBTX, execID uint64, from, to WorkflowState) error {
	if !CanTransitionWorkflow(from, to) {
		return fmt.Errorf("%w: workflow_execution %s→%s (id=%d)", ErrIllegalTransition, from, to, execID)
	}
	stmt := `UPDATE workflow_execution
   SET status=?, status_changed_at=NOW(3), last_activity_at=NOW(3)`
	if to == WorkflowRunning {
		stmt += `, started_at=IFNULL(started_at, NOW(3))`
	}
	stmt += ` WHERE id=? AND status=?`
	res, err := q.ExecContext(ctx, stmt, to, execID, from)
	if err != nil {
		return fmt.Errorf("engine: transition workflow %d %s→%s: %w", execID, from, to, err)
	}
	return checkWfCAS(ctx, q, res, execID, from)
}

// StartExecution:PENDING→RUNNING(首次起跑落定 started_at)。
func StartExecution(ctx context.Context, q DBTX, execID uint64) error {
	return TransitionWorkflow(ctx, q, execID, WorkflowPending, WorkflowRunning)
}

// SuspendExecution:RUNNING→WAITING_APPROVAL(全部节点挂起后的执行级镜像)。
func SuspendExecution(ctx context.Context, q DBTX, execID uint64) error {
	return TransitionWorkflow(ctx, q, execID, WorkflowRunning, WorkflowWaitingApproval)
}

// ResumeExecution:WAITING_APPROVAL→RUNNING(审批通过后的执行级镜像)。
func ResumeExecution(ctx context.Context, q DBTX, execID uint64) error {
	return TransitionWorkflow(ctx, q, execID, WorkflowWaitingApproval, WorkflowRunning)
}

// FinishExecution 把执行迁入终态(to ∈ SUCCEEDED/FAILED/CANCELLED)。
// from 集合由 wfEdges 生成(防 ENUM 尾部追加时遗漏),SQL IN 谓词即其投影。
// 列纪律:outputs 仅 SUCCEEDED 携带;error_code/message 仅 FAILED 携带
// (CANCELLED 的取消理由归 audit,T2.6);finished_at 由 CAS 恰好置一次。
func FinishExecution(ctx context.Context, q DBTX, execID uint64, to WorkflowState, outputs json.RawMessage, code, msg string) error {
	if !to.Terminal() {
		return fmt.Errorf("%w: workflow_execution →%s 非终态 (id=%d)", ErrIllegalTransition, to, execID)
	}
	froms := wfFroms(to)
	if len(froms) == 0 {
		return fmt.Errorf("%w: workflow_execution →%s 无合法来源 (id=%d)", ErrIllegalTransition, to, execID)
	}
	var outArg, codeArg, msgArg any
	if to == WorkflowSucceeded {
		outArg = nullableJSON(outputs)
	}
	if to == WorkflowFailed {
		codeArg, msgArg = code, msg
	}
	quoted := make([]string, len(froms))
	for i, f := range froms {
		quoted[i] = "'" + string(f) + "'"
	}
	res, err := q.ExecContext(ctx, `
UPDATE workflow_execution
   SET status=?, outputs=?, error_code=?, error_message=?,
       finished_at=NOW(3), status_changed_at=NOW(3), last_activity_at=NOW(3)
 WHERE id=? AND status IN (`+strings.Join(quoted, ",")+`)`,
		to, outArg, codeArg, msgArg, execID)
	if err != nil {
		return fmt.Errorf("engine: finish workflow %d →%s: %w", execID, to, err)
	}
	return checkWfCAS(ctx, q, res, execID, froms[0])
}

// TouchExecution 刷新执行活跃时刻(节点迁移同事务组合;DDL:last_activity_at
// 任一节点/attempt 推进的同事务刷新,否则健康长执行被 reconciler 误报)。
//
// 注意:本语句**不满足**「CAS 必带 version+1 ⇒ affected==matched」不变量——它是
// 单调 bump,与行内现值同毫秒时 changed-rows=0 属正常幂等 no-op,不是失败。
// 0 行后必须复查行存在性区分「同毫秒 bump」与「行不存在」,不得直接判 NotFound。
func TouchExecution(ctx context.Context, q DBTX, execID uint64) error {
	res, err := q.ExecContext(ctx,
		`UPDATE workflow_execution SET last_activity_at=NOW(3) WHERE id=?`, execID)
	if err != nil {
		return fmt.Errorf("engine: touch workflow %d: %w", execID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("engine: rows affected (workflow %d): %w", execID, err)
	}
	if n == 1 {
		return nil
	}
	var one int
	// FOR UPDATE 当前读:与 CAS 复查同语义(存在性答案虽不受快照影响,统一姿势防漂移)。
	err = q.QueryRowContext(ctx, `SELECT 1 FROM workflow_execution WHERE id=? FOR UPDATE`, execID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: workflow_execution %d", ErrNotFound, execID)
	}
	if err != nil {
		return fmt.Errorf("engine: recheck workflow_execution %d: %w", execID, err)
	}
	return nil // 行在,同毫秒幂等 bump
}

// checkWfCAS:0 行复查区分 NotFound/Conflict(workflow 无 version,ObservedVer 恒 0)。
func checkWfCAS(ctx context.Context, q DBTX, res sql.Result, execID uint64, from WorkflowState) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("engine: rows affected (workflow %d): %w", execID, err)
	}
	if n == 1 {
		return nil
	}
	var got WorkflowState
	// FOR UPDATE 当前读,理由同 checkNodeCAS:防 RR read view 过期快照污染 Observed。
	err = q.QueryRowContext(ctx, `SELECT status FROM workflow_execution WHERE id=? FOR UPDATE`, execID).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: workflow_execution %d", ErrNotFound, execID)
	}
	if err != nil {
		return fmt.Errorf("engine: recheck workflow_execution %d: %w", execID, err)
	}
	return &ConflictError{ID: execID, Expect: NodeState(from), Observed: NodeState(got)}
}
