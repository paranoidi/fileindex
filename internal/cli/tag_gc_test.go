package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paranoidi/fileindex/db"
)

// absentTag is a tagged file removed from disk and pruned, its content gone for ageDays.
func absentTag(t *testing.T, ageDays string) (h *harness, d *db.DB, tree, content string) {
	h, d, tree, file := taggedFile(t)
	content = "ember glow"
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	h.mustRun("prune", tree)
	if _, err := d.Exec("UPDATE content_tag SET gone_at = datetime('now', ?)", "-"+ageDays+" days"); err != nil {
		t.Fatal(err)
	}
	return h, d, tree, content
}

func TestTagGCDeletesOldAbsentContentTags(t *testing.T) {
	h, d, tree, content := absentTag(t, "40")
	if out := h.mustRun("tag-gc", "30"); out != "Deleted 1 tag(s) on content absent for more than 30 day(s)\n" {
		t.Errorf("output: %q", out)
	}
	if n := count(t, d, "content_tag"); n != 0 {
		t.Errorf("content_tag rows = %d", n)
	}
	// the tag name stays, and the content returning has no tag
	if out := h.mustRun("tags", "list"); !strings.Contains(out, "#amber") {
		t.Errorf("tags list: %q", out)
	}
	back := filepath.Join(tree, "back.txt")
	writeFile(t, back, content)
	h.mustRun("scan", tree)
	wantTagged(t, h, tree, "amber")
}

func TestTagGCKeepsNewerAbsentContentTags(t *testing.T) {
	h, d, _, _ := absentTag(t, "10")
	if out := h.mustRun("tag-gc", "30"); out != "Deleted 0 tag(s) on content absent for more than 30 day(s)\n" {
		t.Errorf("output: %q", out)
	}
	if n := count(t, d, "content_tag"); n != 1 {
		t.Errorf("content_tag rows = %d", n)
	}
}

func TestTagGCKeepsPresentContentHoweverOld(t *testing.T) {
	h, d, tree, file := taggedFile(t)
	if _, err := d.Exec("UPDATE content_tag SET created_at = '2001-01-01 00:00:00'"); err != nil {
		t.Fatal(err)
	}
	h.mustRun("tag-gc", "0")
	wantTagged(t, h, tree, "amber", file)
}

// gone_at can be stale only through a bug, and must not cost a live tag.
func TestTagGCRechecksContentIsAbsent(t *testing.T) {
	h, d, tree, file := taggedFile(t)
	if _, err := d.Exec("UPDATE content_tag SET gone_at = '2001-01-01 00:00:00'"); err != nil {
		t.Fatal(err)
	}
	h.mustRun("tag-gc", "0")
	wantTagged(t, h, tree, "amber", file)
}

func TestContentReturningProtectsTagFromGC(t *testing.T) {
	h, d, tree, content := absentTag(t, "40")
	back := filepath.Join(tree, "back.txt")
	writeFile(t, back, content)
	h.mustRun("scan", tree)
	var gone *string
	if err := d.QueryRow("SELECT CAST(gone_at AS TEXT) FROM content_tag").Scan(&gone); err != nil || gone != nil {
		t.Errorf("gone_at = %v, %v", gone, err)
	}
	h.mustRun("tag-gc", "0")
	wantTagged(t, h, tree, "amber", back)
}

func TestTagGCZeroLeavesEverythingElse(t *testing.T) {
	h, d, tree, content := absentTag(t, "0")
	present := filepath.Join(tree, "present.txt")
	pendingFile := filepath.Join(tree, "pending.txt")
	dir := filepath.Join(tree, "shelf")
	writeFile(t, present, "meadow")
	writeFile(t, filepath.Join(dir, "x.txt"), "clover")
	h.mustRun("scan", tree)
	h.mustRun("tag", present, "amber")
	h.mustRun("tag", dir, "amber")
	writeFile(t, pendingFile, "bramble")
	h.mustRun("scan", "--no-hash", tree)
	h.mustRun("tag", pendingFile, "amber")

	if out := h.mustRun("tag-gc", "0"); !strings.HasPrefix(out, "Deleted 1 tag(s)") {
		t.Errorf("output: %q", out)
	}
	wantTagged(t, h, tree, "amber", present, pendingFile, dir+"/")
	if n := count(t, d, "pending_tag"); n != 1 {
		t.Errorf("pending rows = %d", n)
	}
	_ = content
}

func TestTagGCRefusesBadDays(t *testing.T) {
	h, d, _, _ := absentTag(t, "40")
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"tag-gc"}, 2}, // usage errors exit 2 throughout the CLI
		{[]string{"tag-gc", "-5"}, 2},
		{[]string{"tag-gc", "soon"}, 1},
		{[]string{"tag-gc", "1.5"}, 1},
		{[]string{"tag-gc", "--", "-5"}, 1},
	} {
		if code, _, errOut := h.run(c.args...); code != c.code || errOut == "" {
			t.Errorf("%v: exit %d, stderr %q", c.args, code, errOut)
		}
	}
	if n := count(t, d, "content_tag"); n != 1 {
		t.Errorf("content_tag rows = %d, want nothing deleted", n)
	}
}

// Age alone never deletes a tag in a scan or prune.
func TestScanAndPruneNeverExpireTags(t *testing.T) {
	h, d, tree, _ := absentTag(t, "4000")
	h.mustRun("scan", tree)
	h.mustRun("prune", tree)
	if n := count(t, d, "content_tag"); n != 1 {
		t.Errorf("content_tag rows = %d", n)
	}
}
