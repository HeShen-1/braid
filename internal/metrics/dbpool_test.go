package metrics

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestDBPoolObserve:单帧 Stats 快照落位;WaitCount 是外部累计值,counter
// 只能经 Add 增量镜像(差分),重复快照不重复计数。
func TestDBPoolObserve(t *testing.T) {
	s, _ := NewSet()
	s.DBPool.Observe(sql.DBStats{
		MaxOpenConnections: 25, OpenConnections: 7, InUse: 3, Idle: 4, WaitCount: 10,
	})
	if got := testutil.ToFloat64(s.DBPool.MaxOpen); got != 25 {
		t.Fatalf("max_open = %v, want 25", got)
	}
	if got := testutil.ToFloat64(s.DBPool.OpenConnections); got != 7 {
		t.Fatalf("open = %v, want 7", got)
	}
	if got := testutil.ToFloat64(s.DBPool.InUse); got != 3 {
		t.Fatalf("in_use = %v, want 3", got)
	}
	if got := testutil.ToFloat64(s.DBPool.Idle); got != 4 {
		t.Fatalf("idle = %v, want 4", got)
	}
	if got := testutil.ToFloat64(s.DBPool.WaitTotal); got != 10 {
		t.Fatalf("wait_total = %v, want 10", got)
	}

	// 第二帧:WaitCount 10→13 只加 3,其余 gauge 原位覆写。
	s.DBPool.Observe(sql.DBStats{
		MaxOpenConnections: 40, OpenConnections: 8, InUse: 5, Idle: 3, WaitCount: 13,
	})
	if got := testutil.ToFloat64(s.DBPool.WaitTotal); got != 13 {
		t.Fatalf("wait_total after second frame = %v, want 13", got)
	}
	if got := testutil.ToFloat64(s.DBPool.InUse); got != 5 {
		t.Fatalf("in_use after second frame = %v, want 5", got)
	}
	if got := testutil.ToFloat64(s.DBPool.MaxOpen); got != 40 {
		t.Fatalf("max_open after second frame = %v, want 40", got)
	}
}

// sampleDriver:零拨号最小 driver,让采集器对着真实 *sql.DB 计数采样
// (Conn 获取后 OpenConnections=1 可被采集器观测)。
type sampleDriver struct{}

func (sampleDriver) Open(string) (driver.Conn, error) { return sampleConn{}, nil }

type sampleConn struct{}

func (sampleConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("stub: prepare unsupported")
}
func (sampleConn) Close() error              { return nil }
func (sampleConn) Begin() (driver.Tx, error) { return nil, errors.New("stub: begin unsupported") }

func init() { sql.Register("metrics-dbpool-sample", sampleDriver{}) }

// TestCollectDBPoolRunsUntilCancel:首拍立即 + ctx 取消即退出(不泄漏
// goroutine)。间隔用 1h:gauge 在秒级取到值只可能来自 ticker 前的首拍
// Observe——删掉首拍(只靠周期)则本测试必红,唯一定位「启动即可见」性质。
func TestCollectDBPoolRunsUntilCancel(t *testing.T) {
	dbh, err := sql.Open("metrics-dbpool-sample", "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer dbh.Close() //nolint:errcheck // 测试退出,关闭错误无处置义
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := dbh.Conn(ctx) // 持一条连接:OpenConnections=1
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close() //nolint:errcheck // 同上

	s, _ := NewSet()
	done := make(chan struct{})
	go func() {
		collectDBPool(ctx, dbh, s.DBPool, time.Hour)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for testutil.ToFloat64(s.DBPool.OpenConnections) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("open_connections gauge never sampled 1 (got %v) — first snapshot before ticker missing?",
				testutil.ToFloat64(s.DBPool.OpenConnections))
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("collector did not exit after ctx cancel")
	}
}
