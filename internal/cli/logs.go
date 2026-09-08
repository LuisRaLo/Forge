package cli

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"
)

// logLine is one unified, time-ordered entry from either the task's state
// event trail or its agent execution history.
type logLine struct {
	At   string
	Kind string
	Text string
}

func newLogsCommand(configPath func() string) *cobra.Command {
	var asJSON, transcript bool

	cmd := &cobra.Command{
		Use:   "logs <task-id>",
		Short: "Show a task's state history and agent execution log, in order",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := open(cmd.Context(), configPath())
			if err != nil {
				return err
			}
			defer app.Close()

			taskID := args[0]
			if _, err := app.Repo.Get(cmd.Context(), taskID); err != nil {
				return err
			}

			events, err := app.Tasks.Events(cmd.Context(), taskID)
			if err != nil {
				return err
			}
			runs, err := app.Runs.ListByTask(cmd.Context(), taskID)
			if err != nil {
				return err
			}

			var lines []logLine
			for _, ev := range events {
				text := fmt.Sprintf("%s -> %s", dash(string(ev.FromStatus)), ev.ToStatus)
				if ev.Reason != "" {
					text += ": " + ev.Reason
				}
				lines = append(lines, logLine{
					At: ev.CreatedAt.Format("2006-01-02T15:04:05Z07:00"), Kind: "state", Text: text,
				})
			}
			for _, r := range runs {
				text := fmt.Sprintf("agent=%s runtime=%s status=%s duration=%s",
					r.Agent, r.Runtime, r.Status, r.Duration)
				if r.Usage.CostUSD != nil {
					text += fmt.Sprintf(" cost=$%.4f", *r.Usage.CostUSD)
				}
				if r.Error != "" {
					text += " error=" + r.Error
				}
				lines = append(lines, logLine{
					At: r.FinishedAt.Format("2006-01-02T15:04:05Z07:00"), Kind: "run", Text: text,
				})
				if transcript {
					for _, ev := range r.Transcript {
						evText := string(ev.Type)
						if ev.Text != "" {
							evText += ": " + ev.Text
						}
						lines = append(lines, logLine{
							At: ev.Timestamp.Format("2006-01-02T15:04:05Z07:00"), Kind: "agent", Text: evText,
						})
					}
				}
			}
			sort.SliceStable(lines, func(i, j int) bool { return lines[i].At < lines[j].At })

			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, lines)
			}
			if len(lines) == 0 {
				fmt.Fprintln(out, "no log entries")
				return nil
			}
			tw := newTable(out)
			for _, l := range lines {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", l.At, l.Kind, l.Text)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit logs as JSON")
	cmd.Flags().BoolVarP(&transcript, "transcript", "t", false,
		"also show each run's step-by-step agent activity (assistant text, tool calls)")
	return cmd
}
