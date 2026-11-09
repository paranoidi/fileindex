// Package db holds the schema v13 SQLite index and every query on it.
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

const SchemaVersion = 13

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
    sha256_sample TEXT,
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
` + contentTagSchema + `
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
CREATE INDEX idx_file_sha256_sample ON file(sha256_sample);
CREATE INDEX idx_file_size ON file(filesize);
CREATE INDEX idx_dir_tag_dir_id ON dir_tag(dir_id);
` + instanceIndexes + sampleTagSchema + dirPrintSchema + ftsSchema + `INSERT INTO schema_version (version) VALUES (13);
`

// contentTagSchema holds file tags. A content tag is keyed by sha256, not file.id:
// file rows are swept as orphans, and the tag must outlive that, so a move or a
// rescan finds it again. gone_at is set while no file row has the content, by the
// sites that delete and insert file rows (markGone, CommitScanBatch): a trigger on
// file nearly doubled the cost of a scan batch, even one that never fired an UPDATE.
// A pending tag holds the tag of an instance with no content yet, until its hash
// lands (CommitScanBatch). pending_tag has a rowid because instance deletes cascade
// into it, and a cascade into a WITHOUT ROWID table made pruning 12% slower.
const contentTagSchema = `CREATE TABLE content_tag (
    tag_id INTEGER NOT NULL REFERENCES tag(id) ON DELETE CASCADE,
    sha256 TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    gone_at TIMESTAMP,
    PRIMARY KEY (sha256, tag_id)
) WITHOUT ROWID;
CREATE TABLE pending_tag (
    tag_id INTEGER NOT NULL REFERENCES tag(id) ON DELETE CASCADE,
    instance_id INTEGER NOT NULL REFERENCES instance(id) ON DELETE CASCADE,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(instance_id, tag_id)
);
`

// sampleTagSchema holds the pending tags of deleted instances that had a sample
// hash, keyed by size and sample, until an instance or file with the same pair
// takes them back (CommitScanBatch) or TagGC ends them: the file may have moved
// before its full hash landed. kept_at is when its instance went.
const sampleTagSchema = `CREATE TABLE sample_tag (
    tag_id INTEGER NOT NULL REFERENCES tag(id) ON DELETE CASCADE,
    size INTEGER NOT NULL,
    sha256_sample TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    kept_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (size, sha256_sample, tag_id)
) WITHOUT ROWID;
`

// dirPrintSchema holds the prints of deleted tagged Dirs: their tags, totals over
// the subtree, and their printFiles largest files with path under the Dir, size
// and hashes, until ReattachDirTags finds the Dir again or TagGC ends them.
const dirPrintSchema = `CREATE TABLE dir_print (
    id INTEGER PRIMARY KEY,
    files INTEGER NOT NULL,
    bytes INTEGER NOT NULL,
    kept_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE dir_print_tag (
    print_id INTEGER NOT NULL REFERENCES dir_print(id) ON DELETE CASCADE,
    tag_id INTEGER NOT NULL REFERENCES tag(id) ON DELETE CASCADE,
    created_at TIMESTAMP,
    PRIMARY KEY (print_id, tag_id)
) WITHOUT ROWID;
CREATE TABLE dir_print_entry (
    print_id INTEGER NOT NULL REFERENCES dir_print(id) ON DELETE CASCADE,
    rel TEXT NOT NULL,
    size INTEGER NOT NULL,
    sha256 TEXT,
    sha256_sample TEXT,
    PRIMARY KEY (print_id, rel)
) WITHOUT ROWID;
`

// instanceDDL is the instance table under the given name. file_id is NULL while
// the file is not hashed yet (it is already listed and found by name); size is
// the size seen at the last walk or hash, so listings never need the file row.
// sha256_sample is set on a fast-hashed instance, which has no file row to hold it.
func instanceDDL(name string) string {
	return `CREATE TABLE ` + name + ` (
    id INTEGER PRIMARY KEY,
    file_id INTEGER,
    dir_id INTEGER NOT NULL,
    filename TEXT NOT NULL,
    filename_lower TEXT NOT NULL,
    size INTEGER NOT NULL,
    mtime TIMESTAMP,
    sha256_sample TEXT,
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

// fastIndex serves PotentialDuplicates and the sample tag lookups. It is
// partial, so instances without a fast hash cost nothing.
const fastIndex = `CREATE INDEX idx_instance_fast ON instance(size, sha256_sample) WHERE file_id IS NULL AND sha256_sample IS NOT NULL;
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
const migrateV8 = `ALTER TABLE instance ADD COLUMN sha256_sample TEXT;
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

// migrateV10 moves file tags from instances to content: hashed instances give
// content tags, unhashed ones pending tags. Every empty file is the same content,
// so a tag on one is dropped, its path printed first.
func (d *DB) migrateV10(w io.Writer) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT i.dir_id, i.filename, t.name FROM file_tag ft
JOIN instance i ON i.id = ft.instance_id JOIN tag t ON t.id = ft.tag_id
LEFT JOIN file f ON f.id = i.file_id WHERE COALESCE(f.filesize, i.size) = 0 ORDER BY i.dir_id, i.filename, t.name`)
	if err != nil {
		return err
	}
	type dropped struct {
		dir       int64
		name, tag string
	}
	var empties []dropped
	for rows.Next() {
		var e dropped
		if err := rows.Scan(&e.dir, &e.name, &e.tag); err != nil {
			rows.Close()
			return err
		}
		empties = append(empties, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	paths := dirPaths{}
	for _, e := range empties {
		p, err := paths.file(tx, e.dir, e.name)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "Dropping tag #%s from empty file: %s\n", e.tag, p)
	}
	if _, err := tx.Exec(contentTagSchema + `
INSERT OR IGNORE INTO content_tag (tag_id, sha256, created_at)
SELECT ft.tag_id, f.sha256, MIN(ft.created_at) FROM file_tag ft
JOIN instance i ON i.id = ft.instance_id JOIN file f ON f.id = i.file_id
WHERE f.filesize > 0 GROUP BY ft.tag_id, f.sha256;
INSERT INTO pending_tag (tag_id, instance_id, created_at)
SELECT ft.tag_id, i.id, ft.created_at FROM file_tag ft
JOIN instance i ON i.id = ft.instance_id WHERE i.file_id IS NULL AND i.size > 0;`); err != nil {
		return err
	}
	var gained int64
	if err := tx.QueryRow(`SELECT COUNT(*) FROM content_tag ct CROSS JOIN file f ON f.sha256 = ct.sha256
CROSS JOIN instance i ON i.file_id = f.id
WHERE NOT EXISTS (SELECT 1 FROM file_tag ft WHERE ft.tag_id = ct.tag_id AND ft.instance_id = i.id)`).Scan(&gained); err != nil {
		return err
	}
	if _, err := tx.Exec("DROP TABLE file_tag; INSERT INTO schema_version (version) VALUES (11);"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Fprintf(w, "File tags now belong to content and apply to all copies: %d copies gained a tag.\n", gained)
	return nil
}

// migrateV11 replaces the first-8-KB hash with the sample hash: the column is
// renamed where it still has its old name, and every stored value is cleared, as
// none of them is a sample. Fast-hashed instances become plain unhashed ones.
func (d *DB) migrateV11() error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"file", "instance"} {
		var old bool
		if err := tx.QueryRow("SELECT EXISTS (SELECT 1 FROM pragma_table_info(?) WHERE name = 'sha256_8kb')", table).Scan(&old); err != nil {
			return err
		}
		if old {
			if _, err := tx.Exec("ALTER TABLE " + table + " RENAME COLUMN sha256_8kb TO sha256_sample"); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`DROP INDEX IF EXISTS idx_file_sha256_8kb;
CREATE INDEX IF NOT EXISTS idx_file_sha256_sample ON file(sha256_sample);
UPDATE file SET sha256_sample = NULL WHERE sha256_sample IS NOT NULL;
UPDATE instance SET sha256_sample = NULL WHERE sha256_sample IS NOT NULL;
` + sampleTagSchema + `INSERT INTO schema_version (version) VALUES (12);`); err != nil {
		return err
	}
	return tx.Commit()
}

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

// Upgrade migrates a v6 to v12 DB to the current schema, announced on w
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
	if v.Int64 < 10 {
		if err := d.upgradeToV10(w, v.Int64); err != nil {
			return err
		}
	}
	if v.Int64 < 11 {
		fmt.Fprintln(w, "Migrating database to schema v11 (tags follow content)...")
		if err := d.migrateV10(w); err != nil {
			return err
		}
	}
	if v.Int64 < 12 {
		fmt.Fprintln(w, "Migrating database to schema v12 (sample hashes replace first-8-KB hashes)...")
		if err := d.migrateV11(); err != nil {
			return err
		}
		fmt.Fprintln(w, "Fast hashes were cleared: `scan --fast` or a full scan hashes those files again.")
	}
	fmt.Fprintln(w, "Migrating database to schema v13 (directory tags kept by their files)...")
	return d.execTx(dirPrintSchema + "INSERT INTO schema_version (version) VALUES (13);")
}

// upgradeToV10 runs the migrations up to v10 from a v6, v7, v8 or v9 DB.
func (d *DB) upgradeToV10(w io.Writer, v int64) error {
	if v == 6 {
		fmt.Fprintln(w, "Migrating database to schema v7 (full-text search)...")
		if err := d.execTx(migrateV6); err != nil {
			return err
		}
		v = 7
	}
	switch v {
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
		return fmt.Errorf("Database schema v%d is too old to upgrade, expected at least v6", v)
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
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec("DELETE FROM file WHERE NOT EXISTS (SELECT 1 FROM instance WHERE file_id = file.id)")
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := markGone(tx, n); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// markGone stamps the content tags whose content lost its last file row, after
// deleted file rows were deleted. It looks at each content tag once, so it suits
// sweeps; RemoveInstance stamps its one sha256 directly.
func markGone(q Querier, deleted int64) error {
	if deleted == 0 {
		return nil
	}
	_, err := q.Exec(`UPDATE content_tag SET gone_at = CURRENT_TIMESTAMP WHERE gone_at IS NULL
AND NOT EXISTS (SELECT 1 FROM file f WHERE f.sha256 = content_tag.sha256)`)
	return err
}
