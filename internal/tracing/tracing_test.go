package tracing

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/trace"
)

// Off 模式契约:span/trace id 照常生成,导出面零字节——audit trace_id 不随观测开关退化。
func TestOffProviderKeepsTraceIDAndExportsNothing(t *testing.T) {
	var buf bytes.Buffer
	p, err := NewProvider(Config{Exporter: ExporterOff, ServiceName: "test", Writer: &buf})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	ctx, span := p.Tracer("braid/test").Start(context.Background(), "op")
	defer span.End()

	sc := span.SpanContext()
	if !sc.IsValid() {
		t.Fatal("span context must stay valid under off exporter")
	}
	if got := trace.SpanContextFromContext(ctx); got.TraceID() != sc.TraceID() {
		t.Fatal("ctx must carry the started span's trace id")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("off exporter wrote %d bytes, want 0", buf.Len())
	}
}

// stdout 模式契约:导出的 JSON 携带生成侧的 trace id(导出的就是生成的)。
func TestStdoutProviderExportsStartedSpan(t *testing.T) {
	var buf bytes.Buffer
	p, err := NewProvider(Config{Exporter: ExporterStdout, ServiceName: "test", Writer: &buf})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	_, span := p.Tracer("braid/test").Start(context.Background(), "op")
	want := span.SpanContext().TraceID().String()
	span.End()

	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown must flush the batcher: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `"op"`) {
		t.Fatalf("exported output missing span name:\n%s", out)
	}
	if !strings.Contains(out, want) {
		t.Fatalf("exported output missing trace id %s:\n%s", want, out)
	}
}

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"off ok", Config{Exporter: ExporterOff}, false},
		{"stdout ok", Config{Exporter: ExporterStdout}, false},
		{"empty ok (zero value = off)", Config{}, false},
		{"jaeger ok (endpoint defaults)", Config{Exporter: ExporterJaeger}, false},
		{"jaeger http endpoint ok", Config{Exporter: ExporterJaeger, Endpoint: "http://jaeger:4318"}, false},
		{"jaeger https endpoint ok", Config{Exporter: ExporterJaeger, Endpoint: "https://collector:4318/v1/traces"}, false},
		{"jaeger bad scheme", Config{Exporter: ExporterJaeger, Endpoint: "ftp://jaeger:4318"}, true},
		{"jaeger no host", Config{Exporter: ExporterJaeger, Endpoint: "http://"}, true},
		{"jaeger no scheme", Config{Exporter: ExporterJaeger, Endpoint: "jaeger:4318"}, true},
		{"unknown rejected", Config{Exporter: "zipkin"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestConfigFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want Exporter
	}{
		{"unset defaults to off", nil, ExporterOff},
		{"empty defaults to off", map[string]string{"BRAID_TRACE_EXPORTER": ""}, ExporterOff},
		{"spaces trimmed", map[string]string{"BRAID_TRACE_EXPORTER": " stdout "}, ExporterStdout},
		{"stdout", map[string]string{"BRAID_TRACE_EXPORTER": "stdout"}, ExporterStdout},
		{"jaeger", map[string]string{"BRAID_TRACE_EXPORTER": "jaeger"}, ExporterJaeger},
		{"unknown passes through for Validate to reject", map[string]string{"BRAID_TRACE_EXPORTER": "otlp"}, Exporter("otlp")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ConfigFromEnv(func(k string) string { return tc.env[k] })
			if got.Exporter != tc.want {
				t.Fatalf("Exporter = %q, want %q", got.Exporter, tc.want)
			}
			if _, err := NewProvider(Config{Exporter: got.Exporter}); err == nil && tc.want == Exporter("otlp") {
				t.Fatal("NewProvider must reject unknown exporter")
			}
		})
	}
}

// jaeger 模式 env 面:endpoint 经 BRAID_TRACE_OTLP_ENDPOINT 读取,缺省留空由
// NewProvider 填默认(off/stdout 导出器对 endpoint 无感)。
func TestConfigFromEnvJaegerEndpoint(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"unset leaves empty (default filled by NewProvider)", nil, ""},
		{"endpoint read and trimmed", map[string]string{"BRAID_TRACE_OTLP_ENDPOINT": " http://jaeger:4318 "}, "http://jaeger:4318"},
		{"endpoint read even for off (ignored by exporter)", map[string]string{"BRAID_TRACE_EXPORTER": "off", "BRAID_TRACE_OTLP_ENDPOINT": "http://x:1"}, "http://x:1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ConfigFromEnv(func(k string) string { return tc.env[k] })
			if got.Endpoint != tc.want {
				t.Fatalf("Endpoint = %q, want %q", got.Endpoint, tc.want)
			}
		})
	}
}

// jaeger 模式契约(D-t121 #1:OTLP/HTTP → Jaeger OTLP 口):span 经真实 HTTP
// POST 抵达 endpoint 的 /v1/traces(protobuf 体非空,200 即收)。
func TestJaegerProviderExportsOverHTTP(t *testing.T) {
	var posts int
	var bodyLen int
	var gotPath, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		bodyLen = len(raw)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p, err := NewProvider(Config{Exporter: ExporterJaeger, Endpoint: srv.URL, ServiceName: "test"})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	_, span := p.Tracer("braid/test").Start(context.Background(), "op")
	want := span.SpanContext().TraceID().String()
	span.End()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown must flush the batcher: %v", err)
	}
	if posts != 1 {
		t.Fatalf("export POSTs = %d, want 1", posts)
	}
	if gotPath != "/v1/traces" {
		t.Fatalf("export path = %q, want /v1/traces", gotPath)
	}
	if !strings.HasPrefix(gotCT, "application/x-protobuf") {
		t.Fatalf("export content-type = %q, want application/x-protobuf*", gotCT)
	}
	if bodyLen == 0 {
		t.Fatal("export body empty, want protobuf span batch")
	}
	if want == "" {
		t.Fatal("trace id must exist for round-trip sanity")
	}
}

// 默认 writer = os.Stdout 的构造路径必须可用(stdout 生产形态)。
func TestStdoutDefaultsToOSStdout(t *testing.T) {
	p, err := NewProvider(Config{Exporter: ExporterStdout, ServiceName: "test"})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

var _ = os.Stdout // keep os import aligned with production default path

// D-t121 #3 payload 链上下文契约(单源):字段提取 + SpanContext 构造——
// 真值/派生回退/坏形态/无链上下量四臂。
func TestPayloadSpanContext(t *testing.T) {
	const (
		traceOK = "4bf92f3577b34da6a3ce929d0e0e4736"
		spanOK  = "00f067aa0ba902b7"
	)
	derived := traceOK[:16] // D-t62 #2:trace id 前 8 字节(16 hex 字符)派生
	// 字段提取。
	p, ok := ExtractPayloadTrace([]byte(`{"trace_id":"` + traceOK + `","span_id":"` + spanOK + `"}`))
	if !ok || p.TraceID != traceOK || p.SpanID != spanOK {
		t.Fatalf("ExtractPayloadTrace = %+v ok=%v", p, ok)
	}
	if _, ok := ExtractPayloadTrace([]byte(`[1,2,3]`)); ok {
		t.Fatal("non-object payload must extract !ok")
	}
	if p, ok := ExtractPayloadTrace([]byte(`{"execution_id":1}`)); !ok || p.TraceID != "" {
		t.Fatalf("absent trace: %+v ok=%v, want zero value + ok", p, ok)
	}

	cases := []struct {
		name        string
		trace, span string
		wantOK      bool
		wantSpan    string // parent span id
		wantBadSpan bool
	}{
		{"real span id", traceOK, spanOK, true, spanOK, false},
		{"legacy absent span id", traceOK, "", true, derived, false},
		{"malformed span id", traceOK, "NOTHEX", true, derived, true},
		{"wrong length span id", traceOK, "0123456789abcdef0123", true, derived, true},
		{"no trace", "", spanOK, false, "", false},
		{"malformed trace", "NOTHEXATALL", "", false, "", false},
		{"wrong length trace", "0123456789abcdef", "", false, "", false},
		{"all-zero trace (valid hex, invalid id)", "00000000000000000000000000000000", "", false, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, ok, bad := PayloadSpanContext(tc.trace, tc.span)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if bad != tc.wantBadSpan {
				t.Fatalf("badSpanID = %v, want %v", bad, tc.wantBadSpan)
			}
			if !tc.wantOK {
				return
			}
			if !sc.IsValid() || !sc.IsRemote() {
				t.Fatalf("span context = %v, want valid remote parent", sc)
			}
			if got := sc.TraceID().String(); got != tc.trace {
				t.Fatalf("trace = %s, want %s", got, tc.trace)
			}
			if got := sc.SpanID().String(); got != tc.wantSpan {
				t.Fatalf("parent span = %s, want %s", got, tc.wantSpan)
			}
		})
	}
}

// 零前缀 trace id 派生全零 span id / span_id 字面全零:无效 remote parent
// 必须走短路臂(review 二轮修:每个被接受的 id 必真续链)。字面全零
// span_id 被 SpanIDFromHex 拒 → 派生回退 + warn(链仍续,非孤儿)。
// TruncateID 的 rune 边界:多字节值不在字节中间切断。
func TestPayloadSpanContextZeroIDArms(t *testing.T) {
	const traceOK = "4bf92f3577b34da6a3ce929d0e0e4736"
	derived := traceOK[:16] // D-t62 #2 派生形态
	cases := []struct {
		name        string
		trace, span string
		wantOK      bool
		wantSpan    string
	}{
		{"zero-prefix trace derives zero span id", "0000000000000000aaaaaaaaaaaaaaaa", "", false, ""},
		{"all-zero literal span id falls back derived", traceOK, "0000000000000000", true, derived},
		{"real values still link", traceOK, "00f067aa0ba902b7", true, "00f067aa0ba902b7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, ok, _ := PayloadSpanContext(tc.trace, tc.span)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok {
				if !sc.IsValid() {
					t.Fatalf("accepted id assembled invalid span context %v", sc)
				}
				if got := sc.SpanID().String(); got != tc.wantSpan {
					t.Fatalf("span = %s, want %s", got, tc.wantSpan)
				}
			}
		})
	}
	if got := TruncateID(string([]byte{0x61, 0xe4, 0xb8, 0xad, 0xe6, 0x96, 0x87}) + strings.Repeat("x", 80)); !utf8.ValidString(got) {
		t.Fatalf("TruncateID produced invalid UTF-8: %q", got)
	}
	if got := TruncateID(strings.Repeat("a", 100)); len(got) != 64 {
		t.Fatalf("TruncateID(ascii) len = %d, want 64", len(got))
	}
}

// https→TLS 臂(review MEDIUM 修):https endpoint 下首字节必须是 TLS
// ClientHello(0x16)——无条件 WithInsecure 变异下首字节是 ASCII 方法名,红。
// 裸 listener 不应答握手,exporter 报错属预期;Shutdown 带超时防挂。
func TestJaegerExporterHTTPSUsesTLS(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }() //nolint:errcheck // 测试收尾,关闭错误无处置义
	firstByte := make(chan byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 1)
		_, _ = conn.Read(buf)
		firstByte <- buf[0]
		_ = conn.Close() // 不应答握手:exporter 快速失败, Shutdown 超时兜底
	}()
	p, err := NewProvider(Config{
		Exporter: ExporterJaeger, ServiceName: "test",
		Endpoint: "https://" + ln.Addr().String(),
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	_, span := p.Tracer("braid/test").Start(context.Background(), "op")
	span.End()
	shCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = p.Shutdown(shCtx) // 裸 listener 必然握手失败:错误非本臂断言面
	select {
	case b := <-firstByte:
		if b != 0x16 {
			t.Fatalf("first byte = %#x, want 0x16 (TLS ClientHello) — insecure leak on https endpoint", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no connection arrived at https endpoint")
	}
}
