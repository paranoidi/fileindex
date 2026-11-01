package cli

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paranoidi/fileindex/db"
)

// side is one directory of a diff, files keyed by path relative to it.
type side struct {
	path           string
	files          map[string]db.Entry
	names          []string            // sorted keys of files
	byHash         map[string][]string // content hash -> relative paths, sorted
	missing, extra []string
}

func (a *app) loadSide(d *db.DB, path string, id int64, recursive bool) (*side, error) {
	entries, err := d.FilesUnder(id, recursive)
	if err != nil {
		return nil, err
	}
	s := &side{path: path, files: map[string]db.Entry{}, byHash: map[string][]string{}}
	var recorded []string
	for _, e := range entries { // already sorted by path
		rel, err := filepath.Rel(path, e.Path)
		if err != nil {
			return nil, err
		}
		recorded = append(recorded, e.Path)
		if !e.Hashed { // nothing to compare yet; still checked against the filesystem
			continue
		}
		s.files[rel] = e
		s.names = append(s.names, rel)
		s.byHash[e.Content] = append(s.byHash[e.Content], rel)
	}
	s.missing, s.extra, err = a.fsCheck(path, recorded, recursive)
	return s, err
}

func diffCmd(a *app) *cobra.Command {
	var recursive bool
	cmd := &cobra.Command{
		Use:   "diff SRC DST",
		Short: "Compare two indexed directories, and each with the filesystem",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			d, err := a.openDB()
			if err != nil {
				return err
			}
			var sides [2]*side
			for i, label := range []string{"Source", "Destination"} {
				path, err := db.Resolve(args[i])
				if err != nil {
					return err
				}
				id, found, err := db.DirByPath(d, path)
				if err != nil {
					return err
				}
				if !found {
					return fmt.Errorf("%s path '%s' not found in database", label, path)
				}
				if sides[i], err = a.loadSide(d, path, *id, recursive); err != nil {
					return err
				}
			}
			a.printDiff(sides[0], sides[1], recursive)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "Compare whole trees")
	return cmd
}

func (a *app) printDiff(src, dst *side, recursive bool) {
	w := a.stdout
	mode := "non-recursive"
	if recursive {
		mode = "recursive"
	}
	fmt.Fprintf(w, "Comparing %s vs %s\n(%s comparison)\n\n", src.path, dst.path, mode)

	// The two blocks use different labels on purpose.
	for i, s := range []*side{src, dst} {
		if len(s.missing) == 0 && len(s.extra) == 0 {
			continue
		}
		missingLabel, extraLabel := "Missing files", "Extra files not in database"
		if i == 1 {
			missingLabel, extraLabel = "Missing files from filesystem", "Missing files from database"
		}
		fmt.Fprintf(w, "Filesystem check for %s:\n", s.path)
		if len(s.missing) > 0 {
			fmt.Fprintf(w, "  ✘ %s (%d):\n", missingLabel, len(s.missing))
			for _, p := range s.missing {
				fmt.Fprintf(w, "    %s\n", p)
			}
		}
		if len(s.extra) > 0 {
			fmt.Fprintf(w, "  ∆ %s (%d):\n", extraLabel, len(s.extra))
			for _, p := range s.extra {
				fmt.Fprintf(w, "    %s\n", p)
			}
		}
		fmt.Fprintln(w)
	}

	var onlySrc, onlyDst, differ, jpeg []string
	var sameContent [][2]string // src name, dst names joined
	for _, name := range src.names {
		s := src.files[name]
		dd, ok := dst.files[name]
		switch {
		case !ok && dst.byHash[s.Content] != nil:
			sameContent = append(sameContent, [2]string{name, strings.Join(dst.byHash[s.Content], ", ")})
		case !ok:
			onlySrc = append(onlySrc, name)
		case s.Content != dd.Content:
			differ = append(differ, name)
		case s.IsJPEG && s.SHA256 != dd.SHA256:
			jpeg = append(jpeg, name)
		}
	}
	for _, name := range dst.names {
		if _, ok := src.files[name]; !ok && src.byHash[dst.files[name].Content] == nil {
			onlyDst = append(onlyDst, name)
		}
	}

	dbDiffs := len(onlySrc) + len(onlyDst) + len(sameContent) + len(differ) + len(jpeg)
	fmt.Fprint(w, "Directory comparison (database):\n\n")
	if len(onlySrc) > 0 {
		fmt.Fprintf(w, "Only in %s (%d files):\n", src.path, len(onlySrc))
		for _, n := range onlySrc {
			fmt.Fprintf(w, "  %-40s %10s\n", n, db.FormatSize(src.files[n].Size))
		}
		fmt.Fprintln(w)
	}
	if len(onlyDst) > 0 {
		fmt.Fprintf(w, "Only in %s (%d files):\n", dst.path, len(onlyDst))
		for _, n := range onlyDst {
			fmt.Fprintf(w, "  %-40s %10s\n", n, db.FormatSize(dst.files[n].Size))
		}
		fmt.Fprintln(w)
	}
	if len(sameContent) > 0 {
		fmt.Fprintf(w, "Same content, different names (%d groups):\n", len(sameContent))
		for _, sc := range sameContent {
			fmt.Fprintf(w, "  %-40s %10s\n    → renamed/duplicated in %s as: %s\n", sc[0], db.FormatSize(src.files[sc[0]].Size), dst.path, sc[1])
		}
		fmt.Fprintln(w)
	}
	if len(differ) > 0 {
		fmt.Fprintf(w, "Same name, different content (%d files):\n", len(differ))
		for _, n := range differ {
			s, dd := src.files[n], dst.files[n]
			fmt.Fprintf(w, "  %-40s\n    %s: %10s  hash: %s...\n    %s: %10s  hash: %s...\n",
				n, src.path, db.FormatSize(s.Size), s.Content[:16], dst.path, db.FormatSize(dd.Size), dd.Content[:16])
		}
		fmt.Fprintln(w)
	}
	if len(jpeg) > 0 {
		fmt.Fprintf(w, "Same JPEG data, metadata differences (%d files):\n", len(jpeg))
		for _, n := range jpeg {
			s, dd := src.files[n], dst.files[n]
			fmt.Fprintf(w, "  %-40s %10s\n    %s hash: %s...\n    %s hash: %s...\n",
				n, db.FormatSize(s.Size), src.path, s.SHA256[:16], dst.path, dd.SHA256[:16])
		}
		fmt.Fprintln(w)
	}

	fsIssues := slices.ContainsFunc([]*side{src, dst}, func(s *side) bool { return len(s.missing)+len(s.extra) > 0 })
	if dbDiffs == 0 && !fsIssues {
		fmt.Fprintln(w, "Directories are identical and all files present in filesystem.")
		return
	}
	fmt.Fprintln(w, "Summary:")
	if fsIssues {
		fmt.Fprintln(w, "  Filesystem checks:")
		for _, s := range []*side{src, dst} {
			if len(s.missing)+len(s.extra) > 0 {
				fmt.Fprintf(w, "    %s: %d missing, %d extra\n", s.path, len(s.missing), len(s.extra))
			}
		}
	}
	if dbDiffs > 0 {
		fmt.Fprintf(w, "  Database comparison:\n    Files only in %s: %d\n    Files only in %s: %d\n"+
			"    Files with same content but different names: %d\n    Files with same name but different content: %d\n"+
			"    JPEG files with same image data but different metadata: %d\n",
			src.path, len(onlySrc), dst.path, len(onlyDst), len(sameContent), len(differ), len(jpeg))
	}
}
