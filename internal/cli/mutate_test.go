package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paranoidi/fileindex/db"
	"github.com/paranoidi/fileindex/internal/dbtest"
)

func dirID(t *testing.T, d *db.DB, p string) int64 {
	t.Helper()
	id, found, err := db.DirByPath(d, p)
	if err != nil || !found {
		t.Fatalf("dir %s not in DB (%v)", p, err)
	}
	return *id
}

func instanceDir(t *testing.T, d *db.DB, filename string) int64 {
	t.Helper()
	var id int64
	if err := d.QueryRow("SELECT dir_id FROM instance WHERE filename = ?", filename).Scan(&id); err != nil {
		t.Fatalf("instance %s: %v", filename, err)
	}
	return id
}

// --- mv ---

func TestMvDirRename(t *testing.T) {
	h, d := findHarness(t)
	tree := t.TempDir()
	src, dst := filepath.Join(tree, "src"), filepath.Join(tree, "parent", "renamed")
	os.MkdirAll(dst, 0o755)
	dbtest.AddFile(t, d, filepath.Join(src, "child", "file.txt"), "x")
	srcID, childID := dirID(t, d, src), dirID(t, d, filepath.Join(src, "child"))

	out := h.mustRun("mv", src, dst)
	if out != "\nMove completed:\n  Directory rows updated: 1\n  File instances updated: 0\n" {
		t.Errorf("output %q", out)
	}
	if dirID(t, d, dst) != srcID {
		t.Error("renamed dir did not keep its id")
	}
	if dirID(t, d, filepath.Join(dst, "child")) != childID {
		t.Error("child did not follow")
	}
}

func TestMvDirMerge(t *testing.T) {
	h, d := findHarness(t)
	tree := t.TempDir()
	src, dst := filepath.Join(tree, "src"), filepath.Join(tree, "existing")
	os.MkdirAll(dst, 0o755)
	dbtest.AddFile(t, d, filepath.Join(src, "root.txt"), "r")
	dbtest.AddFile(t, d, filepath.Join(src, "nested", "nested.txt"), "n")
	dbtest.AddFile(t, d, filepath.Join(dst, "other.txt"), "o")
	childID, dstID := dirID(t, d, filepath.Join(src, "nested")), dirID(t, d, dst)

	h.mustRun("mv", src, dst)
	if _, found, _ := db.DirByPath(d, src); found {
		t.Error("src dir still recorded")
	}
	if instanceDir(t, d, "root.txt") != dstID || instanceDir(t, d, "nested.txt") != childID {
		t.Error("instances not moved as expected")
	}
	if dirID(t, d, filepath.Join(dst, "nested")) != childID {
		t.Error("nested dir not re-parented")
	}
}

// Same-named subdirs merge recursively, a shared tag is kept once, a
// shared filename is an error and nothing changes.
func TestMvDirMergeRecursive(t *testing.T) {
	h, d := findHarness(t)
	tree := t.TempDir()
	src, dst := filepath.Join(tree, "src"), filepath.Join(tree, "dst")
	os.MkdirAll(dst, 0o755)
	dbtest.AddFile(t, d, filepath.Join(src, "sub", "a.jpg"), "a")
	dbtest.AddFile(t, d, filepath.Join(dst, "sub", "b.jpg"), "b")
	d.Exec("INSERT INTO tag (name) VALUES ('t')")
	d.Exec("INSERT INTO dir_tag (tag_id, dir_id) SELECT id, ? FROM tag", dirID(t, d, src))
	d.Exec("INSERT INTO dir_tag (tag_id, dir_id) SELECT id, ? FROM tag", dirID(t, d, dst))

	h.mustRun("mv", src, dst)
	sub := dirID(t, d, filepath.Join(dst, "sub"))
	if instanceDir(t, d, "a.jpg") != sub || instanceDir(t, d, "b.jpg") != sub {
		t.Error("sub dirs not merged")
	}
	if n := count(t, d, "dir_tag"); n != 1 {
		t.Errorf("dir_tag rows %d, want 1", n)
	}

	src2 := filepath.Join(tree, "src2")
	dbtest.AddFile(t, d, filepath.Join(src2, "sub", "a.jpg"), "different a")
	code, _, errOut := h.run("mv", src2, dst)
	want := "Error: Cannot merge: " + filepath.Join(dst, "sub", "a.jpg") + " exists in both source and destination\n"
	if code != 1 || errOut != want {
		t.Errorf("clash: exit %d %q", code, errOut)
	}
	if _, found, _ := db.DirByPath(d, filepath.Join(src2, "sub")); !found {
		t.Error("failed merge changed the DB")
	}
}

func TestMvIntoOwnSubtree(t *testing.T) {
	h, d := findHarness(t)
	dbtest.AddFile(t, d, "/a/b/x.jpg", "x")
	code, _, errOut := h.run("mv", "-f", "/a", "/a/b/c")
	if code != 1 || errOut != "Error: Cannot move /a into its own subdirectory /a/b/c\n" {
		t.Errorf("exit %d %q", code, errOut)
	}
}

func TestMvFile(t *testing.T) {
	h, d := findHarness(t)
	tree := t.TempDir()
	src, dst := filepath.Join(tree, "src_dir", "file.txt"), filepath.Join(tree, "dst_dir", "renamed.txt")
	writeFile(t, dst, "data")
	dbtest.AddFile(t, d, src, "data")
	out := h.mustRun("mv", src, dst)
	if !strings.Contains(out, "File instances updated: 1\n") {
		t.Errorf("output %q", out)
	}
	if instanceDir(t, d, "renamed.txt") != dirID(t, d, filepath.Dir(dst)) {
		t.Error("instance not moved")
	}
}

func TestMvChecks(t *testing.T) {
	h, d := findHarness(t)
	tree := t.TempDir()
	dir := filepath.Join(tree, "dir")
	dbtest.AddFile(t, d, filepath.Join(dir, "f.jpg"), "f")
	writeFile(t, filepath.Join(tree, "plain.jpg"), "p")

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{filepath.Join(tree, "nope"), tree}, "Error: Source path '" + filepath.Join(tree, "nope") + "' not found in database\n"},
		{[]string{dir, filepath.Join(tree, "gone")}, "Error: Destination path does not exist: " + filepath.Join(tree, "gone") + "\nUse -f to override this check.\n"},
		{[]string{dir, filepath.Join(tree, "plain.jpg")}, "Error: Destination path is not a directory: " + filepath.Join(tree, "plain.jpg") + "\nUse -f to override this check.\n"},
		{[]string{filepath.Join(dir, "f.jpg"), tree}, "Error: Destination path is not a file: " + tree + "\nUse -f to override this check.\n"},
	} {
		if code, _, errOut := h.run(append([]string{"mv"}, c.args...)...); code != 1 || errOut != c.want {
			t.Errorf("%v: exit %d %q", c.args, code, errOut)
		}
	}
	if out := h.mustRun("mv", dir, dir+"/"); out != "Source and destination paths match. Nothing to do.\n" {
		t.Errorf("same path: %q", out)
	}
	// -f skips the filesystem checks
	h.mustRun("mv", "-f", dir, filepath.Join(tree, "gone"))
	dirID(t, d, filepath.Join(tree, "gone"))
}

// --- prune ---

func TestPrune(t *testing.T) {
	h, d, tree := scanHarness(t)
	writeFile(t, filepath.Join(tree, "keep.jpg"), "k")
	writeFile(t, filepath.Join(tree, "gone.jpg"), "g")
	writeFile(t, filepath.Join(tree, "sub", "x.jpg"), "x")
	h.mustRun("scan", tree)
	os.Remove(filepath.Join(tree, "gone.jpg"))
	os.RemoveAll(filepath.Join(tree, "sub"))

	out := h.mustRun("prune", tree)
	want := "Pruning removed file: " + tree + "/gone.jpg\nPruning removed directory: " + tree + "/sub\n" +
		"\nPrune completed:\n  Files removed: 2\n  Directories removed: 1\n"
	if out != want { // 2 instances, not 2 + 2 orphaned file rows
		t.Errorf("got %q\nwant %q", out, want)
	}
	if n := count(t, d, "file"); n != 1 {
		t.Errorf("file rows %d", n)
	}

	// the start dir stays even when its last file goes; the global
	// sweep then removes the now-empty chain, and that counts as dirs
	os.Remove(filepath.Join(tree, "keep.jpg"))
	out = h.mustRun("prune", tree)
	if strings.Contains(out, "Removed empty directory: "+tree+"\n") || !strings.Contains(out, "Files removed: 1\n") {
		t.Errorf("start dir: %q", out)
	}

	if code, _, errOut := h.run("prune", filepath.Join(tree, "nope")); code != 1 || errOut != "Error: Path does not exist: "+tree+"/nope\n" {
		t.Errorf("missing: exit %d %q", code, errOut)
	}
	other := t.TempDir()
	if code, _, errOut := h.run("prune", other); code != 1 || errOut != "Error: Path '"+other+"' not found in database\n" {
		t.Errorf("unindexed: exit %d %q", code, errOut)
	}
}

// --- remove ---

func TestRemove(t *testing.T) {
	h, d := findHarness(t)
	dbtest.AddFile(t, d, "/test/subdir/a.jpg", "a")
	dbtest.AddFile(t, d, "/test/subdir/deep/b.jpg", "b")
	dbtest.AddFile(t, d, "/other/c.jpg", "c")
	out := h.mustRun("remove", "/test/subdir")
	want := "Removing all instances in directory tree: /test/subdir\nCleaning up orphaned file records\n" +
		"\nRemoved from database:\n  Instances removed: 2\n  Files removed: 2\n  Directories removed: 3\n"
	if out != want {
		t.Errorf("got %q\nwant %q", out, want)
	}
	if _, found, _ := db.DirByPath(d, "/test/subdir"); found {
		t.Error("dir still recorded")
	}
	if code, _, errOut := h.run("remove", "/nonexistent/path"); code != 1 || errOut != "Error: Path '/nonexistent/path' not found in database\n" {
		t.Errorf("missing: exit %d %q", code, errOut)
	}
}
