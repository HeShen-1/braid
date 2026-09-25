package worker

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"braid/internal/metrics"
	"braid/internal/notify"
	"braid/internal/scheduler"
)

// TestNotifyOutcomesCounted(T6.4):notify 结果四分支各自落对应 counter 面
// ——sent / deduped / failed / degraded(nil Notifier)。零栈单测面经
// dispatchApprovalNotify(asynq 壳外的消费核心)。
func TestNotifyOutcomesCounted(t *testing.T) {
	boom := errors.New("smtp down")
	cases := []struct {
		name     string
		notifier ApprovalNotifier
		want     string
		wantErr  bool
	}{
		{"sent", &fakeNotifier{}, metrics.NotifyOutcomeSent, false},
		{"deduped", &fakeNotifier{fn: func(call notifyCall) error { return notify.ErrAlreadyNotified }}, metrics.NotifyOutcomeDeduped, false},
		{"failed", &fakeNotifier{fn: func(call notifyCall) error { return boom }}, metrics.NotifyOutcomeFailed, true},
		{"degraded", nil, metrics.NotifyOutcomeDegraded, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set, _ := metrics.NewSet()
			h := &attHandler{deps: Deps{Notifier: tc.notifier, Metrics: set.Worker}, log: slog.Default()}
			err := h.dispatchApprovalNotify(context.Background(), "notify:1",
				[]byte(`{"execution_id":11,"node_execution_id":12,"reason":"escalated"}`))
			if tc.wantErr && err == nil {
				t.Fatal("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			if got := testutil.ToFloat64(set.Worker.NotifyResults.WithLabelValues(tc.want)); got != 1 {
				t.Errorf("notify_results{%s} = %v, want 1", tc.want, got)
			}
		})
	}
	// 哨兵面:reason 词表闸拒绝路径不计数(发射面漂移不是通知结果)。
	set, _ := metrics.NewSet()
	h := &attHandler{deps: Deps{Notifier: &fakeNotifier{}, Metrics: set.Worker}, log: slog.Default()}
	if err := h.dispatchApprovalNotify(context.Background(), "notify:1",
		[]byte(`{"execution_id":11,"node_execution_id":12,"reason":"unknown"}`)); err == nil {
		t.Fatal("reason gate should reject")
	}
	for _, outcome := range []string{metrics.NotifyOutcomeSent, metrics.NotifyOutcomeDeduped,
		metrics.NotifyOutcomeFailed, metrics.NotifyOutcomeDegraded} {
		if got := testutil.ToFloat64(set.Worker.NotifyResults.WithLabelValues(outcome)); got != 0 {
			t.Errorf("gated path counted as %s: %v", outcome, got)
		}
	}
	_ = scheduler.ReasonApprovalEscalated
}
