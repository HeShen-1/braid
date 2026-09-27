package worker

// T6.2 消费半边:asynq 任务 payload 自描述携带 trace_id(outbox 事件发射
// 时塞入,relay 直投不感知),middleware 在 handler 之前 extract,经
// ContextWithRemoteSpanContext 注入后起段根 span——当段链续接 payload 的
// trace(trace id 不变),下游 executor / 调度事务 / audit 落行从同一 ctx
// 取值(D-2026-09-27-t62-otel-trace)。
//
// trace 传播是观测面不是业务面:payload 无 trace / 形态坏 = 零注入照常执行
// (形态坏 slog warn 留痕),绝不拒收业务任务。

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel/trace"
)

// taskTraceProbe 是 payload 的 trace 探针形状:四类事件 payload 都以
// trace_id 键携带(omitempty 缺席 = 无 trace 上下文发射)。单字段解析不做
// 全量反序列化——payload 其余字段归各 handler 的严格 Unmarshal。
type taskTraceProbe struct {
	TraceID string `json:"trace_id"`
}

// traceMiddleware 包裹每个任务:extract payload trace → remote parent 注入
// → 段根 span(span 名 = task type)。Tracer nil = 观测关闭,直通。
// 返回 asynq ServeMux.Use 的裸函数形状(func(Handler) Handler)。
func traceMiddleware(tracer trace.Tracer, log *slog.Logger) func(asynq.Handler) asynq.Handler {
	return func(next asynq.Handler) asynq.Handler {
		return asynq.HandlerFunc(func(ctx context.Context, t *asynq.Task) error {
			if tracer == nil {
				return next.ProcessTask(ctx, t)
			}
			ctx = withTaskTrace(ctx, t, log)
			ctx, span := tracer.Start(ctx, t.Type())
			defer span.End()
			return next.ProcessTask(ctx, t)
		})
	}
}

// withTaskTrace 把 payload trace_id 注入为 remote span context。
//
// SpanID 自 trace id 前 8 字节确定性派生:SDK 判定 remote parent 有效需要
// 非零 SpanID,而 P0 传播协议只携 trace_id(审计/查询面的唯一契约值)。
// 伪 parent span id 在 P1 协议携带真 span id 时消解;off/stdout 导出面下
// 无实害(导出的 span 树里它只是一个不存在的上游节点)。
func withTaskTrace(ctx context.Context, t *asynq.Task, log *slog.Logger) context.Context {
	var probe taskTraceProbe
	if err := json.Unmarshal(t.Payload(), &probe); err != nil {
		return ctx // 载荷非对象/坏 JSON:消费面各自响亮处理,trace 面零注入降级
	}
	if probe.TraceID == "" {
		return ctx
	}
	tid, err := trace.TraceIDFromHex(probe.TraceID)
	if err != nil {
		taskID, _ := asynq.GetTaskID(ctx)
		// 合法值恒 32 hex;截断防队列写者塞超长字符串膨胀单行日志
		//(review LOW:体积面,无注入——slog 转义在)。
		bad := probe.TraceID
		if len(bad) > 64 {
			bad = bad[:64]
		}
		log.Warn("worker.trace_payload_invalid", "task_id", taskID, "trace_id", bad)
		return ctx
	}
	var sid [8]byte
	copy(sid[:], tid[:8])
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid, SpanID: trace.SpanID(sid), TraceFlags: trace.FlagsSampled,
		Remote: true, // 传播边界上的 parent 是 remote(review LOW:规范位)
	})
	return trace.ContextWithRemoteSpanContext(ctx, sc)
}
