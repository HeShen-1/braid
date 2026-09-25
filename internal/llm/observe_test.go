package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// obsRecorder is the in-package test double for the Observability seam.
type obsRecorder struct {
	mu       sync.Mutex
	failures []string
	calls    []string
	durs     []time.Duration
}

func (o *obsRecorder) ObserveHTTPAttempt(code string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failures = append(o.failures, code)
}

func (o *obsRecorder) ObserveCall(outcome string, d time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, outcome)
	o.durs = append(o.durs, d)
}

func (o *obsRecorder) snapshot() (failures, calls []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string{}, o.failures...), append([]string{}, o.calls...)
}

func TestObserveClass(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"429", &StatusError{StatusCode: 429}, "429"},
		{"5xx", &StatusError{StatusCode: 503}, "5xx"},
		{"other 4xx", &StatusError{StatusCode: 401}, "4xx"},
		{"missing usage", ErrMissingUsage, "protocol"},
		{"bad response", fmt.Errorf("%w: decode", ErrBadResponse), "protocol"},
		{"deadline", context.DeadlineExceeded, "timeout"},
		{"network", errors.New("connection refused"), "network"},
	}
	for _, tc := range cases {
		if got := observeClass(tc.err); got != tc.want {
			t.Errorf("%s: observeClass = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func newObsTestConfig(t *testing.T, base string) Config {
	t.Helper()
	cfg := DefaultConfig()
	cfg.BaseURL = base
	cfg.APIKey = "sk-test"
	cfg.Timeout = 5 * time.Second
	cfg.BackoffBase = time.Millisecond
	cfg.BackoffFactor = 1
	cfg.BackoffMax = 2 * time.Millisecond
	cfg.PriceTable = mustTable(t, minPriceTable)
	return cfg
}

func successBody(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"model": "m",
		"choices": []map[string]any{
			{"message": map[string]string{"role": "assistant", "content": "ok"},
				"finish_reason": "stop"},
		},
		"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestChatCompletionObservesMidCall429AndOutcome(t *testing.T) {
	var n int32
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		n++
		cur := n
		mu.Unlock()
		if cur <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"slow down"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(successBody(t))
	}))
	defer srv.Close()

	obs := &obsRecorder{}
	c, err := NewOpenAICompat(newObsTestConfig(t, srv.URL), obs)
	if err != nil {
		t.Fatalf("NewOpenAICompat: %v", err)
	}
	resp, err := c.ChatCompletion(context.Background(), Request{
		Model: "m", Messages: []Message{{Role: "user", Content: "hi"}},
		IdempotencyKey: "att:1:1",
	})
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if resp.Usage.TotalTokens != 2 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	failures, calls := obs.snapshot()
	if len(failures) != 2 || failures[0] != "429" || failures[1] != "429" {
		t.Errorf("failures = %v, want two 429s (mid-call retries must be visible: T6.4/D-t36)", failures)
	}
	if len(calls) != 1 || calls[0] != "succeeded" {
		t.Errorf("calls = %v, want [succeeded]", calls)
	}
}

func TestChatCompletionObservesFailedOutcomeAndExhausted429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limited"}}`))
	}))
	defer srv.Close()

	obs := &obsRecorder{}
	c, err := NewOpenAICompat(newObsTestConfig(t, srv.URL), obs)
	if err != nil {
		t.Fatalf("NewOpenAICompat: %v", err)
	}
	_, err = c.ChatCompletion(context.Background(), Request{
		Model: "m", Messages: []Message{{Role: "user", Content: "hi"}},
		IdempotencyKey: "att:1:1",
	})
	if err == nil {
		t.Fatal("expected exhaustion error")
	}
	failures, calls := obs.snapshot()
	if len(failures) != 3 {
		t.Errorf("failures = %v, want 3 (MaxAttempts)", failures)
	}
	for _, f := range failures {
		if f != "429" {
			t.Errorf("failure class %q, want 429", f)
		}
	}
	if len(calls) != 1 || calls[0] != "failed" {
		t.Errorf("calls = %v, want [failed]", calls)
	}
}

func TestChatCompletionNilObservability(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(successBody(t))
	}))
	defer srv.Close()
	c, err := NewOpenAICompat(newObsTestConfig(t, srv.URL), nil)
	if err != nil {
		t.Fatalf("NewOpenAICompat: %v", err)
	}
	if _, err := c.ChatCompletion(context.Background(), Request{
		Model: "m", Messages: []Message{{Role: "user", Content: "hi"}},
		IdempotencyKey: "att:1:1",
	}); err != nil {
		t.Fatalf("ChatCompletion with nil obs: %v", err)
	}
}
