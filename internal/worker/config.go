package worker

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config 是 worker 的全部可调参数(默认值集中在此,T7.1 配置体系换装时原样搬;
// ConfigFromEnv 是唯一环境解析点,字段名即未来配置键——与 relay.Config 同规)。
type Config struct {
	// Concurrency 是 asynq 并发处理数(同时 active 的任务上限)。
	Concurrency int
	// Queues 是消费队列及严格优先级(asynq StrictPriority 模式:权重即优先级,
	// 高权队列空转前不碰低权)。键集必须覆盖 relay 可投的全部队列——入队到未
	// serve 的队列会永挂 pending(outbox 注释:队列集合正确性归 worker 配置)。
	Queues map[string]int
	// WorkerID 是实例标识(hostname 级),落 node_attempt.worker_id:kill -9 的
	// 检测与归因字段(T8.1)。DDL VARCHAR(64)。
	WorkerID string
	// HeartbeatInterval 是 attempt 心跳间隔。判死阈值(T2.5)须 > 本值×N 且 >
	// 单次外部调用上限,调本值必须同步审 T2.5 grace。
	HeartbeatInterval time.Duration
	// RetryDelay 是 asynq 内部重试延迟(MaxRetry=1 语义的显式钉死,见 worker.go
	// RetryDelayFunc 决策注释)。
	RetryDelay time.Duration
	// ShutdownTimeout 是优雅停机上限:在飞 handler 收口窗口,超时强杀(留
	// RUNNING attempt 归 T2.5 判死)。
	ShutdownTimeout time.Duration
}

// DefaultConfig 返回生产默认值。
func DefaultConfig() Config {
	return Config{
		Concurrency:       10,
		Queues:            map[string]int{"default": 1},
		WorkerID:          "",
		HeartbeatInterval: 10 * time.Second,
		RetryDelay:        5 * time.Second,
		ShutdownTimeout:   30 * time.Second,
	}
}

// queueNameRe 与 outbox 队列词表同源([a-z][a-z0-9_-]{0,63},VARCHAR(64))。
var queueNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Validate 报告配置矛盾(启动即败,不进运行期)。上界对着真实表示域:
// Concurrency ≤ 256 防误配打爆 DB 连接池;HeartbeatInterval/RetryDelay 下限
// 10ms 防心跳风暴与忙重试。
func (c Config) Validate() error {
	switch {
	case c.Concurrency < 1:
		return fmt.Errorf("worker: concurrency %d < 1", c.Concurrency)
	case c.Concurrency > 256:
		return fmt.Errorf("worker: concurrency %d > 256", c.Concurrency)
	case len(c.Queues) == 0:
		return fmt.Errorf("worker: no queues configured")
	case len(c.WorkerID) > 64:
		return fmt.Errorf("worker: worker id %q longer than 64 (DDL VARCHAR(64))", c.WorkerID)
	case c.HeartbeatInterval < 10*time.Millisecond:
		return fmt.Errorf("worker: heartbeat interval %s < 10ms", c.HeartbeatInterval)
	case c.RetryDelay < 10*time.Millisecond:
		return fmt.Errorf("worker: retry delay %s < 10ms", c.RetryDelay)
	case c.ShutdownTimeout <= 0:
		return fmt.Errorf("worker: shutdown timeout %s <= 0", c.ShutdownTimeout)
	}
	for name, weight := range c.Queues {
		if !queueNameRe.MatchString(name) {
			return fmt.Errorf("worker: queue name %q not matching %s", name, queueNameRe)
		}
		if weight < 1 {
			return fmt.Errorf("worker: queue %q weight %d < 1", name, weight)
		}
	}
	return nil
}

// ConfigFromEnv 从环境变量构造配置(BRAID_WORKER_ 前缀;未设键取默认)。格式:
// BRAID_WORKER_QUEUES="default:1,approval:5";BRAID_WORKER_ID 空 → 调用方自定
// (cmd/worker 用 hostname)。getenv 注入 os.Getenv,测试换桩。
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	c := DefaultConfig()
	var err error
	if c.Concurrency, err = envInt(getenv, "BRAID_WORKER_CONCURRENCY", c.Concurrency); err != nil {
		return c, err
	}
	if c.Queues, err = envQueues(getenv("BRAID_WORKER_QUEUES"), c.Queues); err != nil {
		return c, err
	}
	c.WorkerID = getenv("BRAID_WORKER_ID")
	if c.HeartbeatInterval, err = envDuration(getenv, "BRAID_WORKER_HEARTBEAT_INTERVAL", c.HeartbeatInterval); err != nil {
		return c, err
	}
	if c.RetryDelay, err = envDuration(getenv, "BRAID_WORKER_RETRY_DELAY", c.RetryDelay); err != nil {
		return c, err
	}
	if c.ShutdownTimeout, err = envDuration(getenv, "BRAID_WORKER_SHUTDOWN_TIMEOUT", c.ShutdownTimeout); err != nil {
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
		return def, fmt.Errorf("worker: %s=%q: %w", key, raw, err)
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
		return def, fmt.Errorf("worker: %s=%q: %w", key, raw, err)
	}
	return n, nil
}

// envQueues 解析 "name:weight,…" 列表;raw 空 → 返回默认(不覆盖)。
func envQueues(raw string, def map[string]int) (map[string]int, error) {
	if raw == "" {
		return def, nil
	}
	qs := make(map[string]int)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		name, w, found := strings.Cut(part, ":")
		if !found {
			return def, fmt.Errorf("worker: BRAID_WORKER_QUEUES entry %q: want name:weight", part)
		}
		weight, err := strconv.Atoi(strings.TrimSpace(w))
		if err != nil {
			return def, fmt.Errorf("worker: BRAID_WORKER_QUEUES entry %q: %w", part, err)
		}
		qs[strings.TrimSpace(name)] = weight
	}
	return qs, nil
}
