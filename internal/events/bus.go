// Package events provides a small in-process publish/subscribe bus used to
// push task state changes to real-time listeners (the web UI's WebSocket
// connections) the moment they happen, rather than making a listener poll
// the database. Nothing in internal/core, internal/scheduler or
// internal/storage imports this package — publishing is wired in at the
// edge (internal/cli's `serve` command), via PublishingTaskRepository,
// keeping the orchestrator core unaware that anything is listening.
package events

import (
	"sync"
	"time"
)

// Type identifies what kind of thing happened.
type Type string

const (
	TypeTaskCreated    Type = "task_created"
	TypeTaskTransition Type = "task_transition"
)

// Event is one thing that happened, broadcast to every subscriber.
type Event struct {
	Type      Type      `json:"type"`
	TaskID    string    `json:"task_id"`
	Timestamp time.Time `json:"timestamp"`
	// Task is the task's state after the event, when available, so a
	// listener can update its view without a follow-up fetch.
	Task any `json:"task,omitempty"`
	// From/To are populated for TypeTaskTransition.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Reason mirrors the audit reason recorded for the transition.
	Reason string `json:"reason,omitempty"`
}

// Bus is a small, in-memory fan-out publisher. The zero value is ready to
// use.
type Bus struct {
	mu   sync.Mutex
	subs map[int]chan Event
	next int
}

// Subscribe registers a new listener with the given channel buffer size and
// returns the channel to read from and a function to unsubscribe. Callers
// must call the unsubscribe function when done, or the channel leaks.
func (b *Bus) Subscribe(buffer int) (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs == nil {
		b.subs = map[int]chan Event{}
	}
	if buffer < 1 {
		buffer = 1
	}
	id := b.next
	b.next++
	ch := make(chan Event, buffer)
	b.subs[id] = ch

	unsubscribe := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if existing, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(existing)
		}
	}
	return ch, unsubscribe
}

// Publish broadcasts ev to every current subscriber. A subscriber whose
// buffer is full has the event dropped for it rather than blocking the
// publisher — a slow or stalled UI client must never be able to stall the
// orchestrator itself.
func (b *Bus) Publish(ev Event) {
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}
