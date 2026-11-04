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
	var quiet, all, noHash, fast bool
	cmd := &cobra.Command{
		Use:   "scan PATHS...",
		Short: "Scan directories and index files",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if fast && (all || noHash) { // --all clears file_id, which would wipe existing full hashes
				return usageError{fmt.Errorf("--fast cannot be combined with --all or --no-hash")}
			}
			out := a.stdout
			if quiet { // quiet means no stdout at all
				out = io.Discard
			}
			return a.scan(args, out, quiet, !all, noHash, fast)
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress progress output")
	cmd.Flags().BoolVar(&noHash, "no-hash", false, "Only index names, sizes and mtimes; files stay unhashed (searchable by name) until a later scan hashes them")
	cmd.Flags().BoolVarP(&fast, "fast", "f", false, "Hash only the first 8 KB of larger files; a later scan without --fast hashes them fully. duplicates reports them as potential, and misses re-tagged JPEGs until then")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "Re-hash every file, including ones already indexed with the same size and mtime")
	return cmd
}

// scanReporter draws Run's progress on the terminal, one block per stage in
// Run's order: scanning, pruning, hashing. Each stage change swaps the block.
type scanReporter struct {
	*progress.Progress
	out         io.Writer
	workers     int
	skipHash    bool
	fast        bool
	elapsed     func() time.Duration
	newProgress func(workers int) *progress.Progress
}

func (s *scanReporter) Log(msg string) { s.Print(s.out, []byte(msg+"\n")) }
func (s *scanReporter) Indexed(int)    {}

// Discovered ends the scanning stage; pruning follows unless interrupted.
func (s *scanReporter) Discovered(st scan.Stats) {
	s.Clear()
	if st.Interrupted {
		return
	}
	fmt.Fprintln(s.out, "\nPruning directories...")
	s.Progress = s.newProgress(0)
}

// Pruned ends the pruning stage; hashing follows unless skipped.
func (s *scanReporter) Pruned(_ scan.Stats, toHash int) {
	s.Clear()
	if s.skipHash || toHash == 0 {
		return
	}
	verb := "Hashing"
	if s.fast {
		verb = "Fast-hashing"
	}
	fmt.Fprintf(s.out, "\n%s %d file(s)...\n", verb, toHash)
	s.Progress = s.newProgress(max(s.workers, 1))
	s.Hashing(toHash)
}

// Hashed is Run's last call: the summary.
func (s *scanReporter) Hashed(st scan.Stats) {
	s.Clear()
	printScanStats(s.out, st, s.elapsed(), st.Interrupted)
}

func (a *app) scan(args []string, out io.Writer, quiet, missingOnly, skipHash, fast bool) error {
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
	defer a.handlesInterrupt.Store(false)
	start := time.Now()
	rep := &scanReporter{
		Progress: progress.NewProgress(a.stdout, 0, roots, quiet), out: out, workers: workers, skipHash: skipHash, fast: fast,
		elapsed:     func() time.Duration { return time.Since(start) },
		newProgress: func(w int) *progress.Progress { return progress.NewProgress(a.stdout, w, roots, quiet) },
	}
	prevLog := logPrint
	logPrint = func(w io.Writer, b []byte) { rep.Print(w, b) } // rep.Progress changes between stages
	defer func() { logPrint = prevLog }()
	fmt.Fprintf(out, "Scanning %d path(s)...\n", len(roots))

	st, err := scan.Run(a.ctx, d, roots, scan.Options{Rules: rules, Workers: workers, MissingOnly: missingOnly, SkipHash: skipHash, Fast: fast, BatchFiles: batchFiles}, rep)
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

	a.printErrors(st.Errors)
	return nil
}

// printErrors lists per-file errors.
// ponytail: the errors list goes to stderr, not stdout,
// so -q still shows failures.
func (a *app) printErrors(errs []string) {
	if len(errs) == 0 {
		return
	}
	fmt.Fprintln(a.stderr, "\nErrors encountered:")
	for _, e := range errs[:min(10, len(errs))] {
		fmt.Fprintf(a.stderr, "  %s\n", e)
	}
	if len(errs) > 10 {
		fmt.Fprintf(a.stderr, "  ... and %d more errors\n", len(errs)-10)
	}
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
