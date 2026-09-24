package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// 本文件是仓库内唯一允许对 node_execution 发 UPDATE 的地方(AGENTS 铁律:
// 节点状态迁移只经 CAS helper,WHERE 必含 id=? AND status=? AND version=?)。
//
// 调用方契约(T2.6,D-2026-09-23-t26-audit-events):每次经本文件 helper 的
// 状态迁移,调用方必须在同一事务落对应 audit 行(internal/audit);非迁移
// 裁决(Redispatch/Extend)同规。新调用点不落审计行 = 破坏 T2.6 验收。
//
// 裁决不变量:每条 CAS 必含 version=version+1 ⇒ 谓词命中行必被改值 ⇒
// RowsAffected==matched(CanonicalDSN 未开 clientFoundRows,0 = changed 语义);
// 「0 行 = CAS 失败」判定因此可信。新增 CAS 语句评审 checklist 第一条。
//
// 返回 newVer = expectVer+1 本地算术,禁止读回(消灭 RR 快照读歧义,热路径零额外往返)。
// 败方路径才付一次复查 SELECT 区分 NotFound/Conflict(见 nodeCASFailed)。
//
// 时间一律 DB 端 NOW(3)(会话 UTC 由连接契约保证),多 worker 应用时钟漂移不入库;
// deadline 是唯一应用侧时间,偏差由 reconciler 阈值×N 吸收(T2.5)。

// ClaimInput 是进入 EXECUTING 的两条边(Claim/Resume)共用的参数。
type ClaimInput struct {
	WorkerID      string
	DeadlineAt    time.Time       // 零值 → NULL(无截止)
	InputSnapshot json.RawMessage // 已解析输入(全部 {{...}} 已求值,T2.4 求值器产出)
}

// ClaimNode:PENDING→EXECUTING,认领并同语句写入输入快照(DDL 尾注参考语句逐字)。
// 快照恰好由唯一获胜者写一次:败方 0 行,字节不可能落行。
func ClaimNode(ctx context.Context, q DBTX, id uint64, expectVer uint32, in ClaimInput) (uint32, error) {
	if err := validSnapshot(in.InputSnapshot); err != nil {
		return 0, err
	}
	res, err := q.ExecContext(ctx, `
UPDATE node_execution
   SET status='EXECUTING', worker_id=?, input_snapshot=?, deadline_at=?,
       started_at=IFNULL(started_at, NOW(3)), attempts=attempts+1,
       status_changed_at=NOW(3), version=version+1
 WHERE id=? AND status='PENDING' AND version=?`,
		in.WorkerID, in.InputSnapshot, nullTime(in.DeadlineAt), id, expectVer)
	if err != nil {
		return 0, fmt.Errorf("engine: claim node %d: %w", id, err)
	}
	return checkNodeCAS(ctx, q, res, id, NodePending, expectVer)
}

// ResumeNode:WAITING_APPROVAL→EXECUTING(审批复入,新 attempt)。
// started_at 跨复入不变(DDL:「首次 EXECUTING」),故无 IFNULL;快照重写为新解析值。
func ResumeNode(ctx context.Context, q DBTX, id uint64, expectVer uint32, in ClaimInput) (uint32, error) {
	if err := validSnapshot(in.InputSnapshot); err != nil {
		return 0, err
	}
	res, err := q.ExecContext(ctx, `
UPDATE node_execution
   SET status='EXECUTING', worker_id=?, input_snapshot=?, deadline_at=?,
       attempts=attempts+1, status_changed_at=NOW(3), version=version+1
 WHERE id=? AND status='WAITING_APPROVAL' AND version=?`,
		in.WorkerID, in.InputSnapshot, nullTime(in.DeadlineAt), id, expectVer)
	if err != nil {
		return 0, fmt.Errorf("engine: resume node %d: %w", id, err)
	}
	return checkNodeCAS(ctx, q, res, id, NodeWaitingApproval, expectVer)
}

// SucceedNode:EXECUTING→SUCCEEDED。worker_id 入 WHERE 是纵深防御(version 已随
// cancel/requeue 变化,此处再锁租约归属:旧 worker 的迟到收口必败)。
func SucceedNode(ctx context.Context, q DBTX, id uint64, expectVer uint32, workerID string, output json.RawMessage) (uint32, error) {
	res, err := q.ExecContext(ctx, `
UPDATE node_execution
   SET status='SUCCEEDED', output=?, deadline_at=NULL, finished_at=NOW(3),
       status_changed_at=NOW(3), version=version+1
 WHERE id=? AND status='EXECUTING' AND version=? AND worker_id=?`,
		nullableJSON(output), id, expectVer, workerID)
	if err != nil {
		return 0, fmt.Errorf("engine: succeed node %d: %w", id, err)
	}
	return checkNodeCAS(ctx, q, res, id, NodeExecuting, expectVer)
}

// FailNode:EXECUTING→FAILED。attempts<max_attempts 的节点层重试由调用方比对后
// 自行决策(T2.3),本 helper 只落终局失败。
func FailNode(ctx context.Context, q DBTX, id uint64, expectVer uint32, workerID, code, msg string) (uint32, error) {
	res, err := q.ExecContext(ctx, `
UPDATE node_execution
   SET status='FAILED', error_code=?, error_message=?, deadline_at=NULL,
       finished_at=NOW(3), status_changed_at=NOW(3), version=version+1
 WHERE id=? AND status='EXECUTING' AND version=? AND worker_id=?`,
		code, msg, id, expectVer, workerID)
	if err != nil {
		return 0, fmt.Errorf("engine: fail node %d: %w", id, err)
	}
	return checkNodeCAS(ctx, q, res, id, NodeExecuting, expectVer)
}

// SuspendNode:EXECUTING→WAITING_APPROVAL。worker_id 保留归因(列注释:NULL=未被执行/
// 剪枝;挂起行已被执行过,idx_node_worker 故障归因)。deadline=now+timeout_hours 由
// 调用方算。DDL 挂起契约:与当前 attempt 收口(RUNNING→SUCCEEDED)同事务是 T4.1 的
// 强制(否则留 RUNNING attempt 被心跳判死扫描误报),本签名收 DBTX 即为该组合留口。
func SuspendNode(ctx context.Context, q DBTX, id uint64, expectVer uint32, workerID string, deadline time.Time) (uint32, error) {
	res, err := q.ExecContext(ctx, `
UPDATE node_execution
   SET status='WAITING_APPROVAL', deadline_at=?, status_changed_at=NOW(3), version=version+1
 WHERE id=? AND status='EXECUTING' AND version=? AND worker_id=?`,
		nullTime(deadline), id, expectVer, workerID)
	if err != nil {
		return 0, fmt.Errorf("engine: suspend node %d: %w", id, err)
	}
	return checkNodeCAS(ctx, q, res, id, NodeExecuting, expectVer)
}

// FailSuspendedNode:WAITING_APPROVAL→FAILED(T4.3 审批超时 fail 策略终败;
// error_code 词表 DDL 预留 approval_timeout)。无 worker_id 门——超时裁决方是
// reaper,无租约可验(version CAS 足够,镜像 RequeueNode/RedispatchNode 的无
// worker_id 形态);worker_id 保留挂起归因不动。挂起行 attempt 已收口(T4.1
// 挂起契约),无 attempt 侧动作。
func FailSuspendedNode(ctx context.Context, q DBTX, id uint64, expectVer uint32, code, msg string) (uint32, error) {
	res, err := q.ExecContext(ctx, `
UPDATE node_execution
   SET status='FAILED', error_code=?, error_message=?, deadline_at=NULL,
       finished_at=NOW(3), status_changed_at=NOW(3), version=version+1
 WHERE id=? AND status='WAITING_APPROVAL' AND version=?`,
		code, msg, id, expectVer)
	if err != nil {
		return 0, fmt.Errorf("engine: fail suspended node %d: %w", id, err)
	}
	return checkNodeCAS(ctx, q, res, id, NodeWaitingApproval, expectVer)
}

// MarkNodeEscalated:挂起节点的升级通知时刻一次性落定(Q9:escalate 仅一次,
// DDL escalated_at 注释)。status-preserving CAS(RedispatchNode/
// ExtendNodeDeadline 同款);escalated_at IS NULL 进谓词 = 并发双写的 DB 兜底
// (调用方在行锁内先读 escalated_at 分支,正常路径不会撞)。escalated_at
// NULL→值必改行,version+1 ⇒ RowsAffected==matched 不变量保持。deadline 不清:
// 升级后节点仍处超时判定面(fail 策略换装/后续裁决仍需 deadline)。
func MarkNodeEscalated(ctx context.Context, q DBTX, id uint64, expectVer uint32) (uint32, error) {
	res, err := q.ExecContext(ctx, `
UPDATE node_execution
   SET escalated_at=NOW(3), version=version+1
 WHERE id=? AND status='WAITING_APPROVAL' AND version=? AND escalated_at IS NULL`,
		id, expectVer)
	if err != nil {
		return 0, fmt.Errorf("engine: mark node escalated %d: %w", id, err)
	}
	return checkNodeCAS(ctx, q, res, id, NodeWaitingApproval, expectVer)
}

// RequeueNode:EXECUTING→PENDING,恢复干净 PENDING——快照/租约/截止/非终局错误全清
// (DDL 契约「PENDING 未解析 input_snapshot 恒 NULL」);错误史在 node_attempt 与
// audit_log,状态列只保终局语义。仅 reconciler 语义(T2.5 判死重入队)。
func RequeueNode(ctx context.Context, q DBTX, id uint64, expectVer uint32) (uint32, error) {
	res, err := q.ExecContext(ctx, `
UPDATE node_execution
   SET status='PENDING', worker_id=NULL, input_snapshot=NULL, deadline_at=NULL,
       error_code=NULL, error_message=NULL, status_changed_at=NOW(3), version=version+1
 WHERE id=? AND status='EXECUTING' AND version=?`,
		id, expectVer)
	if err != nil {
		return 0, fmt.Errorf("engine: requeue node %d: %w", id, err)
	}
	return checkNodeCAS(ctx, q, res, id, NodeExecuting, expectVer)
}

// RedispatchInput 是 RedispatchNode 的再派发参数。
type RedispatchInput struct {
	WorkerID   string    // 再派发方(完成上一个 attempt 的调度进程)
	DeadlineAt time.Time // 零值 → NULL
}

// RedispatchNode:节点级重试的再派发(status-preserving CAS,EXECUTING→EXECUTING)。
// attempt 业务失败且 attempts<max_attempts 时,调度器/reaper 同事务组合:本 CAS +
// outbox att:<id>:<attempts+1>。重试权威在平台节点层(T2.3 移交),不经 PENDING
// (RequeueNode 保留为 runbook 手工原语,D-2026-09-22-t25-reaper-semantics #5)。
//   - attempts+1 是 DDL「claim 时 +1」的延展:平台重试的 claim 即本语句;
//   - 不写 error_code(终局失败专用列)、不重写 input_snapshot(前驱输出终态
//     不可变 ⇒ 重求值恒同,快照复现语义不受影响);
//   - deadline 必须刷新(否则 T2.5 会把重试窗内的节点当超时重入队);
//   - 状态机矩阵无新边(status 不变,非状态迁移);version+1 ⇒ RowsAffected==matched
//     不变量保持。
func RedispatchNode(ctx context.Context, q DBTX, id uint64, expectVer uint32, in RedispatchInput) (uint32, error) {
	res, err := q.ExecContext(ctx, `
UPDATE node_execution
   SET worker_id=?, deadline_at=?, attempts=attempts+1,
       status_changed_at=NOW(3), version=version+1
 WHERE id=? AND status='EXECUTING' AND version=?`,
		in.WorkerID, nullTime(in.DeadlineAt), id, expectVer)
	if err != nil {
		return 0, fmt.Errorf("engine: redispatch node %d: %w", id, err)
	}
	return checkNodeCAS(ctx, q, res, id, NodeExecuting, expectVer)
}

// ExtendNodeDeadline:EXECUTING 节点的 deadline 一次性重挂(status-preserving
// CAS,reaper 迟到启动路径:deadline 窗被队列排队吃掉而执行本体未超窗,
// started_at ≥ deadline_at 判据,D-2026-09-22-t25-reaper-semantics #3)。
// 与 RedispatchNode 互补:attempts/worker_id/快照全不动(不烧预算、不改租约),
// status_changed_at 不触(非状态迁移);deadline_at 必变值 ⇒ version+1 ⇒
// RowsAffected==matched 不变量保持。
func ExtendNodeDeadline(ctx context.Context, q DBTX, id uint64, expectVer uint32, deadline time.Time) (uint32, error) {
	res, err := q.ExecContext(ctx, `
UPDATE node_execution
   SET deadline_at=?, version=version+1
 WHERE id=? AND status='EXECUTING' AND version=?`,
		nullTime(deadline), id, expectVer)
	if err != nil {
		return 0, fmt.Errorf("engine: extend node deadline %d: %w", id, err)
	}
	return checkNodeCAS(ctx, q, res, id, NodeExecuting, expectVer)
}

// SkipNode:PENDING→SKIPPED(分支剪枝,T2.4)。自 PENDING 出,快照恒 NULL,不触碰该列。
func SkipNode(ctx context.Context, q DBTX, id uint64, expectVer uint32) (uint32, error) {
	res, err := q.ExecContext(ctx, `
UPDATE node_execution
   SET status='SKIPPED', finished_at=NOW(3), status_changed_at=NOW(3), version=version+1
 WHERE id=? AND status='PENDING' AND version=?`,
		id, expectVer)
	if err != nil {
		return 0, fmt.Errorf("engine: skip node %d: %w", id, err)
	}
	return checkNodeCAS(ctx, q, res, id, NodePending, expectVer)
}

// CancelNode:任意非终态→CANCELLED,唯一的 generic 边(from 集合即全部非终态)。
// 取消理由归 audit(T2.6),不写 error_code(取消不是失败)。自 EXECUTING 取消后,
// 在途 worker 的收口 CAS 因 version 已变必败 = 状态机定义内的收敛。
func CancelNode(ctx context.Context, q DBTX, id uint64, expectVer uint32) (uint32, error) {
	res, err := q.ExecContext(ctx, `
UPDATE node_execution
   SET status='CANCELLED', deadline_at=NULL, finished_at=NOW(3),
       status_changed_at=NOW(3), version=version+1
 WHERE id=? AND status IN ('PENDING','EXECUTING','WAITING_APPROVAL') AND version=?`,
		id, expectVer)
	if err != nil {
		return 0, fmt.Errorf("engine: cancel node %d: %w", id, err)
	}
	// Cancel 谓词是「非终态集合」,无单一 from:Expect 留空(集合不捏造单值),
	// 复查仍能区分 NotFound 与 Conflict(Observed=真实态)。
	return checkNodeCAS(ctx, q, res, id, "", expectVer)
}

// checkNodeCAS 统一裁决:affected==1 → newVer=expectVer+1;affected==0 → 复查行
// 区分 ErrNotFound / *ConflictError。仅败方路径付这次 SELECT 往返。
func checkNodeCAS(ctx context.Context, q DBTX, res sql.Result, id uint64, expect NodeState, expectVer uint32) (uint32, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("engine: rows affected (node %d): %w", id, err)
	}
	if n == 1 {
		return expectVer + 1, nil
	}
	var got NodeState
	var ver uint32
	// FOR UPDATE 当前读:调用方事务可能已有一致性读(RR read view),普通 SELECT 会
	// 回读本事务旧快照 → Observed 过期甚至 Expect==Observed 自相矛盾。此刻败方
	// UPDATE 已返回 0 行,行锁持有者已提交/回滚,锁读读到最新已提交行。
	err = q.QueryRowContext(ctx, `SELECT status, version FROM node_execution WHERE id=? FOR UPDATE`, id).Scan(&got, &ver)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: node_execution %d", ErrNotFound, id)
	}
	if err != nil {
		return 0, fmt.Errorf("engine: recheck node_execution %d: %w", id, err)
	}
	return 0, &ConflictError{ID: id, Expect: expect, ExpectVer: expectVer, Observed: got, ObservedVer: ver}
}

// validSnapshot:进入 EXECUTING 必须携带合法 JSON 快照(空/坏 JSON 均为调用方 bug,
// 不触 DB)。引擎存原文不校验内容——完整性契约归 T2.4 求值器测试。
func validSnapshot(snap json.RawMessage) error {
	if len(snap) == 0 || !json.Valid(snap) {
		return fmt.Errorf("%w: %q", ErrInvalidSnapshot, string(snap))
	}
	return nil
}

// nullableJSON:终态 output 允许 NULL(如无 outputs 的节点),非空必须合法 JSON。
func nullableJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
