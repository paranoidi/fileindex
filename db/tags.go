package db

import (
	"cmp"
	"database/sql"
	"fmt"
	"slices"
)

func getOrCreateTag(tx *sql.Tx, name string) (int64, error) {
	var id int64
	err := tx.QueryRow("SELECT id FROM tag WHERE name = ?", name).Scan(&id)
	if err == sql.ErrNoRows {
		r, err := tx.Exec("INSERT INTO tag (name) VALUES (?)", name)
		if err != nil {
			return 0, err
		}
		return r.LastInsertId()
	}
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec("UPDATE tag SET last_seen = CURRENT_TIMESTAMP WHERE id = ?", id)
	return id, err
}

// tagIDs looks up existing tags; unknown names are left out.
func tagIDs(q Querier, names []string) ([]int64, []string, error) {
	var ids []int64
	var found []string
	for _, n := range names {
		var id int64
		err := q.QueryRow("SELECT id FROM tag WHERE name = ?", n).Scan(&id)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		ids, found = append(ids, id), append(found, n)
	}
	return ids, found, nil
}

func (d *DB) ids(q string, args ...any) ([]int64, error) {
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// TagTarget is what a tag operation applies to: instances and dirs.
type TagTarget struct {
	Instances, Dirs []int64
}

// TreeTarget is a dir, its descendant dirs and every instance in them.
func (d *DB) TreeTarget(dir int64) (TagTarget, error) {
	dirs, err := TreeIDs(d, &dir, true)
	if err != nil {
		return TagTarget{}, err
	}
	insts, err := d.ids(treeCTE+"SELECT id FROM instance WHERE dir_id IN (SELECT id FROM dir_tree)", dir)
	return TagTarget{Instances: insts, Dirs: dirs}, err
}

// AddTags tags every target with every name, creating tags as needed, and
// returns how many new associations were made.
func (d *DB) AddTags(t TagTarget, names []string) (int, error) {
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var tags []int64
	for _, n := range names {
		id, err := getOrCreateTag(tx, n)
		if err != nil {
			return 0, err
		}
		tags = append(tags, id)
	}
	added := 0
	for _, pass := range []struct {
		table, col string
		ids        []int64
	}{{"dir_tag", "dir_id", t.Dirs}, {"file_tag", "instance_id", t.Instances}} {
		for _, id := range pass.ids {
			for _, tag := range tags {
				r, err := tx.Exec("INSERT OR IGNORE INTO "+pass.table+" (tag_id, "+pass.col+") VALUES (?, ?)", tag, id)
				if err != nil {
					return 0, err
				}
				n, _ := r.RowsAffected()
				added += int(n)
			}
		}
	}
	return added, tx.Commit()
}

// Removed is one association removed by RemoveTags.
type Removed struct {
	Tag, Path string
	IsDir     bool
}

// RemoveTags removes the named tags from every target. Unknown tag names are
// skipped. Results list dirs then files, each sorted by path, tags
// in argument order.
func (d *DB) RemoveTags(t TagTarget, names []string) ([]Removed, error) {
	tags, found, err := tagIDs(d, names)
	if err != nil || len(tags) == 0 {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var dirs, files []Removed
	paths := dirPaths{}
	for _, pass := range []struct {
		table, col string
		ids        []int64
		isDir      bool
	}{{"dir_tag", "dir_id", t.Dirs, true}, {"file_tag", "instance_id", t.Instances, false}} {
		for _, id := range pass.ids {
			var path string
			if pass.isDir {
				path, err = paths.dir(tx, id)
			} else {
				var dir int64
				var name string
				if err = tx.QueryRow("SELECT dir_id, filename FROM instance WHERE id = ?", id).Scan(&dir, &name); err == nil {
					path, err = paths.file(tx, dir, name)
				}
			}
			if err != nil {
				return nil, err
			}
			for i, tag := range tags {
				r, err := tx.Exec("DELETE FROM "+pass.table+" WHERE tag_id = ? AND "+pass.col+" = ?", tag, id)
				if err != nil {
					return nil, err
				}
				if n, _ := r.RowsAffected(); n > 0 {
					rm := Removed{Tag: found[i], Path: path, IsDir: pass.isDir}
					if pass.isDir {
						dirs = append(dirs, rm)
					} else {
						files = append(files, rm)
					}
				}
			}
		}
	}
	byPath := func(a, b Removed) int { return cmp.Compare(a.Path, b.Path) }
	slices.SortStableFunc(dirs, byPath)
	slices.SortStableFunc(files, byPath)
	return append(dirs, files...), tx.Commit()
}

// TagCount is one row of `tags list`.
type TagCount struct {
	Name        string
	Files, Dirs int
}

func (d *DB) ListTags() ([]TagCount, error) {
	rows, err := d.Query(`SELECT t.name, COUNT(DISTINCT ft.instance_id), COUNT(DISTINCT dt.dir_id)
FROM tag t LEFT JOIN file_tag ft ON t.id = ft.tag_id LEFT JOIN dir_tag dt ON t.id = dt.tag_id
GROUP BY t.id, t.name ORDER BY t.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TagCount
	for rows.Next() {
		var c TagCount
		if err := rows.Scan(&c.Name, &c.Files, &c.Dirs); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteTag deletes a tag and, by cascade, its associations; returns how many there were.
func (d *DB) DeleteTag(name string) (int, error) {
	var id int64
	if err := d.QueryRow("SELECT id FROM tag WHERE name = ?", name).Scan(&id); err == sql.ErrNoRows {
		return 0, fmt.Errorf("Tag not found: %s", name)
	} else if err != nil {
		return 0, err
	}
	var n int
	if err := d.QueryRow("SELECT (SELECT COUNT(*) FROM file_tag WHERE tag_id = ?1) + (SELECT COUNT(*) FROM dir_tag WHERE tag_id = ?1)", id).Scan(&n); err != nil {
		return 0, err
	}
	_, err := d.Exec("DELETE FROM tag WHERE id = ?", id)
	return n, err
}

// FindByTags returns files directly in dir (whole tree with recursive) and dirs
// below it (never dir itself) that carry every named tag. Files first, then
// dirs, each sorted by path; dirs end in "/". Duplicate names count once.
func (d *DB) FindByTags(dir int64, names []string, recursive bool) ([]string, error) {
	names = slices.Compact(slices.Sorted(slices.Values(names)))
	tags, found, err := tagIDs(d, names)
	if err != nil || len(found) < len(names) {
		return nil, err // an unknown tag matches nothing
	}
	in, args := InList(tags)
	need := len(tags)

	instQ := "SELECT i.id, i.dir_id, i.filename FROM instance i WHERE i.dir_id = ?"
	dirQ := "SELECT id FROM dir WHERE parent_id = ?"
	if recursive {
		instQ = treeCTE + "SELECT i.id, i.dir_id, i.filename FROM instance i WHERE i.dir_id IN (SELECT id FROM dir_tree)"
		dirQ = treeCTE + "SELECT id FROM dir_tree WHERE id != ?1"
	}
	instQ += " AND (SELECT COUNT(DISTINCT tag_id) FROM file_tag WHERE instance_id = i.id AND tag_id IN (" + in + ")) = ?"
	rows, err := d.Query(instQ, append(append([]any{dir}, args...), need)...)
	if err != nil {
		return nil, err
	}
	type inst struct {
		dir  int64
		name string
	}
	var insts []inst
	for rows.Next() {
		var id int64
		var x inst
		if err := rows.Scan(&id, &x.dir, &x.name); err != nil {
			rows.Close()
			return nil, err
		}
		insts = append(insts, x)
	}
	rows.Close()
	var files []string
	paths := dirPaths{}
	for _, x := range insts {
		p, err := paths.file(d, x.dir, x.name)
		if err != nil {
			return nil, err
		}
		files = append(files, p)
	}

	dirIDs, err := d.ids("SELECT id FROM ("+dirQ+") WHERE (SELECT COUNT(DISTINCT tag_id) FROM dir_tag WHERE dir_id = id AND tag_id IN ("+in+")) = ?",
		append(append([]any{dir}, args...), need)...)
	if err != nil {
		return nil, err
	}
	var dirs []string
	if err := paths.preload(d, dirIDs); err != nil {
		return nil, err
	}
	for _, id := range dirIDs {
		p, err := paths.dir(d, id)
		if err != nil {
			return nil, err
		}
		if p != "/" {
			p += "/"
		}
		dirs = append(dirs, p)
	}
	slices.Sort(files)
	slices.Sort(dirs)
	return append(files, dirs...), nil
}

// IndexTagTarget resolves a path like the CLI's tag target but from the index
// instead of the disk, so it works for entries gone from disk: an indexed dir
// (with recursive, its whole tree), else an indexed instance.
func (d *DB) IndexTagTarget(path string, recursive bool) (TagTarget, error) {
	dir, isDir, err := DirByPath(d, path)
	switch {
	case err != nil:
		return TagTarget{}, err
	case isDir && recursive:
		return d.TreeTarget(*dir)
	case isDir:
		return TagTarget{Dirs: []int64{*dir}}, nil
	case recursive:
		return TagTarget{}, fmt.Errorf("Recursive mode requires a directory: %s", path)
	}
	id, found, err := d.InstanceID(path)
	if err != nil {
		return TagTarget{}, err
	}
	if !found {
		return TagTarget{}, fmt.Errorf("Path not found in database: %s", path)
	}
	return TagTarget{Instances: []int64{id}}, nil
}

const (
	// fileTags and subdirTags list (entry name, tag) for everything directly in a dir.
	fileTags = `SELECT i.filename, t.name FROM instance i JOIN file_tag ft ON ft.instance_id = i.id
JOIN tag t ON t.id = ft.tag_id WHERE i.dir_id = ? ORDER BY t.name`
	subdirTags = `SELECT d.name, t.name FROM dir d JOIN dir_tag dt ON dt.dir_id = d.id
JOIN tag t ON t.id = dt.tag_id WHERE d.parent_id = ? ORDER BY t.name`
)

// tagsByName runs fileTags or subdirTags and groups the tags by entry name,
// each list sorted. Names are unique per dir, so they key as well as ids.
func (d *DB) tagsByName(query string, dir int64) (map[string][]string, error) {
	rows, err := d.Query(query, dir)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var name, tag string
		if err := rows.Scan(&name, &tag); err != nil {
			return nil, err
		}
		out[name] = append(out[name], tag)
	}
	return out, rows.Err()
}

// EntryTags returns the tags of the named entries directly in the dir at path:
// a file's from its instance, a subdir's from its dir row (both, if the index
// has both under one name). Untagged and unindexed names are left out. The
// query count doesn't grow with the number of names.
func (d *DB) EntryTags(dir string, names []string) (map[string][]string, error) {
	out := map[string][]string{}
	id, found, err := DirByPath(d, dir)
	if err != nil || !found {
		return out, err
	}
	files, err := d.tagsByName(fileTags, *id)
	if err != nil {
		return nil, err
	}
	subdirs, err := d.tagsByName(subdirTags, *id)
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if tags := append(files[n], subdirs[n]...); len(tags) > 0 {
			out[n] = tags
		}
	}
	return out, nil
}
