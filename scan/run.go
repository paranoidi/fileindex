package scan

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"slices"

	"github.com/paranoidi/fileindex/db"
)

// A batch is committed at whichever limit is reached first.
const (
	batchFiles       = 1000
	batchBytes int64 = 1 << 30
)

// Options tune Run.
type Options struct {
	Rules       Rules
	Workers     int  // hash workers; 0 = auto-scale
	MissingOnly bool // skip files indexed with the same size and mtime (the CLI's default; --all turns it off)
	BatchFiles  int  // files per commit; 0 = 1000. Smaller batches make a cancel land sooner.
}

// Reporter receives Run's progress. Worker, Speed and Scan are called from
// Run's goroutines concurrently, so implementations must be safe for concurrent use.
type Reporter interface {
	Worker(id int, path string)                      // hash worker id (1..N) started on path
	Speed(workers int, bps float64)                  // hashing speed and worker count, every tick
	Scan(dir string, scanned, newFiles, skipped int) // discovery progress; dir "" = done
	Hashed(res Stats)                                // hashing done and committed; pruning follows unless res.Interrupted
	Prune(dir string, checked int)                   // pruning progress
	Log(msg string)                                  // a removal made by pruning
}

// Stats is what Run did. Errors are per-file problems that didn't stop it.
type Stats struct {
	Processed, Added, Updated, Skipped, Errored int
	TotalSize                                   int64
	PrunedFiles, PrunedDirs, OrphanDirs         int64
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

// Run scans roots into d: it hashes new and changed files (committing batches
// in the background), then prunes records under each root that are gone from
// disk and deletes orphaned dir and file rows. Cancelling ctx stops it between
// files or pruned dirs; Run then returns with Interrupted set once the commit
// in flight has finished. It holds no package state and handles no signals.
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

	var discoverErr error
	paths := discover(ctx, d, opt.Rules, roots, opt.MissingOnly, r, &res, &discoverErr)
	if opt.MissingOnly {
		// Discover fully before hashing, so -m's reads don't interleave with batch writes.
		paths = slices.Values(slices.Collect(paths))
	}

	// A batch commits in the background while the next one is hashed; otherwise
	// every commit stalls all workers, which shows with many small files.
	type committed struct {
		batch []db.ScannedFile
		res   db.BatchResult
		err   error
	}
	var batch []db.ScannedFile
	var batchSize int64
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
		res.Added += c.res.Added
		res.Updated += c.res.Updated
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

	for h := range HashAll(ctx, paths, opt.Workers, r.Worker, r.Speed) {
		if h.Err != nil { // errors go to the list only, not the log too
			res.Errored++
			res.Errors = append(res.Errors, h.Err.Error())
			continue
		}
		dir, err := db.Resolve(filepath.Dir(h.Path)) // stored under the real path
		if err != nil {
			res.Errored++
			res.Errors = append(res.Errors, fmt.Sprintf("Error processing file info for %s: %v", h.Path, err))
			continue
		}
		batch = append(batch, db.ScannedFile{
			Dir: dir, Name: filepath.Base(h.Path), Size: h.Size, Mtime: h.Mtime,
			MD5: h.Hashes.MD5, SHA256: h.Hashes.SHA256, SHA256_8KB: h.Hashes.SHA256_8KB, NoExifJPEG: h.Hashes.NoExifJPEG,
		})
		res.Processed++
		res.TotalSize += h.Size
		batchSize += h.Size
		if len(batch) >= maxFiles || batchSize >= batchBytes {
			if err := commit(); err != nil {
				return res, err
			}
		}
	}
	res.Interrupted = ctx.Err() != nil
	if err := commit(); err != nil {
		return res, err
	}
	if err := wait(); err != nil {
		return res, err
	}
	if discoverErr != nil {
		return res, discoverErr
	}
	r.Hashed(res)
	// Checked after Hashed so a cancel landing during commit/wait isn't lost.
	if res.Interrupted || ctx.Err() != nil {
		res.Interrupted = true
		return res, nil
	}

	for _, root := range roots {
		id, found, err := db.DirByPath(d, root)
		if err != nil {
			return res, err
		}
		if !found {
			continue // a scanned file, not a dir
		}
		f, n, err := d.PruneDir(ctx, root, *id, r.Prune, r.Log)
		res.PrunedFiles += f
		res.PrunedDirs += n
		if ctx.Err() != nil {
			res.Interrupted = true
			break // PruneDir committed its work; cleaning up keeps the DB tidy
		}
		if err != nil {
			return res, err
		}
	}
	if res.OrphanDirs, err = d.DeleteOrphanedDirs(); err != nil {
		return res, err
	}
	_, err = d.DeleteOrphanedFiles()
	return res, err
}

// discover walks the roots, reporting every file. With missingOnly it drops
// files recorded at the same resolved dir+name with the same size and mtime,
// counting them as skipped.
func discover(ctx context.Context, d *db.DB, rules Rules, roots []string, missingOnly bool, r Reporter, res *Stats, errOut *error) iter.Seq[string] {
	return func(yield func(string) bool) {
		scanned, found := 0, 0
		// Walk yields a dir's files together, so the last dir's records are all we keep.
		lastDir, recorded := "", map[string]db.Recorded(nil)
		for _, root := range roots {
			for p := range rules.Walk(root) {
				// Missing-only collects discovery before HashAll runs, so a cancel must stop it here.
				if ctx.Err() != nil {
					return
				}
				scanned++
				dir := filepath.Dir(p)
				keep := true
				if missingOnly {
					if dir != lastDir {
						lastDir, recorded = dir, nil
						if real, err := db.Resolve(dir); err == nil {
							if recorded, err = d.DirFiles(real); err != nil {
								*errOut = err
								return
							}
						}
					}
					if rec, ok := recorded[filepath.Base(p)]; ok {
						if fi, err := os.Stat(p); err == nil {
							keep = !rec.Unchanged(fi.Size(), fi.ModTime())
						}
					}
					if keep {
						found++
					} else {
						res.Skipped++
					}
				}
				r.Scan(dir, scanned, found, res.Skipped)
				if keep && !yield(p) {
					return
				}
			}
		}
		r.Scan("", scanned, found, res.Skipped)
	}
}
