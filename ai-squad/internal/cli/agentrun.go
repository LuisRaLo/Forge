package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/santillana/ai-squad/internal/core"
)

func newAgentRunCommand(configPath func() string) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "run <agent> <task-id>",
		Short: "Manually run one agent against a task, outside the scheduler",
		Long: "Executes the named agent once against the task's workspace and prints the\n" +
			"result. This is a debugging tool: unlike the scheduler, it does not claim\n" +
			"the task, does not change its status, and does not advance the workflow.\n" +
			"The run and its artifact are still recorded, so it shows up in\n" +
			"`ai-squad logs`.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentName, taskID := args[0], args[1]

			app, err := open(cmd.Context(), configPath())
			if err != nil {
				return err
			}
			defer app.Close()

			def, err := app.Agents.Get(agentName)
			if err != nil {
				return err
			}
			task, err := app.Repo.Get(cmd.Context(), taskID)
			if err != nil {
				return err
			}

			runtimeName := app.EffectiveRuntime(def)
			rt, err := app.Runtimes.Runtime(runtimeName)
			if err != nil {
				return err
			}

			ws, err := app.Workspaces.Acquire(cmd.Context(), task)
			if err != nil {
				return fmt.Errorf("acquire workspace: %w", err)
			}

			stepID := core.StepID(task.Workflow, task.Step, agentName, 0) + "-manual-" + core.SystemClock().Format("20060102T150405")

			req := core.RunRequest{
				TaskID: task.ID, StepID: stepID, Agent: def,
				SystemPrompt: def.SystemPrompt,
				Prompt:       fmt.Sprintf("# Task\n%s\n\n%s\n", task.Title, task.Description),
				WorkspaceDir: ws.Path,
				Permissions:  def.Permissions,
				Limits:       def.Limits,
			}

			out := cmd.OutOrStdout()
			result, err := rt.Execute(cmd.Context(), req, func(_ context.Context, ev core.Event) {
				if ev.Text != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "[%s] %s\n", ev.Type, ev.Text)
				}
			})
			if err != nil {
				return err
			}

			run := &core.AgentRun{
				TaskID: task.ID, StepID: stepID, Agent: def.Name, Runtime: runtimeName,
				Status: core.RunSucceeded, SessionID: result.SessionID, StopReason: result.StopReason,
				Usage: result.Usage, Duration: result.Duration,
			}
			if _, err := app.Runs.Record(cmd.Context(), run); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to record run: %v\n", err)
			}

			if asJSON {
				return writeJSON(out, result)
			}
			fmt.Fprintln(out, result.Text)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the full result as JSON")
	return cmd
}
