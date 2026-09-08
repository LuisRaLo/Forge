package cli

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	agenttemplates "github.com/santillana/ai-squad/agents"
	configtemplate "github.com/santillana/ai-squad/config"
	"github.com/santillana/ai-squad/internal/config"
	"github.com/santillana/ai-squad/internal/storage"
)

func newInitCommand(configPath func() string) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create the data directory, configuration and database",
		Long: "init prepares ~/.ai-squad (or the configured data directory) with a starter\n" +
			"configuration, the default agent definitions and a migrated database.\n\n" +
			"It is idempotent: existing files are left untouched unless --force is given.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd.Context(), cmd.OutOrStdout(), configPath(), force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"overwrite existing configuration and agent definitions")
	return cmd
}

func runInit(ctx context.Context, out io.Writer, configPath string, force bool) error {
	cfgPath, err := config.ExpandPath(configPath)
	if err != nil {
		return err
	}
	dataDir := filepath.Dir(cfgPath)

	// 0700: the data directory holds task history and logs, which are not
	// other users' business.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory %s: %w", dataDir, err)
	}
	fmt.Fprintf(out, "data directory  %s\n", dataDir)

	written, err := writeFileIfAbsent(cfgPath, configtemplate.Default, 0o600, force)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "configuration   %s %s\n", cfgPath, status(written))

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(cfg.System.AgentsDir, 0o700); err != nil {
		return fmt.Errorf("create agents directory: %w", err)
	}
	count, err := writeAgentTemplates(cfg.System.AgentsDir, force)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "agents          %s (%d written)\n", cfg.System.AgentsDir, count)

	db, err := storage.Open(ctx, cfg.DatabasePath())
	if err != nil {
		return err
	}
	defer db.Close()

	if err := storage.Migrate(ctx, db); err != nil {
		return err
	}
	fmt.Fprintf(out, "database        %s (migrated)\n", cfg.DatabasePath())

	fmt.Fprintf(out, "\nReady. Create your first task with:\n"+
		"  ai-squad task create --title \"...\" --repo <path> --workflow feature\n")
	return nil
}

func writeAgentTemplates(dir string, force bool) (int, error) {
	entries, err := fs.ReadDir(agenttemplates.FS, ".")
	if err != nil {
		return 0, fmt.Errorf("read embedded agent templates: %w", err)
	}

	written := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, err := fs.ReadFile(agenttemplates.FS, e.Name())
		if err != nil {
			return written, fmt.Errorf("read embedded agent %s: %w", e.Name(), err)
		}
		ok, err := writeFileIfAbsent(filepath.Join(dir, e.Name()), body, 0o600, force)
		if err != nil {
			return written, err
		}
		if ok {
			written++
		}
	}
	return written, nil
}

// writeFileIfAbsent writes body to path unless it already exists, reporting
// whether it wrote. This is what makes `init` safe to re-run.
func writeFileIfAbsent(path string, body []byte, perm os.FileMode, force bool) (bool, error) {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return false, nil
		} else if !os.IsNotExist(err) {
			return false, fmt.Errorf("stat %s: %w", path, err)
		}
	}
	if err := os.WriteFile(path, body, perm); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}

func status(written bool) string {
	if written {
		return "(created)"
	}
	return "(already present, left untouched)"
}
