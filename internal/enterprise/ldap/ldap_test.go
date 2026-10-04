package ldap

// LDAP bind 单元面(零栈,零 docker):filter 转义向量、触线前双闸
// (词域门 + 空口令拒收)、不可达目录的 500 族归位、启动期探测响亮败。
// 容器化全链(success/unknown_user/wrong_password/disabled/歧义/
// login_audit method='ldap')= ldap_e2e_test.go(T17.6 验收方案)。

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"braid/internal/auth"
)

// closedPortAddr 指向必然拒连的本机端口(1 = tcpmux,常无监听):
// 触线路径的确定性「目录不可达」,不依赖 docker。
const closedPortAddr = "ldap://127.0.0.1:1"

func closedPortCfg() LDAPConfig {
	return LDAPConfig{
		Addr: closedPortAddr, BindDN: "cn=svc,dc=example,dc=org", BindPassword: "svc-secret",
		BaseDN: "dc=example,dc=org", Filter: DefaultLDAPFilter,
	}
}

// TestBuildUserFilter:RFC 4515 转义向量(纵深第二道;第一道 =
// ValidUsername 词域门,这些输入在真实链路里触不到线——单元面直接
// 钉转义行为本身,防后续重构换掉转义单点)。
func TestBuildUserFilter(t *testing.T) {
	tests := []struct {
		name, tmpl, in, want string
	}{
		{"plain", "(uid=%s)", "alice", "(uid=alice)"},
		{"star", "(uid=%s)", "a*b", `(uid=a\2ab)`},
		{"parens", "(uid=%s)", "a(b)c", `(uid=a\28b\29c)`},
		{"backslash", "(uid=%s)", `a\b`, `(uid=a\5cb)`},
		{"nul", "(uid=%s)", "a\x00b", `(uid=a\00b)`},
		{"compound template", "(&(objectClass=person)(uid=%s))", "eve", "(&(objectClass=person)(uid=eve))"},
		// review LOW 修:代入 = strings.Replace 直换,printf 杂动词按字面
		// 保留(不再产出 %!d(MISSING);Validate 的恰一 %s 契约仍在)。
		{"stray printf verb stays literal", "(uid=%s)(ou=%d)", "eve", "(uid=eve)(ou=%d)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildUserFilter(tt.tmpl, tt.in); got != tt.want {
				t.Fatalf("buildUserFilter(%q, %q) = %q, want %q", tt.tmpl, tt.in, got, tt.want)
			}
		})
	}
}

// TestAuthenticatePreWireGates:触线前双闸在拨号前判定——closedPortAddr
// 若被触线即得拨号错误(非凭据族),闸命中则凭据族错误先返,证明
// 词域外/空口令路径零目录交互(不付费拨号、不泄漏目录存在性)。
func TestAuthenticatePreWireGates(t *testing.T) {
	p := LDAPBindProvider{Cfg: closedPortCfg(), DB: nil}

	t.Run("word-domain invalid maps to invalid_input pre-wire", func(t *testing.T) {
		_, err := p.Authenticate(context.Background(), "A*B(c)", "pw")
		var ce *auth.CredentialsError
		if !errors.As(err, &ce) {
			t.Fatalf("err = %v, want CredentialsError", err)
		}
		if ce.Reason != auth.ReasonInvalidInput {
			t.Fatalf("reason = %q, want %q", ce.Reason, auth.ReasonInvalidInput)
		}
	})
	t.Run("empty password maps to wrong_password pre-wire (anonymous-bind guard)", func(t *testing.T) {
		_, err := p.Authenticate(context.Background(), "alice", "")
		var ce *auth.CredentialsError
		if !errors.As(err, &ce) {
			t.Fatalf("err = %v, want CredentialsError", err)
		}
		if ce.Reason != auth.ReasonWrongPassword {
			t.Fatalf("reason = %q, want %q", ce.Reason, auth.ReasonWrongPassword)
		}
	})
	t.Run("in-domain input reaches the wire: connect error, never credentials-family", func(t *testing.T) {
		_, err := p.Authenticate(context.Background(), "alice", "pw")
		if err == nil {
			t.Fatal("closed port must fail")
		}
		var ce *auth.CredentialsError
		if errors.As(err, &ce) {
			t.Fatalf("identity-source fault misclassified as login verdict: %v", err)
		}
		if !strings.Contains(err.Error(), "ldap connect") {
			t.Fatalf("err = %v, want connect-family wrap", err)
		}
	})
}

// TestNewLDAPBindProviderProbeFail:启动期探测不可达 = 构造即败(镜像
// entoidc discovery 启动期拉取:配置错误不伪装成运行期登录 500)。
func TestNewLDAPBindProviderProbeFail(t *testing.T) {
	_, err := NewLDAPBindProvider(context.Background(), closedPortCfg(), nil)
	if err == nil {
		t.Fatal("closed port must fail the startup probe")
	}
	if !strings.Contains(err.Error(), "startup probe") {
		t.Fatalf("err = %v, want startup-probe wrap", err)
	}
}

// TestAuthenticateBoundedOnStalledDirectory:ldapOpTimeout 不变量的零栈
// 守卫臂(对抗审计 LOW 修)——accept 后静默不答的目录形态(正是注释
// 要防的「无界挂死」),Authenticate 必须在上界内以身份源故障(500 族)
// 返回而非挂死。不设 elapsed 断言(时序 flake 面);有界性本身即断言
// ——若超时界回归丢失,15s watchdog 红(差于包超时的一锅端,诊断面
// 更准)。
func TestAuthenticateBoundedOnStalledDirectory(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			_ = c // 握手即静默:不读不写,模拟目录应答挂死
		}
	}()
	p := LDAPBindProvider{Cfg: LDAPConfig{
		Addr: "ldap://" + ln.Addr().String(), BindDN: "cn=svc,dc=example,dc=org",
		BindPassword: "svc-secret", BaseDN: "dc=example,dc=org", Filter: DefaultLDAPFilter,
	}, DB: nil}
	done := make(chan error, 1)
	go func() {
		_, err := p.Authenticate(context.Background(), "alice", "pw")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled directory must not authenticate")
		}
		var ce *auth.CredentialsError
		if errors.As(err, &ce) {
			t.Fatalf("identity-source fault misclassified as login verdict: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Authenticate hung past the op-timeout bound (stalled directory must not hang the login path)")
	}
}
