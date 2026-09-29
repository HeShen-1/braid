package notify

// Sender 是 approval.notify 事件的 SMTP 发送面(T4.5):worker 换装点消费
// (handleApprovalNotify → NotifyApproval)。单方法编排 = 去重查 → 回表审批
// 上下文 → 渲染 → net/smtp 发送 → 送达审计;全部失败面响亮上抛(worker
// 侧 error ⇒ asynq 内部重试一次后 archived,T6.4 可见),唯一幂等收敛 =
// ErrAlreadyNotified(已送达过的重投)。

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"braid/internal/audit"
	"braid/internal/engine"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
)

// ErrAlreadyNotified:该 (execution, node_execution) 的通知已送达
// (approval.notified 审计行存在)。at-least-once 重投的幂等哨兵——worker
// 侧映射为正常 ack(非错误,不烧重试预算)。
var ErrAlreadyNotified = errors.New("notify: approval notification already delivered")

// Deps 是运行依赖。Logger 为 nil 时用 slog.Default();WorkerID 落审计行
// actor_id(送达归因到消费进程,cmd 装配传 cfg.WorkerID)。
type Deps struct {
	DB       *sql.DB
	IDs      *snowflake.Generator // 审计行 id(应用侧 Snowflake)
	WorkerID string
	Logger   *slog.Logger
}

// Sender 是生产实现(worker.ApprovalNotifier 接口的结构化满足方;接口在
// worker 侧定义,本包不 import worker——与 nodes→worker 同向的反置:消费
// 面接口 + 本包零反向依赖)。
type Sender struct {
	deps Deps
	cfg  Config
	log  *slog.Logger
}

// NewSender 构造发送器;依赖缺损或配置非法响亮报错(不静默空转)。
func NewSender(d Deps, cfg Config) (*Sender, error) {
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
	return &Sender{deps: d, cfg: cfg, log: log}, nil
}

// NotifyApproval 送达一封审批通知(exec/node 标识 + 成因码)。步骤与幂等
// 语义见包注释;快照真相源 = node_execution.input_snapshot(engine 不变量:
// 挂起 ⇒ NOT NULL 且可解析为 ApprovalInput——坏快照 = 数据缺陷,响亮失败)。
// NotifyApproval 发送审批升级通知。round = 挂起所属轮(T9.2,D-t92 决策 7):
// 去重锚按轮匹配——approval.notified 行 payload 携 round,回滚重挂后的
// 第二轮升级是新的通知意图,不再被第一轮行静默吞(对抗审计 HIGH 修:队列
// TaskID 轮次段只修了半边)。升级窗取舍:T9.2 前的存量 notified 行 payload
// 无 round 键(JSON_EXTRACT 为 NULL)不参与去重 ⇒ 同一挂起重投时最坏多发
// 一封(有界、可观测;反向选择会让存量行吞掉新轮通知,不可接受)。
func (s *Sender) NotifyApproval(ctx context.Context, execID, nodeExecID uint64, reason string, round int) error {
	if execID == 0 || nodeExecID == 0 {
		return errors.New("notify: zero exec or node exec id")
	}

	// [1] 回表审批上下文(payload 仅 id 引用的消费侧兑现)。created_at 同读:
	// [2] 去重查用它钉 audit_log 日分区(镜像 audit.Append 的 DATE_FORMAT
	// 物化表达式,created_at 不可变 ⇒ 两处恒同值;无分区谓词则逐分区探测
	// 本地索引——sec 评审 LOW②)。
	var (
		rowExecID  uint64
		nodeID     string
		nodeStatus string
		deadline   sql.NullTime
		snapshot   []byte
		defName    string
		defVersion uint64
		execDate   string
	)
	err := s.deps.DB.QueryRowContext(ctx, `
SELECT n.execution_id, n.node_id, n.status, n.deadline_at, n.input_snapshot,
       w.definition_name, w.definition_version,
       DATE_FORMAT(w.created_at, '%Y-%m-%d')
  FROM node_execution n
  JOIN workflow_execution w ON w.id = n.execution_id
 WHERE n.id=?`, nodeExecID).
		Scan(&rowExecID, &nodeID, &nodeStatus, &deadline, &snapshot, &defName, &defVersion, &execDate)
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

	// [2] 去重查:已送达(跨进程/重启持久锚)⇒ 幂等哨兵。两个已声明边界:
	// a) 顺序窗——发送成功与审计 INSERT 之间崩溃 ⇒ 重投重发一封(见包注释);
	// b) 并发窗(对抗审计 LOW①)——asynq lease(30s,heartbeater 续期)过期后
	//    recoverer 重投与原实例停摆中的发送重叠 ⇒ 双发 + 双审计行(无唯一键,
	//    均合法 INSERT)。默认 Timeout=15s 下 conn 墙钟 deadline 先于最短重投
	//    路径(~60s:lease 过期余量 + recoverer 1min tick)杀死停摆发送,窗口
	//    实际不可达;Timeout 朝 2m 上限配置时理论可达,后果有界(每事件至多
	//    1 封重复),与 a) 同后果类。闭合手段(预插 pending 锚/Redis SETNX)
	//    会把已声明缺口换成更糟缺口(锚先行崩溃 = 永久吞通知且 T6.4 无丢失
	//    信号),不做。
	var one int
	err = s.deps.DB.QueryRowContext(ctx,
		`SELECT 1 FROM audit_log
		 WHERE exec_date=? AND execution_id=? AND node_execution_id=? AND event_type=?
		   AND JSON_EXTRACT(payload, '$.round') = ? LIMIT 1`,
		execDate, execID, nodeExecID, audit.EventApprovalNotified, round).Scan(&one)
	if err == nil {
		return ErrAlreadyNotified
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("notify: dedup check node %d: %w", nodeExecID, err)
	}

	var snap scheduler.ApprovalInput
	if len(snapshot) == 0 {
		return fmt.Errorf("notify: node %d has NULL snapshot (never suspended? data defect)", nodeExecID)
	}
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		return fmt.Errorf("notify: approval snapshot node %d: %w", nodeExecID, err)
	}

	// [3] 渲染 + [4] 发送(外部调用,ctx/Timeout 双兜底)。
	subject, body := s.render(execID, nodeID, nodeStatus, deadline, defName, defVersion, snap, reason)
	msg := s.buildMessage(subject, body)
	if err := s.send(ctx, msg); err != nil {
		return err
	}
	s.log.Info("notify.sent",
		"execution_id", execID, "node_execution_id", nodeExecID,
		"node_id", nodeID, "reason", reason, "recipients", len(s.cfg.NotifyTo))

	// [5] 送达审计行(去重锚的写半边;失败上抛 ⇒ 重投重发一封,见包注释
	// 已知边界)。
	auditID, err := s.deps.IDs.Next()
	if err != nil {
		return fmt.Errorf("notify: gen audit id: %w", err)
	}
	raw, err := json.Marshal(audit.ApprovalNotifiedPayload{NodeID: nodeID, Reason: reason, Round: round})
	if err != nil {
		return fmt.Errorf("notify: marshal approval.notified payload: %w", err)
	}
	// T9.3:串链协议要求调用方事务(裸池句柄 = 锚定/读尾/落行/推头四语句
	// 散落 autocommit ⇒ 链叉);"独立小事务"从注释变为事实。
	if err := engine.WithTx(ctx, s.deps.DB, func(q engine.DBTX) error {
		return audit.Append(ctx, q, audit.Event{
			ID: auditID, EventType: audit.EventApprovalNotified,
			ExecutionID: execID, NodeExecID: nodeExecID, NodeID: nodeID,
			ActorType: audit.ActorWorker, ActorID: s.deps.WorkerID, Payload: raw,
		})
	}); err != nil {
		return fmt.Errorf("notify: append approval.notified node %d: %w", nodeExecID, err)
	}
	return nil
}

// render 产出主题与正文(中文,纯文本 P0)。上下文字段与 API 审批详情读面
// (api/approval.go approvalDetailDTO)同源:快照声明面 + 执行钉版四件套中
// 的名称/版本。
func (s *Sender) render(execID uint64, nodeID, nodeStatus string, deadline sql.NullTime,
	defName string, defVersion uint64, snap scheduler.ApprovalInput, reason string) (string, string) {
	subject := fmt.Sprintf("[braid] 审批升级提醒:%s v%d(执行 #%d)", defName, defVersion, execID)

	var b strings.Builder
	b.WriteString("审批节点已超时升级,等待处理。\n\n")
	fmt.Fprintf(&b, "工作流:%s v%d(执行 #%d)\n", defName, defVersion, execID)
	fmt.Fprintf(&b, "节点:%s(当前状态:%s)\n", nodeID, nodeStatus)
	fmt.Fprintf(&b, "审批人:%s\n", strings.Join(snap.Approvers, "、"))
	fmt.Fprintf(&b, "模式:%s\n", snap.Mode)
	// instructions 是 DSL 自由文本(schema 仅限长,无 pattern):逐行加引用
	// 前缀,使其无法冒充平台顶格文案行(对抗审计 LOW②:仿「审批入口」行 +
	// 外链的钓鱼面;发布 DSL 属半特权操作,此为低成本视觉收敛,非根治)。
	for _, line := range strings.Split(strings.ReplaceAll(snap.Instructions, "\r\n", "\n"), "\n") {
		fmt.Fprintf(&b, "> %s\n", line)
	}
	fmt.Fprintf(&b, "超时策略:%s(挂起 %.0f 小时后升级)\n", snap.OnTimeout, snap.TimeoutHours)
	if deadline.Valid {
		fmt.Fprintf(&b, "截止时刻:%s\n", deadline.Time.UTC().Format("2006-01-02 15:04:05 UTC"))
	}
	fmt.Fprintf(&b, "升级原因:%s\n\n", reason)
	fmt.Fprintf(&b, "审批入口(登录后操作):\n%s\n\n",
		s.approvalLink(execID, nodeID))
	b.WriteString("本邮件由 braid 工作流平台自动发送,无需回复。\n")
	return subject, b.String()
}

// approvalLink 拼审批链接:BaseURL + API GET 路由同形路径(内置 Web 审批页
// P1 落地时同路径承接)。
func (s *Sender) approvalLink(execID uint64, nodeID string) string {
	return fmt.Sprintf("%s/executions/%d/approvals/%s", s.cfg.ApprovalBaseURL, execID, nodeID)
}

// buildMessage 组 RFC 5322 信头 + 正文。中文主题走 B 编码字(RFC 2047);
// 正文 UTF-8 纯文本(地址仅 ASCII,无需编码)。
func (s *Sender) buildMessage(subject, body string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", s.cfg.From)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(s.cfg.NotifyTo, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", encodeHeaderWord(subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.Bytes()
}

// encodeHeaderWord:非 ASCII 或含 CR/LF 的头值整串 B 编码(=?UTF-8?B?…?=
// ,base64 输出无控制字符,注入面结构性中和);纯 ASCII 且无控制字符原样。
// 当前主题恒走编码分支(常量中文文案),ASCII 直通是未来英文文案下的
// 防线(sec 评审 LOW①:直通分支也必须拒 CR/LF)。
func encodeHeaderWord(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		ascii := true
		for _, r := range s {
			if r > 127 {
				ascii = false
				break
			}
		}
		if ascii {
			return s
		}
	}
	return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
}

// send 走 net/smtp 对话:拨号(ctx + Timeout 双兜底——handler ctx 无 deadline,
// 裸 Dialer 会吃满内核 SYN 重试 ~2min,review MEDIUM①)+ 会话 deadline
// (Timeout)→ 可选 STARTTLS → 可选 PLAIN 认证 → 信封 → DATA → QUIT。
// 认证仅在 TLS 会话上发送(net/smtp PlainAuth 对非 TLS 非 localhost 拒绝
// ——明文+认证组合是配置错误,Validate 之外由标准库兜底)。DATA 已被接受
// (w.Close 成功)后 QUIT 失败不构成发送失败:置之不理仅 warn——否则必然
// 重发一封且去重锚缺席(review MEDIUM②)。
func (s *Sender) send(ctx context.Context, msg []byte) error {
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	// 单一预算(对抗审计 LOW⑤):拨号时长计入总预算——start 锚点先取,
	// 会话 deadline = start+Timeout(而非拨号后再给满一整个 Timeout),
	// 单次发送最坏墙钟 = Timeout,与 config 注释/文档口径一致。
	start := time.Now()
	conn, err := (&net.Dialer{Timeout: s.cfg.Timeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("notify: dial %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(start.Add(s.cfg.Timeout)); err != nil {
		return fmt.Errorf("notify: set deadline: %w", err)
	}
	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return fmt.Errorf("notify: smtp greeting %s: %w", addr, err)
	}
	defer func() { _ = c.Close() }() //nolint:errcheck // QUIT 正常路径已关,残留 Close 幂等
	if s.cfg.StartTLS {
		if err := c.StartTLS(&tls.Config{
			ServerName: s.cfg.Host,
			MinVersion: tls.VersionTLS12,
			RootCAs:    s.cfg.TLSRootCAs, // nil = 系统信任池;私有 CA 程序化注入面
		}); err != nil {
			return fmt.Errorf("notify: starttls %s: %w", addr, err)
		}
	}
	if s.cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			return fmt.Errorf("notify: auth %s: %w", addr, err)
		}
	}
	if err := c.Mail(s.cfg.From); err != nil {
		return fmt.Errorf("notify: mail from: %w", err)
	}
	for _, to := range s.cfg.NotifyTo {
		if err := c.Rcpt(to); err != nil {
			return fmt.Errorf("notify: rcpt %s: %w", to, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("notify: data: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("notify: write message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("notify: close data: %w", err)
	}
	if err := c.Quit(); err != nil {
		s.log.Warn("notify.quit_ignored_after_delivery", "addr", addr, "err", err)
	}
	return nil
}
