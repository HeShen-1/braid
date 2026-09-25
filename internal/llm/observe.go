package llm

import (
	"context"
	"errors"
	"time"
)

// Observability is the adapter's metrics seam (T6.4). The worker injects its
// metrics adapter here; this package stays free of any metrics dependency.
// nil = observability disabled (default in unit tests).
//
// The class/outcome strings are mirrored by the constants in internal/metrics
// (Code*/CallSucceeded/CallFailed); the wiring anchor test in cmd/worker pins
// both vocabularies together.
type Observability interface {
	// ObserveHTTPAttempt reports one failed HTTP round trip observed inside
	// the retry loop — the only place mid-call 429s absorbed by retry are
	// visible (D-t36 §7 / T6.4).
	ObserveHTTPAttempt(class string)
	// ObserveCall reports one ChatCompletion call (terminal outcome) with
	// its full wall time.
	ObserveCall(outcome string, d time.Duration)
}

// Terminal call outcomes.
const (
	CallOutcomeSucceeded = "succeeded"
	CallOutcomeFailed    = "failed"
)

// HTTP failure classes (closed set). Exported so the metrics vocabulary in
// internal/metrics can be pinned to the same literals by the wiring anchor
// test in cmd/worker — this package still never imports metrics.
const (
	HTTPClass429      = "429"
	HTTPClass5xx      = "5xx"
	HTTPClass4xx      = "4xx"
	HTTPClassNetwork  = "network"
	HTTPClassTimeout  = "timeout"
	HTTPClassProtocol = "protocol"
)

// observeClass maps one failed round trip to its failure class. Build
// failures (request construction) never reach here — they make no HTTP
// attempt.
func observeClass(err error) string {
	var se *StatusError
	if errors.As(err, &se) {
		switch {
		case se.StatusCode == 429:
			return HTTPClass429
		case se.StatusCode >= 500:
			return HTTPClass5xx
		default:
			return HTTPClass4xx
		}
	}
	if errors.Is(err, ErrMissingUsage) || errors.Is(err, ErrBadResponse) {
		return HTTPClassProtocol
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return HTTPClassTimeout
	}
	return HTTPClassNetwork
}

func (c *OpenAICompat) observeFailure(err error) {
	if c.obs != nil {
		c.obs.ObserveHTTPAttempt(observeClass(err))
	}
}
