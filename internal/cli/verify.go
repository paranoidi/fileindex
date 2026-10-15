package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/paranoidi/fileindex/db"
	"github.com/paranoidi/fileindex/scan"
)

const okMark = "✔︎" // ✔ with the text variation selector, so it renders as text, not emoji

// lineProgress rewrites one line in place with \r, padding over the previous text.
type lineProgress struct {
	w    io.Writer
	last int
}

func (p *lineProgress) print(line string) {
	s := "\r" + line
	n := utf8.RuneCountInString(s)
	fmt.Fprint(p.w, s+strings.Repeat(" ", max(0, p.last-n)))
	p.last = n
}

func (p *lineProgress) clear() {
	if p.last > 0 {
		fmt.Fprint(p.w, strings.Repeat(" ", p.last)+"\r")
		p.last = 0
	}
}

func verifyCmd(a *app) *cobra.Command {
	var recursive, quiet bool
	cmd := &cobra.Command{
		Use:   "verify path",
		Short: "Check indexed files against the disk by size and hash",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path, err := db.Resolve(args[0])
			if err != nil {
				return err
			}
			fi, err := os.Stat(path)
			if err != nil {
				return fmt.Errorf("Path '%s' does not exist", path)
			}
			d, err := a.openDB()
			if err != nil {
				return err
			}
			if !fi.IsDir() {
				return a.verifyFile(d, path, quiet)
			}
			return a.verifyDir(d, path, recursive, quiet)
		},
	}
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "Verify the whole tree")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "No progress output")
	return cmd
}

func printHashMismatch(w io.Writer, indent, wantMD5, gotMD5, wantSHA, gotSHA string) {
	fmt.Fprintf(w, "%sExpected MD5:    %s\n%sActual MD5:      %s\n%sExpected SHA256: %s\n%sActual SHA256:   %s\n",
		indent, wantMD5, indent, gotMD5, indent, wantSHA, indent, gotSHA)
}

func (a *app) verifyFile(d *db.DB, path string, quiet bool) error {
	info, found, err := d.FileInfo(path)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("File '%s' not found in database", path)
	}
	w := a.stdout
	if !quiet {
		fmt.Fprintf(w, "Verifying: %s\n", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Size() != info.Size {
		fmt.Fprintf(w, "\n✘ Size mismatch:\n  Expected: %s, Actual: %s\n", db.FormatSize(info.Size), db.FormatSize(fi.Size()))
		return exitCode(1)
	}
	r := scan.HashFile(path)
	if r.Err != nil {
		fmt.Fprintf(w, "\n✘ Error calculating hashes: %v\n", r.Err)
		return exitCode(1)
	}
	if r.Hashes.MD5 != info.MD5 {
		fmt.Fprintln(w, "\n✘ Hash mismatch:")
		printHashMismatch(w, "  ", info.MD5, r.Hashes.MD5, info.SHA256, r.Hashes.SHA256)
		return exitCode(1)
	}
	fmt.Fprintf(w, "%s File verified successfully\n", okMark)
	return nil
}

type verifyIssue struct {
	path, detail string
}

func (a *app) verifyDir(d *db.DB, path string, recursive, quiet bool) error {
	id, found, err := db.DirByPath(d, path)
	if err != nil {
		return err
	}
	var entries []db.Entry
	if found {
		if entries, err = d.FilesUnder(*id, recursive); err != nil {
			return err
		}
	}
	if len(entries) == 0 {
		if found && !recursive {
			return fmt.Errorf("no files directly in %s; use -r", path)
		}
		return fmt.Errorf("Path '%s' not found in database", path)
	}

	w := a.stdout
	progress := &lineProgress{w: w}
	var ok int
	var missing []string
	var sizes, hashes, failed []verifyIssue
	var recorded []string
	for k, e := range entries {
		recorded = append(recorded, e.Path)
		if !quiet {
			progress.print(fmt.Sprintf("Verifying [%d/%d]: %s", k+1, len(entries), e.Path))
		}
		fi, err := os.Stat(e.Path)
		if err != nil {
			missing = append(missing, e.Path)
			continue
		}
		if fi.Size() != e.Size {
			sizes = append(sizes, verifyIssue{e.Path, fmt.Sprintf("Expected: %s, Actual: %s", db.FormatSize(e.Size), db.FormatSize(fi.Size()))})
			continue
		}
		r := scan.HashFile(e.Path)
		switch {
		case r.Err != nil: // a file that can't be hashed is an issue
			failed = append(failed, verifyIssue{e.Path, r.Err.Error()})
		case r.Hashes.MD5 != e.MD5:
			var b strings.Builder
			printHashMismatch(&b, "    ", e.MD5, r.Hashes.MD5, e.SHA256, r.Hashes.SHA256)
			hashes = append(hashes, verifyIssue{e.Path, strings.TrimSuffix(b.String(), "\n")})
		default:
			ok++
		}
	}
	if !quiet {
		progress.clear()
	}
	_, extra, err := a.fsCheck(path, recorded, recursive)
	if err != nil {
		return err
	}

	mode := "non-recursive"
	if recursive {
		mode = "recursive"
	}
	fmt.Fprintf(w, "Verification of %s\n(%s verification)\n\n", path, mode)
	if len(missing) > 0 {
		fmt.Fprintf(w, "✘ Missing files from filesystem (%d):\n", len(missing))
		for _, p := range missing {
			fmt.Fprintf(w, "  %s\n", p)
		}
		fmt.Fprintln(w)
	}
	for _, sec := range []struct {
		title  string
		issues []verifyIssue
		indent string
	}{{"Size mismatches", sizes, "    "}, {"Hash mismatches", hashes, ""}, {"Hash errors", failed, "    "}} {
		if len(sec.issues) == 0 {
			continue
		}
		fmt.Fprintf(w, "✘ %s (%d):\n", sec.title, len(sec.issues))
		for _, is := range sec.issues {
			fmt.Fprintf(w, "  %s\n%s%s\n", is.path, sec.indent, is.detail)
		}
		fmt.Fprintln(w)
	}
	if len(extra) > 0 {
		fmt.Fprintf(w, "∆ Missing files from database (%d):\n", len(extra))
		for _, p := range extra {
			fmt.Fprintf(w, "  %s\n", p)
		}
		fmt.Fprintln(w)
	}

	if len(missing)+len(sizes)+len(hashes)+len(failed)+len(extra) == 0 {
		fmt.Fprintf(w, "%s All files verified successfully (%d files)\n", okMark, ok)
		return nil
	}
	fmt.Fprintf(w, "Summary:\n  %s Verified OK:      %d\n  ✘ Missing files:    %d\n  ✘ Size mismatches:  %d\n  ✘ Hash mismatches:  %d\n",
		okMark, ok, len(missing), len(sizes), len(hashes))
	if len(failed) > 0 {
		fmt.Fprintf(w, "  ✘ Hash errors:      %d\n", len(failed))
	}
	fmt.Fprintf(w, "  ∆ Extra files:      %d\n", len(extra))
	return exitCode(1)
}
