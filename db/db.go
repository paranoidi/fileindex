// Package db holds the schema v10 SQLite index and every query on it.
//
// Read TIMESTAMP columns (mtime, created_at, last_seen) as CAST(col AS TEXT):
// the driver otherwise parses them into time.Time and reformats them.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const SchemaVersion = 10

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

var schema = `
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
` + instanceDDL("instance") + `
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
CREATE INDEX idx_file_sha256 ON file(sha256);
CREATE INDEX idx_file_sha256_noexif_jpeg ON file(sha256_noexif_jpeg);
CREATE INDEX idx_file_sha256_8kb ON file(sha256_8kb);
CREATE INDEX idx_file_size ON file(filesize);
CREATE INDEX idx_file_tag_instance_id ON file_tag(instance_id);
CREATE INDEX idx_dir_tag_dir_id ON dir_tag(dir_id);
` + instanceIndexes + ftsSchema + `INSERT INTO schema_version (version) VALUES (10);
`

// instanceDDL is the instance table under the given name. file_id is NULL while
// the file is not hashed yet (it is already listed and found by name); size is
// the size seen at the last walk or hash, so listings never need the file row.
// sha256_8kb is set on a fast-hashed instance, which has no file row to hold it.
func instanceDDL(name string) string {
	return `CREATE TABLE ` + name + ` (
    id INTEGER PRIMARY KEY,
    file_id INTEGER,
    dir_id INTEGER NOT NULL,
    filename TEXT NOT NULL,
    filename_lower TEXT NOT NULL,
    size INTEGER NOT NULL,
    mtime TIMESTAMP,
    sha256_8kb TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_seen TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (file_id) REFERENCES file(id) ON DELETE SET NULL,
    FOREIGN KEY (dir_id) REFERENCES dir(id) ON DELETE CASCADE,
    UNIQUE(dir_id, filename)
);
`
}

const instanceIndexes = `CREATE INDEX idx_instance_file_id ON instance(file_id);
` + fastIndex

// fastIndex serves PotentialDuplicates. It is partial, so instances without a
// fast hash cost nothing.
const fastIndex = `CREATE INDEX idx_instance_fast ON instance(size, sha256_8kb) WHERE file_id IS NULL AND sha256_8kb IS NOT NULL;
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
` + ftsTriggers
const ftsTriggers = `CREATE TRIGGER instance_fts_ai AFTER INSERT ON instance BEGIN
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

// migrateV8 adds the fast hash column and its index.
const migrateV8 = `ALTER TABLE instance ADD COLUMN sha256_8kb TEXT;
` + fastIndex + `INSERT INTO schema_version (version) VALUES (9);
`

// migrateV9 drops indexes no query needs. They were a fifth of the DB and every
// instance insert paid for them: the UNIQUE constraints already index md5,
// tag name, (dir_id, filename), (parent_id, name) and (tag_id, ...), LIKE with
// ESCAPE cannot seek on the *_lower columns, and nothing filters on mtime or on
// a bare filename or dir name.
const migrateV9 = `DROP INDEX IF EXISTS idx_instance_dir_id;
DROP INDEX IF EXISTS idx_instance_filename;
DROP INDEX IF EXISTS idx_instance_filename_lower;
DROP INDEX IF EXISTS idx_instance_mtime;
DROP INDEX IF EXISTS idx_file_md5;
DROP INDEX IF EXISTS idx_dir_parent_id;
DROP INDEX IF EXISTS idx_dir_name;
DROP INDEX IF EXISTS idx_dir_name_lower;
DROP INDEX IF EXISTS idx_tag_name;
DROP INDEX IF EXISTS idx_file_tag_tag_id;
DROP INDEX IF EXISTS idx_dir_tag_tag_id;
INSERT INTO schema_version (version) VALUES (10);
`

// migrateV7 rebuilds instance for the current schema, going straight to v9 since
// instanceDDL already has v9's column (SQLite cannot drop NOT NULL in
// place): file_id becomes nullable and size is copied from the file row. Ids are
// kept, so file_tag rows and the external-content FTS indexes stay valid and
// need no rebuild. It runs on one connection with foreign keys off, because
// dropping instance would otherwise cascade into file_tag.
func (d *DB) migrateV7() error {
	ctx := context.Background()
	conn, err := d.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(ctx, "PRAGMA foreign_keys = ON") }() // the connection returns to the pool
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(instanceDDL("instance_new") + `
INSERT INTO instance_new (id, file_id, dir_id, filename, filename_lower, size, mtime, created_at, last_seen)
SELECT i.id, i.file_id, i.dir_id, i.filename, i.filename_lower, f.filesize, i.mtime, i.created_at, i.last_seen
FROM instance i JOIN file f ON f.id = i.file_id;
DROP TABLE instance;
ALTER TABLE instance_new RENAME TO instance;
` + instanceIndexes + ftsTriggers + `INSERT INTO schema_version (version) VALUES (9);`); err != nil {
		return err
	}
	var bad int
	if err := tx.QueryRow("SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&bad); err != nil {
		return err
	}
	if bad > 0 {
		return fmt.Errorf("Migration to schema v9 left %d foreign key violations", bad)
	}
	return tx.Commit()
}

// Init switches to WAL and creates the current schema on a new DB. An existing DB must
// already be on it: only Upgrade migrates. Init and Upgrade are the only places
// that change the journal mode or the schema.
func (d *DB) Init() error {
	if _, err := d.Exec("PRAGMA journal_mode = WAL"); err != nil {
		return err
	}
	exists, err := d.hasTable("schema_version")
	if err != nil {
		return err
	}
	if !exists {
		return d.execTx(schema)
	}
	return d.RequireSchema()
}

// Upgrade migrates a v6, v7, v8 or v9 DB to the current schema, announced on w
// since rebuilding the FTS indexes takes minutes on millions of instances. A DB
// already current is left alone.
func (d *DB) Upgrade(w io.Writer) error {
	if err := d.RequireSchema(); err != ErrOutdatedSchema {
		return err
	}
	if _, err := d.Exec("PRAGMA journal_mode = WAL"); err != nil {
		return err
	}
	v, err := d.version()
	if err != nil {
		return err
	}
	if v.Int64 == 6 {
		fmt.Fprintln(w, "Migrating database to schema v7 (full-text search)...")
		if err := d.execTx(migrateV6); err != nil {
			return err
		}
		v.Int64 = 7
	}
	switch v.Int64 {
	case 7:
		fmt.Fprintln(w, "Migrating database to schema v9 (files indexed before hashing, fast hashes)...")
		if err := d.migrateV7(); err != nil {
			return err
		}
	case 8:
		fmt.Fprintln(w, "Migrating database to schema v9 (fast hashes)...")
		if err := d.execTx(migrateV8); err != nil {
			return err
		}
	case 9:
	default:
		return fmt.Errorf("Database schema v%d is too old to upgrade, expected at least v6", v.Int64)
	}
	fmt.Fprintln(w, "Migrating database to schema v10 (redundant indexes dropped)...")
	if err := d.execTx(migrateV9); err != nil {
		return err
	}
	fmt.Fprintln(w, "`fileindex vacuum` returns the freed space to the disk; until then new data reuses it.")
	return nil
}

func (d *DB) execTx(ddl string) error {
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

var ErrOutdatedSchema = errors.New("database schema is outdated, run `fileindex upgrade-db`")

var ErrNewerSchema = errors.New("database schema is newer than this fileindex supports, upgrade fileindex")

// RequireSchema fails early on a DB that `init` never ran on, one still on
// an older schema (`upgrade-db` migrates it), or one on a newer schema than this
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
	Unhashed                                                  int64 // instances waiting to be hashed
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
		{"SELECT COUNT(*) FROM instance WHERE file_id IS NULL", &s.Unhashed},
		{"SELECT COALESCE(SUM(filesize), 0) FROM file", &s.TotalSize},
		{`SELECT COUNT(*) FROM (SELECT file_id FROM instance WHERE file_id IS NOT NULL GROUP BY file_id HAVING COUNT(*) > 1) c
JOIN file f ON f.id = c.file_id WHERE f.filesize > 0`, &s.DuplicateFiles},
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
	res, err := d.Exec("DELETE FROM file WHERE NOT EXISTS (SELECT 1 FROM instance WHERE file_id = file.id)")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
