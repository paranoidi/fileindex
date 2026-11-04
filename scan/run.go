package scan

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/paranoidi/fileindex/db"
)

// A batch is committed at whichever limit is reached first (Options.BatchInterval
// replaces the file count).
const (
	batchFiles       = 1000
	batchBytes int64 = 1 << 30
)

// Options tune Run.
type Options struct {
	Rules       Rules
	Workers     int  // hash workers; 0 = auto-scale
	MissingOnly bool // skip files indexed and hashed with the same size and mtime (the CLI's default; --all turns it off)
	SkipHash    bool // index and prune only: new and changed files stay unhashed, findable by name, until a later run hashes them
	Fast        bool // hash only the first 8 KB of files larger than that; they stay unhashed until a later run without Fast
	BatchFiles  int  // files per commit; 0 = 1000. Smaller batches make a cancel land sooner.
	// BatchInterval, when > 0, commits a batch once this long has passed since its
	// first file instead of after BatchFiles files. Every commit flushes the
	// full-text index, so with many small files fewer, larger commits scan faster.
	BatchInterval time.Duration
}

// Reporter receives Run's progress. Worker, Speed and Scan are called from
// Run's goroutines concurrently, so implementations must be safe for concurrent use.
type Reporter interface {
	Worker(id int, path string)                      // hash worker id (1..N) started on path
	Speed(workers int, bps float64)                  // hashing speed and worker count, every tick
	Scan(dir string, scanned, newFiles, skipped int) // discovery progress; dir "" = done
	Hashed(res Stats)                                // last call: hashing done and committed, or skipped or cut short (res.Interrupted)
	Prune(dir string, checked int)                   // pruning progress
	Log(msg string)                                  // a removal made by pruning
}

// FileReporter is an optional extension of Reporter: FileDone is called, from
// Run's hashing loop, once per file whose hashing finished or failed.
type FileReporter interface{ FileDone() }

// PhaseReporter is an optional extension of Reporter: a Reporter that also
// implements it can tell Run's three stages apart. The calls, in order:
// Scan and Indexed while walking, Discovered, Prune and Log while pruning,
// Pruned, Worker and Speed while hashing, then Hashed.
type PhaseReporter interface {
	// Indexed is called after each discovery commit with the number of files
	// committed so far in this run. They are findable by name from then on,
	// still unhashed.
	Indexed(committed int)
	// Discovered is called once the walk and its commits are done (also when
	// cancelled). Pruning follows unless res.Interrupted.
	Discovered(res Stats)
	// Pruned is called once pruning is done and not interrupted. toHash is the
	// number of unhashed instances under the roots after pruning, including ones
	// left by earlier runs: the total for hashing progress (an upper bound when
	// roots nest). Hashing follows unless Options.SkipHash.
	Pruned(res Stats, toHash int)
}

// Stats is what Run did. Errors are per-file problems that didn't stop it.
// Added and Updated count instances written by discovery, Processed and
// TotalSize the files hashed.
type Stats struct {
	Processed, Added, Updated, Skipped, Errored int
	Discovered                                  int // files committed by discovery (Added + Updated)
	TotalSize                                   int64
	PrunedFiles, PrunedDirs                     int64
	Errors                                      []string
	Interrupted                                 bool // ctx was cancelled; the DB holds every committed batch and no half-applied one
}

// ResolveRoots resolves scan arguments to real absolute paths that exist.
func ResolveRoots(args []string) ([]string, error) {
	var roots []string
	for _, arg := range args {
		p, err := db.Resolve(arg)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("Path does not exist: %s", p)
		}
		roots = append(roots, p)
	}
	return roots, nil
}

// Run scans roots into d in two phases. Phase 1 walks the roots and commits
// new and changed files as unhashed instances (size, mtime, no file_id) in
// batches while it walks, so they are findable by name at once; dirs are
// recorded in the same batches (empty ones too). Then it prunes records under
// each root that are gone from disk and deletes orphaned file rows, so stale
// names leave the index before any hashing. Phase 2 hashes every unhashed
// instance under the roots, including ones left by an interrupted or SkipHash
// run, and commits batches in the background. SkipHash stops after pruning.
// Fast stores only a first-8-KB hash on the instance, skipping instances that
// have one; an instance with a full hash is never touched.
// A cancel in any stage skips the later ones.
//
// A file that changes between the walk and hashing is stored with the size and
// mtime it was hashed at, so hashes always match the instance. A file that
// cannot be hashed stays unhashed and is retried by the next run.
//
// Memory is bounded whatever the tree size: the walk streams into one batch
// (plus one dir's records for the skip check) and hashing pages through
// unhashed instances by id, one page at a time; nothing collects the file list.
//
// Cancelling ctx stops it between files or pruned dirs; Run then returns with
// Interrupted set once the commit in flight has finished. It holds no package
// state and handles no signals.
func Run(ctx context.Context, d *db.DB, args []string, opt Options, r Reporter) (Stats, error) {
	var res Stats
	if opt.Workers < 0 {
		return res, fmt.Errorf("Number of processes must be at least 1")
	}
	roots, err := ResolveRoots(args)
	if err != nil {
		return res, err
	}
	if err := d.RequireSchema(); err != nil { // fail before hashing anything
		return res, err
	}
	// drop the no-EXIF hash of bare "FF D8" stored by older versions.
	// ponytail: runs every scan (indexed, cheap); drop once old DBs are rescanned.
	if _, err := d.Exec(`UPDATE file SET sha256_noexif_jpeg = NULL WHERE sha256_noexif_jpeg = '71563ad80061407ede9c6f316836284bd3710a520c5a792b5eda1cb703690815'`); err != nil {
		return res, err
	}
	maxFiles := opt.BatchFiles
	if maxFiles <= 0 {
		maxFiles = batchFiles
	}

	scopes, err := discover(ctx, d, opt, maxFiles, roots, r, &res)
	if err != nil {
		return res, err
	}
	res.Interrupted = ctx.Err() != nil
	pr, _ := r.(PhaseReporter)
	if pr != nil {
		pr.Discovered(res)
	}
	if !res.Interrupted {
		if err := pruneRoots(ctx, d, roots, r, &res); err != nil {
			return res, err
		}
	}
	if !res.Interrupted {
		toHash := 0 // counted after pruning, so pruned rows are not in it
		for _, sc := range scopes {
			n, err := d.CountUnhashed(sc, opt.Fast)
			if err != nil {
				return res, err
			}
			toHash += n
		}
		if pr != nil {
			pr.Pruned(res, toHash)
		}
		if !opt.SkipHash {
			if err := hashPending(ctx, d, opt, maxFiles, scopes, r, &res); err != nil {
				return res, err
			}
			// hashing re-attaches or replaces file rows: drop what lost its last instance
			if _, err := d.DeleteOrphanedFiles(); err != nil {
				return res, err
			}
		}
	}
	r.Hashed(res)
	return res, nil
}

// pruneRoots prunes the records under each root that are gone from disk, then
// deletes orphaned file rows (also after a cancel, which keeps the DB tidy).
func pruneRoots(ctx context.Context, d *db.DB, roots []string, r Reporter, res *Stats) error {
	for _, root := range roots {
		id, found, err := db.DirByPath(d, root)
		if err != nil {
			return err
		}
		if !found {
			continue // a scanned file, not a dir
		}
		f, n, err := d.PruneDir(ctx, root, *id, r.Prune, r.Log)
		res.PrunedFiles += f
		res.PrunedDirs += n
		if ctx.Err() != nil {
			res.Interrupted = true
			break // PruneDir committed its work
		}
		if err != nil {
			return err
		}
	}
	_, err := d.DeleteOrphanedFiles()
	return err
}

// discover is phase 1. It walks the roots and commits each batch (synchronously,
// so the skip check's reads never interleave with a background write) of
// files that are new or differ from their record, plus the dirs entered.
// With MissingOnly, a file recorded with the same size and mtime is skipped;
// an unhashed one is left as is for phase 2. RootDone fires for a root once its
// files are committed. It returns the paths phase 2 selects unhashed instances
// under: the roots, and dirs reached through symlinks outside them, where the
// index stores their files.
func discover(ctx context.Context, d *db.DB, opt Options, maxFiles int, roots []string, r Reporter, res *Stats) ([]string, error) {
	rr, _ := r.(RootReporter)
	pr, _ := r.(PhaseReporter)
	isOwn := ownFiles(d.Path)
	scopes := slices.Clone(roots)
	var batch []db.DiscoveredFile
	var dirs []string
	var batchStart time.Time
	var werr error
	committed, scanned, found := 0, 0, 0

	flush := func() error {
		if len(batch) == 0 && len(dirs) == 0 {
			return nil
		}
		br, err := d.CommitDiscovered(batch, dirs)
		if err != nil {
			return err
		}
		res.Added += br.Added
		res.Updated += br.Updated
		committed += len(batch)
		res.Discovered = committed
		batch, dirs = batch[:0], dirs[:0]
		if pr != nil {
			pr.Indexed(committed)
		}
		return nil
	}
	// added is called after an item joined the batch.
	added := func() {
		if len(batch)+len(dirs) == 1 {
			batchStart = time.Now()
		}
		full := len(batch)+len(dirs) >= maxFiles
		if opt.BatchInterval > 0 {
			full = time.Since(batchStart) >= opt.BatchInterval
		}
		if full && werr == nil {
			werr = flush()
		}
	}
	onDir := func(dir string) {
		real, err := db.Resolve(dir)
		if werr != nil || err != nil {
			return
		}
		dirs = append(dirs, real)
		added()
		if !slices.ContainsFunc(scopes, func(s string) bool { return within(s, real) }) {
			scopes = append(scopes, real) // reached through a symlink
		}
	}

	// Walk yields a dir's files together, so the last dir's records are all we keep.
	lastDir, recorded := "", map[string]db.Recorded(nil)
walk:
	for i, root := range roots {
		for p := range opt.Rules.WalkDirs(root, onDir) {
			if ctx.Err() != nil || werr != nil {
				break walk
			}
			scanned++
			dir := filepath.Dir(p)
			if isOwn(p) {
				Logger.Debug("Skipping the index database: " + p)
				res.Skipped++
				continue
			}
			fi, err := os.Stat(p)
			if err == nil && !fi.Mode().IsRegular() {
				err = fmt.Errorf("Path is not a regular file: %s", p)
			}
			realDir := ""
			if err == nil {
				realDir, err = db.Resolve(dir) // stored under the real path
			}
			if err != nil {
				res.Errored++
				res.Errors = append(res.Errors, fmt.Sprintf("Error processing file info for %s: %v", p, err))
				continue
			}
			write := true
			if opt.MissingOnly {
				if realDir != lastDir {
					lastDir = realDir
					if recorded, werr = d.DirFiles(realDir); werr != nil {
						break walk
					}
				}
				if rec, ok := recorded[filepath.Base(p)]; ok && rec.Same(fi.Size(), fi.ModTime()) {
					write = false // unchanged: nothing to write
					if rec.Hashed || (opt.Fast && rec.Fast) {
						res.Skipped++
					} else {
						found++ // waits for phase 2
					}
				} else {
					found++
				}
			} else {
				found++
			}
			r.Scan(dir, scanned, found, res.Skipped)
			if write {
				batch = append(batch, db.DiscoveredFile{Dir: realDir, Name: filepath.Base(p), Size: fi.Size(), Mtime: fi.ModTime()})
				added()
			}
		}
		if werr != nil || ctx.Err() != nil {
			break
		}
		if err := flush(); err != nil {
			werr = err
			break
		}
		if rr != nil {
			rr.RootDone(i)
		}
	}
	// a cancelled walk still keeps what it found
	if err := flush(); err != nil && werr == nil {
		werr = err
	}
	r.Scan("", scanned, found, res.Skipped)
	return scopes, werr
}

// within reports whether p is dir or inside it.
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// hashPage is how many unhashed instances phase 2 loads at a time.
const hashPage = 1000

// hashPending is phase 2: it hashes the unhashed instances under scopes, paging
// through them by id.
func hashPending(ctx context.Context, d *db.DB, opt Options, maxFiles int, scopes []string, r Reporter, res *Stats) error {
	var pageErr error
	paths := func(yield func(string) bool) {
		for _, sc := range scopes {
			for after := int64(0); ; {
				page, err := d.UnhashedUnder(sc, after, hashPage, opt.Fast)
				if err != nil {
					pageErr = err
					return
				}
				if len(page) == 0 {
					break
				}
				for _, u := range page {
					after = u.ID
					if ctx.Err() != nil || !yield(u.Path) {
						return
					}
				}
			}
		}
	}
	var onDone func()
	if fr, ok := r.(FileReporter); ok {
		onDone = fr.FileDone
	}
	if err := hashPaths(ctx, d, opt, maxFiles, paths, r.Worker, r.Speed, onDone, res); err != nil {
		return err
	}
	return pageErr
}

// HashPaths fully hashes the given indexed files and records them. Per-file
// problems and a cancel land in the returned Stats, as in Run.
func HashPaths(ctx context.Context, d *db.DB, paths []string, workers int, onStart func(int, string), onTick func(int, float64), onDone func()) (Stats, error) {
	var res Stats
	err := hashPaths(ctx, d, Options{Workers: workers}, batchFiles, slices.Values(paths), onStart, onTick, onDone, &res)
	return res, err
}

// hashPaths hashes paths and commits batches in the background while the next
// one is hashed; otherwise every commit stalls all workers, which shows with
// many small files.
func hashPaths(ctx context.Context, d *db.DB, opt Options, maxFiles int, paths iter.Seq[string], onStart func(int, string), onTick func(int, float64), onDone func(), res *Stats) error {
	type committed struct {
		batch []db.ScannedFile
		res   db.BatchResult
		err   error
	}
	var batch []db.ScannedFile
	var batchSize int64
	var batchStart time.Time    // when the batch being filled got its first file
	var inflight chan committed // nil when no commit is running
	wait := func() error {
		if inflight == nil {
			return nil
		}
		c := <-inflight
		inflight = nil
		if c.err != nil {
			return c.err
		}
		for i, ferr := range c.res.Failed {
			res.Processed--
			res.TotalSize -= c.batch[i].Size
			res.Errored++
			res.Errors = append(res.Errors, ferr.Error())
		}
		return nil
	}
	commit := func() error {
		if err := wait(); err != nil || len(batch) == 0 {
			return err
		}
		b, ch := batch, make(chan committed, 1)
		go func() {
			res, err := d.CommitScanBatch(b)
			ch <- committed{b, res, err}
		}()
		inflight, batch, batchSize = ch, nil, 0
		return nil
	}
	// an error return must not leave a commit running against d
	defer func() { _ = wait() }()

	for h := range HashAll(ctx, paths, opt.Workers, opt.Fast, onStart, onTick) {
		if onDone != nil {
			onDone()
		}
		if h.Err != nil { // errors go to the list only, not the log too
			res.Errored++
			res.Errors = append(res.Errors, h.Err.Error())
			continue
		}
		if len(batch) == 0 {
			batchStart = time.Now()
		}
		batch = append(batch, db.ScannedFile{
			Dir: filepath.Dir(h.Path), Name: filepath.Base(h.Path), Size: h.Size, Mtime: h.Mtime,
			MD5: h.Hashes.MD5, SHA256: h.Hashes.SHA256, SHA256_8KB: h.Hashes.SHA256_8KB, NoExifJPEG: h.Hashes.NoExifJPEG,
		})
		read := h.Size
		if h.Hashes.SHA256 == "" { // fast: only the head was read
			read = min(read, headSize)
		}
		res.Processed++
		res.TotalSize += read
		batchSize += read
		full := len(batch) >= maxFiles
		if opt.BatchInterval > 0 {
			full = time.Since(batchStart) >= opt.BatchInterval
		}
		if full || batchSize >= batchBytes {
			if err := commit(); err != nil {
				return err
			}
		}
	}
	res.Interrupted = ctx.Err() != nil
	if err := commit(); err != nil {
		return err
	}
	return wait()
}

// ownFiles reports whether a walked path is the database at dbPath or one of
// its -wal/-shm/-journal files. A scan must never open them: closing any fd on
// a file drops every POSIX lock this process holds on it, SQLite's included, and
// the next connection then truncates the -shm file under the others' mappings
// (SQLITE_IOERR_IN_PAGE). Names are compared first; only a match is stat'ed.
func ownFiles(dbPath string) func(p string) bool {
	if dbPath == "" {
		return func(string) bool { return false }
	}
	base := filepath.Base(dbPath)
	names := map[string]bool{base: true, base + "-wal": true, base + "-shm": true, base + "-journal": true}
	dbDir, err := os.Stat(filepath.Dir(dbPath))
	if err != nil {
		return func(string) bool { return false }
	}
	return func(p string) bool {
		if !names[filepath.Base(p)] {
			return false
		}
		fi, err := os.Stat(filepath.Dir(p))
		return err == nil && os.SameFile(fi, dbDir)
	}
}
