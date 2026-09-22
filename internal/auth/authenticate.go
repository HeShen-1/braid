package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

// dummy 哈希:未知用户路径的时序均衡。索引 miss(µs 级)与 bcrypt 比对
// (~100ms)的时差是用户名枚举 oracle;miss 时对固定哈希跑一次真实比对
// 再统一拒绝,两条失败路径的耗时形状一致。懒生成(首个 miss 才付一次
// ~60ms)。恒定 DefaultCost,与 DefaultBcryptCost 对齐——这是均衡有效性
// 的前提(cost 偏离默认的取舍见 config.BcryptCost 注释);不按目标账号
// cost 逐比特追逐,那反而把存量哈希成本分布泄漏进时序面。存在性锚 =
// TestAuthenticateUnknownUserTimingFloor(删除本函数即红)。
var (
	dummyOnce sync.Once
	dummyHash []byte
)

func dummyCompare(password string) {
	dummyOnce.Do(func() {
		h, err := bcrypt.GenerateFromPassword([]byte("braid-timing-equalizer-target"), bcrypt.DefaultCost)
		if err != nil {
			// 合法域内参数生成失败不可达;响亮失败优于静默失去时序均衡。
			panic(fmt.Sprintf("auth: dummy hash: %v", err))
		}
		dummyHash = h
	})
	_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password)) // 恒 mismatch,仅取时序
}

// Authenticate 校验用户名口令,成功返回读齐角色的用户视图。
//
// 失败面貌(防枚举):
//   - 域外输入(非法 username 形态、密码长度越界):不可能命中真实
//     凭证,前置快速拒绝不触 DB(>72 同时封 bcrypt 截断与「仅超长口令
//     可探测」的存在性信道)——Reason=invalid_input;
//   - 未知用户 / 错密码:统一 ErrInvalidCredentials,时序经 dummy 比对
//     均衡——Reason=unknown_user / wrong_password;
//   - DISABLED:只在密码验证通过后暴露(持凭证者的可诊断性;错密码
//     恒 ErrInvalidCredentials,防账号状态探测——顺序由
//     TestAuthenticateDisabledOrdering 钉死,调换即红)。
func Authenticate(ctx context.Context, q DBTX, username, password string) (User, error) {
	if !validUsername(username) ||
		len(password) < minPasswordLen || len(password) > maxPasswordLen {
		return User{}, &CredentialsError{Username: username, Reason: ReasonInvalidInput}
	}
	rec, err := findByUsername(ctx, q, username)
	if errors.Is(err, errUserNotFound) {
		dummyCompare(password)
		return User{}, &CredentialsError{Username: username, Reason: ReasonUnknownUser}
	}
	if err != nil {
		return User{}, err // 查询故障原样上抛,不吞不降级
	}
	if bcrypt.CompareHashAndPassword([]byte(rec.passwordHash), []byte(password)) != nil {
		return User{}, &CredentialsError{Username: username, Reason: ReasonWrongPassword}
	}
	if rec.Status == StatusDisabled {
		return User{}, fmt.Errorf("%w: %q", ErrUserDisabled, username)
	}
	return rec.User, nil
}
