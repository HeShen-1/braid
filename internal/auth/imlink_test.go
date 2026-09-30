package auth

// T14.3 IM 鉴权短链单测(D-t143 决策 2):roundtrip、金向量(官方样例
// 定方向 + 本机 Python hmac/hashlib 实算数值——同会话同理解手写两遍
// 不构成独立,D-t143 决策 1 教训)、篡改/换钥/过期/短密钥、签发词面闸
//(Round/NodeExecID 越界拒签)、TTL 单点覆写。

import (
	"errors"
	"testing"
	"time"
)

const imLinkTestSecret = "braid-im-link-secret-0123456789ab"

func TestIMLinkRoundTrip(t *testing.T) {
	now := time.Unix(1760000000, 0)
	in := IMLinkPayload{
		ExecutionID: 42, NodeExecID: 7, NodeID: "approve_node",
		Approver: "alice", Round: 2, Action: "approve",
	}
	d, s, err := SignIMLink(imLinkTestSecret, in, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	out, err := VerifyIMLink(imLinkTestSecret, d, s, now.Add(23*time.Hour))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	// ExpiresAt 是签发侧单点覆写,roundtrip 还原的即覆写值。
	in.ExpiresAt = now.Add(IMLinkTTL).Unix()
	if out != in {
		t.Fatalf("payload mismatch: got %+v want %+v", out, in)
	}
}

// 金向量:payload JSON 与 HMAC 由本机 Python 独立实算(secret 同
// imLinkTestSecret,33 字节 ≥ MinIMLinkSecretLen 32;now=1760000000 ⇒
// x=now+86400)。Go 侧只对拍 d/s 字节——编码顺序、base64url 无 padding、
// HMAC 输入面任一漂移即红。
func TestIMLinkGoldenVector(t *testing.T) {
	const wantD = "eyJlIjo0MiwibiI6NywiZCI6ImFwcHJvdmVfbm9kZSIsImEiOiJhbGljZSIsInIiOjIsInYiOiJhcHByb3ZlIiwieCI6MTc2MDA4NjQwMH0"
	const wantS = "ECXFbCGpQdBUi1sj4We7Phe6ZjNquzRFE5J8LV_Tjxw"
	d, s, err := SignIMLink(imLinkTestSecret, IMLinkPayload{
		ExecutionID: 42, NodeExecID: 7, NodeID: "approve_node",
		Approver: "alice", Round: 2, Action: "approve",
	}, time.Unix(1760000000, 0))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if d != wantD || s != wantS {
		t.Fatalf("golden mismatch:\n d=%s\n s=%s", d, s)
	}
}

func TestIMLinkVerifyRejects(t *testing.T) {
	now := time.Unix(1760000000, 0)
	d, s, err := SignIMLink(imLinkTestSecret, IMLinkPayload{
		ExecutionID: 1, NodeExecID: 2, NodeID: "ap", Approver: "bob", Round: 1, Action: "reject",
	}, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	if _, err := VerifyIMLink(imLinkTestSecret, flipLast(d), s, now); !errors.Is(err, ErrIMLinkInvalid) {
		t.Fatalf("tampered d: err=%v want ErrIMLinkInvalid", err)
	}
	if _, err := VerifyIMLink(imLinkTestSecret, d, flipLast(s), now); !errors.Is(err, ErrIMLinkInvalid) {
		t.Fatalf("tampered s: err=%v want ErrIMLinkInvalid", err)
	}
	if _, err := VerifyIMLink("braid-im-link-OTHER-0123456789ab", d, s, now); !errors.Is(err, ErrIMLinkInvalid) {
		t.Fatalf("wrong secret: err=%v want ErrIMLinkInvalid", err)
	}
	// 过期分立:签名有效、now 越 x → ErrIMLinkExpired(不是 Invalid)。
	if _, err := VerifyIMLink(imLinkTestSecret, d, s, now.Add(IMLinkTTL)); !errors.Is(err, ErrIMLinkExpired) {
		t.Fatalf("expired: err=%v want ErrIMLinkExpired", err)
	}
	// 边界:x 当刻已过(exp 语义 = now >= x 即拒)。
	if _, err := VerifyIMLink(imLinkTestSecret, d, s, time.Unix(now.Add(IMLinkTTL).Unix(), 0)); !errors.Is(err, ErrIMLinkExpired) {
		t.Fatalf("expiry boundary: err=%v want ErrIMLinkExpired", err)
	}
	// 坏 base64 / 短签名面。
	if _, err := VerifyIMLink(imLinkTestSecret, "!!!not-base64!!!", s, now); !errors.Is(err, ErrIMLinkInvalid) {
		t.Fatalf("bad d encoding: err=%v want ErrIMLinkInvalid", err)
	}
	if _, err := VerifyIMLink(imLinkTestSecret, d, "c2hvcnQ", now); !errors.Is(err, ErrIMLinkInvalid) {
		t.Fatalf("short sig: err=%v want ErrIMLinkInvalid", err)
	}
	// 短密钥:签发侧响亮报错、验证侧归 Invalid。
	if _, _, err := SignIMLink("short", IMLinkPayload{}, now); err == nil {
		t.Fatal("sign with short secret: want error")
	}
	if _, err := VerifyIMLink("short", d, s, now); !errors.Is(err, ErrIMLinkInvalid) {
		t.Fatalf("verify with short secret: err=%v want ErrIMLinkInvalid", err)
	}
	// 签发词面闸(对抗审计 LOW 修):消费侧哨兵值(Round<1 / NodeExecID=0)
	// 结构性不可签出——api 轮绑定守卫的 -1 哨兵与 0 零值进不了签名词面。
	for name, p := range map[string]IMLinkPayload{
		"round zero":              {ExecutionID: 1, NodeExecID: 2, NodeID: "ap", Approver: "bob", Round: 0, Action: "approve"},
		"round negative sentinel": {ExecutionID: 1, NodeExecID: 2, NodeID: "ap", Approver: "bob", Round: -1, Action: "approve"},
		"node exec zero":          {ExecutionID: 1, NodeExecID: 0, NodeID: "ap", Approver: "bob", Round: 1, Action: "approve"},
	} {
		if _, _, err := SignIMLink(imLinkTestSecret, p, now); err == nil {
			t.Fatalf("mint with %s: want loud reject", name)
		}
	}
}

// flipLast 翻转中位字符(保 base64url 词面内变异:+1;不翻末位——
// base64url 尾字符可有 4 个无效位,翻它解码可能不变,变异即空转;
// token_test 的 flip 已占用,本文件另名)。
func flipLast(v string) string {
	b := []byte(v)
	b[len(b)/2]++
	return string(b)
}
