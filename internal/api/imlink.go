package api

// IM 鉴权短链入口(T14.3,D-2026-09-30-t143 决策 4;T17.5 确认页第二跳,
// D-2026-10-04-t175 决策 1):/api/v1/im/approval?d=…&s=…——钉钉审批卡片
// 按钮的落点,挂 authed 组外(凭据 = HMAC capability 本身,无 Bearer/无
// 登录)。GET = 只读确认页(验签 + 展示签名事实,零 DB 读零写入——链路
// 扫描器 SafeLinks/unfurl 只发 GET,结构性拿不到落票面,预取闭合);
// POST = 表单提交进 applyApprovalDecision 单点(锁行/名单门/重放裁决/
// 定局守卫/落票/resume 全一致,与 Bearer 投票同源),凭据走 form body
// 不进访问日志 query 面。在野旧卡片的 GET 链接平滑兼容(多一跳不失效)。
//
// 响应面 = 内联极小 HTML(零 JS/零第三方资产,CSP default-src 'none' +
// X-Frame-Options DENY + no-store——页面内嵌 capability 表单值且 IM
// webview 有缓存面)。信任边界(D-t143 决策 6 登记):短链证明「持有者
// 可代所指 approver 投票」,不证明点击者身份——群成员可点任意按钮,票落
// decided_by = 所指 approver,审计 payload via=im_link 归因可见;确认页
// 显式展示「将以谁的身分投什么票」,比直达形态多一道身份可视面。
// GET 不做决定事务预检(签名有效 = 平台签发过该票意图,已投/已定局在
// POST 结果页呈现,与直达时代行为一致)。CSRF 不适用:凭据 = capability
// 本身,authed 组外无 cookie 会话面。误触边界(直达时代登记)随确认页
// 结构性收窄:GET 打开不落票,提交前有展示面。
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

// imLinkVerify 是两跳共享的凭据闸:通道关闭 404(路由恒挂载——配置开关
// 不进路由表,部署面零漂移)→ 验签(过期 410 / 坏签名 403)→ payload
// 词面闸(action/node_id 是签发面词表,词面外 = 伪造或签发面漂移,按
// 无效链接面收口,不给伪造面语义信号)。错误页已渲染时返回 false。
func (s *Server) imLinkVerify(c *gin.Context, d, sg string) (auth.IMLinkPayload, bool) {
	if s.cfg.IMLinkSecret == "" {
		renderIMPage(c, http.StatusNotFound, "链接不可用", "本部署未启用 IM 审批入口。")
		return auth.IMLinkPayload{}, false
	}
	p, err := auth.VerifyIMLink(s.cfg.IMLinkSecret, d, sg, time.Now())
	switch {
	case errors.Is(err, auth.ErrIMLinkExpired):
		renderIMPage(c, http.StatusGone, "链接已过期",
			"该链接有效期已过(24 小时),请通过审批页或最新通知卡片操作。")
		return p, false
	case err != nil:
		renderIMPage(c, http.StatusForbidden, "链接无效",
			"签名校验未通过,链接可能被篡改或已吊销。")
		return p, false
	}
	if _, _, ok := decisionFromRequest(p.Action); !ok {
		renderIMPage(c, http.StatusForbidden, "链接无效", "签名校验未通过,链接可能被篡改或已吊销。")
		return p, false
	}
	if !nodeIDRe.MatchString(p.NodeID) {
		renderIMPage(c, http.StatusForbidden, "链接无效", "签名校验未通过,链接可能被篡改或已吊销。")
		return p, false
	}
	return p, true
}

// imApprovalConfirm:GET /api/v1/im/approval——第一跳,只读确认页。
func (s *Server) imApprovalConfirm(c *gin.Context) {
	d, sg := c.Query("d"), c.Query("s")
	p, ok := s.imLinkVerify(c, d, sg)
	if !ok {
		return
	}
	renderIMConfirmPage(c, p, d, sg)
}

// imApprovalSubmit:POST /api/v1/im/approval——第二跳,表单提交落票
// (凭据从 form body 读,query 面词面不消费)。
func (s *Server) imApprovalSubmit(c *gin.Context) {
	// 读体上限(对抗审计 MEDIUM/LOW 修):d/s 词面 ≤ 数百字节,4KiB 充裕;
	// 未认证面在验签前解析无上限 body 是内存/临时盘放大面(multipart 被
	// net/http 全量 spool),镜像 readBodyLimited 读体纪律截断。超限 →
	// ParseForm 失败 → PostForm 空 → 验签 403 无效链接面收口,零新错误面。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, imLinkMaxBodyBytes)
	d, sg := c.PostForm("d"), c.PostForm("s")
	p, ok := s.imLinkVerify(c, d, sg)
	if !ok {
		return
	}
	dbDecision, face, _ := decisionFromRequest(p.Action)

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

// imLinkMaxBodyBytes 是第二跳 POST 的读体上限(d/s base64url 词面 ≤ 数百
// 字节,4KiB 含表单编码余量充裕;对抗审计 MEDIUM/LOW 修,镜像 readBodyLimited
// 纪律)。
const imLinkMaxBodyBytes = 4 << 10

// renderIMConfirmPage 渲染只读确认页:展示签名 payload 的公开事实
// (身份/动作/执行/节点/轮次)+ 原生 POST 表单(零 JS;hidden 凭据回传
// ——d/s 是 base64url 词面,html.EscapeString 进 value 属性后无属性逃逸
// 面,纵深纪律同 renderIMPage 全量转义)。form action 从请求路径自取
// (对抗审计 LOW 修:硬编码根绝对路径在路径前缀部署下第二跳 404;自取
// 与「GET 可达」天然同路径域)。Referrer-Policy: no-referrer(对抗审计
// LOW 修):表单提交的 Referer 默认携带本页完整 URL(含 ?d&s),该头使
// 「凭据走 form body 不进访问日志」端到端成立。
func renderIMConfirmPage(c *gin.Context, p auth.IMLinkPayload, d, sg string) {
	c.Header("Content-Security-Policy", "default-src 'none'")
	c.Header("X-Frame-Options", "DENY")
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	body := "<!DOCTYPE html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">" +
		"<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">" +
		"<title>审批确认</title></head><body>" +
		"<h1>审批确认</h1>" +
		"<p>确认后将以 " + html.EscapeString(p.Approver) + " 的身份对执行 #" +
		fmt.Sprintf("%d", p.ExecutionID) + " 节点 " + html.EscapeString(p.NodeID) +
		fmt.Sprintf("(第 %d 轮)投出「%s」票。</p>", p.Round, imFaceZh(p.Action)) +
		"<form method=\"POST\" action=\"" + html.EscapeString(c.Request.URL.Path) + "\">" +
		"<input type=\"hidden\" name=\"d\" value=\"" + html.EscapeString(d) + "\">" +
		"<input type=\"hidden\" name=\"s\" value=\"" + html.EscapeString(sg) + "\">" +
		"<button type=\"submit\">确认" + imFaceZh(p.Action) + "</button>" +
		"</form>" +
		"<p>braid 工作流平台</p></body></html>"
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(body))
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
// 映射;竞速读回需要 DB 词面)。词面已在 imLinkVerify 共享前置闸裁过,
// 此处不可达臂返零值由读回报错收口。
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
