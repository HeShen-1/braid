// Package api 实现 braid REST 面(ARCHITECTURE:39,89:definition CRUD +
// execution 触发/查询 + 审批 + 审计查询)。T5.4 落地 definition CRUD 与
// 服务骨架;认证接线是最小形态(本地中间件直调 auth.ParseToken),T5.2
// RBAC 中间件落地时整块替换 middleware.go,其余面不动。
package api

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"braid/internal/auth"
	"braid/internal/snowflake"
)

// api 配置面(auth/relay/worker 同款四件套:Config + DefaultConfig +
// Validate + ConfigFromEnv;字段名即未来配置键,T7.1 换装时原样搬)。
// BRAID_API_* 区域;JWT 密钥与 auth 共用 BRAID_AUTH_JWT_SECRET(单部署
// 单密钥,签发方 T5.2、验证方本包)。
const (
	// DefaultAddr 监听地址默认值:仅绑回环——公开面由部署层显式打开,
	// 与 dev 栈 MySQL/Redis/Asynqmon 同一默认姿态。8081 避开 Asynqmon 8080。
	DefaultAddr = "127.0.0.1:8081"
	// DefaultSnowflakeNode 是 api 进程的雪花节点号默认值(compose 静态
	// 槽位表 api=1/worker=2/relay=3/reaper=4;值表 T7.1 配置化)。
	DefaultSnowflakeNode = 1
)

// Config 是 api 的全部可调参数。
type Config struct {
	// Addr HTTP 监听地址(host:port)。
	Addr string
	// JWTSecret Bearer token 验证密钥,与 auth 签发侧共用;只经
	// BRAID_AUTH_JWT_SECRET env 注入,无默认值(缺省启动即败),不进
	// 代码/日志/审计(铁律)。
	JWTSecret string
	// ToolAllowlist 发布时工具白名单(T5.4 静态配置;T3.2 registry 落地
	// 后换源,消费点只有 publish 一处)。nil/空 = 名单为空集 = 禁用一切
	// 工具(dsl.WithToolAllowlist 语义),发布引用任何工具都 400。
	ToolAllowlist []string
	// SnowflakeNode 雪花节点号,definition.id 应用侧生成(发布前可知,
	// 供钉版引用)。跨进程重号唯一症状是业务表 PK 1062。
	SnowflakeNode uint16
}

// DefaultConfig 返回生产默认值(JWTSecret 故意留空:必须显式供给)。
func DefaultConfig() Config {
	return Config{
		Addr:          DefaultAddr,
		SnowflakeNode: DefaultSnowflakeNode,
	}
}

// Validate 报告配置矛盾(启动即败,不进运行期)。
func (c Config) Validate() error {
	if len(c.JWTSecret) < auth.MinSecretLen {
		return fmt.Errorf("api: jwt secret shorter than %d bytes", auth.MinSecretLen)
	}
	if _, _, err := net.SplitHostPort(c.Addr); err != nil {
		return fmt.Errorf("api: addr %q: %w", c.Addr, err)
	}
	if c.SnowflakeNode > snowflake.MaxNodeID {
		return fmt.Errorf("api: snowflake node %d > %d", c.SnowflakeNode, snowflake.MaxNodeID)
	}
	seen := make(map[string]bool, len(c.ToolAllowlist))
	for _, t := range c.ToolAllowlist {
		if t == "" {
			return fmt.Errorf("api: tool allowlist contains empty entry")
		}
		if seen[t] {
			return fmt.Errorf("api: tool allowlist duplicate %q", t)
		}
		seen[t] = true
	}
	return nil
}

// ConfigFromEnv 从环境变量构造配置(BRAID_API_ 前缀 + 共用的
// BRAID_AUTH_JWT_SECRET;未设键取默认,JWTSecret 无默认)。getenv 注入
// os.Getenv,测试换桩;构造末尾过 Validate——密钥缺省/过短在此启动即败。
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	c := DefaultConfig()
	c.JWTSecret = getenv("BRAID_AUTH_JWT_SECRET")
	if v := getenv("BRAID_API_ADDR"); v != "" {
		c.Addr = v
	}
	if raw := getenv("BRAID_API_SNOWFLAKE_NODE"); raw != "" {
		n, err := strconv.Atoi(raw)
		// 越界在转换前拒绝:uint16 截断会把 [65536,66559] 折叠回合法
		// 节点域,静默绕过 Validate 的 fail-fast(错误配置变成「看起来
		// 合法的另一个节点号」,重号症状只剩 PK 1062,不可诊断)。
		if err != nil || n < 0 || n > int(snowflake.MaxNodeID) {
			return c, fmt.Errorf("api: BRAID_API_SNOWFLAKE_NODE=%q: want 0..%d", raw, snowflake.MaxNodeID)
		}
		c.SnowflakeNode = uint16(n)
	}
	if raw := getenv("BRAID_TOOL_ALLOWLIST"); raw != "" {
		parts := strings.Split(raw, ",")
		c.ToolAllowlist = make([]string, 0, len(parts))
		for _, p := range parts {
			c.ToolAllowlist = append(c.ToolAllowlist, strings.TrimSpace(p))
		}
	}
	return c, c.Validate()
}
