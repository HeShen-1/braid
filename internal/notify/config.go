// Package notify 实现 T4.5 SMTP 审批通知:worker 消费 approval.notify 事件后
// 的发送面(回表审批上下文 → 渲染邮件 → net/smtp 发送 → 送达审计)。
//
// 引擎消费链上的位置:
//
//	reaper 升级事务(escalated_at 恰好一次锚)→ outbox approval.notify
//	(task_id=notify:<node_exec_id>)→ relay 投递 → worker handleApprovalNotify
//	→ 本包 Sender.NotifyApproval(回表审批上下文(input_snapshot + 执行钉版
//	  名称/版本,顺带取 created_at 钉分区)→ 去重查 audit approval.notified 行
//	  → 渲染邮件 → SMTP 发送 → 送达审计行)
//
// 去重(D-t43 #3「按 task_id 去重」的实现拍板):asynq at-least-once 的重复
// 投递以「发送成功后落的 approval.notified 审计行」为锚——重投先查行,命中即
// 幂等跳过(worker 侧映射为 ack)。审计行跨进程/重启持久,优于进程内 LRU
// (崩溃窗失效)且零新依赖(免 Redis SETNX 触 go.mod 铁律面);T6.4 副产
// 对账面:escalated(意图)有行而 notified(送达)缺行 = 通知丢失。已知
// 边界:发送成功与审计 INSERT 之间崩溃 ⇒ 重投重发一封(至多一次重复,
// 恰好一次锚仍在 reaper 侧 escalated_at 意图层)。
//
// 收件面(T12.2,D-t45 #2 兑现):快照 approvers(用户名)→ users.email
// 逐人收件(一人一封);BRAID_SMTP_NOTIFY_TO 降为 ops 副本/兜底通道;无
// 邮箱的 approver 名字保留正文、由 ops 副本兜底。零可解析收件人 = 发送面
// 响亮失败(归 worker 死信告警),不静默。
package notify

import (
	"crypto/x509"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config 是 SMTP 通知的全部可调参数(relay/llm/config.go 同款四件套;字段名
// 即配置键,T7.1 已换装收编)。Password 仅经 env 注入,不进代码/
// 日志/审计(铁律)。Host 空 = 整组未配置(合法:worker 保留 T4.3 占位
// log-only 降级面,dev 栈无 SMTP 可用);Host 非空则 From/ApprovalBaseURL
// 必填——半配置启动即败,优于静默半瘫;NotifyTo 可选(T12.2 起为 ops
// 副本通道,零可解析收件人由发送期响亮失败兜底)。
type Config struct {
	// Host 是 SMTP 服务器主机名;空 = 未配置(调用方降级,Validate 放行)。
	Host string
	// Port 是 SMTP 端口,默认 587(STARTTLS 惯例口;内网明文中继可改 25 并
	// 关 STARTTLS)。
	Port int
	// Username/Password 是 SMTP 认证凭证;Username 空 = 不认证(内网开中继
	// 形态)。Password 仅经 BRAID_SMTP_PASSWORD 注入。
	Username string
	Password string
	// From 是信封+信头发件人(MAIL FROM / From:)。
	From string
	// NotifyTo 是 ops 副本/兜底收件人列表(逗号分隔 env 解析;T12.2 扇出
	// 后为可选通道:设则每封升级通知追加一封 ops 副本——可见性保底,行为
	// 向后兼容;未配 email 的 approver 由本副本兜底可见)。纯扇出形态
	// (空)合法,但零可解析收件人时发送面响亮失败(归 worker 死信告警)。
	NotifyTo []string
	// ApprovalBaseURL 是审批链接前缀(内置 Web 审批页/API 同形路径
	// /executions/<id>/approvals/<node_id> 的宿主根);尾斜杠规范化后存。
	ApprovalBaseURL string
	// StartTLS 控制连接后是否协商 STARTTLS;默认开(生产惯例),明文仅测试
	// 与内网分段环境显式关闭。
	StartTLS bool
	// TLSRootCAs 是 STARTTLS 的证书信任池注入面:私有 CA 中继部署经
	// BRAID_SMTP_CA_FILE(PEM 文件路径)注入(T12.2,D-t45 §7 蒸发回收;
	// 文件不可读/PEM 空 = 启动即败);nil = 系统信任池(默认)。
	TLSRootCAs *x509.CertPool
	// Timeout 是单次发送全预算(拨号+握手+传输,单一段:拨号时长计入——
	// 会话 deadline 从拨号前锚点起算,最坏墙钟 = Timeout)。
	Timeout time.Duration
}

// DefaultConfig 返回生产默认值(Host/From/NotifyTo/ApprovalBaseURL 无默认:
// 未配置语义由 Host 空承担,其余三项随 Host 显式供给)。
func DefaultConfig() Config {
	return Config{
		Port:     587,
		StartTLS: true,
		Timeout:  15 * time.Second,
	}
}

// mailAddrRe 是收件/发件地址的最小形约束:恰一 @、两侧非空、无空白/控制字符
// (内网中继域名形态如 ops@localhost 合法,不做公网 DNS 形状假设)。
var mailAddrRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+$`)

// Validate 报告配置矛盾(启动即败,不进运行期)。上界:Timeout ≤2m(通知是
// 旁路面,不允许长占 worker 槽);端口全域校验。配对闸(对抗审计 M②③):
//   - 反向半配置:Host 缺失但 From/NotifyTo/ApprovalBaseURL 任一非空 ⇒ 报错
//     (HOST 拼错/丢失而其余键就位时,静默降级 = 邮件永不发出且零启动信号,
//     违背本包「半配置启动即败」自立原则;PORT/TIMEOUT/STARTTLS 孤儿容忍
//     ——纯旋钮无语义承诺);
//   - 凭证轴:Password 存在即意图认证 ⇒ Username 必须同在(否则静默变
//     不认证,对要求认证的中继每发 530);Username 非空且 Host 非 loopback
//     ⇒ StartTLS 必须开(否则 net/smtp PlainAuth 对明文拒发,每发送必败
//     ——标准库兜底是生产流量期每发必炸,不是启动闸)。
func (c Config) Validate() error {
	if c.Host == "" {
		// 未配置 + 语义键孤儿 = 反向半配置(见方法注释);纯未配置放行。
		if c.From != "" || len(c.NotifyTo) > 0 || c.ApprovalBaseURL != "" {
			return fmt.Errorf("notify: smtp host empty but from/notify_to/approval_base_url set (half-configured: fix BRAID_SMTP_HOST or clear the group)")
		}
		return nil
	}
	switch {
	case c.Port < 1 || c.Port > 65535:
		return fmt.Errorf("notify: port %d out of [1,65535]", c.Port)
	case !mailAddrRe.MatchString(c.From):
		return fmt.Errorf("notify: from %q not an address", c.From)
	// NotifyTo 可选(T12.2 扇出后为 ops 副本通道,非唯一收件面);零可解析
	// 收件人的发送期响亮失败在 sender 侧(配置期不可判——approvers 快照
	// 是运行期数据)。
	case c.Timeout <= 0 || c.Timeout > 2*time.Minute:
		return fmt.Errorf("notify: timeout %s out of (0,2m]", c.Timeout)
	case c.Password != "" && c.Username == "":
		return fmt.Errorf("notify: password set without username (silent no-auth drift; supply BRAID_SMTP_USERNAME)")
	case c.Username != "" && !c.StartTLS && !isLoopbackHost(c.Host):
		return fmt.Errorf("notify: username auth requires STARTTLS on non-loopback host %q (net/smtp refuses plaintext PLAIN auth; set BRAID_SMTP_STARTTLS=true)", c.Host)
	}
	for _, to := range c.NotifyTo {
		if !mailAddrRe.MatchString(to) {
			return fmt.Errorf("notify: notify_to entry %q not an address", to)
		}
	}
	u, err := url.Parse(c.ApprovalBaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("notify: approval base url %q not absolute http(s)", c.ApprovalBaseURL)
	}
	return nil
}

// isLoopbackHost 判明文+认证的退化合法形态(本地 fake/中继测试面)。
func isLoopbackHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// ConfigFromEnv 从环境变量构造配置(BRAID_SMTP_ 前缀 + BRAID_APPROVAL_BASE_URL;
// 未设键取默认)。getenv 注入 os.Getenv,测试换桩;构造末尾过 Validate。
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	c := DefaultConfig()
	c.Host = getenv("BRAID_SMTP_HOST")
	c.Username = getenv("BRAID_SMTP_USERNAME")
	c.Password = getenv("BRAID_SMTP_PASSWORD")
	c.From = getenv("BRAID_SMTP_FROM")
	c.ApprovalBaseURL = strings.TrimRight(getenv("BRAID_APPROVAL_BASE_URL"), "/")
	var err error
	if c.Port, err = envInt(getenv, "BRAID_SMTP_PORT", c.Port); err != nil {
		return c, err
	}
	if c.Timeout, err = envDuration(getenv, "BRAID_SMTP_TIMEOUT", c.Timeout); err != nil {
		return c, err
	}
	if raw := getenv("BRAID_SMTP_STARTTLS"); raw != "" {
		// 显式供给才可关(默认开);非法值启动即败防拼错静默降级明文。
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return c, fmt.Errorf("notify: BRAID_SMTP_STARTTLS=%q: %w", raw, err)
		}
		c.StartTLS = v
	}
	if raw := getenv("BRAID_SMTP_NOTIFY_TO"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			to := strings.TrimSpace(part)
			if to == "" {
				// 空条目(尾逗号/双逗号)= 配置错而非可跳过项:静默吞掉会
				// 掩盖误删地址的拼写事故,收件面宁严勿漏。
				return c, fmt.Errorf("notify: BRAID_SMTP_NOTIFY_TO entry %q empty", part)
			}
			c.NotifyTo = append(c.NotifyTo, to)
		}
	}
	if raw := getenv("BRAID_SMTP_CA_FILE"); raw != "" {
		pool, err := loadCAPEM(raw)
		if err != nil {
			return c, err
		}
		c.TLSRootCAs = pool
	}
	return c, c.Validate()
}

// loadCAPEM 读 PEM CA 包并构建信任池(T12.2,D-t45 §7 预登记形态落地:
// env 文件路径 → 程序化注入面的桥)。全仓首个 env=文件路径键:读失败/
// 零证书即败(fail-closed,半配置纪律——HOST 在场而 CA 键坏 = 部署意图
// 明确但材料不可用,静默回退系统池会让私有 CA 中继的 STARTTLS 每发必败
// 于 x509 验证,错误面从启动挪到生产流量期);Host 空时孤儿容忍(纯旋钮
// 先例:PORT/TIMEOUT/STARTTLS 同待遇)。
func loadCAPEM(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("notify: BRAID_SMTP_CA_FILE=%q: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("notify: BRAID_SMTP_CA_FILE=%q: no PEM certificates parsed", path)
	}
	return pool, nil
}

func envInt(getenv func(string) string, key string, def int) (int, error) {
	raw := getenv(key)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def, fmt.Errorf("notify: %s=%q: %w", key, raw, err)
	}
	return n, nil
}

func envDuration(getenv func(string) string, key string, def time.Duration) (time.Duration, error) {
	raw := getenv(key)
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return def, fmt.Errorf("notify: %s=%q: %w", key, raw, err)
	}
	return d, nil
}
