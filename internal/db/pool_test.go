package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestDefaultPoolConfigPinsConservativeValues:env 化不得顺改保守默认
// (250726d 定稿口径:4×25 < 151 错峰 + 5min 消化陈旧连接 + Idle=Open 防缩池)。
func TestDefaultPoolConfigPinsConservativeValues(t *testing.T) {
	c := DefaultPoolConfig()
	if c.MaxOpen != 25 || c.MaxIdle != 25 || c.ConnMaxLifetime != 5*time.Minute {
		t.Fatalf("defaults = %+v, want {25 25 5m}", c)
	}
}

// TestParsePoolConfig:三键解析、缺省回填与坏值启动即败(表驱动)。
// 变异自查:Validate 丢了 → idle_over_open / lifetime_zero 红;解析丢了
// 覆盖臂 → override 红。
func TestParsePoolConfig(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    PoolConfig
		wantErr string
	}{
		{
			name: "empty env yields defaults",
			env:  nil,
			want: DefaultPoolConfig(),
		},
		{
			name: "all keys override",
			env: map[string]string{
				"BRAID_DB_MAX_OPEN":          "10",
				"BRAID_DB_MAX_IDLE":          "4",
				"BRAID_DB_CONN_MAX_LIFETIME": "90s",
			},
			want: PoolConfig{MaxOpen: 10, MaxIdle: 4, ConnMaxLifetime: 90 * time.Second},
		},
		{
			name: "max open alone",
			env:  map[string]string{"BRAID_DB_MAX_OPEN": "40"},
			want: PoolConfig{MaxOpen: 40, MaxIdle: 25, ConnMaxLifetime: 5 * time.Minute},
		},
		{
			name: "lifetime alone",
			env:  map[string]string{"BRAID_DB_CONN_MAX_LIFETIME": "30m"},
			want: PoolConfig{MaxOpen: 25, MaxIdle: 25, ConnMaxLifetime: 30 * time.Minute},
		},
		{
			name:    "max open not int",
			env:     map[string]string{"BRAID_DB_MAX_OPEN": "ten"},
			wantErr: `BRAID_DB_MAX_OPEN="ten"`,
		},
		{
			name:    "max idle not int",
			env:     map[string]string{"BRAID_DB_MAX_IDLE": "x"},
			wantErr: `BRAID_DB_MAX_IDLE="x"`,
		},
		{
			name:    "max open zero",
			env:     map[string]string{"BRAID_DB_MAX_OPEN": "0"},
			wantErr: "max open 0 < 1",
		},
		{
			name:    "max open negative",
			env:     map[string]string{"BRAID_DB_MAX_OPEN": "-3"},
			wantErr: "max open -3 < 1",
		},
		{
			name:    "idle over open",
			env:     map[string]string{"BRAID_DB_MAX_OPEN": "5", "BRAID_DB_MAX_IDLE": "6"},
			wantErr: "max idle 6 > max open 5",
		},
		{
			name:    "idle negative",
			env:     map[string]string{"BRAID_DB_MAX_IDLE": "-1"},
			wantErr: "max idle -1 < 0",
		},
		{
			name:    "lifetime bad duration",
			env:     map[string]string{"BRAID_DB_CONN_MAX_LIFETIME": "5x"},
			wantErr: `BRAID_DB_CONN_MAX_LIFETIME="5x"`,
		},
		{
			name:    "lifetime zero",
			env:     map[string]string{"BRAID_DB_CONN_MAX_LIFETIME": "0s"},
			wantErr: "lifetime 0s <= 0",
		},
		{
			name:    "lifetime negative",
			env:     map[string]string{"BRAID_DB_CONN_MAX_LIFETIME": "-1m"},
			wantErr: "lifetime -1m0s <= 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePoolConfig(func(k string) string { return tt.env[k] })
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParsePoolConfig() err = nil, want containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParsePoolConfig() err = %q, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePoolConfig() unexpected err: %v", err)
			}
			if got != tt.want {
				t.Fatalf("ParsePoolConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// stubPoolDriver:计数 Open 的最小 driver(零拨号、零网络),让 ConfigurePool
// 的施加值可黑盒观测——Stats 只读面覆盖 MaxOpen/Idle,lifetime 经「过期后
// 重取必开新连接」计数断言。
type stubPoolDriver struct {
	opened atomic.Int64
}

func (d *stubPoolDriver) Open(string) (driver.Conn, error) {
	d.opened.Add(1)
	return &stubPoolConn{}, nil
}

type stubPoolConn struct{}

func (c *stubPoolConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("stub: prepare unsupported")
}
func (c *stubPoolConn) Close() error              { return nil }
func (c *stubPoolConn) Begin() (driver.Tx, error) { return nil, errors.New("stub: begin unsupported") }

var stubDriverSeq atomic.Int64

// openStub:每用例独立 driver 名(sql.Register 全局表防重名),独立计数。
func openStub(t *testing.T) (*sql.DB, *stubPoolDriver) {
	t.Helper()
	drv := &stubPoolDriver{}
	name := fmt.Sprintf("poolstub-%d", stubDriverSeq.Add(1))
	sql.Register(name, drv)
	dbh, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open stub: %v", err)
	}
	t.Cleanup(func() { _ = dbh.Close() })
	return dbh, drv
}

// mustPool:测试内施加池配置(失败即 fatal)。
func mustPool(t *testing.T, dbh *sql.DB, cfg PoolConfig) {
	t.Helper()
	if err := ConfigurePool(dbh, cfg); err != nil {
		t.Fatalf("ConfigurePool: %v", err)
	}
}

// TestConfigurePoolAppliesConfig:改配置 → 行为变化(池上限生效)的验收锚。
// MaxOpen/Idle 经 Stats 只读面;lifetime 经 cleaner 扫掠后重取开新连接;
// 钳制臂(MaxOpen=1)证明并发获取真被上限挡住,而非仅 Stats 写了个数;
// 零值臂锁死「拒收不施加」(D-t63 回归面)。
func TestConfigurePoolAppliesConfig(t *testing.T) {
	ctx := context.Background()

	t.Run("zero config rejected without overriding applied values", func(t *testing.T) {
		dbh, _ := openStub(t)
		mustPool(t, dbh, PoolConfig{MaxOpen: 7, MaxIdle: 3, ConnMaxLifetime: time.Minute})
		if err := ConfigurePool(dbh, PoolConfig{}); err == nil {
			t.Fatal("zero PoolConfig accepted, want reject")
		}
		// Stats 只读面:拒收不得覆盖已生效的 7(施加 SetMaxOpenConns(0) 也会
		// 显示 0,故「仍为 7」才证明拒收发生在施加前)。
		if s := dbh.Stats(); s.MaxOpenConnections != 7 {
			t.Fatalf("MaxOpenConnections = %d, want 7 (rejected config must not override applied values)", s.MaxOpenConnections)
		}
	})

	t.Run("stats reflect applied values", func(t *testing.T) {
		dbh, _ := openStub(t)
		cfg := PoolConfig{MaxOpen: 7, MaxIdle: 3, ConnMaxLifetime: time.Minute}
		mustPool(t, dbh, cfg)
		if s := dbh.Stats(); s.MaxOpenConnections != 7 {
			t.Fatalf("MaxOpenConnections = %d, want 7", s.MaxOpenConnections)
		}
	})

	t.Run("idle surplus closed on return", func(t *testing.T) {
		dbh, drv := openStub(t)
		mustPool(t, dbh, PoolConfig{MaxOpen: 4, MaxIdle: 1, ConnMaxLifetime: time.Minute})
		conns := make([]*sql.Conn, 3)
		for i := range conns {
			c, err := dbh.Conn(ctx)
			if err != nil {
				t.Fatalf("conn %d: %v", i, err)
			}
			conns[i] = c
		}
		if got := drv.opened.Load(); got != 3 {
			t.Fatalf("opened = %d after 3 acquires, want 3", got)
		}
		for _, c := range conns {
			_ = c.Close()
		}
		waitFor(t, 2*time.Second, func() bool { return dbh.Stats().Idle == 1 })
	})

	t.Run("expired connection not reused", func(t *testing.T) {
		dbh, drv := openStub(t)
		mustPool(t, dbh, PoolConfig{MaxOpen: 2, MaxIdle: 1, ConnMaxLifetime: 20 * time.Millisecond})
		c, err := dbh.Conn(ctx)
		if err != nil {
			t.Fatalf("conn: %v", err)
		}
		_ = c.Close() // 回池,20ms 后过期
		// cleaner 扫掠或惰性复用检查任一路径生效 → 重取必开新连接。
		waitFor(t, 2*time.Second, func() bool {
			c2, err := dbh.Conn(ctx)
			if err != nil {
				t.Fatalf("reconn: %v", err)
			}
			_ = c2.Close()
			return drv.opened.Load() >= 2
		})
	})

	t.Run("max open clamps concurrent acquires", func(t *testing.T) {
		dbh, drv := openStub(t)
		mustPool(t, dbh, PoolConfig{MaxOpen: 1, MaxIdle: 1, ConnMaxLifetime: time.Minute})
		var (
			inflight atomic.Int64
			peak     atomic.Int64
			wg       sync.WaitGroup
			start    = make(chan struct{})
		)
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start // 同拍起跑:无钳制时 4 个获取会立即并发在飞
				c, err := dbh.Conn(ctx)
				if err != nil {
					t.Errorf("conn: %v", err)
					return
				}
				n := inflight.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				time.Sleep(10 * time.Millisecond) // 持有窗:让钳制可见
				inflight.Add(-1)
				_ = c.Close()
			}()
		}
		close(start)
		wg.Wait() // 全部完成:钳制是排队,不是死锁
		// 钳制证据:在飞峰值 ≤ MaxOpen(第 2 个获取必须等第 1 个释放)。
		if got := peak.Load(); got > 1 {
			t.Fatalf("peak in-flight conns = %d, want <= 1 (MaxOpen=1 clamp)", got)
		}
		// 串行复用证据:MaxIdle=1 下归还连接进空闲表被下个获取复用,
		// 4 个串行获取全程恰 1 条物理连接。
		if got := drv.opened.Load(); got != 1 {
			t.Fatalf("opened = %d, want 1 (serial acquires reuse the idle conn)", got)
		}
	})
}

// waitFor:轮询断言(截止内每 5ms 重试;超时 fatal)——用于 cleaner 等
// 异步扫掠路径,替代固定 sleep 防 CI 慢机抖红。
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", d)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
