package reaper

// T9.3 链校验第七作业 + 启动自检(D-2026-09-29-t93 #5)。校验本体在
// internal/audit/verify.go(全链 linkage 复走/五族断链判据/落闩);此处只
// 驱动:分窗取待校验头(升序 = 多 head 钉死锁序)→ 逐执行校验 → 指标发射。
//
// 语义要点:断链是**结局不是故障**——闩已落定、写路径已拒(Append/Enqueue
// 读 head 即 ErrChainBroken),作业返回 nil(RunOnce 错误隔离语义保持:
// 已执法的断链不该每 tick 以 firstErr 形式刷屏;响亮面 = slog.Error +
// braid_audit_chain_broken_total/heads 指标 + AuditChainBroken 告警规则)。
// infra 错误(DB 不可达等)以 error 上抛,下轮重试。

import (
	"context"
	"fmt"

	"braid/internal/audit"
)

// 驱动上界(有界 tick,余量下轮;同 auditFlushRoundCap 语义):
// headsPerTick 限单轮处理执行数(单执行校验 = 全链复走,行数上界在
// audit.chainFetchCap)。
const (
	verifyHeadsPerTick = 100
	// startupVerifyMaxRounds 限启动自检的全量排空轮数(每轮 headsPerTick
	// 个头;溢出 = 待校验面超常,交周期作业收敛并告警)。
	startupVerifyMaxRounds = 100
)

// verifyAuditChain 是 RunOnce 第七作业:校验待校验链并发射观测。
func (r *Reaper) verifyAuditChain(ctx context.Context) error {
	heads, err := audit.VerifyDueHeads(ctx, r.deps.DB, verifyHeadsPerTick)
	if err != nil {
		return fmt.Errorf("reaper: audit chain due heads: %w", err)
	}
	var rows, newlyBroken int
	for _, execID := range heads {
		out, err := audit.VerifyChain(ctx, r.deps.DB, execID)
		if err != nil {
			return fmt.Errorf("reaper: audit chain verify (exec %d): %w", execID, err)
		}
		rows += out.Checked
		if out.Skipped {
			// 巨链跳过:规模事件,响亮但不炸作业(其余头照常校验)
			r.log.Warn("audit.chain_verify_skipped", "execution_id", execID, "reason", out.Reason)
		}
		if out.NewlyBroken {
			newlyBroken++
			// 响亮(首见一次;持续态由 gauge/告警承载,不重复刷日志)
			r.log.Error("audit.chain_broken", "execution_id", execID, "reason", out.Reason,
				"hint", "appends refused until manual reset (chain_heads.broken_at)")
		}
	}
	if r.m != nil {
		r.m.ChainVerifyRows.Add(float64(rows))
		r.m.ChainBrokenTotal.Add(float64(newlyBroken))
		var broken int
		if err := r.deps.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM chain_heads WHERE broken_at IS NOT NULL`).Scan(&broken); err != nil {
			return fmt.Errorf("reaper: audit chain broken count: %w", err)
		}
		r.m.ChainBrokenHeads.Set(float64(broken))
	}
	return nil
}

// StartupVerify 启动自检(验收「启动自检 + 定期校验」的启动半边):排空
// 全部待校验头。infra 错误只响亮记日志不阻断启动——reaper 的其余职责
// (flush/janitor/判死)不因校验面故障停摆;断链闩由本函数落定(执法在
// 写路径,与 reaper 是否存活无关)。
func (r *Reaper) StartupVerify(ctx context.Context) {
	for round := 0; round < startupVerifyMaxRounds; round++ {
		heads, err := audit.VerifyDueHeads(ctx, r.deps.DB, verifyHeadsPerTick)
		if err != nil {
			r.log.Error("audit.chain_startup_verify_failed", "err", err)
			return
		}
		if len(heads) == 0 {
			r.log.Info("audit.chain_startup_verified", "rounds", round)
			return
		}
		for _, execID := range heads {
			out, err := audit.VerifyChain(ctx, r.deps.DB, execID)
			if err != nil {
				r.log.Error("audit.chain_startup_verify_failed", "execution_id", execID, "err", err)
				return
			}
			if out.Skipped {
				r.log.Warn("audit.chain_verify_skipped", "execution_id", execID, "reason", out.Reason)
			}
			if out.NewlyBroken {
				r.log.Error("audit.chain_broken", "execution_id", execID, "reason", out.Reason,
					"hint", "appends refused until manual reset (chain_heads.broken_at)")
			}
		}
	}
	r.log.Warn("audit.chain_startup_rounds_exhausted",
		"max_rounds", startupVerifyMaxRounds, "hint", "backlog beyond startup budget; periodic job will converge")
}
