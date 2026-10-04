package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"braid/internal/auth"
	"braid/internal/db"
	"braid/internal/snowflake"
)

// runUserCreate 是 `braid user create` 的 argv 层:flag 解析 + 必填校验,
// 用法错误退出 2;库/网络路径在 userCreate(DB 直连,D-2026-10-03-t162)。
func runUserCreate(ctx context.Context, args []string, env func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("braid user create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	username := fs.String("username", "", "username, ^[a-z][a-z0-9_-]{0,127}$ (required)")
	roles := fs.String("roles", "", "comma-separated roles from author,approver,admin (required)")
	email := fs.String("email", "", "profile email, optional")
	tenant := fs.Uint64("tenant", 0, "tenant id, 0 = platform tenant")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *username == "" || *roles == "" {
		sayln(stderr, "%s", "braid user create: -username and -roles are required")
		return ExitUsage
	}
	if fs.NArg() != 0 { // 与 publish 的位置参闸同则:多余位置参即用法错,不静默吞
		say(stderr, "braid user create: unexpected positional arguments %v\n", fs.Args())
		return ExitUsage
	}
	return userCreate(ctx, env, userCreateParams{
		username: *username,
		rolesRaw: *roles,
		email:    *email,
		tenant:   *tenant,
	}, stdout, stderr)
}

// userCreateParams 是 user create 的已解析入参(argv 层校验后)。
type userCreateParams struct {
	username string
	rolesRaw string
	email    string
	tenant   uint64
}

// parseRoles 拆逗号词表 → []auth.Role(词表真源在 auth:词域/重复授予由
// CreateUser 前置校验裁决,此处只解析不重复词表)。
func parseRoles(raw string) ([]auth.Role, error) {
	parts := strings.Split(raw, ",")
	roles := make([]auth.Role, 0, len(parts))
	for _, p := range parts {
		r := strings.TrimSpace(p)
		if r == "" {
			return nil, fmt.Errorf("empty role in list %q", raw)
		}
		roles = append(roles, auth.Role(r))
	}
	return roles, nil
}

// userCreate 建号体(D-2026-10-03-t162-cli-connection):env/argv 校验后
// 全部委托 createUserCore;命令层只做错误归因(冲突 = 退出 1 + 既有措辞)
// 与成功叙事。
func userCreate(ctx context.Context, env func(string) string, p userCreateParams, stdout, stderr io.Writer) int {
	dsn := env("BRAID_MYSQL_DSN")
	if dsn == "" {
		sayln(stderr, "%s", "braid user create: BRAID_MYSQL_DSN is required (same env contract as cmd/api)")
		return ExitError
	}
	password := env("BRAID_CLI_PASSWORD")
	if password == "" {
		sayln(stderr, "%s", "braid user create: BRAID_CLI_PASSWORD is required (never a flag; e.g. read -rsp 'password: ' pw; export BRAID_CLI_PASSWORD=$pw)")
		return ExitError
	}
	roles, err := parseRoles(p.rolesRaw)
	if err != nil {
		say(stderr, "braid user create: %v\n", err)
		return ExitError
	}
	var email *string
	if p.email != "" {
		email = &p.email
	}
	if err := createUserCore(ctx, dsn, p.username, password, roles, email, p.tenant, stderr); err != nil {
		if errors.Is(err, auth.ErrConflict) {
			say(stderr, "braid user create: username already exists (or role re-granted): %v\n", err)
		} else {
			say(stderr, "braid user create: %v\n", err)
		}
		return ExitError
	}
	say(stdout, "created user %q roles=%v tenant=%d\n", p.username, roles, p.tenant)
	return ExitOK
}

// createUserCore 建号核心(T16.3 抽出供 seed 复用):DB 直连复用 auth
// 原语同则(HashPassword/WithTx/CreateUser),雪花号走短命租约(KindAPI
// + auto 池 + 零能力宣告,Release 删行)。返回裸错误——ErrConflict 由
// 调用方归因:CLI 命令层 = 退出 1,seed 层 = 已存在跳过(幂等)。哈希在
// 事务外且在租约前算(~100ms CPU 不进锁窗,同时收窄租约持有窗);stderr
// 只承接租约释放告警(失败路径原样上抛)。
func createUserCore(ctx context.Context, dsn, username, password string, roles []auth.Role, email *string, tenant uint64, stderr io.Writer) error {
	hash, err := auth.HashPassword(password, auth.DefaultBcryptCost)
	if err != nil {
		return err
	}
	dbh, err := sql.Open("mysql", db.CanonicalDSN(dsn))
	if err != nil {
		return fmt.Errorf("open mysql: %w", err)
	}
	defer dbh.Close() //nolint:errcheck // 一次性进程退出路径
	if err := dbh.PingContext(ctx); err != nil {
		return fmt.Errorf("mysql ping: %w", err)
	}
	// 短命租约(D-t162 决策 4):auto 池仲裁防生成器撞号;零能力宣告
	// = 本进程不参与 mode=all 混跑(建号窗内 fleet 闸瞬时 503 属既定
	// 边界,fail-closed)。
	lease, err := snowflake.StartLease(ctx, dbh, snowflake.LeaseConfig{
		Kind:   snowflake.KindAPI,
		Spec:   snowflake.Auto(),
		Holder: snowflake.DefaultHolder(),
	})
	if err != nil {
		return fmt.Errorf("snowflake lease: %w", err)
	}
	ids, err := snowflake.New(lease.Node())
	if err != nil {
		releaseLease(lease, stderr)
		return fmt.Errorf("snowflake: %w", err)
	}
	lease.Guard(ids)
	err = auth.WithTx(ctx, dbh, func(tx auth.DBTX) error {
		return auth.CreateUser(ctx, tx, ids, auth.CreateUserParams{
			Username:     username,
			PasswordHash: hash,
			Email:        email,
			TenantID:     tenant,
			Roles:        roles,
		})
	})
	releaseLease(lease, stderr)
	return err
}

// releaseLease 建号收尾删租约行(独立 Background ctx:根 ctx 在失败路径
// 可能已取消;有界窗,失败仅告警——行过期即被 auto 池回收,无泄漏面)。
func releaseLease(lease *snowflake.Lease, stderr io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lease.Release(ctx); err != nil {
		say(stderr, "braid user create: snowflake lease release: %v\n", err)
	}
}
