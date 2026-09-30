package api

// IM 鉴权短链入口(T14.3,D-2026-09-30-t143 决策 4):GET /api/v1/im/
// approval?d=…&s=…——钉钉审批卡片按钮的直达动作面,挂 authed 组外
//(凭据 = HMAC capability 本身,无 Bearer/无登录)。决定语义与 Bearer
// POST 同源:验证后进 applyApprovalDecision 单点(锁行/名单门/重放裁决/
// 定局守卫/落票/resume 全一致),入口只做凭据换装与 HTML 结果渲染。
//
// 响应面 = 内联极小 HTML(零 JS/零第三方资产,CSP default-src 'none' +
// X-Frame-Options DENY + no-store——页面反映落票事实且 IM webview 有
// 缓存面)。信任边界(D-t143 决策 6 登记):短链证明「持有者可代所指
// approver 投票」,不证明点击者身份——群成员可点任意按钮,票落
// decided_by = 所指 approver,审计 payload via=im_link 归因可见。
// 已知边界:按钮直达无确认页,误触 = 重放 200/改判 409 结果页兜底;
// trace span 不在本面(authed 组外)——决策行自身 trace_id(请求段)为
// 空,但 linked_trace_id 仍显式链回执行主链 root,审计串联不受影响
//(对抗审计 LOW 修:原表述写反了空列);定局票的 execution.resume 事件
// 携空 trace 段 ⇒ worker 按孤儿链口径短路,续跑段在 trace 视图与主链
// 断开、仅审计面经 linked_trace_id 串联(既有孤儿链兜底,声明即闭环)。

import (
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"braid/internal/auth"
)

// imApproval:GET /api/v1/im/approval。
func (s *Server) imApproval(c *gin.Context) {
	// 通道关闭面:api 未配 BRAID_IM_LINK_SECRET ⇒ 无在野链接可验,404
	//(路由恒挂载——配置开关不进路由表,部署面零漂移)。
	if s.cfg.IMLinkSecret == "" {
		renderIMPage(c, http.StatusNotFound, "链接不可用", "本部署未启用 IM 审批入口。")
		return
	}
	d, sg := c.Query("d"), c.Query("s")
	p, err := auth.VerifyIMLink(s.cfg.IMLinkSecret, d, sg, time.Now())
	switch {
	case errors.Is(err, auth.ErrIMLinkExpired):
		renderIMPage(c, http.StatusGone, "链接已过期",
			"该链接有效期已过(24 小时),请通过审批页或最新通知卡片操作。")
		return
	case err != nil:
		renderIMPage(c, http.StatusForbidden, "链接无效",
			"签名校验未通过,链接可能被篡改或已吊销。")
		return
	}
	// payload 词面闸:action/node_id 是签发面词表,词面外 = 伪造或签发面
	// 漂移,不进决定事务(同样按无效链接面收口,不给伪造面语义信号)。
	dbDecision, face, ok := decisionFromRequest(p.Action)
	if !ok {
		renderIMPage(c, http.StatusForbidden, "链接无效", "签名校验未通过,链接可能被篡改或已吊销。")
		return
	}
	if !nodeIDRe.MatchString(p.NodeID) {
		renderIMPage(c, http.StatusForbidden, "链接无效", "签名校验未通过,链接可能被篡改或已吊销。")
		return
	}

	out, replay, err := s.applyApprovalDecision(c.Request.Context(), decisionCall{
		ExecID: p.ExecutionID, NodeID: p.NodeID, Actor: p.Approver,
		Decision: dbDecision, Via: "im_link",
		LinkRound: p.Round, LinkNodeExecID: p.NodeExecID,
	})
	if err != nil {
		s.renderIMDecisionError(c, err, p)
		return
	}
	if replay {
		renderIMPage(c, http.StatusOK, "已投过票",
			fmt.Sprintf("%s 此前已%s该审批(执行 #%d 节点 %s),本次点击未产生新写入。",
				p.Approver, imFaceZh(face), p.ExecutionID, p.NodeID))
		return
	}
	detail := fmt.Sprintf("%s 已%s(执行 #%d 节点 %s,第 %d 轮)。",
		p.Approver, imFaceZh(face), p.ExecutionID, p.NodeID, out.Round)
	if out.Final {
		detail += "本轮已定局,工作流将继续。"
	} else {
		detail += "等待其余审批人投票。"
	}
	renderIMPage(c, http.StatusOK, "投票已记录", detail)
}

// renderIMDecisionError 把决定事务哨兵映射到结果页(与 mapApprovalError
// 同风格;HTML 面文案面向点击者,不透出内部错误串)。
func (s *Server) renderIMDecisionError(c *gin.Context, err error, p auth.IMLinkPayload) {
	switch {
	case errors.Is(err, errDeciderRace):
		// 并发双投败方读回(与 resolveDeciderRace 同语义,HTML 面)。
		replay, _, rerr := s.deciderRaceReadback(c.Request.Context(), p.ExecutionID, p.NodeID, p.Approver, imActionDB(p.Action))
		if rerr != nil {
			renderIMPage(c, http.StatusInternalServerError, "暂时无法处理", "请稍后重试或通过审批页操作。")
			return
		}
		if replay {
			renderIMPage(c, http.StatusOK, "已投过票",
				fmt.Sprintf("%s 此前已按同一方向投票,本次点击未产生新写入。", p.Approver))
			return
		}
		renderIMPage(c, http.StatusConflict, "投票冲突",
			fmt.Sprintf("%s 此前已投过相反方向的票,同一轮内不可改判。", p.Approver))
	case errors.Is(err, ErrLinkRoundStale):
		renderIMPage(c, http.StatusConflict, "链接已失效",
			"该链接所属的投票轮次已过(审批可能已被回滚重挂),请使用最新通知卡片。")
	case errors.Is(err, ErrAlreadyDecided):
		renderIMPage(c, http.StatusConflict, "该审批已定局", "本轮投票已定局,不再接受新票。")
	case errors.Is(err, ErrDecisionConflict):
		renderIMPage(c, http.StatusConflict, "投票冲突",
			fmt.Sprintf("%s 此前已投过相反方向的票,同一轮内不可改判。", p.Approver))
	case errors.Is(err, ErrNotApprover):
		// 名单门拒绝落审计(issue #33 硬约束,Bearer 路径同款;orch-review
		// LOW 修正:短链入口不对称缺口)。actor = 短链所指 approver
		//(签名有效 ⇒ 平台签发面的事实主体;名单外 = 签发后名单变更或
		// 签发面漂移,拒绝尝试是事实)。锚定闸(writeDecisionDeniedAudit
		// 内部)保证只在真实挂起审批节点上留痕。
		s.writeDecisionDeniedAudit(c, p.ExecutionID, p.NodeID, p.Approver, p.Action, "membership")
		renderIMPage(c, http.StatusForbidden, "无权投票",
			fmt.Sprintf("%s 不在该审批的审批人名单内。", p.Approver))
	case errors.Is(err, ErrNotWaiting), errors.Is(err, ErrExecTerminal), errors.Is(err, ErrNotApproval):
		renderIMPage(c, http.StatusConflict, "该审批已收口", "该节点已不在等待审批状态。")
	case errors.Is(err, ErrNotFound):
		renderIMPage(c, http.StatusNotFound, "审批不存在", "链接指向的执行或节点不存在。")
	default:
		s.logInternalIMError(c, err, p)
		renderIMPage(c, http.StatusInternalServerError, "暂时无法处理", "请稍后重试或通过审批页操作。")
	}
}

// imActionDB 是短链 action 词面 → DB ENUM(与 decisionFromRequest 同源
// 映射;竞速读回需要 DB 词面)。词面已在 imApproval 前置闸裁过,此处
// 不可达臂返零值由读回报错收口。
func imActionDB(action string) string {
	db, _, _ := decisionFromRequest(action)
	return db
}

// imFaceZh 是请求面词表 → 结果页中文动词。
func imFaceZh(face string) string {
	if face == "reject" {
		return "驳回"
	}
	return "批准"
}

// renderIMPage 渲染极小 HTML 结果页:服务端渲染 + 全量转义(动态值只有
// 平台事实与词面受限的 id/approver,仍统一转义——纵深纪律,零 JS 面下
// XSS 结构性不可达,转义是防御层不是补救)。
func renderIMPage(c *gin.Context, status int, title, detail string) {
	c.Header("Content-Security-Policy", "default-src 'none'")
	c.Header("X-Frame-Options", "DENY")
	c.Header("Cache-Control", "no-store")
	body := "<!DOCTYPE html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">" +
		"<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">" +
		"<title>" + html.EscapeString(title) + "</title></head><body>" +
		"<h1>" + html.EscapeString(title) + "</h1><p>" + html.EscapeString(detail) + "</p>" +
		"<p>braid 工作流平台</p></body></html>"
	c.Data(status, "text/html; charset=utf-8", []byte(body))
}

// logInternalIMError:非哨兵错误零遥测不可接受(内部故障静默 = 白屏无
// 信号);错误串不含短链秘密(验证面只收 d/s,不持密钥值)。
func (s *Server) logInternalIMError(c *gin.Context, err error, p auth.IMLinkPayload) {
	slog.Error("api: im approval decision failure",
		"err", err, "execution_id", p.ExecutionID, "node_id", p.NodeID,
		"approver", p.Approver, "round", p.Round)
}
