package progress

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
)

func TestRel(t *testing.T) {
	for _, c := range []struct{ path, want string }{
		{"/r/a/b/f.jpg", "f.jpg"},     // under the scanning dir
		{"/r/a/c/g.jpg", "a/c/g.jpg"}, // lagging worker: root-relative
		{"/x/h.jpg", "/x/h.jpg"},      // outside everything
	} {
		if got := rel(c.path, "/r/a/b", "/r"); got != c.want {
			t.Errorf("rel(%q) = %q, want %q", c.path, got, c.want)
		}
	}
	if got := rel("/r/f.jpg", "", "/r"); got != "f.jpg" {
		t.Errorf("idle dir: got %q", got)
	}
}

func TestFitPath(t *testing.T) {
	const long = "langfilter/.venv/lib/python3.13/site-packages/pygments/lexers/snobol.py"
	for _, w := range []int{40, 55, 80} {
		got := fitPath(long, w)
		if !strings.HasSuffix(got, "/snobol.py") || runewidth.StringWidth(got) > w {
			t.Errorf("fitPath(%d) = %q", w, got)
		}
	}
	if got := fitPath("a/bb/ccc/f", 8); got != "a/b/c…/f" {
		t.Errorf("longest dir shrinks first: got %q", got)
	}
	if got := fitPath("dir/abcdefghijklmnopqrstuvwxyz0123456789.go", 12); got != "dir/a…789.go" {
		t.Errorf("long basename: got %q", got)
	}
	if got := fitPath("/x/日本語のファイル.jpg", 12); runewidth.StringWidth(got) > 12 {
		t.Errorf("wide runes overflow: %q", got)
	}
	for w, want := range map[int]string{0: "", 1: "/"} {
		if got := fitPath("/", w); got != want {
			t.Errorf("fitPath(/, %d) = %q", w, got)
		}
	}
}

// Per-file updates must not each draw; a forced draw still shows the latest state.
func TestRedrawThrottled(t *testing.T) {
	var out bytes.Buffer
	p := NewProgress(&out, 0, nil, false)
	p.active = true
	p.fd = -1 // cols falls back to 80
	for i := range 1000 {
		p.Scan("/d", i+1, 0, 0)
	}
	if n := strings.Count(out.String(), "Scanned: "); n > 5 {
		t.Fatalf("1000 updates drew %d times", n)
	}
	p.Speed(0, 0)
	if !strings.Contains(out.String(), "Scanned: 1000") {
		t.Fatalf("forced draw lost the latest count: %q", out.String())
	}
}
