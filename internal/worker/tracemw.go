package worker

// OTel 消费半边(T6.2 + D-2026-09-29-t121-otel-p1):asynq 任务 payload 自
// 描述携带 trace_id(+span_id;outbox 事件发射时塞入,relay 直投不感知),
// middleware 在 handler 之前 extract,经 ContextWithRemoteSpanContext 注入后
// 起段根 span(span 名 = task type)——当段链续接 payload 的 trace,下游
// executor / 调度事务 / audit 落行从同一 ctx 取值。
//
// span_id 到场即真 parent(D-t121 #3:段根 span 的 parent 从 P0 伪上游节点
// 变真发射 span);缺席 = legacy payload 滚动升级窗,派生回退;坏形态 =
// warn 留痕后派生(观察面不拒收业务任务)。
//
// 孤儿链口径短路(D-t121 #5):payload 无有效链上下文(trace 缺席/坏形态/
// 载荷非对象)→ 零注入、零 span、任务照常执行——P0「无 trace 仍自起段根
// span」废止,审计不再出现单行孤儿链 id(approval.notified 行回归 NULL
// 如实化,生产面唯一无 trace 任务 = approval.notify,发射面钉缺席)。

import (
	"context"
	"log/slog"

	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel/trace"

	"braid/internal/tracing"
)

// traceMiddleware 包裹每个任务:extract payload trace → remote parent 注入
// → 段根 span(span 名 = task type);无有效链上下文 → 短路零 span。
// Tracer nil = 观测关闭,直通。返回 asynq ServeMux.Use 的裸函数形状
// (func(Handler) Handler)。
func traceMiddleware(tracer trace.Tracer, log *slog.Logger) func(asynq.Handler) asynq.Handler {
	return func(next asynq.Handler) asynq.Handler {
		return asynq.HandlerFunc(func(ctx context.Context, t *asynq.Task) error {
			if tracer == nil {
				return next.ProcessTask(ctx, t)
			}
			pctx, linked := withTaskTrace(ctx, t, log)
			if !linked {
				// 孤儿链口径短路:零 span,不伪造链(观察面不拒收业务任务)。
				return next.ProcessTask(ctx, t)
			}
			ctx, span := tracer.Start(pctx, t.Type())
			defer span.End()
			return next.ProcessTask(ctx, t)
		})
	}
}

// withTaskTrace 把 payload 链上下文注入为 remote span context(契约单源
// internal/tracing.PayloadSpanContext:span_id 真值/派生回退/坏形态三臂
// 见其注释)。返回 (ctx, true) = 注入成功(调用方起段根 span);
// (ctx, false) = 无有效链上下文(调用方短路零 span)。
func withTaskTrace(ctx context.Context, t *asynq.Task, log *slog.Logger) (context.Context, bool) {
	p, _ := tracing.ExtractPayloadTrace(t.Payload())
	if p.TraceID == "" {
		return ctx, false
	}
	sc, ok, badSpan := tracing.PayloadSpanContext(p.TraceID, p.SpanID)
	if !ok {
		taskID, _ := asynq.GetTaskID(ctx)
		// 合法值恒 32 hex;截断防队列写者塞超长字符串膨胀单行日志
		//(体积面,无注入——slog 转义在;单源 tracing.TruncateID)。
		log.Warn("worker.trace_payload_invalid", "task_id", taskID, "trace_id", tracing.TruncateID(p.TraceID))
		return ctx, false
	}
	if badSpan {
		taskID, _ := asynq.GetTaskID(ctx)
		log.Warn("worker.span_payload_invalid", "task_id", taskID, "span_id", tracing.TruncateID(p.SpanID))
	}
	return trace.ContextWithRemoteSpanContext(ctx, sc), true
}
