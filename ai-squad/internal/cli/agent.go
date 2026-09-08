package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

func newAgentCommand(configPath func() string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Inspect agent definitions",
	}
	cmd.AddCommand(
		newAgentListCommand(configPath),
		newAgentShowCommand(configPath),
		newAgentRunCommand(configPath),
	)
	return cmd
}

func newAgentListCommand(configPath func() string) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List configured agents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := open(cmd.Context(), configPath())
			if err != nil {
				return err
			}
			defer app.Close()

			list := app.Agents.List()
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, list)
			}

			tw := newTable(out)
			fmt.Fprintln(tw, "NAME\tRUNTIME\tFILESYSTEM\tSHELL\tNETWORK\tGIT\tTIMEOUT\tDESCRIPTION")
			for _, a := range list {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					a.Name,
					app.EffectiveRuntime(a),
					a.Permissions.Filesystem,
					describeShell(a.Permissions.Shell),
					yesNo(a.Permissions.Network),
					yesNo(a.Permissions.GitWrite),
					describeTimeout(a),
					truncate(a.Description, 40))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit agents as JSON")
	return cmd
}

func newAgentShowCommand(configPath func() string) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "show <agent>",
		Short: "Show one agent, including the capabilities its runtime must provide",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := open(cmd.Context(), configPath())
			if err != nil {
				return err
			}
			defer app.Close()

			def, err := app.Agents.Get(args[0])
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, def)
			}

			tw := newTable(out)
			fmt.Fprintf(tw, "Name\t%s\n", def.Name)
			fmt.Fprintf(tw, "Description\t%s\n", dash(def.Description))
			fmt.Fprintf(tw, "Runtime\t%s\n", app.EffectiveRuntime(def))
			fmt.Fprintf(tw, "Model\t%s\n", dash(def.Model))
			fmt.Fprintf(tw, "Filesystem\t%s\n", def.Permissions.Filesystem)
			fmt.Fprintf(tw, "Shell\t%s\n", describeShell(def.Permissions.Shell))
			fmt.Fprintf(tw, "Network\t%s\n", yesNo(def.Permissions.Network))
			fmt.Fprintf(tw, "Git write\t%s\n", yesNo(def.Permissions.GitWrite))
			fmt.Fprintf(tw, "Timeout\t%s\n", describeTimeout(def))
			fmt.Fprintf(tw, "Max attempts\t%d\n", def.Limits.MaxAttempts)
			fmt.Fprintf(tw, "Requires\t%s\n", joinCapabilities(def.RequiredCapabilities()))
			if err := tw.Flush(); err != nil {
				return err
			}

			fmt.Fprintf(out, "\nSystem prompt\n%s\n", indent(def.SystemPrompt, "  "))
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the agent as JSON")
	return cmd
}

func describeShell(p core.ShellPolicy) string {
	if !p.Enabled {
		return "denied"
	}
	if len(p.Commands) == 0 {
		return "any"
	}
	return strings.Join(p.Commands, ",")
}

func describeTimeout(a *core.AgentDefinition) string {
	if a.Limits.Timeout <= 0 {
		return "-"
	}
	return a.Limits.Timeout.String()
}

func joinCapabilities(set core.CapabilitySet) string {
	list := set.List()
	if len(list) == 0 {
		return "-"
	}
	parts := make([]string, len(list))
	for i, c := range list {
		parts[i] = string(c)
	}
	return strings.Join(parts, ", ")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
