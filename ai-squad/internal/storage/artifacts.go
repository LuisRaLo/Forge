package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/santillana/ai-squad/internal/core"
)

const artifactColumns = `id, task_id, step_id, name, agent, content, created_at`

// ArtifactRepo is the SQLite implementation of core.ArtifactRepository.
type ArtifactRepo struct {
	db    *DB
	clock core.Clock
}

// NewArtifactRepo builds an artifact repository. A nil clock uses the system
// clock.
func NewArtifactRepo(db *DB, clock core.Clock) *ArtifactRepo {
	if clock == nil {
		clock = core.SystemClock
	}
	return &ArtifactRepo{db: db, clock: clock}
}

var _ core.ArtifactRepository = (*ArtifactRepo)(nil)

// Save stores an artifact, replacing any prior one for the same (task, step).
func (r *ArtifactRepo) Save(ctx context.Context, a *core.Artifact) (*core.Artifact, error) {
	if a == nil {
		return nil, core.Invalid("artifact", "must not be nil")
	}
	if a.TaskID == "" {
		return nil, core.Invalid("artifact.task_id", "must be set")
	}
	if a.StepID == "" {
		return nil, core.Invalid("artifact.step_id", "must be set")
	}
	if a.Name == "" {
		return nil, core.Invalid("artifact.name", "must be set")
	}
	if len(a.Content) == 0 || !json.Valid(a.Content) {
		return nil, core.Invalid("artifact.content", "must be valid, non-empty JSON")
	}

	saved := *a
	saved.CreatedAt = r.clock()

	err := withTx(ctx, r.db.DB, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO artifacts (task_id, step_id, name, agent, content, created_at)
			VALUES (?,?,?,?,?,?)
			ON CONFLICT (task_id, step_id) DO UPDATE SET
				name = excluded.name, agent = excluded.agent,
				content = excluded.content, created_at = excluded.created_at`,
			saved.TaskID, saved.StepID, saved.Name, saved.Agent,
			string(saved.Content), formatTime(saved.CreatedAt),
		); err != nil {
			return fmt.Errorf("save artifact: %w", err)
		}
		row := tx.QueryRowContext(ctx,
			`SELECT id FROM artifacts WHERE task_id = ? AND step_id = ?`, saved.TaskID, saved.StepID)
		return row.Scan(&saved.ID)
	})
	if err != nil {
		return nil, err
	}
	return &saved, nil
}

// ListByTask returns a task's artifacts, oldest first.
func (r *ArtifactRepo) ListByTask(ctx context.Context, taskID string) ([]*core.Artifact, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+artifactColumns+` FROM artifacts WHERE task_id = ? ORDER BY id ASC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list artifacts for %s: %w", taskID, err)
	}
	defer rows.Close()

	out := []*core.Artifact{}
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, fmt.Errorf("scan artifact: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate artifacts: %w", err)
	}
	return out, nil
}

// Latest returns the most recent artifact with the given name for a task.
func (r *ArtifactRepo) Latest(ctx context.Context, taskID, name string) (*core.Artifact, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT `+artifactColumns+` FROM artifacts
		WHERE task_id = ? AND name = ? ORDER BY id DESC LIMIT 1`, taskID, name)

	a, err := scanArtifact(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("artifact %s for task %s: %w", name, taskID, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get latest artifact: %w", err)
	}
	return a, nil
}

func scanArtifact(s rowScanner) (*core.Artifact, error) {
	var (
		a         core.Artifact
		content   string
		createdAt string
	)
	if err := s.Scan(&a.ID, &a.TaskID, &a.StepID, &a.Name, &a.Agent, &content, &createdAt); err != nil {
		return nil, err
	}
	a.Content = json.RawMessage(content)
	var err error
	if a.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	return &a, nil
}
