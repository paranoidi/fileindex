package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/paranoidi/fileindex/db"
)

func duplicatesCmd(a *app) *cobra.Command {
	var within bool
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
			all, err := d.Duplicates(filter, within)
			if err != nil {
				return err
			}
			var groups []db.DupGroup
			for _, g := range all {
				if g.Size >= minSize {
					groups = append(groups, g)
				}
			}
			if len(groups) == 0 {
				fmt.Fprintln(a.stdout, "No duplicate files found")
				return nil
			}

			w := a.stdout
			fmt.Fprintf(w, "Found %d sets of duplicate files:\n\n", len(groups))
			var wasted int64
			for i, g := range groups {
				fmt.Fprintf(w, "%d. File size: %s (%d instances)\n", i+1, db.FormatSize(g.Size), len(g.Locations))
				for _, l := range g.Locations {
					fmt.Fprintf(w, "     %s\n", l)
				}
				fmt.Fprintln(w)
				wasted += g.Wasted
			}
			fmt.Fprintf(w, "Total wasted space: %s\n", db.FormatSize(wasted))
			return nil
		},
	}
	cmd.Flags().BoolVar(&within, "within", false, "Only groups whose copies are all inside path")
	cmd.Flags().StringVar(&minSizeArg, "min-size", "", "Minimum file size, e.g. 10M, 500KB")
	return cmd
}
