package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paranoidi/fileindex/db"
	"github.com/paranoidi/fileindex/internal/dbtest"
)

// --- duplicates ---

func TestDuplicates(t *testing.T) {
	h, d := findHarness(t)
	dbtest.AddFile(t, d, "/m/dir1/a.jpg", "same")
	dbtest.AddFile(t, d, "/m/dir2/b.jpg", "same")
	dbtest.AddFile(t, d, "/m/dir1/c.jpg", "inner")
	dbtest.AddFile(t, d, "/m/dir1/sub/d.jpg", "inner")
	dbtest.AddFile(t, d, "/m/dir3/lonely.jpg", "lonely")
	dbtest.AddFile(t, d, "/m/dir1/empty.jpg", "") // empty files share a hash but are no duplicates
	dbtest.AddFile(t, d, "/m/dir2/empty.jpg", "")

	want := "Finding duplicates...\nFound 2 sets of duplicate files:\n\n" +
		"1. File size: 5 bytes (2 instances)\n     /m/dir1/c.jpg\n     /m/dir1/sub/d.jpg\n\n" +
		"2. File size: 4 bytes (2 instances)\n     /m/dir1/a.jpg\n     /m/dir2/b.jpg\n\n" +
		"Total wasted space: 9 bytes\n"
	if out := h.mustRun("duplicates"); out != want {
		t.Errorf("got %q\nwant %q", out, want)
	}

	// a filter keeps groups with at least one copy inside, and still lists every copy
	if out := h.mustRun("duplicates", "/m/dir2"); !strings.Contains(out, "Found 1 sets") || !strings.Contains(out, "/m/dir1/a.jpg") {
		t.Errorf("filter dir2: %q", out)
	}
	// --within keeps only groups entirely inside
	if out := h.mustRun("duplicates", "/m/dir1", "--within"); !strings.Contains(out, "Found 1 sets") || strings.Contains(out, "dir2") {
		t.Errorf("--within dir1: %q", out)
	}
	if out := h.mustRun("duplicates", "/m/dir2", "--within"); out != "Finding duplicates...\nNo duplicate files found\n" {
		t.Errorf("--within dir2: %q", out)
	}
	if out := h.mustRun("duplicates", "--min-size", "5"); !strings.Contains(out, "Found 1 sets") || !strings.Contains(out, "5 bytes") {
		t.Errorf("--min-size 5: %q", out)
	}
}

func TestDuplicatesErrors(t *testing.T) {
	h, d := findHarness(t)
	code, out, errOut := h.run("duplicates", "--min-size", "10XB")
	if code != 1 || out != "" || errOut != "Error parsing --min-size: Unknown size unit: XB in 10XB\n" {
		t.Errorf("--min-size: exit %d %q %q", code, out, errOut)
	}
	if code, _, _ := h.run("duplicates", "--within"); code != 2 {
		t.Errorf("--within without path: exit %d", code)
	}
	// a path is matched as written: not in the index means an error, before any output
	dir := t.TempDir()
	dbtest.AddFile(t, d, filepath.Join(dir, "real", "a.jpg"), "x")
	os.MkdirAll(filepath.Join(dir, "real"), 0o755)
	os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "link"))
	t.Chdir(dir)
	for _, p := range []string{"/nonexistent", "real", ".", filepath.Join(dir, "link")} {
		code, out, errOut := h.run("duplicates", p)
		if want := "Path '" + p + "' not found in database"; code != 1 || out != "" || !strings.Contains(errOut, want) {
			t.Errorf("%s: exit %d %q %q", p, code, out, errOut)
		}
	}
}

// Names with "; " survive, and waste counts real sizes.
func TestDuplicatesRowsAndWaste(t *testing.T) {
	h, d := findHarness(t)
	dbtest.AddFile(t, d, "/m/a;b; c.jpg", "x")
	dbtest.AddFile(t, d, "/m/plain.jpg", "x")
	// two JPEGs with the same image data and different metadata sizes
	dir, _ := db.GetOrCreatePath(d, "/p")
	for i, size := range []int64{100, 150} {
		r, _ := d.Exec("INSERT INTO file (md5, sha256, sha256_noexif_jpeg, filesize) VALUES (?, ?, 'img', ?)", i, i, size)
		id, _ := r.LastInsertId()
		d.Exec("INSERT INTO instance (file_id, dir_id, filename, filename_lower, size) VALUES (?, ?, ?, ?, ?)", id, *dir, string(rune('a'+i))+".jpg", string(rune('a'+i))+".jpg", size)
	}
	out := h.mustRun("duplicates")
	if !strings.Contains(out, "     /m/a;b; c.jpg\n     /m/plain.jpg\n") {
		t.Errorf("locations: %q", out)
	}
	if !strings.Contains(out, "1. File size: 100 bytes (2 instances)\n     /p/a.jpg\n     /p/b.jpg\n") {
		t.Errorf("jpeg group: %q", out)
	}
	// 1 byte for the x copy, plus 100 (keep the 150-byte JPEG, drop the 100-byte one)
	if !strings.HasSuffix(out, "Total wasted space: 101 bytes\n") {
		t.Errorf("waste: %q", out)
	}
}

// --- diff ---

func TestDiff(t *testing.T) {
	h, _, tree := scanHarness(t)
	src, dst := filepath.Join(tree, "src"), filepath.Join(tree, "dst")
	writeFile(t, filepath.Join(src, "same.jpg"), "same")
	writeFile(t, filepath.Join(dst, "same.jpg"), "same")
	writeFile(t, filepath.Join(src, "only.jpg"), "only src")
	writeFile(t, filepath.Join(dst, "extra.jpg"), "only dst")
	writeFile(t, filepath.Join(src, "old.jpg"), "renamed")
	writeFile(t, filepath.Join(dst, "new.jpg"), "renamed")
	writeFile(t, filepath.Join(src, "changed.jpg"), "v1")
	writeFile(t, filepath.Join(dst, "changed.jpg"), "v2!")
	writeFile(t, filepath.Join(src, "photo.jpg"), fakeJPEG("A"))
	writeFile(t, filepath.Join(dst, "photo.jpg"), fakeJPEG("B, longer"))
	h.mustRun("scan", tree)
	writeFile(t, filepath.Join(src, "unindexed.jpg"), "u")
	os.Remove(filepath.Join(dst, "extra.jpg"))

	code, out, _ := h.run("diff", src, dst)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{
		"Comparing " + src + " vs " + dst + "\n(non-recursive comparison)\n\n",
		"Filesystem check for " + src + ":\n  ∆ Extra files not in database (1):\n    " + src + "/unindexed.jpg\n\n",
		"Filesystem check for " + dst + ":\n  ✘ Missing files from filesystem (1):\n    " + dst + "/extra.jpg\n\n",
		"Directory comparison (database):\n\n",
		"Only in " + src + " (1 files):\n  only.jpg                                    8 bytes\n\n",
		"Only in " + dst + " (1 files):\n  extra.jpg                                   8 bytes\n\n",
		"Same content, different names (1 groups):\n  old.jpg                                     7 bytes\n    → renamed/duplicated in " + dst + " as: new.jpg\n\n",
		"Same name, different content (1 files):\n  changed.jpg                             \n    " + src + ":    2 bytes  hash: ",
		"Same JPEG data, metadata differences (1 files):\n  photo.jpg ",
		"Summary:\n  Filesystem checks:\n    " + src + ": 0 missing, 1 extra\n    " + dst + ": 1 missing, 0 extra\n",
		"    Files only in " + src + ": 1\n    Files only in " + dst + ": 1\n    Files with same content but different names: 1\n" +
			"    Files with same name but different content: 1\n    JPEG files with same image data but different metadata: 1\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	if out := h.mustRun("diff", src, src); !strings.HasSuffix(out, "Directory comparison (database):\n\nSummary:\n  Filesystem checks:\n    "+src+": 0 missing, 1 extra\n    "+src+": 0 missing, 1 extra\n") {
		t.Errorf("self diff: %q", out)
	}

	code, _, errOut := h.run("diff", src, "/nonexistent")
	if code != 1 || errOut != "Error: Destination path '/nonexistent' not found in database\n" {
		t.Errorf("missing dst: exit %d %q", code, errOut)
	}
}

// -r compares by path relative to each root, not bare filename.
func TestDiffRecursive(t *testing.T) {
	h, _, tree := scanHarness(t)
	src, dst := filepath.Join(tree, "src"), filepath.Join(tree, "dst")
	writeFile(t, filepath.Join(src, "a", "x.jpg"), "1")
	writeFile(t, filepath.Join(src, "b", "x.jpg"), "2")
	writeFile(t, filepath.Join(dst, "a", "x.jpg"), "1")
	writeFile(t, filepath.Join(dst, "b", "x.jpg"), "2")
	h.mustRun("scan", tree)
	if out := h.mustRun("diff", "-r", src, dst); !strings.HasSuffix(out, "Directories are identical and all files present in filesystem.\n") {
		t.Errorf("identical trees: %q", out)
	}

	os.Rename(filepath.Join(dst, "b", "x.jpg"), filepath.Join(dst, "a", "y.jpg"))
	h.mustRun("scan", tree)
	out := h.mustRun("diff", "-r", src, dst)
	if !strings.Contains(out, "Same content, different names (1 groups):\n  b/x.jpg ") || !strings.Contains(out, "as: a/y.jpg\n") {
		t.Errorf("moved file: %q", out)
	}
}
