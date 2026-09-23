// Package tools 实现工具运行时(T3.2):registry(内部注册表)+ ToolRunner
// 接口(P0 本地实现 LocalRunner)+ per-call scoped token(签发/过期/传递)。
//
// 凭证纪律(ARCHITECTURE §3.4,验收面):per-call 短时 scoped token 是工具
// 进程可见的唯一凭证形态;平台主凭证(DB DSN/LLM key/token 签名密钥)永不进
// 子进程环境、永不落审计。token 的 P0 校验点在本包调用门(执行前验签+过期);
// P1 工具回调平台 API 时同一 token 由平台侧复验(密钥届时经部署配置共享)。
//
// Redis:本包不消费 Redis(registry 内存态、token 无状态自验),无需登记
// 测试 DB 基号。
package tools

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefaultTokenTTL 是 scoped token 的默认寿命(秒级窗口:签发→工具调用开始
// 的短暂间隙;工具执行期长短由 attempt ctx 界住,不靠 token)。
const DefaultTokenTTL = 60 * time.Second

// token 错误词表(LocalRunner 据此映射失败码)。
var (
	ErrTokenInvalid = errors.New("tools: invalid scoped token")
	ErrTokenExpired = errors.New("tools: scoped token expired")
)

// TokenScope 绑定 token 的使用面:单节点单 attempt 单工具。Verify 时逐域
// 比对——A 工具的 token 用不到 B 工具上(scoped 的语义半边)。
type TokenScope struct {
	NodeExecID uint64
	AttemptNo  uint16
	Tool       string
}

// tokenPayload 是签名前的明文体。Nonce 使同 scope 的两次签发不同
// (per-call 可区分);P1 回调校验若需防重放,以 Nonce 为撤销键。
type tokenPayload struct {
	NodeExecID uint64 `json:"ne"`
	AttemptNo  uint16 `json:"an"`
	Tool       string `json:"tl"`
	ExpiresAt  int64  `json:"exp"` // Unix 秒
	Nonce      string `json:"nc"`  // 8 随机字节 hex
}

// TokenIssuer 签发并校验无状态 HMAC scoped token。key 为进程内秘密:
// 默认每次构造随机生成(重启即轮换,P0 校验点同进程内自洽);需要跨进程
// 校验时经部署配置显式传入(BRAID_TOOL_TOKEN_SECRET)。
type TokenIssuer struct {
	key   []byte
	ttl   time.Duration
	clock func() time.Time
}

// NewTokenIssuer 构造签发器。key 为 nil 时生成 32 字节随机密钥;ttl ≤ 0
// 时取 DefaultTokenTTL;clock 为 nil 时用 time.Now(测试注入时钟以推进过期)。
func NewTokenIssuer(key []byte, ttl time.Duration, clock func() time.Time) *TokenIssuer {
	if len(key) == 0 {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			panic(fmt.Sprintf("tools: rand key: %v", err)) // crypto/rand 失败 = 系统级熵源故障,启动即败
		}
	}
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	if clock == nil {
		clock = time.Now
	}
	return &TokenIssuer{key: key, ttl: ttl, clock: clock}
}

// Issue 为给定 scope 签发一枚短时 token(编码:base64url(payload) +
// "." + base64url(HMAC-SHA256),无状态自包含)。
func (t *TokenIssuer) Issue(scope TokenScope) (string, error) {
	nc := make([]byte, 8)
	if _, err := rand.Read(nc); err != nil {
		return "", fmt.Errorf("tools: token nonce: %w", err)
	}
	p := tokenPayload{
		NodeExecID: scope.NodeExecID,
		AttemptNo:  scope.AttemptNo,
		Tool:       scope.Tool,
		ExpiresAt:  t.clock().Add(t.ttl).Unix(),
		Nonce:      fmt.Sprintf("%x", nc),
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("tools: token payload: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	return body + "." + t.sign(body), nil
}

// Verify 校验 token:格式、签名、scope 三域比对、过期。任一不过即拒
// (验收:token 过期后工具调用被拒——LocalRunner 在分派工具前调用本门)。
func (t *TokenIssuer) Verify(token string, scope TokenScope) error {
	dot := -1
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			dot = i
			break
		}
	}
	if dot <= 0 || dot == len(token)-1 || strings.Count(token, ".") != 1 {
		return ErrTokenInvalid
	}
	body, sig := token[:dot], token[dot+1:]
	if !hmac.Equal([]byte(t.sign(body)), []byte(sig)) {
		return ErrTokenInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return ErrTokenInvalid
	}
	var p tokenPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return ErrTokenInvalid
	}
	if p.NodeExecID != scope.NodeExecID || p.AttemptNo != scope.AttemptNo || p.Tool != scope.Tool {
		return ErrTokenInvalid
	}
	if t.clock().Unix() >= p.ExpiresAt {
		return ErrTokenExpired
	}
	return nil
}

func (t *TokenIssuer) sign(body string) string {
	m := hmac.New(sha256.New, t.key)
	m.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
