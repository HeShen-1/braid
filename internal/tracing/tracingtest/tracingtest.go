// Package tracingtest 提供直读导出面的测试 tracer 工具(跨包测试共享,
// 镜像 internal/redistest、internal/db/sqltest 先例):parent span id /
// attributes 等断言需要 ReadOnlySpan 面,生产导出器(off/stdout/jaeger)
// 不回读。
package tracingtest

import (
	"context"
	"slices"
	"sync"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Recorder 记录全部已结束 span(并发安全;Syncer 导出 = span End 即达,
// 无批处理时延)。
type Recorder struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

// New 构造同步导出的 provider + 记录器;调用方负责 provider 生命周期
// (Shutdown 归 t.Cleanup 或显式)。
func New() (*sdktrace.TracerProvider, *Recorder) {
	rec := &Recorder{}
	return sdktrace.NewTracerProvider(sdktrace.WithSyncer(rec)), rec
}

// ExportSpans 实现 sdktrace.SpanExporter(同步追加)。
func (r *Recorder) ExportSpans(_ context.Context, ss []sdktrace.ReadOnlySpan) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, ss...)
	return nil
}

// Shutdown 实现 sdktrace.SpanExporter。
func (r *Recorder) Shutdown(context.Context) error { return nil }

// Snapshot 返回已捕获 span 的拷贝(调用方持有后不受后续导出影响)。
func (r *Recorder) Snapshot() []sdktrace.ReadOnlySpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.spans)
}

// Names 是 span 名列表(集合覆盖断言的便捷面)。
func (r *Recorder) Names() []string {
	spans := r.Snapshot()
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.Name())
	}
	return out
}
