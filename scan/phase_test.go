package scan_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paranoidi/fileindex/db"
	"github.com/paranoidi/fileindex/scan"
)

func countNull(t *testing.T, d *db.DB) int {
	t.Helper()
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM instance WHERE file_id IS NULL").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// SkipHash indexes files by name only; a later run hashes them without any change.
func TestSkipHashThenRescanHashes(t *testing.T) {
	ctx := context.Background()
	d, tree := newIndex(t)
	write(t, tree, "orchard/lantern.txt", "orchard/meadow.txt", "harbor/anchor.txt")

	if _, err := scan.Run(ctx, d, []string{tree}, scan.Options{MissingOnly: true, SkipHash: true}, &recorder{}); err != nil {
		t.Fatal(err)
	}
	root, _, _ := db.DirByPath(d, tree)
	found, err := d.FindByWords("lantern", *root)
	if err != nil || len(found) != 1 || found[0].Hashed || found[0].Size != int64(len("orchard/lantern.txt")) {
		t.Fatalf("FindByWords = %+v, %v", found, err)
	}
	if n := countNull(t, d); n != 3 {
		t.Fatalf("unhashed = %d, want 3", n)
	}

	st, err := scan.Run(ctx, d, []string{tree}, scan.Options{MissingOnly: true}, &recorder{})
	if err != nil || st.Processed != 3 || st.Added != 0 {
		t.Fatalf("rescan = %+v, %v", st, err)
	}
	if n := countNull(t, d); n != 0 {
		t.Fatalf("unhashed after rescan = %d", n)
	}
	// and now everything is skipped
	st, _ = scan.Run(ctx, d, []string{tree}, scan.Options{MissingOnly: true}, &recorder{})
	if st.Processed != 0 || st.Skipped != 3 {
		t.Fatalf("third run = %+v", st)
	}
}

// A changed file loses its hash at discovery, and gets the new one when hashed.
func TestContentChangeResetsFileID(t *testing.T) {
	ctx := context.Background()
	d, tree := newIndex(t)
	write(t, tree, "pebble.txt")
	opt := scan.Options{MissingOnly: true}
	if _, err := scan.Run(ctx, d, []string{tree}, opt, &recorder{}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(tree, "pebble.txt")
	if err := os.WriteFile(p, []byte("a longer body"), 0o644); err != nil {
		t.Fatal(err)
	}
	opt.SkipHash = true
	if _, err := scan.Run(ctx, d, []string{tree}, opt, &recorder{}); err != nil {
		t.Fatal(err)
	}
	info, found, err := d.FileInfo(p)
	if err != nil || !found || info.Hashed || info.MD5 != "" || info.Size != 13 {
		t.Fatalf("FileInfo after change = %+v, %v, %v", info, found, err)
	}
	opt.SkipHash = false
	if _, err := scan.Run(ctx, d, []string{tree}, opt, &recorder{}); err != nil {
		t.Fatal(err)
	}
	if info, _, _ = d.FileInfo(p); !info.Hashed || info.Size != 13 {
		t.Fatalf("FileInfo after rehash = %+v", info)
	}
}

// cancelAt cancels once hashing has started.
type cancelOnWorker struct {
	recorder
	cancel context.CancelFunc
}

func (c *cancelOnWorker) Worker(int, string) { c.cancel() }

// Cancelling during hashing leaves committed rows consistent; a rescan finishes.
func TestCancelDuringHashThenRescan(t *testing.T) {
	d, tree := newIndex(t)
	write(t, tree, "a/one", "a/two", "a/three", "a/four", "a/five")
	ctx, cancel := context.WithCancel(context.Background())
	rec := &cancelOnWorker{cancel: cancel}
	opt := scan.Options{MissingOnly: true, Workers: 1, BatchFiles: 1}
	st, err := scan.Run(ctx, d, []string{tree}, opt, rec)
	if err != nil || !st.Interrupted {
		t.Fatalf("cancelled run = %+v, %v", st, err)
	}
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM instance").Scan(&n); err != nil || n != 5 {
		t.Fatalf("instances = %d, %v (discovery must be committed)", n, err)
	}
	st, err = scan.Run(context.Background(), d, []string{tree}, opt, &recorder{})
	if err != nil || st.Interrupted || countNull(t, d) != 0 {
		t.Fatalf("rescan = %+v, %v, unhashed %d", st, err, countNull(t, d))
	}
}

// phaseRecorder keeps the order in which stages were announced; repeated
// calls of one kind are collapsed.
type phaseRecorder struct {
	recorder
	indexed []int
	toHash  int
	order   []string
	cancel  context.CancelFunc // called on the first Prune
}

func (p *phaseRecorder) mark(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n := len(p.order); n == 0 || p.order[n-1] != s {
		p.order = append(p.order, s)
	}
}

func (p *phaseRecorder) Indexed(n int)              { p.indexed = append(p.indexed, n); p.mark("indexed") }
func (p *phaseRecorder) Discovered(scan.Stats)      { p.mark("discovered") }
func (p *phaseRecorder) Pruned(_ scan.Stats, n int) { p.toHash = n; p.mark("pruned") }
func (p *phaseRecorder) Hashed(st scan.Stats)       { p.mark("hashed"); p.recorder.Hashed(st) }
func (p *phaseRecorder) Worker(int, string)         { p.mark("worker") }
func (p *phaseRecorder) Prune(dir string, n int) {
	p.mark("prune")
	if p.cancel != nil {
		p.cancel()
	}
	p.recorder.Prune(dir, n)
}

func TestPhaseReporterOrder(t *testing.T) {
	d, tree := newIndex(t)
	write(t, tree, "x/pear", "x/plum", "y/fig")
	rec := &phaseRecorder{}
	if _, err := scan.Run(context.Background(), d, []string{tree}, scan.Options{MissingOnly: true, BatchFiles: 2}, rec); err != nil {
		t.Fatal(err)
	}
	want := "indexed discovered prune pruned worker hashed"
	if got := strings.Join(rec.order, " "); got != want {
		t.Fatalf("order = %q, want %q", got, want)
	}
	if rec.toHash != 3 || rec.indexed[len(rec.indexed)-1] != 3 {
		t.Fatalf("indexed %v, toHash %d", rec.indexed, rec.toHash)
	}
}

// SkipHash is walk + prune; toHash does not count rows pruning removed.
func TestSkipHashOrderAndPrunedNotCounted(t *testing.T) {
	ctx := context.Background()
	d, tree := newIndex(t)
	write(t, tree, "x/pear", "x/plum", "y/fig")
	skip := scan.Options{MissingOnly: true, SkipHash: true}
	if _, err := scan.Run(ctx, d, []string{tree}, skip, &recorder{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(tree, "x/pear")); err != nil {
		t.Fatal(err)
	}
	rec := &phaseRecorder{}
	st, err := scan.Run(ctx, d, []string{tree}, skip, rec)
	if err != nil || st.PrunedFiles != 1 {
		t.Fatalf("run = %+v, %v", st, err)
	}
	if got := strings.Join(rec.order, " "); got != "indexed discovered prune pruned hashed" {
		t.Fatalf("order = %q", got)
	}
	if rec.toHash != 2 {
		t.Fatalf("toHash = %d, want 2 (the pruned row is gone)", rec.toHash)
	}
	if n := countNull(t, d); n != 2 {
		t.Fatalf("unhashed = %d", n)
	}
}

// A cancel while pruning skips hashing and reports Interrupted.
func TestCancelDuringPruneSkipsHashing(t *testing.T) {
	d, tree := newIndex(t)
	write(t, tree, "x/pear", "x/plum")
	ctx, cancel := context.WithCancel(context.Background())
	rec := &phaseRecorder{cancel: cancel}
	st, err := scan.Run(ctx, d, []string{tree}, scan.Options{MissingOnly: true}, rec)
	if err != nil || !st.Interrupted || st.Processed != 0 {
		t.Fatalf("run = %+v, %v", st, err)
	}
	if got := strings.Join(rec.order, " "); got != "indexed discovered prune hashed" {
		t.Fatalf("order = %q", got)
	}
	if n := countNull(t, d); n != 2 {
		t.Fatalf("unhashed = %d, want 2", n)
	}
}

// Everything walked is recorded unhashed with --all, even unchanged files.
func TestAllRehashesEverything(t *testing.T) {
	ctx := context.Background()
	d, tree := newIndex(t)
	write(t, tree, "one", "two")
	if _, err := scan.Run(ctx, d, []string{tree}, scan.Options{MissingOnly: true}, &recorder{}); err != nil {
		t.Fatal(err)
	}
	st, err := scan.Run(ctx, d, []string{tree}, scan.Options{}, &recorder{})
	if err != nil || st.Processed != 2 || st.Updated != 2 {
		t.Fatalf("--all run = %+v, %v", st, err)
	}
}
