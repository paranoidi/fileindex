package cli

import (
	"fmt"

	"github.com/paranoidi/fileindex/db"
	"github.com/spf13/cobra"
)

func initCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Initialize database and configuration",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			d, err := a.openDB()
			if err != nil {
				return err
			}
			if err := d.Init(); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Database initialized at: %s\n", d.Path)
			fmt.Fprintf(a.stdout, "Configuration file at: %s\n", a.cfg.Path)
			return nil
		},
	}
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
