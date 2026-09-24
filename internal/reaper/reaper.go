package reaper

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"time"

	"braid/internal/audit"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
)

// Deps 是运行依赖。Sched 委托 ④b 终态补判(scheduler.FinishIfDone)与
// NodeTimeout 词表;Locker 为 nil = leaderless(每 tick 全跑,fail-open 的
// 静态形态,测试与降级部署用)。Logger 为 nil 用 slog.Default()。
type Deps struct {
	DB     *sql.DB
	Sched  *scheduler.Scheduler
	IDs    *snowflake.Generator // 重派事件行 id(应用侧 Snowflake)
	Locker Locker
	Logger *slog.Logger
}

// Reaper 是 reconciler 实例(无内部状态,字段在构造时定格)。
type Reaper struct {
	deps Deps
	cfg  Config
	log  *slog.Logger
}

// New 构造 reaper;依赖缺损响亮报错(不静默空转)。Validate 归启动路径
// (cmd/reaper),此处不重复。
func New(d Deps, cfg Config) (*Reaper, error) {
	if d.DB == nil || d.Sched == nil || d.IDs == nil {
		return nil, errors.New("reaper: nil DB, Sched or IDs")
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Reaper{deps: d, cfg: cfg, log: log}, nil
}

// Run 阻塞运行直到 ctx 取消:janitor 独立 goroutine(分区 DDL 永不冻结扫描
// 循环——攻击 8/12 吸收)+ audit flush 独立 goroutine(T6.1 暂存搬运,
// 秒级 tick 与 janitor 小时级分置)+ tick 循环(选主闸 → RunOnce → ±20%
// 抖动睡眠,抖动去同步多实例同拍扫描)。Redis 选主 fail-open:不可达 →
// warn 后照常执行本 tick(leaderless);正确性不依赖锁(处置事务锁后
// 复核 + CAS;flush 的 ON DUP 幂等使 leaderless 重叠搬运亦良性)。
func (r *Reaper) Run(ctx context.Context) error {
	jctx, jcancel := context.WithCancel(ctx)
	defer jcancel()
	jdone := make(chan struct{})
	go func() {
		defer close(jdone)
		r.janitorLoop(jctx)
	}()
	fdone := make(chan struct{})
	go func() {
		defer close(fdone)
		r.auditFlushLoop(jctx)
	}()

	token := fmt.Sprintf("%s:%d:%d", r.cfg.ID, os.Getpid(), time.Now().UnixNano())
	ttl := 2 * r.cfg.ScanInterval
	defer func() {
		if r.deps.Locker == nil {
			return
		}
		rctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := r.deps.Locker.Release(rctx, token); err != nil {
			r.log.Warn("leader.release_fail", "err", err)
		}
	}()

	for {
		if r.leaderGate(ctx, token, ttl) {
			if err := r.RunOnce(ctx); err != nil {
				r.log.Error("reap.once_failed", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			<-jdone
			<-fdone
			return nil
		case <-time.After(jittered(r.cfg.ScanInterval)):
		}
	}
}

// leaderGate:本 tick 是否由本实例执行。true = 持锁/续期成功/leaderless/
// 选主不可达(fail-open);false = 他人持锁。
func (r *Reaper) leaderGate(ctx context.Context, token string, ttl time.Duration) bool {
	if r.deps.Locker == nil {
		return true
	}
	held, err := r.deps.Locker.AcquireOrRenew(ctx, token, ttl)
	if err != nil {
		r.log.Warn("leader.unavailable", "err", err)
		return true
	}
	return held
}

// RunOnce 执行一轮全量扫描(导出供测试;不调 Validate——校验属启动路径,
// 测试经 Config 字面量注入毫秒级值)。四组扫描顺序执行、逐作业错误隔离:
// 单作业失败记日志不阻断其余(扫描面互相独立),首个错误仍上抛供调用方
// 观测(测试断言),Run 循环只记日志。
func (r *Reaper) RunOnce(ctx context.Context) error {
	jobs := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"scan_attempts", r.scanAttempts},   // ① attempt 判死 → reapAttempt
		{"scan_nodes", r.scanNodes},         // ② deadline/分支B + ③ 孤儿 → reapAttempt
		{"scan_execs", r.scanExecs},         // ④a PENDING 判死 + ④b 终态补判
		{"scan_approvals", r.scanApprovals}, // ⑤ WAITING_APPROVAL 超时/孤儿(T4.3)
	}
	var firstErr error
	for _, j := range jobs {
		if err := j.fn(ctx); err != nil {
			r.log.Error("reap.job_failed", "job", j.name, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("reaper: %s: %w", j.name, err)
			}
		}
	}
	return firstErr
}

// jittered 返回 ±20% 抖动后的间隔(去同步多实例扫描相位)。
func jittered(d time.Duration) time.Duration {
	j := int64(float64(d) * 0.2)
	return d - time.Duration(j) + time.Duration(rand.Int64N(2*j+1))
}

// auditFlushRoundCap 是单 tick 的搬运行数上限(有界 tick,余量下轮;
// 同 janitor outboxRoundCap 语义)。搬运逻辑本体在 audit.FlushAll
// (internal/audit,单点供测试与多消费方复用)。
const auditFlushRoundCap = 10000

// flushOnce 搬运一轮暂存审计行(audit_pending → audit_log,T6.1 异步批量
// 的消费半边)。毒行隔离与批量契约见 audit/flush.go;此处只驱动。
func (r *Reaper) flushOnce(ctx context.Context) error {
	n, err := audit.FlushAll(ctx, r.deps.DB, r.cfg.AuditFlushBatch, auditFlushRoundCap)
	if err != nil {
		return fmt.Errorf("reaper: audit flush: %w", err)
	}
	if n > 0 {
		r.log.Info("audit.flushed", "rows", n)
	}
	return nil
}

// auditFlushLoop 独立 goroutine:暂存审计行搬运(秒级 tick,与 janitor 的
// 小时级分区作业分置——可见性滞后上界语义见 Config.AuditFlushInterval)。
// 错误只记日志不退出(收敛性作业,下轮重试;与 janitorLoop 同形)。
func (r *Reaper) auditFlushLoop(ctx context.Context) {
	if err := r.flushOnce(ctx); err != nil {
		r.log.Error("audit.flush_round_failed", "err", err)
	}
	t := time.NewTicker(r.cfg.AuditFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.flushOnce(ctx); err != nil {
				r.log.Error("audit.flush_round_failed", "err", err)
			}
		}
	}
}
