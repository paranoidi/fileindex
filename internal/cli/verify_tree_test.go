package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paranoidi/fileindex/internal/dbtest"
)

// --- tree: annotation and rendering ---

func TestTreeAnnotation(t *testing.T) {
	for _, c := range []struct {
		n    treeNode
		want string
	}{
		{treeNode{}, ""},
		{treeNode{dbFiles: 3}, " {db-3f}"},
		{treeNode{dbFiles: 3, fsFiles: 2}, " {db-3f, fs-2f}"},
		{treeNode{dbFiles: 3, dbDirs: 1, fsFiles: 2}, " {db-3f:1d, fs-2f}"},
		{treeNode{dbFiles: 3, dbDirs: 1, fsFiles: 2, fsDirs: 4}, " {db-3f:1d, fs-2f:4d}"},
		{treeNode{fsDirs: 1}, " {fs-1d}"},
	} {
		if got := c.n.annotation(); got != c.want {
			t.Errorf("%+v: got %q want %q", c.n, got, c.want)
		}
	}
}

func renderLines(root *treeNode, limit bool, depth int, dOnly bool) string {
	r := &treeRenderer{limit: limit, maxDepth: depth, discrepanciesOnly: dOnly}
	r.render(root, "", true, 0)
	return strings.Join(r.lines, "\n")
}

func TestTreeRender(t *testing.T) {
	gc1, gc2 := &treeNode{name: "grandchild1"}, &treeNode{name: "grandchild2"}
	c1 := &treeNode{name: "child1", children: []*treeNode{gc1, gc2}}
	c2 := &treeNode{name: "child2"}
	root := &treeNode{name: "root", children: []*treeNode{c1, c2}}

	if got := renderLines(root, false, 0, false); got != "root\n├── child1\n│   ├── grandchild1\n│   └── grandchild2\n└── child2" {
		t.Errorf("nested:\n%s", got)
	}
	if got := renderLines(root, true, 0, false); got != "root" {
		t.Errorf("-L 0: %q", got)
	}
	if got := renderLines(root, true, 1, false); got != "root\n├── child1\n└── child2" {
		t.Errorf("-L 1: %q", got)
	}
	if got := renderLines(root, true, -1, false); got != "" {
		t.Errorf("-L -1: %q", got)
	}

	root.dbFiles, c1.fsFiles = 2, 3
	if got := renderLines(root, false, 0, false); !strings.HasPrefix(got, "root {db-2f}\n├── child1 {fs-3f}\n") {
		t.Errorf("annotated: %q", got)
	}
	// -d hides clean child2 but keeps the clean root, which has a child to show
	root.dbFiles = 0
	if got := renderLines(root, false, 0, true); got != "root\n└── child1 {fs-3f}" {
		t.Errorf("-d: %q", got)
	}

	// a child selected only for a deeper difference must not leave a
	// dangling "├── " when -L cuts that difference off
	c1.fsFiles, gc2.dbFiles = 0, 1
	c2.fsFiles = 1
	if got := renderLines(root, true, 1, true); got != "root\n└── child2 {fs-1f}" {
		t.Errorf("-d -L 1: %q", got)
	}
}

// --- tree: building and the command ---

func TestTreeCommand(t *testing.T) {
	h, d, tree := scanHarness(t)
	t.Setenv("FILEINDEX_SCANNING_FILES", "*.jpg")
	writeFile(t, filepath.Join(tree, "both", "a.jpg"), "a")
	writeFile(t, filepath.Join(tree, "both", "only_in_db.jpg"), "x")
	h.mustRun("scan", tree)
	os.Remove(filepath.Join(tree, "both", "only_in_db.jpg"))
	dbtest.AddFile(t, d, filepath.Join(tree, "db_only_dir", "z.jpg"), "z")
	writeFile(t, filepath.Join(tree, "fs_only_dir", "new.jpg"), "n")
	writeFile(t, filepath.Join(tree, "both", "empty.jpg"), "")  // 0-byte files count
	writeFile(t, filepath.Join(tree, ".cache", "c.jpg"), "c")   // excluded dir
	writeFile(t, filepath.Join(tree, "both", "notes.txt"), "n") // not a scanned pattern

	name := filepath.Base(tree)
	code, out, _ := h.run("tree", tree)
	want := name + " {db-1d, fs-1d}\n├── both {db-1f, fs-1f}\n├── db_only_dir {fs-1f}\n└── fs_only_dir {db-1f}\n"
	if code != 1 || out != want {
		t.Errorf("union: exit %d\n%s\nwant\n%s", code, out, want)
	}
	if _, out, _ := h.run("tree", tree, "--db"); !strings.Contains(out, "db_only_dir") || strings.Contains(out, "fs_only_dir") {
		t.Errorf("--db: %s", out)
	}
	if _, out, _ := h.run("tree", tree, "--fs"); strings.Contains(out, "db_only_dir") || !strings.Contains(out, "fs_only_dir") {
		t.Errorf("--fs: %s", out)
	}
	if code, _, _ := h.run("tree", tree, "--db", "--fs"); code != 2 {
		t.Errorf("--db --fs: exit %d", code)
	}
	// -L hides the children but the exit code still reports them
	if code, out, _ := h.run("tree", tree, "-l", "0"); code != 1 || out != name+" {db-1d, fs-1d}\n" {
		t.Errorf("-l 0: exit %d %q", code, out)
	}

	clean := filepath.Join(tree, "both")
	os.RemoveAll(filepath.Join(tree, "fs_only_dir"))
	h.mustRun("scan", clean)
	if out := h.mustRun("tree", clean); out != "both\n" {
		t.Errorf("clean: %q", out)
	}

	missing := filepath.Join(tree, "nope")
	if code, _, errOut := h.run("tree", missing); code != 1 || errOut != "Error: Path '"+missing+"' not found in database or filesystem\n" {
		t.Errorf("missing: exit %d %q", code, errOut)
	}
	if code, _, errOut := h.run("tree", filepath.Join(tree, "fs_only_dir"), "--db"); code != 1 || !strings.Contains(errOut, "not found in database\n") {
		t.Errorf("--db missing: exit %d %q", code, errOut)
	}
}

// Dir symlinks are followed only with follow_symlinks, and loops end.
func TestTreeSymlinks(t *testing.T) {
	h, _, tree := scanHarness(t)
	writeFile(t, filepath.Join(tree, "real", "a.jpg"), "a")
	os.Symlink(tree, filepath.Join(tree, "real", "loop"))
	h.mustRun("scan", tree)
	if out := h.mustRun("tree", tree); out != filepath.Base(tree)+"\n└── real\n" {
		t.Errorf("no follow: %q", out)
	}
	t.Setenv("FILEINDEX_SCANNING_FOLLOW_SYMLINKS", "true")
	done := make(chan string)
	go func() { _, out, _ := h.run("tree", tree); done <- out }()
	select {
	case out := <-done:
		if strings.Count(out, "loop") > 0 {
			t.Errorf("loop entered: %q", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tree did not terminate on a symlink loop")
	}
}

// --- verify ---

func TestVerify(t *testing.T) {
	h, d, tree := scanHarness(t)
	writeFile(t, filepath.Join(tree, "ok.jpg"), "ok")
	writeFile(t, filepath.Join(tree, "sub", "deep.jpg"), "deep")
	h.mustRun("scan", tree)

	if out := h.mustRun("verify", tree, "-q"); out != "Verification of "+tree+"\n(non-recursive verification)\n\n"+okMark+" All files verified successfully (1 files)\n" {
		t.Errorf("clean: %q", out)
	}
	if out := h.mustRun("verify", tree, "-r", "-q"); !strings.Contains(out, "(2 files)") {
		t.Errorf("clean -r: %q", out)
	}
	if out := h.mustRun("verify", filepath.Join(tree, "ok.jpg")); out != "Verifying: "+tree+"/ok.jpg\n"+okMark+" File verified successfully\n" {
		t.Errorf("single file: %q", out)
	}
	// progress lines are rewritten with \r and cleared
	if out := h.mustRun("verify", tree); !strings.HasPrefix(out, "\rVerifying [1/1]: "+tree+"/ok.jpg") {
		t.Errorf("progress: %q", out)
	}

	// break things: a size change, a same-size content change, a missing and a new file
	writeFile(t, filepath.Join(tree, "big.jpg"), "big")
	writeFile(t, filepath.Join(tree, "same.jpg"), "aaaa")
	writeFile(t, filepath.Join(tree, "gone.jpg"), "gone")
	h.mustRun("scan", tree)
	writeFile(t, filepath.Join(tree, "big.jpg"), "bigger")
	writeFile(t, filepath.Join(tree, "same.jpg"), "bbbb")
	os.Remove(filepath.Join(tree, "gone.jpg"))
	writeFile(t, filepath.Join(tree, "new.jpg"), "new")

	code, out, _ := h.run("verify", tree, "-q")
	for _, want := range []string{
		"✘ Missing files from filesystem (1):\n  " + tree + "/gone.jpg\n\n",
		"✘ Size mismatches (1):\n  " + tree + "/big.jpg\n    Expected: 3 bytes, Actual: 6 bytes\n\n",
		"✘ Hash mismatches (1):\n  " + tree + "/same.jpg\n    Expected MD5:    " + sum(md5Of, "aaaa") + "\n    Actual MD5:      " + sum(md5Of, "bbbb") + "\n",
		"∆ Missing files from database (1):\n  " + tree + "/new.jpg\n\n",
		"Summary:\n  " + okMark + " Verified OK:      1\n  ✘ Missing files:    1\n  ✘ Size mismatches:  1\n  ✘ Hash mismatches:  1\n  ∆ Extra files:      1\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if code != 1 {
		t.Errorf("exit %d", code)
	}

	code, out, _ = h.run("verify", filepath.Join(tree, "same.jpg"), "-q")
	if code != 1 || !strings.Contains(out, "\n✘ Hash mismatch:\n  Expected MD5:    ") {
		t.Errorf("single mismatch: exit %d %q", code, out)
	}
	_ = d
}

func TestVerifyErrors(t *testing.T) {
	h, _, tree := scanHarness(t)
	writeFile(t, filepath.Join(tree, "sub", "a.jpg"), "a")
	h.mustRun("scan", tree)
	// a dir with only subdirs needs -r
	code, _, errOut := h.run("verify", tree)
	if code != 1 || errOut != "Error: no files directly in "+tree+"; use -r\n" {
		t.Errorf("only subdirs: exit %d %q", code, errOut)
	}
	other := t.TempDir()
	if code, _, errOut := h.run("verify", other); code != 1 || errOut != "Error: Path '"+other+"' not found in database\n" {
		t.Errorf("unindexed dir: exit %d %q", code, errOut)
	}
	writeFile(t, filepath.Join(other, "x.jpg"), "x")
	if code, _, errOut := h.run("verify", filepath.Join(other, "x.jpg")); code != 1 || !strings.Contains(errOut, "not found in database") {
		t.Errorf("unindexed file: exit %d %q", code, errOut)
	}
	if code, _, errOut := h.run("verify", filepath.Join(tree, "nope")); code != 1 || errOut != "Error: Path '"+tree+"/nope' does not exist\n" {
		t.Errorf("missing path: exit %d %q", code, errOut)
	}

	// an unreadable file is an issue, not silently skipped
	if os.Getuid() != 0 {
		p := filepath.Join(tree, "sub", "a.jpg")
		os.Chmod(p, 0)
		defer os.Chmod(p, 0o644)
		code, out, _ := h.run("verify", "-r", "-q", tree)
		if code != 1 || !strings.Contains(out, "✘ Hash errors (1):\n  "+p+"\n    Error reading file ") || !strings.Contains(out, "  ✘ Hash errors:      1\n") {
			t.Errorf("unreadable: exit %d %q", code, out)
		}
	}
}
