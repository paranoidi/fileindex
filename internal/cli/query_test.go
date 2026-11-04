package cli

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/paranoidi/fileindex/db"
	"github.com/paranoidi/fileindex/internal/dbtest"
)

// --- find ---

func findHarness(t *testing.T) (*harness, *db.DB) {
	h := newHarness(t)
	return h, dbtest.Init(t, h.dbPath())
}

func TestFind(t *testing.T) {
	h, d := findHarness(t)
	for _, p := range []string{
		"/m/test.txt", "/m/Test2.TXT", "/m/a.jpg", "/m/b.jpg", "/m/sub/nested.txt",
		"/m/test1.txt", "/m/test22.txt", "/m/IMG_1.jpg", "/m/IMGx1.jpg", "/m/50%.jpg", "/m/äx.jpg",
	} {
		dbtest.AddFile(t, d, p, p)
	}
	db.GetOrCreatePath(d, "/m/TestDir")

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"test.txt", "/m"}, "/m/test.txt\n"},
		{[]string{"*.jpg", "/m/sub"}, ""},
		{[]string{"nested.txt", "/m"}, "/m/sub/nested.txt\n"},
		{[]string{"test?.txt", "/m"}, "/m/Test2.TXT\n/m/test1.txt\n"},
		{[]string{"test2.txt", "/m"}, "/m/Test2.TXT\n"},                                                  // case-insensitive
		{[]string{"testdir", "/m"}, "/m/TestDir/\n"},                                                     // dirs too, with a trailing /
		{[]string{"TESTDIR", "/m", "-t", "d"}, "/m/TestDir/\n"},                                          // dirs only
		{[]string{"test*", "/m", "-t", "f"}, "/m/Test2.TXT\n/m/test.txt\n/m/test1.txt\n/m/test22.txt\n"}, // files only, byte order
		{[]string{"IMG_*", "/m"}, "/m/IMG_1.jpg\n"},                                                      // _ is literal
		{[]string{"50%.jpg", "/m"}, "/m/50%.jpg\n"},                                                      // % is literal
		{[]string{"Ä*", "/m"}, "/m/äx.jpg\n"},                                                            // non-ASCII case
		{[]string{"m", "/m"}, ""},                                                                        // the root itself is never a result
		{[]string{"*mgx*", "/m"}, "/m/IMGx1.jpg\n"},                                                      // trigram index path
		{[]string{"*mg_1*", "/m"}, "/m/IMG_1.jpg\n"},                                                     // trigram path, _ still literal
		{[]string{"*ÄX.JPG", "/m"}, "/m/äx.jpg\n"},                                                       // trigram path, non-ASCII case
		{[]string{"*.t?t", "/m/sub"}, "/m/sub/nested.txt\n"},                                             // no 3-char literal run: plain LIKE
	} {
		code, out, _ := h.run(append([]string{"find"}, c.args...)...)
		if out != c.want || (code == 0) != (c.want != "") {
			t.Errorf("find %v: exit %d, got %q, want %q", c.args, code, out, c.want)
		}
	}

	// files come before dirs
	db.GetOrCreatePath(d, "/m/a.jpg.d")
	if out := h.mustRun("find", "a.jpg*", "/m"); out != "/m/a.jpg\n/m/a.jpg.d/\n" {
		t.Errorf("order: %q", out)
	}
}

func TestFindWords(t *testing.T) {
	h, d := findHarness(t)
	for _, p := range []string{"/m/Holiday_Beach 2024.jpg", "/m/sub/holiday-snow.png", "/m/Café menu.pdf", "/m/screenshot.png"} {
		dbtest.AddFile(t, d, p, p)
	}
	db.GetOrCreatePath(d, "/m/holiday") // dirs are not word-indexed
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"holiday", "/m", "-w"}, "/m/Holiday_Beach 2024.jpg\n/m/sub/holiday-snow.png\n"},
		{[]string{"holiday NOT snow", "/m", "-w"}, "/m/Holiday_Beach 2024.jpg\n"},
		{[]string{"scr*", "/m", "--words"}, "/m/screenshot.png\n"},
		{[]string{"cafe", "/m", "-w"}, "/m/Café menu.pdf\n"},                  // diacritics ignored
		{[]string{`"beach 2024"`, "/m", "-w"}, "/m/Holiday_Beach 2024.jpg\n"}, // phrase
		{[]string{"holiday", "/m/sub", "-w"}, "/m/sub/holiday-snow.png\n"},    // limited to the path
		{[]string{"shot", "/m", "-w"}, ""},                                    // whole words only
	} {
		code, out, _ := h.run(append([]string{"find"}, c.args...)...)
		if out != c.want || (code == 0) != (c.want != "") {
			t.Errorf("find %v: exit %d, got %q, want %q", c.args, code, out, c.want)
		}
	}
	if code, _, errOut := h.run("find", `"unterminated`, "/m", "-w"); code != 2 || !strings.Contains(errOut, "invalid word query") {
		t.Errorf("syntax error: exit %d, %q", code, errOut)
	}
	if code, _, _ := h.run("find", "holiday", "/m", "-w", "-t", "d"); code != 2 {
		t.Errorf("-w -t d: exit %d", code)
	}
}

func TestFindOutdatedSchema(t *testing.T) {
	h, d := findHarness(t)
	if _, err := d.Exec("INSERT INTO schema_version (version) VALUES (6)"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("DELETE FROM schema_version WHERE version = 10"); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := h.run("find", "*", "/m"); code != 1 || !strings.Contains(errOut, "run `fileindex upgrade-db`") {
		t.Errorf("exit %d, %q", code, errOut)
	}
}

func TestFindErrors(t *testing.T) {
	h, d := findHarness(t)
	dbtest.AddFile(t, d, "/m/a.jpg", "a")
	code, _, errOut := h.run("find", "zzz", "/m")
	if code != 1 || errOut != "No matches found for pattern 'zzz' in /m\n" {
		t.Errorf("no match: exit %d, %q", code, errOut)
	}
	if code, _, _ := h.run("find", "*", "/nonexistent"); code != 1 {
		t.Errorf("unknown path: exit %d", code)
	}
	if code, _, _ := h.run("find", "*", "/m", "-t", "x"); code != 2 {
		t.Errorf("-t x: exit %d", code)
	}
}

func TestFindVerifyDetails(t *testing.T) {
	h, d := findHarness(t)
	tree := t.TempDir()
	writeFile(t, filepath.Join(tree, "here.jpg"), "abc")
	dbtest.AddFile(t, d, filepath.Join(tree, "here.jpg"), "abc")
	dbtest.AddFile(t, d, filepath.Join(tree, "gone.jpg"), "x")

	if out := h.mustRun("find", "*.jpg", tree, "--verify"); out != "✘ "+tree+"/gone.jpg\n"+tree+"/here.jpg\n" {
		t.Errorf("verify: %q", out)
	}
	want := "Found 1 match(es):\n\n✔ " + tree + "/here.jpg\n  Type: file\n  Size: 3 bytes\n  Modified: " + dbtest.Mtime + "\n\n"
	if out := h.mustRun("find", "here.jpg", tree, "--verify", "--details"); out != want {
		t.Errorf("details: got %q want %q", out, want)
	}
}

// --- match ---

func sum(f func([]byte) []byte, s string) string { return hex.EncodeToString(f([]byte(s))) }
func md5Of(b []byte) []byte                      { s := md5.Sum(b); return s[:] }
func sha256Of(b []byte) []byte                   { s := sha256.Sum256(b); return s[:] }

func TestMatchByHash(t *testing.T) {
	h, d := findHarness(t)
	dbtest.AddFile(t, d, "/m/b.jpg", "content")
	dbtest.AddFile(t, d, "/m/a.jpg", "content")
	for _, hash := range []string{sum(md5Of, "content"), sum(sha256Of, "content")} {
		if out := h.mustRun("match", "--hash", hash); out != "/m/a.jpg\n/m/b.jpg\n" {
			t.Errorf("--hash %s: %q", hash, out)
		}
		if out := h.mustRun("match", hash); out != "/m/a.jpg\n/m/b.jpg\n" { // positional
			t.Errorf("positional: %q", out)
		}
	}
	if out := h.mustRun("match", "--hash", sum(md5Of, "content"), "--short"); out != "2\n" {
		t.Errorf("--short: %q", out)
	}
	code, out, errOut := h.run("match", "--hash", "bogus", "--short")
	if code != 1 || out != "0\n" {
		t.Errorf("bogus --short: exit %d %q %q", code, out, errOut)
	}
	code, _, errOut = h.run("match", "/no/such/file.jpg")
	if code != 1 || errOut != "No files found matching the provided hash\n" {
		t.Errorf("nonexistent path: exit %d %q", code, errOut)
	}
	out = h.mustRun("match", "--hash", sum(md5Of, "content"), "--details")
	if !strings.HasPrefix(out, "Found 2 file instance(s):\n\nFile: /m/a.jpg\nSize: 7 bytes\nMD5: ") ||
		!strings.Contains(out, "Modified: "+dbtest.Mtime+"\n") {
		t.Errorf("--details: %q", out)
	}
}

func TestMatchSingleFile(t *testing.T) {
	h, _, tree := scanHarness(t)
	writeFile(t, filepath.Join(tree, "a.jpg"), "same")
	writeFile(t, filepath.Join(tree, "b.jpg"), "same")
	writeFile(t, filepath.Join(tree, "lonely.jpg"), "unique")
	h.mustRun("scan", tree)

	// the file itself is filtered out; only the other copy matches
	out := h.mustRun("match", filepath.Join(tree, "a.jpg"))
	want := tree + "/a.jpg\n  Hash (SHA256): " + sum(sha256Of, "same") + "\n  ✔ Found 1 match(es) in database:\n    " + tree + "/b.jpg (4 bytes)\n"
	if out != want {
		t.Errorf("got %q\nwant %q", out, want)
	}
	if out := h.mustRun("match", filepath.Join(tree, "a.jpg"), "--short"); out != "1\n" {
		t.Errorf("--short: %q", out)
	}

	// no match exits 1, but --short still exits 0 (match-count relies on it)
	code, out, _ := h.run("match", filepath.Join(tree, "lonely.jpg"))
	if code != 1 || !strings.Contains(out, "  ✘ No matches found in database\n") {
		t.Errorf("no match: exit %d %q", code, out)
	}
	if code, out, _ := h.run("match", filepath.Join(tree, "lonely.jpg"), "--short"); code != 0 || out != "0\n" {
		t.Errorf("no match --short: exit %d %q", code, out)
	}

	// --fast uses the first-8KiB hash
	if out := h.mustRun("match", filepath.Join(tree, "a.jpg"), "-f"); !strings.Contains(out, "Hash (SHA256 (8KB)): ") {
		t.Errorf("--fast: %q", out)
	}
}

func fakeJPEG(exif string) string {
	n := len(exif) + 2
	return "\xff\xd8\xff\xe1" + string([]byte{byte(n >> 8), byte(n)}) + exif + "\xff\xda\x00\x02image data\xff\xd9"
}

func TestMatchJPEGIgnoresMetadata(t *testing.T) {
	h, _, tree := scanHarness(t)
	writeFile(t, filepath.Join(tree, "db", "a.jpg"), fakeJPEG("camera A"))
	h.mustRun("scan", filepath.Join(tree, "db"))
	writeFile(t, filepath.Join(tree, "new", "b.jpg"), fakeJPEG("camera B, edited"))
	out := h.mustRun("match", filepath.Join(tree, "new", "b.jpg"))
	if !strings.Contains(out, "Hash (SHA256 (no EXIF)): ") || !strings.Contains(out, tree+"/db/a.jpg") {
		t.Errorf("got %q", out)
	}
}

// A broken symlink is reported as an unreadable path, not looked up as a hash.
func TestMatchBrokenSymlink(t *testing.T) {
	h, _, tree := scanHarness(t)
	link := filepath.Join(tree, "broken.jpg")
	os.Symlink(filepath.Join(tree, "nowhere"), link)
	code, _, errOut := h.run("match", link)
	if code != 1 || errOut != "Error: Error calculating hashes: File not found: "+link+"\n" {
		t.Errorf("exit %d, %q", code, errOut)
	}
}

func TestMatchDirectory(t *testing.T) {
	h, _, tree := scanHarness(t)
	writeFile(t, filepath.Join(tree, "db", "x.jpg"), "x")
	h.mustRun("scan", filepath.Join(tree, "db"))
	in := filepath.Join(tree, "in")
	writeFile(t, filepath.Join(in, "copy.jpg"), "x")
	writeFile(t, filepath.Join(in, "new.txt"), "n") // any file, no patterns apply
	writeFile(t, filepath.Join(in, "sub", "deep.jpg"), "x")
	syscall.Mkfifo(filepath.Join(in, "sub", "fifo"), 0o644)
	os.Symlink(filepath.Join(tree, "nowhere"), filepath.Join(in, "sub", "broken"))

	out := h.mustRun("match", in)
	want := "✔ copy.jpg (1 bytes)\n  Found 1 match(es):\n    " + tree + "/db/x.jpg\n\n" +
		"✘ new.txt\n  No matches in database\n\n" +
		"Summary:\n  Total files: 2\n  Matched: 1\n  Not matched: 1\n"
	if out != want {
		t.Errorf("got %q\nwant %q", out, want)
	}
	// -r skips the fifo and the broken link instead of counting errors
	if out := h.mustRun("match", in, "-r", "--short"); out != "2\n" {
		t.Errorf("-r --short: %q", out)
	}
	// --abort-depth implies -r and aborts on the first file past the depth
	if code, out, errOut := h.run("match", in, "--abort-depth", "1"); code != 1 || !strings.HasSuffix(errOut, "Error: Abort depth 1 exceeded: sub/deep.jpg\n") || !strings.Contains(out, "✔ copy.jpg") {
		t.Errorf("--abort-depth 1: exit %d %q %q", code, out, errOut)
	}
	if out := h.mustRun("match", in, "--abort-depth", "2", "--short"); out != "2\n" {
		t.Errorf("--abort-depth 2: %q", out)
	}
	// the files already indexed in db/ match each other only if copies exist
	if code, out, _ := h.run("match", filepath.Join(tree, "db"), "--short"); code != 0 || out != "0\n" {
		t.Errorf("self-filter in dir mode: exit %d %q", code, out)
	}

	empty := filepath.Join(tree, "empty")
	os.Mkdir(empty, 0o755)
	if code, out, _ := h.run("match", empty, "--short"); code != 1 || out != "0\n" {
		t.Errorf("empty --short: exit %d %q", code, out)
	}
	if code, _, errOut := h.run("match", empty); code != 1 || errOut != "No files found in "+empty+"\n" {
		t.Errorf("empty: exit %d %q", code, errOut)
	}
}

// --- ls ---

func TestLs(t *testing.T) {
	h, d, tree := scanHarness(t)
	t.Setenv("FILEINDEX_SCANNING_FILES", "*.jpg")
	writeFile(t, filepath.Join(tree, "a.jpg"), "aaa")
	writeFile(t, filepath.Join(tree, "sub", "x.jpg"), "x")
	h.mustRun("scan", tree)
	inst := 0
	d.QueryRow("SELECT id FROM instance WHERE filename = 'a.jpg'").Scan(&inst)
	d.Exec("INSERT INTO tag (name) VALUES ('red'), ('blue')")
	d.Exec("INSERT INTO file_tag (tag_id, instance_id) SELECT id, ? FROM tag", inst)
	sub, _, _ := db.DirByPath(d, filepath.Join(tree, "sub"))
	d.Exec("INSERT INTO dir_tag (tag_id, dir_id) SELECT id, ? FROM tag WHERE name = 'red'", *sub)

	fi, _ := os.Stat(filepath.Join(tree, "a.jpg"))
	mtime := db.FormatMtime(fi.ModTime())
	want := "Contents of " + tree + ":\n\nDirectories:\n  sub/ #red\n\nFiles:\n" +
		"  a.jpg                                       3 bytes  " + mtime + " #blue #red\n" +
		"\nTotal: 1 file(s), 3 bytes\n"
	if out := h.mustRun("ls", tree); out != want {
		t.Errorf("got %q\nwant %q", out, want)
	}

	// differences: one file gone, three new and one excluded by pattern
	os.Remove(filepath.Join(tree, "a.jpg"))
	writeFile(t, filepath.Join(tree, "b.jpg"), "b")
	writeFile(t, filepath.Join(tree, "c.jpg"), "c")
	writeFile(t, filepath.Join(tree, "notes.txt"), "n")
	writeFile(t, filepath.Join(tree, "empty.jpg"), "")
	code, out, _ := h.run("ls", tree)
	if code != 1 || !strings.HasSuffix(out, "\n✘ Missing files from filesystem (1):\n  a.jpg\n\n∆ Missing files from database (3):\n  b.jpg\n  c.jpg\n  empty.jpg\n\n") {
		t.Errorf("exit %d, %q", code, out)
	}

	// a dir with only subdirs in the DB still reports new files
	os.Remove(filepath.Join(tree, "b.jpg"))
	os.Remove(filepath.Join(tree, "c.jpg"))
	h.mustRun("scan", tree)
	writeFile(t, filepath.Join(tree, "sub2", "keep", "k.jpg"), "k")
	h.mustRun("scan", filepath.Join(tree, "sub2"))
	writeFile(t, filepath.Join(tree, "sub2", "new.jpg"), "new")
	code, out, _ = h.run("ls", filepath.Join(tree, "sub2"))
	if code != 1 || !strings.Contains(out, "(no files)\n\n∆ Missing files from database (1):\n  new.jpg\n") {
		t.Errorf("only subdirs: exit %d, %q", code, out)
	}

	code, _, errOut := h.run("ls", "/nonexistent/x")
	if code != 1 || errOut != "Error: Path '/nonexistent/x' not found in database\n" {
		t.Errorf("not found: exit %d %q", code, errOut)
	}
}
