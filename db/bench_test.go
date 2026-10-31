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
