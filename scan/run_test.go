package scan_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paranoidi/fileindex/config"
	"github.com/paranoidi/fileindex/db"
	"github.com/paranoidi/fileindex/scan"
)

// recorder is a Reporter that keeps what it was told.
type recorder struct {
	mu      sync.Mutex
	hashed  []scan.Stats
	pruned  []string
	logs    []string
	onPrune func()
}

func (r *recorder) Worker(int, string)         {}
func (r *recorder) Speed(int, float64)         {}
func (r *recorder) Scan(string, int, int, int) {}
func (r *recorder) Hashed(st scan.Stats)       { r.mu.Lock(); r.hashed = append(r.hashed, st); r.mu.Unlock() }
func (r *recorder) Log(msg string)             { r.mu.Lock(); r.logs = append(r.logs, msg); r.mu.Unlock() }
func (r *recorder) Prune(dir string, checked int) {
	r.mu.Lock()
	r.pruned = append(r.pruned, dir)
	r.mu.Unlock()
	if r.onPrune != nil {
		r.onPrune()
	}
}

// newIndex opens and initialises a DB in a temp dir and returns it with a
// resolved temp tree to scan.
func newIndex(t *testing.T) (*db.DB, string) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Init(); err != nil {
		t.Fatal(err)
	}
	tree, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d, tree
}

func write(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The calls paras-commander makes, through the public packages only.
func TestLibraryAPI(t *testing.T) {
	ctx := context.Background()
	d, tree := newIndex(t)

	cfgPath := filepath.Join(t.TempDir(), "config.ini")
	cfg, err := config.Read(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatalf("config.Read wrote %s", cfgPath)
	}
	rules, err := scan.RulesFromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	workers, err := cfg.ParallelWorkers()
	if err != nil {
		t.Fatal(err)
	}

	write(t, tree, "orchard/pebble.jpg", "orchard/lantern.jpg", "harbor/meadow.jpg", "harbor/notes.txt")
	rec := &recorder{}
	st, err := scan.Run(ctx, d, []string{tree}, scan.Options{Rules: rules, Workers: workers, MissingOnly: true}, rec)
	if err != nil {
		t.Fatal(err)
	}
	if st.Added != 4 || st.Interrupted || len(rec.hashed) != 1 || len(rec.pruned) == 0 {
		t.Fatalf("stats %+v, hashed %d, pruned %v", st, len(rec.hashed), rec.pruned)
	}

	orchard := filepath.Join(tree, "orchard")
	pebble := filepath.Join(orchard, "pebble.jpg")
	if err := os.Remove(pebble); err != nil {
		t.Fatal(err)
	}
	// tag a file gone from disk, and the dir, by what the index holds
	for _, p := range []string{pebble, orchard} {
		target, err := d.IndexTagTarget(p, false)
		if err != nil {
			t.Fatal(err)
		}
		if n, err := d.AddTags(target, []string{"velvet", "amber"}); err != nil || n != 2 {
			t.Fatalf("AddTags %s: %d, %v", p, n, err)
		}
	}
	if _, err := d.IndexTagTarget(pebble, true); err == nil {
		t.Error("recursive on a file: want error")
	}

	id, found, err := db.DirByPath(d, orchard)
	if err != nil || !found {
		t.Fatalf("DirByPath: %v %v", found, err)
	}
	files, _, err := d.ListDir(*id)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[1].Name != "pebble.jpg" || !slices.Equal(files[1].Tags, []string{"amber", "velvet"}) || files[0].Tags != nil {
		t.Fatalf("ListDir: %+v", files)
	}
	rootID, _, _ := db.DirByPath(d, tree)
	_, subdirs, err := d.ListDir(*rootID)
	if err != nil || len(subdirs) != 2 || subdirs[0].Tags != nil || !slices.Equal(subdirs[1].Tags, []string{"amber", "velvet"}) {
		t.Fatalf("subdirs %+v, %v", subdirs, err)
	}
	tags, err := d.EntryTags(tree, []string{"orchard", "harbor", "missing"})
	if err != nil || len(tags) != 1 || !slices.Equal(tags["orchard"], []string{"amber", "velvet"}) {
		t.Fatalf("EntryTags: %v, %v", tags, err)
	}

	target, err := d.IndexTagTarget(pebble, false)
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := d.RemoveTags(target, []string{"velvet"}); err != nil || len(removed) != 1 || removed[0].Path != pebble {
		t.Fatalf("RemoveTags: %+v, %v", removed, err)
	}

	// move a dir in the index only: willow exists nowhere on disk
	willow := filepath.Join(tree, "willow")
	if m, err := d.Move(orchard, willow, nil); err != nil || m.Dirs != 1 {
		t.Fatalf("Move: %+v, %v", m, err)
	}
	if _, err := d.Move(willow, filepath.Join(willow, "nested"), nil); err == nil {
		t.Error("dir into itself: want error")
	}
	if _, found, _ := db.DirByPath(d, willow); !found {
		t.Fatal("willow not indexed after Move")
	}
	if _, found, _ := db.DirByPath(d, orchard); found {
		t.Fatal("orchard still indexed after Move")
	}

	r, err := d.RemoveTree(willow)
	if err != nil || r.Instances != 2 || r.Files != 2 || r.Dirs != 1 {
		t.Fatalf("RemoveTree: %+v, %v", r, err)
	}
	if _, err := d.RemoveTree(willow); err == nil {
		t.Error("RemoveTree of a gone dir: want error")
	}
}

// A cancel during prune stops between dirs and leaves no orphan rows.
func TestRunCancelDuringPrune(t *testing.T) {
	d, tree := newIndex(t)
	names := []string{"acorn", "bramble", "cedar", "dune", "ember"}
	for _, n := range names {
		write(t, tree, n+"/"+n+".jpg")
	}
	if _, err := scan.Run(context.Background(), d, []string{tree}, scan.Options{}, &recorder{}); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.Remove(filepath.Join(tree, n, n+".jpg")); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &recorder{onPrune: cancel}
	st, err := scan.Run(ctx, d, []string{tree}, scan.Options{}, rec)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Interrupted || len(rec.pruned) != 1 || st.PrunedFiles != 0 {
		t.Fatalf("stats %+v, pruned dirs %v", st, rec.pruned)
	}
	count := func(q string) int {
		var n int
		if err := d.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count("SELECT COUNT(*) FROM file WHERE id NOT IN (SELECT file_id FROM instance)"); n != 0 {
		t.Errorf("%d orphan file rows", n)
	}
	if n := count("SELECT COUNT(*) FROM instance"); n != len(names) {
		t.Errorf("%d instances left, want %d untouched", n, len(names))
	}

	// a run to the end finishes the job
	st, err = scan.Run(context.Background(), d, []string{tree}, scan.Options{}, &recorder{})
	if err != nil || st.Interrupted || st.PrunedFiles != int64(len(names)) {
		t.Fatalf("rerun: %+v, %v", st, err)
	}
	if n := count("SELECT COUNT(*) FROM file"); n != 0 {
		t.Errorf("%d file rows left", n)
	}
}

func TestRequireSchemaNewer(t *testing.T) {
	d, _ := newIndex(t)
	if _, err := d.Exec("INSERT INTO schema_version (version) VALUES (?)", db.SchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := d.RequireSchema(); !errors.Is(err, db.ErrNewerSchema) {
		t.Errorf("RequireSchema = %v, want ErrNewerSchema", err)
	}
	if _, err := scan.Run(context.Background(), d, []string{t.TempDir()}, scan.Options{}, &recorder{}); !errors.Is(err, db.ErrNewerSchema) {
		t.Errorf("Run = %v, want ErrNewerSchema", err)
	}
}

// rootRecorder also records RootDone, checking each root's files are already
// committed when it is reported.
type rootRecorder struct {
	recorder
	t     *testing.T
	d     *db.DB
	dirs  []string // root dirs to check, "" = skip
	want  []int    // files expected under dirs[i]
	roots []int
}

func (r *rootRecorder) RootDone(i int) {
	if r.dirs[i] != "" {
		got, err := r.d.DirFiles(r.dirs[i])
		if err != nil || len(got) != r.want[i] {
			r.t.Errorf("RootDone(%d): %d files committed (err %v), want %d", i, len(got), err, r.want[i])
		}
	}
	r.mu.Lock()
	r.roots = append(r.roots, i)
	r.mu.Unlock()
}

// Every root is reported done once, after its files are committed, including
// a root with nothing to hash.
func TestRunReportsRootsDone(t *testing.T) {
	d, tree := newIndex(t)
	write(t, tree, "harbor/anchor", "harbor/rope", "lantern", "meadow/clover")
	opt := scan.Options{MissingOnly: true, BatchFiles: 1}
	meadow := filepath.Join(tree, "meadow")
	if _, err := scan.Run(context.Background(), d, []string{meadow}, opt, &recorder{}); err != nil {
		t.Fatal(err)
	}
	rec := &rootRecorder{t: t, d: d,
		dirs: []string{filepath.Join(tree, "harbor"), "", meadow},
		want: []int{2, 0, 1},
	}
	args := []string{filepath.Join(tree, "harbor"), filepath.Join(tree, "lantern"), meadow}
	if _, err := scan.Run(context.Background(), d, args, opt, rec); err != nil {
		t.Fatal(err)
	}
	slices.Sort(rec.roots)
	if !slices.Equal(rec.roots, []int{0, 1, 2}) {
		t.Fatalf("RootDone = %v, want each of 0 1 2 once", rec.roots)
	}
}

// indexCounter counts discovery commits.
type indexCounter struct {
	recorder
	indexed []int
}

func (c *indexCounter) Indexed(n int)          { c.indexed = append(c.indexed, n) }
func (c *indexCounter) Discovered(scan.Stats)  {}
func (c *indexCounter) Pruned(scan.Stats, int) {}

// BatchInterval replaces the file count: with a long interval a root's files
// land in one commit however small BatchFiles is.
func TestRunBatchIntervalReplacesFileCount(t *testing.T) {
	d, tree := newIndex(t)
	write(t, tree, "harbor/anchor", "harbor/rope", "harbor/sail")
	rec := &indexCounter{}
	opt := scan.Options{MissingOnly: true, Workers: 1, BatchFiles: 1, BatchInterval: time.Hour}
	if _, err := scan.Run(context.Background(), d, []string{tree}, opt, rec); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.indexed, []int{3}) {
		t.Fatalf("Indexed = %v, want [3]", rec.indexed)
	}
}

// A scan whose tree holds the index never opens the database files: closing an
// fd on them would drop SQLite's locks in this process.
func TestRunSkipsOwnDatabase(t *testing.T) {
	tree, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(filepath.Join(tree, "state", "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Init(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	write(t, tree, "notes/kettle.txt", "state/index.sqlite-backup")
	st, err := scan.Run(context.Background(), d, []string{tree}, scan.Options{}, &recorder{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	rows, err := d.Query("SELECT filename FROM instance ORDER BY filename")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	if !slices.Equal(names, []string{"index.sqlite-backup", "kettle.txt"}) || st.Skipped < 1 {
		t.Errorf("indexed %q, skipped %d", names, st.Skipped)
	}
}

// doneRecorder counts FileDone calls.
type doneRecorder struct {
	recorder
	done atomic.Int32
}

func (r *doneRecorder) FileDone() { r.done.Add(1) }

// Run reports each hashed file once through FileReporter.
func TestRunReportsFileDone(t *testing.T) {
	d, tree := newIndex(t)
	write(t, tree, "a/one.jpg", "a/two.jpg", "b/three.jpg")
	rec := &doneRecorder{}
	if _, err := scan.Run(context.Background(), d, []string{tree}, scan.Options{}, rec); err != nil {
		t.Fatal(err)
	}
	if n := rec.done.Load(); n != 3 {
		t.Errorf("FileDone called %d times, want 3", n)
	}
}
