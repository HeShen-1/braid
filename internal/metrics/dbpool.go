package metrics

import (
	"context"
	"database/sql"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// DBPoolSet covers the shared *sql.DB pool gauges(T7.1 组合形态观测,D-t63
// 移交回收):MaxOpen 兼「生效配置可见性」(启动 slog 之外的面),in_use/
// max_open 是饱和度口径,WaitTotal 是池饱和的唯一可持续信号(增长 = 该调
// BRAID_DB_MAX_OPEN,容量口径见 deploy/README.md 配额表)。
type DBPoolSet struct {
	OpenConnections prometheus.Gauge
	InUse           prometheus.Gauge
	Idle            prometheus.Gauge
	MaxOpen         prometheus.Gauge
	WaitTotal       prometheus.Counter

	// lastWait 镜像上次快照的 DBStats.WaitCount(外部累计值只能差分 Add
	// 进 counter)。每进程唯一采集 goroutine 串行调用 Observe,无需锁。
	lastWait int64
}

// Observe 把一帧 *sql.DB Stats 快照镜像到 gauge 面;WaitCount 差分累计进
// WaitTotal(重复快照不重复计数)。
func (d *DBPoolSet) Observe(s sql.DBStats) {
	d.OpenConnections.Set(float64(s.OpenConnections))
	d.InUse.Set(float64(s.InUse))
	d.Idle.Set(float64(s.Idle))
	d.MaxOpen.Set(float64(s.MaxOpenConnections))
	if delta := s.WaitCount - d.lastWait; delta > 0 {
		d.WaitTotal.Add(float64(delta))
	}
	d.lastWait = s.WaitCount
}

// PoolCollectInterval 是池 Stats 采样周期:固定常量非旋钮(防旋钮膨胀;
// 池饱和是分钟级现象,30s 分辨率充分)。
const PoolCollectInterval = 30 * time.Second

// CollectDBPool 周期镜像 dbh.Stats() 到 set,直至 ctx 结束;首拍立即
// (启动即可见生效三值,与 cmd 启动 slog 行互补)。
func CollectDBPool(ctx context.Context, dbh *sql.DB, set *DBPoolSet) {
	collectDBPool(ctx, dbh, set, PoolCollectInterval)
}

func collectDBPool(ctx context.Context, dbh *sql.DB, set *DBPoolSet, interval time.Duration) {
	set.Observe(dbh.Stats())
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			set.Observe(dbh.Stats())
		}
	}
}
