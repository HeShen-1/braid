// Package db 提供 braid 数据库迁移(golang-migrate)与连接契约。
//
// 连接契约(全库统一,见 migrations/0001_init.up.sql 文件头):
// DSN 必须含 loc=UTC&parseTime=true&multiStatements=true(最后一个是 golang-migrate
// mysql 驱动执行单文件多语句迁移的前提)。应用代码统一经 CanonicalDSN 补齐参数,
// 不要手拼。
package db

import (
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/mysql" // 注册 mysql:// 迁移驱动
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// CanonicalDSN 在基础 DSN 上补齐全库连接契约参数(loc=UTC、parseTime、multiStatements)。
// 基础 DSN 形如 root:pwd@tcp(127.0.0.1:3306)/braid,已带的参数不覆盖。
func CanonicalDSN(base string) string {
	for _, p := range []string{"loc=", "parseTime=", "multiStatements="} {
		if hasParam(base, p) {
			return base
		}
	}
	return base + "?loc=UTC&parseTime=true&multiStatements=true"
}

func hasParam(dsn, param string) bool {
	for i := 0; i+len(param) <= len(dsn); i++ {
		if dsn[i:i+len(param)] == param && (i == 0 || dsn[i-1] == '&' || dsn[i-1] == '?') {
			return true
		}
	}
	return false
}

// Migrate 把数据库推进到已嵌入迁移的最新版本;已是最新时为 no-op(ErrNoChange 吸收)。
// dsn 会被 CanonicalDSN 补齐契约参数。dirty 状态(上次迁移中断)返回错误,
// 恢复路径见 ROADMAP T7.4 runbook(migrate force + 修复后重跑;全部对象 IF NOT EXISTS,
// 重跑安全)。
func Migrate(dsn string) error {
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("db: migration source: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, "mysql://"+CanonicalDSN(dsn))
	if err != nil {
		return fmt.Errorf("db: migrate init: %w", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("db: migrate up: %w", err)
	}
	return nil
}
