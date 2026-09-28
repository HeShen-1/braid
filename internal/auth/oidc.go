package auth

// OIDC redirect 流插件(T11.1,D-2026-09-28-t111-oidc-subject-anchor)。
// identity.go 的 IdentityProvider 是凭证核验型缝(口令 → User),OIDC 的
// 302 形状装不进该接口——本文件是其姊妹面:StartAuth 产握手态,Complete
// 兑换回调,错误族与 User→SignToken 契约原样复用(CredentialsError →
// 401 / ErrUserDisabled → 403 / 其余 500)。
//
// 主体映射 = 本地账号锚定:claim(preferred_username/email/sub)值精确
// 匹配 users.username 并过既有词域;角色/状态自本地库(IdP 不携带 braid
// 角色,本地行结构性必需),IdP 只做身份核验。0002 头注释预登记的主体列
// 加宽不回收(理由见 D- 条目)。
//
// 握手态(state/nonce/PKCE verifier)由签名短时 cookie 承载:api 进程
// 无状态、零 Redis;密钥自 ClientSecret HKDF 派生(域分隔),伪造/篡改/
// 过期在 parseOIDCState 拒收。防枚举口径:OIDC 路径无口令比对面,未知
// 本地账号的时序差(单次索引 miss)不构成 bcrypt 量级的存在性 oracle,
// 且探测者须先持 IdP 核验过的身份——不引入 dummy 时序均衡。

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// 主体映射源 claim 词表(封闭):sub = OIDC 标准唯一标识(仅 issuer 内
// 唯一, opaque);preferred_username = 惯用名(缺省,与本地账号名最可能
// 同形)。email 词源刻意缺席:RFC 5322 邮箱形态(含 '@'/'.')永不过
// username 词域,本地锚定模型下结构性死路——启动即拒优于可配置但恒 401
// 的运维陷阱(review MEDIUM);email 锚定随 P1 oidc_identity 映射表回归
// (彼时锚定键不再受本地词域钳制)。
const (
	OIDCClaimSub               = "sub"
	OIDCClaimPreferredUsername = "preferred_username"
	// DefaultOIDCClaim 是 Claim 缺省值。
	DefaultOIDCClaim = OIDCClaimPreferredUsername
)

var oidcClaimWords = []string{OIDCClaimSub, OIDCClaimPreferredUsername}

// 传输面 sentinel:错误族(凭据判定)之外的分类面,api 层映射 400(传输/
// 协议面)与 500(身份源故障);本地库故障原样上抛(500)。
var (
	// ErrOIDCState 回调携带的握手态 cookie 拒收(缺失/伪造/过期)。
	ErrOIDCState = errors.New("auth: oidc state rejected")
	// ErrOIDCCode authorization code 兑换失败(无效/过期/PKCE 不符)。
	ErrOIDCCode = errors.New("auth: oidc code exchange failed")
	// ErrOIDCIDToken ID token 拒收(缺 id_token/验签/nonce 不符):身份源
	// 故障,不是登录判定结果。
	ErrOIDCIDToken = errors.New("auth: oidc id token invalid")
)

// OIDCStateCookie / OIDCStateTTL:握手态 cookie 的名字与寿命(api 层装配
// 用);10 分钟覆盖人工完成 IdP 登录的常见时延。OIDCHTTPTimeout 界住出站
// IdP 往返(discovery 拉取与 token 兑换/验签期 JWKS 拉取——x/oauth2 默认
// client 无 deadline,慢/挂 IdP 会吊住启动或回调 handler,review MEDIUM)。
const (
	OIDCStateCookie = "braid_oidc_state"
	OIDCStateTTL    = 10 * time.Minute
	OIDCHTTPTimeout = 10 * time.Second
)

// OIDCConfig 是 OIDC 插件配置组(BRAID_OIDC_*)。半开语义:组整体缺省 =
// OIDC 关闭(路由不挂载);任一键设而组不全 = 启动即败(正向/反向半配置
// 双闸,notify 三闸先例)。Claim 词表封闭,缺省 preferred_username。
type OIDCConfig struct {
	Issuer       string // IdP issuer URL,discovery 基址
	ClientID     string
	ClientSecret string // 凭证,不进日志/审计(铁律)
	RedirectURL  string // 回调绝对 URL,须与 IdP 注册一致
	Claim        string // 主体映射源,词表三值
}

// Configured 报告配置组是否任一键被设置(Validate 的半开判据)。
func (c OIDCConfig) Configured() bool {
	return c.Issuer != "" || c.ClientID != "" || c.ClientSecret != "" || c.RedirectURL != "" || c.Claim != ""
}

// Validate 启动期断言:组全或无、claim 词表、URL 形态(https,仅回环豁免
// http——dev 栈与测试;非回环 http = 明文凭证面,启动即拒)。
func (c OIDCConfig) Validate() error {
	if !c.Configured() {
		return nil
	}
	var missing []string
	if c.Issuer == "" {
		missing = append(missing, "BRAID_OIDC_ISSUER")
	}
	if c.ClientID == "" {
		missing = append(missing, "BRAID_OIDC_CLIENT_ID")
	}
	if c.ClientSecret == "" {
		missing = append(missing, "BRAID_OIDC_CLIENT_SECRET")
	}
	if c.RedirectURL == "" {
		missing = append(missing, "BRAID_OIDC_REDIRECT_URL")
	}
	if len(missing) > 0 {
		return fmt.Errorf("auth: oidc half-configured: missing %s", strings.Join(missing, ", "))
	}
	if !slices.Contains(oidcClaimWords, c.Claim) {
		return fmt.Errorf("auth: BRAID_OIDC_CLAIM=%q: want one of %s", c.Claim, strings.Join(oidcClaimWords, "|"))
	}
	if err := validateHTTPSorLoopback(c.Issuer); err != nil {
		return fmt.Errorf("auth: BRAID_OIDC_ISSUER %q: %w", c.Issuer, err)
	}
	if err := validateHTTPSorLoopback(c.RedirectURL); err != nil {
		return fmt.Errorf("auth: BRAID_OIDC_REDIRECT_URL %q: %w", c.RedirectURL, err)
	}
	return nil
}

// ParseOIDCConfig 从 BRAID_OIDC_* 组构造(getenv 注入,ConfigFromEnv 惯例)。
// 全组缺省返回零值(OIDC 关闭);Claim 缺省在此补齐;半开经 Validate 启动即败。
func ParseOIDCConfig(getenv func(string) string) (OIDCConfig, error) {
	c := OIDCConfig{
		Issuer:       getenv("BRAID_OIDC_ISSUER"),
		ClientID:     getenv("BRAID_OIDC_CLIENT_ID"),
		ClientSecret: getenv("BRAID_OIDC_CLIENT_SECRET"),
		RedirectURL:  getenv("BRAID_OIDC_REDIRECT_URL"),
		Claim:        getenv("BRAID_OIDC_CLAIM"),
	}
	if !c.Configured() {
		return OIDCConfig{}, nil
	}
	if c.Claim == "" {
		c.Claim = DefaultOIDCClaim
	}
	return c, c.Validate()
}

// validateHTTPSorLoopback:绝对 http(s) URL 且带主机;http 仅回环豁免
// (127.0.0.0/8、::1、localhost)。
func validateHTTPSorLoopback(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("bad url: %w", err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("want absolute http(s) url with host")
	}
	if u.Scheme == "http" && !loopbackHost(u.Hostname()) {
		return fmt.Errorf("http only allowed for loopback hosts")
	}
	return nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// oidcAuthState 是回调往返间的握手态:state(授权请求携出、回调回显比对
// ——注入/CSRF 的最早拒绝层,见 Complete)、nonce(进 IdP 授权请求并回嵌
// ID token,重放/注入比对)、verifier(PKCE S256)。序列化为 JSON +
// HMAC-SHA256 签名的单值 cookie。
type oidcAuthState struct {
	State    string `json:"st"`
	Nonce    string `json:"nc"`
	Verifier string `json:"vf"`
	Exp      int64  `json:"xp"` // unix 秒
}

// stateKeyInfo 是 HKDF 域分隔标签:派生密钥与 ClientSecret 的其他潜在
// 用途(如 IdP 侧 basic auth)在密钥空间上互不可达。
const stateKeyInfo = "braid.oidc.state.v1"

func deriveStateKey(clientSecret string) ([]byte, error) {
	return hkdf.Key(sha256.New, []byte(clientSecret), nil, stateKeyInfo, 32)
}

func signOIDCState(key []byte, st oidcAuthState) (string, error) {
	payload, err := json.Marshal(st)
	if err != nil {
		return "", fmt.Errorf("auth: oidc state marshal: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// parseOIDCState 验签 + 解码 + 过期复核;任一环节失败统一归 ErrOIDCState
// 面(细节差异只在错误文本,供日志)。
func parseOIDCState(key []byte, v string) (oidcAuthState, error) {
	dot := strings.IndexByte(v, '.')
	if dot < 0 {
		return oidcAuthState{}, errors.New("malformed state cookie")
	}
	payload, err := base64.RawURLEncoding.DecodeString(v[:dot])
	if err != nil {
		return oidcAuthState{}, fmt.Errorf("state payload: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(v[dot+1:])
	if err != nil {
		return oidcAuthState{}, fmt.Errorf("state signature: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return oidcAuthState{}, errors.New("state signature mismatch")
	}
	var st oidcAuthState
	if err := json.Unmarshal(payload, &st); err != nil {
		return oidcAuthState{}, fmt.Errorf("state payload: %w", err)
	}
	if time.Now().Unix() > st.Exp {
		return oidcAuthState{}, errors.New("state expired")
	}
	if st.State == "" || st.Nonce == "" || st.Verifier == "" {
		return oidcAuthState{}, errors.New("state incomplete")
	}
	return st, nil
}

// randomToken 产 n 字节熵的 base64url(RFC 7636 verifier 合法域:43 字符
// = 32 字节熵;state/nonce 同式)。
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: oidc entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// OIDCProvider 是 OIDC redirect 流插件实现:discovery/JWKS/ID token 验签
// 委托 go-oidc(事实标准库),本地账号锚定复用 findByUsername。
type OIDCProvider struct {
	oauth2   oauth2.Config
	verifier *oidc.IDTokenVerifier
	dbh      DBTX
	claim    string
	stateKey []byte
}

// NewOIDCProvider 构造(含 discovery 拉取:issuer 错/不可达在此启动即败,
// T7.3 fail-fast 哲学;往返界 OIDCHTTPTimeout)。dbh 与 BuiltinProvider
// 同式注入(本地锚定读面)。注意:构造后的 JWKS 拉取走 Verify 期 ctx,
// 不被此处的超时窗覆盖(Complete 侧自理)。
func NewOIDCProvider(ctx context.Context, cfg OIDCConfig, dbh DBTX) (*OIDCProvider, error) {
	dctx, cancel := context.WithTimeout(ctx, OIDCHTTPTimeout)
	defer cancel()
	prov, err := oidc.NewProvider(dctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("auth: oidc discovery: %w", err)
	}
	key, err := deriveStateKey(cfg.ClientSecret)
	if err != nil {
		return nil, fmt.Errorf("auth: oidc state key: %w", err)
	}
	return &OIDCProvider{
		oauth2: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     prov.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			// openid 必携;profile 供 preferred_username。email scope 预留
			// P1 映射表(email 词源本任务缺席,见词表注释),索取无害。
			Scopes: []string{oidc.ScopeOpenID, "profile", "email"},
		},
		verifier: prov.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		dbh:      dbh,
		claim:    cfg.Claim,
		stateKey: key,
	}, nil
}

// OIDCStart 是 /auth/oidc/login 的产出:重定向目标与待下发的签名 cookie。
type OIDCStart struct {
	RedirectURL string
	CookieValue string
}

// StartAuth 产握手态与重定向目标。PKCE S256(RFC 7636):challenge 进授权
// 请求,verifier 留在签名 cookie(仅本服务可达),authorization code 被
// 截获也无法兑付;nonce 进授权请求,由 IdP 回嵌 ID token。
func (p *OIDCProvider) StartAuth() (OIDCStart, error) {
	state, err := randomToken(32)
	if err != nil {
		return OIDCStart{}, err
	}
	nonce, err := randomToken(32)
	if err != nil {
		return OIDCStart{}, err
	}
	verifier, err := randomToken(32)
	if err != nil {
		return OIDCStart{}, err
	}
	challenge := sha256.Sum256([]byte(verifier))
	redirect := p.oauth2.AuthCodeURL(state,
		oauth2.SetAuthURLParam("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:])),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		oauth2.SetAuthURLParam("nonce", nonce),
	)
	cookie, err := signOIDCState(p.stateKey, oidcAuthState{
		State: state, Nonce: nonce, Verifier: verifier,
		Exp: time.Now().Add(OIDCStateTTL).Unix(),
	})
	if err != nil {
		return OIDCStart{}, err
	}
	return OIDCStart{RedirectURL: redirect, CookieValue: cookie}, nil
}

// oidcIDClaims 是本插件消费的 claim 子集(标准词面,不含自由文本透传)。
type oidcIDClaims struct {
	PreferredUsername string `json:"preferred_username"`
}

// Complete 兑现回调:验 state cookie → 回显 state 比对 → code 换 token
// (携 PKCE verifier)→ 验 ID token(签名/iss/aud/exp,go-oidc)+ nonce
// 比对 → claim 抽取 → 本地账号锚定。返回的 subject 是解析出的 claim 值:
// 身份判定失败时仍非空,供 api 层审计归因(口令登录的 req.Username 对应
// 物);传输/协议面失败为空。
//
// 错误族(T5.3 契约复用):
//   - ErrOIDCState / ErrOIDCCode → 传输面(400);
//   - ErrOIDCIDToken → 身份源故障(500);
//   - *CredentialsError(claim 缺失/域外/本地无账号)→ 401;
//   - ErrUserDisabled → 403;
//   - 其余(本地库故障)原样上抛(500)。
func (p *OIDCProvider) Complete(ctx context.Context, cookieValue, state, code string) (User, string, error) {
	st, err := parseOIDCState(p.stateKey, cookieValue)
	if err != nil {
		return User{}, "", fmt.Errorf("%w: %v", ErrOIDCState, err)
	}
	// 回显 state 比对(OIDC Core 3.1.2.1 / OAuth 2.1 BCP):注入 code/state
	// 的最早拒绝层,独立于 PKCE 与 nonce 兜底(review 收口:文档声称的
	// 防御必须是真实存在的比较)。恒时比较。
	if !hmac.Equal([]byte(state), []byte(st.State)) {
		return User{}, "", fmt.Errorf("%w: state mismatch", ErrOIDCState)
	}
	// 出站往返(token 兑换 + Verify 期 JWKS 拉取)统一预算:超时归
	// ErrOIDCCode/ErrOIDCIDToken 既有错误族,不新增分类面。
	ctx, cancel := context.WithTimeout(ctx, OIDCHTTPTimeout)
	defer cancel()
	tok, err := p.oauth2.Exchange(ctx, code, oauth2.SetAuthURLParam("code_verifier", st.Verifier))
	if err != nil {
		return User{}, "", fmt.Errorf("%w: %v", ErrOIDCCode, err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return User{}, "", fmt.Errorf("%w: token response lacks id_token", ErrOIDCIDToken)
	}
	idt, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return User{}, "", fmt.Errorf("%w: %v", ErrOIDCIDToken, err)
	}
	if idt.Nonce == "" || !hmac.Equal([]byte(idt.Nonce), []byte(st.Nonce)) {
		return User{}, "", fmt.Errorf("%w: nonce mismatch", ErrOIDCIDToken)
	}
	var cl oidcIDClaims
	if err := idt.Claims(&cl); err != nil {
		return User{}, "", fmt.Errorf("%w: claims: %v", ErrOIDCIDToken, err)
	}
	subject := p.subjectOf(idt, &cl)
	if !validUsername(subject) {
		// 含 claim 缺失、域外值、email 未验证:词表封闭,统一 invalid_input。
		return User{}, subject, &CredentialsError{Username: subject, Reason: ReasonInvalidInput}
	}
	rec, err := findByUsername(ctx, p.dbh, subject)
	if errors.Is(err, errUserNotFound) {
		return User{}, subject, &CredentialsError{Username: subject, Reason: ReasonUnknownUser}
	}
	if err != nil {
		return User{}, subject, err // 查询故障原样上抛,不吞不降级
	}
	if rec.Status == StatusDisabled {
		return User{}, subject, fmt.Errorf("%w: %q", ErrUserDisabled, subject)
	}
	return rec.User, subject, nil
}

// subjectOf 按 claim 配置取主体字符串(词表两值,见词表注释)。
func (p *OIDCProvider) subjectOf(idt *oidc.IDToken, cl *oidcIDClaims) string {
	if p.claim == OIDCClaimSub {
		return idt.Subject
	}
	return cl.PreferredUsername
}
