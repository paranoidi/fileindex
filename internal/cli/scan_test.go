package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paranoidi/fileindex/db"
)

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// scanHarness is an initialised index plus a real directory tree to scan.
func scanHarness(t *testing.T) (*harness, *db.DB, string) {
	h := newHarness(t)
	h.mustRun("init")
	d, err := db.Open(h.dbPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	tree, _ := filepath.EvalSymlinks(t.TempDir())
	return h, d, tree
}

func count(t *testing.T, d *db.DB, table string) int {
	t.Helper()
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func wantLines(t *testing.T, out string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(out, l+"\n") {
			t.Errorf("missing line %q in:\n%s", l, out)
		}
	}
}

func noLines(t *testing.T, out string, prefixes ...string) {
	t.Helper()
	for _, p := range prefixes {
		if strings.Contains(out, p) {
			t.Errorf("unexpected %q in:\n%s", p, out)
		}
	}
}

// Scan stores absolute paths.
func TestScanStoresAbsolutePaths(t *testing.T) {
	h, d, tree := scanHarness(t)
	writeFile(t, filepath.Join(tree, "sub", "a.jpg"), "aaa")
	out := h.mustRun("scan", tree)
	wantLines(t, out, "Scanning 1 path(s)...", "Files processed: 1", "Files added: 1", "Total size processed: 3 bytes")

	id, found, _ := db.DirByPath(d, filepath.Join(tree, "sub"))
	if !found {
		t.Fatal("dir not stored under its absolute path")
	}
	var md5 string
	var size int64
	var mtime string
	err := d.QueryRow("SELECT f.md5, f.filesize, CAST(i.mtime AS TEXT) FROM instance i JOIN file f ON f.id = i.file_id WHERE i.dir_id = ? AND i.filename = 'a.jpg'", *id).Scan(&md5, &size, &mtime)
	if err != nil || len(md5) != 32 || size != 3 {
		t.Fatalf("row: %q %d %v", md5, size, err)
	}
	fi, _ := os.Stat(filepath.Join(tree, "sub", "a.jpg"))
	if mtime != db.FormatMtime(fi.ModTime()) {
		t.Errorf("mtime %q, want %q", mtime, db.FormatMtime(fi.ModTime()))
	}
}

// A modified file is re-hashed and its old file row is gone.
func TestScanModifiedFile(t *testing.T) {
	h, d, tree := scanHarness(t)
	p := filepath.Join(tree, "a.jpg")
	writeFile(t, p, "version 1")
	h.mustRun("scan", tree)
	writeFile(t, p, "version 2, longer")
	out := h.mustRun("scan", tree)
	wantLines(t, out, "Files processed: 1", "Files updated: 1")
	noLines(t, out, "Files added")
	if count(t, d, "instance") != 1 || count(t, d, "file") != 1 {
		t.Errorf("instances %d, files %d", count(t, d, "instance"), count(t, d, "file"))
	}
}

func TestScanUnchangedFile(t *testing.T) {
	h, d, tree := scanHarness(t)
	writeFile(t, filepath.Join(tree, "a.jpg"), "same")
	h.mustRun("scan", tree)
	out := h.mustRun("scan", "-a", tree)
	wantLines(t, out, "Files updated: 1")
	if count(t, d, "instance") != 1 || count(t, d, "file") != 1 {
		t.Errorf("instances %d, files %d", count(t, d, "instance"), count(t, d, "file"))
	}
}

func TestScanDuplicatesShareFileRow(t *testing.T) {
	h, d, tree := scanHarness(t)
	writeFile(t, filepath.Join(tree, "a", "x.jpg"), "dup")
	writeFile(t, filepath.Join(tree, "b", "y.jpg"), "dup")
	h.mustRun("scan", tree)
	if count(t, d, "instance") != 2 || count(t, d, "file") != 1 {
		t.Errorf("instances %d, files %d", count(t, d, "instance"), count(t, d, "file"))
	}
}

func smallBatches(t *testing.T) {
	old := batchFiles
	batchFiles = 2
	t.Cleanup(func() { batchFiles = old })
}

// scan -m with 2 workers and batches of 2.
func TestScanMissingOnly(t *testing.T) {
	h, d, tree := scanHarness(t)
	smallBatches(t)
	t.Setenv("FILEINDEX_SCANNING_PARALLEL_WORKERS", "2")
	for i := range 6 {
		writeFile(t, filepath.Join(tree, "old", string(rune('a'+i))+".jpg"), "old "+string(rune('a'+i)))
	}
	h.mustRun("scan", tree)
	for i := range 3 {
		writeFile(t, filepath.Join(tree, "new", string(rune('a'+i))+".jpg"), "new "+string(rune('a'+i)))
	}
	out := h.mustRun("scan", tree)
	wantLines(t, out, "Files processed: 3", "Files added: 3", "Files skipped: 6")
	if n := count(t, d, "instance"); n != 9 {
		t.Errorf("instances %d", n)
	}
}

// The default scan re-hashes a file whose size or mtime changed.
func TestScanMissingOnlyPicksUpModified(t *testing.T) {
	h, _, tree := scanHarness(t)
	p := filepath.Join(tree, "a.jpg")
	writeFile(t, p, "v1")
	writeFile(t, filepath.Join(tree, "b.jpg"), "b")
	h.mustRun("scan", tree)
	writeFile(t, p, "v2 longer")
	out := h.mustRun("scan", tree)
	wantLines(t, out, "Files processed: 1", "Files updated: 1", "Files skipped: 1")
}

// The default scan skips unchanged files reached through a followed dir symlink.
func TestScanMissingOnlyThroughSymlink(t *testing.T) {
	h, _, tree := scanHarness(t)
	t.Setenv("FILEINDEX_SCANNING_FOLLOW_SYMLINKS", "true")
	other, _ := filepath.EvalSymlinks(t.TempDir())
	writeFile(t, filepath.Join(other, "a.jpg"), "a")
	os.Symlink(other, filepath.Join(tree, "link"))
	h.mustRun("scan", tree)
	out := h.mustRun("scan", tree)
	wantLines(t, out, "Files processed: 0", "Files skipped: 1")
}

// An md5 collision is a per-file error, the scan carries on.
func TestScanMD5Collision(t *testing.T) {
	h, d, tree := scanHarness(t)
	writeFile(t, filepath.Join(tree, "a.jpg"), "a")
	writeFile(t, filepath.Join(tree, "b.jpg"), "b")
	// pretend another file shares a.jpg's md5
	// held by an instance elsewhere, or the post-prune orphan sweep would drop it
	d.Exec("INSERT INTO file (md5, sha256, filesize) VALUES ('0cc175b9c0f1b6a831c399e269772661', 'other', 1)")
	other, _ := db.GetOrCreatePath(d, "/elsewhere")
	d.Exec("INSERT INTO instance (file_id, dir_id, filename, filename_lower, size) SELECT id, ?, 'x', 'x', 1 FROM file WHERE sha256 = 'other'", *other)
	code, out, errOut := h.run("scan", tree)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	wantLines(t, out, "Files processed: 1", "Files added: 2", "Files with errors: 1")
	if !strings.Contains(errOut, "md5 collision: "+filepath.Join(tree, "a.jpg")) {
		t.Errorf("stderr: %s", errOut)
	}
}

// NULL hash columns get filled on a full (-a) rescan.
func TestScanBackfillsHashes(t *testing.T) {
	h, d, tree := scanHarness(t)
	writeFile(t, filepath.Join(tree, "a.jpg"), "\xff\xd8\xff\xda\x00\x02\xff\xd9")
	h.mustRun("scan", tree)
	d.Exec("UPDATE file SET sha256_8kb = NULL, sha256_noexif_jpeg = NULL")
	h.mustRun("scan", "-a", tree)
	var n int
	d.QueryRow("SELECT COUNT(*) FROM file WHERE sha256_8kb IS NOT NULL AND sha256_noexif_jpeg IS NOT NULL").Scan(&n)
	if n != 1 {
		t.Error("hashes not backfilled")
	}
}

// Scan -a replaces a stale no-EXIF hash on a JPEG, and a non-JPEG
// copy of the same content leaves it alone.
func TestScanRepairsNoExifHash(t *testing.T) {
	h, d, tree := scanHarness(t)
	writeFile(t, filepath.Join(tree, "a.jpg"), "\xff\xd8\x00\xff\xda\x00\x02")
	h.mustRun("scan", tree)
	d.Exec("UPDATE file SET sha256_noexif_jpeg = 'stale'")
	h.mustRun("scan", "-a", tree)
	var n int
	d.QueryRow("SELECT COUNT(*) FROM file WHERE sha256_noexif_jpeg IS NULL").Scan(&n)
	if n != 1 {
		t.Error("stale no-EXIF hash not cleared")
	}

	writeFile(t, filepath.Join(tree, "b.jpg"), "\xff\xd8\xff\xda\x00\x02")
	h.mustRun("scan", tree)
	writeFile(t, filepath.Join(tree, "b.pdf"), "\xff\xd8\xff\xda\x00\x02")
	h.mustRun("scan", "-a", tree)
	d.QueryRow("SELECT COUNT(*) FROM file WHERE sha256_noexif_jpeg IS NOT NULL").Scan(&n)
	if n != 1 {
		t.Error("non-JPEG copy cleared the no-EXIF hash")
	}
}

func TestScanUninitialised(t *testing.T) {
	h := newHarness(t)
	tree := t.TempDir()
	writeFile(t, filepath.Join(tree, "a.jpg"), "a")
	code, out, errOut := h.run("scan", tree)
	if code != 1 || errOut != "Error: database not initialised, run `fileindex init`\n" || out != "" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestScanPathMissing(t *testing.T) {
	h, _, tree := scanHarness(t)
	p := filepath.Join(tree, "nope")
	code, _, errOut := h.run("scan", p)
	if code != 1 || errOut != "Error: Path does not exist: "+p+"\n" {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

// -q prints nothing on stdout, even prune lines.
func TestScanQuiet(t *testing.T) {
	h, _, tree := scanHarness(t)
	writeFile(t, filepath.Join(tree, "a.jpg"), "a")
	writeFile(t, filepath.Join(tree, "b.jpg"), "b")
	h.mustRun("scan", tree)
	os.Remove(filepath.Join(tree, "b.jpg"))
	if out := h.mustRun("scan", "-q", tree); out != "" {
		t.Errorf("stdout %q", out)
	}
}

// Pruning during scan: the scanned root is never removed by prune.
func TestScanPrunes(t *testing.T) {
	h, d, tree := scanHarness(t)
	writeFile(t, filepath.Join(tree, "keep.jpg"), "k")
	writeFile(t, filepath.Join(tree, "gone.jpg"), "g")
	writeFile(t, filepath.Join(tree, "sub", "x.jpg"), "x")
	writeFile(t, filepath.Join(tree, "sub2", "deep", "y.jpg"), "y")
	h.mustRun("scan", tree)

	os.Remove(filepath.Join(tree, "gone.jpg"))
	os.RemoveAll(filepath.Join(tree, "sub"))
	os.RemoveAll(filepath.Join(tree, "sub2"))
	out := h.mustRun("scan", tree)
	wantLines(t, out,
		"Pruning removed file: "+filepath.Join(tree, "gone.jpg"),
		"Pruning removed directory: "+filepath.Join(tree, "sub"),
		"Pruning removed directory: "+filepath.Join(tree, "sub2"),
		"Files pruned: 3",
		"Directories pruned: 3", // sub, sub2 and sub2/deep
	)
	if n := count(t, d, "instance"); n != 1 {
		t.Errorf("instances %d", n)
	}
	if n := count(t, d, "file"); n != 1 {
		t.Errorf("file rows %d (orphans not deleted)", n)
	}

	// dirs still on disk stay recorded, even empty: the root, an empty subdir,
	// and one whose only file was removed
	writeFile(t, filepath.Join(tree, "full", "z.jpg"), "z")
	os.MkdirAll(filepath.Join(tree, "empty", "deeper"), 0o755)
	h.mustRun("scan", tree)
	os.Remove(filepath.Join(tree, "keep.jpg"))
	os.Remove(filepath.Join(tree, "full", "z.jpg"))
	h.mustRun("scan", tree)
	for _, p := range []string{tree, filepath.Join(tree, "full"), filepath.Join(tree, "empty", "deeper")} {
		if _, found, _ := db.DirByPath(d, p); !found {
			t.Errorf("%s not recorded", p)
		}
	}
}

// A hash error appears once, in the errors list on stderr.
func TestScanErrorsListed(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read anything")
	}
	h, _, tree := scanHarness(t)
	p := filepath.Join(tree, "locked.jpg")
	writeFile(t, p, "x")
	os.Chmod(p, 0)
	code, out, errOut := h.run("scan", tree)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	wantLines(t, out, "Files with errors: 1")
	if !strings.Contains(errOut, "\nErrors encountered:\n  Error reading file "+p) || strings.Count(errOut, p) != 1 {
		t.Errorf("stderr: %q", errOut)
	}
}
