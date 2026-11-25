package scan

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// walkTree makes nested dirs, one of them with more files than are stat'ed ahead,
// and returns its root and the files in walk order: a dir's files by name, then
// its subdirs by name.
func walkTree(t *testing.T) (string, []string) {
	t.Helper()
	root := t.TempDir()
	var want []string
	add := func(dir string, n int) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for i := range n {
			p := filepath.Join(dir, fmt.Sprintf("f%05d", i))
			if err := os.WriteFile(p, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			want = append(want, p)
		}
	}
	add(root, 2)
	for a := range 3 * readAhead {
		d := filepath.Join(root, fmt.Sprintf("a%02d", a))
		n := 2
		if a == 1 {
			n = statAhead + 5
		}
		add(d, n)
		for b := range 3 {
			add(filepath.Join(d, fmt.Sprintf("b%d", b)), 1)
		}
	}
	return root, want
}

func TestWalkReadsAheadInOrder(t *testing.T) {
	root, want := walkTree(t)
	var got []string
	cur := ""
	for p, fi := range (Rules{}).WalkDirs(root, func(d string) { cur = d }) {
		if filepath.Dir(p) != cur {
			t.Fatalf("%s yielded while in %s", p, cur)
		}
		if fi.Name() != filepath.Base(p) {
			t.Fatalf("%s came with the stat of %s", p, fi.Name())
		}
		got = append(got, p)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %d files, want %d in serial order", len(got), len(want))
	}
}

func TestWalkStopsEarly(t *testing.T) {
	root, want := walkTree(t)
	for _, stop := range []int{1, 10, len(want) - 1} {
		n := 0
		for range (Rules{}).Walk(root) {
			if n++; n == stop {
				break
			}
		}
	}
}

// A dir read ahead too long ago is read again, so it is not indexed as it was
// before a big sibling was walked.
func TestWalkRereadsStaleListing(t *testing.T) {
	defer func(d time.Duration) { maxListingAge = d }(maxListingAge)
	maxListingAge = 50 * time.Millisecond
	root := t.TempDir()
	for _, d := range []string{"a", "b"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, d, "old"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for p := range (Rules{}).Walk(root) {
		got = append(got, filepath.Base(filepath.Dir(p))+"/"+filepath.Base(p))
		if len(got) == 1 { // in a; b is read ahead by the end of the sleep
			time.Sleep(4 * maxListingAge)
			if err := os.WriteFile(filepath.Join(root, "b", "new"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			time.Sleep(2 * maxListingAge)
		}
	}
	if want := []string{"a/old", "b/new", "b/old"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
