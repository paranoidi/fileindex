package db

import (
	"database/sql"
	"strings"
)

// ExportHeader is the CSV header; one row per instance, file columns repeated.
var ExportHeader = []string{"md5", "sha256", "sha256_noexif_jpeg", "sha256_8kb", "filesize",
	"file_created_at", "file_last_seen", "path", "filename", "mtime", "instance_created_at", "instance_last_seen"}

// ExportRows calls fn with every instance as a row in ExportHeader order, sorted
// by md5, dir id, filename. NULL is "". Timestamps are the stored text.
func (d *DB) ExportRows(fn func([]string) error) error {
	rows, err := d.Query(`SELECT f.md5, f.sha256, f.sha256_noexif_jpeg, f.sha256_8kb, f.filesize,
CAST(f.created_at AS TEXT), CAST(f.last_seen AS TEXT), i.dir_id, i.filename, CAST(i.mtime AS TEXT),
CAST(i.created_at AS TEXT), CAST(i.last_seen AS TEXT)
FROM file f JOIN instance i ON f.id = i.file_id
ORDER BY f.md5, i.dir_id, i.filename`)
	if err != nil {
		return err
	}
	defer rows.Close()
	paths := dirPaths{}
	for rows.Next() {
		var v [12]sql.NullString
		var dir int64
		if err := rows.Scan(&v[0], &v[1], &v[2], &v[3], &v[4], &v[5], &v[6], &dir, &v[8], &v[9], &v[10], &v[11]); err != nil {
			return err
		}
		p, err := paths.dir(d, dir)
		if err != nil {
			return err
		}
		v[7] = sql.NullString{String: p, Valid: true}
		rec := make([]string, len(v))
		for i, s := range v {
			rec[i] = s.String
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ImportRow is one validated CSV row; "" means empty.
type ImportRow struct {
	MD5, SHA256, NoExifJPEG, SHA256_8KB string
	Size                                int64
	FileCreated, FileLastSeen           string
	Path, Filename, Mtime               string
	InstCreated, InstLastSeen           string
}

// nullIfEmpty maps "" to NULL for nullable columns.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// insertOmittingEmpty inserts cols, leaving out timestamp columns whose value
// is "" so their DEFAULT applies.
func insertOmittingEmpty(tx *sql.Tx, table string, cols []string, vals []any, timestamps map[string]string) (sql.Result, error) {
	for c, v := range timestamps {
		if v != "" {
			cols = append(cols, c)
			vals = append(vals, v)
		}
	}
	q := "INSERT INTO " + table + " (" + strings.Join(cols, ", ") + ") VALUES (" +
		strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ") + ")"
	return tx.Exec(q, vals...)
}

// ImportBatch records rows in one transaction. Files are matched by md5; within
// a batch the first row for an md5 supplies its file columns. An instance is
// matched by dir + filename and pointed at the row's file.
// Returns new file rows and new instances.
func (d *DB) ImportBatch(rows []ImportRow) (files, instances int, err error) {
	tx, err := d.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	fileIDs := map[string]int64{}
	for _, r := range rows {
		fileID, seen := fileIDs[r.MD5]
		if !seen {
			err := tx.QueryRow("SELECT id FROM file WHERE md5 = ?", r.MD5).Scan(&fileID)
			switch {
			case err == sql.ErrNoRows:
				res, err := insertOmittingEmpty(tx, "file",
					[]string{"md5", "sha256", "sha256_noexif_jpeg", "sha256_8kb", "filesize"},
					[]any{r.MD5, r.SHA256, nullIfEmpty(r.NoExifJPEG), nullIfEmpty(r.SHA256_8KB), r.Size},
					map[string]string{"created_at": r.FileCreated, "last_seen": r.FileLastSeen})
				if err != nil {
					return 0, 0, err
				}
				fileID, _ = res.LastInsertId()
				files++
			case err != nil:
				return 0, 0, err
			case r.FileLastSeen != "":
				if _, err := tx.Exec("UPDATE file SET last_seen = ? WHERE id = ?", r.FileLastSeen, fileID); err != nil {
					return 0, 0, err
				}
			}
			fileIDs[r.MD5] = fileID
		}

		dirID, err := GetOrCreatePath(tx, r.Path)
		if err != nil {
			return 0, 0, err
		}
		var instID int64
		err = tx.QueryRow("SELECT id FROM instance WHERE dir_id = ? AND filename = ?", dirID, r.Filename).Scan(&instID)
		switch err {
		case sql.ErrNoRows:
			_, err = insertOmittingEmpty(tx, "instance",
				[]string{"file_id", "dir_id", "filename", "filename_lower", "mtime"},
				[]any{fileID, dirID, r.Filename, Lower(r.Filename), nullIfEmpty(r.Mtime)},
				map[string]string{"created_at": r.InstCreated, "last_seen": r.InstLastSeen})
			instances++
		case nil:
			_, err = tx.Exec("UPDATE instance SET file_id = ?, mtime = ?, last_seen = COALESCE(?, last_seen) WHERE id = ?",
				fileID, nullIfEmpty(r.Mtime), nullIfEmpty(r.InstLastSeen), instID)
		}
		if err != nil {
			return 0, 0, err
		}
	}
	return files, instances, tx.Commit()
}
