package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paranoidi/fileindex/db"
)

const csvHeader = "md5,sha256,sha256_noexif_jpeg,sha256_sample,filesize,file_created_at,file_last_seen,path,filename,mtime,instance_created_at,instance_last_seen\r\n"

func rowSet(t *testing.T, d *db.DB) []string {
	t.Helper()
	var out []string
	if err := d.ExportRows(func(r []string) error {
		// ids and timestamps aside, everything must survive the round trip
		out = append(out, strings.Join(append(r[:5:5], r[7], r[8], r[9]), "|"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestExportImportRoundTrip(t *testing.T) {
	h, d, tree := scanHarness(t)
	writeFile(t, filepath.Join(tree, "a.jpg"), fakeJPEG("exif"))
	writeFile(t, filepath.Join(tree, "sub", "b.png"), "png")
	writeFile(t, filepath.Join(tree, "sub", "copy.png"), "png")
	h.mustRun("scan", tree)
	csvPath := filepath.Join(tree, "out", "export.csv")

	out := h.mustRun("export", csvPath)
	if !strings.HasPrefix(out, "Exporting database to: "+csvPath+"\nExport completed in ") ||
		!strings.HasSuffix(out, "Files exported: 2\nInstances exported: 3\nCSV file: "+csvPath+"\n") {
		t.Errorf("export output %q", out)
	}
	data, _ := os.ReadFile(csvPath)
	if !strings.HasPrefix(string(data), csvHeader) || strings.Count(string(data), "\r\n") != 4 {
		t.Errorf("csv:\n%q", data)
	}
	before := rowSet(t, d)

	// import into a fresh index
	h2 := newHarness(t)
	h2.mustRun("init")
	if out := h2.mustRun("import", "--validate-only", csvPath); out != "Validating CSV file: "+csvPath+"\n✔ CSV file is valid\nFiles to import: 2\nInstances to import: 3\n" {
		t.Errorf("validate: %q", out)
	}
	out = h2.mustRun("import", csvPath)
	if !strings.HasSuffix(out, "Files imported: 2\nInstances imported: 3\nTotal files processed: 2\nTotal instances processed: 3\n") {
		t.Errorf("import output %q", out)
	}
	d2, _ := db.Open(h2.dbPath())
	defer d2.Close()
	after := rowSet(t, d2)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("round trip changed rows:\n%v\n%v", before, after)
	}
	// the PNG's empty noexif field is NULL again, so it is not a duplicate of anything
	var n int
	d2.QueryRow("SELECT COUNT(*) FROM file WHERE sha256_noexif_jpeg = ''").Scan(&n)
	if n != 0 {
		t.Errorf("%d files with '' noexif", n)
	}
	// importing again changes nothing
	if out := h2.mustRun("import", csvPath); !strings.Contains(out, "Files imported: 0\nInstances imported: 0\n") {
		t.Errorf("re-import: %q", out)
	}
}

func writeCSV(t *testing.T, rows ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "in.csv")
	os.WriteFile(p, []byte(strings.Join(rows, "\n")+"\n"), 0o644)
	return p
}

const md5A = "0123456789abcdef0123456789abcdef"
const md5B = "fedcba9876543210fedcba9876543210"

var (
	shaA = strings.Repeat("a", 64)
	shaB = strings.Repeat("b", 64)
)

// Empty timestamp fields take the column defaults.
func TestImportDefaults(t *testing.T) {
	h, d := findHarness(t)
	p := writeCSV(t, strings.TrimSuffix(csvHeader, "\r\n"), md5A+","+shaA+",,,5,,,/m,a.jpg,,,")
	h.mustRun("import", p)
	var n int
	d.QueryRow("SELECT COUNT(*) FROM file f JOIN instance i ON i.file_id = f.id WHERE f.created_at IS NOT NULL AND f.last_seen IS NOT NULL AND i.created_at IS NOT NULL AND i.last_seen IS NOT NULL AND i.mtime IS NULL AND f.sha256_sample IS NULL").Scan(&n)
	if n != 1 {
		t.Error("defaults not applied, or empty fields not NULL")
	}
}

// A different file already at the same location is replaced, not a batch failure.
func TestImportLocationClash(t *testing.T) {
	h, d := findHarness(t)
	head := strings.TrimSuffix(csvHeader, "\r\n")
	h.mustRun("import", writeCSV(t, head, md5A+","+shaA+",,,5,,,/m,a.jpg,,,"))
	h.mustRun("import", writeCSV(t, head, md5B+","+shaB+",,,7,,,/m,a.jpg,,,"))
	var md5 string
	d.QueryRow("SELECT f.md5 FROM instance i JOIN file f ON f.id = i.file_id WHERE i.filename = 'a.jpg'").Scan(&md5)
	if md5 != md5B || count(t, d, "instance") != 1 {
		t.Errorf("instance points at %s, %d instances", md5, count(t, d, "instance"))
	}
}

// What the DB can't store fails validation, and a failed batch fails the command.
func TestImportValidation(t *testing.T) {
	h, d := findHarness(t)
	head := strings.TrimSuffix(csvHeader, "\r\n")
	p := writeCSV(t, head,
		","+shaA+",,,1,,,/m,a.jpg,,,",
		md5A+","+shaA+",,,1,,,/m,,,,",
		md5A+","+shaA+",,,x,,,/m,a.jpg,,,",
		md5A+","+shaA+",,,-1,,,/m,a.jpg,,,",
		"short,"+shaA+",,,1,,,/m,a.jpg,,,",
		md5A+",,,,1,,,/m,a.jpg,,,",
		md5A+","+shaA+",,,1,,,,a.jpg,,,",
		md5A+","+shaA+",,,1,,,.,a.jpg,,,",
		md5A+",abc,,,1,,,/m,a.jpg,,,",
		md5A+","+shaA+",,,1_000,,,/m,ok.jpg,,,",
	)
	want := "✘ CSV file has validation errors:\n" +
		"  Row 2: Missing required md5 hash\n  Row 3: Missing filename\n  Row 4: Invalid filesize format: x\n" +
		"  Row 5: Invalid filesize: -1\n  Row 6: Invalid MD5 hash length: 5\n  Row 7: Missing required sha256 hash\n" +
		"  Row 8: Missing path\n  Row 9: Missing path\n  Row 10: Invalid SHA256 hash length: 3\n"
	code, out, _ := h.run("import", "--validate-only", p)
	if code != 1 || out != "Validating CSV file: "+p+"\n"+want {
		t.Errorf("validate: exit %d\n%s", code, out)
	}
	if code, out, _ := h.run("import", p); code != 1 || !strings.HasSuffix(out, "✘ Import failed:\n"+strings.TrimPrefix(want, "✘ CSV file has validation errors:\n")) {
		t.Errorf("import: exit %d\n%s", code, out)
	}
	if count(t, d, "file") != 0 {
		t.Error("an invalid CSV imported rows")
	}

	// missing columns are listed in a stable order
	bad := writeCSV(t, "md5,filename", md5A+",a.jpg")
	code, _, errOut := h.run("import", bad)
	if code != 1 || errOut != "Error: Error reading CSV file: CSV missing required columns: {'file_created_at', 'file_last_seen', 'filesize', 'instance_created_at', 'instance_last_seen', 'mtime', 'path', 'sha256'}\n" {
		t.Errorf("missing columns: exit %d %q", code, errOut)
	}

	// a batch the DB rejects makes the command exit 1
	d.Exec("CREATE TRIGGER fail BEFORE INSERT ON instance BEGIN SELECT RAISE(ABORT, 'boom'); END")
	code, out, errOut = h.run("import", writeCSV(t, head, md5A+","+shaA+",,,1,,,/m,a.jpg,,,"))
	if code != 1 || !strings.Contains(out, "Files imported: 0\nInstances imported: 0\n") ||
		!strings.Contains(errOut, "\nErrors during import:\n  Database error during batch import: ") || !strings.Contains(errOut, "boom") {
		t.Errorf("failed batch: exit %d %q %q", code, out, errOut)
	}

	if code, _, errOut := h.run("import", "/no/such.csv"); code != 1 || errOut != "Error: CSV file not found: /no/such.csv\n" {
		t.Errorf("missing file: exit %d %q", code, errOut)
	}
}
