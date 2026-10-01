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
//   - 发布面 TOCTOU:闸读草稿 DSL 与 Publish 事务间草案可被改写引入
//     mode=all——触发面闸(def.DSL 快照)是最后一道线,发布序纪律兜底;
//   - 回滚到旧(T15.2 代)二进制:旧 acquire 三写路径硬编码 NULL,旧副本
//     重启即抹回宣告 → 闸误拒(fail-closed 方向安全);「行未过期且旧副本
//     未重启」窗内宣告残留 = 误放方向,由 D-t142② 回滚前置 SELECT 承载。
package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
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
func (s *Server) gateFleetApprovalAll(ctx context.Context) *fleetGateRejection {
	fleet, err := snowflake.InFleet(ctx, s.dbh)
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

// gatePublishFleetApprovalAll 是发布面闸(handlers.publishDraft 调用):
// 读草稿 DSL 判定含 mode=all 才查在场宣告,未全宣告 → 503 拒(已写
// body,返回 false)。TOCTOU 边界见文件头。
func (s *Server) gatePublishFleetApprovalAll(c *gin.Context, tenant uint64, id uint64) bool {
	def, err := s.st.GetByID(c.Request.Context(), s.dbh, tenant, id)
	if err != nil {
		mapStoreError(c, err)
		return false
	}
	usesAll, err := dslUsesApprovalAll(def.DSL)
	if err != nil {
		slog.Error("api: fleet gate parse draft dsl", "err", err, "definition_id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		return false
	}
	if !usesAll {
		return true
	}
	rej := s.gateFleetApprovalAll(c.Request.Context())
	if rej == nil {
		return true
	}
	s.noteFleetGateRejection("publish", rej)
	c.JSON(http.StatusServiceUnavailable, fleetGateRejectionBody(rej))
	return false
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
	rej := s.gateFleetApprovalAll(ctx)
	if rej == nil {
		return nil
	}
	s.noteFleetGateRejection("trigger", rej)
	return &triggerError{code: triggerFleetGate, err: fmt.Errorf(
		"fleet gate: %d in-fleet instance(s) missing %s", len(rej.Undeclared), snowflake.CapApprovalModeAll)}
}
