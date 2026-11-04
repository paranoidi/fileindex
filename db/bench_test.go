package db_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/paranoidi/fileindex/db"
	"github.com/paranoidi/fileindex/internal/dbtest"
	"github.com/paranoidi/fileindex/scan"
)

var benchWords = []string{"holiday", "report", "final", "draft", "photo", "invoice", "summer", "backup", "music", "track",
	"video", "notes", "project", "budget", "scan", "letter", "copy", "old", "new", "export"}

// BenchmarkCommitScanBatch commits 20k fresh files in batches of 200, the
// shape of a small-file scan.
func BenchmarkCommitScanBatch(b *testing.B) {
	const total, batch = 20000, 200
	for b.Loop() {
		b.StopTimer()
		d := dbtest.New(b)
		r := rand.New(rand.NewPCG(1, 2))
		files := make([]db.ScannedFile, total)
		for i := range files {
			h := sha256.Sum256(fmt.Append(nil, i))
			s := hex.EncodeToString(h[:])
			name := fmt.Sprintf("%s %s %s_%d.jpg", benchWords[r.IntN(len(benchWords))], benchWords[r.IntN(len(benchWords))], benchWords[r.IntN(len(benchWords))], i)
			files[i] = db.ScannedFile{Dir: fmt.Sprintf("/bench/d%d/sub%d", i/2000, i/400%5), Name: name, Size: 32768,
				Mtime: time.Unix(1700000000+int64(i), 0), MD5: s[:32], SHA256: s, SHA256_8KB: s}
		}
		b.StartTimer()
		for i := 0; i < total; i += batch {
			if _, err := d.CommitScanBatch(files[i : i+batch]); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkPruneDir prunes 20k indexed files gone from 50 dirs that still exist.
func BenchmarkPruneDir(b *testing.B) {
	const total = 20000
	for b.Loop() {
		b.StopTimer()
		d := dbtest.New(b)
		root := b.TempDir()
		files := make([]db.ScannedFile, total)
		for i := range files {
			dir := filepath.Join(root, fmt.Sprintf("d%d", i/400))
			if i%400 == 0 {
				if err := os.Mkdir(dir, 0o755); err != nil {
					b.Fatal(err)
				}
			}
			h := sha256.Sum256(fmt.Append(nil, i))
			s := hex.EncodeToString(h[:])
			files[i] = db.ScannedFile{Dir: dir, Name: fmt.Sprintf("%s report_%d.jpg", benchWords[i%len(benchWords)], i), Size: 1,
				Mtime: time.Unix(1700000000, 0), MD5: s[:32], SHA256: s, SHA256_8KB: s}
		}
		if _, err := d.CommitScanBatch(files); err != nil {
			b.Fatal(err)
		}
		id, _, err := db.DirByPath(d, root)
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		n, _, err := d.PruneDir(context.Background(), root, *id, nil, func(string) {})
		if err != nil || n != total {
			b.Fatal(n, err)
		}
	}
}

// seedDirs inserts a tree of n dirs under "/" (ten children each), fast.
func seedDirs(b *testing.B, d *db.DB, n int) {
	b.Helper()
	if _, err := d.Exec(`INSERT INTO dir (id, parent_id, name, name_lower) VALUES (1, NULL, '/', '/');
WITH RECURSIVE s(i) AS (SELECT 2 UNION ALL SELECT i + 1 FROM s WHERE i < ?)
INSERT INTO dir (id, parent_id, name, name_lower) SELECT i, 1 + (i - 2) / 10, 'd' || i, 'd' || i FROM s`, n); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkUnhashedPaging pages through 5k unhashed files in a tree of 100k
// dirs, as hashing does. Its cost should follow the files, not dirs x pages.
func BenchmarkUnhashedPaging(b *testing.B) {
	const dirs, files = 100000, 5000
	d := dbtest.New(b)
	seedDirs(b, d, dirs)
	if _, err := d.Exec(`WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM s WHERE i < ?)
INSERT INTO instance (dir_id, filename, filename_lower, size) SELECT 2 + i * 19, 'f' || i, 'f' || i, 1 FROM s`, files); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		pages, err := d.UnhashedPages("/", false)
		if err != nil {
			b.Fatal(err)
		}
		n := 0
		for {
			page, err := pages.Next(1000)
			if err != nil {
				b.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			n += len(page)
		}
		if n != files {
			b.Fatal(n)
		}
	}
}

// BenchmarkDeleteOrphans sweeps 500k file rows with none to delete: what every
// scan that changes something pays.
func BenchmarkDeleteOrphans(b *testing.B) {
	const files = 500000
	d := dbtest.New(b)
	seedDirs(b, d, 2)
	if _, err := d.Exec(`WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM s WHERE i < ?)
INSERT INTO file (id, md5, sha256, filesize) SELECT i, 'm' || i, 's' || i, 1 FROM s`, files); err != nil {
		b.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO instance (file_id, dir_id, filename, filename_lower, size)
SELECT id, 2, 'f' || id, 'f' || id, 1 FROM file`); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if n, err := d.DeleteOrphanedFiles(); err != nil || n != 0 {
			b.Fatal(n, err)
		}
	}
}

type nopReporter struct{}

func (nopReporter) Worker(int, string)         {}
func (nopReporter) Speed(int, float64)         {}
func (nopReporter) Scan(string, int, int, int) {}
func (nopReporter) Hashed(scan.Stats)          {}
func (nopReporter) Prune(string, int)          {}
func (nopReporter) Log(string)                 {}

// BenchmarkRescan rescans an unchanged tree of 2k files in 200 dirs: walk, skip
// check and prune, with nothing to write or hash.
func BenchmarkRescan(b *testing.B) {
	d := dbtest.New(b)
	root := b.TempDir()
	for i := range 2000 {
		dir := filepath.Join(root, fmt.Sprintf("a%d", i/200), fmt.Sprintf("b%d", i/10))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", i)), fmt.Append(nil, i), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	opt := scan.Options{MissingOnly: true}
	if _, err := scan.Run(context.Background(), d, []string{root}, opt, nopReporter{}); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		st, err := scan.Run(context.Background(), d, []string{root}, opt, nopReporter{})
		if err != nil || st.Skipped != 2000 {
			b.Fatal(st, err)
		}
	}
}
