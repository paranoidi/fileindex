package db

import (
	"slices"
	"strings"
)

// treeCTE selects a dir and all its descendants as dir_tree(id); bind the root id.
const treeCTE = `WITH RECURSIVE dir_tree AS (
    SELECT id FROM dir WHERE id = ?
    UNION ALL
    SELECT d.id FROM dir d JOIN dir_tree dt ON d.parent_id = dt.id
) `

// Match is one instance of a file found by hash.
type Match struct {
	Path                    string
	Size                    int64
	MD5, SHA256, SHA256_8KB string
	NoExifJPEG              string // "" when NULL
	Mtime                   string // raw stored text, "" when NULL
}

// FindByHash returns every instance whose file has h as any of its four hashes,
// sorted by path. It reads in one transaction so a concurrent prune
// or mv can't delete a dir between the instance query and the path lookups.
func (d *DB) FindByHash(h string) ([]Match, error) {
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT i.dir_id, i.filename, i.size, f.md5, f.sha256,
COALESCE(f.sha256_8kb, ''), COALESCE(f.sha256_noexif_jpeg, ''), COALESCE(CAST(i.mtime AS TEXT), '')
FROM file f JOIN instance i ON f.id = i.file_id
WHERE f.md5 = ?1 OR f.sha256 = ?1 OR f.sha256_noexif_jpeg = ?1 OR f.sha256_8kb = ?1`, h)
	if err != nil {
		return nil, err
	}
	type raw struct {
		dir  int64
		name string
		m    Match
	}
	var found []raw
	for rows.Next() {
		var r raw
		if err := rows.Scan(&r.dir, &r.name, &r.m.Size, &r.m.MD5, &r.m.SHA256, &r.m.SHA256_8KB, &r.m.NoExifJPEG, &r.m.Mtime); err != nil {
			rows.Close()
			return nil, err
		}
		found = append(found, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Match, len(found))
	paths := dirPaths{}
	for i, r := range found {
		if r.m.Path, err = paths.file(tx, r.dir, r.name); err != nil {
			return nil, err
		}
		out[i] = r.m
	}
	slices.SortFunc(out, func(a, b Match) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// likePattern turns a glob into a LIKE pattern for ESCAPE '\': % _ \ are
// literal, * and ? are wildcards.
func likePattern(glob string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`, `*`, `%`, `?`, `_`)
	return r.Replace(glob)
}

// trigramPattern turns a glob into a LIKE pattern for instance_trigram, which
// can't use its index with ESCAPE or without a 3-char literal run. Literal
// % _ \ stay unescaped, so they act as wildcards: the result is a superset that
// the exact likePattern check narrows. ok is false when the index can't help.
func trigramPattern(glob string) (like string, ok bool) {
	like = strings.NewReplacer("*", "%", "?", "_").Replace(glob)
	run := 0
	for _, r := range like {
		if r == '%' || r == '_' {
			run = 0
		} else if run++; run >= 3 {
			ok = true
		}
	}
	return like, ok
}

// Found is one find result. Dirs have IsDir set and no size or mtime.
type Found struct {
	IsDir bool
	Name  string
	Path  string
	Size  int64
	Mtime string
	// Hashed is false while the instance waits to be hashed (dirs: always true).
	Hashed bool
}

// FindByName matches basenames under the dir root (not root itself),
// case-insensitively: the pattern is lowered like the stored *_lower columns.
// Files sorted by name come first, then dirs sorted by name.
func (d *DB) FindByName(pattern string, root int64, files, dirs bool) ([]Found, error) {
	lower := Lower(pattern)
	like := likePattern(lower)
	var out []Found
	if files {
		var err error
		if tri, ok := trigramPattern(lower); ok {
			out, err = d.findFiles("i.id IN (SELECT rowid FROM instance_trigram WHERE filename LIKE ?) AND i.filename_lower LIKE ? ESCAPE '\\'", root, tri, like)
		} else {
			out, err = d.findFiles("i.filename_lower LIKE ? ESCAPE '\\'", root, like)
		}
		if err != nil {
			return nil, err
		}
	}
	if dirs {
		// ponytail: dirs are ~10x fewer than instances, so plain LIKE; add a dir trigram table if this shows up slow
		rows, err := d.Query(treeCTE+`SELECT d.id, d.name FROM dir d
WHERE d.id IN (SELECT id FROM dir_tree) AND d.name_lower LIKE ? ESCAPE '\' AND d.id IS NOT ?
ORDER BY d.name`, root, like, root)
		if err != nil {
			return nil, err
		}
		var ids []int64
		start := len(out)
		for rows.Next() {
			f := Found{IsDir: true, Hashed: true}
			var id int64
			if err := rows.Scan(&id, &f.Name); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, f)
			ids = append(ids, id)
		}
		rows.Close()
		paths := dirPaths{}
		if err := paths.preload(d, ids); err != nil {
			return nil, err
		}
		for i, id := range ids {
			if out[start+i].Path, err = paths.dir(d, id); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// FindByWords matches filenames under the dir root against an FTS5 query
// (words, prefix*, "phrases", AND/OR/NOT), ignoring case and diacritics.
// Results are sorted by name.
func (d *DB) FindByWords(query string, root int64) ([]Found, error) {
	return d.findFiles("i.id IN (SELECT rowid FROM instance_words WHERE instance_words MATCH ?)", root, query)
}

// findFiles returns the instances under root matching cond, sorted by name.
func (d *DB) findFiles(cond string, root int64, args ...any) ([]Found, error) {
	rows, err := d.Query(treeCTE+`SELECT i.dir_id, i.filename, i.size, COALESCE(CAST(i.mtime AS TEXT), ''), i.file_id IS NOT NULL
FROM instance i
WHERE i.dir_id IN (SELECT id FROM dir_tree) AND `+cond+`
ORDER BY i.filename, i.id`, append([]any{root}, args...)...)
	if err != nil {
		return nil, err
	}
	var out []Found
	var dirIDs []int64
	for rows.Next() {
		var f Found
		var dir int64
		if err := rows.Scan(&dir, &f.Name, &f.Size, &f.Mtime, &f.Hashed); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, f)
		dirIDs = append(dirIDs, dir)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	paths := dirPaths{}
	if err := paths.preload(d, dirIDs); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Path, err = paths.file(d, dirIDs[i], out[i].Name); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ListedFile is one file row of a directory listing.
type ListedFile struct {
	Name  string
	Size  int64
	Mtime string // "" when NULL
	// Hashed is false while the file waits to be hashed.
	Hashed bool
	Tags   []string
}

// ListedDir is one subdir row of a directory listing.
type ListedDir struct {
	Name string
	Tags []string
}

// ListDir returns a dir's files and subdirs, each sorted by name, with their
// tags, in four queries whatever the dir holds.
// ponytail: dirs carry no size or mtime; tree totals cost a subtree walk each.
func (d *DB) ListDir(id int64) (files []ListedFile, subdirs []ListedDir, err error) {
	tags, err := d.tagsByName(fileTags, id)
	if err != nil {
		return nil, nil, err
	}
	rows, err := d.Query(`SELECT i.filename, i.size, COALESCE(CAST(i.mtime AS TEXT), ''), i.file_id IS NOT NULL
FROM instance i WHERE i.dir_id = ? ORDER BY i.filename`, id)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var f ListedFile
		if err := rows.Scan(&f.Name, &f.Size, &f.Mtime, &f.Hashed); err != nil {
			rows.Close()
			return nil, nil, err
		}
		f.Tags = tags[f.Name]
		files = append(files, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	if tags, err = d.tagsByName(subdirTags, id); err != nil {
		return nil, nil, err
	}
	names, err := d.strings("SELECT name FROM dir WHERE parent_id = ? ORDER BY name", id)
	if err != nil {
		return nil, nil, err
	}
	for _, n := range names {
		subdirs = append(subdirs, ListedDir{Name: n, Tags: tags[n]})
	}
	return files, subdirs, nil
}
