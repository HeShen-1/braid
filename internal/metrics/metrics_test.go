package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// metricNames anchors the exported metric vocabulary: every metric the Set
// registers must appear here, and every name here must be registered. This is
// the metric-name closed set, mirroring the audit knownEvents precedent
// (internal/audit/writer.go) — typo drift fails this test, not production.
var metricNames = map[string]dto.MetricType{
	"braid_llm_http_failures_total":             dto.MetricType_COUNTER,
	"braid_llm_calls_total":                     dto.MetricType_COUNTER,
	"braid_llm_call_duration_seconds":           dto.MetricType_HISTOGRAM,
	"braid_llm_calls_discarded_total":           dto.MetricType_COUNTER,
	"braid_reaper_reaped_total":                 dto.MetricType_COUNTER,
	"braid_reaper_redispatches_total":           dto.MetricType_COUNTER,
	"braid_reaper_node_failures_total":          dto.MetricType_COUNTER,
	"braid_reaper_node_cancellations_total":     dto.MetricType_COUNTER,
	"braid_reaper_approvals_escalated_total":    dto.MetricType_COUNTER,
	"braid_reaper_scan_backlog_total":           dto.MetricType_COUNTER,
	"braid_audit_flush_batches_total":           dto.MetricType_COUNTER,
	"braid_audit_flush_rows_total":              dto.MetricType_COUNTER,
	"braid_audit_flush_poison_rows_total":       dto.MetricType_COUNTER,
	"braid_audit_flush_duration_seconds":        dto.MetricType_HISTOGRAM,
	"braid_audit_pending_rows":                  dto.MetricType_GAUGE,
	"braid_audit_pending_oldest_age_seconds":    dto.MetricType_GAUGE,
	"braid_outbox_rows":                         dto.MetricType_GAUGE,
	"braid_outbox_oldest_age_seconds":           dto.MetricType_GAUGE,
	"braid_approval_backlog_rows":               dto.MetricType_GAUGE,
	"braid_approval_backlog_oldest_age_seconds": dto.MetricType_GAUGE,
	"braid_stuck_node_candidates":               dto.MetricType_GAUGE,
	"braid_resume_stuck_rows":                   dto.MetricType_GAUGE,
	"braid_approval_notify_missing_rows":        dto.MetricType_GAUGE,
	"braid_notify_results_total":                dto.MetricType_COUNTER,
	"braid_smtp_configured":                     dto.MetricType_GAUGE,
	"braid_asynq_archived_tasks":                dto.MetricType_GAUGE,
	"braid_relay_deliveries_total":              dto.MetricType_COUNTER,
	"braid_redis_memory_used_bytes":             dto.MetricType_GAUGE,
	"braid_redis_memory_max_bytes":              dto.MetricType_GAUGE,
	"braid_asynq_queue_tasks":                   dto.MetricType_GAUGE,
	"braid_login_attempts_total":                dto.MetricType_COUNTER,
}

func gatherMetricTypes(t *testing.T, reg *prometheus.Registry) map[string]dto.MetricType {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := make(map[string]dto.MetricType, len(families))
	for _, mf := range families {
		got[mf.GetName()] = metricTypeOf(mf)
	}
	return got
}

func metricTypeOf(mf *dto.MetricFamily) dto.MetricType {
	switch mf.GetType() {
	case dto.MetricType_COUNTER:
		return dto.MetricType_COUNTER
	case dto.MetricType_GAUGE:
		return dto.MetricType_GAUGE
	case dto.MetricType_HISTOGRAM:
		return dto.MetricType_HISTOGRAM
	default:
		return dto.MetricType_UNTYPED
	}
}

// touchAll instantiates one representative child of every labeled vector.
// Prometheus semantics: a *Vec family is absent from Gather/exposition until
// its first child exists, so the closed-set anchor must touch each one or it
// would silently pass while a misspelled metric name hides unexposed.
func touchAll(s *Set) {
	s.LLM.HTTPFailures.WithLabelValues(CodeRateLimited)
	s.LLM.Calls.WithLabelValues(CallSucceeded)
	s.Reaper.Reaped.WithLabelValues(CodeStaleAttempt)
	s.Reaper.Redispatches.WithLabelValues(TriggerReaped)
	s.Reaper.ScanBacklog.WithLabelValues(ScanAttempt)
	s.Reaper.FlushBatches.WithLabelValues(FlushOK)
	s.Reaper.FlushRows.WithLabelValues(FlushKindFlushed)
	s.Reaper.OutboxRows.WithLabelValues(OutboxDead)
	s.Reaper.OutboxOldest.WithLabelValues(OutboxDead)
	s.Reaper.StuckCandidates.WithLabelValues(ScanAttempt)
	s.Worker.NotifyResults.WithLabelValues(NotifyOutcomeSent)
	s.Relay.Deliveries.WithLabelValues(DeliverDispatched)
	s.Relay.QueueTasks.WithLabelValues("default")
	s.Login.Attempts.WithLabelValues(ResultSucceeded)
}

func TestNewSetRegistersClosedVocabulary(t *testing.T) {
	s, reg := NewSet()
	touchAll(s)
	got := gatherMetricTypes(t, reg)
	for name, typ := range metricNames {
		gotTyp, ok := got[name]
		if !ok {
			t.Errorf("metric %s registered by NewSet is missing (closed-set drift: rename or vocabulary change without updating the anchor)", name)
			continue
		}
		if gotTyp != typ {
			t.Errorf("metric %s: type = %v, want %v", name, gotTyp, typ)
		}
	}
	for name := range got {
		if _, ok := metricNames[name]; !ok {
			t.Errorf("metric %s registered but absent from metricNames anchor (untracked vocabulary)", name)
		}
	}
}

func TestNewSetRegistersIntoFreshRegistryPerCall(t *testing.T) {
	// Two processes in one test binary must not collide: each NewSet pair is
	// independent, so gathering one registry never sees the other's values.
	a, _ := NewSet()
	b, regB := NewSet()
	a.Login.Attempts.WithLabelValues(ResultSucceeded).Inc()
	famB, err := regB.Gather()
	if err != nil {
		t.Fatalf("Gather regB: %v", err)
	}
	for _, mf := range famB {
		if mf.GetName() == "braid_login_attempts_total" {
			for _, m := range mf.GetMetric() {
				if m.GetCounter().GetValue() != 0 {
					t.Errorf("regB sees regA value via %s: cross-registry leak", mf.GetName())
				}
			}
		}
	}
	b.Login.Attempts.WithLabelValues(ResultFailed).Inc()
}

func TestLLMSetSatisfiesLLMObservability(t *testing.T) {
	// Compile-time adaptation proof: *LLMSet must satisfy the llm package's
	// Observability seam without llm importing metrics. The interface is
	// asserted locally here to keep this package free of an llm import.
	type llmObservability interface {
		ObserveHTTPAttempt(class string)
		ObserveCall(outcome string, d time.Duration)
	}
	var _ llmObservability = (*LLMSet)(nil)
}

func TestCounterLabelsRoundTrip(t *testing.T) {
	s, reg := NewSet()
	s.LLM.HTTPFailures.WithLabelValues(CodeRateLimited).Inc()
	s.LLM.HTTPFailures.WithLabelValues(CodeServerError).Add(2)
	s.Reaper.Reaped.WithLabelValues(CodeHeartbeatLost).Inc()
	s.Worker.NotifyResults.WithLabelValues(NotifyOutcomeDegraded).Inc()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := map[string]map[string]float64{}
	for _, mf := range families {
		vals := map[string]float64{}
		for _, m := range mf.GetMetric() {
			var key string
			for _, l := range m.GetLabel() {
				if l.GetName() != "code" && l.GetName() != "outcome" {
					continue
				}
				key = l.GetValue()
			}
			if mf.GetType() == dto.MetricType_COUNTER {
				vals[key] = m.GetCounter().GetValue()
			}
		}
		if len(vals) > 0 {
			got[mf.GetName()] = vals
		}
	}
	want := map[string]map[string]float64{
		"braid_llm_http_failures_total": {CodeRateLimited: 1, CodeServerError: 2},
		"braid_reaper_reaped_total":     {CodeHeartbeatLost: 1},
		"braid_notify_results_total":    {NotifyOutcomeDegraded: 1},
	}
	for name, wantVals := range want {
		for label, v := range wantVals {
			if got[name][label] != v {
				t.Errorf("%s[%s] = %v, want %v", name, label, got[name][label], v)
			}
		}
	}
}

func TestHandlerExposesExposition(t *testing.T) {
	s, reg := NewSet()
	s.LLM.IncLLMCallDiscarded()
	body := handlerBody(t, Handler(reg))
	for _, want := range []string{
		"braid_llm_calls_discarded_total 1",
		"# TYPE braid_llm_calls_discarded_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q", want)
		}
	}
}
