package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func tagTree(t *testing.T) (*harness, string) {
	h, _, tree := scanHarness(t)
	writeFile(t, filepath.Join(tree, "photos", "img1.jpg"), "1")
	writeFile(t, filepath.Join(tree, "photos", "sub", "img2.jpg"), "2")
	h.mustRun("scan", tree)
	return h, tree
}

func TestTagAddRemove(t *testing.T) {
	h, tree := tagTree(t)
	img := filepath.Join(tree, "photos", "img1.jpg")
	photos := filepath.Join(tree, "photos")

	if out := h.mustRun("tag", img, "a", "b"); out != "Added 2 tag(s) #a, #b to "+img+"\n" {
		t.Errorf("add: %q", out)
	}
	if out := h.mustRun("tag", "add", img, "a", "b"); out != "Tags #a, #b already applied to "+img+"\n" {
		t.Errorf("repeat: %q", out)
	}
	if out := h.mustRun("tag", photos, "x", "y"); out != "Added 2 tag(s) #x, #y to "+photos+"\n" {
		t.Errorf("dir add: %q", out)
	}
	// recursive: the dir, its subdir and both files
	if out := h.mustRun("tag", "-r", photos, "trip"); out != "Added 4 tag(s) #trip to "+photos+"\n" {
		t.Errorf("recursive add: %q", out)
	}

	if out := h.mustRun("tag", "remove", img, "a", "zzz"); out != "Removed tag #a from "+img+"\n" {
		t.Errorf("remove: %q", out)
	}
	if out := h.mustRun("tag", "remove", photos, "x"); out != "Removed tag #x from "+photos+"/\n" {
		t.Errorf("dir remove: %q", out)
	}
	// recursive removal lists dirs then files, each sorted by path
	want := "Removed tag #trip from " + photos + "/\nRemoved tag #trip from " + photos + "/sub/\n" +
		"Removed tag #trip from " + img + "\nRemoved tag #trip from " + photos + "/sub/img2.jpg\n"
	if out := h.mustRun("tag", "remove", "-r", photos, "trip"); out != want {
		t.Errorf("recursive remove: got %q\nwant %q", out, want)
	}
	if out := h.mustRun("tag", "remove", img, "nope"); out != "No tags #nope found on "+img+"\n" {
		t.Errorf("nothing removed: %q", out)
	}

	// every failure is one Error line, exit 1
	other := t.TempDir()
	writeFile(t, filepath.Join(other, "x.jpg"), "x")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"tag", filepath.Join(other, "x.jpg"), "t"}, "Error: File not found in database: " + other + "/x.jpg\n"},
		{[]string{"tag", "remove", other, "t"}, "Error: Directory not found in database: " + other + "\n"},
		{[]string{"tag", "-r", other, "t"}, "Error: Directory not found in database: " + other + "\n"},
		{[]string{"tag", "-r", img, "t"}, "Error: Recursive mode requires a directory: " + img + "\n"},
		{[]string{"tag", filepath.Join(other, "nope"), "t"}, "Error: Path does not exist: " + other + "/nope\n"},
		{[]string{"tag", "add", img}, "Error: tag command requires a path and at least one tag\n"},
	} {
		if code, _, errOut := h.run(c.args...); code != 1 || errOut != c.want {
			t.Errorf("%v: exit %d %q", c.args, code, errOut)
		}
	}
	if code, _, _ := h.run("tag", img); code != 2 {
		t.Errorf("tag PATH alone: exit %d", code)
	}
}

// The filesystem root prints as "/".
func TestTagRemoveRoot(t *testing.T) {
	h, _ := tagTree(t)
	h.mustRun("tag", "/", "r")
	if out := h.mustRun("tag", "remove", "/", "r"); out != "Removed tag #r from /\n" {
		t.Errorf("got %q", out)
	}
}

func TestTagsListRm(t *testing.T) {
	h, tree := tagTree(t)
	if out := h.mustRun("tags", "list"); out != "No tags found\n" {
		t.Errorf("empty: %q", out)
	}
	img := filepath.Join(tree, "photos", "img1.jpg")
	h.mustRun("tag", img, "vacation", "photos", "a-very-long-tag")
	h.mustRun("tag", filepath.Join(tree, "photos"), "vacation")

	want := "Found 3 tag(s):\n\n" +
		"Tag                  Files      Dirs\n" +
		strings.Repeat("-", 36) + "\n" +
		"#a-very-long-tag         1         0\n" +
		"#photos                  1         0\n" +
		"#vacation                1         1\n" +
		"\nTotal: 3 tag(s), 3 file(s), 1 dir(s)\n"
	if out := h.mustRun("tags", "list"); out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}

	if out := h.mustRun("tags", "rm", "vacation"); out != "Deleted tag #vacation and removed 2 association(s)\n" {
		t.Errorf("rm: %q", out)
	}
	if code, _, errOut := h.run("tags", "rm", "vacation"); code != 1 || errOut != "Error: Tag not found: vacation\n" {
		t.Errorf("rm missing: exit %d %q", code, errOut)
	}
	if code, out, _ := h.run("tags"); code != 1 || !strings.Contains(out, "Usage:") {
		t.Errorf("tags alone: exit %d %q", code, out)
	}
}

func TestFindTagged(t *testing.T) {
	h, tree := tagTree(t)
	photos := filepath.Join(tree, "photos")
	img1, img2 := filepath.Join(photos, "img1.jpg"), filepath.Join(photos, "sub", "img2.jpg")
	h.mustRun("tag", img1, "a", "b")
	h.mustRun("tag", img2, "a", "b")
	h.mustRun("tag", filepath.Join(photos, "sub"), "a", "b")
	h.mustRun("tag", photos, "a") // the search root itself never matches

	if out := h.mustRun("find-tagged", photos, "a", "b"); out != "Found 2 match(es) with all tags #a, #b:\n\n"+img1+"\n"+photos+"/sub/\n" {
		t.Errorf("non-recursive: %q", out)
	}
	if out := h.mustRun("find-tagged", "-r", tree, "b", "a"); out != "Found 3 match(es) with all tags #b, #a:\n\n"+img1+"\n"+img2+"\n"+photos+"/sub/\n" {
		t.Errorf("recursive: %q", out)
	}
	if out := h.mustRun("find-tagged", photos, "a", "a"); !strings.HasPrefix(out, "Found 2 match(es)") {
		t.Errorf("duplicate tag: %q", out)
	}
	// one argument is a tag, searched from the current directory
	t.Chdir(photos)
	if out := h.mustRun("find-tagged", "b"); !strings.Contains(out, img1+"\n") {
		t.Errorf("default path: %q", out)
	}
	code, _, errOut := h.run("find-tagged", photos, "a", "unknown")
	if code != 1 || errOut != "No matches found with all tags #a, #unknown in "+photos+"\n" {
		t.Errorf("unknown tag: exit %d %q", code, errOut)
	}
}
