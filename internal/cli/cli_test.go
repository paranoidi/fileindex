package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paranoidi/fileindex/db"
	"github.com/paranoidi/fileindex/internal/dbtest"
)

// harness runs fileindex in-process against a temporary HOME.
type harness struct {
	t    *testing.T
	home string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, e := range os.Environ() {
		if k, _, _ := strings.Cut(e, "="); strings.HasPrefix(k, "FILEINDEX_") {
			t.Setenv(k, "")
			os.Unsetenv(k)
		}
	}
	return &harness{t: t, home: home}
}

func (h *harness) dbPath() string {
	return filepath.Join(h.home, ".local", "share", "fileindex", "index.sqlite")
}

func (h *harness) run(args ...string) (code int, stdout, stderr string) {
	var o, e bytes.Buffer
	code = Run(args, &o, &e)
	return code, o.String(), e.String()
}

// mustRun fails the test unless the command exits 0.
func (h *harness) mustRun(args ...string) string {
	h.t.Helper()
	code, out, errOut := h.run(args...)
	if code != 0 {
		h.t.Fatalf("%v: exit %d\nstdout: %s\nstderr: %s", args, code, out, errOut)
	}
	return out
}

func TestInitCreatesConfigAndDB(t *testing.T) {
	h := newHarness(t)
	out := h.mustRun("init")
	cfg := filepath.Join(h.home, ".config", "fileindex", "config.ini")
	want := "Database initialized at: " + h.dbPath() + "\nConfiguration file at: " + cfg + "\n"
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
	if code, _, errOut := h.run("init"); code != 1 || errOut != "Error: already initialised: "+h.dbPath()+"\n" {
		t.Fatalf("second init: exit %d, stderr %q", code, errOut)
	}
}

func TestNoCommandPrintsHelpExit1(t *testing.T) {
	h := newHarness(t)
	code, out, _ := h.run()
	if code != 1 || !strings.Contains(out, "Usage:") {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	// config is written on every command, even this one
	if _, err := os.Stat(filepath.Join(h.home, ".config", "fileindex", "config.ini")); err != nil {
		t.Fatal(err)
	}
}

func TestUsageErrorsExit2(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{{"bogus"}, {"--no-daemon", "stats"}, {"stats", "extra"}} {
		if code, _, errOut := h.run(args...); code != 2 || errOut == "" {
			t.Errorf("%v: exit %d, stderr %q", args, code, errOut)
		}
	}
}

func TestInitSchemaVersionMismatch(t *testing.T) {
	h := newHarness(t)
	d := dbtest.Init(t, h.dbPath())
	if _, err := d.Exec("INSERT INTO schema_version (version) VALUES (11)"); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := h.run("init")
	if code != 1 || errOut != "Error: "+db.ErrNewerSchema.Error()+"\n" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}

// init never migrates: an outdated DB is left for upgrade-db.
func TestInitRefusesOutdatedThenUpgradeDB(t *testing.T) {
	h := newHarness(t)
	d := dbtest.Init(t, h.dbPath())
	for _, q := range []string{"DROP INDEX idx_instance_fast", "ALTER TABLE instance DROP COLUMN sha256_8kb", "DELETE FROM schema_version", "INSERT INTO schema_version (version) VALUES (8)"} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if code, _, errOut := h.run("init"); code != 1 || errOut != "Error: "+db.ErrOutdatedSchema.Error()+"\n" {
		t.Fatalf("init: exit %d, stderr %q", code, errOut)
	}
	code, out, errOut := h.run("upgrade-db")
	if code != 0 || out != "Database at schema v10: "+h.dbPath()+"\n" || !strings.Contains(errOut, "Migrating") {
		t.Fatalf("upgrade-db: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	code, _, errOut = h.run("upgrade-db") // already current
	if code != 0 || errOut != "" {
		t.Fatalf("second upgrade-db: exit %d, stderr %q", code, errOut)
	}
}

func TestUpgradeDBBeforeInit(t *testing.T) {
	h := newHarness(t)
	if code, _, errOut := h.run("upgrade-db"); code != 1 || errOut != "Error: "+db.ErrNotInitialised.Error()+"\n" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}

func TestUpgradeDBTooOld(t *testing.T) {
	h := newHarness(t)
	d := dbtest.Init(t, h.dbPath())
	for _, q := range []string{"DELETE FROM schema_version", "INSERT INTO schema_version (version) VALUES (5)"} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if code, _, errOut := h.run("upgrade-db"); code != 1 || !strings.Contains(errOut, "v5 is too old") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}

func TestStats(t *testing.T) {
	h := newHarness(t)
	d := dbtest.Init(t, h.dbPath())
	dbtest.AddFile(t, d, "/m/a/x.jpg", "hello")
	dbtest.AddFile(t, d, "/m/b/x.jpg", "hello")
	dbtest.AddFile(t, d, "/m/b/y.jpg", strings.Repeat("z", 2048))

	out := h.mustRun("stats")
	want := "Directories: 4\nFiles: 2\nInstances: 3\nTotal size: 2.00 KiB\nDuplicate files: 1\nDatabase size: "
	if !strings.HasPrefix(out, want) {
		t.Fatalf("got %q", out)
	}
}

func TestStatsCountsWAL(t *testing.T) {
	h := newHarness(t)
	d := dbtest.Init(t, h.dbPath())
	dbtest.AddFile(t, d, "/m/x.jpg", "hello") // d stays open, so the WAL is not checkpointed away
	main, _ := os.Stat(h.dbPath())
	wal, err := os.Stat(h.dbPath() + "-wal")
	if err != nil || wal.Size() == 0 {
		t.Fatalf("expected a non-empty -wal file: %v", err)
	}
	s, err := d.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if s.DBSize != main.Size()+wal.Size() {
		t.Fatalf("DBSize %d, want main %d + wal %d", s.DBSize, main.Size(), wal.Size())
	}
}

func TestStatsBeforeInit(t *testing.T) {
	h := newHarness(t)
	code, _, errOut := h.run("stats")
	if code != 1 || !strings.HasPrefix(errOut, "Error: ") || !strings.Contains(errOut, "no such table: dir") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}

func TestVacuum(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	if out := h.mustRun("vacuum"); out != "Vacuuming database...\nDatabase vacuumed successfully\n" {
		t.Fatalf("got %q", out)
	}
}

func TestDatabasePathFromEnvAndConfigFlag(t *testing.T) {
	h := newHarness(t)
	p := filepath.Join(h.home, "elsewhere", "x.db")
	t.Setenv("FILEINDEX_DATABASE_PATH", p)
	cfg := filepath.Join(h.home, "custom.ini")
	out := h.mustRun("--config", cfg, "init")
	if !strings.Contains(out, p) || !strings.Contains(out, cfg) {
		t.Fatalf("got %q", out)
	}
}

func TestVersion(t *testing.T) {
	h := newHarness(t)
	code, out, errOut := h.run("--version")
	if code != 0 || !strings.HasPrefix(out, "fileindex version ") || errOut != "" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestInitAutocomplete(t *testing.T) {
	h := newHarness(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("SHELL", "/usr/bin/fish")
	out := h.mustRun("init", "--autocomplete")
	path := filepath.Join(h.home, ".config", "fish", "completions", "fileindex.fish")
	if !strings.Contains(out, path) {
		t.Fatalf("output %q lacks %s", out, path)
	}
	if b, err := os.ReadFile(path); err != nil || !strings.Contains(string(b), "fileindex") {
		t.Fatalf("script missing: %v", err)
	}
	if _, err := os.Stat(h.dbPath()); err == nil {
		t.Fatal("--autocomplete must not create the database")
	}
	t.Setenv("SHELL", "/bin/tcsh")
	if code, _, _ := h.run("init", "--autocomplete"); code != 1 {
		t.Fatalf("unsupported shell: exit %d", code)
	}
}

func TestInitNonInteractiveSkipsAutocomplete(t *testing.T) {
	h := newHarness(t)
	t.Setenv("SHELL", "/usr/bin/fish")
	if out := h.mustRun("init", "--non-interactive"); strings.Contains(out, "registered") {
		t.Fatalf("unexpected: %q", out)
	}
}
