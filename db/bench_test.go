package db_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
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
