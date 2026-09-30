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
	"strings"
	"time"

	"braid/internal/auth"
	"braid/internal/db"
	"braid/internal/snowflake"
)

// api 配置面(auth/relay/worker 同款四件套:Config + DefaultConfig +
// Validate + ConfigFromEnv;字段名即配置键面(T7.1 已换装收编))。
// BRAID_API_* 区域;JWT 密钥与 auth 共用 BRAID_AUTH_JWT_SECRET(单部署
// 单密钥,签发方 T5.2、验证方本包)。
const (
	// DefaultAddr 监听地址默认值:仅绑回环——公开面由部署层显式打开,
	// 与 dev 栈 MySQL/Redis/Asynqmon 同一默认姿态。8081 避开 Asynqmon 8080。
	DefaultAddr = "127.0.0.1:8081"
	// DefaultSnowflakeNode 是 api 进程的雪花节点号默认值。显式配置面当前仅
	// api=1、worker=2;relay 无雪花生成器,reaper 缺省 PID 派生、可经
	// BRAID_REAPER_SNOWFLAKE_NODE 显式配置(T7.1)。
	DefaultSnowflakeNode = 1
	// DefaultResumeQueue 是审批续跑事件的默认队列(T4.2;见 Config.ResumeQueue)。
	DefaultResumeQueue = "default"
	// 登录防御默认值(T6.4,D-t44 §7-①):IP 固定窗口 10 次/分钟;
	// 连续 5 次失败锁 15 分钟。旋钮化供部署收紧与测试注入短窗。
	DefaultLoginRateLimit   = 10
	DefaultLoginRateWindow  = time.Minute
	DefaultLoginMaxFailures = 5
	DefaultLoginLockout     = 15 * time.Minute

	// http.Server 四超时默认值(原 cmd/api 期常量,T7.1 收编原值搬入):
	// readHeader 封 slowloris;read/write 界住 1MiB DSL 请求体与响应;
	// idle 界住 keep-alive 空闲连接(公开面下均为连接耗尽向量)。
	DefaultReadHeaderTimeout = 10 * time.Second
	DefaultReadTimeout       = 60 * time.Second
	DefaultWriteTimeout      = 30 * time.Second
	DefaultIdleTimeout       = 120 * time.Second
)

// queueNameRe 镜像 outbox 队列字符集(校验单点在 outbox.Append;此处启动
// 期预检让配置错在启动即现,而非首个审批决定 500)。
var queueNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Config 是 api 的全部可调参数。
type Config struct {
	// Addr HTTP 监听地址(host:port)。
	Addr string
	// MySQLDSN 连接串(BRAID_MYSQL_DSN;含凭证,不进日志/审计——铁律)。
	// T7.1 收编 cmd 直读;必填断言在 cmd 读本字段(装配输入非旋钮)。
	MySQLDSN string
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
	// LLM 任务隔离」的独立队列换装归后续配置化(P1 停车场)。
	ResumeQueue string
	// LoginRateLimit / LoginRateWindow:登录 IP 固定窗口限流(T6.4)。
	LoginRateLimit  int
	LoginRateWindow time.Duration
	// LoginMaxFailures / LoginLockout:username 连续失败锁定(T6.4);
	// 锁定响应与限流同形 429(防枚举 oracle,见 loginlimit.go)。
	LoginMaxFailures int
	LoginLockout     time.Duration
	// TokenBinding 是 token 绑定模式(T11.3,D-2026-09-29-t113 #1):
	// off|ua|ua_ip24 闭集,缺省 off(签发不带 bnd claim、校验零介入,
	// 零行为变化)。模式开启 = 全量换发语义:存量无 claim token 一律
	// 401 token_binding_mismatch(fail-closed;存量损失 ≤ token TTL 24h)。
	TokenBinding TokenBindingMode
	// TrustedProxies 是受信代理 CIDR 列表(BRAID_API_TRUSTED_PROXIES,
	// 逗号分隔;空 = 不信任任何代理 = ClientIP 恒 RemoteAddr,fail-closed
	// 现状)。登录防御/审计以 ClientIP 为键:反代公开面必须把代理内网
	// 段列进来,否则全部客户端折叠进代理单地址的同一 IP 窗口(rev2 HIGH:
	// 平台级登录 DoS 面)。仅内网直连部署保持空即可。
	TrustedProxies []string
	// http.Server 四超时(BRAID_API_READ_HEADER_TIMEOUT / READ_TIMEOUT /
	// WRITE_TIMEOUT / IDLE_TIMEOUT;T7.1 收编,原 cmd 常量语义原样)。
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	// Pool 是 MySQL 连接池三值(T7.1,D-t63 移交回收):db.ParsePoolConfig
	// 注入式解析 BRAID_DB_* 三键,施加点 cmd 的 db.ConfigurePool。
	Pool db.PoolConfig
	// OIDC 是 OIDC 插件配置组(T11.1,BRAID_OIDC_*):零值 = 关闭(路由
	// 不挂载);半开在 auth.ParseOIDCConfig 启动即败(组全或无)。
	OIDC auth.OIDCConfig
	// IMLinkSecret 是 IM 鉴权短链验证密钥(T14.3,D-t143 决策 2/7):
	// 与 worker/notify 签发侧同键(BRAID_IM_LINK_SECRET,双端同批供给
	// ——跨组件半配置无法启动期互检,部署契约:worker 配了 api 没配 =
	// 卡片链接点击全 404;api 配了 worker 没配 = 卡片根本发不出);空 =
	// IM 审批入口关闭(端点恒挂载,返回 404,不随配置漂移路由表)。非空
	// 须过 auth.MinIMLinkSecretLen 熵地板。仅经 env 注入,不进日志/审计。
	IMLinkSecret string
}

// DefaultConfig 返回生产默认值(JWTSecret 故意留空:必须显式供给)。
func DefaultConfig() Config {
	return Config{
		Addr:              DefaultAddr,
		SnowflakeNode:     DefaultSnowflakeNode,
		ResumeQueue:       DefaultResumeQueue,
		LoginRateLimit:    DefaultLoginRateLimit,
		LoginRateWindow:   DefaultLoginRateWindow,
		LoginMaxFailures:  DefaultLoginMaxFailures,
		LoginLockout:      DefaultLoginLockout,
		TokenBinding:      TokenBindingOff,
		Pool:              db.DefaultPoolConfig(),
		ReadHeaderTimeout: DefaultReadHeaderTimeout,
		ReadTimeout:       DefaultReadTimeout,
		WriteTimeout:      DefaultWriteTimeout,
		IdleTimeout:       DefaultIdleTimeout,
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
	if c.LoginRateLimit < 1 {
		return fmt.Errorf("api: login rate limit %d < 1", c.LoginRateLimit)
	}
	if c.LoginRateWindow <= 0 {
		return fmt.Errorf("api: login rate window %s <= 0", c.LoginRateWindow)
	}
	if c.LoginMaxFailures < 1 {
		return fmt.Errorf("api: login max failures %d < 1", c.LoginMaxFailures)
	}
	if c.LoginLockout <= 0 {
		return fmt.Errorf("api: login lockout %s <= 0", c.LoginLockout)
	}
	// token 绑定闭集(T11.3):词表外值启动即败(空串 = off 同义,字面
	// 构造零值合法)。
	if !validTokenBinding(c.TokenBinding) {
		return fmt.Errorf("api: token binding %q violates closed vocab off|ua|ua_ip24", c.TokenBinding)
	}
	for _, cidr := range c.TrustedProxies {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("api: trusted proxy %q: %w", cidr, err)
		}
	}
	// 四超时下界 >0:任一为零/负 = 对应攻击面(慢速头部/无界请求体/空闲
	// 连接)重开;上限不设(极端调优自有其部署理由)。
	if c.ReadHeaderTimeout <= 0 || c.ReadTimeout <= 0 || c.WriteTimeout <= 0 || c.IdleTimeout <= 0 {
		return fmt.Errorf("api: http timeouts must be > 0 (got header=%s read=%s write=%s idle=%s)",
			c.ReadHeaderTimeout, c.ReadTimeout, c.WriteTimeout, c.IdleTimeout)
	}
	// OIDC 组(T11.1):零值合法(关闭);程序化直构 Config 的半开组在此
	// 拦下(env 面已由 auth.ParseOIDCConfig 拦,此为第二道)。
	if err := c.OIDC.Validate(); err != nil {
		return err
	}
	// IM 短链密钥(T14.3):空 = 关闭(合法);非空须过熵地板(与 notify
	// 签发侧同口径,auth.MinIMLinkSecretLen 单源——防「可暴破 capability
	// 秘密」静默上线)。
	if len(c.IMLinkSecret) > 0 && len(c.IMLinkSecret) < auth.MinIMLinkSecretLen {
		return fmt.Errorf("api: im link secret %d bytes < %d floor (fix BRAID_IM_LINK_SECRET)",
			len(c.IMLinkSecret), auth.MinIMLinkSecretLen)
	}
	return nil
}

// ConfigFromEnv 从环境变量构造配置(BRAID_API_ 前缀 + 共用的
// BRAID_AUTH_JWT_SECRET;未设键取默认,JWTSecret 无默认)。getenv 注入
// os.Getenv,测试换桩;构造末尾过 Validate——密钥缺省/过短在此启动即败。
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	c := DefaultConfig()
	c.JWTSecret = getenv("BRAID_AUTH_JWT_SECRET")
	c.MySQLDSN = getenv("BRAID_MYSQL_DSN")
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
	if raw := getenv("BRAID_API_LOGIN_RATE"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return c, fmt.Errorf("api: BRAID_API_LOGIN_RATE=%q: want int >= 1", raw)
		}
		c.LoginRateLimit = n
	}
	if raw := getenv("BRAID_API_LOGIN_WINDOW"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("api: BRAID_API_LOGIN_WINDOW=%q: want duration > 0", raw)
		}
		c.LoginRateWindow = d
	}
	if raw := getenv("BRAID_API_LOGIN_MAX_FAILURES"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return c, fmt.Errorf("api: BRAID_API_LOGIN_MAX_FAILURES=%q: want int >= 1", raw)
		}
		c.LoginMaxFailures = n
	}
	if raw := getenv("BRAID_API_LOGIN_LOCKOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("api: BRAID_API_LOGIN_LOCKOUT=%q: want duration > 0", raw)
		}
		c.LoginLockout = d
	}
	if raw := getenv("BRAID_API_TOKEN_BINDING"); raw != "" {
		if !validTokenBinding(TokenBindingMode(raw)) {
			return c, fmt.Errorf("api: BRAID_API_TOKEN_BINDING=%q: want off|ua|ua_ip24", raw)
		}
		c.TokenBinding = TokenBindingMode(raw)
	}
	if raw := getenv("BRAID_API_TRUSTED_PROXIES"); raw != "" {
		for _, cidr := range strings.Split(raw, ",") {
			cidr = strings.TrimSpace(cidr)
			if cidr == "" {
				continue
			}
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				return c, fmt.Errorf("api: BRAID_API_TRUSTED_PROXIES=%q: bad CIDR %q", raw, cidr)
			}
			c.TrustedProxies = append(c.TrustedProxies, cidr)
		}
	}
	// BRAID_TOOL_ALLOWLIST 已随 T3.2 退役:发布白名单真源 = tools.Registry
	// 注册名(NewServer 注入;空 registry = 禁用一切工具,语义不变)。
	// OIDC 组(T11.1):全组缺省 = 零值(关闭);半开启动即败。
	oidcCfg, err := auth.ParseOIDCConfig(getenv)
	if err != nil {
		return c, err
	}
	c.OIDC = oidcCfg
	// IM 短链密钥(T14.3):api 侧只验不签(签发在 worker/notify);空 =
	// IM 审批入口关闭。跨组件半配置(worker 配了 api 没配 = 卡片链接
	// 点击全 404;api 配了 worker 没配 = 卡片根本发不出)无法在本进程
	// 判——部署契约 D-t143 决策 7。
	c.IMLinkSecret = getenv("BRAID_IM_LINK_SECRET")
	pool, err := db.ParsePoolConfig(getenv)
	if err != nil {
		return c, err
	}
	c.Pool = pool
	if c.ReadHeaderTimeout, err = envDuration(getenv, "BRAID_API_READ_HEADER_TIMEOUT", c.ReadHeaderTimeout); err != nil {
		return c, err
	}
	if c.ReadTimeout, err = envDuration(getenv, "BRAID_API_READ_TIMEOUT", c.ReadTimeout); err != nil {
		return c, err
	}
	if c.WriteTimeout, err = envDuration(getenv, "BRAID_API_WRITE_TIMEOUT", c.WriteTimeout); err != nil {
		return c, err
	}
	if c.IdleTimeout, err = envDuration(getenv, "BRAID_API_IDLE_TIMEOUT", c.IdleTimeout); err != nil {
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
		return def, fmt.Errorf("api: %s=%q: %w", key, raw, err)
	}
	return d, nil
}
