// Package scan finds files to index and hashes them.
package scan

import (
	"fmt"
	"io/fs"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"github.com/paranoidi/fileindex/config"
	"github.com/paranoidi/fileindex/db"
)

// Rules decide which files and dirs a walk visits. scan, tree and match share them.
type Rules struct {
	Patterns       []string // include patterns; empty includes everything
	ExcludeDirs    []string
	ExcludeFiles   []string
	FollowSymlinks bool
}

func RulesFromConfig(c *config.Config) (Rules, error) {
	follow, err := c.FollowSymlinks()
	return Rules{c.FilePatterns(), c.ExcludeDirs(), c.ExcludeFiles(), follow}, err
}

// IncludeFile matches a basename: exclude patterns are case-sensitive, include
// patterns effectively case-insensitive.
func (r Rules) IncludeFile(name string) bool {
	for _, p := range r.ExcludeFiles {
		if Fnmatch(name, p) {
			return false
		}
	}
	if len(r.Patterns) == 0 {
		return true
	}
	lower := db.Lower(name)
	for _, p := range r.Patterns {
		if Fnmatch(name, p) || Fnmatch(lower, db.Lower(p)) {
			return true
		}
	}
	return false
}

// ExcludeDir matches a dir basename against exclude_dirs (case-sensitive).
func (r Rules) ExcludeDir(name string) bool {
	for _, p := range r.ExcludeDirs {
		if Fnmatch(name, p) {
			return true
		}
	}
	return false
}

// DirKey identifies a real directory (device, inode), for loop guards.
type DirKey struct{ dev, ino uint64 }

// KeyOf returns fi's DirKey; ok is false where the platform has no inodes.
func KeyOf(fi fs.FileInfo) (DirKey, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return DirKey{}, false
	}
	return DirKey{uint64(st.Dev), st.Ino}, true
}

// Walk yields every regular file under root that the rules include, top-down.
// A root that is a file is yielded on its own if included.
// Symlinks to files are always followed; symlinks to dirs only with FollowSymlinks,
// and each real dir is entered once, so symlink loops end.
// Problems are logged as warnings and the walk carries on.
func (r Rules) Walk(root string) iter.Seq[string] { return r.WalkDirs(root, nil) }

// WalkDirs is Walk that also passes every dir it enters to onDir (when not nil),
// root first and each dir before its files, so empty dirs are seen too.
func (r Rules) WalkDirs(root string, onDir func(dir string)) iter.Seq[string] {
	return func(yield func(string) bool) {
		fi, err := os.Stat(root)
		if err != nil {
			Logger.Warn("Path does not exist: " + root)
			return
		}
		if !fi.IsDir() {
			if r.IncludeFile(filepath.Base(root)) && fi.Mode().IsRegular() {
				yield(root)
			}
			return
		}
		visited := map[DirKey]bool{}
		if k, ok := KeyOf(fi); ok {
			visited[k] = true
		}
		r.walkDir(root, visited, onDir, yield)
	}
}

func (r Rules) walkDir(dir string, visited map[DirKey]bool, onDir func(string), yield func(string) bool) bool {
	if onDir != nil {
		onDir(dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		Logger.Warn(fmt.Sprintf("Error walking directory: %v", err))
		return true
	}
	var subdirs []string
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		isDir := e.IsDir()
		var target fs.FileInfo // for symlinks: what they point to
		if e.Type()&fs.ModeSymlink != 0 {
			if target, err = os.Stat(p); err == nil && target.IsDir() {
				isDir = true
			}
		}
		if isDir {
			if r.ExcludeDir(e.Name()) || (target != nil && !r.FollowSymlinks) {
				continue
			}
			subdirs = append(subdirs, p)
			continue
		}
		if !r.IncludeFile(e.Name()) {
			continue
		}
		fi, err := os.Stat(p)
		switch {
		case os.IsNotExist(err):
			Logger.Debug("File disappeared during scan: " + p)
		case err != nil:
			Logger.Warn(fmt.Sprintf("Error accessing file %s: %v", p, err))
		case !fi.Mode().IsRegular():
			Logger.Debug("Skipping non-regular file: " + p)
		default:
			if !yield(p) {
				return false
			}
		}
	}
	for _, sub := range subdirs {
		fi, err := os.Stat(sub)
		if err != nil {
			Logger.Warn(fmt.Sprintf("Error walking directory: %v", err))
			continue
		}
		if k, ok := KeyOf(fi); ok {
			if visited[k] {
				continue
			}
			visited[k] = true
		}
		if !r.walkDir(sub, visited, onDir, yield) {
			return false
		}
	}
	return true
}

// Logger receives the walk and hash warnings. It discards by default so a host
// program (a TUI) never gets text on its terminal; the CLI points it at its own
// handler.
var Logger = slog.New(slog.DiscardHandler)
