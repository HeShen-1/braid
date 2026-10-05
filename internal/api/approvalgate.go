// mode=all 混跑窗硬闸(T15.6;D-t151 决策 6 + D-t142①):旧二进制对
// 会签 mode=all 的三方破坏腿——旧 api 首票即定局(否决权静默剥夺,
// 不可逆)、旧 worker 无定局复核直接收口 premature 事件、旧 reaper 对
// 部分票节点空转重投——由启动期能力宣告收敛:三类实例 acquire 租约时
// 写 capabilities(snowflake.CapApprovalModeAll),api 在发布/触发含
// mode=all 的工作流前查「在场实例全部宣告」,在场未宣告即拒。
//
// 语义边界(如实登记,不高估闭合度):
//   - fail-closed:在场未宣告拒、在场查询错误同拒(误放 < 误拒,死实例
//     租约未过期 → 误拒方向安全);
//   - 某 kind 零在场 = vacuous 放行:停摆 ≠ 破坏,发布序纪律由 UPGRADE
//     承载(决策 6 钦定首落地窗仍靠人工纪律);
//   - 铺满窗/残余窗:硬闸只对租约表全舰队上线后的升级生效,首个落地窗
//     旧二进制不写租约表 + LB 路由旧 api = 零闸放行(UPGRADE §mode=all);
//   - 已挂起的 mode=all 节点不在闸面(D-t142:混跑窗内属已知破坏窗);
//   - 拒绝痕迹 = 响亮 slog + braid_api_fleet_gate_rejections_total 计数器
//     (audit_log 结构性执行锚定——writer validate 钉死 ExecutionID 非零,
//     物化前拒绝落不进执行事实域;零迁移禁新表);
//   - 发布面闸在 Publish 事务内对锁定读内容判定(T20.3 收前置窗:闸对象
//     与翻转内容是同一份 cur,「闸读旧稿、事务发新稿」的改写走私被行锁
//     串行化——赶在锁定读前提交的改写由闸对新内容判定,迟到者见非
//     DRAFT 冲突);在场扫描钉事务连接(持事务经池另取连接 = 并发发布
//     自饿死面,已消);残余边界 = 闸在场扫描与 commit 间舰队组成漂移
//     (租约表不随本事务钉住),触发面闸(def.DSL 快照)仍是最后一道
//     线,发布序纪律兜底;
//   - 回滚到旧(T15.2 代)二进制:旧 acquire 三写路径硬编码 NULL,旧副本
//     重启即抹回宣告 → 闸误拒(fail-closed 方向安全);「行未过期且旧副本
//     未重启」窗内宣告残留 = 误放方向,由 D-t142② 回滚前置 SELECT 承载。
package api

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/gin-gonic/gin"

	"braid/internal/engine/dsl"
	"braid/internal/metrics"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
)

// workflowUsesApprovalAll 报告解析后的工作流是否含会签 mode=all 审批节点
// (词面单源 scheduler.ApprovalModeAll;节点集是 DSL 平铺清单,审批节点
// 无嵌套形态)。放 internal/api 不入 dsl 包:engine 面零改动(与 t16.1
// 分层划线并行面解耦),判定是 api 闸策略非 DSL 语义。
func workflowUsesApprovalAll(wf *dsl.Workflow) bool {
	for i := range wf.Nodes {
		if a := wf.Nodes[i].Approval; a != nil && a.Mode == scheduler.ApprovalModeAll {
			return true
		}
	}
	return false
}

// dslUsesApprovalAll 解析原文并判定。已入库 DSL 在写入路径全部过过
// dsl.Parse,此处失败 = 存储层契约破坏,按内部错误响亮(不静默放行)。
func dslUsesApprovalAll(raw []byte) (bool, error) {
	wf, err := dsl.Parse(raw)
	if err != nil {
		return false, fmt.Errorf("fleet gate: parse dsl: %w", err)
	}
	return workflowUsesApprovalAll(wf), nil
}

// fleetGateRejection 是闸的 fail-closed 判决:Undeclared 列在场未宣告实例
// (诊断面:kind/holder/到期);ScanErr 是在场查询失败变体(同拒)。
type fleetGateRejection struct {
	Undeclared []snowflake.FleetInstance
	ScanErr    error
}

// gateFleetApprovalAll 查在场实例(snowflake.InFleet 单点:三类全扫、
// 过期判定 DB 端 NOW(3))并判定 CapApprovalModeAll 全宣告。nil = 放行。
// q 可传池(触发面)或 Publish 事务句柄(发布面——扫描钉事务连接,
// 持事务期间经池另取连接是并发发布自饿死面,T20.3 审计收口)。
func (s *Server) gateFleetApprovalAll(ctx context.Context, q snowflake.FleetScanner) *fleetGateRejection {
	fleet, err := snowflake.InFleet(ctx, q)
	if err != nil {
		return &fleetGateRejection{ScanErr: fmt.Errorf("fleet scan: %w", err)}
	}
	var missing []snowflake.FleetInstance
	for _, inst := range fleet {
		if !slices.Contains(inst.Capabilities, snowflake.CapApprovalModeAll) {
			missing = append(missing, inst)
		}
	}
	if len(missing) > 0 {
		return &fleetGateRejection{Undeclared: missing}
	}
	return nil
}

// fleetUndeclaredDTO 是拒绝 body 的诊断行(holder 是 infra 标识非机密;
// 面向运维定位混跑窗里的旧二进制)。
type fleetUndeclaredDTO struct {
	Kind      string    `json:"kind"`
	Holder    string    `json:"holder"`
	ExpiresAt time.Time `json:"expires_at"`
}

// fleetGateErrorCode 是两协议面共用的错误码词面(REST body error 字段 /
// MCP 工具错误文本;混跑窗是暂时态,503 = 客户端升级完成后重试即解)。
const fleetGateErrorCode = "fleet_capability_gate"

// fleetGateRejectionBody 组 503 body:未宣告行列表(或查询失败 reason)。
func fleetGateRejectionBody(rej *fleetGateRejection) gin.H {
	if rej.ScanErr != nil {
		return gin.H{"error": fleetGateErrorCode, "reason": "fleet_unavailable"}
	}
	und := make([]fleetUndeclaredDTO, 0, len(rej.Undeclared))
	for _, inst := range rej.Undeclared {
		und = append(und, fleetUndeclaredDTO{
			Kind: string(inst.Kind), Holder: inst.HolderID, ExpiresAt: inst.ExpiresAt,
		})
	}
	return gin.H{"error": fleetGateErrorCode, "undeclared": und}
}

// noteFleetGateRejection 落拒绝痕迹:响亮 slog + 计数器(reason 单源
// 供 metrics label 复用)。action = "publish" | "trigger"。
func (s *Server) noteFleetGateRejection(action string, rej *fleetGateRejection) {
	reason := metrics.FleetGateReasonUndeclared
	if rej.ScanErr != nil {
		reason = metrics.FleetGateReasonUnavailable
	}
	if s.fleetGateMetrics != nil {
		s.fleetGateMetrics.Rejected.WithLabelValues(reason).Inc()
	}
	if rej.ScanErr != nil {
		slog.Error("api: fleet capability gate rejected (scan error)",
			"action", action, "err", rej.ScanErr)
		return
	}
	for _, inst := range rej.Undeclared {
		slog.Error("api: fleet capability gate rejected (undeclared in-fleet instance)",
			"action", action, "kind", string(inst.Kind), "holder", inst.HolderID,
			"expires_at", inst.ExpiresAt.UTC(), "want_capability", snowflake.CapApprovalModeAll)
	}
}

// fleetGateAbort 是发布面闸在 Publish 事务内的中止信号:携带拒绝判决
// (handler errors.As 后映射 503 诊断 body);WithTx 回滚整个发布。
type fleetGateAbort struct {
	rej *fleetGateRejection
}

func (a *fleetGateAbort) Error() string {
	if a.rej.ScanErr != nil {
		return fmt.Sprintf("fleet gate: fleet scan: %v", a.rej.ScanErr)
	}
	return fmt.Sprintf("fleet gate: %d in-fleet instance(s) missing %s",
		len(a.rej.Undeclared), snowflake.CapApprovalModeAll)
}

// publishFleetGate 是发布面闸(T20.3 起在 Publish 事务内执行,经 Publish
// 的 gate 参数注入;handlers.publishDraft 调用):对锁定读内容判定,含
// mode=all 才查在场宣告(扫描经 q 钉事务连接),未全宣告 →
// fleetGateAbort(事务回滚,拒绝痕迹在拒绝点落)。TOCTOU 边界见文件头。
func (s *Server) publishFleetGate(ctx context.Context, q DBTX, cur Definition) error {
	usesAll, err := dslUsesApprovalAll(cur.DSL)
	if err != nil {
		slog.Error("api: fleet gate parse draft dsl", "err", err, "definition_id", cur.ID)
		return fmt.Errorf("fleet gate: parse dsl: %w", err)
	}
	if !usesAll {
		return nil
	}
	rej := s.gateFleetApprovalAll(ctx, q)
	if rej == nil {
		return nil
	}
	s.noteFleetGateRejection("publish", rej)
	return &fleetGateAbort{rej: rej}
}

// gateTriggerFleetApprovalAll 是触发面闸(execution.triggerCore 调用,
// REST/MCP 共享判定核):def.DSL 是钉版快照(物化前唯一真源),判定 +
// 查在场,未全宣告 → 分类码拒绝(triggerError 面,REST 映射 503,MCP
// 面 generic 文本)。闸先于幂等键处理——重放请求在混跑窗同拒(误拒方向
// 安全,升级完成后重试即得重放回显)。
func (s *Server) gateTriggerFleetApprovalAll(ctx context.Context, def Definition) *triggerError {
	usesAll, err := dslUsesApprovalAll(def.DSL)
	if err != nil {
		return &triggerError{code: triggerInternal, err: err}
	}
	if !usesAll {
		return nil
	}
	rej := s.gateFleetApprovalAll(ctx, s.dbh)
	if rej == nil {
		return nil
	}
	s.noteFleetGateRejection("trigger", rej)
	return &triggerError{code: triggerFleetGate, err: fmt.Errorf(
		"fleet gate: %d in-fleet instance(s) missing %s", len(rej.Undeclared), snowflake.CapApprovalModeAll)}
}
