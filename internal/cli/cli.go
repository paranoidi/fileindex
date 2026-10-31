// Package cli wires the cobra commands. Each command lives in its own file.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"sync/atomic"

	"github.com/spf13/cobra"

	"github.com/paranoidi/fileindex/config"
	"github.com/paranoidi/fileindex/db"
)

// exitCode ends a command with a code and no "Error:" line (e.g. "differences found").
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

// usageError is a bad argument found after parsing; it exits 2 like a flag error.
type usageError struct{ error }

// app is the per-run state commands share.
type app struct {
	cfg    *config.Config
	db     *db.DB
	stdout io.Writer
	stderr io.Writer
	// ctx is cancelled by Ctrl-C once a command has set handlesInterrupt;
	// otherwise Ctrl-C prints "Operation interrupted by user" and exits 130.
	ctx              context.Context
	handlesInterrupt atomic.Bool
}

// openDB opens the configured DB on first use.
func (a *app) openDB() (*db.DB, error) {
	if a.db == nil {
		d, err := db.Open(a.cfg.DatabasePath())
		if err != nil {
			return nil, err
		}
		a.db = d
	}
	return a.db, nil
}

// version is the git tag go build stamped into the binary, or "(devel)".
func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "(devel)"
}

// Run executes one fileindex invocation and returns its exit code.
// Usage errors exit 2, command errors print "Error: <msg>" and exit 1.
func Run(args []string, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &app{stdout: stdout, stderr: stderr, ctx: ctx}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	defer signal.Stop(sigs)
	go func() {
		select {
		case <-sigs:
			if a.handlesInterrupt.Load() {
				cancel()
				return
			}
			fmt.Fprintln(stderr, "\nOperation interrupted by user")
			os.Exit(130)
		case <-ctx.Done():
		}
	}()

	var configPath string
	var verbose bool
	started := false

	root := &cobra.Command{
		Use:           "fileindex",
		Short:         "Index local files by content hash",
		Version:       version(),
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			started = true
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			a.cfg = cfg
			setupLogging(stderr, verbose, cfg.Get("output", "log_level"))
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SetOut(stdout)
			cmd.Help()
			return exitCode(1)
		},
	}
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().StringVar(&configPath, "config", "", "Path to configuration file")
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose output")
	root.Args = cobra.NoArgs

	root.AddCommand(initCmd(a), statsCmd(a), vacuumCmd(a), scanCmd(a), findCmd(a), lsCmd(a), matchCmd(a), duplicatesCmd(a), diffCmd(a), verifyCmd(a), treeCmd(a), mvCmd(a), pruneCmd(a), removeCmd(a), exportCmd(a), importCmd(a), tagCmd(a), tagsCmd(a), findTaggedCmd(a))

	err := root.Execute()
	if a.db != nil {
		a.db.Close()
	}
	var code exitCode
	var usage usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &code):
		return int(code)
	case !started, errors.As(err, &usage):
		fmt.Fprintf(stderr, "fileindex: error: %v\n", err)
		return 2
	}
	fmt.Fprintf(stderr, "Error: %v\n", err)
	return 1
}
