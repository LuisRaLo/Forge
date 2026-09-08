package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/santillana/ai-squad/internal/config"
)

func newConfigCommand(configPath func() string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and validate the configuration",
	}
	cmd.AddCommand(
		newConfigValidateCommand(configPath),
		newConfigShowCommand(configPath),
	)
	return cmd
}

func newConfigValidateCommand(configPath func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Check the configuration and agent definitions without running anything",
		Long: "Loads the configuration, parses every agent definition and verifies that\n" +
			"each agent is bound to a runtime that exists and that every workflow step\n" +
			"names a real agent. Exits non-zero on the first problem found.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := open(cmd.Context(), configPath())
			if err != nil {
				return err
			}
			defer app.Close()

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "configuration %s is valid\n", app.Cfg.Path())
			fmt.Fprintf(out, "  agents     %d\n", len(app.Agents.List()))
			fmt.Fprintf(out, "  runtimes   %d\n", len(app.Cfg.Runtimes))
			fmt.Fprintf(out, "  providers  %d\n", len(app.Cfg.Providers))
			fmt.Fprintf(out, "  workflows  %d\n", len(app.Cfg.Workflows))
			return nil
		},
	}
}

func newConfigShowCommand(configPath func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the effective configuration",
		Long: "Prints the configuration as loaded, with defaults filled in. Credentials\n" +
			"are never included: only the names of the environment variables holding\n" +
			"them are shown.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath())
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), cfg)
		},
	}
}
