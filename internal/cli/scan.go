package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/paranoidi/fileindex/db"
	"github.com/paranoidi/fileindex/internal/progress"
	"github.com/paranoidi/fileindex/scan"
)

// batchFiles is scan.Options.BatchFiles; 0 is scan's default. Tests lower it.
var batchFiles int

func scanCmd(a *app) *cobra.Command {
	var quiet, all bool
	cmd := &cobra.Command{
		Use:   "scan PATHS...",
		Short: "Scan directories and index files",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			out := a.stdout
			if quiet { // quiet means no stdout at all
				out = io.Discard
			}
			return a.scan(args, out, quiet, !all)
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress progress output")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "Re-hash every file, including ones already indexed with the same size and mtime")
	return cmd
}

// scanReporter draws Run's progress on the terminal: the hashing block, then
// after hashed swaps in the pruning one.
type scanReporter struct {
	*progress.Progress
	out    io.Writer
	hashed func(scan.Stats)
}

func (s *scanReporter) Hashed(st scan.Stats) { s.hashed(st) }
func (s *scanReporter) Log(msg string)       { s.Print(s.out, []byte(msg+"\n")) }

func (a *app) scan(args []string, out io.Writer, quiet, missingOnly bool) error {
	rules, err := scan.RulesFromConfig(a.cfg)
	if err != nil {
		return err
	}
	workers, err := a.cfg.ParallelWorkers()
	if err != nil {
		return err
	}
	if workers < 0 {
		return fmt.Errorf("Number of processes must be at least 1")
	}
	roots, err := scan.ResolveRoots(args) // Run resolves them too; the progress block needs them first
	if err != nil {
		return err
	}
	d, err := a.openDB()
	if err != nil {
		return err
	}
	if err := d.RequireSchema(); err != nil { // before the "Scanning" line
		return err
	}

	a.handlesInterrupt.Store(true)
	rep := &scanReporter{Progress: progress.NewProgress(a.stdout, max(workers, 1), roots, quiet), out: out}
	prevLog := logPrint
	logPrint = rep.Print
	defer func() { logPrint = prevLog }()
	fmt.Fprintf(out, "Scanning %d path(s)...\n", len(roots))
	start := time.Now()
	rep.hashed = func(st scan.Stats) {
		rep.Clear()
		printScanStats(out, st, time.Since(start), st.Interrupted)
		// From here Ctrl-C exits outright, like the prune command.
		// Checked after the store so a Ctrl-C landing during commit/wait isn't lost.
		a.handlesInterrupt.Store(false)
		if st.Interrupted || a.ctx.Err() != nil {
			return // Run sees the cancel too and stops
		}
		fmt.Fprintln(out, "\nPruning directories...")
		rep.Progress = progress.NewProgress(a.stdout, 0, roots, quiet)
	}

	st, err := scan.Run(a.ctx, d, roots, scan.Options{Rules: rules, Workers: workers, MissingOnly: missingOnly, BatchFiles: batchFiles}, rep)
	rep.Clear()
	if err != nil {
		return err
	}
	if st.Interrupted {
		return exitCode(130)
	}
	if st.PrunedFiles > 0 {
		fmt.Fprintf(out, "Files pruned: %d\n", st.PrunedFiles)
	}
	if st.PrunedDirs > 0 {
		fmt.Fprintf(out, "Directories pruned: %d\n", st.PrunedDirs)
	}

	// ponytail: the errors list goes to stderr, not stdout,
	// so -q still shows failures.
	if len(st.Errors) > 0 {
		fmt.Fprintln(a.stderr, "\nErrors encountered:")
		for _, e := range st.Errors[:min(10, len(st.Errors))] {
			fmt.Fprintf(a.stderr, "  %s\n", e)
		}
		if len(st.Errors) > 10 {
			fmt.Fprintf(a.stderr, "  ... and %d more errors\n", len(st.Errors)-10)
		}
	}
	return nil
}

func printScanStats(out io.Writer, st scan.Stats, elapsed time.Duration, interrupted bool) {
	verb := "completed"
	if interrupted {
		verb = "interrupted"
	}
	fmt.Fprintf(out, "\nScan %s in %.2f seconds\n", verb, elapsed.Seconds())
	fmt.Fprintf(out, "Files processed: %d\n", st.Processed)
	for _, c := range []struct {
		label string
		n     int
	}{{"Files added", st.Added}, {"Files updated", st.Updated}, {"Files skipped", st.Skipped}, {"Files with errors", st.Errored}} {
		if c.n > 0 {
			fmt.Fprintf(out, "%s: %d\n", c.label, c.n)
		}
	}
	fmt.Fprintf(out, "Total size processed: %s\n", db.FormatSize(st.TotalSize))
	if st.TotalSize > 0 && elapsed > 0 {
		fmt.Fprintf(out, "Hashing speed: %.2f MB/s\n", float64(st.TotalSize)/elapsed.Seconds()/1048576)
	}
}
