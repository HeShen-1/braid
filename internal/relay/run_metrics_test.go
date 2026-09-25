package relay

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"braid/internal/metrics"
)

// TestDeliveryOutcomesCounted(T6.4):投递计数三分支——成功 dispatched、
// 退避 backoff、穷尽 dead。conflict 分支需要 Redis 内已有同 ID 任务,
// 由 e2e(投递幂等面)覆盖,此零栈面不造。
func TestDeliveryOutcomesCounted(t *testing.T) {
	t.Run("dispatched", func(t *testing.T) {
		f := newRFixture(t)
		id, _ := f.row("mq-" + fmt.Sprint(newID()))
		set, _ := metrics.NewSet()
		runRelayMetrics(t, f.db, &stubEnq{}, testCfg(), set.Relay, func() bool {
			status, _, _, _ := f.outboxRow(id)
			return status == "DISPATCHED"
		})
		if got := testutil.ToFloat64(set.Relay.Deliveries.WithLabelValues(metrics.DeliverDispatched)); got != 1 {
			t.Errorf("deliveries{dispatched} = %v, want 1", got)
		}
	})
	t.Run("dead", func(t *testing.T) {
		f := newRFixture(t)
		id, _ := f.row("mq-" + fmt.Sprint(newID()))
		set, _ := metrics.NewSet()
		cfg := testCfg()
		cfg.MaxAttempts = 1
		runRelayMetrics(t, f.db, &stubEnq{fn: func(int, string, []byte) error {
			return errors.New("redis: down (injected)")
		}}, cfg, set.Relay, func() bool {
			status, attempts, _, _ := f.outboxRow(id)
			return status == "DEAD" && attempts == 1
		})
		if got := testutil.ToFloat64(set.Relay.Deliveries.WithLabelValues(metrics.DeliverDead)); got != 1 {
			t.Errorf("deliveries{dead} = %v, want 1", got)
		}
		if got := testutil.ToFloat64(set.Relay.Deliveries.WithLabelValues(metrics.DeliverBackoff)); got != 0 {
			t.Errorf("deliveries{backoff} = %v, want 0(MaxAttempts=1 首败即 DEAD)", got)
		}
	})
	t.Run("backoff", func(t *testing.T) {
		f := newRFixture(t)
		id, _ := f.row("mq-" + fmt.Sprint(newID()))
		set, _ := metrics.NewSet()
		cfg := testCfg()
		cfg.BackoffBase = time.Hour
		cfg.BackoffMax = time.Hour
		runRelayMetrics(t, f.db, &stubEnq{fn: func(int, string, []byte) error {
			return errors.New("redis: down (injected)")
		}}, cfg, set.Relay, func() bool {
			status, attempts, _, _ := f.outboxRow(id)
			return status == "PENDING" && attempts == 1
		})
		if got := testutil.ToFloat64(set.Relay.Deliveries.WithLabelValues(metrics.DeliverBackoff)); got != 1 {
			t.Errorf("deliveries{backoff} = %v, want 1", got)
		}
	})
}

// runRelayMetrics 是 runRelay 的带指标变体(既有 10 用例签名不动)。
func runRelayMetrics(t *testing.T, db *sql.DB, eq Enqueuer, cfg Config, m *metrics.RelaySet, stop func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = Run(ctx, Deps{DB: db, Enqueuer: eq, Metrics: m}, cfg)
		close(done)
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		if stop() {
			cancel()
			<-done
			return
		}
		select {
		case <-deadline.C:
			cancel()
			<-done
			t.Fatalf("relay stop condition not met within 5s")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
