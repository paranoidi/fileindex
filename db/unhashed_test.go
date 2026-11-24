package db_test

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/paranoidi/fileindex/db"
	"github.com/paranoidi/fileindex/internal/dbtest"
)

// checkUnhashed compares every dir's unhashed_below with a count made here, in
// Go, from the instances and the dir parents. Returns how many dirs have a
// non-zero count, so a test can tell it checked something.
func checkUnhashed(t testing.TB, d *db.DB) int {
	t.Helper()
	parent := map[int64]int64{}
	stored := map[int64]int64{}
	rows, err := d.Query("SELECT id, COALESCE(parent_id, 0), unhashed_below FROM dir")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, p, n int64
		if err := rows.Scan(&id, &p, &n); err != nil {
			t.Fatal(err)
		}
		parent[id], stored[id] = p, n
	}
	rows.Close()
	want := map[int64]int64{}
	rows, err = d.Query("SELECT dir_id FROM instance WHERE file_id IS NULL")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		for ; id != 0; id = parent[id] {
			want[id]++
		}
	}
	rows.Close()
	nonZero := 0
	for id, n := range stored {
		if n != want[id] {
			p, _ := db.BuildDirPath(d, &id)
			t.Errorf("unhashed_below of %s = %d, want %d", p, n, want[id])
		}
		if want[id] != 0 {
			nonZero++
		}
	}
	return nonZero
}

func unhashedOf(t testing.TB, d *db.DB, path string) int64 {
	t.Helper()
	id, found, err := db.DirByPath(d, path)
	if err != nil || !found {
		t.Fatalf("DirByPath(%s) = %v, %v", path, found, err)
	}
	var n int64
	if err := d.QueryRow("SELECT unhashed_below FROM dir WHERE id = ?", *id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The count stays right through every writer, checked against a brute-force
// count after each step.
func TestUnhashedBelowScenario(t *testing.T) {
	defer func(n int64) { *db.PruneBatch = n }(*db.PruneBatch)
	d := dbtest.New(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(1700000000, 0)
	// put writes the file to disk, so a prune keeps it, and returns it as walked.
	put := func(rels ...string) []db.DiscoveredFile {
		var out []db.DiscoveredFile
		for _, rel := range rels {
			full := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(rel), 0o644); err != nil {
				t.Fatal(err)
			}
			out = append(out, db.DiscoveredFile{Dir: filepath.Dir(full), Name: filepath.Base(full), Size: 3, Mtime: mtime})
		}
		return out
	}
	hash := func(rel, content string) db.ScannedFile {
		m, s := md5.Sum([]byte(content)), sha256.Sum256([]byte(content))
		return db.ScannedFile{Dir: filepath.Join(root, filepath.Dir(rel)), Name: filepath.Base(rel), Size: 3, Mtime: mtime,
			MD5: hex.EncodeToString(m[:]), SHA256: hex.EncodeToString(s[:]), Sample: hex.EncodeToString(s[:])}
	}
	fast := func(rel string) db.ScannedFile {
		return db.ScannedFile{Dir: filepath.Join(root, filepath.Dir(rel)), Name: filepath.Base(rel), Size: 3, Mtime: mtime, Sample: "sample"}
	}
	steps := 0
	check := func(step string, wantNonZero bool) {
		t.Helper()
		steps++
		if n := checkUnhashed(t, d); wantNonZero && n == 0 {
			t.Fatalf("%s: no dir counts anything, the step checks nothing", step)
		}
		if t.Failed() {
			t.Fatalf("stopped after %s", step)
		}
	}
	dirID := func(rel string) int64 {
		id, found, err := db.DirByPath(d, filepath.Join(root, rel))
		if err != nil || !found {
			t.Fatalf("dir %s: %v, %v", rel, found, err)
		}
		return *id
	}
	discover := func(files []db.DiscoveredFile) {
		t.Helper()
		if _, err := d.CommitDiscovered(files, nil); err != nil {
			t.Fatal(err)
		}
	}
	scan := func(files ...db.ScannedFile) {
		t.Helper()
		if res, err := d.CommitScanBatch(files); err != nil || len(res.Failed) != 0 {
			t.Fatalf("CommitScanBatch: %+v, %v", res, err)
		}
	}
	inst := func(rel string) int64 {
		id, found, err := d.InstanceID(filepath.Join(root, rel))
		if err != nil || !found {
			t.Fatalf("instance %s: %v, %v", rel, found, err)
		}
		return id
	}
	want := func(rel string, n int64) {
		t.Helper()
		if got := unhashedOf(t, d, filepath.Join(root, rel)); got != n {
			t.Errorf("unhashed under %s = %d, want %d", rel, got, n)
		}
	}

	// walk: ten files, one of them listed twice in the batch as a new row
	files := put("orchard/apple/red.txt", "orchard/apple/green.txt", "orchard/pear/core.txt", "orchard/pear/stem/leaf.txt",
		"orchard/pear/stem/bud.txt", "orchard/plum/ripe.txt", "meadow/clover/petal.txt", "meadow/clover/seed.txt",
		"meadow/daisy/pollen.txt", "meadow/honey.txt")
	discover(append(files, files[0]))
	check("discover", true)
	want("", 10)
	want("orchard/pear", 3)

	discover(files)
	check("rediscover unchanged", true)
	want("", 10)

	// full hashes: one path twice in the batch, and a fast hash on top of an unhashed file
	scan(hash("orchard/apple/red.txt", "one"), hash("orchard/pear/core.txt", "two"), hash("orchard/pear/stem/leaf.txt", "three"),
		hash("orchard/apple/red.txt", "four"), fast("orchard/pear/stem/bud.txt"), fast("orchard/pear/core.txt"))
	check("partial hash", true)
	want("", 7)
	want("orchard/pear/stem", 1)

	// hashed files change: one twice in the batch, one that was never hashed
	discover(put("orchard/apple/red.txt", "orchard/pear/stem/leaf.txt", "orchard/apple/red.txt", "orchard/apple/green.txt"))
	check("rediscover changed", true)
	want("", 9)

	// import hashes an unhashed file, adds a hashed one and updates a hashed one
	put("meadow/clover/imported.txt")
	if _, _, err := d.ImportBatch([]db.ImportRow{
		{MD5: "m1", SHA256: "s1", Size: 3, Path: filepath.Join(root, "orchard/pear/stem"), Filename: "bud.txt"},
		{MD5: "m2", SHA256: "s2", Size: 3, Path: filepath.Join(root, "meadow/clover"), Filename: "imported.txt"},
		{MD5: "m3", SHA256: "s3", Size: 3, Path: filepath.Join(root, "orchard/pear"), Filename: "core.txt"},
		{MD5: "m1", SHA256: "s1", Size: 3, Path: filepath.Join(root, "orchard/pear/stem"), Filename: "bud.txt"},
	}); err != nil {
		t.Fatal(err)
	}
	check("import", true)
	want("", 8)

	scan(hash("meadow/clover/seed.txt", "five"), hash("meadow/clover/petal.txt", "six"), hash("orchard/plum/ripe.txt", "seven"))
	check("hash more", true)
	want("meadow/clover", 0)
	want("", 5)

	// prune: a unhashed and a hashed file, a dir with an unhashed file, in commits of one deletion
	*db.PruneBatch = 1
	for _, rel := range []string{"orchard/apple/green.txt", "orchard/pear/core.txt", "orchard/pear/stem", "meadow/daisy"} {
		if err := os.RemoveAll(filepath.Join(root, rel)); err != nil {
			t.Fatal(err)
		}
	}
	rootID := dirID("")
	if _, _, err := d.PruneDir(context.Background(), root, rootID, nil, func(string) {}, func([]string) { check("prune commit", false) }); err != nil {
		t.Fatal(err)
	}
	check("prune", true)
	want("", 2)
	*db.PruneBatch = 1000

	// RemoveTree: a dir with unhashed files whose parent stays, and one whose empty ancestors go
	discover(put("orchard/pear/stem/leaf.txt", "orchard/pear/core.txt", "scaffold/one/two/three/deep.txt", "scaffold/keep.txt"))
	check("discover again", true)
	if _, err := d.RemoveTree(filepath.Join(root, "orchard/pear")); err != nil {
		t.Fatal(err)
	}
	check("RemoveTree", true)
	if _, err := d.RemoveTree(filepath.Join(root, "scaffold/one/two/three")); err != nil {
		t.Fatal(err)
	}
	check("RemoveTree with empty ancestors", true)
	want("scaffold", 1)

	// RemoveInstance
	if _, err := d.RemoveInstance(filepath.Join(root, "meadow/honey.txt")); err != nil {
		t.Fatal(err)
	}
	check("RemoveInstance unhashed", true)
	if _, err := d.RemoveInstance(filepath.Join(root, "meadow/clover/seed.txt")); err != nil {
		t.Fatal(err)
	}
	check("RemoveInstance hashed", true)

	// MoveInstance: an unhashed file across dirs and within one, and a hashed one
	discover(put("meadow/daisy/pollen.txt", "meadow/clover/stalk.txt"))
	check("discover moves", true)
	for _, mv := range []struct{ from, dir, name string }{
		{"meadow/daisy/pollen.txt", "orchard/plum", "pollen.txt"},
		{"orchard/plum/pollen.txt", "orchard/plum", "nectar.txt"},
		{"meadow/clover/petal.txt", "scaffold/one", "petal.txt"},
		{"scaffold/one/petal.txt", "meadow/clover", "petal.txt"},
	} {
		if err := d.MoveInstance(inst(mv.from), filepath.Join(root, mv.dir), mv.name); err != nil {
			t.Fatal(err)
		}
		check("MoveInstance "+mv.from, true)
	}

	// MoveDir under a sibling that shares ancestors, up to the root, and a rename
	discover(put("orchard/apple/green.txt", "orchard/apple/core/seed.txt"))
	for _, mv := range []struct{ from, dir, name string }{
		{"orchard/apple", "orchard/plum", "apple"},
		{"orchard/plum/apple/core", "orchard/plum/apple", "heart"},
		{"orchard/plum/apple", "", "apple"},
		{"apple", "meadow", "apple"},
	} {
		dst := filepath.Join(root, mv.dir)
		if err := d.MoveDir(dirID(mv.from), dst, mv.name); err != nil {
			t.Fatal(err)
		}
		check("MoveDir "+mv.from, true)
	}

	// MergeDir: nested same-named dirs, then into the parent and into the grandparent
	discover(put("merge/west/x/one.txt", "merge/west/y/two.txt", "merge/west/f1.txt",
		"merge/east/x/three.txt", "merge/east/z/four.txt", "merge/east/f2.txt", "merge/east/x/deep/five.txt"))
	scan(hash("merge/east/x/three.txt", "eight"), hash("merge/west/f1.txt", "nine"))
	check("discover merge", true)
	for _, m := range []struct{ src, dst string }{{"merge/west", "merge/east"}, {"merge/east/z", "merge/east"}, {"merge/east/x/deep", "merge/east"}} {
		if _, _, err := d.MergeDir(dirID(m.src), dirID(m.dst)); err != nil {
			t.Fatal(err)
		}
		check("MergeDir "+m.src, true)
	}
	want("merge", 5)

	if steps < 25 {
		t.Errorf("only %d steps checked", steps)
	}
}

// Merging two dirs that each hold the same nested names counts every level once.
func TestMergeDirNestedCount(t *testing.T) {
	d := dbtest.New(t)
	at := time.Unix(1700000000, 0)
	var files []db.DiscoveredFile
	for _, p := range []string{"/m/left/a/b/lamp", "/m/left/a/b/desk", "/m/right/a/b/chair", "/m/right/a/shelf", "/m/left/a/rug"} {
		files = append(files, db.DiscoveredFile{Dir: filepath.Dir(p), Name: filepath.Base(p), Size: 1, Mtime: at})
	}
	if _, err := d.CommitDiscovered(files, nil); err != nil {
		t.Fatal(err)
	}
	left, _, _ := db.DirByPath(d, "/m/left")
	right, _, _ := db.DirByPath(d, "/m/right")
	if _, _, err := d.MergeDir(*left, *right); err != nil {
		t.Fatal(err)
	}
	checkUnhashed(t, d)
	if got := unhashedOf(t, d, "/m/right/a/b"); got != 3 {
		t.Errorf("unhashed under /m/right/a/b = %d, want 3", got)
	}
	if got := unhashedOf(t, d, "/m"); got != 5 {
		t.Errorf("unhashed under /m = %d, want 5", got)
	}
}

// Recount repairs damaged counts, and ListDir reports a subdir that holds an unhashed file.
func TestRecountAndListDir(t *testing.T) {
	d := dbtest.New(t)
	dbtest.AddFile(t, d, "/m/tulip/bulb.txt", "bulb")
	if _, err := d.CommitDiscovered([]db.DiscoveredFile{{Dir: "/m/rose/thorn", Name: "bud.txt", Size: 1, Mtime: time.Unix(1700000000, 0)}}, nil); err != nil {
		t.Fatal(err)
	}
	if n := checkUnhashed(t, d); n != 4 { // /, m, rose, thorn
		t.Fatalf("dirs with an unhashed file below: %d", n)
	}
	if _, err := d.Exec("UPDATE dir SET unhashed_below = 7 - unhashed_below"); err != nil {
		t.Fatal(err)
	}
	if err := d.Recount(); err != nil {
		t.Fatal(err)
	}
	checkUnhashed(t, d)
	m, _, _ := db.DirByPath(d, "/m")
	_, subdirs, err := d.ListDir(*m)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range subdirs {
		got[s.Name] = s.Unhashed
	}
	if len(got) != 2 || !got["rose"] || got["tulip"] {
		t.Errorf("Unhashed by subdir = %v", got)
	}
}
