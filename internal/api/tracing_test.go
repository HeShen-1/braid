package api

// T6.2 traceSpan middleware 单元面:Tracing 注入后 request ctx 携带有效
// trace id(触发/决策两路生产点的传播载体);Tracing(nil) 零侵入(请求
// ctx 无 trace id,audit 落 NULL——DDL「内部测试路径允许 NULL」口径)。
// 落库断言(execution.trace_id / triggered 行 / 决策行 linked)在 nodes
// e2e 真链层覆盖(片 5)。

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"

	"braid/internal/tracing"
)

func TestTraceSpanInjectsTraceID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	prov, err := tracing.NewProvider(tracing.Config{Exporter: tracing.ExporterOff, ServiceName: "test"})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	t.Cleanup(func() { _ = prov.Shutdown(t.Context()) })

	var got, gotNil trace.SpanContext
	s := &Server{}
	s.Tracing(prov.Tracer("braid/api"))

	r := gin.New()
	r.Group("/api/v1", s.traceSpan()).GET("/probe", func(c *gin.Context) {
		got = trace.SpanContextFromContext(c.Request.Context())
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/probe", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !got.HasTraceID() {
		t.Fatal("authed request ctx must carry a trace id after Tracing injection")
	}

	// nil 注入臂:零侵入(行为不变锚)。
	sNil := &Server{}
	sNil.Tracing(nil)
	r2 := gin.New()
	r2.Group("/api/v1", sNil.traceSpan()).GET("/probe", func(c *gin.Context) {
		gotNil = trace.SpanContextFromContext(c.Request.Context())
	})
	w2 := httptest.NewRecorder()
	r2.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/api/v1/probe", nil))
	if gotNil.HasTraceID() {
		t.Fatal("Tracing(nil) must leave request ctx free of trace ids")
	}
}
