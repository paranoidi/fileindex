package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/paranoidi/fileindex/db"
	"github.com/paranoidi/fileindex/internal/progress"
)

func pruneCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "prune PATH",
		Short: "Remove records of files and directories gone from disk",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path, err := db.Resolve(args[0])
			if err != nil {
				return err
			}
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("Path does not exist: %s", path)
			}
			d, err := a.openDB()
			if err != nil {
				return err
			}
			pp := progress.NewProgress(a.stdout, 0, []string{path}, false)
			p, err := d.PrunePath(a.ctx, path, pp.Prune, func(msg string) { pp.Print(a.stdout, []byte(msg+"\n")) })
			pp.Clear()
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "\nPrune completed:\n  Files removed: %d\n  Directories removed: %d\n", p.Files, p.Dirs)
			return nil
		},
	}
}

func removeCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "remove PATH",
		Short: "Remove a directory tree from the database (the disk is not touched)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path, err := db.Resolve(args[0])
			if err != nil {
				return err
			}
			d, err := a.openDB()
			if err != nil {
				return err
			}
			r, err := d.RemoveTree(path)
			if err != nil {
				return err
			}
			w := a.stdout
			fmt.Fprintf(w, "Removing all instances in directory tree: %s\n", path)
			fmt.Fprintln(w, "Cleaning up orphaned file records")
			fmt.Fprintf(w, "\nRemoved from database:\n  Instances removed: %d\n  Files removed: %d\n  Directories removed: %d\n", r.Instances, r.Files, r.Dirs)
			return nil
		},
	}
}
