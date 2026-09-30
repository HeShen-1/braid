package notify

// IMSender 是 approval.notify 事件的钉钉卡片发送面(T14.3,D-2026-09-30
// -t143 决策 1):与 SMTP Sender 并列的第二通知通道,结构化满足
// worker.ApprovalNotifier(扇出装配见 fanout.go / cmd/worker.buildNotifier)。
//
// 单方法编排 = 去重查(approval.im_notified 审计行,per-channel 锚)→
// 回表审批上下文 + 本轮票向 → 渲染 actionCard(按钮 = 鉴权短链,签名面
// internal/auth imlink)→ 钉钉 webhook POST(可选加签)→ 送达审计行。
// 失败面响亮上抛(worker 侧 error ⇒ asynq 重试后 archived),幂等收敛 =
// ErrAlreadyNotified。
//
// 卡片语义(D-t143 决策 3):按钮对按 approver 维度签发(会签任意人数下
// 身份显式);pending ≤ maxIMDirectApprovers 直出按钮对,超出/零 pending
// 降级单按钮「打开审批页」;已投者以文本呈现票向(按 D-t142 契约 decisions
// round 重组,不另立字段)。0 pending(升级-消费窗内投票已齐)= 信息卡
// 非响亮失败——窗内竞态是合法时序(票落在 reaper 扫描与 worker 消费
// 之间),不是数据缺陷。

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"braid/internal/audit"
	"braid/internal/auth"
	"braid/internal/engine"
	"braid/internal/scheduler"
)

// maxIMDirectApprovers 是卡片直出按钮对的 pending 上限(≤3 人 = ≤6 按钮;
// 超出降级单按钮「打开审批页」——按钮墙可读性阈值,D-t143 决策 3)。
const maxIMDirectApprovers = 3

// IMSender 见包注释。
type IMSender struct {
	deps Deps
	cfg  IMConfig
	log  *slog.Logger
	now  func() time.Time
	http *http.Client
}

// NewIMSender 构造发送器;依赖缺损或配置非法响亮报错(镜像 NewSender)。
func NewIMSender(d Deps, cfg IMConfig) (*IMSender, error) {
	if d.DB == nil || d.IDs == nil {
		return nil, errors.New("notify: nil DB or IDs")
	}
	if d.WorkerID == "" {
		return nil, errors.New("notify: empty worker id")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	return &IMSender{
		deps: d, cfg: cfg, log: log, now: time.Now,
		http: &http.Client{Timeout: cfg.Timeout},
	}, nil
}

// NotifyApproval 送达一张审批升级卡片(exec/node 标识 + 成因码 + 轮次;
// 语义与 Sender.NotifyApproval 同形)。round = 挂起所属轮(T9.2):去重锚
// 按轮匹配,回滚重挂后的第二轮升级是新的通知意图。
func (s *IMSender) NotifyApproval(ctx context.Context, execID, nodeExecID uint64, reason string, round int) error {
	if execID == 0 || nodeExecID == 0 {
		return errors.New("notify: zero exec or node exec id")
	}

	// [1] 回表审批上下文(与 SMTP Sender 同式:created_at 读出钉审计
	// 日分区;node 行携 id 防错配;tenant_id 是本通道的租户闸——见下)。
	var (
		rowExecID  uint64
		nodeID     string
		nodeStatus string
		deadline   sql.NullTime
		snapshot   []byte
		defName    string
		defVersion uint64
		execDate   string
		execTenant uint64
	)
	err := s.deps.DB.QueryRowContext(ctx, `
SELECT n.execution_id, n.node_id, n.status, n.deadline_at, n.input_snapshot,
       w.definition_name, w.definition_version,
       DATE_FORMAT(w.created_at, '%Y-%m-%d'), w.tenant_id
  FROM node_execution n
  JOIN workflow_execution w ON w.id = n.execution_id
 WHERE n.id=?`, nodeExecID).
		Scan(&rowExecID, &nodeID, &nodeStatus, &deadline, &snapshot, &defName, &defVersion, &execDate, &execTenant)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("notify: node_execution %d vanished (data defect)", nodeExecID)
	}
	if err != nil {
		return fmt.Errorf("notify: read node %d: %w", nodeExecID, err)
	}
	if rowExecID != execID {
		return fmt.Errorf("notify: node %d belongs to execution %d, payload says %d (data defect)",
			nodeExecID, rowExecID, execID)
	}
	// 租户闸(对抗审计 HIGH 修):BRAID_IM_WEBHOOK_URL 是部署级单键,无
	// 租户分流面——他租户(tenant_id≠0)执行的审批上下文(工作流名/名单/
	// instructions)与按钮 capability 推进共享群 = 跨租户暴露 + 跨租户落票
	// 面(D-t112 契约在 IM 通道的未登记旁路)。代码级闭合:非平台租户
	// 执行一律响亮拒发(worker 死信可见,不静默降级)——多租户部署的 IM
	// 通道 = 部署契约禁用(D-t143 决策 6),per-tenant webhook 路由归后续
	// 决策。SMTP 通道不受影响(逐人收件,租户判据在 EmailsByUsernames)。
	if execTenant != 0 {
		return fmt.Errorf("notify: im channel is single-tenant (platform tenant only), execution %d tenant %d (D-t143 #6; per-tenant webhook routing is a future decision)",
			execID, execTenant)
	}

	// [2] 去重查:IM 通道已送达 ⇒ 幂等哨兵(per-channel 锚,D-t143 决策 5
	// ——SMTP 的 approval.notified 行不遮蔽本通道,反之亦然;重投只补发
	// 失败通道)。已声明边界与 SMTP 同族:顺序窗(发送成功与审计 INSERT
	// 间崩溃 ⇒ 至多重发一卡)、并发窗(lease 过期重叠;默认 10s 预算下
	// conn deadline 先于最短重投路径 ~60s 实际不可达,但 BRAID_IM_TIMEOUT
	// 配至 1m 上限时理论可达(临界)——后果同族有界:每事件至多一张
	// 重复卡,SMTP 2m 上限同款 caveat)。
	var one int
	err = s.deps.DB.QueryRowContext(ctx,
		`SELECT 1 FROM audit_log
		 WHERE exec_date=? AND execution_id=? AND node_execution_id=? AND event_type=?
		   AND JSON_EXTRACT(payload, '$.round') = ? LIMIT 1`,
		execDate, execID, nodeExecID, audit.EventApprovalIMNotified, round).Scan(&one)
	if err == nil {
		return ErrAlreadyNotified
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("notify: im dedup check node %d: %w", nodeExecID, err)
	}

	var snap scheduler.ApprovalInput
	if len(snapshot) == 0 {
		return fmt.Errorf("notify: node %d has NULL snapshot (never suspended? data defect)", nodeExecID)
	}
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		return fmt.Errorf("notify: approval snapshot node %d: %w", nodeExecID, err)
	}

	// [3] 本轮票向(D-t142 契约消费:decisions[] 按轮重组,不另立字段)
	// ——卡片呈现「已批准/已驳回/待投」与 pending 名单,会签中间态即
	// 升级时刻的部分投票事实。
	voted := map[string]string{}
	vrows, err := s.deps.DB.QueryContext(ctx,
		`SELECT decided_by, decision FROM approval_decision
		  WHERE node_execution_id=? AND round=?`, nodeExecID, round)
	if err != nil {
		return fmt.Errorf("notify: read round votes node %d: %w", nodeExecID, err)
	}
	for vrows.Next() {
		var by, dec string
		if err := vrows.Scan(&by, &dec); err != nil {
			_ = vrows.Close()
			return fmt.Errorf("notify: scan round votes node %d: %w", nodeExecID, err)
		}
		voted[by] = dec
	}
	if err := vrows.Err(); err != nil {
		return fmt.Errorf("notify: round votes iter node %d: %w", nodeExecID, err)
	}

	// [4] pending(快照名单序;0 pending = 信息卡,见包注释)。
	var pending []string
	for _, a := range snap.Approvers {
		if _, ok := voted[a]; !ok {
			pending = append(pending, a)
		}
	}

	// [5] 渲染卡片(按钮 = 鉴权短链,SignIMLink 单点覆写 exp)。
	payload, err := s.renderCard(execID, nodeExecID, nodeID, deadline, defName, defVersion,
		snap, reason, round, voted, pending)
	if err != nil {
		return err
	}

	// [6] 发送(外部调用;ctx + Client.Timeout 双兜底)。
	if err := s.post(ctx, payload); err != nil {
		return err
	}
	s.log.Info("notify.im_sent",
		"execution_id", execID, "node_execution_id", nodeExecID,
		"node_id", nodeID, "reason", reason, "round", round,
		"approvers", len(snap.Approvers), "pending", len(pending),
		"mode", snap.Mode)

	// [7] 送达审计行(去重锚写半边;T9.3 串链契约:调用方事务)。
	auditID, err := s.deps.IDs.Next()
	if err != nil {
		return fmt.Errorf("notify: gen audit id: %w", err)
	}
	raw, err := json.Marshal(audit.ApprovalIMNotifiedPayload{NodeID: nodeID, Reason: reason, Round: round})
	if err != nil {
		return fmt.Errorf("notify: marshal approval.im_notified payload: %w", err)
	}
	if err := engine.WithTx(ctx, s.deps.DB, func(q engine.DBTX) error {
		return audit.Append(ctx, q, audit.Event{
			ID: auditID, EventType: audit.EventApprovalIMNotified,
			ExecutionID: execID, NodeExecID: nodeExecID, NodeID: nodeID,
			ActorType: audit.ActorWorker, ActorID: s.deps.WorkerID, Payload: raw,
		})
	}); err != nil {
		return fmt.Errorf("notify: append approval.im_notified node %d: %w", nodeExecID, err)
	}
	return nil
}

// renderCard 产出钉钉 actionCard 请求体:markdown 文本(上下文与 SMTP
// render 同源面)+ 按钮(短链直投或降级审批页)。instructions 自由文本
// 逐行 "> " 引用前缀(D-t45 防钓鱼面同款:无法冒充平台顶格文案行)。
func (s *IMSender) renderCard(execID, nodeExecID uint64, nodeID string, deadline sql.NullTime,
	defName string, defVersion uint64, snap scheduler.ApprovalInput,
	reason string, round int, voted map[string]string, pending []string) ([]byte, error) {

	var card dingActionCard
	card.MsgType = "actionCard"
	card.Card.Title = fmt.Sprintf("[braid] 审批升级:%s v%d(执行 #%d)", defName, defVersion, execID)

	var b strings.Builder
	fmt.Fprintf(&b, "### 审批升级:%s v%d(执行 #%d)\n", defName, defVersion, execID)
	fmt.Fprintf(&b, "**节点**:%s(第 %d 轮)\n", nodeID, round)
	modeFace := "或签(任一批准)"
	if snap.Mode == scheduler.ApprovalModeAll {
		modeFace = "会签(全部批准,任一驳回即败)"
	}
	fmt.Fprintf(&b, "**模式**:%s\n", modeFace)
	b.WriteString("**审批人**:")
	var names []string
	for _, a := range snap.Approvers {
		switch voted[a] {
		case scheduler.DecisionApproved:
			names = append(names, a+"(已批准)")
		case scheduler.DecisionRejected:
			names = append(names, a+"(已驳回)")
		default:
			names = append(names, a+"(待投)")
		}
	}
	b.WriteString(strings.Join(names, "、"))
	b.WriteString("\n")
	fmt.Fprintf(&b, "**超时策略**:%s(挂起 %.0f 小时后升级)\n", snap.OnTimeout, snap.TimeoutHours)
	if deadline.Valid {
		fmt.Fprintf(&b, "**截止时刻**:%s\n", deadline.Time.UTC().Format("2006-01-02 15:04:05 UTC"))
	}
	fmt.Fprintf(&b, "**升级原因**:%s\n", reason)
	if len(pending) == 0 {
		b.WriteString("\n该轮投票已齐(定局),工作流将自动收口,无需操作。\n")
	}
	for _, line := range strings.Split(strings.ReplaceAll(snap.Instructions, "\r\n", "\n"), "\n") {
		fmt.Fprintf(&b, "> %s\n", line)
	}
	fmt.Fprintf(&b, "\n[查看审批页](%s)\n", s.pageURL(execID, nodeID))
	card.Card.Text = b.String()
	card.Card.BtnOrientation = "0" // 纵向:按钮对按审批人分组,纵向可读

	if len(pending) > 0 && len(pending) <= maxIMDirectApprovers {
		for _, a := range pending {
			approveURL, err := s.actionURL(execID, nodeExecID, nodeID, a, round, "approve")
			if err != nil {
				return nil, err
			}
			rejectURL, err := s.actionURL(execID, nodeExecID, nodeID, a, round, "reject")
			if err != nil {
				return nil, err
			}
			card.Card.Btns = append(card.Card.Btns,
				dingButton{Title: a + "·批准", ActionURL: approveURL},
				dingButton{Title: a + "·驳回", ActionURL: rejectURL},
			)
		}
	} else {
		// pending 超阈值或零 pending(信息卡):单按钮落审批页(登录路径,
		// 与 SMTP 链接同形)。
		card.Card.Btns = []dingButton{{Title: "打开审批页", ActionURL: s.pageURL(execID, nodeID)}}
	}
	return json.Marshal(card)
}

// pageURL 是 Web 审批页链接(登录路径,与 SMTP approvalLink 同形)。
func (s *IMSender) pageURL(execID uint64, nodeID string) string {
	return fmt.Sprintf("%s/executions/%d/approvals/%s", s.cfg.BaseURL, execID, nodeID)
}

// actionURL 签发一张按钮短链(payload 即按钮的全部事实:D-t143 决策 2;
// exp 由 SignIMLink 单点覆写)。NodeExecID 必填:api 侧 LinkNodeExecID
// 错配守卫(数据缺陷 tripwire)依赖签发面真实携带——漏填 = 守卫恒短路
// 死码(对抗审计 MEDIUM 修)。d/s 是 base64url 词面,URL 拼接免转义。
func (s *IMSender) actionURL(execID, nodeExecID uint64, nodeID, approver string, round int, action string) (string, error) {
	d, sg, err := auth.SignIMLink(s.cfg.LinkSecret, auth.IMLinkPayload{
		ExecutionID: execID, NodeExecID: nodeExecID, NodeID: nodeID,
		Approver: approver, Round: round, Action: action,
	}, s.now())
	if err != nil {
		return "", fmt.Errorf("notify: sign im link node %s approver %s: %w", nodeID, approver, err)
	}
	return fmt.Sprintf("%s/api/v1/im/approval?d=%s&s=%s", s.cfg.BaseURL, d, sg), nil
}

// post 走钉钉 webhook:可选加签(timestamp+sign 拼进 URL)→ JSON POST →
// errcode 裁决。错误面纪律:http.Client 的 *url.Error 携带完整 webhook
// URL——access_token 段是凭证,脱壳只保留底层错误(不进 asynq 错误
// 存储/日志,铁律);服务端 errmsg 是远端可控文本,原样带回但无凭证。
func (s *IMSender) post(ctx context.Context, payload []byte) error {
	u := s.cfg.WebhookURL
	if s.cfg.Secret != "" {
		ts := s.now().UnixMilli()
		sep := "&"
		if !strings.Contains(u, "?") {
			sep = "?"
		}
		u = fmt.Sprintf("%s%stimestamp=%d&sign=%s", u, sep, ts, url.QueryEscape(dingSign(ts, s.cfg.Secret)))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("notify: build im request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("notify: im post dingtalk: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return fmt.Errorf("notify: im read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("notify: im http status %d", resp.StatusCode)
	}
	var dr struct {
		ErrCode *int   `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(body, &dr); err != nil {
		return fmt.Errorf("notify: im response body: %w", err)
	}
	// errcode 键必须显式在场(对抗审计 LOW 修):指针形态下「200 + 合法
	// JSON 但无 errcode 键」= 非钉钉信封(误配到别的端点),零值折叠会
	// 落假阳性送达锚——通知永久丢失且对账判「已送达」。缺键响亮失败。
	if dr.ErrCode == nil {
		return fmt.Errorf("notify: dingtalk response missing errcode envelope (misconfigured webhook endpoint?)")
	}
	if *dr.ErrCode != 0 {
		return fmt.Errorf("notify: dingtalk errcode=%d errmsg=%q", *dr.ErrCode, dr.ErrMsg)
	}
	return nil
}

// dingSign 是钉钉自定义机器人加签(官方口径:mac.init(key=secret) +
// doFinal(msg=`timestamp+"\n"+secret`),HMAC-SHA256 → Base64;调用方负责
// URL 编码。orch-review HIGH 修正:初版 key/msg 写反——官方 Java/Python
// 样例与 prometheus-webhook-dingtalk 等四个生产实现均为 key=secret,
// 写反则加签模式每发必 310000)。金向量见 im_test.go(本机 Python
// hmac/hashlib 独立实算,官方序)。
func dingSign(timestampMs int64, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	// QF1012 要求 Fprintf 形态,errcheck 需显式弃值(check-blank 默认关)
	// ——双闸同过的唯一形;hash.Hash 写内存零错误面。
	_, _ = fmt.Fprintf(mac, "%d\n%s", timestampMs, secret)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// dingButton / dingActionCard 是 actionCard 请求体(btns 独立跳转形态)。
type dingButton struct {
	Title     string `json:"title"`
	ActionURL string `json:"actionURL"`
}

type dingActionCard struct {
	MsgType string `json:"msgtype"`
	Card    struct {
		Title          string       `json:"title"`
		Text           string       `json:"text"`
		BtnOrientation string       `json:"btnOrientation"`
		Btns           []dingButton `json:"btns"`
	} `json:"actionCard"`
}
