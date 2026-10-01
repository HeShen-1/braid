package oidc

// OIDC redirect 流 provider(T11.1):StartAuth 形状锚(PKCE S256/nonce/
// state)+ Complete 全错误族(经 oidctest 假 IdP 走真 exchange/verify)。
// 触库用例走 t13(newDB);传输/协议面用例注入 nil dbh 证明零触库。
// T16.1 随实现迁企业增值面(D-2026-10-01-t161);本地锚定走核心缝
// auth.AnchorLoginUser,错误族断言面向 auth 包符号。
//
// 变异自查:StartAuth 去 nonce 参数 → nonce 断言红(假 IdP 回嵌空) +
// nonce_mismatch 行红;去掉回显 state 比对 → state_echo_mismatch 行红;
// Complete 去验签委托 → bad_audience/expired_token 行红(假 IdP 签发
// 未经变更,仅 claim 覆盖)。

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"braid/internal/auth"
	"braid/internal/enterprise/oidc/oidctest"
)

func sha256SumForTest(b []byte) string {
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// startNonOIDCServer 起一个非 OIDC 端点(discovery 404)。
func startNonOIDCServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const oidcTestClientSecret = "unit-test-oidc-client-secret-words-placeholder"

// newOIDCProvider 构造指向假 IdP 的 provider;claim 空 = 缺省
// (preferred_username);dbh 可为 nil(传输/协议面零触库证明)。
func newOIDCProvider(t *testing.T, idp *oidctest.FakeIdP, dbh auth.DBTX, claim string) *OIDCProvider {
	t.Helper()
	cfg := OIDCConfig{
		Issuer:       idp.Issuer(),
		ClientID:     idp.ClientID,
		ClientSecret: oidcTestClientSecret,
		RedirectURL:  "http://127.0.0.1:1/api/v1/auth/oidc/callback",
	}
	if claim != "" {
		cfg.Claim = claim
	}
	p, err := NewOIDCProvider(context.Background(), cfg, dbh)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	return p
}

// driveAuthorize 走 start → 假 IdP authorize(302),返回握手 cookie、回显
// state 与 code;断言 state 回显一致。
func driveAuthorize(t *testing.T, p *OIDCProvider) (cookie, state, code string) {
	t.Helper()
	start, err := p.StartAuth()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(start.RedirectURL)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // 诊断面
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status = %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("authorize location: %v", err)
	}
	orig, err := url.Parse(start.RedirectURL)
	if err != nil {
		t.Fatalf("start url: %v", err)
	}
	if loc.Query().Get("state") != orig.Query().Get("state") {
		t.Fatalf("state 回显不一致: %q vs %q", loc.Query().Get("state"), orig.Query().Get("state"))
	}
	return start.CookieValue, loc.Query().Get("state"), loc.Query().Get("code")
}

// TestOIDCStartAuthShape:授权请求与握手 cookie 的形状锚。PKCE 锚 =
// cookie 内 verifier 的 S256 恰为授权请求 challenge(两侧对账)。
func TestOIDCStartAuthShape(t *testing.T) {
	idp := oidctest.New(t)
	p := newOIDCProvider(t, idp, nil, "")
	start, err := p.StartAuth()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	u, err := url.Parse(start.RedirectURL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	if q.Get("client_id") != idp.ClientID {
		t.Fatalf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("response_type") != "code" {
		t.Fatalf("response_type = %q", q.Get("response_type"))
	}
	if !strings.Contains(q.Get("scope"), "openid") {
		t.Fatalf("scope = %q, want openid", q.Get("scope"))
	}
	if q.Get("redirect_uri") == "" {
		t.Fatalf("redirect_uri 缺失")
	}
	if q.Get("state") == "" || q.Get("nonce") == "" || q.Get("state") == q.Get("nonce") {
		t.Fatalf("state/nonce 形态: %q / %q", q.Get("state"), q.Get("nonce"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %q", q.Get("code_challenge_method"))
	}
	challenge := q.Get("code_challenge")
	if len(challenge) != 43 { // 32 字节熵 base64url 恰 43 字符
		t.Fatalf("challenge len = %d, want 43", len(challenge))
	}
	st, err := parseOIDCState(p.stateKey, start.CookieValue)
	if err != nil {
		t.Fatalf("cookie parse: %v", err)
	}
	if st.State != q.Get("state") || st.Nonce != q.Get("nonce") {
		t.Fatalf("cookie 握手值与授权请求不一致")
	}
	sum := sha256SumForTest([]byte(st.Verifier))
	if sum != challenge {
		t.Fatalf("challenge 与 verifier 不对应(pkce 断链)")
	}
}

// TestOIDCCompleteSuccess(t13):本地锚定成功——claim → 本地账号 → 角色
// 视图 + subject 回传。
func TestOIDCCompleteSuccess(t *testing.T) {
	d := newDB(t)
	createUser(t, d, "alice", "pw-unused-1", auth.RoleAuthor, auth.RoleApprover)
	idp := oidctest.New(t)
	idp.Claims = map[string]any{"preferred_username": "alice"}
	p := newOIDCProvider(t, idp, d, "")
	cookie, state, code := driveAuthorize(t, p)
	user, subject, err := p.Complete(context.Background(), cookie, state, code)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if subject != "alice" || user.Username != "alice" {
		t.Fatalf("subject/user = %q/%q, want alice", subject, user.Username)
	}
	if len(user.Roles) != 2 || user.Roles[0] != auth.RoleApprover || user.Roles[1] != auth.RoleAuthor {
		t.Fatalf("roles = %v, want 升序 [approver author]", user.Roles)
	}
	if user.Status != auth.StatusActive {
		t.Fatalf("status = %q", user.Status)
	}
}

func TestOIDCCompleteUnknownUser(t *testing.T) {
	d := newDB(t)
	idp := oidctest.New(t)
	idp.Claims = map[string]any{"preferred_username": "nobody-here"}
	p := newOIDCProvider(t, idp, d, "")
	cookie, state, code := driveAuthorize(t, p)
	_, subject, err := p.Complete(context.Background(), cookie, state, code)
	var creds *auth.CredentialsError
	if !errors.As(err, &creds) || creds.Reason != auth.ReasonUnknownUser {
		t.Fatalf("err = %v, want CredentialsError(unknown_user)", err)
	}
	if subject != "nobody-here" {
		t.Fatalf("subject = %q, want nobody-here", subject)
	}
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("err 链缺 ErrInvalidCredentials")
	}
}

func TestOIDCCompleteDisabled(t *testing.T) {
	d := newDB(t)
	createUser(t, d, "alice", "pw-unused-2", auth.RoleAuthor)
	if _, err := d.Exec(`UPDATE users SET status = 'DISABLED' WHERE username = 'alice'`); err != nil {
		t.Fatalf("disable: %v", err)
	}
	idp := oidctest.New(t)
	idp.Claims = map[string]any{"preferred_username": "alice"}
	p := newOIDCProvider(t, idp, d, "")
	cookie, state, code := driveAuthorize(t, p)
	_, subject, err := p.Complete(context.Background(), cookie, state, code)
	if !errors.Is(err, auth.ErrUserDisabled) {
		t.Fatalf("err = %v, want ErrUserDisabled", err)
	}
	if subject != "alice" {
		t.Fatalf("subject = %q, want alice(审计归因)", subject)
	}
}

// TestOIDCCompleteClaimMissing:默认 claim 缺失 → invalid_input(subject 空)。
func TestOIDCCompleteClaimMissing(t *testing.T) {
	d := newDB(t)
	createUser(t, d, "alice", "pw-unused-3", auth.RoleAuthor)
	idp := oidctest.New(t) // 不设 Claims:preferred_username 缺席
	p := newOIDCProvider(t, idp, d, "")
	cookie, state, code := driveAuthorize(t, p)
	_, subject, err := p.Complete(context.Background(), cookie, state, code)
	var creds *auth.CredentialsError
	if !errors.As(err, &creds) || creds.Reason != auth.ReasonInvalidInput {
		t.Fatalf("err = %v, want CredentialsError(invalid_input)", err)
	}
	if subject != "" {
		t.Fatalf("subject = %q, want 空", subject)
	}
}

// TestOIDCCompleteSubClaim(t13):claim=sub 两态——词域内直通、词域外
// (opaque)拒绝。
func TestOIDCCompleteSubClaim(t *testing.T) {
	d := newDB(t)
	createUser(t, d, "alice", "pw-unused-4", auth.RoleApprover)
	idp := oidctest.New(t)
	idp.Claims = map[string]any{"sub": "alice"}
	p := newOIDCProvider(t, idp, d, OIDCClaimSub)
	cookie, state, code := driveAuthorize(t, p)
	if _, subject, err := p.Complete(context.Background(), cookie, state, code); err != nil || subject != "alice" {
		t.Fatalf("词域内 sub: (%q, %v)", subject, err)
	}

	idp2 := oidctest.New(t) // 底座 sub = opaque(词域外)
	p2 := newOIDCProvider(t, idp2, d, OIDCClaimSub)
	cookie2, state2, code2 := driveAuthorize(t, p2)
	_, subject2, err := p2.Complete(context.Background(), cookie2, state2, code2)
	var creds *auth.CredentialsError
	if !errors.As(err, &creds) || creds.Reason != auth.ReasonInvalidInput {
		t.Fatalf("err = %v, want CredentialsError(invalid_input)", err)
	}
	if !strings.Contains(subject2, "|") {
		t.Fatalf("subject = %q, want opaque 原值", subject2)
	}
}

// TestOIDCCompleteEmailClaimRejected:email 词源在配置面即拒(本地锚定
// 模型下结构性死路——RFC 5322 形态永不过 username 词域,review MEDIUM
// 收口;词表拒绝优于可配置但恒 401 的运维陷阱)。锚在配置测试,此处仅
// 钉住 provider 构造面对该词面无第二入口。
func TestOIDCCompleteEmailClaimRejected(t *testing.T) {
	if slices.Contains(oidcClaimWords, "email") {
		t.Fatalf("email 不得出现在映射源词表(结构性死路,见词表注释)")
	}
}

// TestOIDCCompleteTransportRejections:传输/协议面(nil dbh = 零触库证明)。
func TestOIDCCompleteTransportRejections(t *testing.T) {
	idp := oidctest.New(t)
	idp.Claims = map[string]any{"preferred_username": "alice"}
	p := newOIDCProvider(t, idp, nil, "")
	cookie, state, code := driveAuthorize(t, p)

	t.Run("bad_state_cookie", func(t *testing.T) {
		if _, _, err := p.Complete(context.Background(), "garbage", state, code); !errors.Is(err, auth.ErrOIDCState) {
			t.Fatalf("err = %v, want ErrOIDCState", err)
		}
	})
	t.Run("state_echo_mismatch", func(t *testing.T) {
		// 注入面:cookie 有效但回显 state 不符 → 最早拒绝层(review 收口,
		// 独立于 PKCE/nonce 兜底)。
		if _, _, err := p.Complete(context.Background(), cookie, "tampered-echo-state", code); !errors.Is(err, auth.ErrOIDCState) {
			t.Fatalf("err = %v, want ErrOIDCState(state mismatch)", err)
		}
	})
	t.Run("bad_code", func(t *testing.T) {
		if _, _, err := p.Complete(context.Background(), cookie, state, "bogus-code"); !errors.Is(err, auth.ErrOIDCCode) {
			t.Fatalf("err = %v, want ErrOIDCCode", err)
		}
	})
	t.Run("forged_verifier_rejected_by_pkce", func(t *testing.T) {
		st, err := parseOIDCState(p.stateKey, cookie)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		st.Verifier = "wrong-verifier-000000000000000000000000000"
		forged, err := signOIDCState(p.stateKey, st)
		if err != nil {
			t.Fatalf("forge: %v", err)
		}
		// 签名有效但 verifier 与 authorize 时刻 challenge 不符:假 IdP 的
		// PKCE 门必须拒(兑付链断),归 ErrOIDCCode。
		if _, _, err := p.Complete(context.Background(), forged, state, code); !errors.Is(err, auth.ErrOIDCCode) {
			t.Fatalf("err = %v, want ErrOIDCCode(PKCE 拒)", err)
		}
	})
}

// TestOIDCCompleteIDTokenRejections:验签/nonce 面(假 IdP claim 覆盖构造,
// nil dbh = 拒收全部发生在触库之前)。
func TestOIDCCompleteIDTokenRejections(t *testing.T) {
	cases := []struct {
		name   string
		claims map[string]any
	}{
		{"nonce_mismatch", map[string]any{"preferred_username": "alice", "nonce": "tampered-nonce"}},
		{"bad_audience", map[string]any{"preferred_username": "alice", "aud": "other-app"}},
		{"expired_token", map[string]any{"preferred_username": "alice", "exp": time.Now().Add(-time.Hour).Unix()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idp := oidctest.New(t)
			idp.Claims = tc.claims
			p := newOIDCProvider(t, idp, nil, "")
			cookie, state, code := driveAuthorize(t, p)
			if _, _, err := p.Complete(context.Background(), cookie, state, code); !errors.Is(err, ErrOIDCIDToken) {
				t.Fatalf("err = %v, want ErrOIDCIDToken", err)
			}
		})
	}
}

// TestOIDCCompleteExpiredState:握手态过期(签名有效,时间窗关闭)。
func TestOIDCCompleteExpiredState(t *testing.T) {
	idp := oidctest.New(t)
	p := newOIDCProvider(t, idp, nil, "")
	start, err := p.StartAuth()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	st, err := parseOIDCState(p.stateKey, start.CookieValue)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	st.Exp = time.Now().Add(-time.Minute).Unix()
	expired, err := signOIDCState(p.stateKey, st)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, _, err := p.Complete(context.Background(), expired, "any-state", "any-code"); !errors.Is(err, auth.ErrOIDCState) {
		t.Fatalf("err = %v, want ErrOIDCState", err)
	}
}

// TestOIDCProviderDiscoveryFailFast:issuer 不可达/非 OIDC 端点 → 构造即
// 败(启动期 fail-fast,不到运行期)。
func TestOIDCProviderDiscoveryFailFast(t *testing.T) {
	srv := startNonOIDCServer(t)
	cfg := OIDCConfig{
		Issuer: srv.URL, ClientID: "c", ClientSecret: "s",
		RedirectURL: "http://127.0.0.1:1/cb",
	}
	if _, err := NewOIDCProvider(context.Background(), cfg, nil); err == nil {
		t.Fatalf("非 OIDC 端点构造成功,want 启动即败")
	}
}
