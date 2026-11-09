package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const mib = 1 << 20

// filler is n bytes of seed repeated, so each seed is its own content.
func filler(seed string, n int) string {
	return strings.Repeat(seed, n/len(seed)+1)[:n]
}

// filmDir writes a film-like dir: one large video and two small subtitle files,
// all of content unique to seed.
func filmDir(t *testing.T, dir, seed string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "feature.mkv"), filler(seed+" reel ", 3*mib/2))
	writeFile(t, filepath.Join(dir, "subs", "en.srt"), filler(seed+" english ", 40000))
	writeFile(t, filepath.Join(dir, "subs", "fi.srt"), filler(seed+" finnish ", 30000))
}

// taggedFilm is a scanned tree holding a film dir tagged #amber.
func taggedFilm(t *testing.T, scanArgs ...string) (h *harness, tree, dir string) {
	h, _, tree = scanHarness(t)
	dir = filepath.Join(tree, "harbor-lights")
	filmDir(t, dir, "harbor")
	h.mustRun(append([]string{"scan"}, append(scanArgs, tree)...)...)
	h.mustRun("tag", dir, "amber")
	return h, tree, dir
}

func TestDirTagSurvivesRenameOnDiskThenScan(t *testing.T) {
	h, tree, dir := taggedFilm(t)
	moved := filepath.Join(tree, "shelf", "lights-of-the-harbor")
	mustRename(t, dir, moved)
	code, out, errOut := h.run("scan", tree)
	all := out + errOut
	if code != 0 || !strings.Contains(all, "Kept tag #amber of directory by its files: "+dir) ||
		!strings.Contains(all, "Restored tag #amber on directory: "+moved+" (3 of 3 files matched)") {
		t.Fatalf("scan: exit %d\n%s", code, all)
	}
	wantTagged(t, h, tree, "amber", moved+"/") // the new parent is not tagged
}

func TestDirTagSurvivesMoveThenFastScan(t *testing.T) {
	h, tree, dir := taggedFilm(t, "--fast")
	moved := filepath.Join(tree, "beacon")
	mustRename(t, dir, moved)
	h.mustRun("scan", "--fast", tree)
	wantTagged(t, h, tree, "amber", moved+"/")
}

func TestDirTagNeedsHashesAtTheNewPath(t *testing.T) {
	h, tree, dir := taggedFilm(t, "--fast")
	moved := filepath.Join(tree, "beacon")
	mustRename(t, dir, moved)
	h.mustRun("scan", "--no-hash", tree)
	wantTagged(t, h, tree, "amber")
	h.mustRun("scan", "--fast", tree)
	wantTagged(t, h, tree, "amber", moved+"/")
}

func TestDirTagSurvivesRemoveThenRescan(t *testing.T) {
	h, tree, dir := taggedFilm(t)
	h.mustRun("remove", dir)
	wantTagged(t, h, tree, "amber")
	h.mustRun("scan", tree)
	wantTagged(t, h, tree, "amber", dir+"/")
}

func TestRecursiveDirTagsSurviveMove(t *testing.T) {
	h, _, tree := scanHarness(t)
	dir := filepath.Join(tree, "harbor-lights")
	filmDir(t, dir, "harbor")
	writeFile(t, filepath.Join(dir, "subs", "extra.bin"), filler("harbor extra ", 2*mib))
	h.mustRun("scan", tree)
	h.mustRun("tag", "-r", dir, "amber")
	moved := filepath.Join(tree, "beacon")
	mustRename(t, dir, moved)
	h.mustRun("scan", tree)
	got := tagged(t, h, tree, "amber")
	for _, want := range []string{moved + "/", filepath.Join(moved, "subs") + "/"} {
		if !strings.Contains(strings.Join(got, "\n")+"\n", want+"\n") {
			t.Errorf("%s not tagged: %v", want, got)
		}
	}
}

func TestDirTagToleratesSmallChanges(t *testing.T) {
	h, tree, dir := taggedFilm(t)
	moved := filepath.Join(tree, "beacon")
	mustRename(t, dir, moved)
	writeFile(t, filepath.Join(moved, "notes.txt"), filler("harbor notes ", 5000))
	if err := os.Remove(filepath.Join(moved, "subs", "fi.srt")); err != nil {
		t.Fatal(err)
	}
	h.mustRun("scan", tree)
	wantTagged(t, h, tree, "amber", moved+"/")
}

func TestDirTagNotRestoredOnHalfTheBytes(t *testing.T) {
	h, _, tree := scanHarness(t)
	dir := filepath.Join(tree, "pair")
	writeFile(t, filepath.Join(dir, "one.mkv"), filler("pair one ", 2*mib))
	writeFile(t, filepath.Join(dir, "two.mkv"), filler("pair two ", 2*mib))
	h.mustRun("scan", tree)
	h.mustRun("tag", dir, "amber")
	moved := filepath.Join(tree, "half")
	mustRename(t, dir, moved)
	if err := os.Remove(filepath.Join(moved, "two.mkv")); err != nil {
		t.Fatal(err)
	}
	h.mustRun("scan", tree)
	wantTagged(t, h, tree, "amber")
}

// A dir of one small common file is no evidence: the same file elsewhere does not
// take the tag.
func TestDirTagNeedsEvidence(t *testing.T) {
	h, _, tree := scanHarness(t)
	dir, other := filepath.Join(tree, "tool"), filepath.Join(tree, "other-tool")
	writeFile(t, filepath.Join(dir, "LICENSE"), "permission is hereby granted")
	writeFile(t, filepath.Join(other, "LICENSE"), "permission is hereby granted")
	h.mustRun("scan", tree)
	h.mustRun("tag", dir, "amber")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	h.mustRun("scan", tree)
	wantTagged(t, h, tree, "amber")
}

func TestDirTagGoesToEveryCopy(t *testing.T) {
	h, tree, dir := taggedFilm(t)
	a, b := filepath.Join(tree, "copy-a"), filepath.Join(tree, "copy-b")
	filmDir(t, a, "harbor")
	mustRename(t, dir, b)
	h.mustRun("scan", tree)
	wantTagged(t, h, tree, "amber", a+"/", b+"/")
}

func TestDirTagOnEmptyDirIsLost(t *testing.T) {
	h, _, tree := scanHarness(t)
	dir := filepath.Join(tree, "hollow")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tree, "keep.txt"), "keep")
	h.mustRun("scan", tree)
	h.mustRun("tag", dir, "amber")
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := h.run("prune", tree); code != 0 || !strings.Contains(out+errOut, "Lost tag #amber on directory with no files: "+dir) {
		t.Errorf("prune: exit %d %q %q", code, out, errOut)
	}
}

func TestTagGCAndTagDeleteEndPrints(t *testing.T) {
	for _, end := range [][]string{{"tag-gc", "0"}, {"tags", "rm", "amber"}} {
		h, d, tree := scanHarness(t)
		dir := filepath.Join(tree, "harbor-lights")
		filmDir(t, dir, "harbor")
		h.mustRun("scan", tree)
		h.mustRun("tag", dir, "amber")
		moved := filepath.Join(tree, "beacon")
		mustRename(t, dir, moved)
		h.mustRun("prune", tree)
		if n := count(t, d, "dir_print"); n != 1 {
			t.Fatalf("prints after prune = %d", n)
		}
		h.mustRun(end...)
		h.mustRun("scan", tree)
		if n := count(t, d, "dir_print"); n != 0 {
			t.Errorf("%v: prints left = %d", end, n)
		}
		wantTagged(t, h, tree, "amber")
	}
}

// Prune of the old path finds the new one already scanned.
func TestPruneRestoresOnAlreadyScannedCopy(t *testing.T) {
	h, tree, dir := taggedFilm(t)
	moved := filepath.Join(tree, "beacon")
	mustRename(t, dir, moved)
	h.mustRun("scan", moved) // the old record stays: its parent was not scanned
	h.mustRun("prune", tree)
	wantTagged(t, h, tree, "amber", moved+"/")
}

// Phase 2: a tagged dir moved on disk and only its new path scanned gets its tags
// copied there; the old record keeps them until a prune.
func TestLiveDirTagCopiedToNewPath(t *testing.T) {
	h, tree, dir := taggedFilm(t)
	moved := filepath.Join(tree, "beacon")
	mustRename(t, dir, moved)
	code, out, errOut := h.run("scan", moved)
	if code != 0 || !strings.Contains(out+errOut, "Copied tag #amber of moved directory "+dir+" to: "+moved) {
		t.Fatalf("scan: exit %d %q %q", code, out, errOut)
	}
	wantTagged(t, h, tree, "amber", dir+"/", moved+"/")
	h.mustRun("prune", tree)
	wantTagged(t, h, tree, "amber", moved+"/")
	if code, out, errOut := h.run("scan", moved); code != 0 || strings.Contains(out+errOut, "Copied") {
		t.Errorf("rescan copied again: %q %q", out, errOut)
	}
}
