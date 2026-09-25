package metrics

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func handlerBody(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("handler status = %d, want 200", rec.Code)
	}
	b, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func TestServeDisabledOnEmptyAddr(t *testing.T) {
	_, reg := NewSet()
	srv, ok, err := Serve(context.Background(), "", reg)
	if ok || srv != nil || err != nil {
		t.Errorf("Serve with empty addr = (%v, %v, %v), want (nil, false, nil)", srv, ok, err)
	}
}

func TestServeExposesUntilContextCancel(t *testing.T) {
	_, reg := NewSet()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeLoopbackAddr(t)
	srv, ok, err := Serve(ctx, addr, reg)
	if err != nil || !ok || srv == nil {
		t.Fatalf("Serve = (%v, %v, %v), want started", srv, ok, err)
	}
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := http.Get("http://" + addr + "/metrics"); err != nil {
			return // listener closed: pass
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("listener still serving after ctx cancel")
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}
