package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paranoidi/fileindex/db"
)

func findCmd(a *app) *cobra.Command {
	var typ string
	var verify, details, words bool
	cmd := &cobra.Command{
		Use:   "find PATTERN [path]",
		Short: "Find files and directories by name (glob, case-insensitive), or files by words",
		Args:  cobra.RangeArgs(1, 2),
		PreRunE: func(*cobra.Command, []string) error {
			if typ != "" && typ != "f" && typ != "d" {
				return usageError{fmt.Errorf("argument -t/--type: invalid choice: '%s' (choose from 'f', 'd')", typ)}
			}
			if words && typ == "d" {
				return usageError{fmt.Errorf("argument -w/--words: only files are word-indexed, not allowed with -t d")}
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			pattern, arg := args[0], "."
			if len(args) > 1 {
				arg = args[1]
			}
			path, err := db.Resolve(arg)
			if err != nil {
				return err
			}
			d, err := a.openDB()
			if err != nil {
				return err
			}
			if err := d.RequireSchema(); err != nil {
				return err
			}
			id, found, err := db.DirByPath(d, path)
			if err != nil {
				return err
			}
			var results []db.Found
			switch {
			case found && words:
				if results, err = d.FindByWords(pattern, *id); err != nil {
					if strings.Contains(err.Error(), "SQL logic error") { // the statement is fixed, so the query is at fault
						return usageError{fmt.Errorf("invalid word query '%s': %v", pattern, err)}
					}
					return err
				}
			case found:
				if results, err = d.FindByName(pattern, *id, typ != "d", typ != "f"); err != nil {
					return err
				}
			}
			if len(results) == 0 {
				fmt.Fprintf(a.stderr, "No matches found for pattern '%s' in %s\n", pattern, path)
				return exitCode(1)
			}

			if details {
				fmt.Fprintf(a.stdout, "Found %d match(es):\n\n", len(results))
			}
			for _, r := range results {
				prefix := ""
				if verify {
					_, err := os.Stat(r.Path)
					switch {
					case err != nil:
						prefix = "✘ "
					case details:
						prefix = "✔ "
					}
				}
				shown := r.Path
				if r.IsDir {
					shown += "/"
				}
				if !details {
					fmt.Fprintln(a.stdout, prefix+shown)
					continue
				}
				fmt.Fprintln(a.stdout, prefix+shown)
				if r.IsDir {
					fmt.Fprintln(a.stdout, "  Type: directory")
				} else {
					fmt.Fprintln(a.stdout, "  Type: file")
					fmt.Fprintf(a.stdout, "  Size: %s\n", db.FormatSize(r.Size))
					if r.Mtime != "" {
						fmt.Fprintf(a.stdout, "  Modified: %s\n", r.Mtime)
					}
				}
				fmt.Fprintln(a.stdout)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&typ, "type", "t", "", "Only files (f) or directories (d)")
	cmd.Flags().BoolVar(&verify, "verify", false, "Mark results missing from the filesystem")
	cmd.Flags().BoolVar(&details, "details", false, "Show size, type and mtime")
	cmd.Flags().BoolVarP(&words, "words", "w", false, `Match file names by words instead of a glob: holiday, scr*, "new york", a NOT b`)
	return cmd
}
