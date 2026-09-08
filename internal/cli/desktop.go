package cli

import (
	"context"
	"fmt"
	"net"

	"github.com/spf13/cobra"
	webview "github.com/webview/webview_go"

	"github.com/LuisRaLo/ai-squad/internal/web"
)

func newDesktopCommand(configPath func() string) *cobra.Command {
	var width, height int

	cmd := &cobra.Command{
		Use:   "desktop",
		Short: "Open the dashboard in a native window",
		Long: "Starts the scheduler and the dashboard server on a local port, then opens\n" +
			"a native window (WKWebView on macOS) pointed at it — no browser tab needed.\n" +
			"Closing the window stops the scheduler and server together.",
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

			// Bind to a random free port on loopback only — the window is
			// the only intended client, and there is no reason for this
			// port to be reachable from anywhere else on the network.
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return fmt.Errorf("bind local port: %w", err)
			}
			addr := ln.Addr().String()

			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()

			go func() {
				if err := app.Scheduler.Run(ctx); err != nil {
					app.Log.Error("scheduler stopped with an error", "error", err)
				}
			}()
			go func() {
				if err := web.Serve(ctx, ln, srv.Handler(), app.Log); err != nil {
					app.Log.Error("dashboard server stopped with an error", "error", err)
				}
			}()

			// webview's event loop must run on this goroutine (the OS
			// requires the native window to live on the main thread), so
			// everything above is backgrounded and this call blocks until
			// the window is closed.
			w := webview.New(false)
			defer w.Destroy()
			w.SetTitle("ai-squad")
			w.SetSize(width, height, webview.HintNone)
			w.Navigate("http://" + addr)
			w.Run()

			cancel() // window closed: stop the scheduler and server together
			return nil
		},
	}
	cmd.Flags().IntVar(&width, "width", 1200, "window width")
	cmd.Flags().IntVar(&height, "height", 800, "window height")
	return cmd
}
