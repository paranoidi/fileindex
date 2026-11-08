package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/paranoidi/fileindex/db"
)

// tagged lists what find-tagged reports for tag under tree (files, then dirs).
func tagged(t *testing.T, h *harness, tree, tag string) []string {
	t.Helper()
	code, out, _ := h.run("find-tagged", "-r", tree, tag)
	if code != 0 {
		return nil
	}
	_, list, _ := strings.Cut(out, "\n\n")
	return strings.Fields(list)
}

func wantTagged(t *testing.T, h *harness, tree, tag string, paths ...string) {
	t.Helper()
	got := slices.Sorted(slices.Values(tagged(t, h, tree, tag))) // find-tagged lists files, then dirs
	want := slices.Sorted(slices.Values(paths))
	if !slices.Equal(got, want) {
		t.Errorf("tagged #%s:\n got %v\nwant %v", tag, got, want)
	}
}

// taggedFile is a tree holding one scanned file with one tag.
func taggedFile(t *testing.T) (h *harness, d *db.DB, tree, file string) {
	h, d, tree = scanHarness(t)
	file = filepath.Join(tree, "lantern.txt")
	writeFile(t, file, "ember glow")
	h.mustRun("scan", tree)
	h.mustRun("tag", file, "amber")
	return h, d, tree, file
}

func mustRename(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
}

func TestTagSurvivesMoveOnDiskThenScan(t *testing.T) {
	h, d, tree, file := taggedFile(t)
	moved := filepath.Join(tree, "shelf", "beacon.txt")
	mustRename(t, file, moved)
	h.mustRun("scan", tree)
	wantTagged(t, h, tree, "amber", moved)
	if n := count(t, d, "pending_tag"); n != 0 {
		t.Errorf("pending rows = %d", n)
	}
}

func TestTagSurvivesDirRenameOnDiskThenScan(t *testing.T) {
	h, _, tree := scanHarness(t)
	file := filepath.Join(tree, "old", "lantern.txt")
	writeFile(t, file, "ember glow")
	h.mustRun("scan", tree)
	h.mustRun("tag", file, "amber")
	mustRename(t, filepath.Join(tree, "old"), filepath.Join(tree, "new"))
	h.mustRun("scan", tree)
	wantTagged(t, h, tree, "amber", filepath.Join(tree, "new", "lantern.txt"))
}

// The destination scanned on its own: the old path has not been pruned yet, and both carry the tag.
func TestTagShowsAtOnceWhenOnlyDestinationIsScanned(t *testing.T) {
	h, _, tree, file := taggedFile(t)
	moved := filepath.Join(tree, "shelf", "beacon.txt")
	mustRename(t, file, moved)
	h.mustRun("scan", filepath.Join(tree, "shelf"))
	wantTagged(t, h, tree, "amber", file, moved)
	h.mustRun("prune", tree)
	wantTagged(t, h, tree, "amber", moved)
}

func TestTagShowsAfterHashingOfAWalkOnlyScan(t *testing.T) {
	h, _, tree, file := taggedFile(t)
	moved := filepath.Join(tree, "beacon.txt")
	mustRename(t, file, moved)
	h.mustRun("scan", "--no-hash", tree)
	wantTagged(t, h, tree, "amber")
	h.mustRun("scan", tree)
	wantTagged(t, h, tree, "amber", moved)
}

func TestTagSurvivesMovePruneScan(t *testing.T) {
	h, d, tree, file := taggedFile(t)
	moved := filepath.Join(tree, "beacon.txt")
	mustRename(t, file, moved)
	h.mustRun("prune", tree)
	var gone *string
	if err := d.QueryRow("SELECT CAST(gone_at AS TEXT) FROM content_tag").Scan(&gone); err != nil || gone == nil {
		t.Errorf("gone_at while no File has the content = %v, %v", gone, err)
	}
	h.mustRun("scan", tree)
	wantTagged(t, h, tree, "amber", moved)
	if err := d.QueryRow("SELECT CAST(gone_at AS TEXT) FROM content_tag").Scan(&gone); err != nil || gone != nil {
		t.Errorf("gone_at once the content is back = %v, %v", gone, err)
	}
}

func TestTagReturnsWithContent(t *testing.T) {
	h, _, tree, file := taggedFile(t)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	h.mustRun("scan", tree)
	wantTagged(t, h, tree, "amber")
	back := filepath.Join(tree, "restored.txt")
	writeFile(t, back, "ember glow")
	h.mustRun("scan", tree)
	wantTagged(t, h, tree, "amber", back)
}

func TestRemovingLocationKeepsTag(t *testing.T) {
	h, _, tree := scanHarness(t)
	dir := filepath.Join(tree, "sub")
	file := filepath.Join(dir, "lantern.txt")
	writeFile(t, file, "ember glow")
	h.mustRun("scan", tree)
	h.mustRun("tag", file, "amber")
	h.mustRun("remove", dir)
	wantTagged(t, h, tree, "amber")
	h.mustRun("scan", tree)
	wantTagged(t, h, tree, "amber", file)
}

func TestMvKeepsTag(t *testing.T) {
	h, _, tree, file := taggedFile(t)
	moved := filepath.Join(tree, "beacon.txt")
	mustRename(t, file, moved)
	h.mustRun("mv", file, moved)
	wantTagged(t, h, tree, "amber", moved)
}

func TestTagAppliesToAllCopies(t *testing.T) {
	h, _, tree := scanHarness(t)
	a, b, c := filepath.Join(tree, "a.txt"), filepath.Join(tree, "sub", "b-renamed.txt"), filepath.Join(tree, "c.txt")
	writeFile(t, a, "same content")
	writeFile(t, b, "same content")
	writeFile(t, c, "other content")
	h.mustRun("scan", tree)

	out := h.mustRun("tag", a, "amber")
	wantLines(t, out, "Added 1 tag(s) #amber to "+a, "Also applies to 1 other copy")
	wantTagged(t, h, tree, "amber", a, b)

	out = h.mustRun("tag", "remove", b, "amber")
	wantLines(t, out, "Removed tag #amber from "+b, "Also applies to 1 other copy")
	wantTagged(t, h, tree, "amber")

	// no copies, no extra line
	if out := h.mustRun("tag", c, "amber"); strings.Contains(out, "Also applies") {
		t.Errorf("output for a file without copies: %q", out)
	}
}

// Copies outside a recursive target get the tag, and the output says so.
func TestRecursiveTagCountsCopiesOutside(t *testing.T) {
	h, _, tree := scanHarness(t)
	in1, in2, out1 := filepath.Join(tree, "in", "a.txt"), filepath.Join(tree, "in", "b.txt"), filepath.Join(tree, "out", "c.txt")
	writeFile(t, in1, "same content")
	writeFile(t, in2, "same content")
	writeFile(t, out1, "same content")
	h.mustRun("scan", tree)
	out := h.mustRun("tag", "-r", filepath.Join(tree, "in"), "amber")
	wantLines(t, out, "Added 2 tag(s) #amber to "+filepath.Join(tree, "in"), "Also applies to 1 other copy")
	wantTagged(t, h, tree, "amber", in1, in2, out1, filepath.Join(tree, "in")+"/")

	// both targeted copies are reported, though they share one tag
	removed := h.mustRun("tag", "remove", "-r", filepath.Join(tree, "in"), "amber")
	for _, p := range []string{in1, in2} {
		if !strings.Contains(removed, "Removed tag #amber from "+p+"\n") {
			t.Errorf("no removal line for %s in %q", p, removed)
		}
	}
}

func TestPendingTagOnUnhashedFile(t *testing.T) {
	h, d, tree := scanHarness(t)
	file := filepath.Join(tree, "lantern.txt")
	copyOf := filepath.Join(tree, "copy.txt")
	writeFile(t, copyOf, "ember glow")
	h.mustRun("scan", tree)
	writeFile(t, file, "ember glow")
	h.mustRun("scan", "--no-hash", tree)

	h.mustRun("tag", file, "amber")
	if n := count(t, d, "pending_tag"); n != 1 {
		t.Fatalf("pending rows = %d", n)
	}
	wantTagged(t, h, tree, "amber", file)
	if out := h.mustRun("ls", tree); !strings.Contains(out, "#amber") {
		t.Errorf("ls: %q", out)
	}
	if out := h.mustRun("tags", "list"); !strings.Contains(out, "Total: 1 tag(s), 1 file(s), 0 dir(s)") {
		t.Errorf("tags list: %q", out)
	}

	h.mustRun("scan", tree)
	if n := count(t, d, "pending_tag"); n != 0 {
		t.Errorf("pending rows after hashing = %d", n)
	}
	wantTagged(t, h, tree, "amber", file, copyOf)
}

func TestFastScanKeepsTagPending(t *testing.T) {
	h, d, tree := scanHarness(t)
	file := filepath.Join(tree, "big.bin")
	writeFile(t, file, strings.Repeat("x", 20000))
	h.mustRun("scan", "--no-hash", tree)
	h.mustRun("tag", file, "amber")
	h.mustRun("scan", "--fast", tree)
	if n := count(t, d, "pending_tag"); n != 1 {
		t.Errorf("pending rows after a fast scan = %d", n)
	}
	wantTagged(t, h, tree, "amber", file)
	h.mustRun("scan", tree)
	if n := count(t, d, "pending_tag"); n != 0 {
		t.Errorf("pending rows after a full scan = %d", n)
	}
	wantTagged(t, h, tree, "amber", file)
}

func TestRemovePendingTag(t *testing.T) {
	h, d, tree := scanHarness(t)
	file := filepath.Join(tree, "lantern.txt")
	writeFile(t, file, "ember glow")
	h.mustRun("scan", "--no-hash", tree)
	h.mustRun("tag", file, "amber")
	wantLines(t, h.mustRun("tag", "remove", file, "amber"), "Removed tag #amber from "+file)
	if n := count(t, d, "pending_tag"); n != 0 {
		t.Errorf("pending rows = %d", n)
	}
	h.mustRun("scan", tree)
	wantTagged(t, h, tree, "amber")
}

func TestPruneLogsLostPendingTag(t *testing.T) {
	h, d, tree := scanHarness(t)
	file := filepath.Join(tree, "lantern.txt")
	dir := filepath.Join(tree, "sub")
	inDir := filepath.Join(dir, "meadow.txt")
	writeFile(t, file, "ember glow")
	writeFile(t, inDir, "clover")
	h.mustRun("scan", "--no-hash", tree)
	h.mustRun("tag", file, "amber")
	h.mustRun("tag", inDir, "amber")
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := h.run("prune", tree)
	if code != 0 {
		t.Fatalf("prune: exit %d %q", code, errOut)
	}
	all := out + errOut
	if !strings.Contains(all, "Lost tag #amber on unhashed file: "+file) {
		t.Errorf("no line for the file: %q", all)
	}
	if !strings.Contains(all, "Lost 1 tag(s) on unhashed files under: "+dir) {
		t.Errorf("no line for the dir: %q", all)
	}
	if n := count(t, d, "pending_tag"); n != 0 {
		t.Errorf("pending rows = %d", n)
	}
}

func TestEditedFileInheritsTag(t *testing.T) {
	h, d, tree, file := taggedFile(t)
	keep := filepath.Join(tree, "copy.txt")
	writeFile(t, keep, "ember glow")
	h.mustRun("scan", tree)
	writeFile(t, file, "ember glow, and a good deal more")
	h.mustRun("scan", tree)
	wantTagged(t, h, tree, "amber", file, keep)
	if n := count(t, d, "pending_tag"); n != 0 {
		t.Errorf("pending rows = %d", n)
	}
	if n := count(t, d, "content_tag"); n != 2 {
		t.Errorf("content_tag rows = %d, want the old and the new content", n)
	}
	// without the copy the old content stays tagged, absent, until tag-gc
	h.mustRun("tag", "remove", keep, "amber")
	wantTagged(t, h, tree, "amber", file)
}

func TestEditedToEmptyDropsInheritedTag(t *testing.T) {
	h, d, tree, file := taggedFile(t)
	writeFile(t, file, "")
	h.mustRun("scan", tree)
	wantTagged(t, h, tree, "amber")
	if n := count(t, d, "pending_tag"); n != 0 {
		t.Errorf("pending rows = %d", n)
	}
}

func TestForcedRescanLeavesTagsAlone(t *testing.T) {
	h, d, tree, file := taggedFile(t)
	h.mustRun("scan", "-a", tree)
	wantTagged(t, h, tree, "amber", file)
	if n := count(t, d, "pending_tag"); n != 0 {
		t.Errorf("pending rows = %d", n)
	}
	if n := count(t, d, "content_tag"); n != 1 {
		t.Errorf("content_tag rows = %d", n)
	}
}

func TestEmptyFileCannotBeTagged(t *testing.T) {
	h, d, tree := scanHarness(t)
	empty, other, full := filepath.Join(tree, "empty"), filepath.Join(tree, "sub", "also-empty"), filepath.Join(tree, "full.txt")
	writeFile(t, empty, "")
	writeFile(t, other, "")
	writeFile(t, full, "x")
	h.mustRun("scan", tree)

	code, _, errOut := h.run("tag", empty, "amber")
	if code != 1 || errOut != "Error: Empty files can't be tagged: "+empty+"\n" {
		t.Errorf("tag empty: exit %d %q", code, errOut)
	}

	out := h.mustRun("tag", "-r", tree, "amber")
	wantLines(t, out, "Added 3 tag(s) #amber to "+tree, "Skipped 2 empty file(s)")
	wantTagged(t, h, tree, "amber", full, tree+"/sub/")

	// an unhashed empty file too
	late := filepath.Join(tree, "late")
	writeFile(t, late, "")
	h.mustRun("scan", "--no-hash", tree)
	if code, _, errOut := h.run("tag", late, "amber"); code != 1 || !strings.Contains(errOut, "Empty files") {
		t.Errorf("tag unhashed empty: exit %d %q", code, errOut)
	}
	if n := count(t, d, "pending_tag"); n != 0 {
		t.Errorf("pending rows = %d", n)
	}
}

func TestGoneAtFollowsEveryDeleteSite(t *testing.T) {
	gone := func(t *testing.T, d *db.DB) bool {
		t.Helper()
		var at *string
		if err := d.QueryRow("SELECT CAST(gone_at AS TEXT) FROM content_tag").Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at != nil
	}
	t.Run("RemoveInstance", func(t *testing.T) { // API only: see db.TestRemoveInstanceKeepsContentTag
		_, d, _, file := taggedFile(t)
		if _, err := d.RemoveInstance(file); err != nil {
			t.Fatal(err)
		}
		if !gone(t, d) {
			t.Error("gone_at not set")
		}
	})
	t.Run("RemoveTree", func(t *testing.T) {
		h, d, tree := scanHarness(t)
		file := filepath.Join(tree, "sub", "a.txt")
		writeFile(t, file, "ember")
		h.mustRun("scan", tree)
		h.mustRun("tag", file, "amber")
		h.mustRun("remove", filepath.Join(tree, "sub"))
		if !gone(t, d) {
			t.Error("gone_at not set")
		}
	})
	t.Run("DeleteOrphanedFiles", func(t *testing.T) {
		h, d, tree, file := taggedFile(t)
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
		h.mustRun("prune", tree)
		if !gone(t, d) {
			t.Error("gone_at not set")
		}
	})
}

func TestTagsRmRemovesContentAndPending(t *testing.T) {
	h, d, tree, _ := taggedFile(t)
	pendingFile := filepath.Join(tree, "pending.txt")
	writeFile(t, pendingFile, "clover")
	h.mustRun("scan", "--no-hash", tree)
	h.mustRun("tag", pendingFile, "amber")
	h.mustRun("tag", tree, "amber")
	if out := h.mustRun("tags", "rm", "amber"); out != "Deleted tag #amber and removed 3 association(s)\n" {
		t.Errorf("tags rm: %q", out)
	}
	if a, b := count(t, d, "content_tag"), count(t, d, "pending_tag"); a != 0 || b != 0 {
		t.Errorf("content_tag = %d, pending_tag = %d", a, b)
	}
}

// Absent content keeps its tag out of the counts.
func TestTagsListCountsCurrentFiles(t *testing.T) {
	h, _, tree, file := taggedFile(t)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	h.mustRun("prune", tree)
	if out := h.mustRun("tags", "list"); !strings.Contains(out, "Total: 1 tag(s), 0 file(s), 0 dir(s)") {
		t.Errorf("tags list: %q", out)
	}
}

func TestDirTagsStayOnDirs(t *testing.T) {
	h, _, tree := scanHarness(t)
	dir := filepath.Join(tree, "old")
	writeFile(t, filepath.Join(dir, "a.txt"), "ember")
	h.mustRun("scan", tree)
	h.mustRun("tag", dir, "amber")
	wantTagged(t, h, tree, "amber", dir+"/")
	mustRename(t, dir, filepath.Join(tree, "new"))
	h.mustRun("scan", tree)
	wantTagged(t, h, tree, "amber")
}
