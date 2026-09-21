// Package sqltest 提供 braid 引擎层测试基建:指向 deploy/migrate-test.docker-compose.yml
// 起的验证容器(docker compose -f deploy/migrate-test.docker-compose.yml -p braid-t13 up -d),
// 容器不可达时调用方自动 skip。凭证是该一次性本地容器的占位值,非任何真实系统凭证。
//
// 与 internal/db 自身测试的关键差异:引擎测试用**独立 schema**(braid_engine_test),
// 避免 go test ./... 并发跑包时被 db 包 TestMigrateUpDownIdempotent 的 down/up
// 循环拆表(两个测试二进制并行,同 schema 会互相踩)。需要进一步隔离的包
// (如 relay 的轮询测试会捡走全库 eligible 行)用 OpenAt/MustOpenAt 自带 schema。
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

// migrated 记录已迁移(或迁移失败)的 schema,避免同进程重复迁移。
var migrated sync.Map

// MustOpen 返回指向默认测试 schema(braid_engine_test)的连接池。
// 栈不可达或迁移失败 → t.Skipf 并附具体原因(区分两者;迁移失败的 skip
// 属环境损坏,提示语里带 err 供诊断)。
func MustOpen(t TB) *sql.DB {
	t.Helper()
	return MustOpenAt(t, testSchema)
}

// MustOpenAt 是 MustOpen 的多 schema 形态(relay 等需要包级隔离的测试用)。
func MustOpenAt(t TB, schema string) *sql.DB {
	t.Helper()
	d, err := OpenAt(schema)
	if err != nil {
		t.Skipf("braid-t13-mysql 不可达,先起验证容器: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// Open 是 MustOpen 的非 testing 形态(kill -9 测试子进程无法携带 *testing.T):
// 栈不可达/迁移失败返回错误而非 skip,由调用方决定退出码。
func Open() (*sql.DB, error) {
	return OpenAt(testSchema)
}

// OpenAt 打开(懒建+迁移)指定 schema 的连接池。已记录的迁移失败原样传播
// (不静默降级成「打开成功然后 table-not-exist」)。
func OpenAt(schema string) (*sql.DB, error) {
	if v, ok := migrated.Load(schema); ok {
		if err, _ := v.(error); err != nil {
			return nil, err
		}
	} else {
		err := initSchema(schema)
		migrated.Store(schema, err)
		if err != nil {
			return nil, err
		}
	}
	d, err := sql.Open("mysql", db.CanonicalDSN(serverDSN+schema))
	if err != nil {
		return nil, err
	}
	if err := d.Ping(); err != nil {
		_ = d.Close()
		return nil, err
	}
	return d, nil
}

func initSchema(schema string) error {
	admin, err := sql.Open("mysql", db.CanonicalDSN(serverDSN))
	if err != nil {
		return err
	}
	defer func() { _ = admin.Close() }()
	if err := admin.Ping(); err != nil {
		return err
	}
	if _, err := admin.Exec(
		`CREATE DATABASE IF NOT EXISTS ` + schema +
			` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci`); err != nil {
		return fmt.Errorf("create %s: %w", schema, err)
	}
	// golang-migrate 的 schema_migrations 落在该 schema 内;幂等,残留即复用。
	return db.Migrate(serverDSN + schema)
}

// TB 收窄 testing.TB 到测试所需面(避免测试文件外泄 testing 依赖)。
type TB interface {
	Helper()
	Skipf(format string, args ...any)
	Fatalf(format string, args ...any)
	Cleanup(f func())
}
