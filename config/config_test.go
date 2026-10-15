package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefaults(t *testing.T) {
	c := Defaults(filepath.Join(t.TempDir(), "nonexistent.ini"))
	if got := c.Get("scanning", "files"); got != "*" {
		t.Errorf("files = %q", got)
	}
	if n, _ := c.ParallelWorkers(); n != 0 {
		t.Errorf("parallel_workers = %d, want 0 (auto)", n)
	}
	if b, _ := c.FollowSymlinks(); b {
		t.Error("follow_symlinks = true")
	}
	if got := c.Get("output", "log_level"); got != "INFO" {
		t.Errorf("log_level = %q", got)
	}
	if _, err := os.Stat(c.Path); !os.IsNotExist(err) {
		t.Error("Defaults touched the filesystem")
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("FILEINDEX_SCANNING_PARALLEL_WORKERS", "8")
	t.Setenv("FILEINDEX_OUTPUT_LOG_LEVEL", "DEBUG")
	t.Setenv("FILEINDEX_SCANNING_FOLLOW_SYMLINKS", "maybe")
	c := Defaults("")
	if n, _ := c.ParallelWorkers(); n != 8 {
		t.Errorf("parallel_workers = %d", n)
	}
	if got := c.Get("output", "log_level"); got != "DEBUG" {
		t.Errorf("log_level = %q", got)
	}
	if b, err := c.FollowSymlinks(); b || err != nil {
		t.Errorf("follow_symlinks = %v, %v (unknown env value should be false)", b, err)
	}
	t.Setenv("FILEINDEX_SCANNING_PARALLEL_WORKERS", "x")
	if n, err := c.ParallelWorkers(); n != 0 || err != nil {
		t.Errorf("bad env int: %d, %v", n, err)
	}
	t.Setenv("FILEINDEX_SCANNING_PARALLEL_WORKERS", " Auto ")
	if n, err := c.ParallelWorkers(); n != 0 || err != nil {
		t.Errorf("auto: %d, %v", n, err)
	}
}

func TestListParsing(t *testing.T) {
	t.Setenv("FILEINDEX_SCANNING_FILES", "*.txt, *.py , *.md,*.json")
	c := Defaults("")
	if got := c.FilePatterns(); !reflect.DeepEqual(got, []string{"*.txt", "*.py", "*.md", "*.json"}) {
		t.Errorf("FilePatterns = %q", got)
	}
	if !strings.Contains(strings.Join(c.ExcludeDirs(), ","), ".git") {
		t.Error(".git not excluded")
	}
}

func TestLoadCustomFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "test_config.ini")
	os.WriteFile(p, []byte(`
[scanning]
files = *.test
parallel_workers = 10
follow_symlinks = Yes
; comment
# comment
exclude_dirs = a,
  b

[database]
path = /tmp/test.db

[output]
LOG_LEVEL: WARNING
`), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Get("scanning", "files"); got != "*.test" {
		t.Errorf("files = %q", got)
	}
	if n, _ := c.ParallelWorkers(); n != 10 {
		t.Errorf("parallel_workers = %d", n)
	}
	if b, _ := c.FollowSymlinks(); !b {
		t.Error("follow_symlinks")
	}
	if got := c.Get("output", "log_level"); got != "WARNING" {
		t.Errorf("log_level = %q", got)
	}
	if got := c.Get("scanning", "exclude_dirs"); got != "a,\nb" {
		t.Errorf("continuation = %q", got)
	}
	if got := c.Get("scanning", "exclude_files"); got != ".DS_Store,Thumbs.db" {
		t.Errorf("missing key should keep default, got %q", got)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"nosection":  "files = x\n",
		"dupoption":  "[a]\nx = 1\nX = 2\n",
		"dupsection": "[a]\n[a]\n",
	} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		if _, err := Load(p); err == nil || !strings.HasPrefix(err.Error(), "Failed to read configuration from "+p) {
			t.Errorf("%s: %v", name, err)
		}
	}
	p := filepath.Join(dir, "badvals")
	os.WriteFile(p, []byte("[scanning]\nparallel_workers = many\nfollow_symlinks = sure\n"), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err) // bad values only fail when read
	}
	if _, err := c.ParallelWorkers(); err == nil || err.Error() != "invalid literal for int() with base 10: 'many'" {
		t.Errorf("int: %v", err)
	}
	if _, err := c.FollowSymlinks(); err == nil || err.Error() != "Not a boolean: sure" {
		t.Errorf("bool: %v", err)
	}
}

func TestLoadWritesDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".config", "fileindex", "config.ini"))
	if err != nil {
		t.Fatal(err)
	}
	want := `[scanning]
files = *
exclude_dirs = .git,.cache,node_modules,__pycache__
exclude_files = .DS_Store,Thumbs.db
parallel_workers = auto
follow_symlinks = false

[database]
path = ` + filepath.Join(home, ".local/share/fileindex/index.sqlite") + `

[output]
log_level = INFO

`
	if string(b) != want {
		t.Errorf("got:\n%s\nwant:\n%s", b, want)
	}
	if c.Path != filepath.Join(home, ".config", "fileindex", "config.ini") {
		t.Errorf("Path = %q", c.Path)
	}
}

func TestDatabasePathTilde(t *testing.T) {
	t.Setenv("HOME", "/h")
	t.Setenv("FILEINDEX_DATABASE_PATH", "~/x.db")
	if got := Defaults("").DatabasePath(); got != "/h/x.db" {
		t.Errorf("got %q", got)
	}
}

func TestParseInt(t *testing.T) {
	for in, want := range map[string]int{" 7 ": 7, "+3": 3, "-2": -2, "1_000": 1000} {
		if n, err := ParseInt(in); err != nil || n != want {
			t.Errorf("%q: %d %v", in, n, err)
		}
	}
	for _, in := range []string{"", "_1", "1__0", "1.0", "0x10"} {
		if _, err := ParseInt(in); err == nil {
			t.Errorf("%q: want error", in)
		}
	}
}
