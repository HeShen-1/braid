// Package db 提供 braid 数据库迁移(golang-migrate)与连接契约。
//
// 连接契约(全库统一,见 migrations/0001_init.up.sql 文件头):
// DSN 必须含 loc=UTC&parseTime=true&multiStatements=true&time_zone='+00:00'
// (multiStatements 是 golang-migrate mysql 驱动执行单文件多语句迁移的前提;
// time_zone 让 DB 端 NOW(3)/CURRENT_TIMESTAMP(3) 按会话 UTC 求值——DATETIME 存
// 字面值,会话时区即真相,不依赖 mysqld 级 default-time-zone,DDL 文件头契约的
// 另一半)。应用代码统一经 CanonicalDSN 补齐参数,不要手拼。
package db

import (
	"embed"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/mysql" // 注册 mysql:// 迁移驱动
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// canonicalParams 是连接契约参数表;time_zone 值为 URL 转义后的 '+00:00'
// (带引号——go-sql-driver 把未知参数当系统变量在连接初始化时 SET)。
var canonicalParams = []struct{ key, value string }{
	{"loc=", "UTC"},
	{"parseTime=", "true"},
	{"multiStatements=", "true"},
	{"time_zone=", "%27%2B00%3A00%27"},
}

// CanonicalDSN 在基础 DSN 上逐参数补齐全库连接契约(缺哪个补哪个,已带的不覆盖)。
// 基础 DSN 形如 root:pwd@tcp(127.0.0.1:3306)/braid。
func CanonicalDSN(base string) string {
	b := base
	for _, p := range canonicalParams {
		if hasParam(b, p.key) {
			continue
		}
		sep := "?"
		if strings.Contains(b, "?") {
			sep = "&"
		}
		b += sep + p.key + p.value
	}
	return b
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
