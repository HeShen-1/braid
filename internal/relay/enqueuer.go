package relay

import (
	"context"

	"github.com/hibiken/asynq"
)

// Enqueuer 是 relay 对 Asynq 的全部行为依赖,签名与 asynq.Client.EnqueueContext
// 逐字对齐(v0.26.0 收敛面 = 本接口 + NewAsynqEnqueuer;升级 asynq 的 blast
// radius = 1 接口 + 1 适配器,编译器兜底)。
//
// 这是依赖倒置而非测试后门:①「outbox 行 → 队列消息」是传输决策,循环逻辑
// (轮询/标记/退避)不依赖它是 Asynq;②生产路径唯一,无旁路,fake 只存在于
// _test.go 与 kill -9 测试壳;③与本仓既有 idiom 同构——engine helper 收
// DBTX(*sql.DB 满足),relay 收 Enqueuer(AsynqEnqueuer 满足)。
//
// 约定(评审 checklist):asynq.Client 仅 relay 持有。第二个入队点会旁路
// MaxRetry=1 / 确定性 TaskID 的平台级约定。
type Enqueuer interface {
	EnqueueContext(ctx context.Context, task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error)
}

// AsynqEnqueuer 是生产实现:纯转发,零策略。
type AsynqEnqueuer struct {
	client *asynq.Client
}

// NewAsynqEnqueuer 构造生产 Enqueuer。
func NewAsynqEnqueuer(opt asynq.RedisClientOpt) *AsynqEnqueuer {
	return &AsynqEnqueuer{client: asynq.NewClient(opt)}
}

// EnqueueContext 转发 asynq.Client.EnqueueContext。
func (e *AsynqEnqueuer) EnqueueContext(ctx context.Context, t *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	return e.client.EnqueueContext(ctx, t, opts...)
}

// Close 释放底层客户端。
func (e *AsynqEnqueuer) Close() error { return e.client.Close() }
