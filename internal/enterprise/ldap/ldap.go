// Package ldap 是 LDAP bind 插件(T11.1 留桩,T17.6 换 Authenticate
// 本体;T16.1 切入企业增值面,D-2026-10-01-t161-engine-enterprise-split;
// D-2026-10-04-t176)。缝面 = auth.IdentityProvider(核心 auth,
// identity.go),本包零缝改动——凭证核验型身份源的平台挂点。
//
// 形态 = search-then-bind:服务账号(BRAID_LDAP_BIND_DN/_PASSWORD,env)
// 先 bind,按转义 filter 在 BaseDN 下搜索单条目,再用**服务端返回的
// DN** bind 用户密码。不做 DN 模板拼接——DN 注入面结构性消亡;唯一
// 用户输入代入面 = search filter,双保险:① auth.ValidUsername 前置门
// (词域 ^[a-z][a-z0-9_-]{0,127}$ 结构性排除全部 filter 元字符)
// ② RFC 4515 EscapeFilter 转义(纵深,判据独立,失效其一不放大)。
//
// 错误族 = T5.3 契约(identity.go 同款):词域外 → CredentialsError
// invalid_input(401);目录零命中 → unknown_user(401);用户 bind 49 →
// wrong_password(401);本地账号停用 → ErrUserDisabled(403,锚定缝
// 判定);其余(拨号/服务 bind/搜索故障/多命中歧义)原样上抛 = 500
// ——身份源故障不是登录判定结果(login.go default 分支同判)。
//
// 锚定 = auth.AnchorLoginUser(D-t111 同则:搜索命中的 username 锚定
// 本地账号,角色/状态自本地库;本地无账号 → unknown_user,401 与错密
// 同形,防枚举契约不受影响)。
//
// 时域垫平 = T20.5(D-2026-10-06-t205-ldap-timing-pad):unknown_user
// (零命中即返)与 wrong_password(多一次用户 bind 往返 + 目录侧口令
// 验证)的时域差是未认证探测者可聚合的账号枚举 oracle(D-t176 边界⑤
// 登记)。垫平 = 零命中分支纯客户端等待 levelPad——零新增目录操作,
// 任何账号的失败 bind 计数都不可能被累加,服务账号自锁风险(D-t176
// 否决 dummy bind 垫平的根因)结构性消亡;levelPad 启动期在同一探测
// 连接上实测 3 次成功 rebind 取中位,clamp [1ms, 1s]。残差与假设见
// D-t205 接受残差清单。
package ldap

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"

	"braid/internal/auth"
)

// LDAPBindProvider 是凭证核验型 LDAP 身份源(T11.1 留桩,T17.6 换真
// 实现;值类型:零可变状态,typed-nil 面结构性不存在)。配置经
// LDAPConfig(cmd/api 组合根装配,镜像 entoidc);DB = 本地账号锚定
// 存储(生产 = 连接池;失败路径不触库)。
type LDAPBindProvider struct {
	Cfg LDAPConfig
	DB  auth.DBTX
	// levelPad 是零命中分支的时域垫平时长(T20.5):构造期实测、构造
	// 后只读——赋值只发生在 NewLDAPBindProvider 返回前,值类型「零
	// 可变状态」不变量不受影响。零值 = 不垫(字面量构造的测试面与
	// 未配置面)。
	levelPad time.Duration
}

// 桩形状锚延续:实现仍满足 IdentityProvider(缝面零改动的活证)。
var _ auth.IdentityProvider = LDAPBindProvider{}

// ldapOpTimeout 是单条目录连接的整生命周期上界(拨号由 Dialer 同值
// 界住;TLS 握手 + 服务 bind + search + 用户 bind 由底层 net.Conn 的
// 绝对 deadline 界住——对抗审计 MEDIUM 修:go-ldap v3.4.14 的
// SetTimeout 只是 per-message 计时器,管不到 StartTLS 内部握手的裸
// socket I/O,且无 NetConn() 可后设 deadline,故自管拨号)。无界挂死
// 的目录服务会吊住登录请求与启动探测——entoidc.OIDCHTTPTimeout 同判
// (x/oauth2 默认无 deadline,review MEDIUM 先例)。登录连接生命周期
// ≤ 四往返,5s 绝对界宽裕;连接即弃不清 deadline。
const ldapOpTimeout = 5 * time.Second

// 时域垫平参数(T20.5,D-2026-10-06-t205-ldap-timing-pad)。samples =
// 实测 rebind 次数(中位数抗单次调度尖刺);min/max = 钳制界:下界防
// 退化测量(零/亚毫秒),上界防病态目录把零命中登录拖满秒级。对齐
// 依赖「目录口令验证成本均匀」假设(服务账号与用户账号同策略)——
// 假设不成立时垫平部分退化,残差受 ldapOpTimeout 连接绝对界约束
// (clamp 只界 pad 值自身,不构成残差上界:真实 bind 成本可至秒级),
// 登记见 D-t205 接受残差清单。
const (
	levelPadSamples = 3
	levelPadMin     = time.Millisecond
	levelPadMax     = time.Second
)

// NewLDAPBindProvider 构造 + 启动期连通性探测(镜像 entoidc.
// NewOIDCProvider 的 discovery 启动期拉取:目录不可达/服务凭据错 =
// 启动失败,不把配置错误伪装成运行期登录 500)。探测 = 拨号 +
// (可选 StartTLS)+ 服务账号 bind + 实测 rebind ×3(T20.5,同连接
// 取 levelPad)+ 关闭;零搜索,不触碰目录数据。探测连接不缓存:
// 登录期逐次拨号(bind 语义不可复用连接身份)。
func NewLDAPBindProvider(ctx context.Context, cfg LDAPConfig, db auth.DBTX) (LDAPBindProvider, error) {
	// ctx 留在签名面:探测超时由 ldapOpTimeout 界住(镜像 entoidc
	// NewOIDCProvider 的 ctx 形参;go-ldap v3 API 不携 ctx)。
	conn, err := openServiceConn(cfg)
	if err != nil {
		return LDAPBindProvider{}, fmt.Errorf("auth: ldap startup probe %q: %w", cfg.Addr, err)
	}
	// T20.5:探测连接上加做实测——3 次成功 rebind 取中位得 levelPad。
	// rebind 用正确服务凭据(全部成功,失败 bind 计数零累加);测量含
	// bind 消息往返 + 目录侧口令验证,恰是 wrong_password 分支相对
	// 零命中分支多付成本的部署实测估计(拨号/TLS 公共前缀不含)。
	pad, err := measureLevelPad(conn, cfg)
	if err != nil {
		_ = conn.Close()
		return LDAPBindProvider{}, fmt.Errorf("auth: ldap startup probe %q: %w", cfg.Addr, err)
	}
	_ = conn.Close() // 探测连接不缓存:bind 语义不可复用连接身份
	// 实测垫平值落日志(对抗审计 perf 视角 LOW 修):时值非秘密(攻击者
	// 本就可测),运维诊断启动封套失败/漂移过冲(停车场「运行期重校准」
	// 触发判据)需要知道构造期到底测到了什么。slog.Default 惯例镜像
	// internal/enterprise/im。
	slog.Info("auth: ldap timing pad measured", "addr", cfg.Addr, "level_pad", pad)
	return LDAPBindProvider{Cfg: cfg, DB: db, levelPad: pad}, nil
}

// Authenticate 实现 IdentityProvider 契约(缝面零改动):search-then-
// bind + 本地锚定,流程与错误族见包注释。两道触线前闸(拨号零成本
// 路径):① auth.ValidUsername 词域门(filter 代入面前置,亦是注入面
// 第一道);② 空口令结构性拒收——RFC 4511 简单 bind 空口令 =
// unauthenticated/anonymous 语义,部分目录配置放行,绝不把空口令
// 送上线(等值处理:词域内用户名 + 空口令 = wrong_password)。
func (p LDAPBindProvider) Authenticate(ctx context.Context, username, password string) (auth.User, error) {
	if !auth.ValidUsername(username) {
		return auth.User{}, &auth.CredentialsError{Username: username, Reason: auth.ReasonInvalidInput}
	}
	if password == "" {
		return auth.User{}, &auth.CredentialsError{Username: username, Reason: auth.ReasonWrongPassword}
	}
	conn, err := openServiceConn(p.Cfg)
	if err != nil {
		return auth.User{}, fmt.Errorf("auth: ldap connect %q: %w", p.Cfg.Addr, err)
	}
	// 零命中分支显式关连接后置 nil(垫平期间不占目录连接槽),defer
	// 置 nil 守卫防双关;其余路径照旧延迟关。
	defer func() {
		if conn != nil {
			_ = conn.Close()
		}
	}()
	filter := buildUserFilter(p.Cfg.Filter, username)
	// 属性面最小化(review LOW 修):仅取 uid(值 = 搜索代入值,零增量
	// 信息);nil 按 RFC 4511 4.5.1.8 = 全用户属性回传(mail/组/电话全量
	// 进进程,数据最小化破口)。不用 "1.1"(无属性 OID):部分目录实现
	// 忽略它,uid 恒零风险。
	res, err := conn.Search(ldap.NewSearchRequest(
		p.Cfg.BaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, 0, false,
		filter, []string{"uid"}, nil))
	if err != nil {
		return auth.User{}, fmt.Errorf("auth: ldap search under %q: %w", p.Cfg.BaseDN, err)
	}
	switch len(res.Entries) {
	case 0:
		// T20.5 时域垫平:零命中即返比 wrong_password(多一次用户
		// bind + 目录验证)快整整一段,可聚合出账号枚举 oracle。先显式
		// 关连接,再纯客户端等待 levelPad——零目录交互 = 服务账号自锁
		// 风险结构性消亡(D-t176 边界⑤根因不在场);ctx 只可打断等待,
		// 判定已由目录应答决定,打断后照常返回凭据族(不升 500 族)。
		_ = conn.Close()
		conn = nil
		holdLevelPad(ctx, p.levelPad)
		return auth.User{}, &auth.CredentialsError{Username: username, Reason: auth.ReasonUnknownUser}
	case 1:
	default:
		return auth.User{}, fmt.Errorf("auth: ldap search %q: %d entries match, want exactly 1 (directory ambiguity is an identity-source fault, not a login verdict)", filter, len(res.Entries))
	}
	dn := res.Entries[0].DN
	if err := conn.Bind(dn, password); err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
			return auth.User{}, &auth.CredentialsError{Username: username, Reason: auth.ReasonWrongPassword}
		}
		return auth.User{}, fmt.Errorf("auth: ldap user bind: %w", err)
	}
	return auth.AnchorLoginUser(ctx, p.DB, username)
}

// openServiceConn 拨号 + 按配置升级 TLS + 服务账号 bind;返回的连接
// 已持服务身份(搜索面)。「connect」概念含三步:拨号/TLS/服务 bind
// 任一失败 = 目录不可达或配置故障,错误原样上抛由调用方归 500 族。
// cfg 已过 Validate(scheme 词域钉死)。自管拨号(不走 ldap.DialURL):
// ldaps 握手由 tls.DialWithDialer 的 deadline 界住,ldap:// + StartTLS
// 握手由 netConn 绝对 deadline 界住——go-ldap v3.4.14 的 SetTimeout
// 只界消息等待,管不到 StartTLS 内部握手 I/O(对抗审计 MEDIUM 修;
// DialURL 的 ldaps 面已自带 ServerName 验证,此处逐字复刻)。
func openServiceConn(cfg LDAPConfig) (*ldap.Conn, error) {
	u, err := url.Parse(cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("auth: parse addr %q: %w", cfg.Addr, err)
	}
	hostPort := u.Host
	if u.Port() == "" {
		port := "389"
		if u.Scheme == "ldaps" {
			port = "636"
		}
		hostPort = net.JoinHostPort(u.Hostname(), port)
	}
	d := &net.Dialer{Timeout: ldapOpTimeout}
	var netConn net.Conn
	if u.Scheme == "ldaps" {
		netConn, err = tls.DialWithDialer(d, "tcp", hostPort,
			&tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
	} else {
		netConn, err = d.Dial("tcp", hostPort)
	}
	if err != nil {
		return nil, err
	}
	// 整生命周期绝对 deadline:拨号后的一切 I/O(TLS 握手/消息往返)全部
	// 继承;兑现 ldapOpTimeout 包注释不变量。连接即弃(登录 ≤ 四往返),
	// 不留清 deadline 的复杂度。
	if err := netConn.SetDeadline(time.Now().Add(ldapOpTimeout)); err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("auth: set deadline: %w", err)
	}
	conn := ldap.NewConn(netConn, u.Scheme == "ldaps")
	conn.Start()
	if cfg.StartTLS {
		if err := conn.StartTLS(&tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("starttls: %w", err)
		}
	}
	if err := conn.Bind(cfg.BindDN, cfg.BindPassword); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("service bind: %w", err)
	}
	return conn, nil
}

// buildUserFilter 代入模板:EscapeFilter(RFC 4515)转义后的 username
// 替换唯一 %s(恰一 %s 契约在 LDAPConfig.Validate 钉死)。代入用
// strings.Replace 直换而非 Sprintf(review LOW 修)——模板与 printf
// 动词族彻底解耦,%d/%v 等杂动词按字面保留不再产出 %!d(MISSING)
// 垃圾串;转义是纵深第二道——第一道 = auth.ValidUsername 前置门,
// 词域结构性排除 filter 元字符;两道判据独立,失效其一不放大。
func buildUserFilter(tmpl, username string) string {
	return strings.Replace(tmpl, "%s", ldap.EscapeFilter(username), 1)
}

// measureLevelPad 在探测连接上实测 bind 成本(T20.5):3 次成功 rebind
// (同连接重绑定即切换身份,RFC 4511 合法形态)取中位数后钳制。rebind
// 失败 = 探测连接已坏,归启动探测失败族。测量语义:不含拨号/TLS(连接
// 已建),含 bind 消息往返 + 目录侧口令验证——后者正是 wrong_password
// 分支相对零命中分支独有的成本分量。
func measureLevelPad(conn *ldap.Conn, cfg LDAPConfig) (time.Duration, error) {
	samples := make([]time.Duration, 0, levelPadSamples)
	for range levelPadSamples {
		t0 := time.Now()
		if err := conn.Bind(cfg.BindDN, cfg.BindPassword); err != nil {
			return 0, fmt.Errorf("measure rebind: %w", err)
		}
		samples = append(samples, time.Since(t0))
	}
	slices.Sort(samples)
	return clampLevelPad(samples[len(samples)/2]), nil
}

// clampLevelPad 把实测值钳进 [levelPadMin, levelPadMax]。
func clampLevelPad(d time.Duration) time.Duration {
	if d < levelPadMin {
		return levelPadMin
	}
	if d > levelPadMax {
		return levelPadMax
	}
	return d
}

// holdLevelPad 是垫平原语(T20.5):纯客户端等待,零目录交互。timer +
// ctx 双臂 select,ctx 取消立即返回——判定已由目录应答决定,取消
// 不改变调用方的返回值(防把已判定的 401 升成 500 族)。零值/负值
// 直返(字面量构造的 provider 不垫)。
func holdLevelPad(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
