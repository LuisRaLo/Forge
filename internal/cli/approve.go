package cli

import (
	"fmt"
	"os"
	"os/user"

	"github.com/spf13/cobra"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

func newApproveCommand(configPath func() string) *cobra.Command {
	var by, note string

	cmd := &cobra.Command{
		Use:   "approve <task-id>",
		Short: "Approve a task waiting for human sign-off",
		Long: "Moves a task out of WAITING_APPROVAL.\n\n" +
			"V1 never deploys to production automatically: approving a task marks it\n" +
			"COMPLETED and records who approved it and when, but does not itself invoke\n" +
			"any deployment command. Wiring an approved task to an actual production\n" +
			"rollout is a deliberate follow-up action outside this command, not\n" +
			"something `approve` triggers on your behalf.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := open(cmd.Context(), configPath())
			if err != nil {
				return err
			}
			defer app.Close()

			approver := by
			if approver == "" {
				approver = currentUser()
			}
			reason := fmt.Sprintf("approved by %s", approver)
			if note != "" {
				reason += ": " + note
			}

			task, err := app.Repo.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if task.Status != core.StatusWaitingApproval {
				return fmt.Errorf("task %s is %s, not WAITING_APPROVAL: %w",
					task.ID, task.Status, core.ErrInvalidTransition)
			}

			updated, err := app.Repo.Transition(cmd.Context(), task.ID, core.StatusCompleted, reason,
				func(t *core.Task) {
					if t.Metadata == nil {
						t.Metadata = map[string]string{}
					}
					t.Metadata["approved_by"] = approver
				})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is now %s (%s)\n", updated.ID, updated.Status, reason)
			return nil
		},
	}

	cmd.Flags().StringVar(&by, "by", "", "who is approving (defaults to the current OS user)")
	cmd.Flags().StringVar(&note, "note", "", "optional note to record with the approval")
	return cmd
}

func currentUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	return "unknown"
}
