package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/go-sql-driver/mysql"
)

// usernameRe 与 DDL chk_user_username 是同一域约束的双闸(应用侧 +
// 库端 3819);正则逐字一致,漂移由 TestUsernameDomainPinned 变异锚
// 暴露。username 即主体字符串:直通 created_by/triggered_by/actor_id
// 的 VARCHAR(128) 值域,一经创建不可改名。
var usernameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,127}$`)

// reservedUsernames 与审计哨兵冲突的主体名:workflow_execution.
// triggered_by 以 'system' 标记系统触发(0001 设计),人工账号同名会在
// audit 归因/对账面制造「系统行为 vs 人」的混淆——域层拒绝(双闸:
// 此处 + chk_user_username 的 username <> 'system')。
var reservedUsernames = map[string]bool{"system": true}

func validUsername(s string) bool {
	return !reservedUsernames[s] && usernameRe.MatchString(s)
}

// errUserNotFound 包内信号:username 点查无行。不导出(T5.1 无按名查
// 用户的导出消费者;T5.2 中间件只信 claims 不回库),Authenticate 把
// 它翻译成防枚举的 CredentialsError。
var errUserNotFound = errors.New("auth: user not found")

// Status 账号状态(users.status ENUM;词表仅末尾追加)。
type Status string

const (
	StatusActive   Status = "ACTIVE"
	StatusDisabled Status = "DISABLED"
)

// User 是登录路径读齐的用户视图:身份 + 授权快照 + 状态。不含密码哈希
// (哈希只进 Authenticate 内部比对,不出包)。
type User struct {
	ID       uint64
	Username string
	Roles    []Role // 升序排序(确定性 claims);空仅见于无授权行的旁路数据
	Status   Status
}

// userRecord 是存储内部读出形态(含哈希,仅包内流转)。
type userRecord struct {
	User
	passwordHash string
}

// CreateUserParams 创建用户入参。PasswordHash 必须是 HashPassword 的
// 产物(形态校验双闸:此处 + DDL chk_user_password_hash)——参数携带
// 哈希而非明文,强制「哈希在事务外计算」的调用纪律(~100ms CPU 不进
// 锁窗)。
type CreateUserParams struct {
	Username     string
	PasswordHash string
	Roles        []Role
}

func (p CreateUserParams) validate() error {
	if !validUsername(p.Username) {
		return fmt.Errorf("%w: username %q", ErrInvalidInput, p.Username)
	}
	if !validHashShape(p.PasswordHash) {
		return fmt.Errorf("%w: password hash not bcrypt-shaped", ErrInvalidInput)
	}
	return validRoles(p.Roles)
}

// CreateUser 写入 user 行 + 全部角色行(原语不自管事务:调用方经
// WithTx 包裹取得原子性——角色行写败回滚,防「能登录但永无权限」的
// 孤儿状态)。username 重复/角色重复授予 → 1062 → ErrConflict。
func CreateUser(ctx context.Context, q DBTX, ids IDGen, p CreateUserParams) error {
	if err := p.validate(); err != nil {
		return err // 调用方 bug 前置拒绝,不触 DB
	}
	uid, err := ids.Next()
	if err != nil {
		return fmt.Errorf("auth: user id: %w", err)
	}
	if _, err := q.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash) VALUES (?, ?, ?)`,
		uid, p.Username, p.PasswordHash,
	); err != nil {
		return mapDup(fmt.Errorf("auth: insert user %q: %w", p.Username, err))
	}
	for _, r := range p.Roles {
		rid, err := ids.Next()
		if err != nil {
			return fmt.Errorf("auth: user_role id: %w", err)
		}
		if _, err := q.ExecContext(ctx,
			`INSERT INTO user_role (id, user_id, role) VALUES (?, ?, ?)`,
			rid, uid, r,
		); err != nil {
			return mapDup(fmt.Errorf("auth: insert role %q for %q: %w", r, p.Username, err))
		}
	}
	return nil
}

// mapDup 把 MySQL 1062(唯一键冲突)归一为 ErrConflict(engine/attempt.go
// 同款);其余错误原样上抛。
func mapDup(err error) error {
	var me *mysql.MySQLError
	if errors.As(err, &me) && me.Number == 1062 {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return err
}

// findByUsername 两段 QueryRow 读齐登录所需:user 行点查(uk_user_username
// 精确定位,utf8mb4_bin 无折叠)+ roles 经 JSON_ARRAYAGG 单行聚合
// (聚合无 GROUP BY 恒单行,空集得 []——DBTX 维持最小两方法面)。
// 角色在 Go 侧排序:JSON_ARRAYAGG 不支持聚合内 ORDER BY,claims 的
// 确定性由排序保证。
func findByUsername(ctx context.Context, q DBTX, username string) (userRecord, error) {
	var rec userRecord
	err := q.QueryRowContext(ctx,
		`SELECT id, username, password_hash, status FROM users WHERE username = ?`,
		username,
	).Scan(&rec.ID, &rec.Username, &rec.passwordHash, &rec.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, fmt.Errorf("%w: %q", errUserNotFound, username)
	}
	if err != nil {
		return rec, fmt.Errorf("auth: find user %q: %w", username, err)
	}
	var rolesJSON []byte
	if err := q.QueryRowContext(ctx,
		`SELECT COALESCE(JSON_ARRAYAGG(role), JSON_ARRAY()) FROM user_role WHERE user_id = ?`,
		rec.ID,
	).Scan(&rolesJSON); err != nil {
		return rec, fmt.Errorf("auth: roles of %q: %w", username, err)
	}
	if err := json.Unmarshal(rolesJSON, &rec.Roles); err != nil {
		return rec, fmt.Errorf("auth: roles of %q: %w", username, err)
	}
	slices.Sort(rec.Roles)
	return rec, nil
}
