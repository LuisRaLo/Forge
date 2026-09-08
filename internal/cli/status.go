package cli

import (
	"fmt"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

func newStatusCommand(configPath func() string) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show a summary of the orchestrator's state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := open(cmd.Context(), configPath())
			if err != nil {
				return err
			}
			defer app.Close()

			all, err := app.Tasks.List(cmd.Context(), core.TaskFilter{})
			if err != nil {
				return err
			}

			counts := map[core.TaskStatus]int{}
			for _, t := range all {
				counts[t.Status]++
			}

			costTotal, costUnknown, err := app.Runs.CostSince(cmd.Context(), time.Now().Add(-24*time.Hour))
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, map[string]any{
					"config":            app.Cfg.Path(),
					"database":          app.Cfg.DatabasePath(),
					"agents":            len(app.Agents.List()),
					"runtimes":          sortedRuntimeNames(app),
					"max_concurrency":   app.Cfg.Scheduler.MaxConcurrency,
					"tasks_total":       len(all),
					"tasks_by_status":   counts,
					"cost_last_24h_usd": costTotal,
					// Reported cost is whatever runtimes chose to report; a
					// provider that never reports usage contributes nothing to
					// the sum above, so this count is what keeps "we spent
					// $X" from being mistaken for "we spent exactly $X and
					// nothing more" — see core.Usage.CostEstimated.
					"cost_last_24h_unknown_runs": costUnknown,
					"max_daily_cost_usd":         app.Cfg.Limits.MaxDailyCostUSD,
				})
			}

			tw := newTable(out)
			fmt.Fprintf(tw, "Config\t%s\n", app.Cfg.Path())
			fmt.Fprintf(tw, "Database\t%s\n", app.Cfg.DatabasePath())
			fmt.Fprintf(tw, "Agents\t%d\n", len(app.Agents.List()))
			fmt.Fprintf(tw, "Runtimes\t%v\n", sortedRuntimeNames(app))
			fmt.Fprintf(tw, "Max concurrency\t%d\n", app.Cfg.Scheduler.MaxConcurrency)
			fmt.Fprintf(tw, "Tasks\t%d\n", len(all))
			fmt.Fprintf(tw, "Cost (last 24h)\t$%.4f of $%.2f limit\n", costTotal, app.Cfg.Limits.MaxDailyCostUSD)
			if costUnknown > 0 {
				fmt.Fprintf(tw, "Cost (unknown)\t%d run(s) reported no cost; the total above is a floor, not exact\n", costUnknown)
			}
			if err := tw.Flush(); err != nil {
				return err
			}

			if len(all) == 0 {
				return nil
			}
			fmt.Fprintln(out, "\nBy status")
			stw := newTable(out)
			for _, s := range core.AllStatuses() {
				if n := counts[s]; n > 0 {
					fmt.Fprintf(stw, "  %s\t%d\n", s, n)
				}
			}
			return stw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the summary as JSON")
	return cmd
}

func sortedRuntimeNames(app *App) []string {
	out := make([]string, 0, len(app.Cfg.Runtimes))
	for name := range app.Cfg.Runtimes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
