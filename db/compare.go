package db

import (
	"cmp"
	"database/sql"
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
	Size    int64
}

// FilesUnder lists the instances directly in dir id, or in its whole tree with
// recursive, sorted by path.
func (d *DB) FilesUnder(id int64, recursive bool) ([]Entry, error) {
	q := `SELECT i.dir_id, i.filename, COALESCE(f.sha256_noexif_jpeg, f.sha256), f.sha256, f.md5,
f.sha256_noexif_jpeg IS NOT NULL, f.filesize
FROM instance i JOIN file f ON i.file_id = f.id `
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
		if err := rows.Scan(&dir, &e.Path, &e.Content, &e.SHA256, &e.MD5, &e.IsJPEG, &e.Size); err != nil {
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
}

// Duplicates returns content groups with 2+ instances, biggest first. With a
// filter (a dir tree's ids), a group is kept if any instance is inside it, or
// with within, only if all are. Instances are read as rows, so names with
// "; " are safe.
func (d *DB) Duplicates(filter []int64, within bool) ([]DupGroup, error) {
	rows, err := d.Query(`WITH grp AS (
    SELECT COALESCE(f.sha256_noexif_jpeg, f.sha256) AS h
    FROM file f JOIN instance i ON f.id = i.file_id
    WHERE f.filesize > 0
    GROUP BY h HAVING COUNT(i.id) > 1
)
SELECT COALESCE(f.sha256_noexif_jpeg, f.sha256), f.filesize, i.dir_id, i.filename
FROM file f JOIN instance i ON f.id = i.file_id
WHERE COALESCE(f.sha256_noexif_jpeg, f.sha256) IN (SELECT h FROM grp)`)
	if err != nil {
		return nil, err
	}
	type inst struct {
		size int64
		dir  int64
		name string
	}
	byHash := map[string][]inst{}
	var order []string
	for rows.Next() {
		var h string
		var in inst
		if err := rows.Scan(&h, &in.size, &in.dir, &in.name); err != nil {
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
	MD5, SHA256 string
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
	err = d.QueryRow(`SELECT f.md5, f.sha256, f.filesize, COALESCE(CAST(i.mtime AS TEXT), '') FROM instance i JOIN file f ON i.file_id = f.id
WHERE i.dir_id = ? AND i.filename = ?`, *dirID, filepath.Base(path)).Scan(&r.MD5, &r.SHA256, &r.Size, &r.Mtime)
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
