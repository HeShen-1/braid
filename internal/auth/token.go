package auth

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// iss/aud 是代码常量而非 env(单二进制自签自验,签发方=验证方=唯一 API
// 进程,无运维旋钮价值——relay Jitter 同款否决理由);跨部署 token 混淆
// 防线由两串逐字比较承担。
const (
	tokenIssuer   = "braid"
	tokenAudience = "braid-api"
)

// bindingRe 是 bnd claim 的格式域(T11.3 token 绑定):base64url 无
// padding 恰 22 字符 = SHA-256 前 16 字节的定长编码。auth 只管格式,
// 「值与请求指纹比对」的语义归 api 层(middleware 按当前模式复算)。
var bindingRe = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

// Claims 是 braid 的 JWT 载荷:sub(username 主体字符串,直通
// created_by/actor_id 值域)+ roles(签发时刻的授权快照,T5.2 中间件
// 只信 claims 不回库)+ tnt(签发时刻的租户快照,D-2026-09-30-t112;
// 缺席 = 0 = 平台租户——0011 迁移前的存量 token 与平台租户同判,
// ≤24h TTL 界,roles 快照同界)+ bnd(可选 token 绑定指纹,
// D-2026-09-29-t113:空 = 未绑定,off 模式语义)+ 标准三件套
// iat/exp/iss/aud。
type Claims struct {
	Roles   []Role `json:"roles"`
	Tenant  uint64 `json:"tnt,omitempty"`
	Binding string `json:"bnd,omitempty"`
	jwt.RegisteredClaims
}

// SignToken 签发 HS256 token。ttl 必须为正(过期 token 的构造走包内
// signClaims 出口,不给生产面留「负 TTL 合法」的脚枪)。输入先过
// validUsername/validRoles(调用方 bug 前置拒绝,不产出坏 token)。
// 等价于 SignTokenWithBinding(binding="")。
func SignToken(secret string, ttl time.Duration, username string, roles []Role, tenant uint64) (string, error) {
	return SignTokenWithBinding(secret, ttl, username, roles, tenant, "")
}

// SignTokenWithBinding 签发携带绑定指纹的 token(T11.3):binding 非空
// 时必须过 bindingRe(签发面 bug 前置拒绝,不产出消费面不可比对的
// claim);空串 = 不携带 claim(off 模式两形态同一出口)。tenant 是
// 签发时刻的用户归属租户(D-t112:0 = 平台租户合法值;用户行迁租后
// token 残余访问 ≤ TTL 24h,roles 快照同界——显式传参使新签发点无法
// 遗忘租户维度,编译器强制)。
func SignTokenWithBinding(secret string, ttl time.Duration, username string, roles []Role, tenant uint64, binding string) (string, error) {
	if ttl <= 0 {
		return "", fmt.Errorf("%w: ttl %s <= 0", ErrInvalidInput, ttl)
	}
	if ttl > MaxTokenTTL {
		// 吊销上界在唯一导出签发原语处自洽(不止 Config.Validate 一层):
		// 任何调用方(含未来运维工具)都签不出突破 DISABLED+过期吊销
		// 语义的长命 token。
		return "", fmt.Errorf("%w: ttl %s > %s (revocation bound)", ErrInvalidInput, ttl, MaxTokenTTL)
	}
	if !validUsername(username) {
		return "", fmt.Errorf("%w: username %q", ErrInvalidInput, username)
	}
	if err := validRoles(roles); err != nil {
		return "", err
	}
	if binding != "" && !bindingRe.MatchString(binding) {
		return "", fmt.Errorf("%w: binding violates ^[A-Za-z0-9_-]{22}$", ErrInvalidInput)
	}
	now := time.Now().UTC()
	c := Claims{
		Roles:   roles,
		Tenant:  tenant,
		Binding: binding,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   username,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			Issuer:    tokenIssuer,
			Audience:  jwt.ClaimStrings{tokenAudience},
		},
	}
	return signClaims(secret, c)
}

// signClaims 是唯一签名出口(SignToken 与测试共用):过期 token 的测试
// 在此直构 ExpiresAt=过去,零时钟注入抽象。
func signClaims(secret string, c Claims) (string, error) {
	if len(secret) < MinSecretLen {
		return "", fmt.Errorf("%w: jwt secret shorter than %d bytes", ErrInvalidInput, MinSecretLen)
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(secret))
}

// ParseToken 校验并解析 token,fail-closed:HS256 白名单(拒 alg 混淆,
// 含 none 攻击)、iss/aud 逐字、exp 必存(jwt.WithExpirationRequired)、
// roles 非空且逐字过词表、sub 非空。仅过期单列 ErrTokenExpired,其余
// 一律 ErrTokenInvalid(附底层诊断文本);错误路径返回零值 Claims,
// 不交部分信任。secret 过短是配置误用,走 ErrInvalidInput。
func ParseToken(secret string, token string) (Claims, error) {
	if len(secret) < MinSecretLen {
		return Claims{}, fmt.Errorf("%w: jwt secret shorter than %d bytes", ErrInvalidInput, MinSecretLen)
	}
	var c Claims
	_, err := jwt.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) {
		return []byte(secret), nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(tokenIssuer),
		jwt.WithAudience(tokenAudience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return Claims{}, ErrTokenExpired
		}
		return Claims{}, fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	}
	if c.Subject == "" {
		return Claims{}, fmt.Errorf("%w: empty subject", ErrTokenInvalid)
	}
	if err := validRoles(c.Roles); err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	}
	// bnd 非空时过格式闸(fail-closed):正确密钥签发但携垃圾 claim 的
	// token 在此拒绝,不给消费面留「不可比对的绑定值」(与 roles 词表
	// 同判:防线针对签发面 bug + 消费面信任载荷的组合)。
	if c.Binding != "" && !bindingRe.MatchString(c.Binding) {
		return Claims{}, fmt.Errorf("%w: binding claim format", ErrTokenInvalid)
	}
	return c, nil
}

// RequireRole 是角色越权判定(验收第三条落点;T5.2 Gin 中间件在其上
// 包一层映射 HTTP 状态)。命中返回 nil,不足返回 ErrForbidden(附所需
// 角色与现有角色,便于诊断;不附 sub 以外的凭证信息)。
func RequireRole(c Claims, want Role) error {
	if !validRole(want) {
		return fmt.Errorf("%w: unknown role %q", ErrInvalidInput, want)
	}
	for _, r := range c.Roles {
		if r == want {
			return nil
		}
	}
	return fmt.Errorf("%w: role %q required, subject has %v", ErrForbidden, want, c.Roles)
}
