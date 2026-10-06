package scheduler

// T4.2 审批续跑收口:审批决定经 api 落库(approval_decision 行 + outbox
// execution.resume 事件同事务)后,本文件是 worker 侧的续跑事务——事件消费
// 链 = relay 投递 → worker handleNodeResume → Scheduler.Resume:
//
//	lockAllRows(全行集,node_id 升序)→ 终态守卫 → 类型守卫 → 幂等收敛守卫 →
//	决策复核(approval_decision 行)→ 快照守卫 → ResumeNode(WAITING_APPROVAL→
//	EXECUTING,新 attempt,快照原值重写)→ 迁移审计行 → 执行级镜像
//	(ResumeExecution|TouchExecution)→ 决策分岔:
//	  APPROVED → outputs 声明校验 → SucceedNode → 收口审计行 → 下游推进
//	  REJECTED → FailNode(approval_rejected)→ 收口审计行 → fail-fast
//
// 与 Suspend 的语义分立(勿合并,镜像 suspend.go 头注):Suspend 是「attempt
// 成功 + 节点挂起 + 下游零推进」;Resume 是「节点复入 + 按决定收口 + 下游
// 推进或 fail-fast」。审批节点复入后不回 executor(再执行只会再挂起——
// ApprovalExecutor 是纯挂起决策):本事务内直接终局化,attempt 计数的 +1 是
// DDL「claim 时 +1」对复入边的延展(RedispatchNode 同款注释),无 attempt 行
// (branch 内联 Claim→Succeed 同先例)。
//
// 决策记录:D-2026-09-24-t42-approval-resume。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"braid/internal/audit"
	"braid/internal/engine"
	"braid/internal/engine/dsl"
)

// approval_decision.decision 的 DDL ENUM 词表(0003 迁移逐字;Go 侧单点)。
const (
	DecisionApproved = "APPROVED"
	DecisionRejected = "REJECTED"
)

// 5a 守卫族拒绝哨兵(T17.4):消费侧(worker)按面计数
// braid_resume_events_rejected_total,词面锚 = metrics.ResumeReject*
// (对应关系由 internal/worker/resume_metrics_test.go 钉死)。文案改为
// 哨兵前缀 + 现场参数(机器消费面 = errors.Is,仓内无字符串匹配依赖);
// 上抛语义保持——asynq 重试一次 → archived 是该面既定死信可见性。
var (
	// ErrResumeStaleRound:事件 attempt 号错位(4a)或决定行 round 与挂起轮
	// 不符(5)——滞留旧轮事件或伪造轮次。
	ErrResumeStaleRound = errors.New("scheduler: resume event round mismatch")
	// ErrResumeFabricatedEvent:决定行不存在或锚定到别的执行/节点(5)。
	ErrResumeFabricatedEvent = errors.New("scheduler: resume event references a foreign or missing decision")
	// ErrResumePrematureEvent:本轮票数未定局(5a)——api 只在定局后落
	// resume 事件,此类事件只能来自直插/伪造/早产投递。
	ErrResumePrematureEvent = errors.New("scheduler: resume on undecided round")
	// ErrResumeAnchorDrift:锚行方向与本轮定局方向不符(5a)。
	ErrResumeAnchorDrift = errors.New("scheduler: resume anchor decision disagrees with round outcome")
)

// ResumeRequest 是审批续跑收口的请求(worker 适配 execution.resume 事件产出)。
// DecisionID 锚定 approval_decision 行:续跑事务复核其存在与归属(防伪事件/
// 漂移——payload 仅 id 引用,真相源回表,与 outbox「不回查业务表即可投递」
// 契约互补:投递不自证,消费必复核)。AttemptNo 是事件声称的「本次续跑将
// 创建的 attempt 号」(= TaskID 第四段):与锁行读出的 node.attempts+1 比对,
// 滞留的旧轮事件(P1 回滚重挂后)或伪造事件在此被响亮拒绝,不套用到新轮。
type ResumeRequest struct {
	ExecID     uint64
	NodeExecID uint64
	DecisionID uint64
	AttemptNo  uint16
	WorkerID   string // 续跑方(消费事件的 worker 进程;worker_id 归因)
}

// Resume 是审批续跑回写:单事务 = 锁全行集 → 守卫链 → ResumeNode → 审计 →
// 执行级镜像 → 决策分岔收口。1213/1205 与 Complete/Suspend 同款内部重试
// (只重试回写事务,不重跑任何执行器)。
func (s *Scheduler) Resume(ctx context.Context, req ResumeRequest) error {
	switch {
	case req.ExecID == 0 || req.NodeExecID == 0 || req.DecisionID == 0 || req.AttemptNo == 0:
		return errors.New("scheduler: zero id field(s) in resume request")
	case req.WorkerID == "":
		return errors.New("scheduler: empty worker id in resume request")
	}
	exec, err := s.loadExecution(ctx, req.ExecID)
	if err != nil {
		return err
	}
	wf, g, err := s.loadWorkflow(ctx, exec.definitionID)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		err := s.resumeOnce(ctx, req, wf, g, exec.input)
		if err == nil {
			return nil
		}
		if !engine.Transient(err) || attempt >= maxTransientRetries {
			return err
		}
		backoff := time.Duration(float64(time.Millisecond*25<<(attempt+1)) * (1 + rand.Float64()))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
}

// resumeOnce 是续跑的事务体(suspend.go suspendOnce 的镜像 + 决策分岔)。
func (s *Scheduler) resumeOnce(ctx context.Context, req ResumeRequest,
	wf *dsl.Workflow, g *dag, wfInput []byte) error {
	err := engine.WithTx(ctx, s.deps.DB, func(q engine.DBTX) error {
		if err := s.fence(ctx, q); err != nil {
			return err
		}
		// 1. 有序全行锁(与 suspendOnce/completeOnce 同集同序:并发续跑/
		// 完成/挂起在该序列上串行,幂等收敛守卫的裁决不可能读到过期状态)。
		rows, err := lockAllRows(ctx, q, req.ExecID, g)
		if err != nil {
			return err
		}
		n := rows.byID[req.NodeExecID]
		if n == nil {
			return fmt.Errorf("%w: node_execution %d not in execution %d",
				engine.ErrNotFound, req.NodeExecID, req.ExecID)
		}

		// 2. 终态守卫(快照读,同款):执行已终态(fail-fast 兄弟/取消竞速)
		// 的迟到续跑零动作收敛——决定行已由 api 落库(事实自证),节点留
		// WAITING_APPROVAL 与 suspendOnce 迟到路径同语义(归 T4.3 扫描面)。
		var wfStatus engine.WorkflowState
		if err := q.QueryRowContext(ctx,
			`SELECT status FROM workflow_execution WHERE id=?`, req.ExecID).Scan(&wfStatus); err != nil {
			return fmt.Errorf("scheduler: read execution status %d: %w", req.ExecID, err)
		}
		if wfStatus.Terminal() {
			s.log.Info("resume.discard_terminal_execution",
				"execution_id", req.ExecID, "node_execution_id", req.NodeExecID)
			return nil
		}

		// 3. 类型守卫:非 approval 节点的续跑请求 = 装配缺陷,响亮拒绝。
		if n.nodeType != "approval" {
			return fmt.Errorf("scheduler: resume on non-approval node %q (type %q)", n.nodeID, n.nodeType)
		}
		spec := g.nodes[n.nodeID].Approval
		if spec == nil {
			return fmt.Errorf("scheduler: approval node %q has no spec in pinned dsl (drift)", n.nodeID)
		}

		// 4. 幂等收敛守卫:节点已非 WAITING_APPROVAL = 先前一次投递已完成
		// 续跑(at-least-once 的第二发/接管重跑),零动作收敛。终态与
		// EXECUTING 中间态同样命中(后者窗内不可见:复入与收口同事务)。
		if n.status != engine.NodeWaitingApproval {
			s.log.Info("resume.converged",
				"execution_id", req.ExecID, "node_execution_id", req.NodeExecID,
				"node_status", string(n.status))
			return nil
		}

		// 4a. 轮次守卫:事件声称的 attempt 号必须与本次续跑将创建的 attempt
		// (node.attempts+1,行锁内权威值)一致。P0 单轮下重复投递恒一致;
		// 不一致 = 滞留旧轮事件(P1 回滚重挂后,round-1 决定不得套用
		// round-2 挂起)或伪造事件——响亮拒绝,零动作。
		if req.AttemptNo != n.attempts+1 {
			return fmt.Errorf("%w: event claims attempt %d, node %d is at attempts %d",
				ErrResumeStaleRound, req.AttemptNo, n.id, n.attempts)
		}

		// 5. 决策复核:事件引用的决定行必须存在且锚定本节点与本轮(伪事件/
		// 漂移/旧轮票套新轮挂起均响亮失败;决定行不可变,当前读即终值)。
		// round 锚(T9.2/D-t92 决策 6):决定行的 round 必等于当前挂起所属轮
		//(锁行 attempts)——回滚重挂后 round-1 旧行结构性不可达本轮。
		var decExecID, decNodeExecID uint64
		var decRound int
		var dbDecision, decidedBy string
		var comment sql.NullString
		err = q.QueryRowContext(ctx,
			`SELECT execution_id, node_execution_id, round, decision, decided_by, comment
			   FROM approval_decision WHERE id=?`, req.DecisionID).
			Scan(&decExecID, &decNodeExecID, &decRound, &dbDecision, &decidedBy, &comment)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: decision %d vanished", ErrResumeFabricatedEvent, req.DecisionID)
		}
		if err != nil {
			return fmt.Errorf("scheduler: read decision %d: %w", req.DecisionID, err)
		}
		if decExecID != req.ExecID || decNodeExecID != req.NodeExecID {
			return fmt.Errorf("%w: decision %d anchored to (exec %d, node %d), event claims (exec %d, node %d)",
				ErrResumeFabricatedEvent, req.DecisionID, decExecID, decNodeExecID, req.ExecID, req.NodeExecID)
		}
		if decRound != int(n.attempts) {
			return fmt.Errorf("%w: decision %d is round %d, node %d suspended in round %d",
				ErrResumeStaleRound, req.DecisionID, decRound, n.id, n.attempts)
		}
		decidedComment := ""
		if comment.Valid {
			decidedComment = comment.String
		}

		// 5a. 定局复核(T14.2,D-2026-09-30-t142-and-approval):「消费必
		// 复核」从行存在性延展到定局判据——本轮票数按 mode 裁决
		// (scheduler.ApprovalOutcome 单点):未定局(any 零票不可达、all
		// 票未齐)= 伪/早产事件,响亮拒绝零动作;定局方向与锚行方向交叉
		// 复核(不一致 = 锚行漂移)。mode 真源 = 钉版 DSL spec(effective
		// ApprovalSpec 生效值,与快照物化同源);REJECTED 恒定局(任一
		// 驳回即败,两模式同语义)。
		mode, _, _ := effectiveApprovalSpec(spec)
		var approved, rejected int
		if err := q.QueryRowContext(ctx, ApprovalRoundCountSQL,
			req.NodeExecID, int(n.attempts)).Scan(&approved, &rejected); err != nil {
			return fmt.Errorf("scheduler: count round decisions node %d: %w", n.id, err)
		}
		final, outcome := ApprovalOutcome(mode, len(spec.Approvers), approved, rejected)
		if !final {
			return fmt.Errorf("%w (node %d: mode %s, %d approved/%d rejected of %d approvers)",
				ErrResumePrematureEvent, n.id, mode, approved, rejected, len(spec.Approvers))
		}
		if outcome != dbDecision {
			return fmt.Errorf("%w: decision %d direction %q vs round outcome %q",
				ErrResumeAnchorDrift, req.DecisionID, dbDecision, outcome)
		}

		// 6. 快照守卫(镜像 suspendOnce):engine 不变量 WAITING_APPROVAL ⇒
		// input_snapshot NOT NULL;复入重写为同值(前驱输出终态不可变 ⇒ 求值
		// 恒同,复现语义不受影响)。
		var snap []byte
		if err := q.QueryRowContext(ctx,
			`SELECT input_snapshot FROM node_execution WHERE id=?`, n.id).Scan(&snap); err != nil {
			return fmt.Errorf("scheduler: read snapshot node %d: %w", n.id, err)
		}
		if snap == nil {
			return fmt.Errorf("scheduler: resume node %d without input_snapshot (engine invariant)", n.id)
		}

		// 7. 节点复入:WAITING_APPROVAL→EXECUTING(新 attempt)。deadline 零值
		// → NULL:本事务内随即终局化(成功收口/终败均清 deadline),中间态
		// 不可见,不存在可扫描的 NULL-deadline EXECUTING 窗口。
		newVer, err := engine.ResumeNode(ctx, q, n.id, n.version, engine.ClaimInput{
			WorkerID:      req.WorkerID,
			InputSnapshot: snap,
		})
		if err != nil {
			return err
		}
		n.status = engine.NodeExecuting
		n.version = newVer
		n.attempts++
		n.workerID = sql.NullString{String: req.WorkerID, Valid: true}

		st := newExecState(s, ctx, q, req.ExecID, wf, g, rows, req.WorkerID, wfInput)
		if err := st.stageRow(audit.EventNodeStateChanged, n, 0, audit.NodeStateChangedPayload{
			From: string(engine.NodeWaitingApproval), To: string(engine.NodeExecuting),
			NodeID: n.nodeID, Version: newVer, AttemptNo: n.attempts, WorkerID: req.WorkerID,
		}); err != nil {
			return err
		}

		// 8. 执行级镜像(先于终局化:wfEdges 无 WAITING_APPROVAL→FAILED/
		// SUCCEEDED 直达边,必须先回 RUNNING)。挂起事务的 quiesce rule 镜像:
		// wf= WAITING_APPROVAL(单活动路径挂起)→ ResumeExecution;wf=RUNNING
		// (fan-out 兄弟在飞的降级表示)→ 仅刷活跃时刻。Conflict 良性:并发
		// 续跑(双审批节点)先后镜像,后到者 CAS 败——TransitionWorkflow 已刷
		// 活跃时刻,零动作即可。
		if wfStatus == engine.WorkflowWaitingApproval {
			if err := engine.ResumeExecution(ctx, q, req.ExecID); err != nil {
				if !errors.Is(err, engine.ErrConflict) {
					return err
				}
				s.log.Warn("resume.execution_conflict",
					"execution_id", req.ExecID, "node_execution_id", n.id)
			} else {
				s.log.Info("execution.resumed",
					"execution_id", req.ExecID, "node_execution_id", n.id)
			}
		} else if err := engine.TouchExecution(ctx, q, req.ExecID); err != nil {
			return err
		}

		// 9. 决策分岔收口。
		switch dbDecision {
		case DecisionApproved:
			out, oerr := approvalOutput(spec, dbDecision, decidedBy, decidedComment)
			if oerr != nil {
				// 声明即契约(镜像 llm extractOutputs 的 bad_llm_output):
				// 不可满足的 outputs 声明显式终败,不留不可引用的半成功节点。
				return st.failApprovalNode(n, newVer, "bad_approval_output", oerr.Error())
			}
			doneVer, err := engine.SucceedNode(ctx, q, n.id, newVer, req.WorkerID, out)
			if err != nil {
				return err
			}
			if err := st.stageRow(audit.EventNodeStateChanged, n, 0, audit.NodeStateChangedPayload{
				From: string(engine.NodeExecuting), To: string(engine.NodeSucceeded),
				NodeID: n.nodeID, Version: doneVer, AttemptNo: n.attempts, WorkerID: req.WorkerID,
			}); err != nil {
				return err
			}
			n.status = engine.NodeSucceeded
			n.version = doneVer
			n.output = sql.NullString{String: string(out), Valid: true}
			// env 必须在节点收口之后构建:后继引用本节点 output。
			if err := st.buildEnv(wfInput); err != nil {
				return err
			}
			if err := st.advanceFrom(n.nodeID); err != nil {
				return err
			}
			s.log.Info("approval.approved",
				"execution_id", req.ExecID, "node_id", n.nodeID,
				"decided_by", decidedBy, "attempt_no", n.attempts)
		case DecisionRejected:
			msg := fmt.Sprintf("rejected by %s", decidedBy)
			if err := st.failApprovalNode(n, newVer, "approval_rejected", msg); err != nil {
				return err
			}
			s.log.Info("approval.rejected",
				"execution_id", req.ExecID, "node_id", n.nodeID, "decided_by", decidedBy)
		default:
			return fmt.Errorf("scheduler: decision %d value %q outside vocabulary", req.DecisionID, dbDecision)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// 提交后新事务判终态(镜像 Complete:批准可能是最后一个节点;拒绝路径
	// 已在事务内收口 FAILED,此处恒 no-op)。失败只记日志不上抛。
	if ferr := s.finishIfDone(ctx, req.ExecID, wf, wfInput); ferr != nil {
		s.log.Error("execution.finish_check_failed", "execution_id", req.ExecID, "err", ferr)
	}
	return nil
}

// failApprovalNode 是审批节点的终败收口(FailNode + 迁移审计行 + fail-fast,
// completeOnce 预算耗尽分支的同构):拒绝(approval_rejected)与不可满足的
// outputs 声明(bad_approval_output)共用。failExecution 的 Conflict 良性
// (并发终局化先者胜),其内部已吞。
func (st *execState) failApprovalNode(n *nodeRow, expectVer uint32, code, msg string) error {
	failVer, err := engine.FailNode(st.ctx, st.q, n.id, expectVer, st.workerID, code, msg)
	if err != nil {
		return err
	}
	if err := st.stageRow(audit.EventNodeStateChanged, n, 0, audit.NodeStateChangedPayload{
		From: string(engine.NodeExecuting), To: string(engine.NodeFailed),
		NodeID: n.nodeID, Version: failVer, Code: code,
		AttemptNo: n.attempts, WorkerID: st.workerID,
	}); err != nil {
		return err
	}
	n.status = engine.NodeFailed
	n.version = failVer
	return st.failExecution("node_failed",
		fmt.Sprintf("node %s failed: %s: %s", n.nodeID, code, msg))
}

// approvalOutput 物化审批节点的 node_execution.output。产出字段集 =
// {decision, decided_by, comment}(全 string;comment 未填写 = 空串,键
// 恒在——形状跨决定方向稳定)。DSL outputs 声明是引用契约(T1.2:未声明
// 字段不可被引用),声明字段不在产出集或类型非 string = 定义缺陷 → 错误,
// 调用方按 bad_approval_output 终败(镜像 llm extractOutputs「声明即契约」)。
func approvalOutput(spec *dsl.ApprovalNode, dbDecision, decidedBy, comment string) (json.RawMessage, error) {
	for field, typ := range spec.Outputs {
		if typ != "string" {
			return nil, fmt.Errorf("approval output %q declared as %q (approval produces string fields only)", field, typ)
		}
		switch field {
		case "decision", "decided_by", "comment":
		default:
			return nil, fmt.Errorf("approval output %q is not producible (vocabulary: decision/decided_by/comment)", field)
		}
	}
	return json.Marshal(map[string]string{
		"decision":   approvalOutputValue(dbDecision),
		"decided_by": decidedBy,
		"comment":    comment,
	})
}

// approvalOutputValue 把 DDL ENUM 决定值映射为节点 output 的请求面词表
// (小写;{{nodes.<id>.output.decision}} 的解析值)。
func approvalOutputValue(dbDecision string) string {
	switch dbDecision {
	case DecisionApproved:
		return "approved"
	case DecisionRejected:
		return "rejected"
	}
	return ""
}
