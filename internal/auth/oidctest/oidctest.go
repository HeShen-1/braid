// Package oidctest 提供进程内假 IdP(T11.1 测试基建):discovery / JWKS /
// authorize / token 四端点,RS256 签发 ID token,PKCE S256 强制校验。auth
// 与 api 两包测试共用(internal/notify/notifytest 先例)。
//
// 覆盖层语义:Claims map 在每次签发时合并进 ID token 标准底座(iss/aud/
// sub/exp/iat/nonce),测试据此构造正负例(如 preferred_username、email、
// aud 覆盖、exp 过期)。底座 sub 是词域外 opaque 值,默认 claim 配置
// (preferred_username)下不配置 Claims 即负例。
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// pendingAuth 是 authorize 时刻钉下的握手事实,token 兑换时逐项复核
// (state 回显、nonce 回嵌、PKCE challenge、redirect_uri 一致)。
type pendingAuth struct {
	state, nonce, challenge, redirectURI string
}

// FakeIdP 是可配置的假 IdP。
type FakeIdP struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey
	kid string

	// ClientID 是 authorize 请求必须携带的 client_id。
	ClientID string

	mu      sync.Mutex
	pending map[string]*pendingAuth
	// Claims 是 ID token 签发时的覆盖层(并发保护下读;测试串行设置)。
	Claims map[string]any
}

// New 启动假 IdP 并注册 t.Cleanup。
func New(t *testing.T) *FakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("oidctest: rsa key: %v", err)
	}
	f := &FakeIdP{
		t: t, key: key, kid: "oidctest-key-1",
		ClientID: "braid-test-client",
		pending:  map[string]*pendingAuth{},
		Claims:   map[string]any{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", f.handleDiscovery)
	mux.HandleFunc("/jwks.json", f.handleJWKS)
	mux.HandleFunc("/authorize", f.handleAuthorize)
	mux.HandleFunc("/token", f.handleToken)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// Issuer 返回 issuer URL(httptest 回环地址,配置面 http-loopback 豁免内)。
func (f *FakeIdP) Issuer() string { return f.srv.URL }

func (f *FakeIdP) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	base := f.srv.URL
	writeJSON(f.t, w, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/authorize",
		"token_endpoint":                        base + "/token",
		"jwks_uri":                              base + "/jwks.json",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"grant_types_supported":                 []string{"authorization_code"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (f *FakeIdP) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       &f.key.PublicKey,
		KeyID:     f.kid,
		Algorithm: "RS256",
		Use:       "sig",
	}}}
	writeJSON(f.t, w, set)
}

func (f *FakeIdP) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != f.ClientID {
		http.Error(w, "unauthorized_client", http.StatusBadRequest)
		return
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "pkce S256 required", http.StatusBadRequest)
		return
	}
	code := randomCode()
	f.mu.Lock()
	f.pending[code] = &pendingAuth{
		state:       q.Get("state"),
		nonce:       q.Get("nonce"),
		challenge:   q.Get("code_challenge"),
		redirectURI: q.Get("redirect_uri"),
	}
	f.mu.Unlock()
	sep := "?"
	if u := q.Get("redirect_uri"); u != "" && u[len(u)-1] == '?' {
		sep = "&"
	}
	// 浏览器视角:302 回回调面,携 code + state 回显。
	http.Redirect(w, r, q.Get("redirect_uri")+sep+"code="+url.QueryEscape(code)+
		"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
}

func (f *FakeIdP) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		http.Error(w, "unsupported_grant_type", http.StatusBadRequest)
		return
	}
	code := r.PostForm.Get("code")
	f.mu.Lock()
	p, ok := f.pending[code]
	claims := make(map[string]any, len(f.Claims)+6)
	for k, v := range f.Claims {
		claims[k] = v
	}
	f.mu.Unlock()
	if !ok {
		http.Error(w, "invalid_grant: unknown code", http.StatusBadRequest)
		return
	}
	// client 认证面(x/oauth2 basic auth 或 body 参数)不在本测试矩阵:
	// 客户端凭证由 braid 侧持有,假 IdP 只钉握手事实。
	if ru := r.PostForm.Get("redirect_uri"); ru != p.redirectURI {
		http.Error(w, "invalid_grant: redirect_uri mismatch", http.StatusBadRequest)
		return
	}
	// PKCE S256 强制:verifier 哈希必须等于 authorize 时刻的 challenge。
	v := r.PostForm.Get("code_verifier")
	if v == "" {
		http.Error(w, "invalid_grant: code_verifier required", http.StatusBadRequest)
		return
	}
	sum := sha256.Sum256([]byte(v))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != p.challenge {
		http.Error(w, "invalid_grant: pkce verification failed", http.StatusBadRequest)
		return
	}
	now := time.Now()
	base := map[string]any{
		"iss": f.srv.URL,
		"aud": f.ClientID,
		// 词域外 opaque sub:默认 claim(preferred_username)下未配置
		// Claims 即负例;claim=sub 的正例须显式覆盖。
		"sub":   "oidc|opaque-1234567890",
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		"nonce": p.nonce,
	}
	for k, v := range claims { // 覆盖层优先(测试构造 aud/exp/nonce 负例的通道)
		base[k] = v
	}
	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: f.key},
		(&jose.SignerOptions{}).WithHeader("kid", f.kid).WithType("JWT"),
	)
	if err != nil {
		http.Error(w, "signer", http.StatusInternalServerError)
		return
	}
	raw, err := jwt.Signed(sig).Claims(base).Serialize()
	if err != nil {
		http.Error(w, "sign", http.StatusInternalServerError)
		return
	}
	writeJSON(f.t, w, map[string]any{
		"access_token": "at-" + code,
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     raw,
		"scope":        "openid profile email",
	})
}

// writeJSON 在 HTTP handler goroutine 内只可 Errorf(Fatalf 会 Goexit,
// 客户端悬死;review LOW);编码失败以 500 收口。
func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("oidctest: encode: %v", err)
		http.Error(w, "encode", http.StatusInternalServerError)
	}
}

func randomCode() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("oidctest: entropy: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
