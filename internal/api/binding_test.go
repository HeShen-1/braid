package api

// token 绑定指纹单测(T11.3,D-2026-09-29-t113 #1):模式分流、定长形
// 态(与 auth.bindingRe 同词域)、IP 前缀折叠语义、回退确定性。签发→
// 校验的 HTTP 闭环锚在 middleware_test(TestAuthenticateBinding)与
// login/oidc 集成测试,本文件只锚纯函数面。

import (
	"regexp"
	"testing"
)

func TestBindingFingerprintModes(t *testing.T) {
	const ua = "Mozilla/5.0 braid-test"
	if got := bindingFingerprint(TokenBindingOff, ua, "10.0.0.1"); got != "" {
		t.Fatalf("off 模式指纹非空: %q", got)
	}
	// 空串 = off 同义(字面构造零值)。
	if got := bindingFingerprint("", ua, "10.0.0.1"); got != "" {
		t.Fatalf("空串模式指纹非空: %q", got)
	}
	uaFP := bindingFingerprint(TokenBindingUA, ua, "10.0.0.1")
	if uaFP == "" {
		t.Fatal("ua 模式指纹为空")
	}
	// IP 不参与 ua 模式。
	if got := bindingFingerprint(TokenBindingUA, ua, "10.9.9.9"); got != uaFP {
		t.Fatalf("ua 模式随 IP 漂移: %q vs %q", got, uaFP)
	}
	// UA 变化 → 指纹变化。
	if got := bindingFingerprint(TokenBindingUA, "curl/8.0", "10.0.0.1"); got == uaFP {
		t.Fatal("不同 UA 产出相同指纹")
	}
}

// bindingShapeRe 与 auth.bindingRe 同词域(base64url 无 padding 恰 22
// 字符):api 侧产出必须能过 auth 侧格式闸,否则签发即拒。
var bindingShapeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

func TestBindingFingerprintShape(t *testing.T) {
	for _, m := range []TokenBindingMode{TokenBindingUA, TokenBindingUAIP24} {
		fp := bindingFingerprint(m, "Mozilla/5.0", "2001:db8:1:2:3:4:5:6")
		if !bindingShapeRe.MatchString(fp) {
			t.Fatalf("mode %s 指纹 %q 不符 auth.bindingRe 词域", m, fp)
		}
	}
}

func TestBindingFingerprintIPPrefix(t *testing.T) {
	const ua = "Mozilla/5.0"
	same := bindingFingerprint(TokenBindingUAIP24, ua, "10.1.2.3")
	// 同 /24 网段漂移容忍。
	if got := bindingFingerprint(TokenBindingUAIP24, ua, "10.1.2.254"); got != same {
		t.Fatalf("同 /24 指纹漂移: %q vs %q", got, same)
	}
	// 跨网段 → 失配。
	if got := bindingFingerprint(TokenBindingUAIP24, ua, "10.1.3.1"); got == same {
		t.Fatal("跨 /24 指纹未变")
	}
	// v6 /64 同前缀容忍、跨前缀失配。
	v6 := bindingFingerprint(TokenBindingUAIP24, ua, "2001:db8:1:2:aaaa::1")
	if got := bindingFingerprint(TokenBindingUAIP24, ua, "2001:db8:1:2:bbbb::9999"); got != v6 {
		t.Fatalf("同 /64 指纹漂移: %q vs %q", got, v6)
	}
	if got := bindingFingerprint(TokenBindingUAIP24, ua, "2001:db8:1:3::1"); got == v6 {
		t.Fatal("跨 /64 指纹未变")
	}
	// v4-mapped 形态与纯 v4 同前缀(netip Unmap 折叠,两形态同判)。
	if got := bindingFingerprint(TokenBindingUAIP24, ua, "::ffff:10.1.2.7"); got != same {
		t.Fatalf("v4-mapped 与纯 v4 前缀不一致: %q vs %q", got, same)
	}
	// v6 环回折叠形:::1 → "::/64"(Masked 规范形态;对抗审计 LOW——
	// 原先只锚 2001:db8 族,环回折叠无锚)。
	if got := ipPrefixText("::1"); got != "::/64" {
		t.Fatalf("::1 折叠 = %q, want \"::/64\"", got)
	}
	if got := bindingFingerprint(TokenBindingUAIP24, ua, "::1"); got == v6 {
		t.Fatal("::1 与 2001:db8 前缀产出相同指纹")
	}
}

func TestIPPrefixTextFallback(t *testing.T) {
	// 非法/空 IP 回退原文:指纹输入确定性优先(「失败即拒」会把
	// ClientIP 形态异常放大成登录面不可用)。
	if got := ipPrefixText(""); got != "" {
		t.Fatalf("空串回退: %q", got)
	}
	if got := ipPrefixText("not-an-ip"); got != "not-an-ip" {
		t.Fatalf("非法输入回退: %q", got)
	}
}

func TestValidTokenBindingVocab(t *testing.T) {
	for _, ok := range []string{"", "off", "ua", "ua_ip24"} {
		if !validTokenBinding(TokenBindingMode(ok)) {
			t.Fatalf("%q 应为词表内(空串 = off 同义)", ok)
		}
	}
	for _, bad := range []string{"on", "UA", "ip24", "ua-ip24", "ua_ip24 "} {
		if validTokenBinding(TokenBindingMode(bad)) {
			t.Fatalf("%q 不应在词表内", bad)
		}
	}
}
