package db

import (
	"cmp"
	"database/sql"
	"encoding/json"
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

func (d *DB) ids(q string, args ...any) ([]int64, error) { return queryIDs(d, q, args...) }

func queryIDs(q Querier, query string, args ...any) ([]int64, error) {
	rows, err := q.Query(query, args...)
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
// returns how many new associations were made. An instance's tag goes on its
// content, so copies share it (and share one association); an unhashed instance
// holds a pending tag until its hash lands. Empty files are skipped: all of them
// are the same content (EmptyInstances counts them).
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
	q := &stmtCache{tx: tx, stmts: map[string]*sql.Stmt{}}
	added := 0
	add := func(query string, key any) error {
		for _, tag := range tags {
			r, err := q.Exec(query, tag, key)
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			added += int(n)
		}
		return nil
	}
	for _, id := range t.Dirs {
		if err := add("INSERT OR IGNORE INTO dir_tag (tag_id, dir_id) VALUES (?, ?)", id); err != nil {
			return 0, err
		}
	}
	for _, id := range t.Instances {
		size, sha, err := instanceContent(q, id)
		if err != nil {
			return 0, err
		}
		switch {
		case size == 0:
		case sha.Valid:
			err = add("INSERT OR IGNORE INTO content_tag (tag_id, sha256) VALUES (?, ?)", sha.String)
		default:
			err = add("INSERT OR IGNORE INTO pending_tag (tag_id, instance_id) VALUES (?, ?)", id)
		}
		if err != nil {
			return 0, err
		}
	}
	return added, tx.Commit()
}

// instanceContent returns an instance's size and its content's sha256 (invalid while unhashed).
func instanceContent(q Querier, id int64) (int64, sql.NullString, error) {
	var size int64
	var sha sql.NullString
	err := q.QueryRow("SELECT i.size, f.sha256 FROM instance i LEFT JOIN file f ON f.id = i.file_id WHERE i.id = ?", id).Scan(&size, &sha)
	return size, sha, err
}

// EmptyInstances counts the target's empty files, which AddTags skips.
func (d *DB) EmptyInstances(t TagTarget) (int, error) {
	list, err := json.Marshal(t.Instances)
	if err != nil {
		return 0, err
	}
	var n int
	err = d.QueryRow("SELECT COUNT(*) FROM instance WHERE size = 0 AND id IN (SELECT value FROM json_each(?))", string(list)).Scan(&n)
	return n, err
}

// OtherCopies counts the instances outside the target that share content with a
// file in it, so a tag change on the target reaches them too.
func (d *DB) OtherCopies(t TagTarget) (int, error) {
	list, err := json.Marshal(t.Instances)
	if err != nil {
		return 0, err
	}
	var n int
	err = d.QueryRow(`WITH target(id) AS (SELECT value FROM json_each(?1))
SELECT COUNT(*) FROM instance o WHERE o.id NOT IN (SELECT id FROM target) AND o.file_id IN
(SELECT f.id FROM target t CROSS JOIN instance i ON i.id = t.id CROSS JOIN file f ON f.id = i.file_id WHERE f.filesize > 0)`, string(list)).Scan(&n)
	return n, err
}

// Removed is one association removed by RemoveTags.
type Removed struct {
	Tag, Path string
	IsDir     bool
}

// RemoveTags removes the named tags from every target; for a file that means its
// content, so copies outside the target lose them too (OtherCopies counts those).
// Unknown tag names are skipped. Results list dirs then the targeted files, each
// sorted by path, tags in argument order.
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
	q := &stmtCache{tx: tx, stmts: map[string]*sql.Stmt{}}
	var dirs, files []Removed
	paths := dirPaths{}
	for _, id := range t.Dirs {
		path, err := paths.dir(tx, id)
		if err != nil {
			return nil, err
		}
		for i, tag := range tags {
			r, err := q.Exec("DELETE FROM dir_tag WHERE tag_id = ? AND dir_id = ?", tag, id)
			if err != nil {
				return nil, err
			}
			if n, _ := r.RowsAffected(); n > 0 {
				dirs = append(dirs, Removed{Tag: found[i], Path: path, IsDir: true})
			}
		}
	}
	type contentKey struct {
		tag int64
		sha string
	}
	cleared := map[contentKey]bool{} // copies in the target share a row: each still reports it
	for _, id := range t.Instances {
		var dir int64
		var name string
		var sha sql.NullString
		if err := q.QueryRow(`SELECT i.dir_id, i.filename, f.sha256 FROM instance i LEFT JOIN file f ON f.id = i.file_id
WHERE i.id = ?`, id).Scan(&dir, &name, &sha); err != nil {
			return nil, err
		}
		path, err := paths.file(tx, dir, name)
		if err != nil {
			return nil, err
		}
		for i, tag := range tags {
			r, err := q.Exec("DELETE FROM pending_tag WHERE tag_id = ? AND instance_id = ?", tag, id)
			if err != nil {
				return nil, err
			}
			n, _ := r.RowsAffected()
			hit := n > 0
			if k := (contentKey{tag, sha.String}); sha.Valid {
				if r, err = q.Exec("DELETE FROM content_tag WHERE tag_id = ? AND sha256 = ?", tag, sha.String); err != nil {
					return nil, err
				}
				if n, _ := r.RowsAffected(); n > 0 {
					cleared[k] = true
				}
				hit = hit || cleared[k]
			}
			if hit {
				files = append(files, Removed{Tag: found[i], Path: path})
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

// taggedInstances selects the current instances carrying the tag whose id is the
// expression tag: by content or pending. Tags on absent content match nothing.
func taggedInstances(tag string) string {
	return `SELECT i.id FROM content_tag ct CROSS JOIN file f ON f.sha256 = ct.sha256 CROSS JOIN instance i ON i.file_id = f.id
WHERE ct.tag_id = ` + tag + ` UNION SELECT instance_id FROM pending_tag WHERE tag_id = ` + tag
}

func (d *DB) ListTags() ([]TagCount, error) {
	rows, err := d.Query(`SELECT t.name, (SELECT COUNT(*) FROM (` + taggedInstances("t.id") + `)),
(SELECT COUNT(*) FROM dir_tag WHERE tag_id = t.id) FROM tag t ORDER BY t.name`)
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
	if err := d.QueryRow("SELECT (SELECT COUNT(*) FROM ("+taggedInstances("?1")+")) + (SELECT COUNT(*) FROM dir_tag WHERE tag_id = ?1)", id).Scan(&n); err != nil {
		return 0, err
	}
	_, err := d.Exec("DELETE FROM tag WHERE id = ?", id)
	return n, err
}

// TagGC deletes the content tags whose content has been absent for more than days
// days, and the sample tags and directory prints kept that long, and returns how
// many tags. Content that has
// a file row keeps its tags whatever gone_at says: only this deletes a tag on age,
// so it must never cost a live one.
func (d *DB) TagGC(days int) (int64, error) {
	age := fmt.Sprintf("-%d days", days)
	r, err := d.Exec(`DELETE FROM content_tag WHERE gone_at IS NOT NULL AND gone_at <= datetime('now', ?)
AND NOT EXISTS (SELECT 1 FROM file f WHERE f.sha256 = content_tag.sha256)`, age)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	if r, err = d.Exec("DELETE FROM sample_tag WHERE kept_at <= datetime('now', ?)", age); err != nil {
		return n, err
	}
	m, _ := r.RowsAffected()
	var k int64
	if err := d.QueryRow(`SELECT COUNT(*) FROM dir_print_tag WHERE print_id IN
(SELECT id FROM dir_print WHERE kept_at <= datetime('now', ?))`, age).Scan(&k); err != nil {
		return n + m, err
	}
	if _, err = d.Exec("DELETE FROM dir_print WHERE kept_at <= datetime('now', ?)", age); err != nil {
		return n + m, err
	}
	return n + m + k, nil
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
	instQ += ` AND (SELECT COUNT(*) FROM tag t WHERE t.id IN (` + in + `) AND (
EXISTS (SELECT 1 FROM file f CROSS JOIN content_tag ct ON ct.sha256 = f.sha256 AND ct.tag_id = t.id WHERE f.id = i.file_id)
OR EXISTS (SELECT 1 FROM pending_tag p WHERE p.instance_id = i.id AND p.tag_id = t.id))) = ?`
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
	fileTags = `SELECT i.filename, t.name FROM instance i CROSS JOIN file f ON f.id = i.file_id
CROSS JOIN content_tag ct ON ct.sha256 = f.sha256 CROSS JOIN tag t ON t.id = ct.tag_id WHERE i.dir_id = ?1
UNION SELECT i.filename, t.name FROM instance i CROSS JOIN pending_tag p ON p.instance_id = i.id
CROSS JOIN tag t ON t.id = p.tag_id WHERE i.dir_id = ?1 ORDER BY 2`
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

// fileTagsByName is tagsByName(fileTags): the query walks every instance of the
// dir, which an index without file tags can skip.
func (d *DB) fileTagsByName(dir int64) (map[string][]string, error) {
	var some bool
	if err := d.QueryRow("SELECT EXISTS (SELECT 1 FROM content_tag) OR EXISTS (SELECT 1 FROM pending_tag)").Scan(&some); err != nil {
		return nil, err
	}
	if !some {
		return map[string][]string{}, nil
	}
	return d.tagsByName(fileTags, dir)
}

// EntryTags returns the tags of the named entries directly in the dir at path:
// a file's from its content or pending tags, a subdir's from its dir row (both, if the index
// has both under one name). Untagged and unindexed names are left out. The
// query count doesn't grow with the number of names.
func (d *DB) EntryTags(dir string, names []string) (map[string][]string, error) {
	out := map[string][]string{}
	id, found, err := DirByPath(d, dir)
	if err != nil || !found {
		return out, err
	}
	files, err := d.fileTagsByName(*id)
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
