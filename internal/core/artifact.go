package core

import (
	"context"
	"encoding/json"
	"time"
)

// Artifact is structured output one workflow step produced for later steps
// (or a human) to read. Agents communicate only through artifacts and task
// state, never directly with one another.
type Artifact struct {
	ID        int64
	TaskID    string
	StepID    string
	Name      string
	Agent     string
	Content   json.RawMessage
	CreatedAt time.Time
}

// ArtifactRepository persists artifacts.
type ArtifactRepository interface {
	// Save stores an artifact. Saving under the same (task, step) twice
	// replaces the row, so re-running a step is idempotent.
	Save(ctx context.Context, a *Artifact) (*Artifact, error)

	// ListByTask returns a task's artifacts, oldest first.
	ListByTask(ctx context.Context, taskID string) ([]*Artifact, error)

	// Latest returns the most recent artifact with the given name for a
	// task, or an error wrapping ErrNotFound.
	Latest(ctx context.Context, taskID, name string) (*Artifact, error)
}
