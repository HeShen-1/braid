package auth

import (
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// 密码策略:8..72。上界 72 = bcrypt 输入截断线(Go 实现静默截断第 73
// 字节起,「同一哈希对应多个密码」不可接受);下界 8 是认证包可接受的
// 最小口令面(拒绝 1 字节口令是安全评审必挂项)。域在 HashPassword 与
// Authenticate 双侧生效。
const (
	minPasswordLen = 8
	maxPasswordLen = 72
	bcryptHashLen  = 60
)

// HashPassword 计算 bcrypt 哈希。cost 是 bcrypt 库合法域 [MinCost,MaxCost]
// (4..31;生产建议 ≥10,见 Config.DefaultBcryptCost 与注释);测试用
// bcrypt.MinCost 换速度。调用纪律:在事务外调用(~100ms CPU 不进锁窗),
// 哈希结果经 CreateUserParams.PasswordHash 落库。
func HashPassword(password string, cost int) (string, error) {
	switch {
	case len(password) < minPasswordLen:
		return "", fmt.Errorf("%w: password shorter than %d", ErrInvalidInput, minPasswordLen)
	case len(password) > maxPasswordLen:
		return "", fmt.Errorf("%w: password longer than %d (bcrypt truncation line)", ErrInvalidInput, maxPasswordLen)
	case cost < bcrypt.MinCost || cost > bcrypt.MaxCost:
		return "", fmt.Errorf("%w: bcrypt cost %d out of [%d,%d]", ErrInvalidInput, cost, bcrypt.MinCost, bcrypt.MaxCost)
	}
	b, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	if err != nil {
		return "", fmt.Errorf("auth: hash password: %w", err)
	}
	return string(b), nil
}

// validHashShape 报告哈希是否为 bcrypt 形态(60 ASCII + $2a$/$2b$/$2y$
// 前缀)。DDL chk_user_password_hash 是同一门禁的库端半边(防直插降级),
// 此处是应用侧半边(防调用方 bug 触发 3819 后难归因)。
func validHashShape(h string) bool {
	if len(h) != bcryptHashLen {
		return false
	}
	for _, p := range []string{"$2a$", "$2b$", "$2y$"} {
		if h[:4] == p {
			return true
		}
	}
	return false
}
