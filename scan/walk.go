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
	"time"

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
func (r Rules) Walk(root string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for p := range r.WalkDirs(root, nil) {
			if !yield(p) {
				return
			}
		}
	}
}

// WalkDirs is Walk that also passes every dir it enters to onDir (when not nil),
// root first and each dir before its files, so empty dirs are seen too, and
// yields each file's stat (symlinks followed) so callers need not repeat it.
// A few dirs are read ahead in parallel; onDir and yield are still called from the
// caller's goroutine, in the order of a serial walk.
func (r Rules) WalkDirs(root string, onDir func(dir string)) iter.Seq2[string, fs.FileInfo] {
	return func(yield func(string, fs.FileInfo) bool) {
		fi, err := os.Stat(root)
		if err != nil {
			Logger.Warn("Path does not exist: " + root)
			return
		}
		if !fi.IsDir() {
			if r.IncludeFile(filepath.Base(root)) && fi.Mode().IsRegular() {
				yield(root, fi)
			}
			return
		}
		w := walker{r, onDir, yield, map[DirKey]bool{}, make(chan struct{}, readers), make(chan struct{})}
		defer close(w.done)
		w.dir(root, r.list(root))
	}
}

const (
	// readers is how many dirs a walk reads at once. On a SATA SSD 4 cut a no-op
	// rescan's walk by 60% cold and 45% warm, and more gained nothing: the
	// consumer's queries are the limit then.
	readers = 4
	// readAhead is how many subdirs of one dir are read before they are needed.
	readAhead = 2 * readers
	// statAhead caps the files of a dir stat'ed by its reader; the walk stats the
	// rest as it yields them, so a huge dir neither delays a cancel nor holds all
	// its stats.
	// ponytail: so a huge flat dir is stat'ed serially.
	statAhead = 1000
)

// maxListingAge is when a dir read ahead is read again: the siblings of a big
// subtree wait until all of it is walked. A var so tests can lower it.
var maxListingAge = 10 * time.Second

// listing is one dir as its reader found it. Readers only collect; the walk logs.
type listing struct {
	dir     fs.FileInfo
	statErr error // of the dir itself: it was not entered
	entries []fs.DirEntry
	readErr error
	ahead   []entry // the first entries, stat'ed
	at      time.Time
}

// entry is a dir entry judged by the rules; path is "" when they leave it out.
type entry struct {
	path  string
	isDir bool
	fi    fs.FileInfo // of a file
	err   error
}

// list reads dir and stats its first files. Safe for concurrent use.
func (r Rules) list(dir string) *listing {
	l := &listing{at: time.Now()}
	if l.dir, l.statErr = os.Stat(dir); l.statErr != nil {
		return l
	}
	if l.entries, l.readErr = os.ReadDir(dir); l.readErr != nil {
		return l
	}
	n := min(len(l.entries), statAhead)
	l.ahead = make([]entry, n)
	for i, e := range l.entries[:n] {
		l.ahead[i] = r.entry(dir, e)
	}
	l.at = time.Now()
	return l
}

func (r Rules) entry(dir string, e fs.DirEntry) entry {
	p := filepath.Join(dir, e.Name())
	isDir := e.IsDir()
	var target fs.FileInfo // for symlinks: what they point to
	if e.Type()&fs.ModeSymlink != 0 {
		var err error
		if target, err = os.Stat(p); err == nil && target.IsDir() {
			isDir = true
		}
	}
	if isDir {
		if r.ExcludeDir(e.Name()) || (target != nil && !r.FollowSymlinks) {
			return entry{}
		}
		return entry{path: p, isDir: true}
	}
	if !r.IncludeFile(e.Name()) {
		return entry{}
	}
	fi, err := os.Stat(p)
	return entry{path: p, fi: fi, err: err}
}

// walker is one walk. Its methods run on the walk's goroutine, except the reads
// readAhead starts.
type walker struct {
	r       Rules
	onDir   func(string)
	yield   func(string, fs.FileInfo) bool
	visited map[DirKey]bool
	slots   chan struct{} // one per dir being read ahead
	done    chan struct{} // closed when the walk ends, so waiting reads give up
}

// dir walks dir from its listing; false means the walk is over.
func (w *walker) dir(dir string, l *listing) bool {
	if l.statErr != nil {
		Logger.Warn(fmt.Sprintf("Error walking directory: %v", l.statErr))
		return true
	}
	if k, ok := KeyOf(l.dir); ok {
		if w.visited[k] {
			return true
		}
		w.visited[k] = true
	}
	if w.onDir != nil {
		w.onDir(dir)
	}
	if l.readErr != nil {
		Logger.Warn(fmt.Sprintf("Error walking directory: %v", l.readErr))
		return true
	}
	var subdirs []string
	for i, de := range l.entries {
		var e entry
		if i < len(l.ahead) {
			e = l.ahead[i]
		} else {
			e = w.r.entry(dir, de)
		}
		switch {
		case e.path == "":
		case e.isDir:
			subdirs = append(subdirs, e.path)
		case os.IsNotExist(e.err):
			Logger.Debug("File disappeared during scan: " + e.path)
		case e.err != nil:
			Logger.Warn(fmt.Sprintf("Error accessing file %s: %v", e.path, e.err))
		case !e.fi.Mode().IsRegular():
			Logger.Debug("Skipping non-regular file: " + e.path)
		default:
			if !w.yield(e.path, e.fi) {
				return false
			}
		}
	}
	l.entries, l.ahead = nil, nil // not needed while the subtree is walked
	var ahead []chan *listing     // of subdirs[i:], at most readAhead
	for i, sub := range subdirs {
		for len(ahead) < min(readAhead, len(subdirs)-i) {
			ahead = append(ahead, w.readAhead(subdirs[i+len(ahead)]))
		}
		sl := <-ahead[0]
		ahead = ahead[1:]
		if time.Since(sl.at) > maxListingAge {
			sl = w.r.list(sub)
		}
		if !w.dir(sub, sl) {
			return false
		}
	}
	return true
}

// readAhead lists dir in the background. The channel is buffered, so a listing
// nobody takes does not keep its reader around.
func (w *walker) readAhead(dir string) chan *listing {
	c := make(chan *listing, 1)
	go func() {
		select {
		case <-w.done:
			return
		case w.slots <- struct{}{}:
		}
		defer func() { <-w.slots }()
		c <- w.r.list(dir)
	}()
	return c
}

// Logger receives the walk and hash warnings. It discards by default so a host
// program (a TUI) never gets text on its terminal; the CLI points it at its own
// handler.
var Logger = slog.New(slog.DiscardHandler)
