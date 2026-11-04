package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/paranoidi/fileindex/db"
	"github.com/paranoidi/fileindex/internal/progress"
	"github.com/paranoidi/fileindex/scan"
)

func duplicatesCmd(a *app) *cobra.Command {
	var within, fullHash bool
	var minSizeArg string
	cmd := &cobra.Command{
		Use:   "duplicates [path]",
		Short: "Find files with the same content (JPEGs compared without metadata)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if within && len(args) == 0 {
				return usageError{fmt.Errorf("--within requires a path")}
			}
			var minSize int64
			if minSizeArg != "" {
				n, err := db.ParseSize(minSizeArg)
				if err != nil { // stderr, not stdout
					fmt.Fprintf(a.stderr, "Error parsing --min-size: %v\n", err)
					return exitCode(1)
				}
				minSize = n
			}
			d, err := a.openDB()
			if err != nil {
				return err
			}
			if err := d.RequireSchema(); err != nil {
				return err
			}
			fmt.Fprintln(a.stdout, "Finding duplicates...")

			var filter []int64
			if len(args) > 0 {
				path, err := db.Resolve(args[0])
				if err != nil {
					return err
				}
				id, found, err := db.DirByPath(d, path)
				if err != nil {
					return err
				}
				if !found {
					fmt.Fprintln(a.stdout, "No duplicate files found")
					return nil
				}
				if filter, err = db.TreeIDs(d, id, true); err != nil {
					return err
				}
			}
			potentialSets := func() ([]db.DupGroup, error) {
				all, err := d.PotentialDuplicates(filter, within)
				return atLeast(all, minSize), err
			}
			if fullHash {
				potential, err := potentialSets()
				if err != nil {
					return err
				}
				if err := a.hashPotential(d, potential); err != nil {
					return err
				}
			}
			all, err := d.Duplicates(filter, within)
			if err != nil {
				return err
			}
			groups := atLeast(all, minSize)
			potential, err := potentialSets()
			if err != nil {
				return err
			}
			fast, none, err := d.CountNotFullyHashed(filter, minSize)
			if err != nil {
				return err
			}

			w := a.stdout
			if len(groups) == 0 && len(potential) == 0 {
				fmt.Fprintln(w, "No duplicate files found")
			}
			if len(groups) > 0 {
				fmt.Fprintf(w, "Found %d sets of duplicate files:\n\n", len(groups))
				fmt.Fprintf(w, "Total wasted space: %s\n", db.FormatSize(printGroups(w, groups)))
			}
			if len(potential) > 0 {
				if len(groups) > 0 {
					fmt.Fprintln(w)
				}
				fmt.Fprintf(w, "Found %d sets of potential duplicates (same size and first 8 KB, not fully hashed):\n\n", len(potential))
				fmt.Fprintf(w, "Potential wasted space: up to %s\n", db.FormatSize(printGroups(w, potential)))
			}
			if fast+none > 0 {
				hint := "Hash them with `fileindex scan`."
				if len(potential) > 0 {
					hint = "Confirm potential duplicates with --full-hash-potential, or hash everything with `fileindex scan`."
				}
				fmt.Fprintf(w, "\n%d files not fully hashed (%d fast-hashed, %d unhashed). %s\n", fast+none, fast, none, hint)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&within, "within", false, "Only groups whose copies are all inside path")
	cmd.Flags().StringVar(&minSizeArg, "min-size", "", "Minimum file size, e.g. 10M, 500KB")
	cmd.Flags().BoolVarP(&fullHash, "full-hash-potential", "f", false, "Fully hash the fast-hashed files of potential duplicates first, to confirm or rule them out. Useful only if scan --fast was used.")
	return cmd
}

func atLeast(groups []db.DupGroup, minSize int64) []db.DupGroup {
	var out []db.DupGroup
	for _, g := range groups {
		if g.Size >= minSize {
			out = append(out, g)
		}
	}
	return out
}

// printGroups lists groups and returns their wasted space.
func printGroups(w io.Writer, groups []db.DupGroup) (wasted int64) {
	for i, g := range groups {
		fmt.Fprintf(w, "%d. File size: %s (%d instances)\n", i+1, db.FormatSize(g.Size), len(g.Locations))
		for _, l := range g.Locations {
			fmt.Fprintf(w, "     %s\n", l)
		}
		fmt.Fprintln(w)
		wasted += g.Wasted
	}
	return wasted
}

// hashPotential fully hashes the fast-hashed files of groups. Files that cannot
// be hashed are listed and stay potential; Ctrl-C keeps what was committed.
func (a *app) hashPotential(d *db.DB, groups []db.DupGroup) error {
	var paths []string
	for _, g := range groups {
		paths = append(paths, g.Unverified...)
	}
	if len(paths) == 0 {
		return nil
	}
	workers, err := a.cfg.ParallelWorkers()
	if err != nil {
		return err
	}
	if workers < 0 {
		return fmt.Errorf("Number of processes must be at least 1")
	}
	a.handlesInterrupt.Store(true)
	defer a.handlesInterrupt.Store(false)
	fmt.Fprintf(a.stdout, "Hashing %d potential duplicate file(s)...\n", len(paths))
	p := progress.NewProgress(a.stdout, max(workers, 1), nil, false)
	prevLog := logPrint
	logPrint = func(w io.Writer, b []byte) { p.Print(w, b) }
	defer func() { logPrint = prevLog }()
	p.Hashing(len(paths))
	st, err := scan.HashPaths(a.ctx, d, paths, workers, p.Worker, p.Speed, p.FileDone)
	p.Clear()
	if err != nil {
		return err
	}
	if st.Interrupted {
		return exitCode(130)
	}
	a.printErrors(st.Errors)
	return nil
}
