package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Handler returns the exposition handler for a registry. The API process
// mounts it on its existing gin router next to /healthz; other entries use
// Serve for a dedicated listener.
func Handler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

// Serve starts a standalone metrics listener bound at addr (loopback by
// deployment convention); ctx done shuts it down with a bounded grace period.
// An empty addr disables the listener entirely (returns nil, false, nil) —
// metrics stay opt-in per process, mirroring the builtin-tools
// "unconfigured = not registered" posture. A listen failure returns an error
// (startup-adjacent: caller makes it fatal); a mid-flight serve failure only
// logs — observability loss must not kill the process it observes (review
// advisory: cross-goroutine panic was unclean shutdown by another name).
func Serve(ctx context.Context, addr string, reg *prometheus.Registry) (*http.Server, bool, error) {
	if addr == "" {
		return nil, false, nil
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", Handler(reg))
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, false, fmt.Errorf("metrics: listen %s: %w", addr, err)
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics: serve loop failed", "addr", addr, "err", err)
		}
	}()
	return srv, true, nil
}
