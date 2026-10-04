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
package ldap

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
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

// NewLDAPBindProvider 构造 + 启动期连通性探测(镜像 entoidc.
// NewOIDCProvider 的 discovery 启动期拉取:目录不可达/服务凭据错 =
// 启动失败,不把配置错误伪装成运行期登录 500)。探测 = 拨号 +
// (可选 StartTLS)+ 服务账号 bind + 关闭;零搜索,不触碰目录数据。
// 探测连接不缓存:登录期逐次拨号(bind 语义不可复用连接身份)。
func NewLDAPBindProvider(ctx context.Context, cfg LDAPConfig, db auth.DBTX) (LDAPBindProvider, error) {
	// ctx 留在签名面:探测超时由 ldapOpTimeout 界住(镜像 entoidc
	// NewOIDCProvider 的 ctx 形参;go-ldap v3 API 不携 ctx)。
	conn, err := openServiceConn(cfg)
	if err != nil {
		return LDAPBindProvider{}, fmt.Errorf("auth: ldap startup probe %q: %w", cfg.Addr, err)
	}
	_ = conn.Close() // 探测连接不缓存:bind 语义不可复用连接身份
	return LDAPBindProvider{Cfg: cfg, DB: db}, nil
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
	defer func() { _ = conn.Close() }()
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
