package db

import (
	"cmp"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"slices"
)

// Entry is one instance under a dir, for diff and filesystem checks.
type Entry struct {
	Path    string // full path
	Content string // sha256_noexif_jpeg if set, else sha256
	SHA256  string
	MD5     string
	IsJPEG  bool // has a sha256_noexif_jpeg
	Hashed  bool // false while the instance waits to be hashed; the hash fields are then empty
	Size    int64
	Mtime   string // as stored, "" when NULL; Recorded{Size, Mtime}.Unchanged compares it with a file
}

// FilesUnder lists the instances directly in dir id, or in its whole tree with
// recursive, sorted by path.
func (d *DB) FilesUnder(id int64, recursive bool) ([]Entry, error) {
	q := `SELECT i.dir_id, i.filename, COALESCE(f.sha256_noexif_jpeg, f.sha256, ''), COALESCE(f.sha256, ''), COALESCE(f.md5, ''),
COALESCE(f.sha256_noexif_jpeg IS NOT NULL, 0), i.size, COALESCE(CAST(i.mtime AS TEXT), ''), i.file_id IS NOT NULL
FROM instance i LEFT JOIN file f ON i.file_id = f.id `
	if recursive {
		q = treeCTE + q + "WHERE i.dir_id IN (SELECT id FROM dir_tree)"
	} else {
		q += "WHERE i.dir_id = ?"
	}
	rows, err := d.Query(q, id)
	if err != nil {
		return nil, err
	}
	var out []Entry
	var dirs []int64
	for rows.Next() {
		var e Entry
		var dir int64
		if err := rows.Scan(&dir, &e.Path, &e.Content, &e.SHA256, &e.MD5, &e.IsJPEG, &e.Size, &e.Mtime, &e.Hashed); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, e)
		dirs = append(dirs, dir)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	paths := dirPaths{}
	for i := range out {
		if out[i].Path, err = paths.file(d, dirs[i], out[i].Path); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(out, func(a, b Entry) int { return cmp.Compare(a.Path, b.Path) })
	return out, nil
}

// DupGroup is a set of instances with the same content (metadata-free hash for
// JPEGs, else sha256).
type DupGroup struct {
	Size      int64 // smallest instance size
	Wasted    int64 // total size minus the largest copy
	Locations []string
	// Unverified are the Locations that are only fast-hashed (PotentialDuplicates).
	Unverified []string
}

// Duplicates ignores unhashed instances. It returns content groups with 2+ instances, biggest first. With a
// filter (a dir tree's ids), a group is kept if any instance is inside it, or
// with within, only if all are. Instances are read as rows, so names with
// "; " are safe.
func (d *DB) Duplicates(filter []int64, within bool) ([]DupGroup, error) {
	return d.dupGroups(`WITH grp AS (
    SELECT COALESCE(f.sha256_noexif_jpeg, f.sha256) AS h
    FROM file f JOIN instance i ON f.id = i.file_id
    WHERE f.filesize > 0
    GROUP BY h HAVING COUNT(i.id) > 1
)
SELECT COALESCE(f.sha256_noexif_jpeg, f.sha256), f.filesize, i.dir_id, i.filename, 0
FROM file f JOIN instance i ON f.id = i.file_id
WHERE COALESCE(f.sha256_noexif_jpeg, f.sha256) IN (SELECT h FROM grp)`, filter, within)
}

// PotentialDuplicates returns the sets of 2+ instances with the same size and
// first 8 KB where at least one is only fast-hashed, biggest first; filter and
// within work as in Duplicates. A set with no fast-hashed member is left out:
// its full hashes already say whether it is a duplicate.
func (d *DB) PotentialDuplicates(filter []int64, within bool) ([]DupGroup, error) {
	// Starts from the fast-hashed rows and looks hashed ones up by their keys (CROSS
	// JOIN fixes that order), so its cost follows the number of fast-hashed files, not
	// the size of the index. INDEXED BY because the planner otherwise picks
	// idx_instance_file_id and reads every unhashed row, 700x slower on 500k of them.
	return d.dupGroups(`WITH fast AS (
    SELECT size, sha256_8kb AS head, dir_id, filename
    FROM instance INDEXED BY idx_instance_fast WHERE file_id IS NULL AND sha256_8kb IS NOT NULL
), inst AS (
    SELECT size, head, dir_id, filename, 1 AS fast FROM fast
    UNION ALL
    SELECT i.size, k.head, i.dir_id, i.filename, 0
    FROM (SELECT DISTINCT size, head FROM fast) k
    CROSS JOIN file f ON f.sha256_8kb = k.head
    CROSS JOIN instance i ON i.file_id = f.id AND i.size = k.size
)
SELECT size || ':' || head, size, dir_id, filename, fast
FROM (SELECT *, COUNT(*) OVER (PARTITION BY size, head) AS n FROM inst)
WHERE n > 1`, filter, within)
}

// CountNotFullyHashed counts the instances of at least minSize without a full
// hash, inside filter's dirs (nil = everywhere): fast-hashed ones, and those
// with no hash at all.
func (d *DB) CountNotFullyHashed(filter []int64, minSize int64) (fast, none int, err error) {
	q, args := "SELECT COUNT(*), COUNT(sha256_8kb) FROM instance WHERE file_id IS NULL AND size >= ?", []any{minSize}
	if filter != nil {
		ids, err := json.Marshal(filter)
		if err != nil {
			return 0, 0, err
		}
		q, args = q+" AND dir_id IN (SELECT value FROM json_each(?))", append(args, string(ids))
	}
	var total int
	err = d.QueryRow(q, args...).Scan(&total, &fast)
	return fast, total - fast, err
}

// dupGroups groups query's rows (key, size, dir_id, filename, fast) by key,
// applies filter and within, and sorts the groups biggest first.
func (d *DB) dupGroups(query string, filter []int64, within bool) ([]DupGroup, error) {
	rows, err := d.Query(query)
	if err != nil {
		return nil, err
	}
	type inst struct {
		size int64
		dir  int64
		name string
		fast bool
	}
	byHash := map[string][]inst{}
	var order []string
	for rows.Next() {
		var h string
		var in inst
		if err := rows.Scan(&h, &in.size, &in.dir, &in.name, &in.fast); err != nil {
			rows.Close()
			return nil, err
		}
		if _, ok := byHash[h]; !ok {
			order = append(order, h)
		}
		byHash[h] = append(byHash[h], in)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	inFilter := map[int64]bool{}
	for _, id := range filter {
		inFilter[id] = true
	}
	type keyed struct {
		hash string
		g    DupGroup
	}
	var groups []keyed
	paths := dirPaths{}
	for _, h := range order {
		insts := byHash[h]
		if filter != nil {
			n := 0
			for _, in := range insts {
				if inFilter[in.dir] {
					n++
				}
			}
			if n == 0 || (within && n < len(insts)) {
				continue
			}
		}
		g := DupGroup{Size: insts[0].size}
		var total, largest int64
		for _, in := range insts {
			g.Size = min(g.Size, in.size)
			largest = max(largest, in.size)
			total += in.size
			p, err := paths.file(d, in.dir, in.name)
			if err != nil {
				return nil, err
			}
			g.Locations = append(g.Locations, p)
			if in.fast {
				g.Unverified = append(g.Unverified, p)
			}
		}
		g.Wasted = total - largest
		slices.Sort(g.Locations)
		groups = append(groups, keyed{h, g})
	}
	slices.SortFunc(groups, func(a, b keyed) int {
		return cmp.Or(cmp.Compare(b.g.Size, a.g.Size), cmp.Compare(a.hash, b.hash))
	})
	out := make([]DupGroup, len(groups))
	for i, k := range groups {
		out[i] = k.g
	}
	return out, nil
}

// FileRecord is what the index holds for one instance.
type FileRecord struct {
	MD5, SHA256 string // empty while Hashed is false
	Hashed      bool
	Mtime       string // as stored, empty if unset
	Size        int64
}

// FileInfo looks up the instance at an absolute file path.
func (d *DB) FileInfo(path string) (FileRecord, bool, error) {
	var r FileRecord
	dirID, found, err := DirByPath(d, filepath.Dir(path))
	if err != nil || !found {
		return r, false, err
	}
	err = d.QueryRow(`SELECT COALESCE(f.md5, ''), COALESCE(f.sha256, ''), i.file_id IS NOT NULL, i.size, COALESCE(CAST(i.mtime AS TEXT), '')
FROM instance i LEFT JOIN file f ON i.file_id = f.id
WHERE i.dir_id = ? AND i.filename = ?`, *dirID, filepath.Base(path)).Scan(&r.MD5, &r.SHA256, &r.Hashed, &r.Size, &r.Mtime)
	if err == sql.ErrNoRows {
		return r, false, nil
	}
	return r, err == nil, err
}

// DirTags returns a dir's tag names, sorted.
func (d *DB) DirTags(id int64) ([]string, error) {
	return d.strings(`SELECT t.name FROM tag t JOIN dir_tag dt ON t.id = dt.tag_id
WHERE dt.dir_id = ? ORDER BY t.name`, id)
}
