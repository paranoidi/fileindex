package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paranoidi/fileindex/db"
	"github.com/paranoidi/fileindex/scan"
)

func tagSuffix(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	return " #" + strings.Join(tags, " #")
}

func lsCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "ls [path]",
		Short: "List an indexed directory and compare it with the filesystem",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			arg := "."
			if len(args) > 0 {
				arg = args[0]
			}
			path, err := db.Resolve(arg)
			if err != nil {
				return err
			}
			d, err := a.openDB()
			if err != nil {
				return err
			}
			id, found, err := db.DirByPath(d, path)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("Path '%s' not found in database", path)
			}
			files, subdirs, err := d.ListDir(*id)
			if err != nil {
				return err
			}

			w := a.stdout
			fmt.Fprintf(w, "Contents of %s:\n\n", path)
			if len(subdirs) > 0 {
				fmt.Fprintln(w, "Directories:")
				for _, s := range subdirs {
					fmt.Fprintf(w, "  %s/%s\n", s.Name, tagSuffix(s.Tags))
				}
				fmt.Fprintln(w)
			}
			if len(files) > 0 {
				fmt.Fprintln(w, "Files:")
				var total int64
				for _, f := range files {
					mtime := f.Mtime
					if mtime == "" {
						mtime = "N/A"
					}
					fmt.Fprintf(w, "  %-40s %10s  %s%s\n", f.Name, db.FormatSize(f.Size), mtime, tagSuffix(f.Tags))
					total += f.Size
				}
				fmt.Fprintf(w, "\nTotal: %d file(s), %s\n", len(files), db.FormatSize(total))
			} else {
				fmt.Fprintln(w, "(no files)")
				if len(subdirs) == 0 {
					fmt.Fprintln(w, "(empty directory)")
				}
			}

			if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
				return nil
			}
			var recorded []string
			for _, f := range files {
				recorded = append(recorded, filepath.Join(path, f.Name))
			}
			missing, extra, err := a.fsCheck(path, recorded, false)
			if err != nil {
				return err
			}
			if len(missing) > 0 || len(extra) > 0 {
				fmt.Fprintln(w)
			}
			if len(missing) > 0 {
				fmt.Fprintf(w, "✘ Missing files from filesystem (%d):\n", len(missing))
				for _, m := range missing {
					fmt.Fprintf(w, "  %s\n", filepath.Base(m))
				}
				fmt.Fprintln(w)
			}
			if len(extra) > 0 {
				fmt.Fprintf(w, "∆ Missing files from database (%d):\n", len(extra))
				for _, e := range extra {
					fmt.Fprintf(w, "  %s\n", filepath.Base(e))
				}
				fmt.Fprintln(w)
			}
			if len(missing) > 0 || len(extra) > 0 {
				return exitCode(1)
			}
			return nil
		},
	}
}

// fsCheck compares recorded file paths under root with the disk: one level, or
// the whole tree with recursive. On disk, only non-empty regular files that scan
// would index count, and the check always runs, even with no recorded
// files. Returns recorded-but-gone and present-but-unrecorded paths, sorted.
func (a *app) fsCheck(root string, recorded []string, recursive bool) (missing, extra []string, err error) {
	rules, err := scan.RulesFromConfig(a.cfg)
	if err != nil {
		return nil, nil, err
	}
	onDisk := map[string]bool{}
	if fi, err := os.Stat(root); err == nil && fi.IsDir() {
		if recursive {
			for p := range rules.Walk(root) {
				onDisk[p] = true
			}
		} else {
			entries, err := os.ReadDir(root)
			if err != nil {
				return nil, nil, err
			}
			for _, e := range entries {
				p := filepath.Join(root, e.Name())
				fi, err := os.Stat(p)
				if err == nil && fi.Mode().IsRegular() && rules.IncludeFile(e.Name()) {
					onDisk[p] = true
				}
			}
		}
	}
	inDB := map[string]bool{}
	for _, p := range recorded {
		inDB[p] = true
		if !onDisk[p] {
			missing = append(missing, p)
		}
	}
	for p := range onDisk {
		if !inDB[p] {
			extra = append(extra, p)
		}
	}
	slices.Sort(missing)
	slices.Sort(extra)
	return missing, extra, nil
}
