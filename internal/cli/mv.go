package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/paranoidi/fileindex/db"
)

func mvCmd(a *app) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "mv SRC DST",
		Short: "Update the database after a file or directory was moved on disk",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			src, err := db.Resolve(args[0])
			if err != nil {
				return err
			}
			dst, err := db.Resolve(args[1])
			if err != nil {
				return err
			}
			if src == dst {
				fmt.Fprintln(a.stdout, "Source and destination paths match. Nothing to do.")
				return nil
			}
			d, err := a.openDB()
			if err != nil {
				return err
			}

			check := func(isDir bool) error {
				if force {
					return nil
				}
				fi, err := os.Stat(dst)
				problem := ""
				switch {
				case err != nil:
					problem = "Destination path does not exist"
				case isDir && !fi.IsDir():
					problem = "Destination path is not a directory"
				case !isDir && !fi.Mode().IsRegular():
					problem = "Destination path is not a file"
				}
				if problem != "" {
					fmt.Fprintf(a.stderr, "Error: %s: %s\nUse -f to override this check.\n", problem, dst)
					return exitCode(1)
				}
				return nil
			}
			m, err := d.Move(src, dst, check)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "\nMove completed:\n  Directory rows updated: %d\n  File instances updated: %d\n", m.Dirs, m.Files)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Force update even if filesystem state does not match")
	return cmd
}
