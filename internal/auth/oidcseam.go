package auth

import (
	"errors"
	"time"
)

// OIDC 插件缝契约(T11.1 立面,T16.1 收线,D-2026-10-01-t161-engine-
// enterprise-split):协议实现(OIDCProvider/配置组/握手态签名)已迁
// internal/enterprise/oidc(企业增值面);本文件持有核心侧消费面契约——
// api 包的回调 handler(登录面 glue,留核心)只依赖下列符号,企业实现
// 经 api.Server.OIDCProvider setter 注入(cmd 组合根装配),未注入 =
// 路由不挂载(OSS 构建剔除企业树后登录面完好)。

// 传输面 sentinel:错误族(凭据判定,CredentialsError/ErrUserDisabled)之外
// 的分类面,api 层映射 400(传输/协议面)与 500(身份源故障);本地库故障
// 原样上抛(500)。ErrOIDCIDToken(身份源故障)留在企业实现侧——api 面
// 落 default 500 分支,不按名引用。
var (
	// ErrOIDCState 回调携带的握手态 cookie 拒收(缺失/伪造/过期)。
	ErrOIDCState = errors.New("auth: oidc state rejected")
	// ErrOIDCCode authorization code 兑换失败(无效/过期/PKCE 不符)。
	ErrOIDCCode = errors.New("auth: oidc code exchange failed")
)

// OIDCStateCookie / OIDCStateTTL:握手态 cookie 的名字与寿命(api 回调
// handler 装配用);10 分钟覆盖人工完成 IdP 登录的常见时延。
const (
	OIDCStateCookie = "braid_oidc_state"
	OIDCStateTTL    = 10 * time.Minute
)

// OIDCStart 是 /auth/oidc/login 的产出:重定向目标与待下发的签名 cookie。
type OIDCStart struct {
	RedirectURL string
	CookieValue string
}
