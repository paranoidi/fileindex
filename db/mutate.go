package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
)

// InstanceID finds the instance at an absolute file path.
func (d *DB) InstanceID(path string) (int64, bool, error) {
	dirID, found, err := DirByPath(d, filepath.Dir(path))
	if err != nil || !found {
		return 0, false, err
	}
	var id int64
	err = d.QueryRow("SELECT id FROM instance WHERE dir_id = ? AND filename = ?", *dirID, filepath.Base(path)).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	return id, err == nil, err
}

// MoveInstance moves an instance to the dir at dstDir (created as needed) under name.
func (d *DB) MoveInstance(id int64, dstDir, name string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	dirID, err := GetOrCreatePath(tx, dstDir)
	if err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE instance SET dir_id = ?, filename = ?, filename_lower = ? WHERE id = ?", dirID, name, Lower(name), id); err != nil {
		return err
	}
	return tx.Commit()
}

// MoveDir re-parents and renames a dir; its subtree follows.
func (d *DB) MoveDir(id int64, dstParent, name string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	parent, err := GetOrCreatePath(tx, dstParent)
	if err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE dir SET parent_id = ?, name = ?, name_lower = ? WHERE id = ?", parent, name, Lower(name), id); err != nil {
		return err
	}
	return tx.Commit()
}

// MergeDir moves everything in dir src into dir dst, in one transaction, and
// deletes src. Same-named subdirs are merged recursively, a tag on both sides is
// kept once, and a filename on both sides is an error naming the file.
// Returns dir rows re-parented and instances moved.
func (d *DB) MergeDir(src, dst int64) (dirs, files int64, err error) {
	tx, err := d.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	if dirs, files, err = mergeDir(tx, src, dst); err != nil {
		return 0, 0, err
	}
	return dirs, files, tx.Commit()
}

func mergeDir(tx *sql.Tx, src, dst int64) (dirs, files int64, err error) {
	var clash string
	err = tx.QueryRow(`SELECT s.filename FROM instance s JOIN instance t ON t.dir_id = ? AND t.filename = s.filename
WHERE s.dir_id = ? ORDER BY s.filename LIMIT 1`, dst, src).Scan(&clash)
	if err == nil {
		p, _ := BuildFilePath(tx, &dst, clash)
		return 0, 0, fmt.Errorf("Cannot merge: %s exists in both source and destination", p)
	} else if err != sql.ErrNoRows {
		return 0, 0, err
	}
	r, err := tx.Exec("UPDATE instance SET dir_id = ? WHERE dir_id = ?", dst, src)
	if err != nil {
		return 0, 0, err
	}
	files, _ = r.RowsAffected()

	rows, err := tx.Query(`SELECT s.id, t.id FROM dir s LEFT JOIN dir t ON t.parent_id = ? AND t.name = s.name
WHERE s.parent_id = ? ORDER BY s.name`, dst, src)
	if err != nil {
		return 0, 0, err
	}
	type pair struct {
		src int64
		dst sql.NullInt64
	}
	var children []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.src, &p.dst); err != nil {
			rows.Close()
			return 0, 0, err
		}
		children = append(children, p)
	}
	rows.Close()
	for _, c := range children {
		if !c.dst.Valid {
			if _, err := tx.Exec("UPDATE dir SET parent_id = ? WHERE id = ?", dst, c.src); err != nil {
				return 0, 0, err
			}
			dirs++
			continue
		}
		dd, ff, err := mergeDir(tx, c.src, c.dst.Int64)
		if err != nil {
			return 0, 0, err
		}
		dirs, files = dirs+dd, files+ff
	}

	if _, err := tx.Exec("UPDATE OR IGNORE dir_tag SET dir_id = ? WHERE dir_id = ?", dst, src); err != nil {
		return 0, 0, err
	}
	// leftover clashing tags go with src via ON DELETE CASCADE
	if _, err := tx.Exec("DELETE FROM dir WHERE id = ?", src); err != nil {
		return 0, 0, err
	}
	return dirs, files, nil
}

// Moved counts what Move changed.
type Moved struct{ Dirs, Files, OrphanDirs int64 }

// Move records that the file or dir at src now lives at dst (both absolute and
// resolved): an instance is moved, a dir is moved or, when dst is already an
// indexed dir, merged into it. Orphaned dirs are cleaned up after.
// check (may be nil) runs once src is found, with whether it is a dir; an error
// from it aborts the move. The disk is not looked at otherwise.
func (d *DB) Move(src, dst string, check func(isDir bool) error) (Moved, error) {
	var m Moved
	if src == dst {
		return m, nil
	}
	srcDir, isDir, err := DirByPath(d, src)
	if err != nil {
		return m, err
	}
	var instID int64
	if !isDir {
		var found bool
		if instID, found, err = d.InstanceID(src); err != nil {
			return m, err
		} else if !found {
			return m, fmt.Errorf("Source path '%s' not found in database", src)
		}
	}
	if rel, _ := filepath.Rel(src, dst); isDir && rel != ".." && !strings.HasPrefix(rel, "../") { // would make a dir cycle
		return m, fmt.Errorf("Cannot move %s into its own subdirectory %s", src, dst)
	}
	if check != nil {
		if err := check(isDir); err != nil {
			return m, err
		}
	}

	switch {
	case !isDir:
		if err := d.MoveInstance(instID, filepath.Dir(dst), filepath.Base(dst)); err != nil {
			return m, err
		}
		m.Files = 1
	default:
		dstDir, exists, err := DirByPath(d, dst)
		if err != nil {
			return m, err
		}
		if exists {
			if m.Dirs, m.Files, err = d.MergeDir(*srcDir, *dstDir); err != nil {
				return m, err
			}
		} else {
			if err := d.MoveDir(*srcDir, filepath.Dir(dst), filepath.Base(dst)); err != nil {
				return m, err
			}
			m.Dirs = 1
		}
	}
	m.OrphanDirs, err = d.DeleteOrphanedDirs()
	return m, err
}
