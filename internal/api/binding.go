// token 绑定的 api 侧半边(T11.3,D-2026-09-29-t113-auth-depth #1):
// 模式词表 + 指纹计算单点。auth 包管 bnd claim 的签发/解析/格式闸;
// 本文件管「值从哪来、怎么比」——签发点(login + OIDC 回调)与校验点
// (Authenticate)共用同一 bindingFingerprint,复算比对才可能成立。
//
// 威胁模型(如实边界):防 token 外泄后异客户端重放(脚本外发/日志泄
// 漏);同浏览器 in-place XSS 可读 navigator.userAgent 伪造,不设防
// (CSP+textContent 缓解面不变,D-t44)。POS(cookie 双证)形态已否决
// (破坏程序化 API 契约 + 重开 D-t44 已关闭的 CSRF 面)。
package api

import (
	"crypto/sha256"
	"encoding/base64"
	"net/netip"
)

// TokenBindingMode 是 BRAID_API_TOKEN_BINDING 的闭集词表:off(缺省,
// 签发不带 claim、校验零介入,零行为变化)/ ua(绑 User-Agent)/
// ua_ip24(再叠 IP 前缀:v4 /24、v6 /64——前缀粒度容忍网段内漂移,
// ClientIP 经 TrustedProxies 契约)。空串与 off 同义(Config 字面构造
// 的零值合法,既有装配零扰动)。
type TokenBindingMode string

const (
	TokenBindingOff    TokenBindingMode = "off"
	TokenBindingUA     TokenBindingMode = "ua"
	TokenBindingUAIP24 TokenBindingMode = "ua_ip24"
)

// validTokenBinding 报告词表内(含空串 = off 同义)。
func validTokenBinding(m TokenBindingMode) bool {
	switch m {
	case "", TokenBindingOff, TokenBindingUA, TokenBindingUAIP24:
		return true
	}
	return false
}

// bindingFingerprint 按 mode 计算绑定指纹:SHA-256 输入前 16 字节的
// base64url 无 padding 定长 22 字符(与 auth 侧 bindingRe 同形)。
// off(及空串)返回空串 = 不携带 claim。
//
// 已知边界(D-t113 登记):空 User-Agent 客户端的指纹退化为全类共享
// 常量(SHA-256("")),UA 缺席客户端之间互可重放——主面(浏览器/常见
// HTTP 库恒送 UA)不受影响;不给空 UA 拒绝服务(那会把无 UA 客户端
// 整体锁死在登录面外)。
func bindingFingerprint(mode TokenBindingMode, userAgent, clientIP string) string {
	switch mode {
	case TokenBindingUA:
		return bindingHash(userAgent)
	case TokenBindingUAIP24:
		return bindingHash(userAgent + "|" + ipPrefixText(clientIP))
	default:
		return ""
	}
}

// bindingHash 是指纹的编码形态单点。
func bindingHash(in string) string {
	sum := sha256.Sum256([]byte(in))
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

// ipPrefixText 折叠 IP 到前缀文本:v4 → /24、v6 → /64(Masked 规范形
// 态,v4-mapped 统一 Unmap 成 v4 同判)。解析失败/空串回退原文:指纹
// 输入必须确定性优先——「失败即拒」会把 ClientIP 形态异常放大成登录
// 面不可用,而回退仍保住「同输入同指纹」的比对语义。
func ipPrefixText(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	if addr.Is4() || addr.Is4In6() {
		return netip.PrefixFrom(addr.Unmap(), 24).Masked().String()
	}
	return netip.PrefixFrom(addr, 64).Masked().String()
}
