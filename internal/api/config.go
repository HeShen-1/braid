// Package api 实现 braid REST 面(ARCHITECTURE:39,89:definition CRUD +
// execution 触发/查询 + 审批 + 审计查询)。T5.4 落地 definition CRUD 与
// 服务骨架;认证接线是最小形态(本地中间件直调 auth.ParseToken),T5.2
// RBAC 中间件落地时整块替换 middleware.go,其余面不动。
package api

import (
	"fmt"
	"net"
	"regexp"
	"strconv"

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
	// DefaultSnowflakeNode 是 api 进程的雪花节点号默认值。显式配置面当前仅
	// api=1、worker=2;relay 无雪花生成器,reaper 为 PID 派生(显式化归
	// T7.1 配置体系)。
	DefaultSnowflakeNode = 1
	// DefaultResumeQueue 是审批续跑事件的默认队列(T4.2;见 Config.ResumeQueue)。
	DefaultResumeQueue = "default"
)

// queueNameRe 镜像 outbox 队列字符集(校验单点在 outbox.Append;此处启动
// 期预检让配置错在启动即现,而非首个审批决定 500)。
var queueNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Config 是 api 的全部可调参数。
type Config struct {
	// Addr HTTP 监听地址(host:port)。
	Addr string
	// JWTSecret Bearer token 验证密钥,与 auth 签发侧共用;只经
	// BRAID_AUTH_JWT_SECRET env 注入,无默认值(缺省启动即败),不进
	// 代码/日志/审计(铁律)。
	JWTSecret string
	// SnowflakeNode 雪花节点号,definition.id 应用侧生成(发布前可知,
	// 供钉版引用)。跨进程重号唯一症状是业务表 PK 1062。
	SnowflakeNode uint16
	// ResumeQueue 是审批续跑事件(execution.resume)的 outbox 队列名
	// (T4.2)。必须在 worker ServeQueues 内,否则事件永挂 pending
	// (outbox 包注释的已知边界)。缺省 "default"(与 scheduler.Deps.Queue
	// 同值,worker 默认消费);DDL §5 queue 列注释钦定「审批/续跑与重
	// LLM 任务隔离」的独立队列在此换装(T7.1 配置体系迁居)。
	ResumeQueue string
}

// DefaultConfig 返回生产默认值(JWTSecret 故意留空:必须显式供给)。
func DefaultConfig() Config {
	return Config{
		Addr:          DefaultAddr,
		SnowflakeNode: DefaultSnowflakeNode,
		ResumeQueue:   DefaultResumeQueue,
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
	if !queueNameRe.MatchString(c.ResumeQueue) {
		return fmt.Errorf("api: resume queue %q violates ^[a-z][a-z0-9_-]{0,63}$", c.ResumeQueue)
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
	if v := getenv("BRAID_API_RESUME_QUEUE"); v != "" {
		c.ResumeQueue = v
	}
	// BRAID_TOOL_ALLOWLIST 已随 T3.2 退役:发布白名单真源 = tools.Registry
	// 注册名(NewServer 注入;空 registry = 禁用一切工具,语义不变)。
	return c, c.Validate()
}
