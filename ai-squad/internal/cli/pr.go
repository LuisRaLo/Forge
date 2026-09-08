package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/LuisRaLo/ai-squad/internal/git"
	"github.com/LuisRaLo/ai-squad/internal/github"
)

func newPRCommand(configPath func() string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pr",
		Short: "Push a task's branch and manage its pull request",
		Long: "GitHub operations for a task's workspace. ai-squad never merges a pull\n" +
			"request and never deploys to production from these commands — that always\n" +
			"stays a human decision, made outside the tool (`ai-squad approve` records\n" +
			"the decision; it does not act on GitHub or a deployment target itself).",
	}
	cmd.AddCommand(
		newPRCreateCommand(configPath),
		newPRStatusCommand(configPath),
		newPRCommentsCommand(configPath),
	)
	return cmd
}

func newPRCreateCommand(configPath func() string) *cobra.Command {
	var base, body string
	var draft bool

	cmd := &cobra.Command{
		Use:   "create <task-id>",
		Short: "Push a task's branch and open a pull request for it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := open(cmd.Context(), configPath())
			if err != nil {
				return err
			}
			defer app.Close()

			task, err := app.Repo.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if task.WorkspacePath == "" {
				return fmt.Errorf("task %s has no workspace yet; it has not been executed", task.ID)
			}

			g := git.New()
			branch := task.Branch
			if branch == "" {
				if branch, err = g.CurrentBranch(cmd.Context(), task.WorkspacePath); err != nil {
					return fmt.Errorf("determine branch: %w", err)
				}
			}
			if err := g.Push(cmd.Context(), task.WorkspacePath, "origin", branch); err != nil {
				return fmt.Errorf("push: %w", err)
			}

			prBody := body
			if prBody == "" {
				prBody = task.Description
			}
			pr, err := github.New().CreatePR(cmd.Context(), github.CreatePROptions{
				Dir: task.WorkspacePath, Title: task.Title, Body: prBody, Base: base, Draft: draft,
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), pr.URL)
			return nil
		},
	}
	cmd.Flags().StringVar(&base, "base", "", "target branch (defaults to the repository default)")
	cmd.Flags().StringVar(&body, "body", "", "pull request body (defaults to the task description)")
	cmd.Flags().BoolVar(&draft, "draft", false, "open as a draft pull request")
	return cmd
}

func newPRStatusCommand(configPath func() string) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "status <task-id>",
		Short: "Show CI check status for a task's pull request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := open(cmd.Context(), configPath())
			if err != nil {
				return err
			}
			defer app.Close()

			task, err := app.Repo.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if task.WorkspacePath == "" {
				return fmt.Errorf("task %s has no workspace yet", task.ID)
			}

			status, err := github.New().PRChecks(cmd.Context(), task.WorkspacePath, "")
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, status)
			}
			fmt.Fprintf(out, "Overall: %s\n\n", status.Overall())
			tw := newTable(out)
			fmt.Fprintln(tw, "NAME\tBUCKET\tSTATE\tDESCRIPTION")
			for _, c := range status.Checks {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.Name, c.Bucket, c.State, dash(c.Description))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit CI status as JSON")
	return cmd
}

func newPRCommentsCommand(configPath func() string) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "comments <task-id>",
		Short: "Show comments on a task's pull request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := open(cmd.Context(), configPath())
			if err != nil {
				return err
			}
			defer app.Close()

			task, err := app.Repo.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if task.WorkspacePath == "" {
				return fmt.Errorf("task %s has no workspace yet", task.ID)
			}

			comments, err := github.New().PRComments(cmd.Context(), task.WorkspacePath, "")
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, comments)
			}
			if len(comments) == 0 {
				fmt.Fprintln(out, "no comments")
				return nil
			}
			for _, c := range comments {
				fmt.Fprintf(out, "%s:\n%s\n\n", c.Author, c.Body)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit comments as JSON")
	return cmd
}
