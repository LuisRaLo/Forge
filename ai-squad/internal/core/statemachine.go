package core

import "sort"

// transitions is the single source of truth for the task lifecycle. Any
// transition absent from this table is rejected; there are no implicit edges.
//
// Happy path:
//
//	PENDING -> PLANNING -> READY -> RUNNING -> REVIEW -> COMPLETED
//
// QA rejection re-enters implementation:
//
//	REVIEW -> RUNNING
//
// Deployment to production is never automatic; it goes through
// WAITING_APPROVAL and requires an explicit human decision.
var transitions = map[TaskStatus][]TaskStatus{
	StatusPending: {
		StatusPlanning, StatusReady, StatusBlocked, StatusCancelled,
	},
	StatusPlanning: {
		StatusReady, StatusWaiting, StatusFailed, StatusBlocked, StatusCancelled,
	},
	StatusReady: {
		StatusRunning, StatusBlocked, StatusCancelled,
	},
	StatusRunning: {
		StatusReview, StatusWaiting, StatusWaitingApproval,
		StatusCompleted, StatusFailed, StatusBlocked, StatusCancelled,
	},
	StatusWaiting: {
		StatusReady, StatusRunning, StatusFailed, StatusBlocked, StatusCancelled,
	},
	StatusReview: {
		// RUNNING is the QA/CI feedback loop sending work back to the
		// implementer. It is bounded by Task.MaxAttempts, never unbounded.
		StatusRunning, StatusWaitingApproval, StatusCompleted,
		StatusFailed, StatusBlocked, StatusCancelled,
	},
	StatusWaitingApproval: {
		StatusRunning, StatusCompleted, StatusFailed, StatusBlocked, StatusCancelled,
	},
	StatusFailed: {
		// Retry re-queues the task; it does not jump straight to RUNNING so
		// that the scheduler remains the only component that starts work.
		StatusReady, StatusBlocked, StatusCancelled,
	},
	StatusBlocked: {
		StatusReady, StatusCancelled,
	},
	StatusCompleted: {},
	StatusCancelled: {},
}

// CanTransition reports whether from -> to is a permitted edge.
func CanTransition(from, to TaskStatus) bool {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// ValidateTransition returns nil when from -> to is permitted, otherwise a
// *TransitionError wrapping ErrInvalidTransition.
func ValidateTransition(taskID string, from, to TaskStatus) error {
	if !from.Valid() {
		return Invalid("status", "unknown source status "+string(from))
	}
	if !to.Valid() {
		return Invalid("status", "unknown target status "+string(to))
	}
	if from == to {
		// Self-transitions are rejected so that callers cannot use them to
		// mask a missed state change. Idempotent no-ops are handled one
		// layer up, in the task service.
		return &TransitionError{TaskID: taskID, From: from, To: to}
	}
	if !CanTransition(from, to) {
		return &TransitionError{TaskID: taskID, From: from, To: to}
	}
	return nil
}

// NextStates lists the permitted successors of from, sorted for stable output.
func NextStates(from TaskStatus) []TaskStatus {
	out := append([]TaskStatus(nil), transitions[from]...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
