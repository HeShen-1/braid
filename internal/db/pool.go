package db

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// PoolConfig 是业务入口的 MySQL 连接池三值(T7.1 env 化,D-t63 移交回收:
// 2026-09-26 的保守硬编码默认在此升级为可配置,值语义原样)。
//
// 取值口径(DefaultPoolConfig):MaxOpen=25 与 dev compose 单 MySQL 实例的
// 默认 max_connections(151)错峰容纳四入口(4×25=100 < 151,留运维余量);
// Lifetime=5min 消化 MySQL wait_timeout 与 LB 轮转下的陈旧连接;Idle=MaxOpen
// 使突发后不缩池抖动(低流量部署可经 BRAID_DB_MAX_IDLE 调低)。容量口径
// 全表见 deploy/README.md 配额表。
type PoolConfig struct {
	MaxOpen         int
	MaxIdle         int
	ConnMaxLifetime time.Duration
}

// DefaultPoolConfig 返回保守默认(250726d 定稿原值)。
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		MaxOpen:         25,
		MaxIdle:         25,
		ConnMaxLifetime: 5 * time.Minute,
	}
}

// Validate 报告池值矛盾(启动即败,不进运行期):MaxOpen≥1;0≤MaxIdle≤
// MaxOpen(idle 超过 open 只会闲置收紧上限语义);ConnMaxLifetime>0(零/负
// 即关闭生命周期管理,回归 D-t63 修复前的陈旧连接面)。
func (c PoolConfig) Validate() error {
	switch {
	case c.MaxOpen < 1:
		return fmt.Errorf("db: pool max open %d < 1", c.MaxOpen)
	case c.MaxIdle < 0:
		return fmt.Errorf("db: pool max idle %d < 0", c.MaxIdle)
	case c.MaxIdle > c.MaxOpen:
		return fmt.Errorf("db: pool max idle %d > max open %d", c.MaxIdle, c.MaxOpen)
	case c.ConnMaxLifetime <= 0:
		return fmt.Errorf("db: pool conn max lifetime %s <= 0", c.ConnMaxLifetime)
	}
	return nil
}

// ParsePoolConfig 从 getenv 读三键(BRAID_DB_MAX_OPEN / BRAID_DB_MAX_IDLE /
// BRAID_DB_CONN_MAX_LIFETIME;未设取默认,坏值启动即败)。getenv 注入
// os.Getenv;四入口 ConfigFromEnv 是唯一集中解析点(换装点纪律,注入式
// 同 llm/notify/tracing 先例),构造末尾过 Validate。
func ParsePoolConfig(getenv func(string) string) (PoolConfig, error) {
	c := DefaultPoolConfig()
	if raw := getenv("BRAID_DB_MAX_OPEN"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return c, fmt.Errorf("db: BRAID_DB_MAX_OPEN=%q: want int >= 1", raw)
		}
		c.MaxOpen = n
	}
	if raw := getenv("BRAID_DB_MAX_IDLE"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return c, fmt.Errorf("db: BRAID_DB_MAX_IDLE=%q: want int in [0, max open]", raw)
		}
		c.MaxIdle = n
	}
	if raw := getenv("BRAID_DB_CONN_MAX_LIFETIME"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return c, fmt.Errorf("db: BRAID_DB_CONN_MAX_LIFETIME=%q: want duration > 0", raw)
		}
		c.ConnMaxLifetime = d
	}
	// 软上限收敛(T12.2):idle 伴超限时随降——否则 clamped open 下
	// Validate 的 idle>open 硬错误会把软上限偷换成拒启(闸值语义漂移)。
	if c.MaxOpen > MaxOpenSoftCap {
		effectiveIdle := c.MaxIdle
		if effectiveIdle > MaxOpenSoftCap {
			effectiveIdle = MaxOpenSoftCap
		}
		slog.Warn("db: BRAID_DB_MAX_OPEN over soft cap, clamped",
			"configured_max_open", c.MaxOpen,
			"effective_max_open", MaxOpenSoftCap,
			"effective_max_idle", effectiveIdle,
			"hint", "single-process pool exceeds the fleet default budget; raise MySQL max_connections and check deploy/README.md #1: sum of MAX_OPEN across all entrances <= max_connections - 30")
		c.MaxOpen = MaxOpenSoftCap
		c.MaxIdle = effectiveIdle
	}
	return c, c.Validate()
}

// MaxOpenSoftCap 单进程 MAX_OPEN 软上限(T12.2,#67-③ 自伤型误配护栏):
// 锚 = deploy/README.md §1 对账表「Σ 常驻四入口默认 = 100」——单进程超过
// 全 fleet 默认预算,即越过容量公式(Σ MAX_OPEN ≤ max_connections − 预留
// ≥30,MySQL 默认 151)的默认头寸,误配大值可打满 max_connections 自伤。
// 超限不拒启(软上限):收敛到上限 + 启动 warn 指向公式,生效值仍经各
// cmd 启动 slog 与 braid_db_pool_max_open 指标可见。真要单进程 >100 =
// 同轮抬 MySQL max_connections 并自核公式;常量属部署契约,改动走 D- 条目。
const MaxOpenSoftCap = 100

// ConfigurePool 对业务入口的 *sql.DB 施加池三值——四入口(cmd/api|worker|
// relay|reaper)唯一施加点,值经各自 Config.Pool 注入。施加前过 Validate:
// 零值 MaxOpen 会 SetMaxOpenConns(0) = 无上限(恰是 D-t63 修掉的回归面),
// 在施加点结构性挡住跳过 ParsePoolConfig 的手搓字面量调用方。
func ConfigurePool(dbh *sql.DB, cfg PoolConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	dbh.SetMaxOpenConns(cfg.MaxOpen)
	dbh.SetMaxIdleConns(cfg.MaxIdle)
	dbh.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	return nil
}
