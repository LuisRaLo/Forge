package cli

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

func newWorkerCommand(configPath func() string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run the scheduler",
	}
	cmd.AddCommand(newWorkerStartCommand(configPath), newWorkerStatusCommand(configPath))
	return cmd
}

func newWorkerStartCommand(configPath func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Drain the currently claimable task queue once, then exit",
		Long: "Recovers any task left behind by a crashed process, then claims and runs\n" +
			"tasks until none remain claimable. Unlike `ai-squad daemon`, this returns —\n" +
			"suitable for a cron-triggered pass rather than a long-running service.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := open(cmd.Context(), configPath())
			if err != nil {
				return err
			}
			defer app.Close()

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			fmt.Fprintln(cmd.OutOrStdout(), "draining claimable tasks…")
			if err := app.Scheduler.RunOnce(ctx); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "done")
			return nil
		},
	}
}

func newWorkerStatusCommand(configPath func() string) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show worker pool configuration and currently active/claimable tasks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := open(cmd.Context(), configPath())
			if err != nil {
				return err
			}
			defer app.Close()

			active, err := app.Tasks.List(cmd.Context(), core.TaskFilter{
				Statuses: []core.TaskStatus{core.StatusPlanning, core.StatusRunning},
			})
			if err != nil {
				return err
			}
			claimable, err := app.Tasks.List(cmd.Context(), core.TaskFilter{
				Statuses: []core.TaskStatus{core.StatusPending, core.StatusReady, core.StatusReview},
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, map[string]any{
					"max_concurrency": app.Cfg.Scheduler.MaxConcurrency,
					"poll_interval":   app.Cfg.Scheduler.PollInterval.Duration().String(),
					"active":          len(active),
					"claimable":       len(claimable),
				})
			}

			tw := newTable(out)
			fmt.Fprintf(tw, "Max concurrency\t%d\n", app.Cfg.Scheduler.MaxConcurrency)
			fmt.Fprintf(tw, "Poll interval\t%s\n", app.Cfg.Scheduler.PollInterval.Duration())
			fmt.Fprintf(tw, "Active (running now)\t%d\n", len(active))
			fmt.Fprintf(tw, "Claimable (queued)\t%d\n", len(claimable))
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit worker status as JSON")
	return cmd
}

func newDaemonCommand(configPath func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "daemon",
		Short: "Run the scheduler continuously until interrupted",
		Long: "Recovers any task left behind by a crashed process, then polls and\n" +
			"dispatches tasks continuously. Stops on SIGINT/SIGTERM, waiting for\n" +
			"in-flight steps to finish before exiting.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := open(cmd.Context(), configPath())
			if err != nil {
				return err
			}
			defer app.Close()

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			fmt.Fprintf(cmd.OutOrStdout(), "ai-squad daemon started (max_concurrency=%d, poll_interval=%s). Ctrl+C to stop.\n",
				app.Cfg.Scheduler.MaxConcurrency, app.Cfg.Scheduler.PollInterval.Duration())

			if err := app.Scheduler.Run(ctx); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "stopped")
			return nil
		},
	}
}
