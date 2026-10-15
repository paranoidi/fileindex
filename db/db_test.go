package db_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
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
	if n != 43 { // 7 tables + 18 indexes + 6 autoindexes + 2 FTS tables, their 7 shadow tables, 3 triggers
		t.Errorf("sqlite_master rows = %d, want 43", n)
	}
	var mode string
	d.QueryRow("PRAGMA journal_mode").Scan(&mode)
	if mode != "wal" {
		t.Errorf("journal_mode = %q", mode)
	}
	if err := d.Init(io.Discard); err != nil {
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
	if err := d.Init(io.Discard); err != nil {
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

func TestDeleteOrphans(t *testing.T) {
	d := dbtest.New(t)
	db.GetOrCreatePath(d, "orphan1")
	db.GetOrCreatePath(d, "orphan2/child")
	keep := dbtest.AddFile(t, d, "/m/keep/x.jpg", "x")
	gone := dbtest.AddFile(t, d, "/m/gone/y.jpg", "y")
	d.Exec("DELETE FROM instance WHERE id = ?", gone)

	n, err := d.DeleteOrphanedDirs()
	if err != nil || n != 4 { // orphan1, orphan2, child, gone
		t.Errorf("DeleteOrphanedDirs = %d, %v", n, err)
	}
	if _, found, _ := db.DirByPath(d, "/m/keep"); !found {
		t.Error("/m/keep was deleted")
	}
	n, err = d.DeleteOrphanedFiles()
	if err != nil || n != 1 {
		t.Errorf("DeleteOrphanedFiles = %d, %v", n, err)
	}
	_ = keep
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
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.RemoveAll(filepath.Join(root, "link")))
	must(os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "link")))
	must(os.RemoveAll(filepath.Join(root, "nowfile")))
	must(os.WriteFile(filepath.Join(root, "nowfile"), []byte("x"), 0o644))

	id, _, err := db.DirByPath(d, root)
	must(err)
	var out strings.Builder
	files, dirs, err := d.PruneDir(context.Background(), root, *id, nil, logTo(&out))
	must(err)
	if files != 1 || dirs != 1 || !strings.Contains(out.String(), "nowfile") {
		t.Errorf("removed %d files, %d dirs:\n%s", files, dirs, out.String())
	}

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
	if err := d.Init(&out); err != nil {
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
