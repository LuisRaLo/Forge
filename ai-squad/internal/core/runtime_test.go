package core

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRuntimeErrorRetryable(t *testing.T) {
	t.Parallel()

	retryable := []RuntimeErrorKind{ErrKindTimeout, ErrKindRateLimit, ErrKindTransient}
	for _, kind := range retryable {
		e := &RuntimeError{Runtime: "claude", Kind: kind}
		if !e.Retryable() {
			t.Errorf("%s should be retryable", kind)
		}
		if !IsRetryable(fmt.Errorf("wrapped: %w", e)) {
			t.Errorf("IsRetryable must see through wrapping for %s", kind)
		}
	}

	// A permission denial or a budget breach must never be retried: retrying
	// would either loop forever or keep spending.
	permanent := []RuntimeErrorKind{
		ErrKindAuth, ErrKindPermanent, ErrKindPermissionDenied,
		ErrKindBudgetExceeded, ErrKindUnsupported, ErrKindCancelled,
	}
	for _, kind := range permanent {
		e := &RuntimeError{Runtime: "claude", Kind: kind}
		if e.Retryable() {
			t.Errorf("%s must not be retryable", kind)
		}
	}

	if IsRetryable(errors.New("plain error")) {
		t.Error("a non-runtime error is not retryable")
	}
}

func TestRuntimeErrorUnwraps(t *testing.T) {
	t.Parallel()

	cause := errors.New("connection refused")
	err := &RuntimeError{Runtime: "ollama", Kind: ErrKindTransient, Message: "dial failed", Err: cause}
	if !errors.Is(err, cause) {
		t.Fatal("RuntimeError must unwrap to its cause")
	}
	if err.Error() == "" {
		t.Fatal("RuntimeError must render a message")
	}
}

func TestEventSinkEmitToleratesNil(t *testing.T) {
	t.Parallel()

	var sink EventSink
	// Must not panic: a run without a listener is normal.
	sink.Emit(context.Background(), Event{Type: EventTypeStarted})
}

func TestEventSinkEmitStampsTime(t *testing.T) {
	t.Parallel()

	var got Event
	sink := EventSink(func(_ context.Context, ev Event) { got = ev })
	sink.Emit(context.Background(), Event{Type: EventTypeLog, Text: "hello"})

	if got.Timestamp.IsZero() {
		t.Fatal("Emit must stamp events lacking a timestamp")
	}
	if time.Since(got.Timestamp) > time.Minute {
		t.Errorf("timestamp looks wrong: %s", got.Timestamp)
	}

	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sink.Emit(context.Background(), Event{Type: EventTypeLog, Timestamp: fixed})
	if !got.Timestamp.Equal(fixed) {
		t.Error("Emit must preserve an explicit timestamp")
	}
}

func TestCapabilitySetListIsSortedAndStable(t *testing.T) {
	t.Parallel()

	set := NewCapabilitySet(CapabilityShell, CapabilityStreaming, CapabilityTools)
	list := set.List()
	for i := 1; i < len(list); i++ {
		if list[i-1] > list[i] {
			t.Fatalf("List must be sorted, got %v", list)
		}
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 capabilities, got %v", list)
	}
	if set.Has("nonexistent") {
		t.Error("Has must reject unknown capabilities")
	}
}

func TestUsageCostDistinguishesUnknownFromZero(t *testing.T) {
	t.Parallel()

	// A provider that reports no cost is not the same as a free call; the
	// cost controls depend on telling these apart.
	var unknown Usage
	if unknown.CostUSD != nil {
		t.Fatal("zero Usage must report unknown cost, not zero cost")
	}

	free := 0.0
	known := Usage{CostUSD: &free, CostEstimated: true}
	if known.CostUSD == nil || *known.CostUSD != 0 {
		t.Fatal("an explicit zero cost must be representable")
	}
	if !known.CostEstimated {
		t.Fatal("estimated cost must be flagged as such")
	}
}
