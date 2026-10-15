package cli

import (
	"fmt"

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
			if err := d.Init(a.stderr); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Database initialized at: %s\n", d.Path)
			fmt.Fprintf(a.stdout, "Configuration file at: %s\n", a.cfg.Path)
			return nil
		},
	}
}
