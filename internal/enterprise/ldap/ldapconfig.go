// LDAP bind 插件配置组(T17.6,D-2026-10-04-t176):BRAID_LDAP_* 半开
// 语义镜像 entoidc.OIDCConfig;BindPassword 仅经 env 注入,不进日志/
// 审计(铁律)。实现归本包,缝面(auth.IdentityProvider)留核心。
package ldap

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// DefaultLDAPFilter 是 BRAID_LDAP_FILTER 的缺省模板:恰一个 %s 代入
// username(RFC 4515 转义后)。uid 为主键属性是目录事实惯例,精确等值
// filter——非子串匹配,零目录端宽放。%% 转义不支持(企业 filter 不需要,
// 恰一 %s 契约在 Validate 钉死)。
const DefaultLDAPFilter = "(uid=%s)"

// LDAPConfig 是 LDAP bind 插件配置组(BRAID_LDAP_*)。半开语义:组整体
// 缺省 = LDAP 关闭(idp 保持内置 BuiltinProvider,登录面零变化);任一
// 键设而必填不全 = 启动即败(正向/反向半配置双闸,OIDC 同款)。
type LDAPConfig struct {
	Addr         string // ldap://host:port | ldaps://host:port(TLS 由方案承载)
	BindDN       string // 服务账号 DN:search 前先 bind(search-then-bind 形态)
	BindPassword string // 服务账号密码(env-only,不进日志)
	BaseDN       string // 用户条目搜索基
	Filter       string // 搜索模板,恰一个 %s;缺省 DefaultLDAPFilter
	StartTLS     bool   // ldap:// 上的 StartTLS 升级;ldaps:// 下矛盾即拒
}

// Configured 报告配置组是否任一键被设置(Validate 的半开判据)。STARTTLS
// 显式 "0" 单独设置同样算组内已设,与 Filter/必填键同判:任一键设而必填
// 不全 = 启动即败(显式触碰过本组就该响亮表态,不为布尔假值开特例;
// D-t176 决策 5 半配置双闸口径;review LOW 修——原「判 off」注释与
// 实际控制流不符)。Parse 的原始串判据同口径(rawTLS 非空即不入
// 「全缺省」早退)。
func (c LDAPConfig) Configured() bool {
	return c.Addr != "" || c.BindDN != "" || c.BindPassword != "" || c.BaseDN != "" ||
		c.Filter != "" || c.StartTLS
}

// Validate 启动期断言:组全或无、Addr 形态(scheme ∈ {ldap, ldaps} +
// host 非空)、Filter 恰一 %s、StartTLS 与 ldaps 方案矛盾拒。
func (c LDAPConfig) Validate() error {
	if !c.Configured() {
		return nil
	}
	var missing []string
	if c.Addr == "" {
		missing = append(missing, "BRAID_LDAP_ADDR")
	}
	if c.BindDN == "" {
		missing = append(missing, "BRAID_LDAP_BIND_DN")
	}
	if c.BindPassword == "" {
		missing = append(missing, "BRAID_LDAP_BIND_PASSWORD")
	}
	if c.BaseDN == "" {
		missing = append(missing, "BRAID_LDAP_BASE_DN")
	}
	if len(missing) > 0 {
		return fmt.Errorf("auth: ldap half-configured: missing %s", strings.Join(missing, ", "))
	}
	u, err := url.Parse(c.Addr)
	if err != nil {
		return fmt.Errorf("auth: BRAID_LDAP_ADDR %q: %w", c.Addr, err)
	}
	if u.Scheme != "ldap" && u.Scheme != "ldaps" {
		return fmt.Errorf("auth: BRAID_LDAP_ADDR %q: unsupported scheme %q (want ldap:// or ldaps://)", c.Addr, u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("auth: BRAID_LDAP_ADDR %q: missing host", c.Addr)
	}
	if n := strings.Count(c.Filter, "%s"); n != 1 {
		return fmt.Errorf("auth: BRAID_LDAP_FILTER=%q: want exactly one %%s substitution, got %d", c.Filter, n)
	}
	// 杂散 % 序列拒收(对抗审计 LOW 修):%%s 能过上面的 %s 计数,请求期
	// 才以坏 filter 炸成全量登录 500——词域在启动期收紧:filter 里唯一
	// 合法的 % 就是那一个 %s(buildUserFilter 用 strings.Replace 直换,
	// 无 printf 动词语义,%d 等按字面本就不合法目录 filter)。
	if strings.Count(c.Filter, "%") != 1 {
		return fmt.Errorf("auth: BRAID_LDAP_FILTER=%q: stray %% sequences rejected (the only legal %% is the single %%s)", c.Filter)
	}
	if c.StartTLS && u.Scheme == "ldaps" {
		return fmt.Errorf("auth: BRAID_LDAP_STARTTLS with ldaps:// addr: TLS already carried by the scheme, StartTLS upgrade is contradictory")
	}
	return nil
}

// ParseLDAPConfig 从 BRAID_LDAP_* 组构造(getenv 注入,ConfigFromEnv
// 惯例;消费方 = cmd/api 组合根直呼,镜像 entoidc.ParseOIDCConfig)。
// 全组缺省返回零值(LDAP 关闭);Filter 缺省在此补齐;STARTTLS 键非空
// 即解析布尔(词域外响亮拒);半开经 Validate 启动即败。
func ParseLDAPConfig(getenv func(string) string) (LDAPConfig, error) {
	c := LDAPConfig{
		Addr:         getenv("BRAID_LDAP_ADDR"),
		BindDN:       getenv("BRAID_LDAP_BIND_DN"),
		BindPassword: getenv("BRAID_LDAP_BIND_PASSWORD"),
		BaseDN:       getenv("BRAID_LDAP_BASE_DN"),
		Filter:       getenv("BRAID_LDAP_FILTER"),
	}
	rawTLS := getenv("BRAID_LDAP_STARTTLS")
	if !c.Configured() && rawTLS == "" {
		return LDAPConfig{}, nil
	}
	if rawTLS != "" {
		v, err := strconv.ParseBool(rawTLS)
		if err != nil {
			return LDAPConfig{}, fmt.Errorf("auth: BRAID_LDAP_STARTTLS=%q: want bool (1/t/true/0/f/false)", rawTLS)
		}
		c.StartTLS = v
	}
	if c.Filter == "" {
		c.Filter = DefaultLDAPFilter
	}
	if err := c.Validate(); err != nil {
		return LDAPConfig{}, err
	}
	return c, nil
}
