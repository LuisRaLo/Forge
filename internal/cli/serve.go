package cli

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/LuisRaLo/ai-squad/internal/web"
)

func newServeCommand(configPath func() string) *cobra.Command {
	var addr string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the scheduler and the web dashboard together, headless",
		Long: "Starts the scheduler (same as `ai-squad daemon`) and the HTTP+WebSocket\n" +
			"dashboard in one process, listening on --addr. Does not open a window —\n" +
			"use `ai-squad desktop` for that. Stops on SIGINT/SIGTERM.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := open(cmd.Context(), configPath())
			if err != nil {
				return err
			}
			defer app.Close()

			srv, err := newDashboardServer(app)
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			go func() {
				if err := app.Scheduler.Run(ctx); err != nil {
					app.Log.Error("scheduler stopped with an error", "error", err)
				}
			}()

			fmt.Fprintf(cmd.OutOrStdout(), "ai-squad dashboard: http://%s  (Ctrl+C to stop)\n", addr)
			return web.ListenAndServe(ctx, addr, srv.Handler(), app.Log)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:8080", "address to listen on")
	return cmd
}

// newDashboardServer wires internal/web.Server from an already-opened App,
// shared by both `serve` and `desktop`.
func newDashboardServer(app *App) (*web.Server, error) {
	return web.New(web.Deps{
		Tasks: app.Tasks, Repo: app.Repo, Runs: app.Runs, Artifacts: app.Artifacts,
		Agents: app.Agents, Runtimes: app.Runtimes, Bus: app.Events,
		EffectiveRuntime:     app.EffectiveRuntime,
		CheckRuntimeOverride: app.CheckRuntimeOverride,
	})
}
