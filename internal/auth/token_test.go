package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// pastClaims 构造完整合法 claims 但 exp 在过去——过期 token 的标准
// 造法(直构经包内 signClaims 出口,零时钟注入)。
func pastClaims(username string, roles []Role) Claims {
	past := time.Now().UTC().Add(-time.Minute)
	return Claims{Roles: roles, RegisteredClaims: jwt.RegisteredClaims{
		Subject:   username,
		IssuedAt:  jwt.NewNumericDate(past.Add(-time.Hour)),
		ExpiresAt: jwt.NewNumericDate(past),
		Issuer:    tokenIssuer,
		Audience:  jwt.ClaimStrings{tokenAudience},
	}}
}

func validClaims(username string, roles []Role) Claims {
	now := time.Now().UTC()
	return Claims{Roles: roles, RegisteredClaims: jwt.RegisteredClaims{
		Subject:   username,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		Issuer:    tokenIssuer,
		Audience:  jwt.ClaimStrings{tokenAudience},
	}}
}

func TestSignParseRoundTrip(t *testing.T) {
	tok, err := SignToken(testSecret, time.Hour, "alice", []Role{RoleAuthor, RoleApprover})
	if err != nil {
		t.Fatalf("SignToken: %v", err)
	}
	c, err := ParseToken(testSecret, tok)
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	if c.Subject != "alice" {
		t.Fatalf("sub = %q, want alice", c.Subject)
	}
	if len(c.Roles) != 2 || c.Roles[0] != RoleAuthor || c.Roles[1] != RoleApprover {
		t.Fatalf("roles = %v, want [author approver]", c.Roles)
	}
	if c.Issuer != tokenIssuer || len(c.Audience) != 1 || c.Audience[0] != tokenAudience {
		t.Fatalf("iss/aud = %q/%v", c.Issuer, c.Audience)
	}
	if c.IssuedAt == nil || c.ExpiresAt == nil || !c.ExpiresAt.After(c.IssuedAt.Time) {
		t.Fatalf("iat/exp 缺失或倒置: %v %v", c.IssuedAt, c.ExpiresAt)
	}
}

func TestParseExpired(t *testing.T) {
	tok, err := signClaims(testSecret, pastClaims("alice", []Role{RoleAdmin}))
	if err != nil {
		t.Fatalf("signClaims: %v", err)
	}
	_, err = ParseToken(testSecret, tok)
	// 互斥断言:过期必须是 ErrTokenExpired 且不是 ErrTokenInvalid
	// (防两类失败分类塌缩成一类,消费方失去「重登录 vs 重新认证」区分)。
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("err = %v, want ErrTokenExpired", err)
	}
	if errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("过期 token 被归入 ErrTokenInvalid,分类塌缩")
	}
}

// 篡改签名段(JWT 三段末段)首字符:必然走 HMAC 比对失败 →
// ErrTokenInvalid。变异锚名副其实:若解析器被变异成「跳过签名校验」,
// 本测试红(此前版本篡改的是 header 段,拒绝发生在 alg 解析步,
// 锚点与名义不符——review 实测纠正)。
func TestParseTamperedSignature(t *testing.T) {
	tok, _ := SignToken(testSecret, time.Hour, "alice", []Role{RoleAdmin})
	b := []byte(tok)
	b[len(b)-1] = flip(b[len(b)-1])
	_, err := ParseToken(testSecret, string(b))
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
	if errors.Is(err, ErrTokenExpired) {
		t.Fatalf("篡改被误报为过期")
	}
}

func flip(b byte) byte {
	if b == 'a' {
		return 'b'
	}
	return 'a'
}

func TestParseWrongSecret(t *testing.T) {
	tok, _ := SignToken(testSecret, time.Hour, "alice", []Role{RoleAdmin})
	if _, err := ParseToken(altSecret, tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}

// 变异验证:删掉 ParseToken 的 jwt.WithValidMethods 选项,本测试红
// (已实测);TestParseAlgNoneRejected 另受 v5 keyfunc 对 none 的密钥
// 语义加固,白名单的杀死性断言以 HS512 为准。
func TestParseAlgNoneRejected(t *testing.T) {
	tok, err := jwt.NewWithClaims(jwt.SigningMethodNone, validClaims("alice", []Role{RoleAdmin})).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("craft none token: %v", err)
	}
	if _, err := ParseToken(testSecret, tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("alg=none token 被接受")
	}
}

func TestParseAlgHS512Rejected(t *testing.T) {
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS512, validClaims("alice", []Role{RoleAdmin})).
		SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("craft hs512 token: %v", err)
	}
	if _, err := ParseToken(testSecret, tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("alg=HS512 token 被接受(白名单只认 HS256)")
	}
}

// 变异验证:删掉 jwt.WithExpirationRequired,无 exp 的合法签名 token
// 会被静默接受——本测试是其杀死性断言。
func TestParseMissingExpRejected(t *testing.T) {
	c := validClaims("alice", []Role{RoleAdmin})
	c.ExpiresAt = nil
	tok, err := signClaims(testSecret, c)
	if err != nil {
		t.Fatalf("signClaims: %v", err)
	}
	if _, err := ParseToken(testSecret, tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("无 exp 的 token 被接受")
	}
}

func TestParseWrongIssuerAudience(t *testing.T) {
	badIss := validClaims("alice", []Role{RoleAdmin})
	badIss.Issuer = "evil"
	tok, _ := signClaims(testSecret, badIss)
	if _, err := ParseToken(testSecret, tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("错 issuer 被接受")
	}

	badAud := validClaims("alice", []Role{RoleAdmin})
	badAud.Audience = jwt.ClaimStrings{"other-service"}
	tok, _ = signClaims(testSecret, badAud)
	if _, err := ParseToken(testSecret, tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("错 audience 被接受")
	}
}

// 变异验证:删掉 ParseToken 的 validRoles 后置校验,正确密钥签发但
// 携词表外角色(或空角色)的 token 会被接受——fail-closed 的杀死性
// 断言(伪造 claims 需持密钥,防线针对的是「签发面 bug + 消费面信任
// 载荷」的组合)。
func TestParseRolesVocabFailClosed(t *testing.T) {
	badRole := validClaims("alice", []Role{Role("root")})
	tok, _ := signClaims(testSecret, badRole)
	if _, err := ParseToken(testSecret, tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("词表外角色 role=root 被接受")
	}

	empty := validClaims("alice", nil)
	tok, _ = signClaims(testSecret, empty)
	if _, err := ParseToken(testSecret, tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("空角色 token 被接受")
	}
}

func TestParseEmptySubject(t *testing.T) {
	c := validClaims("", []Role{RoleAdmin})
	tok, _ := signClaims(testSecret, c)
	if _, err := ParseToken(testSecret, tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("空 sub 被接受")
	}
}

func TestSignTokenRejectsBadInput(t *testing.T) {
	if _, err := SignToken(testSecret, 0, "alice", []Role{RoleAdmin}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("ttl=0: %v", err)
	}
	if _, err := SignToken(testSecret, -time.Minute, "alice", []Role{RoleAdmin}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("负 ttl: %v", err)
	}
	// 吊销上界在签发原语处自洽(不止 Config.Validate):超 7d 拒绝,
	// 恰在边界放行。
	if _, err := SignToken(testSecret, MaxTokenTTL+time.Second, "alice", []Role{RoleAdmin}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("超上界 ttl: %v", err)
	}
	if _, err := SignToken(testSecret, MaxTokenTTL, "alice", []Role{RoleAdmin}); err != nil {
		t.Fatalf("上界边界应合法: %v", err)
	}
	if _, err := SignToken("short", time.Hour, "alice", []Role{RoleAdmin}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("短 secret: %v", err)
	}
	if _, err := SignToken(testSecret, time.Hour, "Bad Name", []Role{RoleAdmin}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("非法 username: %v", err)
	}
	if _, err := SignToken(testSecret, time.Hour, "alice", []Role{Role("root")}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("词表外角色: %v", err)
	}
}

func TestParseShortSecretRejected(t *testing.T) {
	tok, _ := SignToken(testSecret, time.Hour, "alice", []Role{RoleAdmin})
	if _, err := ParseToken("too-short-secret", tok); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("短 secret: %v, want ErrInvalidInput(配置误用,非 token 问题)", err)
	}
}

func TestRequireRole(t *testing.T) {
	c := validClaims("alice", []Role{RoleAuthor, RoleApprover})
	if err := RequireRole(c, RoleAuthor); err != nil {
		t.Fatalf("持角色被拒: %v", err)
	}
	if err := RequireRole(c, RoleApprover); err != nil {
		t.Fatalf("持角色被拒: %v", err)
	}
	err := RequireRole(c, RoleAdmin)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("角色越权: err = %v, want ErrForbidden", err)
	}
	if !errors.Is(RequireRole(c, Role("root")), ErrInvalidInput) {
		t.Fatalf("未知角色入参未前置拒绝")
	}
}
