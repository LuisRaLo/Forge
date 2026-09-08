package core

import (
	"errors"
	"fmt"
)

// Sentinel errors for the domain. Callers match with errors.Is.
var (
	ErrNotFound          = errors.New("not found")
	ErrAlreadyExists     = errors.New("already exists")
	ErrValidation        = errors.New("validation failed")
	ErrInvalidTransition = errors.New("invalid state transition")
	ErrTerminal          = errors.New("task is in a terminal state")
	ErrMaxAttempts       = errors.New("maximum attempts exceeded")
)

// ValidationError reports a single invalid field. It wraps ErrValidation so
// that errors.Is(err, ErrValidation) holds anywhere up the call chain.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Reason)
}

func (e *ValidationError) Unwrap() error { return ErrValidation }

// Invalid builds a ValidationError.
func Invalid(field, reason string) error {
	return &ValidationError{Field: field, Reason: reason}
}

// Invalidf builds a ValidationError with a formatted reason.
func Invalidf(field, format string, args ...any) error {
	return &ValidationError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// TransitionError reports a rejected state machine transition.
type TransitionError struct {
	TaskID string
	From   TaskStatus
	To     TaskStatus
}

func (e *TransitionError) Error() string {
	if e.TaskID != "" {
		return fmt.Sprintf("task %s: cannot transition %s -> %s", e.TaskID, e.From, e.To)
	}
	return fmt.Sprintf("cannot transition %s -> %s", e.From, e.To)
}

func (e *TransitionError) Unwrap() error { return ErrInvalidTransition }
