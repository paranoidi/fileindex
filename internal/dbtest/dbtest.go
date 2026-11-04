// Package dbtest seeds index rows directly, so command tests don't need a real scan.
package dbtest

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"path"
	"path/filepath"
	"testing"

	"github.com/paranoidi/fileindex/db"
)

// New returns an initialised DB in a temp dir.
func New(t testing.TB) *db.DB {
	t.Helper()
	return Init(t, filepath.Join(t.TempDir(), "index.sqlite"))
}

// Init opens and initialises the DB at p, closing it when the test ends.
func Init(t testing.TB, p string) *db.DB {
	t.Helper()
	d, err := db.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Init(); err != nil {
		t.Fatal(err)
	}
	return d
}

// Mtime is the mtime AddFile stores.
const Mtime = "2024-01-02 03:04:05"

// AddFile records content as an instance at the absolute path p, creating the dirs
// and the file row (md5, sha256, sha256_8kb of content; no noexif hash) as needed.
// Returns the instance id.
func AddFile(t testing.TB, d *db.DB, p, content string) int64 {
	t.Helper()
	m := md5.Sum([]byte(content))
	s := sha256.Sum256([]byte(content))
	head := content[:min(len(content), 8192)]
	s8 := sha256.Sum256([]byte(head))

	dirID, err := db.GetOrCreatePath(d, path.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	var fileID int64
	err = d.QueryRow("SELECT id FROM file WHERE sha256 = ?", hex.EncodeToString(s[:])).Scan(&fileID)
	if err != nil {
		res, err := d.Exec("INSERT INTO file (md5, sha256, sha256_8kb, filesize) VALUES (?, ?, ?, ?)",
			hex.EncodeToString(m[:]), hex.EncodeToString(s[:]), hex.EncodeToString(s8[:]), len(content))
		if err != nil {
			t.Fatal(err)
		}
		fileID, _ = res.LastInsertId()
	}
	name := path.Base(p)
	res, err := d.Exec("INSERT INTO instance (file_id, dir_id, filename, filename_lower, size, mtime) VALUES (?, ?, ?, ?, ?, ?)",
		fileID, dirID, name, db.Lower(name), len(content), Mtime)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}
