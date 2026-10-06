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

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"braid/internal/audit"
	"braid/internal/metrics"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
)

// Deps 是运行依赖。Sched 委托 ④b 终态补判(scheduler.FinishIfDone)与
// NodeTimeout 词表;Locker 为 nil = leaderless(每 tick 全跑,fail-open 的
// 静态形态,测试与降级部署用)。Logger 为 nil 用 slog.Default()。
type Deps struct {
	DB    *sql.DB
	Sched *scheduler.Scheduler
	IDs   *snowflake.Generator // 重派事件行 id(应用侧 Snowflake)
	// Lease 是本进程的节点租约(T20.1 fence 面,D-2026-10-06-t201):reaper
	// 是裁决权威,其合法性 = 自身租约行加冕——reapOnce 处置事务首置加冕
	// 校验,zombie reaper 不得 stomping(裁决写不需要 worker token,权威的
	// 合法性同源同判)。nil = legacy 无 fence(滚动窗/测试装配)。
	Lease  *snowflake.Lease
	Locker Locker
	Logger *slog.Logger
	// Metrics 是观测面(T6.4):nil = 关闭(测试默认)。gauge 采集器也在
	// 其上(collect.go 每 tick SQL 快照)。
	Metrics *metrics.ReaperSet
	// AsynqProbe 对拍 asynq 任务活性(T17.4,D-2026-10-04-t174):分支 B
	// 重派前的 Redis 侧证据复核——死证据只剩陈旧 DISPATCHED 行时,先确认
	// 任务是否仍活在 asynq(积压 ≠ 死)。nil = 关闭(D-t25#10 有界残余窗
	// 语义照旧,测试缺省)。实现持 Inspector,装配在 cmd/reaper 组合根,
	// 本包不 import asynq。
	AsynqProbe AsynqProbe
	// Tracer 是 OTel 观测面(D-t121 #4):nil = 关闭(测试默认)。tick 根
	// span + 每作业子 span + flush/janitor span——**span 恒不进发射 ctx**
	// (作业函数收调用方原始 ctx):reaper 自生审计行保持 D-t62 如实化
	// NULL 口径,notify 事件 outbox 行列值/payload 双写原则不被 tick trace
	// 撬动;与 relay 的 span 入 ctx 形态分立,理由见 D- 条目。
	Tracer trace.Tracer
}

// AsynqProbe 是 asynq 活性对拍缝(T17.4,D-2026-10-04-t174):TaskAlive 报告
// taskID 在 queue 是否仍处活态(pending/active/retry/scheduled/aggregating)。
// false + nil error = 非活(NotFound/archived/completed);非 nil error =
// 探测不可达——调用方 fail-open 回落 DispatchStale 有界窗(D-t25#2 同族)。
// ctx 由调用方透传(探测在处置事务外,不持行锁);asynq Inspector 自身
// 无 ctx 面,实现可忽略。
type AsynqProbe interface {
	TaskAlive(ctx context.Context, queue, taskID string) (bool, error)
}

// Reaper 是 reconciler 实例(无内部状态,字段在构造时定格)。
type Reaper struct {
	deps Deps
	cfg  Config
	log  *slog.Logger
	m    *metrics.ReaperSet
	// leaderToken/scanLeaseTTL 由 Run 铸造(先于三 goroutine 启动,无竞态):
	// 同一实例身份 token 三键共用,键面区分角色(T15.6 定档 A)。
	leaderToken  string
	scanLeaseTTL time.Duration
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
	return &Reaper{deps: d, cfg: cfg, log: log, m: d.Metrics}, nil
}

// Run 阻塞运行直到 ctx 取消:janitor 独立 goroutine(分区 DDL 永不冻结扫描
// 循环——攻击 8/12 吸收)+ audit flush 独立 goroutine(T6.1 暂存搬运,
// 秒级 tick 与 janitor 小时级分置)+ tick 循环(选主闸 → RunOnce → ±20%
// 抖动睡眠,抖动去同步多实例同拍扫描)。Redis 选主 fail-open:不可达 →
// warn 后照常执行本 tick(leaderless);正确性不依赖锁(处置事务锁后
// 复核 + CAS;flush 的预过滤 + 裸 INSERT 使 leaderless 重叠搬运收敛
// ——1062 整批回滚 + 下轮预过滤核销,见 audit/flush.go)。
func (r *Reaper) Run(ctx context.Context) error {
	r.leaderToken = fmt.Sprintf("%s:%d:%d", r.cfg.ID, os.Getpid(), time.Now().UnixNano())
	r.scanLeaseTTL = 2 * r.cfg.ScanInterval
	jctx, jcancel := context.WithCancel(ctx)
	defer jcancel()
	jdone := make(chan struct{})
	go func() {
		defer close(jdone)
		r.janitorLoop(jctx, r.leaderToken)
	}()
	fdone := make(chan struct{})
	go func() {
		defer close(fdone)
		r.auditFlushLoop(jctx, r.leaderToken)
	}()

	defer func() {
		if r.deps.Locker == nil {
			return
		}
		rctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// 三键全释放(正常停机加速接管;漏放 = TTL 过期兜底,语义同)。
		for _, key := range []string{leaderKey, leaderKeyFlush, leaderKeyJanitor} {
			if err := r.deps.Locker.Release(rctx, key, r.leaderToken); err != nil {
				r.log.Warn("leader.release_fail", "err", err, "key", key)
			}
		}
	}()

	for {
		if r.leaderGate(ctx, leaderKey, r.leaderToken, r.scanLeaseTTL) {
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
// 选主不可达(fail-open);false = 他人持锁。key 选锁(三键,T15.6 定档 A)。
func (r *Reaper) leaderGate(ctx context.Context, key, token string, ttl time.Duration) bool {
	if r.deps.Locker == nil {
		return true
	}
	held, err := r.deps.Locker.AcquireOrRenew(ctx, key, token, ttl)
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
//
// span 形态(D-t121 #4):Tracer 非 nil 时包 tick 根 span + 每作业子 span
// (reaper.<job>);span 层级经独立 span ctx 链维系,**作业函数恒收调用方
// 原始 ctx**——audit/outbox 的 applyCtxTrace 读不到 tick trace,reaper
// 自生行保持 NULL 如实化口径(faultinject e2e「非空 trace 恒 X」钉死)。
func (r *Reaper) RunOnce(ctx context.Context) error {
	if r.deps.Tracer == nil {
		return r.runJobs(ctx, nil)
	}
	// 返回的 span ctx 只作 job span 的层级 parent,不进任何作业调用。
	// 错误记录在 End 之前(SDK span End 后 IsRecording=false,RecordError/
	// SetStatus 均为静默 no-op——review 修:先记错后结束,与 job/flush/
	// janitor 包装同序)。
	tctx, tick := r.deps.Tracer.Start(ctx, "reaper.tick")
	defer tick.End()
	err := r.runJobs(ctx, tctx)
	if err != nil {
		tick.RecordError(err)
		tick.SetStatus(codes.Error, err.Error())
	}
	return err
}

// runJobs 逐作业执行。spanParent nil = 无观测直通;非 nil = 每作业在其
// 下开 reaper.<name> 子 span(错误入 span),作业本身仍收原始 ctx。
func (r *Reaper) runJobs(ctx, spanParent context.Context) error {
	jobs := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"scan_attempts", r.scanAttempts},          // ① attempt 判死 → reapAttempt
		{"scan_nodes", r.scanNodes},                // ② deadline/分支B + ③ 孤儿 → reapAttempt
		{"scan_execs", r.scanExecs},                // ④a PENDING 判死 + ④b 终态补判
		{"scan_approvals", r.scanApprovals},        // ⑤ WAITING_APPROVAL 超时/孤儿(T4.3)
		{"collect_metrics", r.collectMetrics},      // T6.4 gauge 快照(只读,错误隔离同上)
		{"verify_audit_chain", r.verifyAuditChain}, // T9.3 链校验第七作业(断链=结局不报错,见 verifyjob.go)
	}
	var firstErr error
	for _, j := range jobs {
		jerr := func() error {
			if spanParent == nil {
				return j.fn(ctx)
			}
			_, span := r.deps.Tracer.Start(spanParent, "reaper."+j.name)
			defer span.End()
			err := j.fn(ctx) // 原始 ctx:span 不充当发射上下文(D-t121 #4)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			return err
		}()
		if jerr != nil {
			r.log.Error("reap.job_failed", "job", j.name, "err", jerr)
			if firstErr == nil {
				firstErr = fmt.Errorf("reaper: %s: %w", j.name, jerr)
			}
		}
	}
	return firstErr
}

// nil-safe 发射 helper(T6.4):Deps.Metrics nil = 观测关闭,发射点免判空。
func (r *Reaper) mReaped(code string) {
	if r.m != nil {
		r.m.Reaped.WithLabelValues(code).Inc()
	}
}

func (r *Reaper) mRedispatched(trigger string) {
	if r.m != nil {
		r.m.Redispatches.WithLabelValues(trigger).Inc()
	}
}

func (r *Reaper) mNodeFailed() {
	if r.m != nil {
		r.m.NodeFailures.Inc()
	}
}

func (r *Reaper) mNodeCancelled() {
	if r.m != nil {
		r.m.NodeCancellations.Inc()
	}
}

func (r *Reaper) mApprovalEscalated() {
	if r.m != nil {
		r.m.ApprovalsEscalated.Inc()
	}
}

func (r *Reaper) mBacklog(scan string) {
	if r.m != nil {
		r.m.ScanBacklog.WithLabelValues(scan).Inc()
	}
}

// mPoisonSnapshot 记一次快照漂移拒绝(T17.4):行保持 WAITING_APPROVAL 且
// 每 tick 重试,counter 是运维信号(镜像 audit flush poison 判据)。
func (r *Reaper) mPoisonSnapshot(reason string) {
	if r.m != nil {
		r.m.PoisonSnapshots.WithLabelValues(reason).Inc()
	}
}

// mAsynqAliveDefer 记一次分支 B 重派因 asynq 活性让位(T17.4,
// D-2026-10-04-t174:Inspector 对拍关闭一个 D-t25#10 残余窗)。
func (r *Reaper) mAsynqAliveDefer() {
	if r.m != nil {
		r.m.AsynqAliveDefers.Inc()
	}
}

// mAsynqProbeUnreachable 记一次分支 B 探测不可达 fail-open(T18.7,
// D-t174 已知边界收口:该路径原仅 warn 无计数,静默回落不可观测)。
func (r *Reaper) mAsynqProbeUnreachable() {
	if r.m != nil {
		r.m.AsynqProbeUnreachable.Inc()
	}
}

// jittered 返回 ±20% 抖动后的间隔(去同步多实例扫描相位)。
func jittered(d time.Duration) time.Duration {
	j := int64(float64(d) * 0.2)
	return d - time.Duration(j) + time.Duration(rand.Int64N(2*j+1))
}

// auditFlushRoundCap 是单 tick 的搬运行数上限(有界 tick,余量下轮;
// 同 janitor janitorRoundCap 语义)。搬运逻辑本体在 audit.FlushAll
// (internal/audit,单点供测试与多消费方复用)。
const auditFlushRoundCap = 10000

// flushOnce 搬运一轮暂存审计行(audit_pending → audit_log,T6.1 异步批量
// 的消费半边)。毒行隔离与批量契约见 audit/flush.go;此处只驱动。span 形态
// 同 RunOnce(D-t121 #4:计时 span 不进发射 ctx,搬运行 trace 保持原值)。
func (r *Reaper) flushOnce(ctx context.Context) error {
	if r.deps.Tracer == nil {
		return r.doFlush(ctx)
	}
	_, span := r.deps.Tracer.Start(ctx, "reaper.audit_flush")
	defer span.End()
	err := r.doFlush(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

func (r *Reaper) doFlush(ctx context.Context) error {
	start := time.Now()
	agg, err := audit.FlushAll(ctx, r.deps.DB, r.cfg.AuditFlushBatch, auditFlushRoundCap)
	if r.m != nil {
		// 计数语义(对抗审计 A6):成功批逐批计数;失败轮只记一次
		// rollback(部分成功轮的已完成批已入 agg.Batches——被回滚,与
		// 行计数的差值即回滚量,可观测)。毒行计数是「在场信号」语义:
		// 毒行每 tick 重扫重计 = 告警持续,直到人工清理(登记 D-t64)。
		if err != nil {
			r.m.FlushBatches.WithLabelValues(metrics.FlushRollback).Inc()
		} else {
			r.m.FlushBatches.WithLabelValues(metrics.FlushOK).Add(float64(agg.Batches))
		}
		r.m.FlushRows.WithLabelValues(metrics.FlushKindFlushed).Add(float64(agg.Flushed))
		r.m.FlushRows.WithLabelValues(metrics.FlushKindReplayed).Add(float64(agg.Replayed))
		r.m.FlushPoison.Add(float64(agg.Skipped))
		r.m.FlushDuration.Observe(time.Since(start).Seconds())
	}
	if err != nil {
		return fmt.Errorf("reaper: audit flush: %w", err)
	}
	if agg.Flushed > 0 {
		r.log.Info("audit.flushed", "rows", agg.Flushed)
	}
	return nil
}

// auditFlushLoop 独立 goroutine:暂存审计行搬运(秒级 tick,与 janitor 的
// 小时级分区作业分置——可见性滞后上界语义见 Config.AuditFlushInterval)。
// 错误只记日志不退出(收敛性作业,下轮重试;与 janitorLoop 同形)。
// 逐轮过自己的 leader 键(T15.6 定档 A,见 gatedRound)。
func (r *Reaper) auditFlushLoop(ctx context.Context, token string) {
	r.gatedRound(ctx, leaderKeyFlush, token, 2*r.cfg.AuditFlushInterval, "audit.flush",
		r.flushOnce)
	t := time.NewTicker(r.cfg.AuditFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.gatedRound(ctx, leaderKeyFlush, token, 2*r.cfg.AuditFlushInterval, "audit.flush",
				r.flushOnce)
		}
	}
}

// gatedRound 是 T15.6 定档 A 的逐轮闸形态(D-t151 决策 7,随演练定档):
// leader 键放行才执行本轮,他人持键整轮跳过——N 副本稳态单跑,消每批
// N-1 次空转(读批 + 预过滤 + PK 锁阻塞的裸 INSERT + 1062 整批回滚)与
// ERROR 日志 N× 放大;fail-open 保留(选主不可达 → 照跑,正确性由收敛
// 结构承载,D-t61)。TTL = 2× 所属循环间隔(与扫描锁同 idiom);leader
// 死亡后接管 ≤ TTL(janitor 2h 上界:分区前滚有 7 天缓冲,良性登记)。
func (r *Reaper) gatedRound(ctx context.Context, key, token string, ttl time.Duration, job string, round func(context.Context) error) {
	if !r.leaderGate(ctx, key, token, ttl) {
		return
	}
	if err := round(ctx); err != nil {
		r.log.Error(job+".round_failed", "err", err)
	}
}
