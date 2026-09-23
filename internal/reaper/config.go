// Package reaper 实现 T2.5 reconciler:MySQL 真相源上的纯周期对账器。
//
// 形态(D-2026-09-22-t25-reaper-semantics #1,设计全文 docs/designs/t2.5-reaper.md):
// 候选扫描全部为无锁普通 SELECT;每一项处置在短事务内「锁后复核 + CAS 收置」
// (锁序 = node 行(node_id 升序)→ attempt → wf,与 Complete/心跳一致)。
// Redis 锁仅做 leader 去重且 fail-open——正确性结构性不依赖任何锁、时钟或
// Redis,这是「断 Redis 30s 仍收敛」验收的结构前提。
package reaper

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"time"
)

// Config 是 reaper 的全部可调参数。默认值集中在此(T7.1 配置体系换装时原样搬);
// ConfigFromEnv 是唯一环境解析点。
type Config struct {
	// ScanInterval 是扫描周期(tick)。派生常量:deadline skew margin = 本值
	// (nodecas.go「reconciler 阈值×N 吸收」契约)、leader TTL = 2×本值。
	ScanInterval time.Duration
	// DeadGrace 是心跳判死阈值,必须 > RetryDelay+HeartbeatInterval(接管重跑
	// 后心跳最坏陈旧度,默认 15s)并留足余量;默认 45s = 3×(5s+10s)。
	// ConfigFromEnv 附跨进程硬校验(见 crossValidateWorkerTimings)。
	DeadGrace time.Duration
	// ExecStale 是扫描④的执行级静默窗(④a PENDING 判死 / ④b RUNNING 终态
	// 补判共用 last_activity_at 谓词阈值)。
	ExecStale time.Duration
	// PendingTTL 是 PENDING 执行的判死阈值(执行启动事件死/丢)。必须 >
	// relay 退避穷尽时长(默认 ~11.8min),否则杀掉仍在退避重投的健康事件。
	PendingTTL time.Duration
	// DispatchStale 是分支 B 活事件闸的 DISPATCHED 活窗:重派/启动事件已投
	// 出但尚未生效(asynq 队列积压)的容忍上限,超过视为死事件可重派。
	DispatchStale time.Duration
	// AuditRetention 是 audit_log 分区保留期(DROP 边界;保留期语义 ≥ retention,
	// 被活执行钉住的分区本轮跳过)。
	AuditRetention time.Duration
	// JanitorInterval 是分区水位/outbox 清理作业的节流间隔(独立 goroutine)。
	JanitorInterval time.Duration
	// OutboxBatch 是 outbox DISPATCHED 清理的单批 DELETE 上限。
	OutboxBatch int
	// Queue 是重派事件的投递队列(必须被 worker ServeQueues,否则事件永挂
	// pending 靠扫描②兜底循环;边界同 outbox 词表)。
	Queue string
	// ID 是 reaper 实例标识(选主 token 前缀 + RedispatchNode 归因)。
	// "reaper:"+ID 落 node_execution.worker_id(VARCHAR(64)),故 ≤57。
	ID string
}

// DefaultConfig 返回生产默认值。
func DefaultConfig() Config {
	return Config{
		ScanInterval:    30 * time.Second,
		DeadGrace:       45 * time.Second,
		ExecStale:       2 * time.Minute,
		PendingTTL:      30 * time.Minute,
		DispatchStale:   30 * time.Minute,
		AuditRetention:  2160 * time.Hour, // 90d
		JanitorInterval: time.Hour,
		OutboxBatch:     500,
		Queue:           "default",
	}
}

var queueNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Validate 报告配置矛盾(启动即败,不进运行期)。RunOnce 不调用本方法——
// 校验属启动路径,测试经 Config 字面量注入毫秒级值。
func (c Config) Validate() error {
	switch {
	case c.ScanInterval < time.Second:
		return fmt.Errorf("reaper: scan interval %s < 1s", c.ScanInterval)
	// 20s 硬下限:env 路径的公式余量(>15s 接管陈旧上界 + 余量;跨进程
	// 强校验见 ConfigFromEnv)。
	case c.DeadGrace < 20*time.Second:
		return fmt.Errorf("reaper: dead grace %s < 20s", c.DeadGrace)
	case c.ExecStale < c.ScanInterval:
		return fmt.Errorf("reaper: exec stale %s < scan interval %s", c.ExecStale, c.ScanInterval)
	case c.PendingTTL < 15*time.Minute:
		return fmt.Errorf("reaper: pending ttl %s < 15m (relay backoff exhaustion ~11.8m)", c.PendingTTL)
	case c.DispatchStale < 10*time.Minute:
		return fmt.Errorf("reaper: dispatch stale %s < 10m", c.DispatchStale)
	case c.AuditRetention < 24*time.Hour:
		return fmt.Errorf("reaper: audit retention %s < 24h", c.AuditRetention)
	case c.JanitorInterval < 5*time.Minute:
		return fmt.Errorf("reaper: janitor interval %s < 5m", c.JanitorInterval)
	case c.OutboxBatch < 1 || c.OutboxBatch > 10000:
		return fmt.Errorf("reaper: outbox batch %d out of [1,10000]", c.OutboxBatch)
	case !queueNameRe.MatchString(c.Queue):
		return fmt.Errorf("reaper: queue %q invalid (want %s)", c.Queue, queueNameRe)
	case len(c.ID) == 0 || len(c.ID) > 57:
		return fmt.Errorf("reaper: id %q empty or >57 chars (reaper: prefix must fit worker_id VARCHAR(64))", c.ID)
	}
	return nil
}

// ConfigFromEnv 从环境变量构造配置(BRAID_REAPER_ 前缀;未设键取默认)。
// getenv 注入 os.Getenv,测试换桩。ID 未设取 hostname(失败退 reaper-<pid>)。
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	c := DefaultConfig()
	var err error
	if c.ScanInterval, err = envDuration(getenv, "BRAID_REAPER_SCAN_INTERVAL", c.ScanInterval); err != nil {
		return c, err
	}
	if c.DeadGrace, err = envDuration(getenv, "BRAID_REAPER_DEAD_GRACE", c.DeadGrace); err != nil {
		return c, err
	}
	if c.ExecStale, err = envDuration(getenv, "BRAID_REAPER_EXEC_STALE", c.ExecStale); err != nil {
		return c, err
	}
	if c.PendingTTL, err = envDuration(getenv, "BRAID_REAPER_PENDING_TTL", c.PendingTTL); err != nil {
		return c, err
	}
	if c.DispatchStale, err = envDuration(getenv, "BRAID_REAPER_DISPATCH_STALE", c.DispatchStale); err != nil {
		return c, err
	}
	if c.AuditRetention, err = envDuration(getenv, "BRAID_REAPER_AUDIT_RETENTION", c.AuditRetention); err != nil {
		return c, err
	}
	if c.JanitorInterval, err = envDuration(getenv, "BRAID_REAPER_JANITOR_INTERVAL", c.JanitorInterval); err != nil {
		return c, err
	}
	if c.OutboxBatch, err = envInt(getenv, "BRAID_REAPER_OUTBOX_BATCH", c.OutboxBatch); err != nil {
		return c, err
	}
	if q := getenv("BRAID_REAPER_QUEUE"); q != "" {
		c.Queue = q
	}
	c.ID = getenv("BRAID_REAPER_ID")
	if c.ID == "" {
		if h, herr := os.Hostname(); herr == nil {
			c.ID = h
		} else {
			c.ID = fmt.Sprintf("reaper-%d", os.Getpid())
		}
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	if err := crossValidateWorkerTimings(getenv, c.DeadGrace); err != nil {
		return c, err
	}
	return c, nil
}

// crossValidateWorkerTimings 是 grace 公式的执行点强制(对抗攻击 3 吸收):
// BRAID_WORKER_RETRY_DELAY / BRAID_WORKER_HEARTBEAT_INTERVAL 与 reaper 同机
// 同 env 共享(compose 部署形态)——
//
//	两者均可读且 grace ≤ retry+hb  → 启动即败(公式被打破,接管窗口内必误判)
//	两者均可读且 grace < 3×(retry+hb) → warn(留观察)
//	不可读(独立 env 部署)      → 仅当 grace < 3×可读的 HeartbeatInterval 时 warn
//
// 键名与默认值(5s/10s)与 internal/worker/config.go 同步维护(reaper 不 import
// worker,依赖方向单向:reaper → {engine,scheduler,outbox,snowflake})。
func crossValidateWorkerTimings(getenv func(string) string, grace time.Duration) error {
	retry, retryOK := envDurOptional(getenv, "BRAID_WORKER_RETRY_DELAY")
	hb, hbOK := envDurOptional(getenv, "BRAID_WORKER_HEARTBEAT_INTERVAL")
	switch {
	case retryOK && hbOK:
		if grace <= retry+hb {
			return fmt.Errorf("reaper: dead grace %s <= worker retry+heartbeat %s (takeover staleness bound broken; raise BRAID_REAPER_DEAD_GRACE)",
				grace, retry+hb)
		}
		if grace < 3*(retry+hb) {
			slog.Warn("reaper: dead grace below 3x(retry+heartbeat) recommendation",
				"dead_grace", grace, "retry_delay", retry, "heartbeat_interval", hb)
		}
	case hbOK:
		if grace < 3*hb {
			slog.Warn("reaper: dead grace below 3x heartbeat",
				"dead_grace", grace, "heartbeat_interval", hb, "retry_delay", "unset")
		}
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
		return def, fmt.Errorf("reaper: %s=%q: %w", key, raw, err)
	}
	return d, nil
}

// envDurOptional:显式设置且可解析 → (值, true);未设置或坏值 → (0, false)
// (坏值不阻断:worker 侧自己会在解析时报错,此处只做保守提示)。
func envDurOptional(getenv func(string) string, key string) (time.Duration, bool) {
	raw := getenv(key)
	if raw == "" {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, false
	}
	return d, true
}

func envInt(getenv func(string) string, key string, def int) (int, error) {
	raw := getenv(key)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def, fmt.Errorf("reaper: %s=%q: %w", key, raw, err)
	}
	return n, nil
}
