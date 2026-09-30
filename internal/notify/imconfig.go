package notify

// IMConfig 是 IM 审批卡片(钉钉自定义群机器人 webhook,T14.3 / D-2026-09-30
// -t143 决策 1)的全部可调参数。字段名即配置键语义(装配换装点
// cmd/worker.buildNotifier)。WebhookURL 空 = 通道未配置(worker 侧该通道
// 不装,镜像 SMTP Host 空的降级语义);设则 LinkSecret 与 BaseURL 必填
// ——卡片按钮即鉴权短链,两缺一即不可渲染,半配置启动即败优于静默半瘫。
//
// 密钥纪律(铁律):Secret/LinkSecret 仅经 env 注入,不进代码/日志/审计;
// 错误面只携带键名与长度事实,不回显值。LinkSecret 是 api/worker 双端键
// (worker 签发短链、api 验证)——跨组件半配置无法启动期互检,部署契约
// 见 D-t143 决策 7(两组件同批供给)。

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"braid/internal/auth"
)

// IMConfig 见包注释。Secret 空 = 机器人安全模式为关键词/IP 白名单
// (钉钉三选一,仅加签模式有 SEC 密钥),请求不加签。
type IMConfig struct {
	// WebhookURL 是钉钉机器人 webhook(端点 oapi.dingtalk.com/robot/
	// send?access_token=…,见 Validate 的 https 闸);access_token 段是
	// 凭证,不进日志/错误串。
	WebhookURL string
	// Secret 是钉钉加签密钥(SEC…,可选——机器人安全模式非加签时为空)。
	Secret string
	// LinkSecret 是鉴权短链 HMAC 密钥(BRAID_IM_LINK_SECRET),与 api 验证
	// 端同键;熵地板 auth.MinIMLinkSecretLen(两端口径单源)。
	LinkSecret string
	// BaseURL 是短链前缀(BRAID_APPROVAL_BASE_URL,与 SMTP 审批链接同源
	// 单键:一个部署对外只有一个审批宿主根);尾斜杠规范化后存。
	BaseURL string
	// Timeout 是单次发送全预算(http.Client.Timeout 承载:拨号+写+读回
	// 一段,与 SMTP Config.Timeout 同纪律);上限 1m(IM 是旁路面,不许
	// 长占 worker 槽)。
	Timeout time.Duration
}

// DefaultIMConfig 返回生产默认值(Timeout 10s:钉钉公网 API 惯例时延,
// 比 SMTP 内网中继默认 15s 略紧)。
func DefaultIMConfig() IMConfig {
	return IMConfig{Timeout: 10 * time.Second}
}

// Validate 报告配置矛盾(启动即败)。通道未配置(WebhookURL 空)时其余
// 键孤儿容忍——LinkSecret 是双端键,api 侧可独立供给验证在野链接
// (D-t143 决策 7),notify 侧无权判它「孤儿」。
//
// 明文闸(对抗审计 LOW×2 修,镜像 SMTP 非 loopback 强制 STARTTLS 纪律):
// WebhookURL 与 BaseURL 仅允许 https,例外 = 回环 host(测试桩/本机
// 形态)——WebhookURL 内嵌 access_token 凭证段、BaseURL 前缀下的短链
// 内嵌 HMAC capability,明文通路 = 凭证被动截获面。
func (c IMConfig) Validate() error {
	if c.WebhookURL == "" {
		return nil
	}
	u, err := url.Parse(c.WebhookURL)
	if err != nil || u.Host == "" || u.Fragment != "" {
		return fmt.Errorf("notify: im webhook url not absolute without fragment (fix BRAID_IM_WEBHOOK_URL)")
	}
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("notify: im webhook url scheme %q: http only allowed on loopback (access_token credential in URL; set https, D-t143 plaintext gate)", u.Scheme)
	}
	if len(c.LinkSecret) < auth.MinIMLinkSecretLen {
		return fmt.Errorf("notify: im link secret %d bytes < %d floor (supply BRAID_IM_LINK_SECRET)",
			len(c.LinkSecret), auth.MinIMLinkSecretLen)
	}
	if c.Timeout <= 0 || c.Timeout > time.Minute {
		return fmt.Errorf("notify: im timeout %s out of (0,1m]", c.Timeout)
	}
	b, err := url.Parse(c.BaseURL)
	if err != nil || b.Host == "" || b.Fragment != "" {
		return fmt.Errorf("notify: im approval base url %q not absolute http(s) without fragment (supply BRAID_APPROVAL_BASE_URL; anchor suffix kills every card link)", c.BaseURL)
	}
	if b.Scheme != "https" && !isLoopbackHost(b.Hostname()) {
		return fmt.Errorf("notify: im approval base url scheme %q: http only allowed on loopback (capability links carry d/s in URL; set https, D-t143 plaintext gate)", b.Scheme)
	}
	return nil
}

// IMConfigFromEnv 从环境变量构造(BRAID_IM_ 前缀 + 双端键
// BRAID_IM_LINK_SECRET + 与 SMTP 共享的 BRAID_APPROVAL_BASE_URL);未设键
// 取默认;构造末尾过 Validate。
func IMConfigFromEnv(getenv func(string) string) (IMConfig, error) {
	c := DefaultIMConfig()
	c.WebhookURL = getenv("BRAID_IM_WEBHOOK_URL")
	c.Secret = getenv("BRAID_IM_SECRET")
	c.LinkSecret = getenv("BRAID_IM_LINK_SECRET")
	c.BaseURL = strings.TrimRight(getenv("BRAID_APPROVAL_BASE_URL"), "/")
	if raw := getenv("BRAID_IM_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return c, fmt.Errorf("notify: BRAID_IM_TIMEOUT=%q: %w", raw, err)
		}
		c.Timeout = d
	}
	return c, c.Validate()
}
