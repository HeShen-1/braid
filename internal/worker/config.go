package worker

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"braid/internal/db"
	"braid/internal/snowflake"
)

// Config 是 worker 的全部可调参数(默认值集中在此,T7.1 已换装收编;
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
	// SnowflakeNode 是本进程雪花节点号(attempt 行 id 应用侧生成)。显式
	// 配置面当前仅 api=1、worker=2 两个常驻进程;relay 无雪花生成器,reaper
	// 缺省 PID 派生、可经 BRAID_REAPER_SNOWFLAKE_NODE 显式配置(T7.1)——
	// 理论可与 1/2 撞号,碰撞面 = reaper 仅产 outbox 事件行 id。跨进程重号
	// 唯一症状是业务表 PK 1062;域校验在 snowflake.New(与 api 同规)。
	SnowflakeNode uint16
	// MySQLDSN 连接串(BRAID_MYSQL_DSN;含凭证,不进日志/审计)。T7.1 收编
	// cmd 直读;必填断言在 cmd 读本字段(装配输入非旋钮)。
	MySQLDSN string
	// RedisQueueAddr 队列 Redis 地址(BRAID_REDIS_QUEUE_ADDR;必填断言在
	// cmd 读本字段)。
	RedisQueueAddr string
	// MetricsAddr 指标面监听地址(BRAID_METRICS_ADDR;空 = 不启)。
	MetricsAddr string
	// ToolTokenSecret 是工具 scoped token 的跨进程校验密钥
	// (BRAID_TOOL_TOKEN_SECRET,hex;缺省 nil = 进程内随机,重启即轮换
	// ——P0 校验点在同进程调用门,自洽)。不进日志/审计(铁律)。
	ToolTokenSecret []byte
	// Pool 是 MySQL 连接池三值(T7.1,D-t63 移交回收):db.ParsePoolConfig
	// 注入式解析 BRAID_DB_* 三键,施加点 cmd 的 db.ConfigurePool。
	Pool db.PoolConfig
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
		SnowflakeNode:     2,
		Pool:              db.DefaultPoolConfig(),
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
	c.MySQLDSN = getenv("BRAID_MYSQL_DSN")
	c.RedisQueueAddr = getenv("BRAID_REDIS_QUEUE_ADDR")
	c.MetricsAddr = getenv("BRAID_METRICS_ADDR")
	if c.HeartbeatInterval, err = envDuration(getenv, "BRAID_WORKER_HEARTBEAT_INTERVAL", c.HeartbeatInterval); err != nil {
		return c, err
	}
	if c.RetryDelay, err = envDuration(getenv, "BRAID_WORKER_RETRY_DELAY", c.RetryDelay); err != nil {
		return c, err
	}
	if c.ShutdownTimeout, err = envDuration(getenv, "BRAID_WORKER_SHUTDOWN_TIMEOUT", c.ShutdownTimeout); err != nil {
		return c, err
	}
	if raw := getenv("BRAID_WORKER_SNOWFLAKE_NODE"); raw != "" {
		n, err := strconv.ParseUint(raw, 10, 16)
		if err != nil || uint64(n) > uint64(snowflake.MaxNodeID) {
			return c, fmt.Errorf("worker: BRAID_WORKER_SNOWFLAKE_NODE=%q: want 0..%d", raw, snowflake.MaxNodeID)
		}
		c.SnowflakeNode = uint16(n)
	}
	if raw := getenv("BRAID_TOOL_TOKEN_SECRET"); raw != "" {
		key, derr := hex.DecodeString(raw)
		if derr != nil {
			// 不回显原始值(密钥素材不进日志/审计——铁律;错误经 fatal 落启动日志)
			return c, fmt.Errorf("worker: BRAID_TOOL_TOKEN_SECRET: want hex: %w", derr)
		}
		c.ToolTokenSecret = key
	}
	if c.Pool, err = db.ParsePoolConfig(getenv); err != nil {
		return c, err
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	if err := crossValidateProducerQueues(getenv, c.Queues); err != nil {
		return c, err
	}
	return c, nil
}

// crossValidateProducerQueues 是「消费键集必须覆盖全部生产方队列」的启动
// 执法(T12.2,D-t43 §7 蒸发回收:reaper 队列漏改 = 重投/通知事件落无
// worker 消费的队列,relay 投递成功标 DISPATCHED、任务永挂、每 DispatchStale
// 窗空转一轮)。镜像 reaper crossValidateWorkerTimings 先例:同机同 env
// compose 部署形态下读他进程配置键(键名与默认值与 internal/api、
// internal/reaper 同步维护,worker 不 import 两者,依赖方向单向)。
// 生产方三族:
//
//	api       → BRAID_API_RESUME_QUEUE(默认 "default";审批续跑 execution.resume)
//	reaper    → BRAID_REAPER_QUEUE(默认 "default";死信网重投 + approval.notify)
//	scheduler → 常量 "default" 无 env(驻 worker 进程,cmd 不设 Deps.Queue)
//
// 任一生产队列不在 BRAID_WORKER_QUEUES 键集 → 启动即败(.env.example 的
// 「必须在 BRAID_WORKER_QUEUES 内」注释契约在此变成执法;坏值形态的生产
// 键同样不落键集,自然触发)。已知边界:多舰队故意按队列拆分 worker 组
// (各自消费子集)= 不支持面——Q14 单实例口径,拆分部署须每组配置全
// 队列超集。
func crossValidateProducerQueues(getenv func(string) string, queues map[string]int) error {
	producers := []struct {
		envKey string
		def    string
		desc   string
	}{
		{"BRAID_API_RESUME_QUEUE", "default", "api approval-resume events"},
		{"BRAID_REAPER_QUEUE", "default", "reaper redispatch/notify events"},
	}
	for _, p := range producers {
		q := getenv(p.envKey)
		if q == "" {
			q = p.def
		}
		if _, ok := queues[q]; !ok {
			return fmt.Errorf("worker: producer queue %q (%s, from %s) not in BRAID_WORKER_QUEUES; tasks enqueued there hang forever (add it to BRAID_WORKER_QUEUES or repoint %s)",
				q, p.desc, p.envKey, p.envKey)
		}
	}
	// scheduler 节点任务族恒投常量 "default"(sched.go Deps.Queue 空值兜底,
	// 无 env 面):键集去掉 default = 整族永挂,同闸执法。
	if _, ok := queues["default"]; !ok {
		return fmt.Errorf("worker: scheduler node-task events are enqueued to constant queue %q but it is not in BRAID_WORKER_QUEUES; node tasks would hang forever (keep %q in BRAID_WORKER_QUEUES)",
			"default", "default")
	}
	return nil
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
