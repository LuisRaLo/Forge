package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Version is the build version, overridable at link time with:
//
//	go build -ldflags "-X github.com/LuisRaLo/ai-squad/internal/cli.Version=v0.1.0"
var Version = "dev"

// Execute builds the command tree and runs it, returning the process exit code.
func Execute() int {
	root := NewRootCommand()
	if err := root.Execute(); err != nil {
		// Cobra has already printed usage where relevant; print the error
		// once, to stderr, without a stack trace.
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

// NewRootCommand builds the ai-squad command tree.
func NewRootCommand() *cobra.Command {
	var configPath string

	root := &cobra.Command{
		Use:   "ai-squad",
		Short: "Local orchestrator for a squad of autonomous software agents",
		Long: "ai-squad coordinates multiple software development agents on your machine.\n\n" +
			"Agents are defined independently of the runtime that executes them, so the\n" +
			"same squad can run on Claude Code, a local model, or a hosted API.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
	}

	root.PersistentFlags().StringVar(&configPath, "config", defaultConfigPath(),
		"path to the configuration file")

	// resolve reads the flag at run time so subcommands see the final value.
	resolve := func() string { return configPath }

	root.AddCommand(
		newInitCommand(resolve),
		newTaskCommand(resolve),
		newAgentCommand(resolve),
		newStatusCommand(resolve),
		newConfigCommand(resolve),
		newWorkerCommand(resolve),
		newDaemonCommand(resolve),
		newApproveCommand(resolve),
		newLogsCommand(resolve),
	)
	return root
}
