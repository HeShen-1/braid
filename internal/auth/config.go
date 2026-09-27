package auth

import (
	"fmt"
	"strconv"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// auth 配置面(relay/config.go 同款四件套:Config + DefaultConfig +
// Validate + ConfigFromEnv;字段名即配置键面,T7.1 已换装收编)。
// auth 是首个 BRAID_AUTH_* 区域用例。
const (
	// DefaultTokenTTL token 有效期默认值。
	DefaultTokenTTL = 24 * time.Hour
	// MaxTokenTTL token 有效期上界:无状态 JWT 下 DISABLED 只拦新登录,
	// 存量 token 存活至过期——TTL 上界即停用后的最大暴露窗,>7 天与
	// 该语义矛盾。
	MaxTokenTTL = 7 * 24 * time.Hour
	// DefaultBcryptCost bcrypt 成本默认值(= bcrypt.DefaultCost,OWASP
	// 下限);测试用 MinCost 换速度。
	DefaultBcryptCost = bcrypt.DefaultCost
	// MinSecretLen HS256 对称密钥长度下界:长度是唯一可机检的强度面
	// (熵不可廉价度量),过短启动即败而非运行期警告。
	MinSecretLen = 32
)

// Config 是 auth 的全部可调参数。
type Config struct {
	// JWTSecret HS256 签名密钥:只经 BRAID_AUTH_JWT_SECRET env 注入,
	// 无默认值(缺省即 Validate 失败,启动即败);不进代码/日志/审计
	// (铁律)。
	JWTSecret string
	// TokenTTL token 有效期,域 (0, MaxTokenTTL]。
	TokenTTL time.Duration
	// BcryptCost 口令哈希成本,域 [bcrypt.MinCost, bcrypt.MaxCost]
	// (上界防误配 DoS:31 档单次分钟级)。时序均衡前提:Authenticate
	// 的 dummy 比对固定 DefaultCost,cost 偏离默认会部分重开未知用户
	// 枚举的时序面(P0 默认部署不受影响;cost 感知均衡归 T5.2 配置
	// 持有方落地)。
	BcryptCost int
}

// DefaultConfig 返回生产默认值(JWTSecret 故意留空:必须显式供给)。
func DefaultConfig() Config {
	return Config{
		TokenTTL:   DefaultTokenTTL,
		BcryptCost: DefaultBcryptCost,
	}
}

// Validate 报告配置矛盾(启动即败,不进运行期)。
func (c Config) Validate() error {
	switch {
	case len(c.JWTSecret) < MinSecretLen:
		return fmt.Errorf("auth: jwt secret shorter than %d bytes", MinSecretLen)
	case c.TokenTTL <= 0:
		return fmt.Errorf("auth: token ttl %s <= 0", c.TokenTTL)
	case c.TokenTTL > MaxTokenTTL:
		return fmt.Errorf("auth: token ttl %s > %s (disabled revocation bound)", c.TokenTTL, MaxTokenTTL)
	case c.BcryptCost < bcrypt.MinCost:
		return fmt.Errorf("auth: bcrypt cost %d < %d", c.BcryptCost, bcrypt.MinCost)
	case c.BcryptCost > bcrypt.MaxCost:
		return fmt.Errorf("auth: bcrypt cost %d > %d", c.BcryptCost, bcrypt.MaxCost)
	}
	return nil
}

// ConfigFromEnv 从环境变量构造配置(BRAID_AUTH_ 前缀;未设键取默认,
// JWTSecret 无默认)。getenv 注入 os.Getenv,测试换桩;构造末尾过
// Validate——密钥缺省/过短在此处启动即败。
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	c := DefaultConfig()
	c.JWTSecret = getenv("BRAID_AUTH_JWT_SECRET")
	var err error
	if c.TokenTTL, err = envDuration(getenv, "BRAID_AUTH_TOKEN_TTL", c.TokenTTL); err != nil {
		return c, err
	}
	if c.BcryptCost, err = envInt(getenv, "BRAID_AUTH_BCRYPT_COST", c.BcryptCost); err != nil {
		return c, err
	}
	return c, c.Validate()
}

func envDuration(getenv func(string) string, key string, def time.Duration) (time.Duration, error) {
	raw := getenv(key)
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return def, fmt.Errorf("auth: %s=%q: %w", key, raw, err)
	}
	return d, nil
}

func envInt(getenv func(string) string, key string, def int) (int, error) {
	raw := getenv(key)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def, fmt.Errorf("auth: %s=%q: %w", key, raw, err)
	}
	return n, nil
}
