package scan_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/paranoidi/fileindex/internal/dbtest"
	"github.com/paranoidi/fileindex/scan"
)

type commit struct {
	stage scan.Stage
	dirs  []string
}

// commitRecorder records each commit report and runs check inside it, as a caller reading
// the index in response would.
type commitRecorder struct {
	recorder
	mu      sync.Mutex
	commits []commit
	check   func(commit)
}

func (r *commitRecorder) Committed(stage scan.Stage, dirs []string) {
	c := commit{stage, slices.Clone(dirs)}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commits = append(r.commits, c)
	if r.check != nil {
		r.check(c)
	}
}

func (r *commitRecorder) dirsOf(stage scan.Stage) []string {
	var out []string
	for _, c := range r.commits {
		if c.stage == stage {
			out = append(out, c.dirs...)
		}
	}
	slices.Sort(out)
	return out
}

func writeAt(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func resolvedTemp(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCommitReporterWalkAndHash(t *testing.T) {
	d := dbtest.New(t)
	root := resolvedTemp(t)
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	writeAt(t, filepath.Join(a, "one.txt"), "1")
	writeAt(t, filepath.Join(a, "two.txt"), "2")
	writeAt(t, filepath.Join(b, "three.txt"), "3")
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := &commitRecorder{}
	r.check = func(c commit) { // the data is readable by the time of the report
		for _, dir := range c.dirs {
			files, err := d.DirFiles(dir)
			if err != nil || len(files) == 0 {
				t.Errorf("stage %v: %s lists %v, %v", c.stage, dir, files, err)
			}
			if c.stage == scan.StageHash {
				hashed := false
				for _, f := range files {
					hashed = hashed || f.Hashed
				}
				if !hashed {
					t.Errorf("hash report for %s before anything there is hashed", dir)
				}
			}
		}
	}
	if _, err := scan.Run(context.Background(), d, []string{root}, scan.Options{}, r); err != nil {
		t.Fatal(err)
	}
	want := []string{a, b} // distinct within a commit; the empty dir has no results to show
	for _, stage := range []scan.Stage{scan.StageWalk, scan.StageHash} {
		if got := r.dirsOf(stage); !slices.Equal(got, want) {
			t.Errorf("stage %v dirs = %v, want %v", stage, got, want)
		}
	}
	if got := r.dirsOf(scan.StagePrune); len(got) != 0 {
		t.Errorf("prune dirs = %v, want none", got)
	}
}

func TestCommitReporterPrune(t *testing.T) {
	d := dbtest.New(t)
	root := resolvedTemp(t)
	a := filepath.Join(root, "a")
	writeAt(t, filepath.Join(a, "stays.txt"), "1")
	writeAt(t, filepath.Join(a, "goes.txt"), "2")
	writeAt(t, filepath.Join(root, "gone-dir", "x.txt"), "3")
	if _, err := scan.Run(context.Background(), d, []string{root}, scan.Options{}, &recorder{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(a, "goes.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "gone-dir")); err != nil {
		t.Fatal(err)
	}
	r := &commitRecorder{}
	r.check = func(c commit) {
		if c.stage != scan.StagePrune {
			return
		}
		if files, err := d.DirFiles(a); err != nil || len(files) != 1 {
			t.Errorf("at the prune report, %s lists %v, %v", a, files, err)
		}
	}
	if _, err := scan.Run(context.Background(), d, []string{root}, scan.Options{MissingOnly: true}, r); err != nil {
		t.Fatal(err)
	}
	if got, want := r.dirsOf(scan.StagePrune), []string{root, a}; !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Errorf("prune dirs = %v, want the removed file's dir and the removed dir's parent %v", got, want)
	}
	if got := r.dirsOf(scan.StageWalk); len(got) != 0 {
		t.Errorf("walk dirs on a rescan changing no file = %v", got)
	}
}

func TestCommitReporterSilentWhenCommitFails(t *testing.T) {
	d := dbtest.New(t)
	root := resolvedTemp(t)
	writeAt(t, filepath.Join(root, "a", "one.txt"), "1")
	if _, err := d.Exec("CREATE TRIGGER refuse BEFORE INSERT ON instance BEGIN SELECT RAISE(ABORT, 'refused'); END"); err != nil {
		t.Fatal(err)
	}
	r := &commitRecorder{}
	if _, err := scan.Run(context.Background(), d, []string{root}, scan.Options{}, r); err == nil {
		t.Fatal("the test needs the commit to fail")
	}
	if len(r.commits) != 0 {
		t.Errorf("reports for a failed commit: %v", r.commits)
	}
}

func TestCommitReporterCancelReportsOnlyWhatCommitted(t *testing.T) {
	d := dbtest.New(t)
	root := resolvedTemp(t)
	for _, n := range []string{"a", "b", "c", "d"} {
		writeAt(t, filepath.Join(root, n, "f.txt"), n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &commitRecorder{}
	r.check = func(commit) { cancel() }
	res, err := scan.Run(ctx, d, []string{root}, scan.Options{BatchFiles: 1}, r)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Interrupted {
		t.Fatal("the test needs the scan to be cancelled")
	}
	for _, c := range r.commits {
		for _, dir := range c.dirs {
			if files, err := d.DirFiles(dir); err != nil || len(files) == 0 {
				t.Errorf("reported %s holds %v, %v", dir, files, err)
			}
		}
	}
	if len(r.commits) == 0 {
		t.Error("no report for the commit before the cancel")
	}
}
