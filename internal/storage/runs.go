package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

const runColumns = `id, task_id, step_id, agent, runtime, status, session_id,
	stop_reason, error, permission_denials, model, input_tokens, output_tokens,
	cache_read_tokens, cache_creation_tokens, cost_usd, cost_estimated,
	duration_ms, started_at, finished_at, transcript`

// RunRepo is the SQLite implementation of core.RunRepository.
type RunRepo struct{ db *DB }

// NewRunRepo builds a run repository.
func NewRunRepo(db *DB) *RunRepo { return &RunRepo{db: db} }

var _ core.RunRepository = (*RunRepo)(nil)

// Record stores a run. It is idempotent per (task_id, step_id): recording the
// same step twice — the natural shape of a retry-safe caller — replaces the
// row instead of accumulating duplicates that would double-count cost.
func (r *RunRepo) Record(ctx context.Context, run *core.AgentRun) (*core.AgentRun, error) {
	if run == nil {
		return nil, core.Invalid("run", "must not be nil")
	}
	if run.TaskID == "" {
		return nil, core.Invalid("run.task_id", "must be set")
	}
	if run.StepID == "" {
		return nil, core.Invalid("run.step_id", "must be set")
	}
	if !run.Status.Valid() {
		return nil, core.Invalid("run.status", "unknown status "+string(run.Status))
	}

	saved := *run
	if saved.FinishedAt.IsZero() {
		saved.FinishedAt = core.SystemClock()
	}
	if saved.StartedAt.IsZero() {
		saved.StartedAt = saved.FinishedAt.Add(-saved.Duration)
	}

	denials, err := json.Marshal(saved.PermissionDenials)
	if err != nil {
		return nil, fmt.Errorf("encode permission denials: %w", err)
	}
	transcript, err := json.Marshal(saved.Transcript)
	if err != nil {
		return nil, fmt.Errorf("encode transcript: %w", err)
	}

	err = withTx(ctx, r.db.DB, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO agent_runs (
				task_id, step_id, agent, runtime, status, session_id,
				stop_reason, error, permission_denials, model, input_tokens,
				output_tokens, cache_read_tokens, cache_creation_tokens,
				cost_usd, cost_estimated, duration_ms, started_at, finished_at,
				transcript
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (task_id, step_id) DO UPDATE SET
				agent = excluded.agent, runtime = excluded.runtime,
				status = excluded.status, session_id = excluded.session_id,
				stop_reason = excluded.stop_reason, error = excluded.error,
				permission_denials = excluded.permission_denials,
				model = excluded.model, input_tokens = excluded.input_tokens,
				output_tokens = excluded.output_tokens,
				cache_read_tokens = excluded.cache_read_tokens,
				cache_creation_tokens = excluded.cache_creation_tokens,
				cost_usd = excluded.cost_usd,
				cost_estimated = excluded.cost_estimated,
				duration_ms = excluded.duration_ms,
				started_at = excluded.started_at, finished_at = excluded.finished_at,
				transcript = excluded.transcript`,
			saved.TaskID, saved.StepID, saved.Agent, saved.Runtime, string(saved.Status),
			saved.SessionID, saved.StopReason, saved.Error, string(denials), saved.Usage.Model,
			saved.Usage.InputTokens, saved.Usage.OutputTokens,
			saved.Usage.CacheReadTokens, saved.Usage.CacheCreationTokens,
			nullableCost(saved.Usage.CostUSD), saved.Usage.CostEstimated,
			saved.Duration.Milliseconds(), formatTime(saved.StartedAt), formatTime(saved.FinishedAt),
			string(transcript),
		)
		if err != nil {
			return fmt.Errorf("record agent run: %w", err)
		}

		// SQLite's upsert does not return the row id on the replace branch
		// via LastInsertId reliably across driver versions, so read it back
		// by the unique key instead of trusting res.
		_ = res
		row := tx.QueryRowContext(ctx,
			`SELECT id FROM agent_runs WHERE task_id = ? AND step_id = ?`,
			saved.TaskID, saved.StepID)
		return row.Scan(&saved.ID)
	})
	if err != nil {
		return nil, err
	}
	return &saved, nil
}

// ListByTask returns a task's runs, oldest first.
func (r *RunRepo) ListByTask(ctx context.Context, taskID string) ([]*core.AgentRun, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM agent_runs WHERE task_id = ? ORDER BY id ASC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list runs for %s: %w", taskID, err)
	}
	defer rows.Close()

	out := []*core.AgentRun{}
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runs: %w", err)
	}
	return out, nil
}

// CostSince totals reported spend since a point in time. Runs with an unknown
// cost are counted but excluded from the sum, so the caller can tell "we spent
// $4.10" from "we spent at least $4.10 and cannot see N more runs".
func (r *RunRepo) CostSince(ctx context.Context, since time.Time) (float64, int, error) {
	var total sql.NullFloat64
	var unknown int

	err := r.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(cost_usd), 0),
		       COALESCE(SUM(CASE WHEN cost_usd IS NULL THEN 1 ELSE 0 END), 0)
		FROM agent_runs WHERE started_at >= ?`,
		formatTime(since)).Scan(&total, &unknown)
	if err != nil {
		return 0, 0, fmt.Errorf("sum cost since %s: %w", since, err)
	}
	return total.Float64, unknown, nil
}

func scanRun(rows *sql.Rows) (*core.AgentRun, error) {
	var (
		run                   core.AgentRun
		status                string
		permissionDenials     string
		costUSD               sql.NullFloat64
		costEstimated         bool
		durationMS            int64
		startedAt, finishedAt string
		transcript            string
	)
	if err := rows.Scan(
		&run.ID, &run.TaskID, &run.StepID, &run.Agent, &run.Runtime, &status,
		&run.SessionID, &run.StopReason, &run.Error, &permissionDenials, &run.Usage.Model,
		&run.Usage.InputTokens, &run.Usage.OutputTokens,
		&run.Usage.CacheReadTokens, &run.Usage.CacheCreationTokens,
		&costUSD, &costEstimated, &durationMS, &startedAt, &finishedAt, &transcript,
	); err != nil {
		return nil, err
	}

	if permissionDenials != "" {
		if err := json.Unmarshal([]byte(permissionDenials), &run.PermissionDenials); err != nil {
			return nil, fmt.Errorf("decode permission denials: %w", err)
		}
	}
	if transcript != "" {
		if err := json.Unmarshal([]byte(transcript), &run.Transcript); err != nil {
			return nil, fmt.Errorf("decode transcript: %w", err)
		}
	}
	run.Status = core.RunStatus(status)
	run.Usage.CostEstimated = costEstimated
	if costUSD.Valid {
		v := costUSD.Float64
		run.Usage.CostUSD = &v
	}
	run.Duration = time.Duration(durationMS) * time.Millisecond

	var err error
	if run.StartedAt, err = parseTime(startedAt); err != nil {
		return nil, err
	}
	if run.FinishedAt, err = parseTime(finishedAt); err != nil {
		return nil, err
	}
	return &run, nil
}

func nullableCost(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}
