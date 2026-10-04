package auth

// IM 鉴权短链(T14.3,D-2026-09-30-t143 决策 2):钉钉审批卡片按钮直达
// 平台 API 的凭据 = 对 payload 的 HMAC-SHA256 签名(否决一次性 token:
// 零存储/零迁移/api 零新依赖,「首点即焚」由 decision 既有幂等承担)。
//
// 消费链位置(T17.5 起两跳形态,D-2026-10-04-t175 决策 1):
//
//	notify(IMSender 渲染卡片) --SignIMLink--> actionURL =
//	  {BRAID_APPROVAL_BASE_URL}/api/v1/im/approval?d=…&s=…
//	  → api imApprovalConfirm(GET --VerifyIMLink--> 只读确认页,零落票
//	    ——预取扫描器结构性拿不到消费面)→ 表单 POST imApprovalSubmit
//	    (--VerifyIMLink--> 决定事务单点,与 Bearer 投票同源:锁行/名单门/
//	    定局守卫/重放语义全一致;凭据走 form body)
//
// 安全性质:payload 签名但不加密(id/approver 是公开事实,秘密只在签名);
// 短链证明「持有者可代所指 approver 投票」,不证明点击者身份——信任
// 边界 = webhook 群成员(D-t143 决策 6 登记)。撤销双层:细 = 既有
// decision 幂等(本轮已投 → 重放面;已定局 → 409;round 变 → 链接失效),
// 粗 = 轮换 BRAID_IM_LINK_SECRET 吊销全量在野链接(无单链吊销面)。
// 密钥双端消费(worker/notify 签发、api 验证),半配置无法启动期互检
// ——部署契约:两组件同批供给(D-t143 决策 7)。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// IMLinkPayload 是短链的签名事实面:单字母键 = URL 长度预算(actionCard
// actionURL 长度受平台约束,完整事实不进 URL)。Action 用请求面词表
// approve|reject(DB ENUM 映射单点 decisionFromRequest 在 api 消费侧)。
type IMLinkPayload struct {
	ExecutionID uint64 `json:"e"`
	NodeExecID  uint64 `json:"n"`
	NodeID      string `json:"d"`
	Approver    string `json:"a"`
	Round       int    `json:"r"`
	Action      string `json:"v"`
	ExpiresAt   int64  `json:"x"` // unix 秒;签发侧 = now+IMLinkTTL(SignIMLink 单点覆写)
}

var (
	// ErrIMLinkExpired:签名有效但过期。与签名失败分立——过期是 payload
	// 内的公开事实,分立渲染「链接已过期」不制造 oracle 面。
	ErrIMLinkExpired = errors.New("auth: im link expired")
	// ErrIMLinkInvalid:解码/签名/结构坏(precision 统一:payload 本就公开,
	// 区分失败细节无收益,统一错误面更简)。
	ErrIMLinkInvalid = errors.New("auth: im link invalid")
)

const (
	// IMLinkTTL 是短链有效期(镜像 DefaultTokenTTL 24h 先例,零新 env;
	// 升级事件在 deadline 后才发射,有效期不锚 deadline 而锚签发时刻)。
	IMLinkTTL = 24 * time.Hour
	// MinIMLinkSecretLen 是 BRAID_IM_LINK_SECRET 的熵地板(字节;api 与
	// notify 两端配置闸同值,防「可暴破的 capability 秘密」静默上线)。
	// 对齐 MinSecretLen 32 先例(对抗审计 MEDIUM 修:capability 链接是
	// 发进群聊的 (msg,tag) 样本 = 离线穷举 oracle,16 字节人记密钥可被
	// 群成员字典攻击后伪签任意投票;.env.example 附 openssl rand -hex 32
	// 生成指引)。
	MinIMLinkSecretLen = 32
)

// SignIMLink 签发 capability:d = base64url(payload JSON),s = base64url
// (HMAC-SHA256(key=secret, msg=d))。ExpiresAt 由本函数单点覆写为
// now+IMLinkTTL——调用方不可自定有效期(防「顺手传个 30 天」的脚枪,
// TTL 是契约不是旋钮)。秘密短于熵地板 = 调用方 bug,响亮拒绝。
// 词面闸(对抗审计 LOW 修):Round ≥ 1 与 NodeExecID > 0 必填——消费侧
// LinkRound<0 是「无绑定」哨兵、0 是「未填」零值,两者混进签名词面会
// 让 api 轮绑定守卫静默失效;签发侧把不变量变闸,哨兵值结构性不可签出。
func SignIMLink(secret string, p IMLinkPayload, now time.Time) (d, s string, err error) {
	if len(secret) < MinIMLinkSecretLen {
		return "", "", fmt.Errorf("auth: im link secret %d bytes < %d floor", len(secret), MinIMLinkSecretLen)
	}
	if p.Round < 1 || p.NodeExecID == 0 {
		return "", "", fmt.Errorf("auth: im link payload round %d / node_exec %d outside mintable vocabulary", p.Round, p.NodeExecID)
	}
	p.ExpiresAt = now.Add(IMLinkTTL).Unix()
	raw, err := json.Marshal(p)
	if err != nil {
		return "", "", fmt.Errorf("auth: marshal im link payload: %w", err)
	}
	d = base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(d))
	s = base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return d, s, nil
}

// VerifyIMLink 验签并还原 payload(now = 调用方时钟,测试控界;签名比较
// 走 hmac.Equal 常数时间)。秘密缺失/过短 = 配置面问题,归入 Invalid
// (启动闸在两端 Config.Validate,这里是运行期兜底)。
func VerifyIMLink(secret, d, s string, now time.Time) (IMLinkPayload, error) {
	var p IMLinkPayload
	if len(secret) < MinIMLinkSecretLen {
		return p, ErrIMLinkInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(d)
	if err != nil {
		return p, ErrIMLinkInvalid
	}
	got, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(got) != sha256.Size {
		return p, ErrIMLinkInvalid
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(d))
	if !hmac.Equal(got, mac.Sum(nil)) {
		return p, ErrIMLinkInvalid
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, ErrIMLinkInvalid
	}
	if now.Unix() >= p.ExpiresAt {
		return p, ErrIMLinkExpired
	}
	return p, nil
}
