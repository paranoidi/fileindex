package db_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/paranoidi/fileindex/db"
	"github.com/paranoidi/fileindex/internal/dbtest"
)

func TestPathParts(t *testing.T) {
	for in, want := range map[string][]string{
		"/a/b":      {"/", "a", "b"},
		"/a/b/":     {"/", "a", "b"},
		"a//b/./c":  {"a", "b", "c"},
		"a/../b":    {"a", "..", "b"},
		"//x/y":     {"//", "x", "y"},
		"///x":      {"/", "x"},
		"/":         {"/"},
		"photos/24": {"photos", "24"},
	} {
		if got := db.PathParts(in); !reflect.DeepEqual(got, want) {
			t.Errorf("PathParts(%q) = %q, want %q", in, got, want)
		}
	}
	if got := db.JoinParts([]string{"/", "m", "a_b"}); got != "/m/a_b" {
		t.Errorf("JoinParts = %q", got)
	}
}

func TestLower(t *testing.T) {
	if got := db.Lower("İSTANBUL Ä"); got != "i̇stanbul ä" {
		t.Errorf("got %q", got)
	}
}

func TestParseSize(t *testing.T) {
	ok := map[string]int64{
		"100": 100, "1024": 1024, "100B": 100, "100b": 100,
		"10K": 10240, "10k": 10240, "10KiB": 10240, "10kib": 10240, "10KB": 10000, "10kb": 10000,
		"5M": 5 << 20, "5MiB": 5 << 20, "5MB": 5e6, "1G": 1 << 30, "1GB": 1e9, "2T": 2 << 40, "2TB": 2e12,
		"1.5K": 1536, "2.5MB": 2500000, "0.5G": 536870912, " 10K ": 10240, " 5MB ": 5000000,
		"10 KB": 10000, ".5K": 512, "0.1KB": 100,
	}
	for in, want := range ok {
		if got, err := db.ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	bad := map[string]string{
		"":      "Invalid size format: ",
		"abc":   "Invalid size format: ABC",
		"KB":    "Invalid size format: KB",
		"-5":    "Invalid size format: -5",
		"10XB":  "Unknown size unit: XB in 10XB",
		"1e3":   "Unknown size unit: E3 in 1E3",
		"1.2.3": "Invalid numeric value in size: 1.2.3",
		".":     "Invalid numeric value in size: .",
	}
	for in, want := range bad {
		if _, err := db.ParseSize(in); err == nil || err.Error() != want {
			t.Errorf("ParseSize(%q) err = %v, want %q", in, err, want)
		}
	}
}

func TestFormatSize(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0 bytes", 100: "100 bytes", 1023: "1023 bytes", 1024: "1.00 KiB", 10240: "10.00 KiB",
		1 << 20: "1.00 MiB", 1 << 30: "1.00 GiB", 3 << 40: "3.00 TiB", 1048575: "1024.00 KiB",
		1500 << 40: "1500.00 TiB", -5: "-5 bytes",
	} {
		if got := db.FormatSize(in); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestResolve(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	os.Mkdir(filepath.Join(dir, "real"), 0o755)
	os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "link"))
	for in, want := range map[string]string{
		filepath.Join(dir, "link", "missing", "x"):    filepath.Join(dir, "real", "missing", "x"),
		filepath.Join(dir, "real", "..", "link"):      filepath.Join(dir, "real"),
		filepath.Join(dir, "gone", "..", "real", "."): filepath.Join(dir, "real"),
	} {
		if got, err := db.Resolve(in); err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestInitSchema(t *testing.T) {
	d := dbtest.New(t)
	var n int
	d.QueryRow("SELECT COUNT(*) FROM sqlite_master").Scan(&n)
	if n != 44 { // 7 tables + 19 indexes + 6 autoindexes + 2 FTS tables, their 7 shadow tables, 3 triggers
		t.Errorf("sqlite_master rows = %d, want 44", n)
	}
	var mode string
	d.QueryRow("PRAGMA journal_mode").Scan(&mode)
	if mode != "wal" {
		t.Errorf("journal_mode = %q", mode)
	}
	if err := d.Init(); err != nil {
		t.Errorf("second Init: %v", err)
	}
}

func TestOpenKeepsJournalMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.db")
	d := dbtest.Init(t, p)
	d.Exec("PRAGMA journal_mode = DELETE")
	d.Close()
	d2, err := db.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if _, err := d2.Stats(); err != nil {
		t.Fatal(err)
	}
	var mode string
	d2.QueryRow("PRAGMA journal_mode").Scan(&mode)
	if mode != "delete" {
		t.Errorf("journal_mode = %q after Open", mode)
	}
}

func TestRequireSchema(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.RequireSchema(); err != db.ErrNotInitialised {
		t.Errorf("got %v", err)
	}
	if err := d.Init(); err != nil {
		t.Fatal(err)
	}
	if err := d.RequireSchema(); err != nil {
		t.Errorf("got %v", err)
	}
}

func TestDirs(t *testing.T) {
	d := dbtest.New(t)
	photos, err := db.GetOrCreateDir(d, nil, "photos")
	if err != nil {
		t.Fatal(err)
	}
	y2024, _ := db.GetOrCreateDir(d, &photos, "2024")
	if y2024 == photos {
		t.Fatal("child id equals parent id")
	}
	if again, _ := db.GetOrCreateDir(d, nil, "photos"); again != photos {
		t.Errorf("GetOrCreateDir not idempotent: %d vs %d", again, photos)
	}

	if id, found, _ := db.DirByPath(d, "photos/2024"); !found || *id != y2024 {
		t.Errorf("DirByPath(photos/2024) = %v, %v", id, found)
	}
	if _, found, _ := db.DirByPath(d, "photos/2025"); found {
		t.Error("DirByPath(photos/2025) found")
	}
	if id, found, _ := db.DirByPath(d, "."); found || id != nil {
		t.Error("DirByPath(.) should be the virtual root")
	}

	if p, _ := db.BuildDirPath(d, &y2024); p != "photos/2024" {
		t.Errorf("BuildDirPath = %q", p)
	}
	if p, _ := db.BuildDirPath(d, nil); p != "" {
		t.Errorf("BuildDirPath(nil) = %q", p)
	}
	missing := int64(999)
	if _, err := db.BuildDirPath(d, &missing); err == nil || err.Error() != "Directory id 999 not found in database" {
		t.Errorf("BuildDirPath(999) err = %v", err)
	}

	abs, _ := db.GetOrCreatePath(d, "/m/a_b")
	if p, _ := db.BuildFilePath(d, abs, "x.jpg"); p != "/m/a_b/x.jpg" {
		t.Errorf("BuildFilePath = %q", p)
	}
	if p, _ := db.BuildFilePath(d, nil, "x.jpg"); p != "x.jpg" {
		t.Errorf("BuildFilePath(nil) = %q", p)
	}

	ids, _ := db.TreeIDs(d, &photos, true)
	if !reflect.DeepEqual(ids, []int64{photos, y2024}) {
		t.Errorf("TreeIDs = %v", ids)
	}
	ids, _ = db.TreeIDs(d, &photos, false)
	if !reflect.DeepEqual(ids, []int64{y2024}) {
		t.Errorf("TreeIDs excl. self = %v", ids)
	}
	if ids, _ := db.TreeIDs(d, nil, true); ids != nil {
		t.Errorf("TreeIDs(nil) = %v", ids)
	}
}

func TestDeleteOrphanedFiles(t *testing.T) {
	d := dbtest.New(t)
	dbtest.AddFile(t, d, "/m/keep/x.jpg", "x")
	gone := dbtest.AddFile(t, d, "/m/gone/y.jpg", "y")
	d.Exec("DELETE FROM instance WHERE id = ?", gone)
	n, err := d.DeleteOrphanedFiles()
	if err != nil || n != 1 {
		t.Errorf("DeleteOrphanedFiles = %d, %v", n, err)
	}
	if _, found, _ := db.DirByPath(d, "/m/gone"); !found {
		t.Error("/m/gone was deleted with its file")
	}
}

func TestRecordDirs(t *testing.T) {
	d := dbtest.New(t)
	if err := d.RecordDirs([]string{"/m", "/m/empty", "/m/empty/deeper", "/other/x"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/m/empty/deeper", "/other/x"} {
		if _, found, _ := db.DirByPath(d, p); !found {
			t.Errorf("%s not recorded", p)
		}
	}
	// recording again finds the same rows
	before, _ := d.Stats()
	if err := d.RecordDirs([]string{"/m", "/m/empty"}); err != nil {
		t.Fatal(err)
	}
	if after, _ := d.Stats(); after.Dirs != before.Dirs {
		t.Errorf("dirs %d -> %d", before.Dirs, after.Dirs)
	}
}

func TestForeignKeysOn(t *testing.T) {
	d := dbtest.New(t)
	dbtest.AddFile(t, d, "/m/x.jpg", "x")
	if _, err := d.Exec("DELETE FROM dir"); err != nil {
		t.Fatal(err)
	}
	var n int
	d.QueryRow("SELECT COUNT(*) FROM instance").Scan(&n)
	if n != 0 {
		t.Errorf("instances left after deleting dirs: %d (cascade off?)", n)
	}
}

func TestFormatMtime(t *testing.T) {
	loc := time.Local
	if got := db.FormatMtime(time.Date(2024, 1, 2, 3, 4, 5, 0, loc)); got != "2024-01-02 03:04:05" {
		t.Errorf("whole second: %q", got)
	}
	ts := time.Date(2025, 10, 30, 22, 34, 21, 81074999, loc)
	if got := db.FormatMtime(ts); got != "2025-10-30 22:34:21.081075" { // rounded half-to-even
		t.Errorf("fraction: %q", got)
	}
	if got := db.FormatMtime(time.Date(2025, 10, 30, 22, 34, 21, 81074400, loc)); got != "2025-10-30 22:34:21.081074" {
		t.Errorf("round down: %q", got)
	}
	if !db.SameMtime("2025-10-30 22:34:21.081075", ts) || !db.SameMtime("2025-10-30 22:34:21.081074", ts) {
		t.Error("SameMtime should allow 1µs")
	}
	if db.SameMtime("2025-10-30 22:34:21.081077", ts) || db.SameMtime("2025-10-30 22:34:21", ts) {
		t.Error("SameMtime too lenient")
	}
}

// Prune commits every PruneBatch deletions; with a batch of 1 every commit
// boundary is crossed, and the result must match one big transaction.
func TestPruneDirBatches(t *testing.T) {
	defer func(n int64) { *db.PruneBatch = n }(*db.PruneBatch)
	type result struct {
		out                  string
		files, dirs          int64
		instances, dirsTotal int
	}
	prune := func(batch int64) result {
		*db.PruneBatch = batch
		d := dbtest.New(t)
		root := t.TempDir()
		for _, p := range []string{"keep.jpg", "gone1.jpg", "gone2.jpg", "sub/x.jpg", "sub/deep/y.jpg", "empty/z.jpg"} {
			full := filepath.Join(root, p)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(p), 0o644); err != nil {
				t.Fatal(err)
			}
			dbtest.AddFile(t, d, full, p)
		}
		for _, p := range []string{"gone1.jpg", "gone2.jpg", "sub", "empty/z.jpg"} {
			if err := os.RemoveAll(filepath.Join(root, p)); err != nil {
				t.Fatal(err)
			}
		}
		id, _, err := db.DirByPath(d, root)
		if err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		var r result
		if r.files, r.dirs, err = d.PruneDir(context.Background(), root, *id, nil, logTo(&out)); err != nil {
			t.Fatal(err)
		}
		r.out = strings.ReplaceAll(out.String(), root, "ROOT")
		d.QueryRow("SELECT COUNT(*) FROM instance").Scan(&r.instances)
		d.QueryRow("SELECT COUNT(*) FROM dir").Scan(&r.dirsTotal)
		return r
	}
	one, big := prune(1), prune(1000)
	if one != big {
		t.Errorf("batch 1: %+v\nbatch 1000: %+v", one, big)
	}
	if one.files != 5 || one.instances != 1 {
		t.Errorf("removed %d files, %d instances left", one.files, one.instances)
	}
}

// Prune trusts one ReadDir per dir; symlinked dirs, dirs replaced by files and
// unlistable dirs must behave as they did with a stat per record.
func TestPruneDirListing(t *testing.T) {
	d := dbtest.New(t)
	root := t.TempDir()
	add := func(p string) string {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
		dbtest.AddFile(t, d, full, p)
		return full
	}
	add("keep.jpg")
	add("real/a.jpg")
	add("link/a.jpg") // recorded under link/, which becomes a symlink to real/
	add("nowfile/b.jpg")
	add("real/gone 1.jpg")
	add("real/gone 2.jpg")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.RemoveAll(filepath.Join(root, "link")))
	must(os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "link")))
	must(os.RemoveAll(filepath.Join(root, "nowfile")))
	must(os.WriteFile(filepath.Join(root, "nowfile"), []byte("x"), 0o644))
	must(os.Remove(filepath.Join(root, "real", "gone 1.jpg")))
	must(os.Remove(filepath.Join(root, "real", "gone 2.jpg")))

	id, _, err := db.DirByPath(d, root)
	must(err)
	var out strings.Builder
	files, dirs, err := d.PruneDir(context.Background(), root, *id, nil, logTo(&out))
	must(err)
	if files != 3 || dirs != 1 || !strings.Contains(out.String(), "nowfile") ||
		!strings.Contains(out.String(), "gone 1.jpg") || !strings.Contains(out.String(), "gone 2.jpg") {
		t.Errorf("removed %d files, %d dirs:\n%s", files, dirs, out.String())
	}
	ftsIntegrity(t, d, "prune")

	if os.Geteuid() == 0 {
		return // root lists anything
	}
	locked := filepath.Dir(add("locked/c.jpg"))
	must(os.Chmod(locked, 0))
	defer os.Chmod(locked, 0o755)
	if _, _, err := d.PruneDir(context.Background(), root, *id, nil, logTo(&out)); err == nil {
		t.Error("unreadable dir: want error, as with per-file stat")
	}
}

// ftsIntegrity fails the test unless both FTS indexes match instance.
func ftsIntegrity(t *testing.T, d *db.DB, when string) {
	t.Helper()
	for _, tbl := range []string{"instance_trigram", "instance_words"} {
		if _, err := d.Exec("INSERT INTO " + tbl + "(" + tbl + ", rank) VALUES ('integrity-check', 1)"); err != nil {
			t.Errorf("%s after %s: %v", tbl, when, err)
		}
	}
}

// pathsOf returns a func that joins find results' paths, failing t on error.
func pathsOf(t *testing.T) func([]db.Found, error) string {
	return func(found []db.Found, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var s []string
		for _, f := range found {
			s = append(s, f.Path)
		}
		return strings.Join(s, " ")
	}
}

func TestFTSStaysInSync(t *testing.T) {
	paths := pathsOf(t)
	d := dbtest.New(t)
	root, _ := db.GetOrCreatePath(d, "/m")
	a := dbtest.AddFile(t, d, "/m/holiday_beach.jpg", "a")
	dbtest.AddFile(t, d, "/m/sub/Holiday-Snow.png", "b")
	ftsIntegrity(t, d, "insert")
	if got := paths(d.FindByWords("holiday", *root)); got != "/m/sub/Holiday-Snow.png /m/holiday_beach.jpg" {
		t.Errorf("words: %q", got)
	}

	if err := d.MoveInstance(a, "/m", "winter_trip.jpg"); err != nil {
		t.Fatal(err)
	}
	ftsIntegrity(t, d, "rename")
	if got := paths(d.FindByWords("holiday", *root)); got != "/m/sub/Holiday-Snow.png" {
		t.Errorf("words after rename: %q", got)
	}
	if got := paths(d.FindByName("*trip*", *root, true, false)); got != "/m/winter_trip.jpg" {
		t.Errorf("trigram after rename: %q", got)
	}

	if _, err := d.Exec("DELETE FROM dir WHERE name = 'sub'"); err != nil { // cascades to the instance
		t.Fatal(err)
	}
	ftsIntegrity(t, d, "cascade delete")
	if got := paths(d.FindByWords("snow", *root)); got != "" {
		t.Errorf("words after delete: %q", got)
	}
}

func TestMigrateV6(t *testing.T) {
	paths := pathsOf(t)
	d := dbtest.New(t)
	root, _ := db.GetOrCreatePath(d, "/m")
	dbtest.AddFile(t, d, "/m/old_photo.jpg", "a")
	// turn it back into a v6 DB: no FTS objects, version 6
	for _, q := range []string{
		"DROP TRIGGER instance_fts_ai", "DROP TRIGGER instance_fts_ad", "DROP TRIGGER instance_fts_au",
		"DROP TABLE instance_trigram", "DROP TABLE instance_words",
		"DELETE FROM schema_version", "INSERT INTO schema_version (version) VALUES (6)",
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.RequireSchema(); err != db.ErrOutdatedSchema {
		t.Errorf("RequireSchema on v6: %v", err)
	}

	var out strings.Builder
	if err := d.Upgrade(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Migrating database to schema v7") {
		t.Errorf("no migration notice: %q", out.String())
	}
	if err := d.RequireSchema(); err != nil {
		t.Errorf("RequireSchema after migration: %v", err)
	}
	ftsIntegrity(t, d, "migration")
	if got := paths(d.FindByWords("photo", *root)); got != "/m/old_photo.jpg" {
		t.Errorf("words after migration: %q", got)
	}
	if got := paths(d.FindByName("old*", *root, true, false)); got != "/m/old_photo.jpg" {
		t.Errorf("find after migration: %q", got)
	}
	dbtest.AddFile(t, d, "/m/new_photo.jpg", "b")
	ftsIntegrity(t, d, "insert after migration")
}

// logTo collects PruneDir's log lines as the CLI prints them.
func logTo(b *strings.Builder) func(string) {
	return func(m string) { b.WriteString(m + "\n") }
}

func TestFileInfoMtime(t *testing.T) {
	d := dbtest.New(t)
	dbtest.AddFile(t, d, "/m/garden/lantern.jpg", "lantern")
	r, found, err := d.FileInfo("/m/garden/lantern.jpg")
	if err != nil || !found || r.Mtime != dbtest.Mtime || r.Size != 7 {
		t.Errorf("FileInfo = %+v, %v, %v", r, found, err)
	}
}

func TestRemoveInstance(t *testing.T) {
	d := dbtest.New(t)
	dbtest.AddFile(t, d, "/m/river/pebble.jpg", "pebble")
	dbtest.AddFile(t, d, "/m/river/bank/willow.jpg", "willow")
	dbtest.AddFile(t, d, "/m/meadow/clover.jpg", "clover")
	dbtest.AddFile(t, d, "/m/meadow/clover-copy.jpg", "clover") // same file row
	dbtest.AddFile(t, d, "/m/hill/top/cloud.jpg", "cloud")

	count := func(table string) (n int) {
		d.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n)
		return
	}
	dirExists := func(p string) bool {
		_, found, _ := db.DirByPath(d, p)
		return found
	}

	// File row still referenced by another instance is kept.
	files := count("file")
	r, err := d.RemoveInstance("/m/meadow/clover.jpg")
	if err != nil || r != (db.RemovedTree{Instances: 1}) || count("file") != files {
		t.Errorf("shared: %+v, %v, files %d", r, err, count("file"))
	}

	// Orphaned file row goes; the dirs left empty stay (they are still on disk).
	r, err = d.RemoveInstance("/m/hill/top/cloud.jpg")
	if err != nil || r != (db.RemovedTree{Instances: 1, Files: 1}) {
		t.Errorf("orphan: %+v, %v", r, err)
	}
	if !dirExists("/m/hill/top") {
		t.Error("/m/hill/top removed with cloud.jpg")
	}

	// Dir with another file or a subdir is kept.
	r, err = d.RemoveInstance("/m/river/pebble.jpg")
	if err != nil || r != (db.RemovedTree{Instances: 1, Files: 1}) || !dirExists("/m/river") {
		t.Errorf("subdir kept: %+v, %v", r, err)
	}
	r, err = d.RemoveInstance("/m/meadow/clover-copy.jpg")
	if err != nil || r.Files != 1 || r.Dirs != 0 || !dirExists("/m/meadow") {
		t.Errorf("last copy: %+v, %v", r, err)
	}

	for _, p := range []string{"/m/river/nothing.jpg", "/nowhere/nothing.jpg"} {
		if _, err := d.RemoveInstance(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("RemoveInstance(%q) = %v, want ErrNotExist", p, err)
		}
	}
}

// CommitScanBatch inserts new instances in one statement; counts, a path seen
// twice in a batch, and the FTS indexes must come out as with per-row inserts.
func TestCommitScanBatch(t *testing.T) {
	paths := pathsOf(t)
	d := dbtest.New(t)
	f := func(dir, name, hash string) db.ScannedFile {
		return db.ScannedFile{Dir: dir, Name: name, Size: 1, Mtime: time.Unix(1700000000, 0),
			MD5: hash[:4], SHA256: hash, SHA256_8KB: hash}
	}
	res, err := d.CommitScanBatch([]db.ScannedFile{
		f("/m", "holiday_beach.jpg", "aaaa1"), f("/m/sub", "snow day.png", "bbbb1"), f("/m", "holiday_beach.jpg", "cccc1"),
	})
	if err != nil || res.Added != 2 || res.Updated != 1 || len(res.Failed) != 0 {
		t.Fatalf("first batch: %+v, %v", res, err)
	}
	if res, err = d.CommitScanBatch([]db.ScannedFile{f("/m/sub", "snow day.png", "dddd1"), f("/m", "new.txt", "eeee1")}); err != nil || res.Added != 1 || res.Updated != 1 {
		t.Fatalf("second batch: %+v, %v", res, err)
	}
	ftsIntegrity(t, d, "scan batches")
	root, _, _ := db.DirByPath(d, "/m")
	if got := paths(d.FindByWords("holiday OR snow", *root)); got != "/m/holiday_beach.jpg /m/sub/snow day.png" {
		t.Errorf("words: %q", got)
	}
	var sha string
	if err := d.QueryRow("SELECT f.sha256 FROM instance i JOIN file f ON f.id = i.file_id WHERE i.filename = 'holiday_beach.jpg'").Scan(&sha); err != nil || sha != "cccc1" {
		t.Errorf("duplicate in batch: last should win, got %q, %v", sha, err)
	}
}

func TestFilesUnderMtime(t *testing.T) {
	d := dbtest.New(t)
	dbtest.AddFile(t, d, "/m/lantern/wick.jpg", "w")
	id, _, _ := db.DirByPath(d, "/m/lantern")
	got, err := d.FilesUnder(*id, true)
	if err != nil || len(got) != 1 || got[0].Mtime != dbtest.Mtime {
		t.Fatalf("FilesUnder = %+v, %v", got, err)
	}
}

// v7DDL is the part of schema v7 that migration to v8 touches or must keep.
const v7DDL = `
CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE dir (id INTEGER PRIMARY KEY, parent_id INTEGER, name TEXT NOT NULL, name_lower TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, last_seen TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (parent_id) REFERENCES dir(id) ON DELETE CASCADE, UNIQUE(parent_id, name));
CREATE TABLE file (id INTEGER PRIMARY KEY, md5 TEXT NOT NULL, sha256 TEXT NOT NULL, sha256_noexif_jpeg TEXT,
    sha256_8kb TEXT, filesize INTEGER NOT NULL, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_seen TIMESTAMP DEFAULT CURRENT_TIMESTAMP, UNIQUE(md5));
CREATE TABLE instance (id INTEGER PRIMARY KEY, file_id INTEGER NOT NULL, dir_id INTEGER NOT NULL,
    filename TEXT NOT NULL, filename_lower TEXT NOT NULL, mtime TIMESTAMP,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, last_seen TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (file_id) REFERENCES file(id) ON DELETE CASCADE,
    FOREIGN KEY (dir_id) REFERENCES dir(id) ON DELETE CASCADE, UNIQUE(dir_id, filename));
CREATE TABLE tag (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_seen TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE file_tag (tag_id INTEGER NOT NULL, instance_id INTEGER NOT NULL, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (tag_id) REFERENCES tag(id) ON DELETE CASCADE,
    FOREIGN KEY (instance_id) REFERENCES instance(id) ON DELETE CASCADE, UNIQUE(tag_id, instance_id));
CREATE TABLE dir_tag (tag_id INTEGER NOT NULL, dir_id INTEGER, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (tag_id) REFERENCES tag(id) ON DELETE CASCADE,
    FOREIGN KEY (dir_id) REFERENCES dir(id) ON DELETE CASCADE, UNIQUE(tag_id, dir_id));
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
INSERT INTO schema_version (version) VALUES (7);
INSERT INTO dir (id, parent_id, name, name_lower) VALUES (1, NULL, '/', '/'), (2, 1, 'orchard', 'orchard');
INSERT INTO file (id, md5, sha256, filesize) VALUES (1, 'm1', 's1', 111), (2, 'm2', 's2', 222);
INSERT INTO instance (id, file_id, dir_id, filename, filename_lower, mtime) VALUES
    (5, 1, 2, 'Lantern.txt', 'lantern.txt', '2024-01-02 03:04:05'), (9, 2, 2, 'meadow.txt', 'meadow.txt', NULL);
INSERT INTO tag (id, name) VALUES (1, 'amber');
INSERT INTO file_tag (tag_id, instance_id) VALUES (1, 9);
INSERT INTO instance_trigram(instance_trigram) VALUES ('rebuild');
INSERT INTO instance_words(instance_words) VALUES ('rebuild');
`

// Migration to v8 keeps instance ids, tags and FTS, and takes size from the file row.
func TestMigrateV7(t *testing.T) {
	paths := pathsOf(t)
	d, err := db.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.Exec(v7DDL); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := d.Upgrade(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "schema v9") {
		t.Errorf("no migration notice: %q", out.String())
	}
	if err := d.RequireSchema(); err != nil {
		t.Fatal(err)
	}
	var sizes string
	if err := d.QueryRow("SELECT group_concat(id || ':' || size || ':' || file_id, ' ') FROM (SELECT * FROM instance ORDER BY id)").Scan(&sizes); err != nil || sizes != "5:111:1 9:222:2" {
		t.Errorf("instances = %q, %v", sizes, err)
	}
	var tagged int
	if err := d.QueryRow("SELECT COUNT(*) FROM file_tag WHERE instance_id = 9").Scan(&tagged); err != nil || tagged != 1 {
		t.Errorf("file_tag rows = %d, %v", tagged, err)
	}
	ftsIntegrity(t, d, "migration")
	root, _, _ := db.DirByPath(d, "/orchard")
	if got := paths(d.FindByWords("lantern", *root)); got != "/orchard/Lantern.txt" {
		t.Errorf("words after migration: %q", got)
	}
	if got := paths(d.FindByName("*eado*", *root, true, false)); got != "/orchard/meadow.txt" {
		t.Errorf("trigram after migration: %q", got)
	}
	// file_id is nullable now, and the triggers survived
	if _, err := d.Exec("INSERT INTO instance (dir_id, filename, filename_lower, size) VALUES (2, 'orchid.txt', 'orchid.txt', 3)"); err != nil {
		t.Fatal(err)
	}
	ftsIntegrity(t, d, "insert after migration")
	if got := paths(d.FindByWords("orchid", *root)); got != "/orchard/orchid.txt" {
		t.Errorf("words after insert: %q", got)
	}
}

// Unhashed instances are listed, never counted as duplicates, and removable.
func TestUnhashedInstances(t *testing.T) {
	d := dbtest.New(t)
	dbtest.AddFile(t, d, "/m/a.bin", "same")
	dbtest.AddFile(t, d, "/m/b.bin", "same")
	if _, err := d.CommitDiscovered([]db.DiscoveredFile{{Dir: "/m", Name: "c.bin", Size: 4, Mtime: time.Unix(1700000000, 0)}}, []string{"/m"}); err != nil {
		t.Fatal(err)
	}
	id, _, _ := db.DirByPath(d, "/m")
	files, _, err := d.ListDir(*id)
	if err != nil || len(files) != 3 || files[2].Name != "c.bin" || files[2].Hashed || !files[0].Hashed || files[2].Size != 4 {
		t.Fatalf("ListDir = %+v, %v", files, err)
	}
	entries, err := d.FilesUnder(*id, false)
	if err != nil || len(entries) != 3 || entries[2].Hashed || entries[2].Content != "" {
		t.Fatalf("FilesUnder = %+v, %v", entries, err)
	}
	groups, err := d.Duplicates(nil, false)
	if err != nil || len(groups) != 1 || len(groups[0].Locations) != 2 {
		t.Fatalf("Duplicates = %+v, %v", groups, err)
	}
	if n, err := d.CountUnhashed("/m", false); err != nil || n != 1 {
		t.Errorf("CountUnhashed = %d, %v", n, err)
	}
	if rm, err := d.RemoveInstance("/m/c.bin"); err != nil || rm.Instances != 1 || rm.Files != 0 {
		t.Errorf("RemoveInstance = %+v, %v", rm, err)
	}
	// the orphan sweep must not choke on NULL file_ids
	if _, err := d.CommitDiscovered([]db.DiscoveredFile{{Dir: "/m", Name: "d.bin", Size: 1, Mtime: time.Unix(1700000000, 0)}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("DELETE FROM instance WHERE filename IN ('a.bin', 'b.bin')"); err != nil {
		t.Fatal(err)
	}
	if n, err := d.DeleteOrphanedFiles(); err != nil || n != 1 {
		t.Errorf("DeleteOrphanedFiles = %d, %v", n, err)
	}
}

// addFast records a fast-hashed instance under dir.
func addFast(t *testing.T, d *db.DB, dir, name string, size int64, head string) {
	t.Helper()
	mtime := time.Unix(1700000000, 0)
	if _, err := d.CommitDiscovered([]db.DiscoveredFile{{Dir: dir, Name: name, Size: size, Mtime: mtime}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CommitScanBatch([]db.ScannedFile{{Dir: dir, Name: name, Size: size, Mtime: mtime, SHA256_8KB: head}}); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateV8(t *testing.T) {
	d := dbtest.New(t)
	for _, q := range []string{"DROP INDEX idx_instance_fast", "ALTER TABLE instance DROP COLUMN sha256_8kb", "DELETE FROM schema_version", "INSERT INTO schema_version (version) VALUES (8)"} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.RequireSchema(); err != db.ErrOutdatedSchema {
		t.Errorf("RequireSchema on v8: %v", err)
	}
	var out strings.Builder
	if err := d.Upgrade(&out); err != nil || !strings.Contains(out.String(), "schema v9") {
		t.Fatalf("Upgrade: %v, %q", err, out.String())
	}
	if err := d.RequireSchema(); err != nil {
		t.Error(err)
	}
	addFast(t, d, "/m", "a", 9000, "h")
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name = 'idx_instance_fast'").Scan(&n); err != nil || n != 1 {
		t.Errorf("index after migration: %d, %v", n, err)
	}
}

func TestFastHashLifecycle(t *testing.T) {
	d := dbtest.New(t)
	addFast(t, d, "/m", "a", 9000, "h")
	files, err := d.DirFiles("/m")
	if r := files["a"]; err != nil || r.Hashed || !r.Fast {
		t.Fatalf("DirFiles = %+v, %v", files, err)
	}
	if n, err := d.CountUnhashed("/m", true); err != nil || n != 0 {
		t.Errorf("CountUnhashed skipping fast = %d, %v", n, err)
	}
	if n, err := d.CountUnhashed("/m", false); err != nil || n != 1 {
		t.Errorf("CountUnhashed = %d, %v", n, err)
	}
	// a changed file loses its fast hash
	if _, err := d.CommitDiscovered([]db.DiscoveredFile{{Dir: "/m", Name: "a", Size: 9001, Mtime: time.Unix(1700000001, 0)}}, nil); err != nil {
		t.Fatal(err)
	}
	if files, _ := d.DirFiles("/m"); files["a"].Fast {
		t.Error("fast hash survived a change")
	}
	// a full hash replaces it, and a late fast hash can't come back beside it
	addFast(t, d, "/m", "a", 9000, "h")
	full := db.ScannedFile{Dir: "/m", Name: "a", Size: 9000, Mtime: time.Unix(1700000000, 0), MD5: "m", SHA256: "s", SHA256_8KB: "h"}
	if _, err := d.CommitScanBatch([]db.ScannedFile{full}); err != nil {
		t.Fatal(err)
	}
	full.MD5, full.SHA256 = "", ""
	if _, err := d.CommitScanBatch([]db.ScannedFile{full}); err != nil {
		t.Fatal(err)
	}
	if files, _ := d.DirFiles("/m"); !files["a"].Hashed || files["a"].Fast {
		t.Errorf("after full hash: %+v", files["a"])
	}
}

func TestPotentialDuplicates(t *testing.T) {
	d := dbtest.New(t)
	if groups, err := d.PotentialDuplicates(nil, false); err != nil || len(groups) != 0 {
		t.Fatalf("empty: %+v, %v", groups, err)
	}
	head := func(content string) string { // what dbtest.AddFile stores in file.sha256_8kb
		var h string
		if err := d.QueryRow("SELECT sha256_8kb FROM file WHERE filesize = ?", len(content)).Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	dbtest.AddFile(t, d, "/h/hashed1", "aaaa")
	dbtest.AddFile(t, d, "/h/hashed2", "aaaa") // hashed+hashed alone: confirmed, not potential
	dbtest.AddFile(t, d, "/h/other", "bbbbbb")
	addFast(t, d, "/q", "matches-other", 6, head("bbbbbb")) // fast+hashed
	addFast(t, d, "/q", "q1", 9000, "h1")                   // fast+fast
	addFast(t, d, "/q/sub", "q2", 9000, "h1")
	addFast(t, d, "/q", "other-size", 9001, "h1") // same head, other size
	addFast(t, d, "/q", "other-head", 9000, "h2") // same size, other head
	if _, err := d.CommitDiscovered([]db.DiscoveredFile{{Dir: "/q", Name: "unhashed", Size: 9000, Mtime: time.Unix(1700000000, 0)}}, nil); err != nil {
		t.Fatal(err)
	}

	groups, err := d.PotentialDuplicates(nil, false)
	if err != nil || len(groups) != 2 {
		t.Fatalf("PotentialDuplicates = %+v, %v", groups, err)
	}
	if g := groups[0]; !slices.Equal(g.Locations, []string{"/q/q1", "/q/sub/q2"}) || len(g.Unverified) != 2 || g.Wasted != 9000 {
		t.Errorf("fast+fast = %+v", g)
	}
	if g := groups[1]; !slices.Equal(g.Locations, []string{"/h/other", "/q/matches-other"}) || !slices.Equal(g.Unverified, []string{"/q/matches-other"}) {
		t.Errorf("fast+hashed = %+v", g)
	}

	sub, _, _ := db.DirByPath(d, "/q/sub")
	subTree, err := db.TreeIDs(d, sub, true)
	if err != nil {
		t.Fatal(err)
	}
	if groups, err := d.PotentialDuplicates(subTree, false); err != nil || len(groups) != 1 || len(groups[0].Locations) != 2 {
		t.Errorf("filter = %+v, %v", groups, err)
	}
	if groups, err := d.PotentialDuplicates(subTree, true); err != nil || len(groups) != 0 {
		t.Errorf("within = %+v, %v", groups, err)
	}

	if fast, none, err := d.CountNotFullyHashed(nil, 0); err != nil || fast != 5 || none != 1 {
		t.Errorf("CountNotFullyHashed = %d, %d, %v", fast, none, err)
	}
	if fast, none, err := d.CountNotFullyHashed(subTree, 0); err != nil || fast != 1 || none != 0 {
		t.Errorf("CountNotFullyHashed in sub = %d, %d, %v", fast, none, err)
	}
	if fast, none, err := d.CountNotFullyHashed(nil, 9001); err != nil || fast != 1 || none != 0 {
		t.Errorf("CountNotFullyHashed min size = %d, %d, %v", fast, none, err)
	}
}

func TestDuplicatesContentKeys(t *testing.T) {
	d := dbtest.New(t)
	mtime := time.Unix(1700000000, 0)
	add := func(name, sha string, noexif *string) {
		t.Helper()
		f := db.ScannedFile{Dir: "/m", Name: name, Size: 10, Mtime: mtime, MD5: "md5" + sha, SHA256: sha, SHA256_8KB: "h", NoExifJPEG: noexif}
		if res, err := d.CommitScanBatch([]db.ScannedFile{f}); err != nil || len(res.Failed) > 0 {
			t.Fatalf("%s: %+v, %v", name, res, err)
		}
	}
	stripped, shared, orphaned := "stripped", "shared", "orphaned"
	add("photo.jpg", "with-exif", &stripped) // the same picture stored stripped under a non-JPEG name
	add("photo.bin", "stripped", nil)
	add("a.jpg", "a", &shared) // same no-EXIF hash, one instance each
	add("b.jpg", "b", &shared)
	add("c.bin", "c", nil) // one File, two instances
	add("c-copy.bin", "c", nil)
	add("unique.bin", "unique", nil)
	add("gone.jpg", "gone", &orphaned) // shares a no-EXIF hash with a file row that lost its instance
	add("kept.jpg", "kept", &orphaned)
	if _, err := d.Exec("DELETE FROM instance WHERE filename = 'gone.jpg'"); err != nil {
		t.Fatal(err)
	}

	groups, err := d.Duplicates(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var got [][]string
	for _, g := range groups {
		got = append(got, g.Locations)
	}
	slices.SortFunc(got, slices.Compare)
	want := [][]string{{"/m/a.jpg", "/m/b.jpg"}, {"/m/c-copy.bin", "/m/c.bin"}, {"/m/photo.bin", "/m/photo.jpg"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Duplicates = %v, want %v", got, want)
	}
}

// Duplicates must agree with grouping every instance by its content key.
func TestDuplicatesMatchesContentGrouping(t *testing.T) {
	d := dbtest.New(t)
	r := rand.New(rand.NewPCG(1, 2))
	mtime := time.Unix(1700000000, 0)
	var batch []db.ScannedFile
	for i := range 500 {
		sha := fmt.Sprintf("s%d", r.IntN(350)) // some files share content
		name := fmt.Sprintf("f%d.bin", i)
		var noexif *string
		switch n := r.IntN(10); {
		case n < 3: // a JPEG, which may share its no-EXIF hash
			x := fmt.Sprintf("x%d", r.IntN(60))
			noexif, name = &x, fmt.Sprintf("f%d.jpg", i)
		case n == 3: // a stripped JPEG's content under a non-JPEG name
			sha = fmt.Sprintf("x%d", r.IntN(60))
		}
		batch = append(batch, db.ScannedFile{Dir: fmt.Sprintf("/m/d%d", i%7), Name: name, Size: int64(r.IntN(5) + 1), Mtime: mtime,
			MD5: "md5" + sha, SHA256: sha, SHA256_8KB: "h", NoExifJPEG: noexif})
	}
	if res, err := d.CommitScanBatch(batch); err != nil || len(res.Failed) > 0 {
		t.Fatalf("%+v, %v", res, err)
	}
	if _, err := d.Exec("DELETE FROM instance WHERE id % 9 = 0"); err != nil { // leaves some file rows orphaned
		t.Fatal(err)
	}

	rows, err := d.Query(`SELECT COALESCE(f.sha256_noexif_jpeg, f.sha256), i.dir_id, i.filename
FROM file f JOIN instance i ON i.file_id = f.id WHERE f.filesize > 0`)
	if err != nil {
		t.Fatal(err)
	}
	dirs := map[int64]string{}
	byKey := map[string][]string{}
	for rows.Next() {
		var key, name string
		var dir int64
		if err := rows.Scan(&key, &dir, &name); err != nil {
			t.Fatal(err)
		}
		byKey[key] = append(byKey[key], fmt.Sprintf("%d/%s", dir, name))
		dirs[dir] = ""
	}
	rows.Close()
	for id := range dirs {
		var name string
		if err := d.QueryRow("SELECT name FROM dir WHERE id = ?", id).Scan(&name); err != nil {
			t.Fatal(err)
		}
		dirs[id] = "/m/" + name + "/"
	}
	var want [][]string
	for _, insts := range byKey {
		if len(insts) < 2 {
			continue
		}
		var paths []string
		for _, in := range insts {
			var id int64
			var name string
			if _, err := fmt.Sscanf(in, "%d/%s", &id, &name); err != nil {
				t.Fatal(err)
			}
			paths = append(paths, dirs[id]+name)
		}
		slices.Sort(paths)
		want = append(want, paths)
	}
	slices.SortFunc(want, slices.Compare)
	if len(want) == 0 {
		t.Fatal("test data has no duplicates")
	}

	groups, err := d.Duplicates(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var got [][]string
	for _, g := range groups {
		got = append(got, g.Locations)
	}
	slices.SortFunc(got, slices.Compare)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Duplicates = %v\nwant %v", got, want)
	}
}
