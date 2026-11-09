package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sampledTagged is a tree holding a file large enough to have a sample apart from
// its full hash, tagged and fast-hashed only, so its tag is pending on a sample.
func sampledTagged(t *testing.T) (h *harness, tree, file string) {
	h, _, tree = scanHarness(t)
	file = filepath.Join(tree, "orchard", "lantern.bin")
	writeFile(t, file, strings.Repeat("harbor meadow ", 3000))
	h.mustRun("scan", "--no-hash", tree)
	h.mustRun("tag", file, "amber")
	h.mustRun("scan", "--fast", tree)
	return h, tree, file
}

func TestSampledPendingTagSurvivesMoveThenFastScan(t *testing.T) {
	h, tree, file := sampledTagged(t)
	moved := filepath.Join(tree, "harbor", "beacon.bin")
	mustRename(t, filepath.Dir(file), filepath.Dir(moved))
	mustRename(t, filepath.Join(filepath.Dir(moved), filepath.Base(file)), moved)
	code, out, errOut := h.run("prune", tree)
	if code != 0 || !strings.Contains(out+errOut, "Kept 1 tag(s) by sample of unhashed files under: "+filepath.Dir(file)) {
		t.Fatalf("prune: exit %d %q %q", code, out, errOut)
	}
	wantTagged(t, h, tree, "amber")
	h.mustRun("scan", "--fast", tree)
	wantTagged(t, h, tree, "amber", moved)
	h.mustRun("scan", tree) // the full hash moves it onto the content
	wantTagged(t, h, tree, "amber", moved)
}

func TestSampledPendingTagSurvivesMoveThenFullScan(t *testing.T) {
	h, tree, file := sampledTagged(t)
	moved := filepath.Join(tree, "beacon.bin")
	mustRename(t, file, moved)
	code, out, errOut := h.run("scan", tree) // prunes the old path, then hashes the new one fully
	if code != 0 || !strings.Contains(out+errOut, "Kept tag #amber by sample of unhashed file: "+file) {
		t.Fatalf("scan: exit %d %q %q", code, out, errOut)
	}
	wantTagged(t, h, tree, "amber", moved)
}

func TestSampledPendingTagSurvivesRemove(t *testing.T) {
	h, tree, file := sampledTagged(t)
	h.mustRun("remove", filepath.Dir(file))
	wantTagged(t, h, tree, "amber")
	h.mustRun("scan", "--fast", tree)
	wantTagged(t, h, tree, "amber", file)
}

func TestSampledTagNeedsSameSample(t *testing.T) {
	h, tree, file := sampledTagged(t)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] = '!' // same size, other tail: an unfinished copy
	moved := filepath.Join(tree, "beacon.bin")
	writeFile(t, moved, string(data))
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	h.mustRun("scan", "--fast", tree)
	wantTagged(t, h, tree, "amber")
}

func TestTagGCEndsSampleTags(t *testing.T) {
	h, tree, file := sampledTagged(t)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	h.mustRun("prune", tree)
	if out := h.mustRun("tag-gc", "0"); !strings.Contains(out, "Deleted 1 tag(s)") {
		t.Errorf("tag-gc: %q", out)
	}
	writeFile(t, file, strings.Repeat("harbor meadow ", 3000))
	h.mustRun("scan", "--fast", tree)
	wantTagged(t, h, tree, "amber")
}
