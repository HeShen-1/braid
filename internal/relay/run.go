// Package relay 实现 outbox 投递循环:T2.2 的消费者侧。relay 只写 outbox 表
// (DDL node_attempt.asynq_task_id 注释:「relay 只投不写业务表」),唯一的外部
// 依赖面是 Enqueuer 缝。
//
// 主循环 = 单事件单事务(锁窗 = 一次 Redis RTT,由 EnqueueTimeout 界住):
//
//	engine.WithTx(ctx, db, func(q) {
//	    SELECT … WHERE status='PENDING' AND available_at<=NOW(3)
//	      ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED   // 无行 → 空事务提交
//	    enqueue(TaskID, Queue, MaxRetry=1)             // Asynq 外部调用
//	    成功 | ErrTaskIDConflict → outbox.MarkDispatched
//	    失败                       → RecordFailure(退避)| MarkDead(穷尽)
//	})
//
// 正确性要点:
//   - SKIP LOCKED + 同事务标记 ⇒ 选择→标记原子:多 relay 天然分片,稳态零重复
//     入队(批事务是吞吐升级路径,须逐行记结果,N≤32);
//   - kill -9 ⇒ 连接断 ⇒ InnoDB 回滚未提交事务 ⇒ 行回 PENDING;若 enqueue 已
//     成功,重启重投撞确定性同 TaskID 的 ErrTaskIDConflict ⇒ 标 DISPATCHED——
//     这就是接受的 at-least-once 语义与验收②的靶窗;
//   - relay 事务只触 outbox,与 engine 锁序铁律(node→wf→attempt)零交集,
//     无 AB-BA 面;
//   - ErrTaskIDConflict 检测用 errors.Is(v0.26.0 client.go:416 以 %w 重包,
//     已对 pin 版源码核实)。冲突语义 = 该 ID 的任务记录存在于 Redis **任意**
//     状态(含 archived——滞留无保证上界,仅同队列惰性裁剪):记录存在 ⇒ 必有
//     成功入队发生过,冲突是**记录存活期内**的收敛加速,不是永久幂等锚——
//     任务完成且无 Retention 时记录即删,之后同 ID 重投会再次成功(仍是
//     at-least-once 契约内的重复);真正的执行级幂等锚 = worker 侧 attempt
//     唯一键(T2.3/T3.1)。安全前提:TaskID 词表全由不可变标识符构成
//     (见 outbox 包注释与 T2.3 验收断言);
//   - Asynq EnqueueContext 丢底层错误类型链(#1166,%v 非 %w)⇒ 失败归因不从
//     err 解包,一律用自家 ctx 状态判定;停机中断(ctx.Err()!=nil)不计尝试、
//     不动行,提交空事务释放锁。
package relay

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"braid/internal/engine"
	"braid/internal/metrics"
	"braid/internal/outbox"
)

// Deps 是运行依赖。Logger 为 nil 时用 slog.Default()。
type Deps struct {
	DB       *sql.DB
	Enqueuer Enqueuer
	Logger   *slog.Logger
	// Metrics 是观测面(T6.4):nil = 关闭(测试默认)。投递计数 + 周期
	// Redis 健康 gauge。
	Metrics *metrics.RelaySet
	// RDB 是队列 Redis 探测连接(INFO memory,内存水线告警字源);
	// nil = 不采 Redis 健康面(实例级零键空间,同 preflight 判)。
	RDB *redis.Client
	// Inspector 是 asynq 队列深度探测面(pending+active+retry = 消费
	// 债务);nil = 不采。观测专用,与投递路径无共享状态。
	Inspector *asynq.Inspector
}

// Run 阻塞运行投递循环直到 ctx 取消。运行期一切错误都是轮次级的(MySQL 断连
// → 记日志退避下一轮;Redis 断连 → 逐行退避/DEAD,长故障靠 T6.4 死信告警
// 兜底);返回非 nil 仅当依赖接线缺损。
func Run(ctx context.Context, d Deps, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if d.DB == nil || d.Enqueuer == nil {
		return errors.New("relay: nil DB or Enqueuer")
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	r := runner{deps: d, cfg: cfg, log: log, rnd: rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0))}
	if d.Metrics != nil && (d.RDB != nil || d.Inspector != nil) {
		// 探测 goroutine 生命周期与 Run 绑定(rev4 advisory):Run 返回前
		// 等它退出,cmd/relay 的 defer Close(Inspector/RDB)不与在飞探测
		// 重叠——否则停机窗口可能出现「已关连接上的最后一次探测」。
		probeDone := make(chan struct{})
		go func() {
			defer close(probeDone)
			r.probeQueueRedis(ctx)
		}()
		defer func() { <-probeDone }()
	}
	log.Info("relay.started",
		"idle_interval", cfg.IdleInterval, "enqueue_timeout", cfg.EnqueueTimeout,
		"max_attempts", cfg.MaxAttempts, "backoff_base", cfg.BackoffBase,
		"backoff_factor", cfg.BackoffFactor, "backoff_max", cfg.BackoffMax)
	defer log.Info("relay.stopped")
	for {
		if ctx.Err() != nil {
			return nil
		}
		hadRow, err := r.deliverOne(ctx)
		if err != nil {
			if ctx.Err() != nil { // 停机撞上在飞轮次:按停机处理(在飞事务已
				// 回滚,行留 PENDING 待下轮;零痕迹会让运维无从区分「干净停机」
				// 与「留了半截事件」,记一条)
				log.Warn("shutdown.interrupted", "err", err)
				return nil
			}
			log.Warn("loop.error", "err", err)
			if !sleepCtx(ctx, cfg.IdleInterval) {
				return nil
			}
			continue
		}
		if !hadRow && !sleepCtx(ctx, cfg.IdleInterval) {
			return nil
		}
		// 有行时立即下一轮:连续排空积压。
	}
}

type outboxRow struct {
	ID        uint64
	EventType string
	TaskID    string
	Queue     string
	Payload   []byte
	Attempts  int
	// trace_id 列本循环不读:OTel 链延续的传输形态(T6.2)未定,payload 自描述
	// 已足够投递;读取留待 T6.2 定案。
}

type runner struct {
	deps Deps
	cfg  Config
	log  *slog.Logger
	rnd  *rand.Rand
}

// deliverOne 处理至多一行:返回是否有行被处理(无论结局)。
func (r *runner) deliverOne(ctx context.Context) (bool, error) {
	var hadRow bool
	err := engine.WithTx(ctx, r.deps.DB, func(q engine.DBTX) error {
		var row outboxRow
		scanErr := q.QueryRowContext(ctx, `
SELECT id, event_type, task_id, queue, payload, deliver_attempts
  FROM outbox
 WHERE status='PENDING' AND available_at<=NOW(3)
 ORDER BY id
 LIMIT 1
 FOR UPDATE SKIP LOCKED`).Scan(
			&row.ID, &row.EventType, &row.TaskID, &row.Queue, &row.Payload, &row.Attempts)
		if errors.Is(scanErr, sql.ErrNoRows) {
			return nil // 空事务提交,锁零持有
		}
		if scanErr != nil {
			return fmt.Errorf("relay: poll outbox: %w", scanErr)
		}
		hadRow = true

		ectx, cancel := context.WithTimeout(ctx, r.cfg.EnqueueTimeout)
		defer cancel()
		// MaxRetry=1 是锁定决策。其 asynq 真实语义 = 预算 1:handler 首败后
		// asynq 自管**一次**内部重试(server 默认 15-45s 退避,故 T2.3 worker
		// 需显式决定 RetryDelayFunc 与平台重试的叠加),二次失败才 archived;
		// 此后重试权威在平台节点层。task type = event_type(开放词表,
		// T2.3 mux 按它路由)。
		_, enqErr := r.deps.Enqueuer.EnqueueContext(ectx,
			asynq.NewTask(row.EventType, row.Payload),
			asynq.TaskID(row.TaskID), asynq.Queue(row.Queue), asynq.MaxRetry(1))
		switch {
		case enqErr == nil:
			r.log.Info("enqueue.ok", "outbox_id", row.ID, "task_id", row.TaskID, "queue", row.Queue)
			r.mDeliver(metrics.DeliverDispatched)
		case errors.Is(enqErr, asynq.ErrTaskIDConflict):
			// 冲突收敛:任务已交到过(kill -9 后重投 / 多 relay / reconciler
			// 重入队),幂等证据而非失败。
			r.log.Info("enqueue.conflict", "outbox_id", row.ID, "task_id", row.TaskID)
			r.mDeliver(metrics.DeliverConflict)
		default:
			// 停机中断判定用自家 ctx(ectx 派生自 ctx),不解读包后的 asynq 错误:
			// 此刻行未获结局,不计尝试、不动行,空事务提交释放锁。
			if ctx.Err() != nil {
				return nil
			}
			msg := enqErr.Error()
			if row.Attempts+1 >= r.cfg.MaxAttempts {
				r.log.Error("row.dead", "outbox_id", row.ID, "task_id", row.TaskID,
					"event_type", row.EventType, "attempts", row.Attempts+1, "err", msg)
				r.mDeliver(metrics.DeliverDead)
				return outbox.MarkDead(ctx, q, row.ID, msg)
			}
			bo := r.backoff(row.Attempts + 1)
			r.log.Warn("enqueue.fail", "outbox_id", row.ID, "task_id", row.TaskID,
				"attempts", row.Attempts+1, "next_available_after", bo, "err", msg)
			r.mDeliver(metrics.DeliverBackoff)
			return outbox.RecordFailure(ctx, q, row.ID, msg, bo)
		}
		return outbox.MarkDispatched(ctx, q, row.ID)
	})
	return hadRow, err
}

// backoff 计算第 attempts 次失败后的等待时长(指数、封顶、±Jitter 抖动)。
func (r *runner) backoff(attempts int) time.Duration {
	b := float64(r.cfg.BackoffBase)
	for i := 1; i < attempts; i++ {
		b *= r.cfg.BackoffFactor
		if b >= float64(r.cfg.BackoffMax) {
			b = float64(r.cfg.BackoffMax)
			break
		}
	}
	if j := r.cfg.Jitter; j > 0 {
		b *= 1 + (r.rnd.Float64()*2-1)*j
	}
	if b < 0 {
		b = 0
	}
	return time.Duration(b)
}

// sleepCtx 睡 d 直到完成或 ctx 取消;返回 false = ctx 已取消。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// mDeliver 是投递结果计数的 nil-safe 发射(T6.4)。
func (r *runner) mDeliver(outcome string) {
	if r.deps.Metrics != nil {
		r.deps.Metrics.Deliveries.WithLabelValues(outcome).Inc()
	}
}

// redisProbeInterval 是队列 Redis 健康探测周期(观测粒度,非运维旋钮)。
const redisProbeInterval = 30 * time.Second

// probeRedis 周期采 INFO memory:used/max 是内存水线告警字源(ARCHITECTURE
// §2「队列实例水线告警」)。失败记日志继续(探测不可用 ≠ 投递不可用)。
func (r *runner) probeQueueRedis(ctx context.Context) {
	t := time.NewTicker(redisProbeInterval)
	defer t.Stop()
	probe := func() {
		if r.deps.RDB != nil {
			raw, err := r.deps.RDB.Info(ctx, "memory").Result()
			if err != nil {
				r.log.Warn("metrics.redis_info", "err", err)
			} else {
				var used, max int64
				for _, line := range strings.Split(raw, "\n") {
					line = strings.TrimSpace(line)
					switch {
					case strings.HasPrefix(line, "used_memory:"):
						if v, err := strconv.ParseInt(strings.TrimPrefix(line, "used_memory:"), 10, 64); err == nil {
							used = v
						}
					case strings.HasPrefix(line, "maxmemory:"):
						if v, err := strconv.ParseInt(strings.TrimPrefix(line, "maxmemory:"), 10, 64); err == nil {
							max = v
						}
					}
				}
				r.deps.Metrics.RedisMemoryUsed.Set(float64(used))
				r.deps.Metrics.RedisMemoryMax.Set(float64(max))
			}
		}
		if r.deps.Inspector != nil {
			queues, err := r.deps.Inspector.Queues()
			if err != nil {
				r.log.Warn("metrics.asynq_queues", "err", err)
			} else {
				for _, q := range queues {
					info, err := r.deps.Inspector.GetQueueInfo(q)
					if err != nil {
						r.log.Warn("metrics.asynq_queue", "queue", q, "err", err)
						continue
					}
					r.deps.Metrics.QueueTasks.WithLabelValues(q).
						Set(float64(info.Pending + info.Active + info.Retry))
				}
			}
		}
	}
	probe()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			probe()
		}
	}
}
