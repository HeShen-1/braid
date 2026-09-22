package auth

import (
	"errors"
	"fmt"
)

// 包级 sentinel:英文消息、包名前缀(仓内风格)。调用方只许 errors.Is /
// errors.As 分类,禁止字符串匹配。
var (
	// ErrInvalidCredentials 登录凭证不符(未知用户/错密码/域外输入的
	// 统一外部面貌)。HTTP 层映射 401。
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	// ErrUserDisabled 密码验证通过但账号被停用(仅持正确凭证者可达,
	// 可诊断性换取有限信息披露)。HTTP 层映射 403。
	ErrUserDisabled = errors.New("auth: user disabled")
	// ErrForbidden 已认证但角色不足(验收第三条「角色越权」的落点)。
	ErrForbidden = errors.New("auth: forbidden")
	// ErrTokenExpired 签名有效但已过期(唯一单列的 token 失败类)。
	ErrTokenExpired = errors.New("auth: token expired")
	// ErrTokenInvalid 其余一切 token 拒绝:篡改/错密钥/alg 混淆/iss 或
	// aud 不符/缺 exp/词表外角色/空 sub。HTTP 层映射 401。
	ErrTokenInvalid = errors.New("auth: token invalid")
	// ErrConflict 唯一键冲突(username 重复/角色重复授予,MySQL 1062)。
	ErrConflict = errors.New("auth: conflict")
	// ErrInvalidInput 前置校验失败(触 DB 前);库入口的配置类误用
	// (过短签名密钥)同走此口——Config.Validate 的启动期错误沿用
	// relay 形态的裸错误,不带 sentinel。调用方 bug,非用户输入错误。
	ErrInvalidInput = errors.New("auth: invalid input")
)

// CredentialsError 登录失败的结构化归因(engine.ConflictError 同款形态):
// Error/Unwrap 对外统一 ErrInvalidCredentials(防枚举),Reason 三态互异
// 供日志与运维诊断。Reason 是日志词汇,不得出现在对客户端的错误文本中。
type CredentialsError struct {
	Username string
	Reason   string
}

// 登录失败内部归因三态(词表封闭,仅日志面)。
const (
	// ReasonInvalidInput 域外输入(非法 username 形态、密码长度越界):
	// 不可能命中任何真实凭证,前置快速拒绝,不触 DB。
	ReasonInvalidInput = "invalid_input"
	// ReasonUnknownUser 用户名不存在(dummy 比对已做时序均衡)。
	ReasonUnknownUser = "unknown_user"
	// ReasonWrongPassword 用户存在但口令不符。
	ReasonWrongPassword = "wrong_password"
)

// Error 用 %q 渲染 Username:invalid_input 路径携带的是未过校验的
// 原始输入,可为任意字节(换行/控制符/ANSI 转义),%s 直嵌构成日志
// 伪造面(CWE-117);%q 转义后关闭(与包内其余插值一致)。
func (e *CredentialsError) Error() string {
	return fmt.Sprintf("%v: %q: %s", ErrInvalidCredentials, e.Username, e.Reason)
}

func (e *CredentialsError) Unwrap() error { return ErrInvalidCredentials }
