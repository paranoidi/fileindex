package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// Dir ids are *int64: nil is the virtual root above the top-level dirs (the
// "/" row for absolute paths), so lookups use SQL's `parent_id IS ?`.

// GetOrCreateDir finds name under parent (touching last_seen) or inserts it.
func GetOrCreateDir(q Querier, parent *int64, name string) (int64, error) {
	var id int64
	err := q.QueryRow("SELECT id FROM dir WHERE parent_id IS ? AND name = ?", parent, name).Scan(&id)
	if err == nil {
		_, err = q.Exec("UPDATE dir SET last_seen = CURRENT_TIMESTAMP WHERE id = ?", id)
		return id, err
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	res, err := q.Exec("INSERT INTO dir (parent_id, name, name_lower) VALUES (?, ?, ?)", parent, name, Lower(name))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetOrCreatePath creates every component of path; "" and "." give nil.
func GetOrCreatePath(q Querier, path string) (*int64, error) {
	if path == "" || path == "." {
		return nil, nil
	}
	var parent *int64
	for _, part := range PathParts(path) {
		id, err := GetOrCreateDir(q, parent, part)
		if err != nil {
			return nil, err
		}
		parent = &id
	}
	return parent, nil
}

// DirByPath resolves path to a dir id without creating anything. found is false
// for a missing path and for "" / "." (the virtual root); callers tell them apart.
func DirByPath(q Querier, path string) (id *int64, found bool, err error) {
	if path == "" || path == "." {
		return nil, false, nil
	}
	var parent *int64
	for _, part := range PathParts(path) {
		var next int64
		err := q.QueryRow("SELECT id FROM dir WHERE parent_id IS ? AND name = ?", parent, part).Scan(&next)
		if err == sql.ErrNoRows {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		parent = &next
	}
	return parent, true, nil
}

// BuildDirPath rebuilds a dir's path from its ancestors; nil gives "".
func BuildDirPath(q Querier, id *int64) (string, error) {
	if id == nil {
		return "", nil
	}
	var parts []string
	cur := id
	for cur != nil {
		var name string
		var parent sql.NullInt64
		err := q.QueryRow("SELECT name, parent_id FROM dir WHERE id = ?", *cur).Scan(&name, &parent)
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("Directory id %d not found in database", *cur)
		}
		if err != nil {
			return "", err
		}
		parts = append(parts, name)
		cur = nil
		if parent.Valid {
			cur = &parent.Int64
		}
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return JoinParts(parts), nil
}

// BuildFilePath is dir path + "/" + filename, or the bare filename at the virtual root.
func BuildFilePath(q Querier, dirID *int64, filename string) (string, error) {
	dir, err := BuildDirPath(q, dirID)
	if err != nil || dir == "" {
		return filename, err
	}
	return JoinParts([]string{dir, filename}), nil
}

// dirPaths memoises BuildDirPath per dir id, so listing many files in few dirs
// doesn't re-walk the same ancestors for every row.
type dirPaths map[int64]string

func (m dirPaths) dir(q Querier, id int64) (string, error) {
	p, ok := m[id]
	if !ok {
		var err error
		if p, err = BuildDirPath(q, &id); err != nil {
			return "", err
		}
		m[id] = p
	}
	return p, nil
}

// file is BuildFilePath for a dir that exists (never the virtual root).
func (m dirPaths) file(q Querier, dir int64, name string) (string, error) {
	p, err := m.dir(q, dir)
	if err != nil {
		return "", err
	}
	return JoinParts([]string{p, name}), nil
}

// TreeIDs returns id and all its descendants, in CTE order; nil gives none.
func TreeIDs(q Querier, id *int64, includeSelf bool) ([]int64, error) {
	if id == nil {
		return nil, nil
	}
	rows, err := q.Query(`
WITH RECURSIVE dir_tree AS (
    SELECT id FROM dir WHERE id = ?
    UNION ALL
    SELECT d.id FROM dir d
    JOIN dir_tree dt ON d.parent_id = dt.id
)
SELECT id FROM dir_tree`, *id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var x int64
		if err := rows.Scan(&x); err != nil {
			return nil, err
		}
		if includeSelf || x != *id {
			ids = append(ids, x)
		}
	}
	return ids, rows.Err()
}

// InList renders "?,?,?" and the args for an IN clause.
func InList(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

const emptyDirs = `SELECT d.id FROM dir d
LEFT JOIN dir child ON d.id = child.parent_id
LEFT JOIN instance i ON d.id = i.dir_id
WHERE child.id IS NULL AND i.id IS NULL`

// DeleteOrphanedDirs repeatedly deletes empty leaf dirs across the whole DB
// until none are left, and returns the total.
func (d *DB) DeleteOrphanedDirs() (int64, error) {
	var total int64
	for {
		res, err := d.Exec("DELETE FROM dir WHERE id IN (" + emptyDirs + ")")
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return total, nil
		}
		total += n
	}
}
