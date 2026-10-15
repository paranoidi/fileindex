package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/paranoidi/fileindex/db"
)

func statsCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "Show database statistics",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			d, err := a.openDB()
			if err != nil {
				return err
			}
			s, err := d.Stats()
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Directories: %d\nFiles: %d\nInstances: %d\nTotal size: %s\nDuplicate files: %d\nDatabase size: %s\n",
				s.Dirs, s.Files, s.Instances, db.FormatSize(s.TotalSize), s.DuplicateFiles, db.FormatSize(s.DBSize))
			return nil
		},
	}
}

func vacuumCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "vacuum",
		Short: "Vacuum database to reclaim space",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			d, err := a.openDB()
			if err != nil {
				return err
			}
			fmt.Fprintln(a.stdout, "Vacuuming database...")
			if err := d.Vacuum(); err != nil {
				return err
			}
			fmt.Fprintln(a.stdout, "Database vacuumed successfully")
			return nil
		},
	}
}
