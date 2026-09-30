package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

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

// emailRe 与 DDL chk_user_email(0009)是用户资料邮箱域的双闸(同则逐字,
// POSIX [:space:] 形态使两字面字节一致,免 MySQL 字符串反斜杠歧义;
// usernameRe/chk_user_username 先例)。形态与 notify 收件址约束同语义
// (恰一 @、两侧非空、无空白);nil = 未配置(可空列)。
var emailRe = regexp.MustCompile(`^[^@[:space:]]+@[^@[:space:]]+$`)

func validEmail(s string) bool { return emailRe.MatchString(s) }

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
// (哈希只进 Authenticate 内部比对,不出包)。Email 是资料邮箱(T12.2
// 扇出投递解析面;nil = 未配置)。TenantID 是归属租户(D-t112 #2:
// 用户单成员;0 = 平台租户存量哨兵——直通 token tnt claim 的签发值)。
type User struct {
	ID       uint64
	Username string
	Email    *string
	TenantID uint64
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
	Email        *string // 可选资料邮箱(T12.2;nil = 未配置)
	TenantID     uint64  // 归属租户(D-t112;0 = 平台租户;存在性由 fk_users_tenant 兜底——1452 响亮失败)
	Roles        []Role
}

func (p CreateUserParams) validate() error {
	if !validUsername(p.Username) {
		return fmt.Errorf("%w: username %q", ErrInvalidInput, p.Username)
	}
	if !validHashShape(p.PasswordHash) {
		return fmt.Errorf("%w: password hash not bcrypt-shaped", ErrInvalidInput)
	}
	if p.Email != nil && !validEmail(*p.Email) {
		return fmt.Errorf("%w: email %q not address-shaped", ErrInvalidInput, *p.Email)
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
		`INSERT INTO users (id, tenant_id, username, password_hash, email) VALUES (?, ?, ?, ?, ?)`,
		uid, p.TenantID, p.Username, p.PasswordHash, p.Email,
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

// EmailsByUsernames 批量解析用户名 → 资料邮箱(T12.2 扇出投递的解析
// 原语,消费方 = notify sender:快照 approvers 名单 → 逐人收件)。tenant
// 是归属判据(D-t112):username 全局唯一下跨租户 DSL 命名他租户主体
// 结构性可达(发布期 approver 租户校验未立),谓词使名单中他租户名字
// 在此缺席——收件面与执行归属同租户,跨租户邮件推送闭合(名字仍保留
// 正文,ops 副本兜底)。只回已配置邮箱的映射:无账号 / email NULL /
// 他租户的名字缺席。空名单返回空映射(不触 DB);用户名经 usernameRe
// 预检——名单来自 DSL 快照(发布期已过同则词域闸),坏形态在此响亮
// 报错而非静默漏配。utf8mb4_bin 精确等值(列继承表默认 ai_ci,大小写
// 折叠会把互异主体并进同一行——D-t63 actor 谓词同判)。
func EmailsByUsernames(ctx context.Context, q DBTX, tenant uint64, usernames []string) (map[string]string, error) {
	if len(usernames) == 0 {
		return map[string]string{}, nil
	}
	for _, name := range usernames {
		if !validUsername(name) {
			return nil, fmt.Errorf("auth: username %q not in domain", name)
		}
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(usernames)), ",")
	args := make([]any, len(usernames))
	for i, name := range usernames {
		args[i] = name
	}
	args = append([]any{tenant}, args...)
	rows, err := q.QueryContext(ctx,
		`SELECT username, email FROM users
		  WHERE email IS NOT NULL AND tenant_id = ? AND username COLLATE utf8mb4_bin IN (`+placeholders+`)`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("auth: resolve emails: %w", err)
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // 只读路径,关闭错误无处置义
	out := make(map[string]string, len(usernames))
	for rows.Next() {
		var name, email string
		if err := rows.Scan(&name, &email); err != nil {
			return nil, fmt.Errorf("auth: scan email row: %w", err)
		}
		out[name] = email
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auth: iterate email rows: %w", err)
	}
	return out, nil
}

// findByUsername 两段 QueryRow 读齐登录所需:user 行点查(uk_user_username
// 精确定位,utf8mb4_bin 无折叠)+ roles 经 JSON_ARRAYAGG 单行聚合
// (聚合无 GROUP BY 恒单行,空集得 []——DBTX 维持最小两方法面)。
// 角色在 Go 侧排序:JSON_ARRAYAGG 不支持聚合内 ORDER BY,claims 的
// 确定性由排序保证。
func findByUsername(ctx context.Context, q DBTX, username string) (userRecord, error) {
	var rec userRecord
	err := q.QueryRowContext(ctx,
		`SELECT id, tenant_id, username, password_hash, status, email FROM users WHERE username = ?`,
		username,
	).Scan(&rec.ID, &rec.TenantID, &rec.Username, &rec.passwordHash, &rec.Status, &rec.Email)
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
