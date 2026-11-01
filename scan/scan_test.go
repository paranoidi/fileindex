package scan

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/jpeg"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Expected matches are the reference fnmatchcase semantics.
var globNames = []string{"a.jpg", "a.JPG", "IMG_1", "x_y", "xy", "bob", "dad", "ax", "cx", "zx", "]", "x", "-", "a", "a[b", "a\\b", "ab\\", "d", "e", "f", "y", "ab", "axxb", "^", "photo.JpG", "t[", "!", ".git", ".gitx", "node_modules", ".hidden", "f.tar.gz", "üa", "\n", "b"}

var globCases = []struct {
	pat  string
	want []string
}{
	{"*.jpg", []string{"a.jpg"}},
	{"*.JPG", []string{"a.JPG"}},
	{"IMG_*", []string{"IMG_1"}},
	{"x?y", []string{"x_y"}},
	{"[abc]*", []string{"a.jpg", "a.JPG", "bob", "ax", "cx", "a", "a[b", "a\\b", "ab\\", "ab", "axxb", "b"}},
	{"[!abc]*", []string{"IMG_1", "x_y", "xy", "dad", "zx", "]", "x", "-", "d", "e", "f", "y", "^", "photo.JpG", "t[", "!", ".git", ".gitx", "node_modules", ".hidden", "f.tar.gz", "üa", "\n"}},
	{"[a-c]x", []string{"ax", "cx"}},
	{"[z-a]x", []string{}},
	{"[!z-a]", []string{"]", "x", "-", "a", "d", "e", "f", "y", "^", "!", "\n", "b"}},
	{"[]]", []string{"]"}},
	{"[!]]x", []string{"ax", "cx", "zx"}},
	{"[-a]", []string{"-", "a"}},
	{"[a-]", []string{"-", "a"}},
	{"a[b", []string{"a[b"}},
	{"a\\b", []string{"a\\b"}},
	{"*\\*", []string{"a\\b", "ab\\"}},
	{"[a-c-e]", []string{"-", "a", "e", "b"}},
	{"[a-cx-z]", []string{"x", "a", "y", "b"}},
	{"a**b", []string{"a[b", "a\\b", "ab", "axxb"}},
	{"[^a]", []string{"a", "^"}},
	{"*.[jJ][pP][gG]", []string{"a.jpg", "a.JPG", "photo.JpG"}},
	{"*[", []string{"t["}},
	{"[!]", []string{}},
	{".git", []string{".git"}},
	{"node_modules", []string{"node_modules"}},
	{"[a-cz-b]", []string{"a", "b"}},
	{".*", []string{".git", ".gitx", ".hidden"}},
	{"*.tar.gz", []string{"f.tar.gz"}},
	{"ü*", []string{"üa"}},
	{"[!-]", []string{"]", "x", "a", "d", "e", "f", "y", "^", "!", "\n", "b"}},
}

func TestFnmatch(t *testing.T) {
	for _, c := range globCases {
		got := []string{}
		for _, n := range globNames {
			if Fnmatch(n, c.pat) {
				got = append(got, n)
			}
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%q (regexp %s): got %q, want %q", c.pat, translate(c.pat), got, c.want)
		}
	}
}

func write(t *testing.T, p, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(paths []string) []string {
	var out []string
	for _, p := range paths {
		out = append(out, filepath.Base(p))
	}
	slices.Sort(out)
	return out
}

func TestWalkBasic(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "test1.txt"), "content1")
	write(t, filepath.Join(d, "test2.py"), "content2")
	write(t, filepath.Join(d, "ignore.log"), "content3")
	write(t, filepath.Join(d, ".DS_Store"), "content4")
	write(t, filepath.Join(d, ".git", "config"), "git config")
	r := Rules{Patterns: []string{"*.txt", "*.py"}, ExcludeDirs: []string{".git", "__pycache__"}, ExcludeFiles: []string{".DS_Store"}}
	if got := names(slices.Collect(r.Walk(d))); !slices.Equal(got, []string{"test1.txt", "test2.py"}) {
		t.Errorf("got %q", got)
	}
}

func TestWalkPatternCase(t *testing.T) {
	d := t.TempDir()
	for _, n := range []string{"photo.jpg", "PHOTO.PNG", "photo.png", "document.txt"} {
		write(t, filepath.Join(d, n), "x")
	}
	r := Rules{Patterns: []string{"*.jpg", "*.PNG"}}
	if got := names(slices.Collect(r.Walk(d))); !slices.Equal(got, []string{"PHOTO.PNG", "photo.jpg", "photo.png"}) {
		t.Errorf("got %q", got)
	}
}

func TestWalkSkipsEmptyAndSpecial(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "file1.txt"), "content1")
	write(t, filepath.Join(d, "empty.txt"), "")
	os.Symlink(filepath.Join(d, "nowhere.txt"), filepath.Join(d, "dangling.txt"))
	os.Symlink(filepath.Join(d, "file1.txt"), filepath.Join(d, "link.txt"))
	r := Rules{Patterns: []string{"*.txt"}}
	if got := names(slices.Collect(r.Walk(d))); !slices.Equal(got, []string{"file1.txt", "link.txt"}) {
		t.Errorf("got %q", got)
	}
	// a file root is yielded alone; the include patterns still apply
	if got := slices.Collect(r.Walk(filepath.Join(d, "file1.txt"))); len(got) != 1 {
		t.Errorf("file root: %q", got)
	}
	if got := slices.Collect((Rules{Patterns: []string{"*.jpg"}}).Walk(filepath.Join(d, "file1.txt"))); len(got) != 0 {
		t.Errorf("file root not matching patterns: %q", got)
	}
}

func TestWalkDirSymlinks(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "real", "a.txt"), "a")
	os.Symlink(filepath.Join(d, "real"), filepath.Join(d, "linked"))
	os.Symlink(d, filepath.Join(d, "real", "loop")) // cycle back to the root

	r := Rules{Patterns: []string{"*.txt"}}
	if got := slices.Collect(r.Walk(d)); !slices.Equal(got, []string{filepath.Join(d, "real", "a.txt")}) {
		t.Errorf("no follow: %q", got)
	}

	r.FollowSymlinks = true
	done := make(chan []string)
	go func() { done <- slices.Collect(r.Walk(d)) }()
	select {
	case got := <-done:
		// "linked" and "real" are the same dir, entered once
		if len(got) != 1 {
			t.Errorf("follow: %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("walk did not terminate on a symlink loop")
	}
}

// jpegWithMetadata encodes a small image and splices an APP1 (EXIF-like) and a
// COM segment after SOI.
func jpegWithMetadata(t *testing.T, exif string) (plain, withMeta []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	plain = buf.Bytes()
	seg := func(marker byte, body string) []byte {
		n := len(body) + 2
		return append([]byte{0xFF, marker, byte(n >> 8), byte(n)}, body...)
	}
	withMeta = append([]byte{0xFF, 0xD8}, seg(0xE1, "Exif\x00\x00"+exif)...)
	withMeta = append(withMeta, seg(0xFE, "a comment")...)
	withMeta = append(withMeta, plain[2:]...)
	return plain, withMeta
}

func TestStripJPEGMetadata(t *testing.T) {
	plain, a := jpegWithMetadata(t, "camera A")
	_, b := jpegWithMetadata(t, "camera B, longer")
	sa, err := StripJPEGMetadata(a)
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := StripJPEGMetadata(b)
	sp, _ := StripJPEGMetadata(plain)
	if !bytes.Equal(sa, sb) || !bytes.Equal(sa, sp) {
		t.Error("same image with different metadata strips differently")
	}
	if bytes.Equal(a, b) {
		t.Fatal("fixtures should differ")
	}
	if bytes.Contains(sa, []byte("Exif")) || bytes.Contains(sa, []byte("comment")) {
		t.Error("metadata survived")
	}
	// image/jpeg writes no APPn, so the only change to plain is nothing at all
	if !bytes.Equal(sp, plain) {
		t.Error("stripping a metadata-free JPEG changed it")
	}

	for name, c := range map[string]struct{ in, want []byte }{
		"SOS copies rest": {[]byte{0xFF, 0xD8, 0xFF, 0xDA, 0x00, 0x02, 0xFF, 0xE1, 0x09, 0xFF, 0xD9}, []byte{0xFF, 0xD8, 0xFF, 0xDA, 0x00, 0x02, 0xFF, 0xE1, 0x09, 0xFF, 0xD9}},
		"RST dropped":     {[]byte{0xFF, 0xD8, 0xFF, 0xD0, 0xFF, 0xDA, 0x00, 0x02}, []byte{0xFF, 0xD8, 0xFF, 0xDA, 0x00, 0x02}},
	} {
		got, err := StripJPEGMetadata(c.in)
		if err != nil || !bytes.Equal(got, c.want) {
			t.Errorf("%s: got % X, %v; want % X", name, got, err, c.want)
		}
	}
	// no SOS means no image data, not a hash every such file shares
	for name, in := range map[string][]byte{
		"no SOS, no EOI":    {0xFF, 0xD8, 0xFF, 0xDB, 0x00, 0x03, 0x01, 0xFF, 0xD9},
		"truncated segment": {0xFF, 0xD8, 0xFF, 0xC0, 0x00, 0x10, 0x01},
		"garbage stops":     {0xFF, 0xD8, 0x12, 0xFF, 0xDA, 0x00, 0x02},
	} {
		if _, err := StripJPEGMetadata(in); err != ErrNoImageData {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := StripJPEGMetadata([]byte("GIF89a")); err != ErrNotJPEG {
		t.Errorf("non-JPEG: %v", err)
	}
}

func hexOf(sum []byte) string { return hex.EncodeToString(sum) }

func TestHashFile(t *testing.T) {
	d := t.TempDir()
	big := bytes.Repeat([]byte("0123456789"), 2000) // > 8 KiB
	p := filepath.Join(d, "big.bin")
	write(t, p, string(big))
	r := HashFile(p)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	m, s, s8 := md5.Sum(big), sha256.Sum256(big), sha256.Sum256(big[:8192])
	if r.Hashes.MD5 != hexOf(m[:]) || r.Hashes.SHA256 != hexOf(s[:]) || r.Hashes.SHA256_8KB != hexOf(s8[:]) || r.Hashes.NoExifJPEG != nil {
		t.Errorf("hashes: %+v", r.Hashes)
	}
	if r.Size != int64(len(big)) || r.Mtime.IsZero() {
		t.Errorf("size %d mtime %v", r.Size, r.Mtime)
	}

	// Same content gives the same md5 (tests/test_hasher.py).
	q := filepath.Join(d, "copy.bin")
	write(t, q, string(big))
	if HashFile(q).Hashes.MD5 != r.Hashes.MD5 {
		t.Error("same content, different md5")
	}

	_, a := jpegWithMetadata(t, "A")
	_, b := jpegWithMetadata(t, "B")
	write(t, filepath.Join(d, "a.JPG"), string(a))
	write(t, filepath.Join(d, "b.jpeg"), string(b))
	ha, hb := HashFile(filepath.Join(d, "a.JPG")), HashFile(filepath.Join(d, "b.jpeg"))
	if ha.Hashes.NoExifJPEG == nil || hb.Hashes.NoExifJPEG == nil || *ha.Hashes.NoExifJPEG != *hb.Hashes.NoExifJPEG {
		t.Errorf("noexif hashes: %v %v", ha.Hashes.NoExifJPEG, hb.Hashes.NoExifJPEG)
	}
	if ha.Hashes.SHA256 == hb.Hashes.SHA256 {
		t.Error("full sha256 should differ")
	}

	write(t, filepath.Join(d, "fake.jpg"), "not a jpeg")
	write(t, filepath.Join(d, "padded.jpg"), "\xff\xd8\xff\xe1\x00\x08Exif\x00\x00\x00\xff\xda\x00\x02image")
	if r := HashFile(filepath.Join(d, "padded.jpg")); r.Err != nil || r.Hashes.NoExifJPEG != nil {
		t.Errorf("padded.jpg: %v %v", r.Err, r.Hashes.NoExifJPEG)
	}
	if r := HashFile(filepath.Join(d, "fake.jpg")); r.Err != nil || r.Hashes.NoExifJPEG != nil {
		t.Errorf("fake.jpg: %v %v", r.Err, r.Hashes.NoExifJPEG)
	}

	if r := HashFile(filepath.Join(d, "missing")); r.Err == nil || r.Err.Error() != "File not found: "+filepath.Join(d, "missing") {
		t.Errorf("missing: %v", r.Err)
	}
	if r := HashFile(d); r.Err == nil || r.Err.Error() != "Path is not a regular file: "+d {
		t.Errorf("dir: %v", r.Err)
	}
}

func TestHashFileChangedMidHash(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.bin")
	write(t, p, "before")
	afterHash = func(string) { os.WriteFile(p, []byte("after, longer"), 0o644) }
	defer func() { afterHash = func(string) {} }()
	if r := HashFile(p); r.Err == nil || r.Err.Error() != "File changed during hashing: "+p {
		t.Errorf("got %v", r.Err)
	}
}

func TestHashAll(t *testing.T) {
	d := t.TempDir()
	var paths []string
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		p := filepath.Join(d, n)
		write(t, p, n)
		paths = append(paths, p)
	}
	for _, workers := range []int{1, 3, 0} {
		var got []string
		for r := range HashAll(context.Background(), slices.Values(paths), workers, nil, nil) {
			if r.Err != nil {
				t.Fatal(r.Err)
			}
			got = append(got, r.Path)
		}
		if workers == 1 && !slices.Equal(got, paths) {
			t.Errorf("sequential order: %q", got)
		}
		slices.Sort(got)
		if !slices.Equal(got, paths) {
			t.Errorf("workers=%d: %q", workers, got)
		}
	}
	// breaking out early must not hang
	for range HashAll(context.Background(), slices.Values(paths), 2, func(int, string) {}, nil) {
		break
	}
	for range HashAll(context.Background(), slices.Values(paths), 0, nil, nil) {
		break
	}

	// auto mode resizing while hashing: every file still comes out exactly once
	defer func(d time.Duration) { tick = d }(tick)
	tick = time.Microsecond
	many := slices.Repeat(paths, 300)
	var ticks int
	n := 0
	for r := range HashAll(context.Background(), slices.Values(many), 0, nil, func(int, float64) { ticks++ }) {
		if r.Err != nil {
			t.Fatal(r.Err)
		}
		n++
	}
	if n != len(many) || ticks == 0 {
		t.Errorf("hashed %d of %d, %d ticks", n, len(many), ticks)
	}
}

func TestScaler(t *testing.T) {
	s := scaler{n: 1, hold: holdTicks - 1}
	run := func(bps float64, want int) {
		t.Helper()
		if got := s.step(bps); got != want {
			t.Fatalf("step(%v) = %d, want %d", bps, got, want)
		}
	}
	run(0, 1)   // no signal: no change
	run(100, 2) // first tick probes up
	run(200, 3) // gained: keep going
	run(300, 4)
	run(305, 3) // under 5% gain: undo
	for range holdTicks - 1 {
		run(300, 3) // hold
	}
	run(300, 2) // probe down
	run(290, 1) // within 5%: fewer workers is fine, keep going
	run(280, 1) // can't go below 1
	for range holdTicks - 1 {
		run(280, 1)
	}
	run(280, 2) // probe up
	run(200, 1) // dropped: undo
	for range holdTicks - 1 {
		run(280, 1)
	}
	run(280, 2) // n == 1, so probe up even though down was due
	run(500, 3)
	run(400, 2) // dropped: scale back
}

// A library caller must never get log text on its terminal: walk warnings go to
// Logger, which discards by default, not to the global slog default (stderr).
func TestWalkWarningsDoNotReachDefaultLogger(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	for range (Rules{}).Walk(filepath.Join(t.TempDir(), "missing")) {
	}
	if buf.Len() != 0 {
		t.Fatalf("walk logged to the default logger: %q", buf.String())
	}
}
