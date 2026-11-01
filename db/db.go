// Package db holds the schema v7 SQLite index and every query on it.
//
// Read TIMESTAMP columns (mtime, created_at, last_seen) as CAST(col AS TEXT):
// the driver otherwise parses them into time.Time and reformats them.
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const SchemaVersion = 7

// Querier is satisfied by *sql.DB and *sql.Tx, so helpers can run inside a caller's transaction.
type Querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

type DB struct {
	*sql.DB
	Path string
}

// Open creates the DB's parent dir and opens a pool where every connection has
// busy_timeout, foreign_keys and a 64 MiB page cache set. The pool is deliberately not capped at one:
// helpers may query while a transaction is open.
// synchronous=NORMAL: in WAL mode commits skip fsync (only checkpoints sync), so
// a slow fsync on a busy disk no longer stalls scan; a power loss can drop the
// last few commits, which a rescan redoes.
// wal_autocheckpoint(16000): checkpoint at a 64 MiB WAL instead of 4 MiB; each
// checkpoint runs inside a commit and fsyncs, and was a fifth of scan's commit
// time. The WAL file stays at its high-water mark afterwards.
// Another process may hold it open for hours alongside CLI scans: WAL lets
// readers run during a scan's writes, and busy_timeout makes writers wait
// their turn instead of failing.
// ponytail: path goes into the DSN unescaped, a '?' in the DB path breaks it.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	sdb, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(30000)&_pragma=foreign_keys(1)&_pragma=cache_size(-65536)&_pragma=synchronous(1)&_pragma=wal_autocheckpoint(16000)")
	if err != nil {
		return nil, err
	}
	return &DB{DB: sdb, Path: path}, nil
}

const schema = `
CREATE TABLE schema_version (
    version INTEGER PRIMARY KEY,
    applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE dir (
    id INTEGER PRIMARY KEY,
    parent_id INTEGER,
    name TEXT NOT NULL,
    name_lower TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_seen TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (parent_id) REFERENCES dir(id) ON DELETE CASCADE,
    UNIQUE(parent_id, name)
);
CREATE TABLE file (
    id INTEGER PRIMARY KEY,
    md5 TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    sha256_noexif_jpeg TEXT,
    sha256_8kb TEXT,
    filesize INTEGER NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_seen TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(md5)
);
CREATE TABLE instance (
    id INTEGER PRIMARY KEY,
    file_id INTEGER NOT NULL,
    dir_id INTEGER NOT NULL,
    filename TEXT NOT NULL,
    filename_lower TEXT NOT NULL,
    mtime TIMESTAMP,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_seen TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (file_id) REFERENCES file(id) ON DELETE CASCADE,
    FOREIGN KEY (dir_id) REFERENCES dir(id) ON DELETE CASCADE,
    UNIQUE(dir_id, filename)
);
CREATE TABLE tag (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_seen TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE file_tag (
    tag_id INTEGER NOT NULL,
    instance_id INTEGER NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (tag_id) REFERENCES tag(id) ON DELETE CASCADE,
    FOREIGN KEY (instance_id) REFERENCES instance(id) ON DELETE CASCADE,
    UNIQUE(tag_id, instance_id)
);
CREATE TABLE dir_tag (
    tag_id INTEGER NOT NULL,
    dir_id INTEGER,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (tag_id) REFERENCES tag(id) ON DELETE CASCADE,
    FOREIGN KEY (dir_id) REFERENCES dir(id) ON DELETE CASCADE,
    UNIQUE(tag_id, dir_id)
);
CREATE INDEX idx_dir_parent_id ON dir(parent_id);
CREATE INDEX idx_dir_name ON dir(name);
CREATE INDEX idx_dir_name_lower ON dir(name_lower);
CREATE INDEX idx_file_md5 ON file(md5);
CREATE INDEX idx_file_sha256 ON file(sha256);
CREATE INDEX idx_file_sha256_noexif_jpeg ON file(sha256_noexif_jpeg);
CREATE INDEX idx_file_sha256_8kb ON file(sha256_8kb);
CREATE INDEX idx_file_size ON file(filesize);
CREATE INDEX idx_instance_dir_id ON instance(dir_id);
CREATE INDEX idx_instance_filename ON instance(filename);
CREATE INDEX idx_instance_filename_lower ON instance(filename_lower);
CREATE INDEX idx_instance_file_id ON instance(file_id);
CREATE INDEX idx_instance_mtime ON instance(mtime);
CREATE INDEX idx_tag_name ON tag(name);
CREATE INDEX idx_file_tag_tag_id ON file_tag(tag_id);
CREATE INDEX idx_file_tag_instance_id ON file_tag(instance_id);
CREATE INDEX idx_dir_tag_tag_id ON dir_tag(tag_id);
CREATE INDEX idx_dir_tag_dir_id ON dir_tag(dir_id);
` + ftsSchema + `INSERT INTO schema_version (version) VALUES (7);
`

// ftsSchema adds two external-content FTS5 indexes over instance.filename, kept
// in sync by triggers: instance_trigram serves find's LIKE (substring), and
// instance_words serves find --words (tokens, prefix, boolean queries).
// Rescans update instance rows without touching filename, so they skip the
// update trigger.
const ftsSchema = `
CREATE VIRTUAL TABLE instance_trigram USING fts5(filename, content='instance', content_rowid='id',
    tokenize='trigram remove_diacritics 0', detail=none, columnsize=0);
CREATE VIRTUAL TABLE instance_words USING fts5(filename, content='instance', content_rowid='id',
    tokenize='unicode61 remove_diacritics 2');
CREATE TRIGGER instance_fts_ai AFTER INSERT ON instance BEGIN
    INSERT INTO instance_trigram(rowid, filename) VALUES (new.id, new.filename);
    INSERT INTO instance_words(rowid, filename) VALUES (new.id, new.filename);
END;
CREATE TRIGGER instance_fts_ad AFTER DELETE ON instance BEGIN
    INSERT INTO instance_trigram(instance_trigram, rowid, filename) VALUES ('delete', old.id, old.filename);
    INSERT INTO instance_words(instance_words, rowid, filename) VALUES ('delete', old.id, old.filename);
END;
CREATE TRIGGER instance_fts_au AFTER UPDATE OF filename ON instance BEGIN
    INSERT INTO instance_trigram(instance_trigram, rowid, filename) VALUES ('delete', old.id, old.filename);
    INSERT INTO instance_words(instance_words, rowid, filename) VALUES ('delete', old.id, old.filename);
    INSERT INTO instance_trigram(rowid, filename) VALUES (new.id, new.filename);
    INSERT INTO instance_words(rowid, filename) VALUES (new.id, new.filename);
END;
`

// migrateV6 adds the FTS indexes to a v6 DB and fills them from instance.
const migrateV6 = ftsSchema + `
INSERT INTO instance_trigram(instance_trigram) VALUES ('rebuild');
INSERT INTO instance_words(instance_words) VALUES ('rebuild');
INSERT INTO schema_version (version) VALUES (7);
`

// Init switches to WAL, then checks the schema version, migrates a v6 DB
// (announced on w, since rebuilding the FTS indexes takes minutes on millions
// of instances) or creates schema v7.
// It is the only place that changes the journal mode or creates tables.
func (d *DB) Init(w io.Writer) error {
	if _, err := d.Exec("PRAGMA journal_mode = WAL"); err != nil {
		return err
	}
	exists, err := d.hasTable("schema_version")
	if err != nil {
		return err
	}
	ddl := schema
	if exists {
		v, err := d.version()
		if err != nil {
			return err
		}
		switch {
		case v.Valid && v.Int64 == SchemaVersion:
			return nil
		case v.Valid && v.Int64 == 6:
			fmt.Fprintln(w, "Migrating database to schema v7 (full-text search)...")
			ddl = migrateV6
		default:
			found := "None"
			if v.Valid {
				found = fmt.Sprint(v.Int64)
			}
			return fmt.Errorf("Database schema version mismatch: expected %d, found %s", SchemaVersion, found)
		}
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(ddl); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) version() (sql.NullInt64, error) {
	var v sql.NullInt64
	err := d.QueryRow("SELECT MAX(version) FROM schema_version").Scan(&v)
	return v, err
}

func (d *DB) hasTable(name string) (bool, error) {
	var n int
	err := d.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&n)
	return n > 0, err
}

var ErrNotInitialised = errors.New("database not initialised, run `fileindex init`")

var ErrOutdatedSchema = errors.New("database schema is outdated, run `fileindex init` to upgrade it")

var ErrNewerSchema = errors.New("database schema is newer than this fileindex supports, upgrade fileindex")

// RequireSchema fails early on a DB that `init` never ran on, one still on
// an older schema (`init` migrates it), or one on a newer schema than this
// code writes.
func (d *DB) RequireSchema() error {
	ok, err := d.hasTable("schema_version")
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotInitialised
	}
	if v, err := d.version(); err != nil {
		return err
	} else if v.Valid && v.Int64 < SchemaVersion {
		return ErrOutdatedSchema
	} else if v.Valid && v.Int64 > SchemaVersion {
		return ErrNewerSchema
	}
	return nil
}

type Stats struct {
	Dirs, Files, Instances, TotalSize, DuplicateFiles, DBSize int64
}

func (d *DB) Stats() (Stats, error) {
	var s Stats
	for _, q := range []struct {
		sql string
		dst *int64
	}{
		{"SELECT COUNT(*) FROM dir", &s.Dirs},
		{"SELECT COUNT(*) FROM file", &s.Files},
		{"SELECT COUNT(*) FROM instance", &s.Instances},
		{"SELECT COALESCE(SUM(filesize), 0) FROM file", &s.TotalSize},
		{"SELECT COUNT(*) FROM file f WHERE f.filesize > 0 AND (SELECT COUNT(*) FROM instance i WHERE i.file_id = f.id) > 1", &s.DuplicateFiles},
	} {
		if err := d.QueryRow(q.sql).Scan(q.dst); err != nil {
			return s, err
		}
	}
	// main file plus -wal
	for _, p := range []string{d.Path, d.Path + "-wal"} {
		if fi, err := os.Stat(p); err == nil {
			s.DBSize += fi.Size()
		}
	}
	return s, nil
}

func (d *DB) Vacuum() error {
	_, err := d.Exec("VACUUM")
	return err
}

// DeleteOrphanedFiles deletes every file row no instance references, across the whole DB.
func (d *DB) DeleteOrphanedFiles() (int64, error) {
	res, err := d.Exec("DELETE FROM file WHERE id NOT IN (SELECT DISTINCT file_id FROM instance)")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
