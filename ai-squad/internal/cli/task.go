package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/santillana/ai-squad/internal/core"
	"github.com/santillana/ai-squad/internal/tasks"
)

func newTaskCommand(configPath func() string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "task",
		Short: "Create and inspect tasks",
	}
	cmd.AddCommand(
		newTaskCreateCommand(configPath),
		newTaskListCommand(configPath),
		newTaskShowCommand(configPath),
		newTaskCancelCommand(configPath),
		newTaskRetryCommand(configPath),
	)
	return cmd
}

func newTaskCreateCommand(configPath func() string) *cobra.Command {
	var (
		title, description string
		repo, branch       string
		workflow, agent    string
		priority           string
		maxAttempts        int
		parent             string
		idempotencyKey     string
		metadata           []string
		asJSON             bool
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a task",
		Long: "Create a task in PENDING state.\n\n" +
			"A task either follows a workflow (--workflow) or targets one agent\n" +
			"(--agent). Pass --idempotency-key to make creation safe to retry: a\n" +
			"repeated call with the same key returns the original task.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			prio, err := core.ParsePriority(priority)
			if err != nil {
				return err
			}
			meta, err := parseKeyValues(metadata)
			if err != nil {
				return err
			}

			app, err := open(cmd.Context(), configPath())
			if err != nil {
				return err
			}
			defer app.Close()

			task, err := app.Tasks.Create(cmd.Context(), tasks.CreateParams{
				Title:          title,
				Description:    description,
				Repository:     repo,
				Branch:         branch,
				Workflow:       workflow,
				Agent:          agent,
				Priority:       prio,
				MaxAttempts:    maxAttempts,
				ParentTask:     parent,
				Metadata:       meta,
				IdempotencyKey: idempotencyKey,
			})
			if err != nil {
				return err
			}

			if asJSON {
				return writeJSON(cmd.OutOrStdout(), task)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s  %s  [%s]\n", task.ID, task.Title, task.Status)
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&title, "title", "", "short task title (required)")
	f.StringVar(&description, "description", "", "full task description")
	f.StringVar(&repo, "repo", "", "path to the git repository to work in")
	f.StringVar(&branch, "branch", "", "target branch for the work")
	f.StringVar(&workflow, "workflow", "", "workflow to run, e.g. feature or bugfix")
	f.StringVar(&agent, "agent", "", "single agent to run instead of a workflow")
	f.StringVar(&priority, "priority", "normal", "low|normal|high|critical, or an integer")
	f.IntVar(&maxAttempts, "max-attempts", 0, "attempts allowed per step (default from config)")
	f.StringVar(&parent, "parent", "", "parent task id")
	f.StringVar(&idempotencyKey, "idempotency-key", "", "make creation replay-safe")
	f.StringArrayVar(&metadata, "metadata", nil, "metadata as key=value (repeatable)")
	f.BoolVar(&asJSON, "json", false, "emit the created task as JSON")
	_ = cmd.MarkFlagRequired("title")
	return cmd
}

func newTaskListCommand(configPath func() string) *cobra.Command {
	var (
		statusFilter []string
		repo         string
		workflow     string
		limit        int
		asJSON       bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List tasks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var statuses []core.TaskStatus
			for _, s := range statusFilter {
				parsed, err := core.ParseStatus(s)
				if err != nil {
					return err
				}
				statuses = append(statuses, parsed)
			}

			app, err := open(cmd.Context(), configPath())
			if err != nil {
				return err
			}
			defer app.Close()

			list, err := app.Tasks.List(cmd.Context(), core.TaskFilter{
				Statuses:   statuses,
				Repository: repo,
				Workflow:   workflow,
				Limit:      limit,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, list)
			}
			if len(list) == 0 {
				fmt.Fprintln(out, "no tasks")
				return nil
			}

			tw := newTable(out)
			fmt.Fprintln(tw, "ID\tSTATUS\tPRIO\tAGENT\tWORKFLOW\tATTEMPTS\tUPDATED\tTITLE")
			for _, t := range list {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%d/%d\t%s\t%s\n",
					t.ID, t.Status, t.Priority, dash(t.Agent), dash(t.Workflow),
					t.Attempts, t.MaxAttempts, relativeTime(t.UpdatedAt),
					truncate(t.Title, 48))
			}
			return tw.Flush()
		},
	}

	f := cmd.Flags()
	f.StringArrayVar(&statusFilter, "status", nil, "filter by status (repeatable)")
	f.StringVar(&repo, "repo", "", "filter by repository path")
	f.StringVar(&workflow, "workflow", "", "filter by workflow")
	f.IntVar(&limit, "limit", 50, "maximum rows to return (0 for no limit)")
	f.BoolVar(&asJSON, "json", false, "emit tasks as JSON")
	return cmd
}

func newTaskShowCommand(configPath func() string) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "show <task-id>",
		Short: "Show a task and its history",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := open(cmd.Context(), configPath())
			if err != nil {
				return err
			}
			defer app.Close()

			task, err := app.Tasks.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			events, err := app.Tasks.Events(cmd.Context(), task.ID)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, struct {
					Task   *core.Task       `json:"task"`
					Events []core.TaskEvent `json:"events"`
				}{task, events})
			}

			tw := newTable(out)
			fmt.Fprintf(tw, "ID\t%s\n", task.ID)
			fmt.Fprintf(tw, "Title\t%s\n", task.Title)
			if task.Description != "" {
				fmt.Fprintf(tw, "Description\t%s\n", truncate(task.Description, 100))
			}
			fmt.Fprintf(tw, "Status\t%s\n", task.Status)
			fmt.Fprintf(tw, "Priority\t%d\n", task.Priority)
			fmt.Fprintf(tw, "Agent\t%s\n", dash(task.Agent))
			fmt.Fprintf(tw, "Workflow\t%s (step %d)\n", dash(task.Workflow), task.Step)
			fmt.Fprintf(tw, "Repository\t%s\n", dash(task.Repository))
			fmt.Fprintf(tw, "Branch\t%s\n", dash(task.Branch))
			fmt.Fprintf(tw, "Workspace\t%s\n", dash(task.WorkspacePath))
			fmt.Fprintf(tw, "Attempts\t%d/%d\n", task.Attempts, task.MaxAttempts)
			if task.ParentTaskID != nil {
				fmt.Fprintf(tw, "Parent\t%s\n", *task.ParentTaskID)
			}
			if task.LastError != "" {
				fmt.Fprintf(tw, "Last error\t%s\n", truncate(task.LastError, 120))
			}
			fmt.Fprintf(tw, "Created\t%s\n", task.CreatedAt.Format("2006-01-02 15:04:05 MST"))
			fmt.Fprintf(tw, "Updated\t%s\n", task.UpdatedAt.Format("2006-01-02 15:04:05 MST"))
			fmt.Fprintf(tw, "Next states\t%s\n", joinStatuses(core.NextStates(task.Status)))
			for k, v := range task.Metadata {
				fmt.Fprintf(tw, "meta.%s\t%s\n", k, v)
			}
			if err := tw.Flush(); err != nil {
				return err
			}

			fmt.Fprintln(out, "\nHistory")
			etw := newTable(out)
			for _, ev := range events {
				fmt.Fprintf(etw, "  %s\t%s -> %s\t%s\n",
					ev.CreatedAt.Format("2006-01-02 15:04:05"),
					dash(string(ev.FromStatus)), ev.ToStatus, ev.Reason)
			}
			return etw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the task and its history as JSON")
	return cmd
}

func newTaskCancelCommand(configPath func() string) *cobra.Command {
	var reason string

	cmd := &cobra.Command{
		Use:   "cancel <task-id>",
		Short: "Cancel a task",
		Long: "Cancel a task. Cancelling an already-cancelled task succeeds and changes\n" +
			"nothing, so the command is safe to repeat.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := open(cmd.Context(), configPath())
			if err != nil {
				return err
			}
			defer app.Close()

			task, err := app.Tasks.Cancel(cmd.Context(), args[0], reason)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is now %s\n", task.ID, task.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why the task is being cancelled")
	return cmd
}

func newTaskRetryCommand(configPath func() string) *cobra.Command {
	var reason string

	cmd := &cobra.Command{
		Use:   "retry <task-id>",
		Short: "Re-queue a failed or blocked task",
		Long: "Re-queue a FAILED or BLOCKED task, resetting its attempt counter for the\n" +
			"current step. A task that is already queued is returned unchanged, so\n" +
			"repeating the command never duplicates work.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := open(cmd.Context(), configPath())
			if err != nil {
				return err
			}
			defer app.Close()

			task, err := app.Tasks.Retry(cmd.Context(), args[0], reason)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is now %s\n", task.ID, task.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why the task is being retried")
	return cmd
}

// parseKeyValues turns repeated "k=v" flags into a map.
func parseKeyValues(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		key, value, ok := strings.Cut(p, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, core.Invalid("metadata", "expected key=value, got "+p)
		}
		out[key] = value
	}
	return out, nil
}

func joinStatuses(list []core.TaskStatus) string {
	if len(list) == 0 {
		return "- (terminal)"
	}
	parts := make([]string, len(list))
	for i, s := range list {
		parts[i] = string(s)
	}
	return strings.Join(parts, ", ")
}
