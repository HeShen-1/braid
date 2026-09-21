// Package sqltest 提供 braid 引擎层测试基建:指向 deploy/migrate-test.docker-compose.yml
// 起的验证容器(docker compose -f deploy/migrate-test.docker-compose.yml -p braid-t13 up -d),
// 容器不可达时调用方自动 skip。凭证是该一次性本地容器的占位值,非任何真实系统凭证。
//
// 与 internal/db 自身测试的关键差异:引擎测试用**独立 schema**(braid_engine_test),
// 避免 go test ./... 并发跑包时被 db 包 TestMigrateUpDownIdempotent 的 down/up
// 循环拆表(两个测试二进制并行,同 schema 会互相踩)。
package sqltest

import (
	"database/sql"
	"fmt"
	"sync"

	_ "github.com/go-sql-driver/mysql"

	"braid/internal/db"
)

const (
	serverDSN  = "root:braid_dev_root@tcp(127.0.0.1:13306)/"
	testSchema = "braid_engine_test"
)

var (
	initOnce sync.Once
	initErr  error
)

// MustOpen 返回指向测试 schema 的连接池(懒建库+迁移,进程内一次)。
// 栈不可达 → t.Skipf(与 internal/db mustSQL 同模式);迁移失败视为环境损坏,fatal。
func MustOpen(t TB) *sql.DB {
	t.Helper()
	initOnce.Do(func() { initErr = initSchema() })
	if initErr != nil {
		t.Skipf("braid-t13-mysql 不可达,先起验证容器: %v", initErr)
	}
	d, err := sql.Open("mysql", db.CanonicalDSN(serverDSN+testSchema))
	if err != nil {
		t.Skipf("mysql driver: %v", err)
	}
	if err := d.Ping(); err != nil {
		t.Skipf("braid-t13-mysql 不可达,先起验证容器: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func initSchema() error {
	admin, err := sql.Open("mysql", db.CanonicalDSN(serverDSN))
	if err != nil {
		return err
	}
	defer func() { _ = admin.Close() }()
	if err := admin.Ping(); err != nil {
		return err
	}
	if _, err := admin.Exec(
		`CREATE DATABASE IF NOT EXISTS ` + testSchema +
			` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci`); err != nil {
		return fmt.Errorf("create %s: %w", testSchema, err)
	}
	// golang-migrate 的 schema_migrations 落在该 schema 内;幂等,残留即复用。
	return db.Migrate(serverDSN + testSchema)
}

// TB 收窄 testing.TB 到测试所需面(避免测试文件外泄 testing 依赖)。
type TB interface {
	Helper()
	Skipf(format string, args ...any)
	Fatalf(format string, args ...any)
	Cleanup(f func())
}
