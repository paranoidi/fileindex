package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/paranoidi/fileindex/db"
	"github.com/spf13/cobra"
)

func initCmd(a *app) *cobra.Command {
	var autocomplete, nonInteractive bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize database and configuration, optionally register shell autocomplete",
		Long: `Initialize database and configuration.

When run in a terminal, also offers to register shell autocomplete (bash, zsh or fish).
--autocomplete registers it without asking and skips database initialization.
--non-interactive never prompts, for scripts.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !autocomplete {
				d, err := a.openDB()
				if err != nil {
					return err
				}
				if err := d.Init(); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Database initialized at: %s\n", d.Path)
				fmt.Fprintf(a.stdout, "Configuration file at: %s\n", a.cfg.Path)
			}
			if autocomplete || (!nonInteractive && stdinIsTerminal() && a.confirmAutocomplete()) {
				return installCompletion(cmd.Root(), a.stdout)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&autocomplete, "autocomplete", false, "Only register shell autocomplete (no prompt, no database changes)")
	cmd.Flags().BoolVar(&nonInteractive, "non-interactive", false, "Never prompt")
	return cmd
}

func stdinIsTerminal() bool {
	return isatty.IsTerminal(os.Stdin.Fd())
}

// confirmAutocomplete asks whether to register autocomplete for the user's shell.
func (a *app) confirmAutocomplete() bool {
	shell := filepath.Base(os.Getenv("SHELL"))
	fmt.Fprintf(a.stdout, "Register autocomplete for %s? [Y/n] ", shell)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "" || line == "y" || line == "yes"
}

// installCompletion writes the completion script for $SHELL to the user's
// completion directory (no root needed).
func installCompletion(root *cobra.Command, out io.Writer) error {
	shell := filepath.Base(os.Getenv("SHELL"))
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		dataDir = filepath.Join(home, ".local", "share")
	}
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		configDir = filepath.Join(home, ".config")
	}
	var path, note string
	var gen func(io.Writer) error
	switch shell {
	case "bash":
		path = filepath.Join(dataDir, "bash-completion", "completions", "fileindex")
		gen = func(w io.Writer) error { return root.GenBashCompletionV2(w, true) }
	case "zsh":
		path = filepath.Join(dataDir, "zsh", "site-functions", "_fileindex")
		gen = root.GenZshCompletion
		note = "Make sure " + filepath.Dir(path) + " is in your fpath before compinit."
	case "fish":
		path = filepath.Join(configDir, "fish", "completions", "fileindex.fish")
		gen = func(w io.Writer) error { return root.GenFishCompletion(w, true) }
	default:
		return fmt.Errorf("cannot register autocomplete: unsupported shell %q (supported: bash, zsh, fish)", shell)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := gen(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(out, "Autocomplete registered at: %s\n", path)
	if note != "" {
		fmt.Fprintln(out, note)
	}
	fmt.Fprintln(out, "Restart your shell to activate it.")
	return nil
}

func upgradeDBCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade-db",
		Short: "Upgrade the database to the current schema",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			d, err := a.openDB()
			if err != nil {
				return err
			}
			if err := d.Upgrade(a.stderr); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Database at schema v%d: %s\n", db.SchemaVersion, d.Path)
			return nil
		},
	}
}
