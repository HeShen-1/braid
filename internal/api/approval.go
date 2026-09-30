// 审批面 handlers(T4.2):GET /executions/:id/approvals/:node_id(审批人可读
// 快照面,issue #33 钦定)+ POST /executions/:id/approvals/:node_id/decision
// (或签决定:第一票定局)。写路径单事务 = 锁 node 行 → 守卫链 → 落
// approval_decision 行 + audit approval.decided + outbox execution.resume
// (task_id=resume:<exec>:<node>:<attempt>,0003 词表)——三写同存亡,
// 续跑权威在 worker 侧 scheduler.Resume(事件驱动,ARCHITECTURE §3.3)。
//
// 鉴权(T5.2 换装后形态,D-t42 §7):POST 的门 = approver|admin 角色闸
// (in-handler,拒绝 403 role_denied + 落 approval.decision_denied 审计行
// reason=role_denied)叠加 DSL approvers[](快照真相源,越权 403 +
// 同事件 reason=membership,issue #33 硬约束;DSL 名单仍是执行时权威)。
// GET 读面收窄到名单内审批人(快照可证名单;含 comment 自由文本的
// decisions[] 时间线不再对任意有效 token 开放);未挂起(无快照)
// fail-closed 403,节点状态读面仍在 GET /executions/:id。
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"

	"braid/internal/audit"
	"braid/internal/auth"
	"braid/internal/engine"
	"braid/internal/outbox"
	"braid/internal/scheduler"
)

// nodeIDRe 镜像 DSL 节点 id 词表(dsl/refs.go 同款):形态非法 400 区别于
// 404(与 bad_id/bad_version 哲学一致)。
var nodeIDRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// maxApprovalComment 是决定备注上限(rune 计——utf8mb4 VARCHAR(2000) 的
// 字符语义;超限 400 拒绝,不静默截断:审批人输入是事实,截断即伪造)。
const maxApprovalComment = 2000

// decideRequest 是 POST 决定请求体。decision 用请求面词表 approve/reject
// (DB 侧 ENUM APPROVED/REJECTED 的映射单点在 decisionFromRequest)。
type decideRequest struct {
	Decision string `json:"decision"`
	Comment  string `json:"comment"`
}

// decisionFromRequest 把请求面词表映射到 DDL ENUM(0003);第二返回值是
// 审计 payload 用的请求面值。
func decisionFromRequest(v string) (db string, face string, ok bool) {
	switch v {
	case "approve":
		return scheduler.DecisionApproved, "approve", true
	case "reject":
		return scheduler.DecisionRejected, "reject", true
	}
	return "", "", false
}

// decisionDTO 是决定行视图(ID 字符串化契约同 definitionDTO,2^53 精度)。
type decisionDTO struct {
	ID          string    `json:"id"`
	ExecutionID string    `json:"execution_id"`
	NodeID      string    `json:"node_id"`
	Round       int       `json:"round"`    // T9.2:投票轮次(挂起所属 attempt 号;跨轮时间线分组维度)
	Decision    string    `json:"decision"` // approve | reject(请求面词表)
	DecidedBy   string    `json:"decided_by"`
	Comment     *string   `json:"comment"`
	DecidedAt   time.Time `json:"decided_at"`
	// Final 是 POST 语义(T14.2,t14.1 画布数据契约):本票落库后本轮是否
	// 定局(any:首票;all:任一驳回或全员批准)。定局票 = true;非定局
	// 中间态票 = 缺省(omitempty,GET 时间线与竞速败者回显恒不带)。
	Final bool `json:"final,omitempty"`
}

func toDecisionDTO(d Decision) decisionDTO {
	return decisionDTO{
		ID:          strconv.FormatUint(d.ID, 10),
		ExecutionID: strconv.FormatUint(d.ExecutionID, 10),
		NodeID:      d.NodeID,
		Round:       d.Round, // T9.2:跨轮时间线的分组维度(回滚重挂后旧轮票对照)
		Decision:    decisionFace(d.Decision),
		DecidedBy:   d.DecidedBy,
		Comment:     d.Comment,
		DecidedAt:   d.DecidedAt,
	}
}

// decisionFace 是 DB ENUM → 请求面词表的反向映射(读面回显;未知值原样
// 透传——ENUM 域外不可达,回显即对账信号)。
func decisionFace(db string) string {
	switch db {
	case scheduler.DecisionApproved:
		return "approve"
	case scheduler.DecisionRejected:
		return "reject"
	}
	return db
}

// errDeciderRace 是决定事务内的哨兵:InsertDecision 撞 uk(同审批人并发
// 双投恰一胜)。事务回滚后在事务外读回裁决(重放/改判)——MySQL 语句失败
// 不必然废整个事务,但保持「哨兵即回滚」的简单纪律(与 scheduler.
// errAttemptAlreadyClosed 同形)。
var errDeciderRace = errors.New("api: decision insert raced with same approver")

// decideApproval:POST /executions/:id/approvals/:node_id/decision。
func (s *Server) decideApproval(c *gin.Context) {
	execID, ok := parseIDParam(c)
	if !ok {
		return
	}
	nodeID := c.Param("node_id")
	if !nodeIDRe.MatchString(nodeID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_node_id"})
		return
	}
	var req decideRequest
	b, ok := readBodyLimited(c)
	if !ok {
		return
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
			return
		}
	}
	dbDecision, face, ok := decisionFromRequest(req.Decision)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_decision"})
		return
	}
	if n := len([]rune(req.Comment)); n > maxApprovalComment {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_comment"})
		return
	}
	actor := actorFrom(c)

	// 角色闸(T5.2 换装①,D-t42 §7):in-handler 而非路由中间件——拒绝
	// 需落审计 + 分面错误码,中间件面做不到。角色集 approver|admin 与
	// definitions/trigger 写面的 admin 豁免惯例一致;角色面在名单门之前
	// (claims-only 零 DB,快速失败),DSL 名单仍是执行时权威。已知取舍
	// (sec 评审 LOW):claims 不回查 DB,被撤角色的 token 在过期前仍过
	// 角色闸(平台级 DISABLED 窗口,auth/config.go 同记)——名单门 +
	// 快照真相源仍在,执行时 RBAC(execauth)对工具调用是现查现判。
	if claims, ok := ClaimsFrom(c); !ok || !claimsHaveAnyRole(claims, auth.RoleApprover, auth.RoleAdmin) {
		// 落行闸(D-t112 审计收口):拒绝审计只对本租户可见执行落——
		// 跨租户/不存在执行上落行会把外方主体注入受害者租户的证据链
		//(403 响应面不变,证据面与「跨租户直取 = 404」契约对齐)。
		if s.execVisibleInTenant(c, execID) {
			s.writeDecisionDeniedAudit(c, execID, nodeID, actor, face, "role_denied")
		}
		c.JSON(http.StatusForbidden, gin.H{"error": "role_denied"})
		return
	}

	// 执行存在性前置(404 面;终态/快照守卫在事务内权威裁决)。
	if _, err := s.st.GetExecution(c.Request.Context(), s.dbh, tenantFrom(c), execID); err != nil {
		mapStoreError(c, err)
		return
	}

	out, replay, err := s.applyApprovalDecision(c.Request.Context(), decisionCall{
		ExecID: execID, NodeID: nodeID, Actor: actor,
		Decision: dbDecision, Comment: req.Comment,
		LinkRound: -1, // Bearer 路径无短链轮绑定(守卫面见 decisionCall)
	})
	if err != nil {
		if errors.Is(err, errDeciderRace) {
			s.resolveDeciderRace(c, execID, nodeID, actor, dbDecision)
			return
		}
		if errors.Is(err, ErrNotApprover) {
			// 越权拒绝必须落审计(issue #33 硬约束)。独立小事务:主事务
			// 已回滚,拒绝事实不与任何决定写同存亡。审计失败仅 slog——403
			// 的裁决不受旁路写入失败影响。
			s.writeDecisionDeniedAudit(c, execID, nodeID, actor, face, "membership")
			mapApprovalError(c, err)
			return
		}
		mapApprovalError(c, err)
		return
	}
	if replay {
		c.JSON(http.StatusOK, out)
		return
	}
	c.JSON(http.StatusCreated, out)
}

// decisionCall 是决定事务单点的调用参数(T14.3 抽核,D-t143 决策 4):
// HTTP Bearer 决定与 IM 鉴权短链决定两入口共消费——锁行/名单门/重放
// 裁决/定局守卫/落票/审计/resume 语义单点,入口层只差「凭据怎么来」,
// 两路行为永不分叉(判据单点从 scheduler.ApprovalOutcome 延伸到投票
// 入口层)。LinkRound/LinkNodeExecID 是短链绑定面(LinkRound<0 = 无轮
// 绑定,Bearer 路径;短链路径恒 ≥0):轮变 ⇒ ErrLinkRoundStale 不落票
// (撤销语义细粒度半边),node_execution 错配 ⇒ 数据缺陷响亮失败。
type decisionCall struct {
	ExecID         uint64
	NodeID         string
	Actor          string // 投票主体(Bearer sub 或短链所指 approver)
	Decision       string // DB ENUM(APPROVED/REJECTED)
	Comment        string
	Via            string // 审计归因:""(Bearer)| "im_link"
	LinkRound      int
	LinkNodeExecID uint64
}

// applyApprovalDecision 是决定事务单点(原 decideApproval 事务体原样
// 抽出,守卫链语义零改动;注释随体迁移)。IM 短链入口经 ctx 无 trace
// span(authed 组外)——决策行自身 trace_id(请求段)为空,但
// linked_trace_id 仍从事务内 rootTrace 落值链回执行主链,审计串联
// 不受影响(对抗审计修正:原注释把空列写成 linked_trace_id,失真)。
func (s *Server) applyApprovalDecision(ctx context.Context, call decisionCall) (out decisionDTO, replay bool, err error) {
	err = WithTx(ctx, s.dbh, func(tx DBTX) error {
		execID, nodeID, actor, dbDecision := call.ExecID, call.NodeID, call.Actor, call.Decision
		node, err := s.st.lockApprovalNode(ctx, tx, execID, nodeID, true)
		if err != nil {
			return err
		}
		// 短链 node_execution 绑定(数据缺陷族:链接指向的 node 行与当前
		// 锁行错配——签发后节点行被替换,响亮失败不落票)。
		if call.LinkNodeExecID != 0 && call.LinkNodeExecID != node.NodeExecID {
			return fmt.Errorf("api: im link node_exec %d != locked %d (data defect)",
				call.LinkNodeExecID, node.NodeExecID)
		}
		if node.Type != "approval" {
			return ErrNotApproval
		}
		// 快照缺失前置:PENDING(未挂起,快照恒 NULL)不可决定——409 面
		// 而非把 NULL 喂给名单解析报 500(engine 不变量:WAITING ⇒ NOT NULL,
		// 故到达此分支必非可决定态)。
		if len(node.Snapshot) == 0 {
			return ErrNotWaiting
		}
		// 快照解析:approvers 名单真相源(WAITING ⇒ NOT NULL 的 engine
		// 不变量;已挂起节点的坏快照 → 500 面,不伪造名单)。
		var snap scheduler.ApprovalInput
		if err := json.Unmarshal(node.Snapshot, &snap); err != nil {
			return fmt.Errorf("api: approval snapshot of node %q: %w", nodeID, err)
		}
		if !containsString(snap.Approvers, actor) {
			return ErrNotApprover
		}
		// mode 词面响亮兜底(T14.2):schema enum 发布期钉死 any|all,快照
		// 漂移到词面外 → 500,不静默按 any 裁决(判据单点对词面的前置)。
		if snap.Mode != "" && snap.Mode != scheduler.ApprovalModeAny && snap.Mode != scheduler.ApprovalModeAll {
			return fmt.Errorf("api: approval snapshot of node %q mode %q outside vocabulary", nodeID, snap.Mode)
		}
		// 轮次锚(T9.2/D-t92 决策 6):round = 锁行 attempts(挂起所属轮),
		// 与 resume 事件第四段(attempts+1)同源单点、恒差 1;旧轮票对
		// 新轮不可见(谓词承载,消费点零漏改面)。
		round := int(node.Attempts)
		// 短链轮绑定(T14.3):链接签发于第 N 轮而节点已进入第 N+k 轮
		//(回滚重挂)⇒ 旧轮链接结构性失效——不把旧轮票落进新轮。
		if call.LinkRound >= 0 && call.LinkRound != round {
			return ErrLinkRoundStale
		}
		// 重放/改判裁决先行(先于终态/状态守卫:已定局的执行 wf 可能已
		// 终态,重放语义必须胜出 409 面)。重放行的 final 按本轮计数补判
		//(锁内读数;非定局票重放恒缺省)。
		approved, rejected, err := s.st.CountRoundDecisions(ctx, tx, node.NodeExecID, round)
		if err != nil {
			return err
		}
		if prior, err := s.st.GetDecisionByApprover(ctx, tx, node.NodeExecID, round, actor); err == nil {
			if prior.Decision == dbDecision {
				out = toDecisionDTO(prior)
				out.Final, _ = scheduler.ApprovalOutcome(snap.Mode, len(snap.Approvers), approved, rejected)
				replay = true // 重放:响应行回显,零新写(200 面)
				return nil
			}
			return ErrDecisionConflict
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		// 定局守卫(T14.2 会签判据,语义单点 scheduler.ApprovalOutcome——
		// D-t42 预告的 CountDecisions 判据改法):本轮既存票已定局(any:
		// 首票;all:任一驳回或全员批准)→ 后续票 409。已投者的重放/改判
		// 已在上方先裁(同人语义优先于定局面)。
		if fin, _ := scheduler.ApprovalOutcome(snap.Mode, len(snap.Approvers), approved, rejected); fin {
			return ErrAlreadyDecided
		}
		// 节点状态守卫先于执行终态(更贴近审批面的裁决码:已收口的节点
		// 报 not_waiting,执行终态是它的下游事实)。
		if node.Status != string(engine.NodeWaitingApproval) {
			return ErrNotWaiting
		}
		// 执行终态守卫(fail-fast 兄弟/取消竞速:迟到决定不产生新语义,
		// worker 侧 resumeOnce 终态守卫是权威镜像)。trace_id 同语句读出:
		// 决策行的 linked_trace_id 段首单点落值(T6.2,审批请求 trace Z
		// 链指回执行主链 root X——D-2026-09-27-t62-otel-trace)。
		var wfStatus string
		var rootTrace sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT status, trace_id FROM workflow_execution WHERE id=?`, execID).
			Scan(&wfStatus, &rootTrace); err != nil {
			return fmt.Errorf("api: read execution status %d: %w", execID, err)
		}
		if engine.WorkflowState(wfStatus).Terminal() {
			return ErrExecTerminal
		}
		decisionID, err := s.ids.Next()
		if err != nil {
			return fmt.Errorf("api: gen decision id: %w", err)
		}
		if err := s.st.InsertDecision(ctx, tx, InsertDecisionParams{
			ID: decisionID, ExecutionID: execID, NodeExecID: node.NodeExecID,
			NodeID: nodeID, Round: round, DecidedBy: actor, Decision: dbDecision, Comment: call.Comment,
		}); err != nil {
			if errors.Is(err, ErrConflict) {
				return errDeciderRace // 并发双投:事务外读回裁决
			}
			return err
		}
		// 迁移零(node 不动:WAITING_APPROVAL 的状态权威在 worker 续跑事务);
		// 审计同事务(D-t26 调用方契约)。每票一行 approval.decided——
		// 会签中间态票同样是决定事实(部分批准时间线)。via 是入口归因
		//(T14.3:"" = Bearer,im_link = 鉴权短链)。
		if err := s.appendApprovalAudit(ctx, tx, execID, node.NodeExecID, nodeID, actor, decisionFace(dbDecision), snap.Mode, rootTrace.String, call.Via); err != nil {
			return err
		}
		// 定局判定重查(含本票;node 行锁内计数稳定):定局票(any 首票 /
		// all 任一驳回 / all 末票批准)→ resume 事件(四段式,末段
		// attempts+1);非定局中间态票 → 零续跑意图,节点保持
		// WAITING_APPROVAL——部分批准中间态,续跑意图只在定局票产生
		//(T14.2;判据单点 scheduler.ApprovalOutcome)。
		approved, rejected, err = s.st.CountRoundDecisions(ctx, tx, node.NodeExecID, round)
		if err != nil {
			return err
		}
		final, _ := scheduler.ApprovalOutcome(snap.Mode, len(snap.Approvers), approved, rejected)
		if final {
			if err := s.appendResumeEvent(ctx, tx, execID, node.NodeExecID, nodeID, node.Attempts+1, decisionID); err != nil {
				return err
			}
		}
		decided, err := s.st.GetDecisionByApprover(ctx, tx, node.NodeExecID, round, actor)
		if err != nil {
			return err
		}
		out = toDecisionDTO(decided)
		out.Final = final
		return nil
	})
	return out, replay, err
}

// resolveDeciderRace:同审批人并发双投的败方读回(胜方已提交):同决定 →
// 200 重放;异决定 → 409 改判。读回裁决抽在 deciderRaceReadback(T14.3:
// IM 短链入口同语义消费)。
func (s *Server) resolveDeciderRace(c *gin.Context, execID uint64, nodeID, actor, dbDecision string) {
	replay, prior, err := s.deciderRaceReadback(c.Request.Context(), execID, nodeID, actor, dbDecision)
	if err != nil {
		mapApprovalError(c, err)
		return
	}
	if replay {
		c.JSON(http.StatusOK, toDecisionDTO(prior))
		return
	}
	mapApprovalError(c, ErrDecisionConflict)
}

// deciderRaceReadback 是竞速败方的读回裁决(T14.3 抽出)。轮次锚定读回
// (T9.2):并发双投必同轮(同一次挂起),按当轮 attempts 查,round-1
// 旧行结构性不可达——旧形态「读回命中旧轮行 → 假 200/误 409」的实害
// 路径封闭。
func (s *Server) deciderRaceReadback(ctx context.Context, execID uint64, nodeID, actor, dbDecision string) (bool, Decision, error) {
	node, err := s.st.lockApprovalNode(ctx, s.dbh, execID, nodeID, false)
	if err != nil {
		return false, Decision{}, err
	}
	prior, err := s.st.GetDecisionByApprover(ctx, s.dbh, node.NodeExecID, int(node.Attempts), actor)
	if err != nil {
		return false, Decision{}, err
	}
	return prior.Decision == dbDecision, prior, nil
}

// getApproval:GET /executions/:id/approvals/:node_id —— 名单内审批人可读
// 面(T5.2 换装②,D-t42 §7):节点状态 + 挂起快照(approvers/mode/
// instructions)+ 截止时刻 + 已决票时间线(含 comment 自由文本——收窄
// 动机:这是 comment 全文唯一暴露面,不再对任意有效 token 开放)。读面
// 无事务(与 GET /executions/:id 同语义:钉版契约下并发迁移间的轻微不
// 一致 P0 可接受)。
func (s *Server) getApproval(c *gin.Context) {
	execID, ok := parseIDParam(c)
	if !ok {
		return
	}
	nodeID := c.Param("node_id")
	if !nodeIDRe.MatchString(nodeID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_node_id"})
		return
	}
	// 执行存在性前置(404 面)+ 租户闸(D-t112:lockApprovalNode 无租户
	// 列,节点读面经 execution 锚派生——本前置是 [S2] 的第三调用面,
	// 曾被本窗批量补丁误删、由 approval_detail_cross_tenant_404 对抗
	// 用例当场抓住,恢复即回归锚)。
	if _, err := s.st.GetExecution(c.Request.Context(), s.dbh, tenantFrom(c), execID); err != nil {
		mapStoreError(c, err)
		return
	}
	node, err := s.st.lockApprovalNode(c.Request.Context(), s.dbh, execID, nodeID, false)
	if err != nil {
		mapStoreError(c, err)
		return
	}
	if node.Type != "approval" {
		mapApprovalError(c, ErrNotApproval)
		return
	}
	// 读面收窄(T5.2 换装②):名单 = 挂起快照的 approvers[](真相源同
	// POST)。未挂起(快照 NULL)无名单可证 → fail-closed 403,节点状态
	// 读面不丢(GET /executions/:id 开放);坏快照 → 500 不伪造名单
	// (POST 同纪律:WAITING ⇒ 快照可解析是 engine 不变量)。
	if len(node.Snapshot) == 0 {
		mapApprovalError(c, ErrNotApprover)
		return
	}
	var snap scheduler.ApprovalInput
	if err := json.Unmarshal(node.Snapshot, &snap); err != nil {
		slog.Error("api: approval snapshot unreadable", "err", err, "execution_id", execID, "node_id", nodeID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		return
	}
	if !containsString(snap.Approvers, actorFrom(c)) {
		mapApprovalError(c, ErrNotApprover)
		return
	}
	decisions, err := s.st.ListDecisions(c.Request.Context(), s.dbh, node.NodeExecID)
	if err != nil {
		mapStoreError(c, err)
		return
	}
	// 会签中间态读面(T14.2,t14.1 画布的数据契约):pending_approvers =
	// 名单 − 本轮已投者,仅在节点 WAITING_APPROVAL 时非空(round = 读行
	// attempts,与 ListDecisions 跨轮时间线按 Round 分组对齐;读面无事务,
	// 钉版契约同款近似)。已知尖锐点(对抗审计登记,D-t142 定局异步窗):
	// 定局票落库后 worker 续跑前的窗口内节点仍 WAITING——已定局轮的
	// 未投者仍出现在本字段与待办列表,其投票将 409 already_decided(窗口
	// 有界);收口后恒空数组(字段恒在场,契约稳定)。
	pending := []string{}
	if node.Status == string(engine.NodeWaitingApproval) {
		voted := make(map[string]bool, len(decisions))
		for _, d := range decisions {
			if d.Round == int(node.Attempts) {
				voted[d.DecidedBy] = true
			}
		}
		for _, a := range snap.Approvers {
			if !voted[a] {
				pending = append(pending, a)
			}
		}
	}
	detail := approvalDetailDTO{
		ExecutionID:      strconv.FormatUint(execID, 10),
		NodeID:           node.NodeID,
		NodeStatus:       node.Status,
		DeadlineAt:       node.DeadlineAt,
		Approvers:        snap.Approvers,
		Mode:             snap.Mode,
		TimeoutHours:     snap.TimeoutHours,
		OnTimeout:        snap.OnTimeout,
		Instructions:     snap.Instructions,
		PendingApprovers: pending,
		Decisions:        []decisionDTO{},
	}
	for _, d := range decisions {
		detail.Decisions = append(detail.Decisions, toDecisionDTO(d))
	}
	c.JSON(http.StatusOK, detail)
}

// approvalDetailDTO 是审批详情视图:状态 + 挂起声明面 + 决定时间线。
type approvalDetailDTO struct {
	ExecutionID      string        `json:"execution_id"`
	NodeID           string        `json:"node_id"`
	NodeStatus       string        `json:"node_status"`
	DeadlineAt       *time.Time    `json:"deadline_at"`
	Approvers        []string      `json:"approvers,omitempty"`
	Mode             string        `json:"mode,omitempty"`
	TimeoutHours     float64       `json:"timeout_hours,omitempty"`
	OnTimeout        string        `json:"on_timeout,omitempty"`
	Instructions     string        `json:"instructions,omitempty"`
	PendingApprovers []string      `json:"pending_approvers"` // T14.2:本轮未投名单(会签中间态;非 WAITING 恒空数组)
	Decisions        []decisionDTO `json:"decisions"`
}

// appendApprovalAudit 落 approval.decided 审计行(决定事务内;actor=user/
// 决定者;node_id 携目标节点;attempt 恒 NULL——决定先于续跑 attempt 创建)。
// linkedTrace 是执行主链 root trace(workflow_execution.trace_id):审批链
// 段首行单点落 linked_trace_id(T6.2)——本行 trace_id = 决策请求 trace Z
// (ctx 自动提取),Z 链头部一行指回 X,两链由此串联。via 是入口归因
// (T14.3:"" = Bearer,im_link = 鉴权短链;决定者身份仍在行级 actor_id)。
func (s *Server) appendApprovalAudit(ctx context.Context, tx DBTX,
	execID, nodeExecID uint64, nodeID, actor, face, mode, linkedTrace, via string) error {
	auditID, err := s.ids.Next()
	if err != nil {
		return fmt.Errorf("api: gen audit id: %w", err)
	}
	raw, err := json.Marshal(audit.ApprovalDecidedPayload{
		NodeID: nodeID, Decision: face, Mode: mode, Via: via,
	})
	if err != nil {
		return fmt.Errorf("api: marshal approval.decided payload: %w", err)
	}
	return audit.Append(ctx, tx, audit.Event{
		ID: auditID, EventType: audit.EventApprovalDecided,
		ExecutionID: execID, NodeExecID: nodeExecID, NodeID: nodeID,
		ActorType: audit.ActorUser, ActorID: actor,
		LinkedTraceID: linkedTrace, Payload: raw,
	})
}

// writeDecisionDeniedAudit 落 approval.decision_denied 审计行(独立小事务;
// 失败仅 slog,不影响 403 裁决)。reason 区分拒绝面:membership(名单外)
// / role_denied(名单内缺角色,T5.2 换装①)。
//
// 灌水防线(T5.2 sec 评审 MEDIUM + 对抗审计收紧):角色闸在存在性守卫
// 之前——任意持 token 主体可对任意 (exec, node_id) 组合触发 403。审计行
// 只在锚定到「真实 approval 节点且快照在册」(= membership 审计的同一
// 谓词面:挂起过、名单可证)时落:幻影 node_id / 非 approval / 未挂起
// PENDING 节点的拒绝裁决照常生效但不留痕——append-only 审计面不混入
// 无名单可证的行。真实挂起节点上的重复拒绝仍逐行留痕(每次尝试都是
// 事实,与 membership 路径同语义);无界灌水的最终闸是基础设施层限流
// (T6.4 域,本面不伪造「不可灌水」承诺)。锚读瞬态故障 = best-effort
// 跳过但必须留 slog(零遥测的静默丢弃是对抗审计 MEDIUM)。
func (s *Server) writeDecisionDeniedAudit(c *gin.Context, execID uint64, nodeID, actor, face, reason string) {
	var nodeExecID uint64
	var nodeType string
	err := s.dbh.QueryRowContext(c.Request.Context(),
		`SELECT id, type FROM node_execution
		 WHERE execution_id=? AND node_id=? AND input_snapshot IS NOT NULL`, execID, nodeID).
		Scan(&nodeExecID, &nodeType)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return // 幻影/未挂起:拒绝裁决生效,审计不落(防灌水)
	case err != nil:
		// 瞬态 DB 故障:裁决不受影响(best-effort 纪律),但静默丢弃零
		// 遥测不可接受——slog 兜底。注意 Scan 出错时 dest 不动,错误分支
		// 必须先于 type 判定(否则 nodeType 零值恒吞掉本分支)。
		slog.Error("api: anchor approval.decision_denied", "err", err, "execution_id", execID, "node_id", nodeID)
		return
	case nodeType != "approval":
		return // 非 approval 节点(有快照的 llm 等):不留痕
	}
	// linked = 执行主链 root trace(T6.2,与 decided 行同款段首落值——
	// 拒绝尝试与决定同是「审批请求作用于执行」的关联行);留痕分支才
	// 点查(拒绝面低频,一次 PK 点查;缺 trace 旧执行 → NULL 合法)。
	var linkedTrace sql.NullString
	if err := s.dbh.QueryRowContext(c.Request.Context(),
		`SELECT trace_id FROM workflow_execution WHERE id=?`, execID).
		Scan(&linkedTrace); err != nil {
		slog.Error("api: anchor approval.decision_denied trace", "err", err, "execution_id", execID)
		return // best-effort 面:瞬态故障跳过但零遥测不可接受(同上)
	}
	auditID, err := s.ids.Next()
	if err != nil {
		slog.Error("api: audit id gen for approval.decision_denied", "err", err, "execution_id", execID)
		return
	}
	raw, err := json.Marshal(audit.ApprovalDecisionDeniedPayload{
		NodeID: nodeID, Attempted: face, Reason: reason,
	})
	if err != nil {
		slog.Error("api: marshal approval.decision_denied payload", "err", err, "execution_id", execID)
		return
	}
	err = WithTx(c.Request.Context(), s.dbh, func(tx DBTX) error {
		return audit.Append(c.Request.Context(), tx, audit.Event{
			ID: auditID, EventType: audit.EventApprovalDecisionDenied,
			ExecutionID: execID, NodeExecID: nodeExecID, NodeID: nodeID,
			ActorType: audit.ActorUser, ActorID: actor,
			LinkedTraceID: linkedTrace.String, Payload: raw,
		})
	})
	if err != nil {
		slog.Error("api: audit approval.decision_denied", "err", err, "execution_id", execID)
	}
}

// appendResumeEvent 落 execution.resume outbox 事件(决定事务内;task_id
// 四段式词表,attempt 段 = 续跑将创建的 attempt 号——锁行读出的确定性值)。
func (s *Server) appendResumeEvent(ctx context.Context, tx DBTX,
	execID, nodeExecID uint64, nodeID string, attempt uint16, decisionID uint64) error {
	// TraceID = 决策请求 trace Z(T6.2 段首新链:worker 消费侧 extract 续
	// span,续跑段行 trace_id=Z;主链 X 经决策行 linked_trace_id 串联)。
	// SpanID = 同请求当段 span id(D-t121 #3:续跑段根的真 parent)。
	var segTrace, segSpan string
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		segTrace = sc.TraceID().String()
		segSpan = sc.SpanID().String()
	}
	raw, err := json.Marshal(scheduler.ResumePayload{
		ExecutionID: execID, NodeExecutionID: nodeExecID,
		DecisionID: decisionID, AttemptNo: attempt, TraceID: segTrace, SpanID: segSpan,
	})
	if err != nil {
		return fmt.Errorf("api: marshal execution.resume payload: %w", err)
	}
	eventID, err := s.ids.Next()
	if err != nil {
		return fmt.Errorf("api: gen resume event id: %w", err)
	}
	return outbox.Append(ctx, tx, outbox.Event{
		ID:          eventID,
		ExecutionID: execID,
		EventType:   scheduler.EventTypeExecutionResume,
		TaskID:      fmt.Sprintf("resume:%d:%s:%d", execID, nodeID, attempt),
		Queue:       s.cfg.ResumeQueue,
		Payload:     raw,
	})
}

// mapApprovalError 把审批面哨兵映射到 HTTP 契约(与 mapStoreError 同风格;
// 审批面错误码显式单列,客户端可编程区分定局语义)。
func mapApprovalError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
	case errors.Is(err, ErrNotApprover):
		c.JSON(http.StatusForbidden, gin.H{"error": "not_approver"})
	case errors.Is(err, ErrNotApproval):
		c.JSON(http.StatusConflict, gin.H{"error": "not_approval"})
	case errors.Is(err, ErrNotWaiting):
		c.JSON(http.StatusConflict, gin.H{"error": "not_waiting"})
	case errors.Is(err, ErrAlreadyDecided):
		c.JSON(http.StatusConflict, gin.H{"error": "already_decided"})
	case errors.Is(err, ErrDecisionConflict):
		c.JSON(http.StatusConflict, gin.H{"error": "decision_conflict"})
	case errors.Is(err, ErrExecTerminal):
		c.JSON(http.StatusConflict, gin.H{"error": "execution_terminal"})
	case errors.Is(err, ErrConflict):
		c.JSON(http.StatusConflict, gin.H{"error": "conflict"})
	default:
		slog.Error("api: approval store failure", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
	}
}

// containsString 报告名单是否含主体(或签成员判定;线性扫描,名单 ≤64)。
func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
