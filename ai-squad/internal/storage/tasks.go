package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/santillana/ai-squad/internal/core"
)

// timeLayout is the on-disk timestamp format: RFC3339 in UTC with nanosecond
// precision, so values sort lexicographically.
const timeLayout = time.RFC3339Nano

// taskColumns is the canonical select list, kept in one place so scanTask and
// every query stay in step.
const taskColumns = `id, title, description, repository, branch, workflow, step,
	agent, status, priority, attempts, max_attempts, parent_task_id,
	workspace_path, last_error, metadata, idempotency_key,
	created_at, updated_at, started_at, completed_at`

// TaskRepo is the SQLite implementation of core.TaskRepository.
type TaskRepo struct {
	db    *DB
	clock core.Clock
}

// NewTaskRepo builds a task repository. A nil clock falls back to the system
// clock.
func NewTaskRepo(db *DB, clock core.Clock) *TaskRepo {
	if clock == nil {
		clock = core.SystemClock
	}
	return &TaskRepo{db: db, clock: clock}
}

// compile-time proof that the repository satisfies the domain port.
var _ core.TaskRepository = (*TaskRepo)(nil)

// Create validates and inserts a task, assigning it a TASK-N identifier.
func (r *TaskRepo) Create(ctx context.Context, t *core.Task) (*core.Task, error) {
	if t == nil {
		return nil, core.Invalid("task", "must not be nil")
	}

	created := *t // copy, so a failed insert cannot mutate the caller's task
	if created.Status == "" {
		created.Status = core.StatusPending
	}
	if created.Priority == 0 {
		created.Priority = core.PriorityNormal
	}
	if created.MaxAttempts == 0 {
		created.MaxAttempts = 3
	}
	now := r.clock()
	created.CreatedAt = now
	created.UpdatedAt = now

	if err := created.Validate(); err != nil {
		return nil, err
	}

	metadata, err := encodeMetadata(created.Metadata)
	if err != nil {
		return nil, err
	}

	err = withTx(ctx, r.db.DB, func(tx *sql.Tx) error {
		if created.ID == "" {
			id, err := nextTaskID(ctx, tx)
			if err != nil {
				return err
			}
			created.ID = id
		}

		_, err := tx.ExecContext(ctx, `
			INSERT INTO tasks (
				id, title, description, repository, branch, workflow, step,
				agent, status, priority, attempts, max_attempts,
				parent_task_id, workspace_path, last_error, metadata,
				idempotency_key, created_at, updated_at, started_at, completed_at
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			created.ID, created.Title, created.Description, created.Repository,
			created.Branch, created.Workflow, created.Step, created.Agent,
			string(created.Status), int(created.Priority), created.Attempts,
			created.MaxAttempts, created.ParentTaskID, created.WorkspacePath,
			created.LastError, metadata, nullableString(created.IdempotencyKey),
			formatTime(now), formatTime(now),
			formatNullableTime(created.StartedAt), formatNullableTime(created.CompletedAt),
		)
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("task %s: %w", created.ID, core.ErrAlreadyExists)
			}
			return fmt.Errorf("insert task: %w", err)
		}

		return insertEvent(ctx, tx, created.ID, "", created.Status, "created", now)
	})
	if err != nil {
		return nil, err
	}
	return &created, nil
}

// Get returns one task by identifier.
func (r *TaskRepo) Get(ctx context.Context, id string) (*core.Task, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id)

	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("task %s: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get task %s: %w", id, err)
	}
	return t, nil
}

// GetByIdempotencyKey returns the task previously created under key.
func (r *TaskRepo) GetByIdempotencyKey(ctx context.Context, key string) (*core.Task, error) {
	if key == "" {
		return nil, core.Invalid("idempotency_key", "must not be empty")
	}
	row := r.db.QueryRowContext(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE idempotency_key = ?`, key)

	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("idempotency key %s: %w", key, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get task by idempotency key: %w", err)
	}
	return t, nil
}

// List returns tasks matching the filter, most urgent first.
func (r *TaskRepo) List(ctx context.Context, f core.TaskFilter) ([]*core.Task, error) {
	var (
		where []string
		args  []any
	)

	if len(f.Statuses) > 0 {
		placeholders := make([]string, len(f.Statuses))
		for i, s := range f.Statuses {
			placeholders[i] = "?"
			args = append(args, string(s))
		}
		where = append(where, "status IN ("+strings.Join(placeholders, ",")+")")
	}
	if f.Repository != "" {
		where = append(where, "repository = ?")
		args = append(args, f.Repository)
	}
	if f.Workflow != "" {
		where = append(where, "workflow = ?")
		args = append(args, f.Workflow)
	}
	if f.Agent != "" {
		where = append(where, "agent = ?")
		args = append(args, f.Agent)
	}
	if f.ParentID != nil {
		where = append(where, "parent_task_id = ?")
		args = append(args, *f.ParentID)
	}

	query := `SELECT ` + taskColumns + ` FROM tasks`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY priority DESC, created_at ASC, id ASC"
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	// Scan everything before returning: the pool holds a single connection,
	// so an open rows cursor must not outlive this function.
	out := []*core.Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks: %w", err)
	}
	return out, nil
}

// Save persists the mutable fields of an existing task. It deliberately does
// not write status: every status change must go through Transition so that the
// state machine cannot be bypassed.
func (r *TaskRepo) Save(ctx context.Context, t *core.Task) error {
	if t == nil {
		return core.Invalid("task", "must not be nil")
	}
	if t.ID == "" {
		return core.Invalid("task.id", "must be set")
	}
	if err := t.Validate(); err != nil {
		return err
	}

	metadata, err := encodeMetadata(t.Metadata)
	if err != nil {
		return err
	}
	now := r.clock()

	res, err := r.db.ExecContext(ctx, `
		UPDATE tasks SET
			title = ?, description = ?, repository = ?, branch = ?,
			workflow = ?, step = ?, agent = ?, priority = ?, attempts = ?,
			max_attempts = ?, parent_task_id = ?, workspace_path = ?,
			last_error = ?, metadata = ?, updated_at = ?,
			started_at = ?, completed_at = ?
		WHERE id = ?`,
		t.Title, t.Description, t.Repository, t.Branch, t.Workflow, t.Step,
		t.Agent, int(t.Priority), t.Attempts, t.MaxAttempts, t.ParentTaskID,
		t.WorkspacePath, t.LastError, metadata, formatTime(now),
		formatNullableTime(t.StartedAt), formatNullableTime(t.CompletedAt),
		t.ID)
	if err != nil {
		return fmt.Errorf("save task %s: %w", t.ID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("save task %s: %w", t.ID, err)
	}
	if affected == 0 {
		return fmt.Errorf("task %s: %w", t.ID, core.ErrNotFound)
	}
	t.UpdatedAt = now
	return nil
}

// Transition atomically moves a task to a new status.
//
// The task is re-read inside the transaction and the state machine is checked
// against that fresh copy, so two workers racing on the same task cannot both
// succeed: the loser sees the winner's status and is rejected with a
// *core.TransitionError.
func (r *TaskRepo) Transition(
	ctx context.Context,
	id string,
	to core.TaskStatus,
	reason string,
	mut func(*core.Task),
) (*core.Task, error) {
	var updated *core.Task

	err := withTx(ctx, r.db.DB, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id)
		current, err := scanTask(row)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("task %s: %w", id, core.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("load task %s: %w", id, err)
		}

		if err := core.ValidateTransition(id, current.Status, to); err != nil {
			return err
		}

		from := current.Status
		now := r.clock()
		current.Status = to
		current.UpdatedAt = now

		switch {
		case to == core.StatusRunning && current.StartedAt == nil:
			started := now
			current.StartedAt = &started
		case to.Terminal():
			completed := now
			current.CompletedAt = &completed
		}

		if mut != nil {
			mut(current)
		}
		// The mutator may not rewrite identity or status.
		current.ID = id
		current.Status = to

		if err := current.Validate(); err != nil {
			return err
		}

		metadata, err := encodeMetadata(current.Metadata)
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks SET
				title = ?, description = ?, repository = ?, branch = ?,
				workflow = ?, step = ?, agent = ?, status = ?, priority = ?,
				attempts = ?, max_attempts = ?, parent_task_id = ?,
				workspace_path = ?, last_error = ?, metadata = ?,
				updated_at = ?, started_at = ?, completed_at = ?
			WHERE id = ? AND status = ?`,
			current.Title, current.Description, current.Repository,
			current.Branch, current.Workflow, current.Step, current.Agent,
			string(to), int(current.Priority), current.Attempts,
			current.MaxAttempts, current.ParentTaskID, current.WorkspacePath,
			current.LastError, metadata, formatTime(now),
			formatNullableTime(current.StartedAt),
			formatNullableTime(current.CompletedAt),
			id, string(from),
		); err != nil {
			return fmt.Errorf("update task %s: %w", id, err)
		}

		if err := insertEvent(ctx, tx, id, from, to, reason, now); err != nil {
			return err
		}

		updated = current
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// Events returns a task's audit trail, oldest first.
func (r *TaskRepo) Events(ctx context.Context, id string) ([]core.TaskEvent, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, task_id, from_status, to_status, reason, created_at
		FROM task_events WHERE task_id = ? ORDER BY id ASC`, id)
	if err != nil {
		return nil, fmt.Errorf("list events for %s: %w", id, err)
	}
	defer rows.Close()

	out := []core.TaskEvent{}
	for rows.Next() {
		var (
			ev        core.TaskEvent
			from, to  string
			createdAt string
		)
		if err := rows.Scan(&ev.ID, &ev.TaskID, &from, &to, &ev.Reason, &createdAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		ev.FromStatus = core.TaskStatus(from)
		ev.ToStatus = core.TaskStatus(to)
		ev.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	return out, nil
}

// nextTaskID atomically increments the task counter and formats an identifier.
func nextTaskID(ctx context.Context, tx *sql.Tx) (string, error) {
	var n int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO counters (name, value) VALUES ('task', 1)
		ON CONFLICT(name) DO UPDATE SET value = value + 1
		RETURNING value`).Scan(&n)
	if err != nil {
		return "", fmt.Errorf("allocate task id: %w", err)
	}
	return fmt.Sprintf("TASK-%d", n), nil
}

func insertEvent(
	ctx context.Context, tx *sql.Tx,
	taskID string, from, to core.TaskStatus, reason string, at time.Time,
) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO task_events (task_id, from_status, to_status, reason, created_at)
		VALUES (?,?,?,?,?)`,
		taskID, string(from), string(to), reason, formatTime(at))
	if err != nil {
		return fmt.Errorf("record task event: %w", err)
	}
	return nil
}

// rowScanner abstracts *sql.Row and *sql.Rows so scanTask serves both.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(s rowScanner) (*core.Task, error) {
	var (
		t              core.Task
		status         string
		priority       int
		parentID       sql.NullString
		metadata       string
		idempotencyKey sql.NullString
		createdAt      string
		updatedAt      string
		startedAt      sql.NullString
		completedAt    sql.NullString
	)

	if err := s.Scan(
		&t.ID, &t.Title, &t.Description, &t.Repository, &t.Branch,
		&t.Workflow, &t.Step, &t.Agent, &status, &priority, &t.Attempts,
		&t.MaxAttempts, &parentID, &t.WorkspacePath, &t.LastError, &metadata,
		&idempotencyKey, &createdAt, &updatedAt, &startedAt, &completedAt,
	); err != nil {
		return nil, err
	}

	t.Status = core.TaskStatus(status)
	t.Priority = core.Priority(priority)
	if parentID.Valid {
		id := parentID.String
		t.ParentTaskID = &id
	}
	t.IdempotencyKey = idempotencyKey.String

	var err error
	if t.Metadata, err = decodeMetadata(metadata); err != nil {
		return nil, err
	}
	if t.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if t.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	if t.StartedAt, err = parseNullableTime(startedAt); err != nil {
		return nil, err
	}
	if t.CompletedAt, err = parseNullableTime(completedAt); err != nil {
		return nil, err
	}
	return &t, nil
}

func encodeMetadata(m map[string]string) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("encode task metadata: %w", err)
	}
	return string(b), nil
}

func decodeMetadata(s string) (map[string]string, error) {
	if s == "" || s == "{}" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("decode task metadata: %w", err)
	}
	return m, nil
}

// nullableString maps the empty string to SQL NULL, so that a UNIQUE index
// tolerates many rows without a key.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func formatNullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

func parseNullableTime(s sql.NullString) (*time.Time, error) {
	if !s.Valid || s.String == "" {
		return nil, nil
	}
	t, err := parseTime(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// isUniqueViolation reports whether err is a SQLite uniqueness failure. The
// driver does not export a typed error for this, so the message is inspected.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE CONSTRAINT")
}
