package core

import (
	"errors"
	"testing"
)

func TestCanTransitionHappyPath(t *testing.T) {
	t.Parallel()

	// The documented feature path must be walkable end to end.
	path := []TaskStatus{
		StatusPending, StatusPlanning, StatusReady,
		StatusRunning, StatusReview, StatusCompleted,
	}
	for i := 0; i < len(path)-1; i++ {
		from, to := path[i], path[i+1]
		if !CanTransition(from, to) {
			t.Errorf("expected %s -> %s to be allowed", from, to)
		}
	}
}

func TestCanTransitionQAFeedbackLoop(t *testing.T) {
	t.Parallel()

	// REVIEW -> RUNNING is what sends failed QA back to the implementer.
	if !CanTransition(StatusReview, StatusRunning) {
		t.Fatal("QA feedback loop REVIEW -> RUNNING must be allowed")
	}
	if !CanTransition(StatusRunning, StatusReview) {
		t.Fatal("RUNNING -> REVIEW must be allowed")
	}
}

func TestCanTransitionRejectsArbitraryEdges(t *testing.T) {
	t.Parallel()

	rejected := []struct{ from, to TaskStatus }{
		// Work must be scheduled, never started directly from PENDING.
		{StatusPending, StatusRunning},
		// A completed task is immutable.
		{StatusCompleted, StatusRunning},
		{StatusCompleted, StatusFailed},
		{StatusCancelled, StatusReady},
		// Retry re-queues; it does not jump the scheduler.
		{StatusFailed, StatusRunning},
		// Approval cannot be skipped by going straight to done from review
		// of a blocked task.
		{StatusBlocked, StatusRunning},
		{StatusBlocked, StatusCompleted},
		// A task cannot report success without ever having run.
		{StatusPending, StatusCompleted},
		{StatusReady, StatusCompleted},
	}
	for _, tc := range rejected {
		if CanTransition(tc.from, tc.to) {
			t.Errorf("expected %s -> %s to be rejected", tc.from, tc.to)
		}
	}
}

func TestTerminalStatesHaveNoSuccessors(t *testing.T) {
	t.Parallel()

	for _, s := range []TaskStatus{StatusCompleted, StatusCancelled} {
		if !s.Terminal() {
			t.Errorf("%s should report Terminal() == true", s)
		}
		if got := NextStates(s); len(got) != 0 {
			t.Errorf("%s should have no successors, got %v", s, got)
		}
		for _, to := range AllStatuses() {
			if CanTransition(s, to) {
				t.Errorf("terminal %s must not transition to %s", s, to)
			}
		}
	}
}

func TestEveryStatusIsReachableExceptPending(t *testing.T) {
	t.Parallel()

	reachable := map[TaskStatus]bool{}
	for _, from := range AllStatuses() {
		for _, to := range NextStates(from) {
			reachable[to] = true
		}
	}
	for _, s := range AllStatuses() {
		if s == StatusPending {
			continue // the entry state, reached by creation
		}
		if !reachable[s] {
			t.Errorf("status %s is unreachable: no transition leads to it", s)
		}
	}
}

func TestEveryNonTerminalStatusCanBeCancelled(t *testing.T) {
	t.Parallel()

	// An operator must always be able to stop work in flight.
	for _, s := range AllStatuses() {
		if s.Terminal() {
			continue
		}
		if !CanTransition(s, StatusCancelled) {
			t.Errorf("status %s must be cancellable", s)
		}
	}
}

func TestValidateTransition(t *testing.T) {
	t.Parallel()

	t.Run("allowed edge returns nil", func(t *testing.T) {
		if err := ValidateTransition("TASK-1", StatusReady, StatusRunning); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("rejected edge wraps ErrInvalidTransition", func(t *testing.T) {
		err := ValidateTransition("TASK-1", StatusCompleted, StatusRunning)
		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("expected ErrInvalidTransition, got %v", err)
		}
		var te *TransitionError
		if !errors.As(err, &te) {
			t.Fatalf("expected *TransitionError, got %T", err)
		}
		if te.TaskID != "TASK-1" || te.From != StatusCompleted || te.To != StatusRunning {
			t.Errorf("transition error lost context: %+v", te)
		}
	})

	t.Run("self transition is rejected", func(t *testing.T) {
		err := ValidateTransition("TASK-1", StatusRunning, StatusRunning)
		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("self transition must be rejected, got %v", err)
		}
	})

	t.Run("unknown status is a validation error", func(t *testing.T) {
		if err := ValidateTransition("TASK-1", TaskStatus("NOPE"), StatusReady); !errors.Is(err, ErrValidation) {
			t.Fatalf("expected ErrValidation, got %v", err)
		}
		if err := ValidateTransition("TASK-1", StatusReady, TaskStatus("NOPE")); !errors.Is(err, ErrValidation) {
			t.Fatalf("expected ErrValidation, got %v", err)
		}
	})
}

func TestNextStatesIsSortedAndCopied(t *testing.T) {
	t.Parallel()

	got := NextStates(StatusRunning)
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("NextStates must be sorted, got %v", got)
		}
	}
	// Mutating the result must not corrupt the transition table.
	if len(got) > 0 {
		got[0] = "MUTATED"
	}
	if second := NextStates(StatusRunning); second[0] == "MUTATED" {
		t.Fatal("NextStates leaked the internal transition table")
	}
}

func TestStatusValidAndActive(t *testing.T) {
	t.Parallel()

	if TaskStatus("bogus").Valid() {
		t.Error("bogus status must not be valid")
	}
	for _, s := range AllStatuses() {
		if !s.Valid() {
			t.Errorf("%s should be valid", s)
		}
	}
	if !StatusRunning.Active() || !StatusPlanning.Active() {
		t.Error("RUNNING and PLANNING are worker-owned states")
	}
	if StatusReady.Active() {
		t.Error("READY is queued, not active")
	}
}

func TestParseStatus(t *testing.T) {
	t.Parallel()

	got, err := ParseStatus("  running ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != StatusRunning {
		t.Errorf("expected RUNNING, got %s", got)
	}
	if _, err := ParseStatus("nope"); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}
