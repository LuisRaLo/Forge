package events

import (
	"context"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

// PublishingTaskRepository decorates a core.TaskRepository, publishing an
// Event to a Bus after every successful Create and Transition. Reads pass
// straight through. Wrapping the repository — rather than teaching
// internal/scheduler or internal/storage about events directly — is what
// keeps the orchestrator core unaware that anything is listening; only
// internal/cli's `serve` wiring knows a Bus exists.
type PublishingTaskRepository struct {
	core.TaskRepository
	bus *Bus
}

// NewPublishingTaskRepository wraps repo, publishing to bus.
func NewPublishingTaskRepository(repo core.TaskRepository, bus *Bus) *PublishingTaskRepository {
	return &PublishingTaskRepository{TaskRepository: repo, bus: bus}
}

var _ core.TaskRepository = (*PublishingTaskRepository)(nil)

// Create inserts the task, then publishes TypeTaskCreated.
func (r *PublishingTaskRepository) Create(ctx context.Context, t *core.Task) (*core.Task, error) {
	created, err := r.TaskRepository.Create(ctx, t)
	if err != nil {
		return nil, err
	}
	r.bus.Publish(Event{Type: TypeTaskCreated, TaskID: created.ID, Task: created})
	return created, nil
}

// Transition applies the transition, then publishes TypeTaskTransition. A
// transition rejected by the state machine (an error) is never published:
// nothing happened, so there is nothing to tell a listener.
func (r *PublishingTaskRepository) Transition(
	ctx context.Context, id string, to core.TaskStatus, reason string, mut func(*core.Task),
) (*core.Task, error) {
	before, beforeErr := r.TaskRepository.Get(ctx, id)

	updated, err := r.TaskRepository.Transition(ctx, id, to, reason, mut)
	if err != nil {
		return nil, err
	}

	from := ""
	if beforeErr == nil {
		from = string(before.Status)
	}
	r.bus.Publish(Event{
		Type: TypeTaskTransition, TaskID: id, Task: updated,
		From: from, To: string(to), Reason: reason,
	})
	return updated, nil
}
