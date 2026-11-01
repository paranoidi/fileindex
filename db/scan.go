package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// FormatMtime renders a file mtime as the index stores it:
// naive local time, microseconds, the fraction left out when it is zero.
// Microseconds come from float seconds, the fraction rounded half-to-even.
func FormatMtime(t time.Time) string {
	secs, frac := math.Modf(float64(t.Unix()) + float64(t.Nanosecond())*1e-9)
	us := int64(math.RoundToEven(frac * 1e6))
	if us >= 1e6 {
		secs, us = secs+1, us-1e6
	} else if us < 0 {
		secs, us = secs-1, us+1e6
	}
	local := time.Unix(int64(secs), 0).Local()
	if us != 0 {
		return local.Format("2006-01-02 15:04:05") + fmt.Sprintf(".%06d", us)
	}
	return local.Format("2006-01-02 15:04:05")
}

// SameMtime compares a stored mtime with a file's, allowing 1µs because
// FormatMtime rounds float seconds where time.Time truncates.
func SameMtime(stored string, t time.Time) bool {
	layout := "2006-01-02 15:04:05"
	if len(stored) > len(layout) {
		layout += ".000000"
	}
	s, err := time.ParseInLocation(layout, stored, time.Local)
	if err != nil {
		return false
	}
	d := s.Sub(t.Truncate(time.Microsecond))
	return d >= -time.Microsecond && d <= time.Microsecond
}

// ScannedFile is one hashed file ready to record. Dir is absolute and resolved.
type ScannedFile struct {
	Dir, Name               string
	Size                    int64
	Mtime                   time.Time
	MD5, SHA256, SHA256_8KB string
	NoExifJPEG              *string
}

func (f ScannedFile) Path() string { return filepath.Join(f.Dir, f.Name) }

// Recorded is the size and stored mtime ("" when NULL) of an indexed instance;
// Hashed is false while the instance waits to be hashed.
type Recorded struct {
	Size   int64
	Mtime  string
	Hashed bool
}

// Same reports whether the file still has the recorded size and mtime,
// whether or not it is hashed.
func (r Recorded) Same(size int64, mtime time.Time) bool {
	return r.Size == size && r.Mtime != "" && SameMtime(r.Mtime, mtime)
}

// Unchanged reports whether the instance is hashed and the file still has the
// recorded size and mtime; an unhashed instance always needs work.
func (r Recorded) Unchanged(size int64, mtime time.Time) bool {
	return r.Hashed && r.Same(size, mtime)
}

// DirFiles returns the instances recorded directly in dir, by filename; a dir
// not in the index gives an empty map. Scan's skip check loads each dir once.
func (d *DB) DirFiles(dir string) (map[string]Recorded, error) {
	out := map[string]Recorded{}
	dirID, found, err := DirByPath(d, dir)
	if err != nil || !found {
		return out, err
	}
	rows, err := d.Query(`SELECT filename, size, COALESCE(CAST(mtime AS TEXT), ''), file_id IS NOT NULL
FROM instance WHERE dir_id = ?`, *dirID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var r Recorded
		if err := rows.Scan(&name, &r.Size, &r.Mtime, &r.Hashed); err != nil {
			return nil, err
		}
		out[name] = r
	}
	return out, rows.Err()
}

// DiscoveredFile is a file seen by the walk, not hashed yet. Dir is absolute and resolved.
type DiscoveredFile struct {
	Dir, Name string
	Size      int64
	Mtime     time.Time
}

// CommitDiscovered records a batch of walked files as unhashed instances, and
// the dirs the walk entered (absolute, resolved, in walk order), in one
// transaction, so the files are findable by name at once. An existing instance
// gets the new size and mtime and loses its file_id: a stale hash never
// survives a change. One INSERT writes all new instances (see CommitScanBatch).
func (d *DB) CommitDiscovered(files []DiscoveredFile, dirs []string) (BatchResult, error) {
	res := BatchResult{Failed: map[int]error{}}
	tx, err := d.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	if err := recordDirs(tx, dirs); err != nil {
		return res, err
	}
	q := &stmtCache{tx: tx, stmts: map[string]*sql.Stmt{}}
	dirIDs := map[string]*int64{}
	var newRows [][]any
	added := map[newKey]int{}
	for _, f := range files {
		dirID, ok := dirIDs[f.Dir]
		if !ok {
			if dirID, err = GetOrCreatePath(q, f.Dir); err != nil {
				return res, err
			}
			dirIDs[f.Dir] = dirID
		}
		mtime := FormatMtime(f.Mtime)
		var instID int64
		err := q.QueryRow("SELECT id FROM instance WHERE dir_id IS ? AND filename = ?", dirID, f.Name).Scan(&instID)
		switch err {
		case nil:
			res.Updated++
			_, err = q.Exec("UPDATE instance SET file_id = NULL, size = ?, mtime = ?, last_seen = CURRENT_TIMESTAMP WHERE id = ?", f.Size, mtime, instID)
		case sql.ErrNoRows:
			k := newKey{dirID, f.Name}
			if j, dup := added[k]; dup {
				res.Updated++
				newRows[j] = []any{nil, dirID, f.Name, Lower(f.Name), f.Size, mtime}
				continue
			}
			res.Added++
			err = nil
			added[k] = len(newRows)
			newRows = append(newRows, []any{nil, dirID, f.Name, Lower(f.Name), f.Size, mtime})
		}
		if err != nil {
			return res, err
		}
	}
	if err := insertInstances(tx, newRows); err != nil {
		return res, err
	}
	return res, tx.Commit()
}

func insertInstances(tx *sql.Tx, newRows [][]any) error {
	if len(newRows) == 0 {
		return nil
	}
	rows, err := json.Marshal(newRows)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO instance (file_id, dir_id, filename, filename_lower, size, mtime)
SELECT value->>0, value->>1, value->>2, value->>3, value->>4, value->>5 FROM json_each(?)`, string(rows))
	return err
}

// UnhashedInstance is an instance waiting to be hashed.
type UnhashedInstance struct {
	ID   int64
	Path string
}

// UnhashedUnder returns up to limit unhashed instances with id > after, by id,
// in the dir tree at path or, when path is an indexed file, that file. Page by
// passing the last id back; the memory held is one page.
func (d *DB) UnhashedUnder(path string, after int64, limit int) ([]UnhashedInstance, error) {
	q, args, err := d.unhashedScope(path)
	if err != nil || q == "" {
		return nil, err
	}
	rows, err := d.Query(q+" AND i.id > ? ORDER BY i.id LIMIT ?",
		append(append([]any{}, args...), after, limit)...)
	if err != nil {
		return nil, err
	}
	var out []UnhashedInstance
	var dirs []int64
	var names []string
	for rows.Next() {
		var u UnhashedInstance
		var dir int64
		var name string
		if err := rows.Scan(&u.ID, &dir, &name); err != nil {
			rows.Close()
			return nil, err
		}
		out, dirs, names = append(out, u), append(dirs, dir), append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	paths := dirPaths{}
	for i := range out {
		if out[i].Path, err = paths.file(d, dirs[i], names[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CountUnhashed counts the instances UnhashedUnder would page through.
func (d *DB) CountUnhashed(path string) (int, error) {
	q, args, err := d.unhashedScope(path)
	if err != nil || q == "" {
		return 0, err
	}
	var n int
	err = d.QueryRow("SELECT COUNT(*) FROM ("+q+")", args...).Scan(&n)
	return n, err
}

// unhashedScope is the query (id, dir_id, filename) of unhashed instances at
// path; "" when path is not in the index.
func (d *DB) unhashedScope(path string) (string, []any, error) {
	const cols = "SELECT i.id, i.dir_id, i.filename FROM instance i WHERE i.file_id IS NULL"
	id, found, err := DirByPath(d, path)
	if err != nil {
		return "", nil, err
	}
	if found {
		return treeCTE + cols + " AND i.dir_id IN (SELECT id FROM dir_tree)", []any{*id}, nil
	}
	dirID, found, err := DirByPath(d, filepath.Dir(path))
	if err != nil || !found {
		return "", nil, err
	}
	return cols + " AND i.dir_id = ? AND i.filename = ?", []any{*dirID, filepath.Base(path)}, nil
}

// pruneBatch is how many deletions PruneDir commits at once. Tests lower it.
var pruneBatch int64 = 1000

// BatchResult reports what CommitScanBatch did. Failed holds per-file errors
// for files that were not recorded, by index into the batch.
type BatchResult struct {
	Added, Updated int
	Failed         map[int]error
}

// CommitScanBatch records a batch of hashed files in one transaction: each
// instance (created if missing) gets the file row, and the size and mtime the
// hashes were taken at, so a file that changed since the walk is stored as it
// was hashed. Files are
// matched by sha256; NULL hash columns of a matched row are backfilled.
// A file whose md5 is already recorded with a different sha256 is reported as a
// per-file failure instead of aborting the batch.
func (d *DB) CommitScanBatch(files []ScannedFile) (BatchResult, error) {
	res := BatchResult{Failed: map[int]error{}}
	tx, err := d.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	q := &stmtCache{tx: tx, stmts: map[string]*sql.Stmt{}}
	dirs := map[string]*int64{} // a batch is mostly a few dirs; resolve each once
	var newRows [][]any         // instances to insert, in one statement at the end
	added := map[newKey]int{}   // index into newRows
	for i, f := range files {
		var fileID int64
		err := q.QueryRow("SELECT id FROM file WHERE sha256 = ?", f.SHA256).Scan(&fileID)
		switch err {
		case nil:
			// A JPEG's fresh no-EXIF hash replaces the stored one, so scan -a repairs
			// stale values; a non-JPEG copy of the same content keeps it.
			ext := strings.ToLower(filepath.Ext(f.Name))
			_, err = q.Exec(`UPDATE file SET last_seen = CURRENT_TIMESTAMP, sha256_8kb = COALESCE(sha256_8kb, ?),
sha256_noexif_jpeg = CASE WHEN ? THEN ? ELSE sha256_noexif_jpeg END WHERE id = ?`,
				f.SHA256_8KB, ext == ".jpg" || ext == ".jpeg", f.NoExifJPEG, fileID)
		case sql.ErrNoRows:
			var other string
			if q.QueryRow("SELECT sha256 FROM file WHERE md5 = ?", f.MD5).Scan(&other) == nil {
				res.Failed[i] = fmt.Errorf("md5 collision: %s has the same md5 as an indexed file with a different sha256", f.Path())
				continue
			}
			var r sql.Result
			r, err = q.Exec("INSERT INTO file (md5, sha256, sha256_noexif_jpeg, sha256_8kb, filesize) VALUES (?, ?, ?, ?, ?)",
				f.MD5, f.SHA256, f.NoExifJPEG, f.SHA256_8KB, f.Size)
			if err == nil {
				fileID, err = r.LastInsertId()
			}
		}
		if err != nil {
			return res, err
		}

		dirID, ok := dirs[f.Dir]
		if !ok {
			if dirID, err = GetOrCreatePath(q, f.Dir); err != nil {
				return res, err
			}
			dirs[f.Dir] = dirID
		}
		mtime := FormatMtime(f.Mtime)
		var instID int64
		err = q.QueryRow("SELECT id FROM instance WHERE dir_id IS ? AND filename = ?", dirID, f.Name).Scan(&instID)
		switch err {
		case nil:
			res.Updated++
			_, err = q.Exec("UPDATE instance SET file_id = ?, size = ?, mtime = ?, last_seen = CURRENT_TIMESTAMP WHERE id = ?", fileID, f.Size, mtime, instID)
		case sql.ErrNoRows:
			k := newKey{dirID, f.Name}
			if j, dup := added[k]; dup { // same resolved path twice in one batch: the last one wins
				res.Updated++
				newRows[j] = []any{fileID, dirID, f.Name, Lower(f.Name), f.Size, mtime}
				continue
			}
			res.Added++
			err = nil
			added[k] = len(newRows)
			newRows = append(newRows, []any{fileID, dirID, f.Name, Lower(f.Name), f.Size, mtime})
		}
		if err != nil {
			return res, err
		}
	}
	// One statement for all new instances: FTS5 flushes its pending terms to
	// a new segment at every statement savepoint, so a per-row INSERT (its
	// triggers need one) wrote and merged an FTS segment per file.
	if err := insertInstances(tx, newRows); err != nil {
		return res, err
	}
	return res, tx.Commit()
}

// newKey identifies an instance CommitScanBatch is about to insert.
type newKey struct {
	dir  *int64
	name string
}

// DeleteInstancesInTree deletes every instance in the dir's subtree and returns the count.
func (d *DB) DeleteInstancesInTree(id int64) (int64, error) { return deleteInstancesInTree(d, id) }

func deleteInstancesInTree(q Querier, id int64) (int64, error) {
	ids, err := TreeIDs(q, &id, true)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	in, args := InList(ids)
	r, err := q.Exec("DELETE FROM instance WHERE dir_id IN ("+in+")", args...)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// deleteDirIfEmpty deletes the dir if it has no child dirs and no instances.
func deleteDirIfEmpty(q Querier, id int64) (bool, error) {
	r, err := q.Exec(`DELETE FROM dir WHERE id = ?
AND NOT EXISTS (SELECT 1 FROM dir WHERE parent_id = ?)
AND NOT EXISTS (SELECT 1 FROM instance WHERE dir_id = ?)`, id, id, id)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n > 0, nil
}

// missing reports whether p is gone from disk. Only "doesn't exist"-type errors
// count as missing; anything else (e.g. permission denied) is returned.
func missing(p string) (gone bool, isDir bool, err error) {
	fi, err := os.Stat(p)
	switch {
	case err == nil:
		return false, fi.IsDir(), nil
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.ELOOP):
		return true, false, nil
	}
	return false, false, err
}

// PruneDir deletes records under the dir at path (id) that are gone from disk,
// passing one line per removal to log. The start dir itself is kept; a path
// missing from disk loses everything under it. A dir still on disk is kept
// even when it holds nothing.
// onDir (may be nil) is called per dir visited with the files checked so far.
// Returns instances and dirs removed.
//
// Work runs in transactions, committed after any dir that brings the deletions
// since the last commit to pruneBatch: one fsync per batch instead of per row,
// and a killed process loses at most one batch, which a rerun redoes.
// ctx is checked between dirs; on cancel the work so far is committed and
// ctx.Err() returned with the counts. Orphaned file rows are left to the caller.
func (d *DB) PruneDir(ctx context.Context, path string, id int64, onDir func(dir string, checked int), log func(msg string)) (files, dirs int64, err error) {
	tx, err := d.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer func() { tx.Rollback() }() // the tx current at return; a no-op once committed
	pending := int64(0)              // deletions since the last commit
	flush := func() error {
		if pending < pruneBatch {
			return nil
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		next, err := d.Begin()
		if err != nil {
			return err
		}
		tx, pending = next, 0
		return nil
	}

	type entry struct {
		path string
		id   int64
	}
	stack := []entry{{path, id}}
	checked := 0
	for len(stack) > 0 {
		if ctx.Err() != nil {
			if err := tx.Commit(); err != nil {
				return files, dirs, err
			}
			return files, dirs, ctx.Err()
		}
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		names, err := queryStrings(tx, "SELECT filename FROM instance WHERE dir_id = ? ORDER BY filename", cur.id)
		if err != nil {
			return files, dirs, err
		}
		if checked += len(names); onDir != nil {
			onDir(cur.path, checked)
		}
		// One listing per dir instead of a stat per record; only names absent from
		// it get stat'd. A failed listing (nil map) stats everything, as before.
		var present map[string]fs.DirEntry
		if entries, err := os.ReadDir(cur.path); err == nil {
			present = make(map[string]fs.DirEntry, len(entries))
			for _, e := range entries {
				present[e.Name()] = e
			}
		}
		var goneNames []string
		for _, name := range names {
			if _, ok := present[name]; ok {
				continue
			}
			gone, _, err := missing(filepath.Join(cur.path, name))
			if err != nil {
				return files, dirs, err
			}
			if gone {
				goneNames = append(goneNames, name)
			}
		}
		if len(goneNames) > 0 {
			// One statement per dir: FTS5 flushes a segment at every statement
			// savepoint, and the delete trigger gave each per-row DELETE one.
			// The names were just listed in this tx, so each one deletes a row.
			list, err := json.Marshal(goneNames)
			if err != nil {
				return files, dirs, err
			}
			r, err := tx.Exec("DELETE FROM instance WHERE dir_id = ? AND filename IN (SELECT value FROM json_each(?))", cur.id, string(list))
			if err != nil {
				return files, dirs, err
			}
			n, _ := r.RowsAffected()
			files, pending = files+n, pending+n
			for _, name := range goneNames {
				log("Pruning removed file: " + filepath.Join(cur.path, name))
			}
		}

		rows, err := tx.Query("SELECT id, name FROM dir WHERE parent_id = ? ORDER BY name", cur.id)
		if err != nil {
			return files, dirs, err
		}
		var subs []entry
		for rows.Next() {
			var e entry
			if err := rows.Scan(&e.id, &e.path); err != nil {
				rows.Close()
				return files, dirs, err
			}
			e.path = filepath.Join(cur.path, e.path)
			subs = append(subs, e)
		}
		rows.Close()
		for _, s := range subs {
			var gone, isDir bool
			if e, ok := present[filepath.Base(s.path)]; ok && e.Type()&fs.ModeSymlink == 0 {
				isDir = e.IsDir()
			} else {
				gone, isDir, err = missing(s.path) // symlinks follow, as before
			}
			if err != nil {
				return files, dirs, err
			}
			if !gone && isDir {
				stack = append(stack, s)
				continue
			}
			f, n, err := deleteTree(tx, s.id)
			if err != nil {
				return files, dirs, err
			}
			files, dirs, pending = files+f, dirs+n, pending+f+n
			log("Pruning removed directory: " + s.path)
		}
		if err := flush(); err != nil {
			return files, dirs, err
		}
	}
	return files, dirs, tx.Commit()
}

func (d *DB) strings(query string, args ...any) ([]string, error) {
	return queryStrings(d, query, args...)
}

func queryStrings(q Querier, query string, args ...any) ([]string, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// stmtCache is a Querier over tx that prepares each query once; a batch runs
// the same few statements thousands of times and parsing them dominated (scan).
type stmtCache struct {
	tx    *sql.Tx
	stmts map[string]*sql.Stmt
}

// stmt returns the prepared query, or nil if preparing failed; callers then run
// the query on tx, which reports the error.
func (c *stmtCache) stmt(query string) *sql.Stmt {
	s, ok := c.stmts[query]
	if !ok {
		s, _ = c.tx.Prepare(query) // closed when tx ends
		c.stmts[query] = s
	}
	return s
}

func (c *stmtCache) Exec(query string, args ...any) (sql.Result, error) {
	if s := c.stmt(query); s != nil {
		return s.Exec(args...)
	}
	return c.tx.Exec(query, args...)
}

func (c *stmtCache) Query(query string, args ...any) (*sql.Rows, error) {
	if s := c.stmt(query); s != nil {
		return s.Query(args...)
	}
	return c.tx.Query(query, args...)
}

func (c *stmtCache) QueryRow(query string, args ...any) *sql.Row {
	if s := c.stmt(query); s != nil {
		return s.QueryRow(args...)
	}
	return c.tx.QueryRow(query, args...)
}

// Pruned counts what PrunePath removed.
type Pruned struct{ Files, Dirs int64 }

// PrunePath prunes the indexed dir at path (PruneDir) and then deletes orphaned
// file rows. On cancel the cleanup still runs, and ctx.Err() is returned
// with the counts so far.
func (d *DB) PrunePath(ctx context.Context, path string, onDir func(dir string, checked int), log func(msg string)) (Pruned, error) {
	id, found, err := DirByPath(d, path)
	if err != nil {
		return Pruned{}, err
	}
	if !found {
		return Pruned{}, fmt.Errorf("Path '%s' not found in database", path)
	}
	var p Pruned
	p.Files, p.Dirs, err = d.PruneDir(ctx, path, *id, onDir, log) // keeps path itself
	if err != nil && ctx.Err() == nil {
		return p, err
	}
	// orphaned file rows are cleaned but not counted: Files counts instances
	if _, err := d.DeleteOrphanedFiles(); err != nil {
		return p, err
	}
	return p, ctx.Err()
}

// RemovedTree counts what RemoveTree deleted.
type RemovedTree struct{ Instances, Files, Dirs int64 }

// RemoveTree deletes the indexed dir at path with everything under it, the
// file rows left orphaned, and the ancestors left empty (path scaffolding, as
// GetOrCreatePath made it). The disk is not looked at.
func (d *DB) RemoveTree(path string) (RemovedTree, error) {
	var r RemovedTree
	id, found, err := DirByPath(d, path)
	if err != nil {
		return r, err
	}
	if !found {
		return r, fmt.Errorf("Path '%s' not found in database", path)
	}
	tx, err := d.Begin()
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	var parent sql.NullInt64
	if err := tx.QueryRow("SELECT parent_id FROM dir WHERE id = ?", *id).Scan(&parent); err != nil {
		return r, err
	}
	if r.Instances, r.Dirs, err = deleteTree(tx, *id); err != nil {
		return r, err
	}
	for parent.Valid {
		cur := parent.Int64
		if err := tx.QueryRow("SELECT parent_id FROM dir WHERE id = ?", cur).Scan(&parent); err != nil {
			return r, err
		}
		ok, err := deleteDirIfEmpty(tx, cur)
		if err != nil {
			return r, err
		}
		if !ok {
			break
		}
		r.Dirs++
	}
	if err := tx.Commit(); err != nil {
		return r, err
	}
	r.Files, err = d.DeleteOrphanedFiles()
	return r, err
}

// RemoveInstance deletes the instance at the absolute file path and its file
// row if no other instance uses it. Its dir stays, even when left empty. Returns an
// error wrapping fs.ErrNotExist if the path is not indexed. The disk is not
// looked at.
func (d *DB) RemoveInstance(path string) (RemovedTree, error) {
	var r RemovedTree
	tx, err := d.Begin()
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	notFound := fmt.Errorf("Path '%s' not found in database: %w", path, fs.ErrNotExist)
	dirID, found, err := DirByPath(tx, filepath.Dir(path))
	if err != nil {
		return r, err
	}
	if !found {
		return r, notFound
	}
	var id int64
	var fileID sql.NullInt64
	err = tx.QueryRow("SELECT id, file_id FROM instance WHERE dir_id = ? AND filename = ?", *dirID, filepath.Base(path)).Scan(&id, &fileID)
	if err == sql.ErrNoRows {
		return r, notFound
	}
	if err != nil {
		return r, err
	}
	if _, err := tx.Exec("DELETE FROM instance WHERE id = ?", id); err != nil {
		return r, err
	}
	r.Instances = 1
	if !fileID.Valid {
		return r, tx.Commit()
	}
	res, err := tx.Exec("DELETE FROM file WHERE id = ? AND NOT EXISTS (SELECT 1 FROM instance WHERE file_id = ?)", fileID.Int64, fileID.Int64)
	if err != nil {
		return r, err
	}
	r.Files, _ = res.RowsAffected()
	return r, tx.Commit()
}
