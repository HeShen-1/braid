package relay

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// Config 是 relay 的全部可调参数。默认值集中在此(T7.1 配置体系换装时原样搬);
// ConfigFromEnv 是唯一环境解析点,字段名即未来配置键。
type Config struct {
	// IdleInterval 是空转(无可投行)时的轮询间隔。有行时连续排空,不睡眠。
	IdleInterval time.Duration
	// EnqueueTimeout 界住单次入队(= 单行锁窗)的时长。
	EnqueueTimeout time.Duration
	// MaxAttempts 是单事件投递尝试上限(有结论的尝试才计数),达到即转 DEAD。
	MaxAttempts int
	// BackoffBase/BackoffFactor/BackoffMax 构成指数退避:第 n 次失败后
	// base*factor^(n-1),封顶 BackoffMax。默认序列 5s→20s→80s→5m(320s 已被
	// 5m 封顶截断)→5m,MaxAttempts=6 时自首次失败约 11.8 分钟转 DEAD——对齐
	// T6.4「该有人看了」的死信告警节奏,又不把 30s 级 Redis 抖动打成死信。
	BackoffBase   time.Duration
	BackoffFactor float64
	BackoffMax    time.Duration
	// Jitter 是退避抖动幅度(±比例),打散未来多 relay 同拍重试。固定 0.2,
	// 不暴露 env(非运维旋钮)。
	Jitter float64
}

// DefaultConfig 返回生产默认值。
func DefaultConfig() Config {
	return Config{
		IdleInterval:   time.Second,
		EnqueueTimeout: 5 * time.Second,
		MaxAttempts:    6,
		BackoffBase:    5 * time.Second,
		BackoffFactor:  4,
		BackoffMax:     5 * time.Minute,
		Jitter:         0.2,
	}
}

// Validate 报告配置矛盾(启动即败,不进运行期)。上界对着真实表示域:
// MaxAttempts ≤ 65535(deliver_attempts SMALLINT UNSIGNED,越界会在第 65536 次
// 记账时 1264 报错回滚 → 行永卡 PENDING 死循环);IdleInterval ≥ 10ms 下限防
// 空转全速轮询打 MySQL;NaN/±Inf 穿透一切比较,显式拒绝。
func (c Config) Validate() error {
	switch {
	case c.IdleInterval < 10*time.Millisecond:
		return fmt.Errorf("relay: idle interval %s < 10ms", c.IdleInterval)
	case c.EnqueueTimeout <= 0:
		return fmt.Errorf("relay: enqueue timeout %s <= 0", c.EnqueueTimeout)
	case c.MaxAttempts < 1:
		return fmt.Errorf("relay: max attempts %d < 1", c.MaxAttempts)
	case c.MaxAttempts > 65535:
		return fmt.Errorf("relay: max attempts %d > 65535 (deliver_attempts SMALLINT UNSIGNED)", c.MaxAttempts)
	case c.BackoffBase <= 0:
		return fmt.Errorf("relay: backoff base %s <= 0", c.BackoffBase)
	case math.IsNaN(c.BackoffFactor) || math.IsInf(c.BackoffFactor, 0):
		return fmt.Errorf("relay: backoff factor %v not finite", c.BackoffFactor)
	case c.BackoffFactor < 1:
		return fmt.Errorf("relay: backoff factor %v < 1", c.BackoffFactor)
	case c.BackoffMax < c.BackoffBase:
		return fmt.Errorf("relay: backoff max %s < base %s", c.BackoffMax, c.BackoffBase)
	case math.IsNaN(c.Jitter) || math.IsInf(c.Jitter, 0):
		return fmt.Errorf("relay: jitter %v not finite", c.Jitter)
	case c.Jitter < 0 || c.Jitter >= 1:
		return fmt.Errorf("relay: jitter %v out of [0,1)", c.Jitter)
	}
	return nil
}

// ConfigFromEnv 从环境变量构造配置(BRAID_RELAY_ 前缀;未设键取默认)。
// getenv 注入 os.Getenv,测试换桩。选 env 弃 flag:Q6 部署形态是 compose,
// env 是部署原生载体。
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	c := DefaultConfig()
	var err error
	if c.IdleInterval, err = envDuration(getenv, "BRAID_RELAY_IDLE_INTERVAL", c.IdleInterval); err != nil {
		return c, err
	}
	if c.EnqueueTimeout, err = envDuration(getenv, "BRAID_RELAY_ENQUEUE_TIMEOUT", c.EnqueueTimeout); err != nil {
		return c, err
	}
	if c.MaxAttempts, err = envInt(getenv, "BRAID_RELAY_MAX_ATTEMPTS", c.MaxAttempts); err != nil {
		return c, err
	}
	if c.BackoffBase, err = envDuration(getenv, "BRAID_RELAY_BACKOFF_BASE", c.BackoffBase); err != nil {
		return c, err
	}
	if c.BackoffFactor, err = envFloat(getenv, "BRAID_RELAY_BACKOFF_FACTOR", c.BackoffFactor); err != nil {
		return c, err
	}
	if c.BackoffMax, err = envDuration(getenv, "BRAID_RELAY_BACKOFF_MAX", c.BackoffMax); err != nil {
		return c, err
	}
	return c, c.Validate()
}

func envDuration(getenv func(string) string, key string, def time.Duration) (time.Duration, error) {
	raw := getenv(key)
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return def, fmt.Errorf("relay: %s=%q: %w", key, raw, err)
	}
	return d, nil
}

func envInt(getenv func(string) string, key string, def int) (int, error) {
	raw := getenv(key)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def, fmt.Errorf("relay: %s=%q: %w", key, raw, err)
	}
	return n, nil
}

func envFloat(getenv func(string) string, key string, def float64) (float64, error) {
	raw := getenv(key)
	if raw == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return def, fmt.Errorf("relay: %s=%q: %w", key, raw, err)
	}
	return f, nil
}
