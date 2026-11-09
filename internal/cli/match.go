package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paranoidi/fileindex/db"
	"github.com/paranoidi/fileindex/scan"
)

type matchOpts struct {
	hash                            string
	recursive, fast, details, short bool
	abortDepth                      int // -1 = unlimited
	onlyMatched, onlyUnmatched      bool
}

func matchCmd(a *app) *cobra.Command {
	var o matchOpts
	cmd := &cobra.Command{
		Use:   "match [path]",
		Short: "Find indexed copies of a file, a directory's files, or a hash",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			d, err := a.openDB()
			if err != nil {
				return err
			}
			if o.abortDepth >= 0 {
				o.recursive = true
			}
			if o.hash != "" {
				return a.matchHash(d, o.hash, o)
			}
			arg := "."
			if len(args) > 0 && args[0] != "" {
				arg = args[0]
			}
			path, err := db.Resolve(arg)
			if err != nil {
				return err
			}
			// A path that exists, even as a broken symlink, is a path.
			if _, err := os.Lstat(path); err != nil {
				return a.matchHash(d, arg, o)
			}
			fi, err := os.Stat(path)
			switch {
			case err != nil || fi.Mode().IsRegular():
				return a.matchFile(d, path, o)
			case fi.IsDir():
				return a.matchDir(d, path, o)
			}
			return fmt.Errorf("Path is not a file or directory: %s", path)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.hash, "hash", "", "Explicit hash value to search for")
	f.BoolVarP(&o.recursive, "recursive", "r", false, "Process directories recursively")
	f.BoolVarP(&o.fast, "fast", "f", false, "Match on the sample hash only (first, middle and last 8 KiB)")
	f.BoolVar(&o.details, "details", false, "Show detailed information")
	f.BoolVar(&o.short, "short", false, "Print only the number of matches")
	f.IntVar(&o.abortDepth, "abort-depth", -1, "Abort with an error on a file deeper than this (implies -r)")
	f.BoolVar(&o.onlyMatched, "matched", false, "Directory mode: list only files with matches")
	f.BoolVar(&o.onlyUnmatched, "unmatched", false, "Directory mode: list only files without matches")
	cmd.MarkFlagsMutuallyExclusive("matched", "unmatched")
	return cmd
}

func (a *app) matchHash(d *db.DB, h string, o matchOpts) error {
	rows, err := d.FindByHash(h)
	if err != nil {
		return err
	}
	if o.short {
		fmt.Fprintln(a.stdout, len(rows))
		if len(rows) == 0 {
			return exitCode(1)
		}
		return nil
	}
	if len(rows) == 0 {
		fmt.Fprintln(a.stderr, "No files found matching the provided hash")
		return exitCode(1)
	}
	if !o.details {
		for _, r := range rows {
			fmt.Fprintln(a.stdout, r.Path)
		}
		return nil
	}
	w := a.stdout
	fmt.Fprintf(w, "Found %d file instance(s):\n\n", len(rows))
	for _, r := range rows {
		fmt.Fprintf(w, "File: %s\nSize: %s\nMD5: %s\nSHA256: %s\n", r.Path, db.FormatSize(r.Size), r.MD5, r.SHA256)
		if r.NoExifJPEG != "" {
			fmt.Fprintf(w, "SHA256 (no EXIF): %s\n", r.NoExifJPEG)
		}
		if r.Sample != "" {
			fmt.Fprintf(w, "SHA256 (sample): %s\n", r.Sample)
		}
		if r.Mtime != "" {
			fmt.Fprintf(w, "Modified: %s\n", r.Mtime)
		}
		fmt.Fprintln(w)
	}
	return nil
}

// pickHash: --fast uses the sample hash; a JPEG with image data uses the
// metadata-free hash, so copies with different EXIF match; else full sha256.
func pickHash(path string, h scan.Hashes, fast bool) (label, value string) {
	ext := strings.ToLower(filepath.Ext(path))
	switch {
	case fast:
		return "SHA256 (sample)", h.Sample
	case (ext == ".jpg" || ext == ".jpeg") && h.NoExifJPEG != nil:
		return "SHA256 (no EXIF)", *h.NoExifJPEG
	}
	return "SHA256", h.SHA256
}

// matchesOf looks up a hash and drops rows that are the input file itself.
func matchesOf(d *db.DB, value, self string) ([]db.Match, error) {
	rows, err := d.FindByHash(value)
	if err != nil {
		return nil, err
	}
	var out []db.Match
	for _, r := range rows {
		if p, err := db.Resolve(r.Path); err == nil && p == self {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (a *app) matchFile(d *db.DB, path string, o matchOpts) error {
	details := o.details && !o.short
	w := a.stdout
	if details {
		fmt.Fprintf(w, "Calculating hash for: %s\n", path)
	}
	r := scan.HashFile(path)
	if r.Err != nil {
		return fmt.Errorf("Error calculating hashes: %v", r.Err)
	}
	label, value := pickHash(path, r.Hashes, o.fast)
	if details {
		fmt.Fprintf(w, "%s: %s\n\n", label, value)
	}
	matches, err := matchesOf(d, value, path)
	if err != nil {
		return err
	}
	if o.short { // exits 0 either way, so match-count keeps printing MATCHES 0
		fmt.Fprintln(w, len(matches))
		return nil
	}
	fmt.Fprintf(w, "%s\n  Hash (%s): %s\n", path, label, value)
	if len(matches) == 0 {
		fmt.Fprintln(w, "  ✘ No matches found in database")
		return exitCode(1)
	}
	fmt.Fprintf(w, "  ✔ Found %d match(es) in database:\n", len(matches))
	for _, m := range matches {
		fmt.Fprintf(w, "    %s (%s)\n", m.Path, db.FormatSize(m.Size))
	}
	return nil
}

// dirFiles lists the regular files (symlinks to files included) in dir, or in
// its whole tree with recursive; dir symlinks are not followed.
func dirFiles(dir string, recursive bool) ([]string, error) {
	var out []string
	if !recursive {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrPermission) {
			return nil, fmt.Errorf("Permission denied accessing %s", dir)
		} else if err != nil {
			return nil, err
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
				out = append(out, p)
			}
		}
		return out, nil
	}
	_ = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil // unreadable dirs are skipped silently
		}
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			out = append(out, p)
		}
		return nil
	})
	return out, nil
}

func (a *app) matchDir(d *db.DB, dir string, o matchOpts) error {
	files, err := dirFiles(dir, o.recursive)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		if o.short {
			fmt.Fprintln(a.stdout, 0)
		} else {
			fmt.Fprintf(a.stderr, "No files found in %s\n", dir)
		}
		return exitCode(1)
	}
	w := a.stdout
	matched, errs := 0, 0
	for _, p := range files {
		rel, _ := filepath.Rel(dir, p)
		if o.abortDepth >= 0 && strings.Count(rel, string(filepath.Separator))+1 > o.abortDepth {
			return fmt.Errorf("Abort depth %d exceeded: %s", o.abortDepth, rel)
		}
		r := scan.HashFile(p)
		if r.Err != nil {
			errs++
			if !o.short {
				fmt.Fprintf(w, "✘ %s\n  Error: %v\n\n", rel, r.Err)
			}
			continue
		}
		_, value := pickHash(p, r.Hashes, o.fast)
		matches, err := matchesOf(d, value, p)
		if err != nil {
			return err
		}
		if len(matches) > 0 {
			matched++
		}
		if o.short || (o.onlyMatched && len(matches) == 0) || (o.onlyUnmatched && len(matches) > 0) {
			continue
		}
		if len(matches) == 0 {
			fmt.Fprintf(w, "✘ %s\n  No matches in database\n\n", rel)
			continue
		}
		fmt.Fprintf(w, "✔ %s (%s)\n  Found %d match(es):\n", rel, db.FormatSize(r.Size), len(matches))
		for _, m := range matches {
			fmt.Fprintf(w, "    %s\n", m.Path)
		}
		fmt.Fprintln(w)
	}
	if o.short {
		fmt.Fprintln(w, matched)
	} else {
		fmt.Fprintf(w, "Summary:\n  Total files: %d\n  Matched: %d\n  Not matched: %d\n", len(files), matched, len(files)-matched-errs)
		if errs > 0 {
			fmt.Fprintf(w, "  Errors: %d\n", errs)
		}
	}
	if errs > 0 {
		return exitCode(1)
	}
	return nil
}
