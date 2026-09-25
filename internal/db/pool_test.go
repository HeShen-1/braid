package db

import (
	"database/sql"
	"testing"
	"time"
)

// TestConfigurePoolAppliesLimits:三值生效断言(零 DB,纯 *sql.DB 内存态)。
// T7.1 配置化换装时本测试同步改读 env 组合。
func TestConfigurePoolAppliesLimits(t *testing.T) {
	dbh, err := sql.Open("mysql", "user:pass@tcp(127.0.0.1:3306)/none") // 不拨号,仅取句柄
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer dbh.Close() //nolint:errcheck // 测试退出,关闭错误无处置义

	if s := dbh.Stats(); s.MaxOpenConnections != 0 {
		t.Fatalf("default MaxOpenConnections = %d, want 0(未配置)", s.MaxOpenConnections)
	}
	ConfigurePool(dbh)
	s := dbh.Stats()
	if s.MaxOpenConnections != 25 {
		t.Fatalf("MaxOpenConnections = %d, want 25", s.MaxOpenConnections)
	}
	// MaxIdle 与 ConnMaxLifetime 无 Stats 只读面,以契约常量钉死,漂移即红。
	if maxIdleConns != 25 {
		t.Fatalf("maxIdleConns = %d, want 25", maxIdleConns)
	}
	if connMaxLifetime != 5*time.Minute {
		t.Fatalf("connMaxLifetime = %v, want 5m", connMaxLifetime)
	}
}
