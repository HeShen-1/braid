// Package metrics centralizes braid's Prometheus metric vocabulary.
//
// Every metric the platform exports is defined in this package and anchored
// by the closed-set test (TestNewSetRegistersClosedVocabulary), mirroring the
// audit event vocabulary precedent in internal/audit. Components receive the
// relevant subset via Deps injection; narrow consumer-side interfaces (e.g.
// llm's Observability, scheduler's MetricsSink) are satisfied implicitly by
// these types so leaf packages never import this one.
//
// Naming: counters end in _total, gauges do not; everything is prefixed
// braid_. Label values are exported constants in this package — callers must
// use them, never raw strings, so label drift is a compile error.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Label vocabularies (closed sets, consumed as constants).
const (
	// LLM HTTP failure classes observed inside the adapter retry loop.
	// Literals are pinned to llm.HTTPClass* by the cmd/worker anchor test.
	CodeRateLimited = "429"
	CodeServerError = "5xx"
	CodeClientError = "4xx"
	CodeNetworkErr  = "network"
	CodeTimeout     = "timeout"
	CodeProtocolErr = "protocol"

	// LLM call outcomes at the ChatCompletion layer. Literals pinned to
	// llm.CallOutcome* by the same anchor test.
	CallSucceeded = "succeeded"
	CallFailed    = "failed"

	// Reaper death verdicts (node_attempt closure codes).
	CodeStaleAttempt  = "stale_attempt"
	CodeDeadlineBurst = "deadline_exceeded"
	CodeHeartbeatLost = "heartbeat_lost"

	// Redispatch triggers.
	TriggerReaped        = "reaped"
	TriggerAttemptFailed = "attempt_failed"
	TriggerApprovalRes   = "approval_resume"

	// Scan identifiers for backlog saturation counters.
	ScanAttempt        = "attempt"
	ScanNodeDeadline   = "node_deadline"
	ScanNodeOrphan     = "node_orphan"
	ScanExecPending    = "exec_pending"
	ScanExecStale      = "exec_stale"
	ScanApproval       = "approval"
	ScanApprovalOrphan = "approval_orphan"

	// Flush batch outcomes.
	FlushOK       = "ok"
	FlushRollback = "rollback"

	// Flush row kinds.
	FlushKindFlushed  = "flushed"
	FlushKindReplayed = "replayed"

	// Outbox row statuses for gauges.
	OutboxDead    = "dead"
	OutboxPending = "pending"

	// Notification handler outcomes.
	NotifyOutcomeSent     = "sent"
	NotifyOutcomeDeduped  = "deduped"
	NotifyOutcomeDegraded = "degraded"
	NotifyOutcomeFailed   = "failed"

	// Relay delivery outcomes.
	DeliverDispatched = "dispatched"
	DeliverConflict   = "conflict"
	DeliverBackoff    = "backoff"
	DeliverDead       = "dead"

	// Login attempt results.
	ResultSucceeded   = "succeeded"
	ResultFailed      = "failed"
	ResultLocked      = "locked"
	ResultRateLimited = "rate_limited"

	// Poison snapshot facets (T17.4): which validation of the approval
	// input_snapshot failed in the reaper's ⑤a disposition tree. Rows stay
	// WAITING_APPROVAL and re-fail every tick — the counter is the operator
	// signal, mirroring the audit flush poison precedent.
	PoisonReasonDecode    = "decode"
	PoisonReasonMode      = "mode"
	PoisonReasonOnTimeout = "on_timeout"

	// Resume event rejection facets (T17.4): why scheduler.Resume refused an
	// execution.resume event (5a premature/fabricated defense surface).
	// Literals are pinned to scheduler sentinel errors by the worker anchor
	// test; stable across the errors.Is boundary.
	ResumeRejectStaleRound  = "stale_round"
	ResumeRejectFabricated  = "fabricated"
	ResumeRejectPremature   = "premature"
	ResumeRejectAnchorDrift = "anchor_drift"
)

// LLMSet covers the LLM adapter retry loop and the scheduler discard path.
type LLMSet struct {
	// HTTPFailures counts every failed HTTP round trip observed by the
	// adapter's retry loop, labeled by failure class. This is the only place
	// mid-call 429s (absorbed by retry) are visible (T6.4/D-t36); successful
	// round trips are counted by braid_llm_calls_total{succeeded}.
	HTTPFailures *prometheus.CounterVec
	// Calls counts ChatCompletion calls by terminal outcome.
	Calls *prometheus.CounterVec
	// CallDuration observes full ChatCompletion wall time per attempt chain.
	CallDuration prometheus.Histogram
	discarded    prometheus.Counter
}

// IncLLMCallDiscarded marks one late-SUCCEEDED-with-usage outcome dropped by the
// Complete yield path: billed by the provider, absent from the ledger
// (D-t36 §7). The audit row llm.call.discarded is the durable twin.
func (l *LLMSet) IncLLMCallDiscarded() { l.discarded.Inc() }

// ObserveHTTPAttempt satisfies the llm package's Observability seam.
func (l *LLMSet) ObserveHTTPAttempt(class string) {
	l.HTTPFailures.WithLabelValues(class).Inc()
}

// ObserveCall satisfies the llm package's Observability seam.
func (l *LLMSet) ObserveCall(outcome string, d time.Duration) {
	l.Calls.WithLabelValues(outcome).Inc()
	l.CallDuration.Observe(d.Seconds())
}

// ReaperSet covers reaper verdicts, scans, flush observability, and the
// periodic DB-sourced gauges (backlog/aging/dead-letter surfaces).
type ReaperSet struct {
	Reaped             *prometheus.CounterVec
	Redispatches       *prometheus.CounterVec
	NodeFailures       prometheus.Counter
	NodeCancellations  prometheus.Counter
	ApprovalsEscalated prometheus.Counter
	ScanBacklog        *prometheus.CounterVec
	FlushBatches       *prometheus.CounterVec
	FlushRows          *prometheus.CounterVec
	FlushPoison        prometheus.Counter
	FlushDuration      prometheus.Histogram
	ChainVerifyRows    prometheus.Counter
	ChainBrokenTotal   prometheus.Counter
	ChainBrokenHeads   prometheus.Gauge
	// FenceRejected counts reaper-side lease-fence rejections (T20.1,
	// D-2026-10-06-t201): verdict txs (attempt/approval/pending-exec scans)
	// rolled back because this reaper lost its lease crown. Twin of
	// FenceSet.Rejected / WorkerSet.FenceRejected (same family, reaper face).
	FenceRejected      prometheus.Counter
	AuditPendingRows   prometheus.Gauge
	AuditPendingOldest prometheus.Gauge
	// AuditPartitionWatermarkDays / AuditPminRows:T20.7——水位衰减的唯一
	// 运行期显影面(janitor 停摆预警)与 p_min 兜底分区落行哨兵(兑现
	// 0001_init.up.sql 监控承诺)。
	AuditPartitionWatermarkDays prometheus.Gauge
	AuditPminRows               prometheus.Gauge
	OutboxRows                  *prometheus.GaugeVec
	OutboxOldest                *prometheus.GaugeVec
	ApprovalBacklog             prometheus.Gauge
	ApprovalOldest              prometheus.Gauge
	StuckCandidates             *prometheus.GaugeVec
	ResumeStuck                 prometheus.Gauge
	ApprovalNotifyMissing       prometheus.Gauge
	// PoisonSnapshots counts approval snapshot validation failures in the ⑤a
	// disposition tree (T17.4): decode failure, mode/on_timeout outside the
	// DSL vocabulary (snapshot drift). Rows are permanently re-tried each
	// tick — increase() > 0 is the alert basis.
	PoisonSnapshots *prometheus.CounterVec
	// IMNotifyMissing mirrors ApprovalNotifyMissing for the IM approval-card
	// channel (approval.im_notified reconciliation, T17.4); gated on the
	// consumer side by braid_im_configured.
	IMNotifyMissing prometheus.Gauge
	// AsynqAliveDefers counts branch-B redispatches deferred because the
	// stale-DISPATCHED task turned out alive in asynq (Inspector cross-check,
	// D-2026-10-04-t174). Each defer is one double-execution residual window
	// closed (D-t25#10).
	AsynqAliveDefers prometheus.Counter
	// AsynqProbeUnreachable counts branch-B fail-opens caused by probe errors
	// (T18.7, D-t174 known-boundary closure): an unreachable Inspector is not
	// death evidence, so the dispatch-stale bounded window keeps governing —
	// the counter makes that silent-fallback path observable (mirror of the
	// warn key node.redispatch_asynq_probe_unreachable).
	AsynqProbeUnreachable prometheus.Counter
}

// WorkerSet covers notify outcomes, SMTP availability, and asynq archived
// backlog.
type WorkerSet struct {
	NotifyResults  *prometheus.CounterVec
	SMTPConfigured prometheus.Gauge
	IMConfigured   prometheus.Gauge
	AsynqArchived  prometheus.Gauge
	// ResumeRejected counts execution.resume events the scheduler refused as
	// stale/fabricated/premature (5a defense surface, T17.4). The error is
	// still propagated (asynq retry → archived stays the visible dead letter);
	// the counter makes the defense surface observable per facet.
	ResumeRejected *prometheus.CounterVec
	// FenceRejected counts worker-side lease-fence rejections (T20.1,
	// D-2026-10-06-t201): claim/heartbeat writes structurally rolled back
	// because this process lost the lease crown (superseded/taken over).
	// Durable twin = the fence error in handler/heartbeat warn logs.
	FenceRejected prometheus.Counter
}

// RelaySet covers outbox delivery outcomes and queue-Redis health.
type RelaySet struct {
	Deliveries      *prometheus.CounterVec
	RedisMemoryUsed prometheus.Gauge
	RedisMemoryMax  prometheus.Gauge
	QueueTasks      *prometheus.GaugeVec
}

// LoginSet covers login attempt outcomes (the durable twin lives in the
// login_audit table, migration 0005).
type LoginSet struct {
	Attempts *prometheus.CounterVec
}

// RollbackSet covers the T9.2 rollback/resume endpoints (api process;
// D-2026-09-28-t92 decision 10). Requested counts every authenticated
// attempt reaching the scheduler seam; Applied counts committed rollbacks;
// Denied breaks rejections down by reason facet (mirroring the structured
// error kinds + the role gate + the T20.2 scp gate's scope_denied). Replayed
// counts T18.5 replay hits —
// duplicate/concurrent-loser requests converged to 200 with the first
// answer (zero new writes), so they stop inflating Denied. Quiesced counts
// T19.6 quiesce rollbacks that wedged >=1 in-flight residue row (wedge-
// in-transaction convergence replacing the reaper settle wait).
type RollbackSet struct {
	Requested prometheus.Counter
	Applied   prometheus.Counter
	Denied    *prometheus.CounterVec // by reason
	Replayed  prometheus.Counter
	Quiesced  prometheus.Counter
}

// FleetGateReason* are the T15.6 fleet capability gate rejection facets:
// undeclared = in-fleet instance missing the capability (mixed-version
// window); unavailable = fleet scan error (fail-closed, same reject path).
const (
	FleetGateReasonUndeclared  = "undeclared"
	FleetGateReasonUnavailable = "fleet_unavailable"
)

// FleetGateSet covers the T15.6 mode=all fleet capability gate (api process;
// D-2026-10-01-t151 decision 6). Rejections are the operator-facing signal of
// a mixed-version rolling window (durable trail is the structured slog, not
// audit_log — execution-anchored domain cannot take pre-materialization rows).
type FleetGateSet struct {
	Rejected *prometheus.CounterVec // by reason (undeclared | fleet_unavailable)
}

// CancelSet covers the T19.3 execution-cancel endpoint (api process;
// D-2026-10-05-t193 decision 5). Same three-counter shape as RollbackSet;
// durable trail is the execution.cancelled/cancel.denied audit pair.
type CancelSet struct {
	Requested prometheus.Counter
	Applied   prometheus.Counter
	Denied    *prometheus.CounterVec // by reason (role_denied/scope_denied/not_cancellable/too_large)
}

// FenceSet covers the T20.1 lease-fence rejection surface (scheduler-side:
// Instantiate/Start/Complete/Suspend/resume/rollback/cancel/ReapInstance-
// Exhausted txs; worker-side twin = WorkerSet.FenceRejected). A rejection
// means the process lost its lease crown mid-flight (zombie window closed at
// the truth source) — the write tx rolled back structurally.
type FenceSet struct {
	Rejected prometheus.Counter
}

// Set aggregates every component subset; exactly one per process.
type Set struct {
	LLM       *LLMSet
	Reaper    *ReaperSet
	Worker    *WorkerSet
	Relay     *RelaySet
	Login     *LoginSet
	Rollback  *RollbackSet
	Cancel    *CancelSet
	FleetGate *FleetGateSet
	Fence     *FenceSet
	DBPool    *DBPoolSet
}

// IncLLMCallDiscarded satisfies the scheduler MetricsSink seam (T6.4).
func (s *Set) IncLLMCallDiscarded() { s.LLM.IncLLMCallDiscarded() }

// IncFenceRejected satisfies the scheduler MetricsSink seam addition (T20.1,
// D-2026-10-06-t201): scheduler-orchestrated write txs rolled back by the
// lease-fence crown check. Surface-agnostic by design (per-face attribution
// rides the fence error logs; the alert face is process-level).
func (s *Set) IncFenceRejected() { s.Fence.Rejected.Inc() }

// IncNodeRedispatched satisfies the scheduler MetricsSink seam addition
// (T12.2, D-t64 reserved-vocabulary activation): the in-worker scheduler
// emits trigger=attempt_failed on its retry branch. The family name says
// "reaper" for historical reasons (D-t64 coined it around the reaper set);
// attempt_failed children surface on the WORKER process exposition, which is
// where the scheduler runs — by design, not drift.
func (s *Set) IncNodeRedispatched() {
	s.Reaper.Redispatches.WithLabelValues(TriggerAttemptFailed).Inc()
}

// NewSet builds a Set registered into a fresh Registry and returns both.
// A fresh registry per process keeps tests hermetic (no global registration).
func NewSet() (*Set, *prometheus.Registry) {
	reg := prometheus.NewRegistry()
	f := promauto.With(reg)
	s := &Set{
		LLM: &LLMSet{
			HTTPFailures: f.NewCounterVec(prometheus.CounterOpts{
				Name: "braid_llm_http_failures_total",
				Help: "Failed LLM HTTP round trips inside the adapter retry loop, by failure class (mid-call 429s included).",
			}, []string{"code"}),
			Calls: f.NewCounterVec(prometheus.CounterOpts{
				Name: "braid_llm_calls_total",
				Help: "LLM ChatCompletion calls by terminal outcome.",
			}, []string{"outcome"}),
			CallDuration: f.NewHistogram(prometheus.HistogramOpts{
				Name:    "braid_llm_call_duration_seconds",
				Help:    "LLM ChatCompletion wall time per attempt chain.",
				Buckets: []float64{.1, .5, 1, 5, 30, 60, 120, 300},
			}),
			discarded: f.NewCounter(prometheus.CounterOpts{
				Name: "braid_llm_calls_discarded_total",
				Help: "Late SUCCEEDED outcomes carrying usage dropped by the Complete yield path: billed by the provider, absent from the ledger (D-t36 §7).",
			}),
		},
		Reaper: &ReaperSet{
			Reaped: f.NewCounterVec(prometheus.CounterOpts{
				Name: "braid_reaper_reaped_total",
				Help: "Attempts closed DEAD by the reaper, by verdict code.",
			}, []string{"code"}),
			Redispatches: f.NewCounterVec(prometheus.CounterOpts{
				Name: "braid_reaper_redispatches_total",
				Help: "Node redispatches, by trigger (reaper verdicts; attempt_failed is emitted by the in-worker scheduler).",
			}, []string{"trigger"}),
			NodeFailures: f.NewCounter(prometheus.CounterOpts{
				Name: "braid_reaper_node_failures_total",
				Help: "Nodes failed by reaper verdicts (fail-fast mirrors included).",
			}),
			NodeCancellations: f.NewCounter(prometheus.CounterOpts{
				Name: "braid_reaper_node_cancellations_total",
				Help: "Nodes cancelled as platform rulings (orphans under terminal executions).",
			}),
			ApprovalsEscalated: f.NewCounter(prometheus.CounterOpts{
				Name: "braid_reaper_approvals_escalated_total",
				Help: "Approval nodes escalated on timeout (exactly-once anchored by escalated_at).",
			}),
			ScanBacklog: f.NewCounterVec(prometheus.CounterOpts{
				Name: "braid_reaper_scan_backlog_total",
				Help: "Scan ticks that saturated their batch budget (scan.backlog): candidate supply exceeds one tick's capacity.",
			}, []string{"scan"}),
			FlushBatches: f.NewCounterVec(prometheus.CounterOpts{
				Name: "braid_audit_flush_batches_total",
				Help: "Audit flush batches by outcome (rollback = reconciliation mismatch or 1062, retried next tick).",
			}, []string{"outcome"}),
			FlushRows: f.NewCounterVec(prometheus.CounterOpts{
				Name: "braid_audit_flush_rows_total",
				Help: "Audit rows moved from audit_pending to audit_log, by kind.",
			}, []string{"kind"}),
			FlushPoison: f.NewCounter(prometheus.CounterOpts{
				Name: "braid_audit_flush_poison_rows_total",
				Help: "Audit pending rows isolated as poison (ghost executions etc.); they never clear and rescan every tick.",
			}),
			FlushDuration: f.NewHistogram(prometheus.HistogramOpts{
				Name:    "braid_audit_flush_duration_seconds",
				Help:    "Audit flush batch wall time.",
				Buckets: prometheus.DefBuckets,
			}),
			ChainVerifyRows: f.NewCounter(prometheus.CounterOpts{
				Name: "braid_audit_chain_verify_rows_total",
				Help: "Audit hash-chain rows re-walked by the verify job (T9.3; incremental watermark advances).",
			}),
			ChainBrokenTotal: f.NewCounter(prometheus.CounterOpts{
				Name: "braid_audit_chain_broken_total",
				Help: "Chains newly latched broken by verification (first sighting only; tamper/deletion/tail-loss/partition-move). Appends for those executions are refused until manual reset.",
			}),
			ChainBrokenHeads: f.NewGauge(prometheus.GaugeOpts{
				Name: "braid_audit_chain_broken_heads",
				Help: "Chain heads currently latched broken (evidence integrity; alert face for T9.3).",
			}),
			AuditPendingRows: f.NewGauge(prometheus.GaugeOpts{
				Name: "braid_audit_pending_rows",
				Help: "Rows currently staged in audit_pending (WAL backlog).",
			}),
			AuditPendingOldest: f.NewGauge(prometheus.GaugeOpts{
				Name: "braid_audit_pending_oldest_age_seconds",
				Help: "Age of the oldest audit_pending row; visibility lag upper bound is AuditFlushInterval — growth here means the knob or the drain rate is off (D-t61).",
			}),
			AuditPartitionWatermarkDays: f.NewGauge(prometheus.GaugeOpts{
				Name: "braid_audit_partition_watermark_days",
				Help: "Audit_log partition watermark headroom in days (max bound minus today, DB clock). Janitor tops to +8; sustained values below 3 mean the janitor has stalled (restart api/worker before the watermark lapses, T20.7).",
			}),
			AuditPminRows: f.NewGauge(prometheus.GaugeOpts{
				Name: "braid_audit_pmin_rows",
				Help: "Rows landed in the audit_log p_min catch-all partition (exec_date before 2026-01-01). Any nonzero value is writer clock drift or backfill — the 0001_init monitoring promise.",
			}),
			OutboxRows: f.NewGaugeVec(prometheus.GaugeOpts{
				Name: "braid_outbox_rows",
				Help: "Outbox rows by status; dead is the durable dead-letter surface (relay exhaustion).",
			}, []string{"status"}),
			OutboxOldest: f.NewGaugeVec(prometheus.GaugeOpts{
				Name: "braid_outbox_oldest_age_seconds",
				Help: "Age of the oldest outbox row by status; pending growth = relay starvation.",
			}, []string{"status"}),
			ApprovalBacklog: f.NewGauge(prometheus.GaugeOpts{
				Name: "braid_approval_backlog_rows",
				Help: "Approvals currently WAITING_APPROVAL under live executions.",
			}),
			ApprovalOldest: f.NewGauge(prometheus.GaugeOpts{
				Name: "braid_approval_backlog_oldest_age_seconds",
				Help: "Time since the oldest WAITING_APPROVAL node changed state (backlog aging).",
			}),
			StuckCandidates: f.NewGaugeVec(prometheus.GaugeOpts{
				Name: "braid_stuck_node_candidates",
				Help: "Scan candidate counts per sweep: attempt (lost heartbeat), node_deadline (EXECUTING past deadline), exec_stale (RUNNING execution, stale activity).",
			}, []string{"scan"}),
			ResumeStuck: f.NewGauge(prometheus.GaugeOpts{
				Name: "braid_resume_stuck_rows",
				Help: "Decided approvals still WAITING_APPROVAL past deadline (resume event dead/stranded): the archived-stranding surface (D-t42). Escalated-but-undecided rows are Q9-by-design waiting and excluded.",
			}),
			ApprovalNotifyMissing: f.NewGauge(prometheus.GaugeOpts{
				Name: "braid_approval_notify_missing_rows",
				Help: "Escalated approvals with no approval.notified row past the grace window (escalated-vs-notified reconciliation, D-t45).",
			}),
			PoisonSnapshots: f.NewCounterVec(prometheus.CounterOpts{
				Name: "braid_reaper_poison_snapshots_total",
				Help: "Approval input_snapshot validation failures in the reaper disposition tree (decode / mode / on_timeout drift); rows re-fail every tick, see the ReaperPoisonSnapshot alert (T17.4).",
			}, []string{"reason"}),
			IMNotifyMissing: f.NewGauge(prometheus.GaugeOpts{
				Name: "braid_approval_im_notified_missing_rows",
				Help: "Escalated approvals with no approval.im_notified row past the grace window (IM channel reconciliation, T17.4); gates on braid_im_configured==1.",
			}),
			AsynqAliveDefers: f.NewCounter(prometheus.CounterOpts{
				Name: "braid_reaper_asynq_alive_defers_total",
				Help: "Branch-B redispatch decisions deferred because the stale-DISPATCHED task is still alive in asynq (Inspector cross-check, D-2026-10-04-t174); per-decision, not per-node — a node re-examined by a second scan in the same tick can defer twice.",
			}),
			AsynqProbeUnreachable: f.NewCounter(prometheus.CounterOpts{
				Name: "braid_reaper_asynq_probe_unreachable_total",
				Help: "Branch-B liveness probes that errored and fail-opened to the dispatch-stale bounded window (T18.7); persistent growth = the reaper cannot reach queue Redis (probe degraded, hardening silently off).",
			}),
			FenceRejected: f.NewCounter(prometheus.CounterOpts{
				Name: "braid_reaper_fence_rejected_total",
				Help: "Lease-fence rejections (T20.1): reaper verdict txs (attempt/approval/pending-exec scans) rolled back because this reaper lost its lease crown; twin of braid_fence_rejected_total / braid_worker_fence_rejected_total.",
			}),
		},
		Worker: &WorkerSet{
			NotifyResults: f.NewCounterVec(prometheus.CounterOpts{
				Name: "braid_notify_results_total",
				Help: "Approval notify handler outcomes; degraded = SMTP unconfigured (missing notified rows are expected then, see braid_smtp_configured).",
			}, []string{"outcome"}),
			SMTPConfigured: f.NewGauge(prometheus.GaugeOpts{
				Name: "braid_smtp_configured",
				Help: "1 when the worker runs with SMTP configured, 0 in degraded log-only mode; gates the notify-missing alert.",
			}),
			IMConfigured: f.NewGauge(prometheus.GaugeOpts{
				Name: "braid_im_configured",
				Help: "1 when the worker runs with the IM approval-card channel (DingTalk webhook) configured; gates ApprovalIMNotifyMissing (T17.4, the T6.4-reserved reconciliation alert).",
			}),
			AsynqArchived: f.NewGauge(prometheus.GaugeOpts{
				Name: "braid_asynq_archived_tasks",
				Help: "Tasks in asynq archived state (dead letters with no DB trace).",
			}),
			ResumeRejected: f.NewCounterVec(prometheus.CounterOpts{
				Name: "braid_resume_events_rejected_total",
				Help: "execution.resume deliveries refused by scheduler.Resume as stale/fabricated/premature/anchor-drift (5a defense, T17.4); per-delivery (redeliveries re-observe), durable twin = the archived task + scheduler error log.",
			}, []string{"reason"}),
			FenceRejected: f.NewCounter(prometheus.CounterOpts{
				Name: "braid_worker_fence_rejected_total",
				Help: "Lease-fence rejections (T20.1): worker-side write txs (claim/heartbeat) rolled back because this process lost its lease crown; scheduler-side twin = braid_fence_rejected_total.",
			}),
		},
		Relay: &RelaySet{
			Deliveries: f.NewCounterVec(prometheus.CounterOpts{
				Name: "braid_relay_deliveries_total",
				Help: "Outbox delivery attempts by outcome.",
			}, []string{"outcome"}),
			RedisMemoryUsed: f.NewGauge(prometheus.GaugeOpts{
				Name: "braid_redis_memory_used_bytes",
				Help: "Queue Redis used_memory from periodic INFO memory probes.",
			}),
			RedisMemoryMax: f.NewGauge(prometheus.GaugeOpts{
				Name: "braid_redis_memory_max_bytes",
				Help: "Queue Redis maxmemory; used/max is the watermark alert basis (0 = unlimited).",
			}),
			QueueTasks: f.NewGaugeVec(prometheus.GaugeOpts{
				Name: "braid_asynq_queue_tasks",
				Help: "Asynq queue depths (pending+active+retry) by queue.",
			}, []string{"queue"}),
		},
		Login: &LoginSet{
			Attempts: f.NewCounterVec(prometheus.CounterOpts{
				Name: "braid_login_attempts_total",
				Help: "Login attempts by result; durable twin in login_audit (migration 0005).",
			}, []string{"result"}),
		},
		Rollback: &RollbackSet{
			Requested: f.NewCounter(prometheus.CounterOpts{
				Name: "braid_rollback_requested_total",
				Help: "Rollback/resume requests reaching the scheduler seam (authenticated).",
			}),
			Applied: f.NewCounter(prometheus.CounterOpts{
				Name: "braid_rollback_applied_total",
				Help: "Committed rollbacks (scheduler.RollbackTo success).",
			}),
			Denied: f.NewCounterVec(prometheus.CounterOpts{
				Name: "braid_rollback_denied_total",
				Help: "Rejected rollback/resume requests by reason (role_denied/not_rollbackable/blocked_inflight/bad_node_id/no_resume_target).",
			}, []string{"reason"}),
			Replayed: f.NewCounter(prometheus.CounterOpts{
				Name: "braid_rollback_replayed_total",
				Help: "Duplicate rollback/resume requests replayed with the first answer (T18.5 idempotent 200; no new writes).",
			}),
			Quiesced: f.NewCounter(prometheus.CounterOpts{
				Name: "braid_rollback_quiesced_total",
				Help: "Committed rollbacks that wedged >=1 in-flight residue row in-transaction (T19.6 quiesce protocol).",
			}),
		},
		Cancel: &CancelSet{
			Requested: f.NewCounter(prometheus.CounterOpts{
				Name: "braid_cancel_requested_total",
				Help: "Execution-cancel requests reaching the scheduler seam (authenticated).",
			}),
			Applied: f.NewCounter(prometheus.CounterOpts{
				Name: "braid_cancel_applied_total",
				Help: "Committed cancellations (scheduler.CancelExecution success).",
			}),
			Denied: f.NewCounterVec(prometheus.CounterOpts{
				Name: "braid_cancel_denied_total",
				Help: "Rejected execution-cancel requests by reason (role_denied/not_cancellable/too_large).",
			}, []string{"reason"}),
		},
		Fence: &FenceSet{
			Rejected: f.NewCounter(prometheus.CounterOpts{
				Name: "braid_fence_rejected_total",
				Help: "Lease-fence rejections (T20.1, D-2026-10-06-t201): scheduler-orchestrated write txs rolled back by the lease crown check; a rejection means this process was superseded mid-flight (zombie window closed at the truth source).",
			}),
		},
		FleetGate: &FleetGateSet{
			Rejected: f.NewCounterVec(prometheus.CounterOpts{
				Name: "braid_api_fleet_gate_rejections_total",
				Help: "mode=all publish/trigger requests rejected by the fleet capability gate (T15.6), by reason; undeclared = mixed-version window signal.",
			}, []string{"reason"}),
		},
		DBPool: &DBPoolSet{
			OpenConnections: f.NewGauge(prometheus.GaugeOpts{
				Name: "braid_db_pool_open_connections",
				Help: "Open MySQL pool connections (database/sql DBStats), sampled every 30s per entrypoint.",
			}),
			InUse: f.NewGauge(prometheus.GaugeOpts{
				Name: "braid_db_pool_in_use_connections",
				Help: "In-use MySQL pool connections; sustained in_use == max_open means the pool is saturated.",
			}),
			Idle: f.NewGauge(prometheus.GaugeOpts{
				Name: "braid_db_pool_idle_connections",
				Help: "Idle MySQL pool connections (database/sql DBStats).",
			}),
			MaxOpen: f.NewGauge(prometheus.GaugeOpts{
				Name: "braid_db_pool_max_open_connections",
				Help: "Effective BRAID_DB_MAX_OPEN per entrypoint (config visibility gauge).",
			}),
			WaitTotal: f.NewCounter(prometheus.CounterOpts{
				Name: "braid_db_pool_wait_total",
				Help: "Cumulative pool acquisitions that waited for a free connection (DBStats WaitCount); growth = saturated, raise BRAID_DB_MAX_OPEN within the MySQL max_connections budget (deploy/README.md).",
			}),
		},
	}
	return s, reg
}
