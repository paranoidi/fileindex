package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A Dir tag lives on its Dir row and ends with it. When a tagged Dir is deleted,
// keepDirPrints first stores its print: its tags and its largest files with their
// hashes. ReattachDirTags finds the dirs whose files match a print and gives them
// its tags; ReattachLiveDirTags does the same for tagged Dirs still indexed but
// gone from disk. See docs/adr/0003-dir-tags-kept-by-print.md.

// Print limits and match thresholds.
const (
	printFiles      = 64      // largest files a print holds
	printCoverage   = 9       // tenths of the larger side's top bytes that must match
	printStrongPart = 5       // tenths of the print's top bytes that must match by content
	printMinBytes   = 1 << 20 // a print this large needs no minimum of strong files...
	printMinStrong  = 3       // ...else this many files must match by content
	printMaxCopies  = 1000    // instances looked at per print entry, so a common file stays cheap
)

// relTreeCTE is the dir with id ? and its subtree, each with its path under it
// ("" for the dir itself, else ending in "/").
const relTreeCTE = `WITH RECURSIVE t(id, rel) AS (
    SELECT ?, ''
    UNION ALL
    SELECT d.id, t.rel || d.name || '/' FROM dir d JOIN t ON d.parent_id = t.id
) `

type printEntry struct {
	rel, sha, sample string
	size             int64
}

// dirPrint is a stored print (id set) or one built from a live Dir (dir set).
type dirPrint struct {
	id, dir   int64
	files     int
	bytes     int64
	tags      []int64
	tagNames  []string
	entries   []printEntry
	entrySize int64 // sum of the entries' sizes
}

// printEntries returns the printFiles largest non-empty files under dir, with
// their path under it and their full hash and sample ("" when absent).
func printEntries(q Querier, dir int64) ([]printEntry, int64, error) {
	rows, err := q.Query(relTreeCTE+`SELECT t.rel || i.filename, i.size, COALESCE(f.sha256, ''),
COALESCE(f.sha256_sample, i.sha256_sample, '') FROM t CROSS JOIN instance i ON i.dir_id = t.id
LEFT JOIN file f ON f.id = i.file_id WHERE i.size > 0 ORDER BY i.size DESC, 1 LIMIT ?`, dir, printFiles)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []printEntry
	var sum int64
	for rows.Next() {
		var e printEntry
		if err := rows.Scan(&e.rel, &e.size, &e.sha, &e.sample); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
		sum += e.size
	}
	return out, sum, rows.Err()
}

// livePrint builds the print of an indexed Dir: totals over its subtree, its tags
// and its entries.
func livePrint(q Querier, dir int64) (dirPrint, error) {
	p := dirPrint{dir: dir}
	if err := q.QueryRow(treeCTE+`SELECT COUNT(*), COALESCE(SUM(i.size), 0) FROM dir_tree t
CROSS JOIN instance i ON i.dir_id = t.id`, dir).Scan(&p.files, &p.bytes); err != nil {
		return p, err
	}
	if err := p.loadTags(q, `SELECT t.id, t.name FROM dir_tag dt CROSS JOIN tag t ON t.id = dt.tag_id WHERE dt.dir_id = ? ORDER BY t.name`, dir); err != nil {
		return p, err
	}
	var err error
	p.entries, p.entrySize, err = printEntries(q, dir)
	return p, err
}

func (p *dirPrint) loadTags(q Querier, query string, arg int64) error {
	rows, err := q.Query(query, arg)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		p.tags, p.tagNames = append(p.tags, id), append(p.tagNames, name)
	}
	return rows.Err()
}

// keepDirPrints stores the print of every tagged Dir in the tree at root (path
// its path) before the tree is deleted. log (may be nil) gets a line per tagged
// Dir: kept, or lost when it holds no non-empty file to match by.
func keepDirPrints(q Querier, root int64, path string, log func(string)) error {
	var some bool
	if err := q.QueryRow("SELECT EXISTS (SELECT 1 FROM dir_tag)").Scan(&some); err != nil || !some {
		return err
	}
	rows, err := q.Query(relTreeCTE+`SELECT DISTINCT t.id, t.rel FROM t CROSS JOIN dir_tag dt ON dt.dir_id = t.id ORDER BY 2`, root)
	if err != nil {
		return err
	}
	type tagged struct {
		id  int64
		rel string
	}
	var dirs []tagged
	for rows.Next() {
		var t tagged
		if err := rows.Scan(&t.id, &t.rel); err != nil {
			rows.Close()
			return err
		}
		dirs = append(dirs, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range dirs {
		p, err := livePrint(q, t.id)
		if err != nil {
			return err
		}
		at := filepath.Join(path, t.rel)
		if len(p.entries) == 0 {
			if log != nil {
				log("Lost " + tagList(p.tagNames) + " on directory with no files: " + at)
			}
			continue
		}
		if err := storePrint(q, p); err != nil {
			return err
		}
		if log != nil {
			log("Kept " + tagList(p.tagNames) + " of directory by its files: " + at)
		}
	}
	return nil
}

func storePrint(q Querier, p dirPrint) error {
	var id int64
	if err := q.QueryRow("INSERT INTO dir_print (files, bytes) VALUES (?, ?) RETURNING id", p.files, p.bytes).Scan(&id); err != nil {
		return err
	}
	if _, err := q.Exec("INSERT INTO dir_print_tag (print_id, tag_id, created_at) SELECT ?, tag_id, created_at FROM dir_tag WHERE dir_id = ?", id, p.dir); err != nil {
		return err
	}
	for _, e := range p.entries {
		if _, err := q.Exec("INSERT INTO dir_print_entry (print_id, rel, size, sha256, sha256_sample) VALUES (?, ?, ?, NULLIF(?, ''), NULLIF(?, ''))",
			id, e.rel, e.size, e.sha, e.sample); err != nil {
			return err
		}
	}
	return nil
}

// loadPrint reads a stored print.
func loadPrint(q Querier, id int64) (dirPrint, error) {
	p := dirPrint{id: id}
	if err := q.QueryRow("SELECT files, bytes FROM dir_print WHERE id = ?", id).Scan(&p.files, &p.bytes); err != nil {
		return p, err
	}
	if err := p.loadTags(q, `SELECT t.id, t.name FROM dir_print_tag pt CROSS JOIN tag t ON t.id = pt.tag_id WHERE pt.print_id = ? ORDER BY t.name`, id); err != nil {
		return p, err
	}
	rows, err := q.Query("SELECT rel, size, COALESCE(sha256, ''), COALESCE(sha256_sample, '') FROM dir_print_entry WHERE print_id = ? ORDER BY size DESC, rel", id)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var e printEntry
		if err := rows.Scan(&e.rel, &e.size, &e.sha, &e.sample); err != nil {
			return p, err
		}
		p.entries = append(p.entries, e)
		p.entrySize += e.size
	}
	return p, rows.Err()
}

// candidates returns the dirs that may be the print's dir: for each probed entry
// with a hash, the dirs holding that content, each lifted by the entry's depth
// under the print's dir. A dir is a candidate only through a content match. A dir
// that passes matches more than half of the print's top bytes by content, so
// probing the largest entries until they hold over half of those bytes cannot
// miss one; for a film that is one entry.
func candidates(q Querier, p dirPrint) ([]int64, error) {
	parents := map[int64]sql.NullInt64{}
	ancestor := func(dir int64, depth int) (int64, bool, error) {
		for range depth {
			parent, ok := parents[dir]
			if !ok {
				if err := q.QueryRow("SELECT parent_id FROM dir WHERE id = ?", dir).Scan(&parent); err != nil {
					return 0, false, err
				}
				parents[dir] = parent
			}
			if !parent.Valid {
				return 0, false, nil
			}
			dir = parent.Int64
		}
		return dir, true, nil
	}
	seen := map[int64]bool{}
	var out []int64
	probed := int64(0)
	for _, e := range p.entries { // largest first
		if probed*10 > p.entrySize*(10-printStrongPart) {
			break
		}
		probed += e.size
		var dirs []int64
		if e.sha != "" {
			ids, err := queryIDs(q, `SELECT i.dir_id FROM file f CROSS JOIN instance i ON i.file_id = f.id WHERE f.sha256 = ? LIMIT ?`, e.sha, printMaxCopies)
			if err != nil {
				return nil, err
			}
			dirs = append(dirs, ids...)
		}
		if e.sample != "" {
			if e.sha == "" { // with a sha256, the query above found these files already
				ids, err := queryIDs(q, `SELECT i.dir_id FROM file f CROSS JOIN instance i ON i.file_id = f.id
WHERE f.sha256_sample = ?1 AND f.filesize = ?2 LIMIT ?3`, e.sample, e.size, printMaxCopies)
				if err != nil {
					return nil, err
				}
				dirs = append(dirs, ids...)
			}
			ids, err := queryIDs(q, `SELECT dir_id FROM instance INDEXED BY idx_instance_fast
WHERE size = ? AND sha256_sample = ? AND file_id IS NULL AND sha256_sample IS NOT NULL LIMIT ?`, e.size, e.sample, printMaxCopies)
			if err != nil {
				return nil, err
			}
			dirs = append(dirs, ids...)
		}
		depth := strings.Count(e.rel, "/")
		for _, dir := range dirs {
			c, ok, err := ancestor(dir, depth)
			if err != nil {
				return nil, err
			}
			if ok && !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out, nil
}

// printMatch is how well a candidate dir matches a print.
type printMatch struct {
	files, strongFiles int
	bytes, strongBytes int64
	candidateSize      int64
}

// score matches the candidate's own largest files against the print. A file
// matches an unused entry by content (strong: same sha256, or same size and
// sample) wherever it lies, else by path and size when the two have no hash kind
// in common (weak).
func score(q Querier, p dirPrint, cand int64) (printMatch, error) {
	var m printMatch
	ce, size, err := printEntries(q, cand)
	if err != nil {
		return m, err
	}
	m.candidateSize = size
	type sampleKey struct {
		size   int64
		sample string
	}
	bySha, bySample, byRel := map[string][]int{}, map[sampleKey][]int{}, map[string]int{}
	for i, e := range p.entries {
		if e.sha != "" {
			bySha[e.sha] = append(bySha[e.sha], i)
		}
		if e.sample != "" {
			k := sampleKey{e.size, e.sample}
			bySample[k] = append(bySample[k], i)
		}
		byRel[e.rel] = i
	}
	used := make([]bool, len(p.entries))
	free := func(idx []int) int {
		for _, i := range idx {
			if !used[i] {
				return i
			}
		}
		return -1
	}
	for _, c := range ce {
		strong, i := true, -1
		if c.sha != "" {
			i = free(bySha[c.sha])
		}
		if i < 0 && c.sample != "" {
			i = free(bySample[sampleKey{c.size, c.sample}])
		}
		if i < 0 {
			strong = false
			if j, ok := byRel[c.rel]; ok && !used[j] && p.entries[j].size == c.size && !comparable(p.entries[j], c) {
				i = j
			}
		}
		if i < 0 {
			continue
		}
		used[i] = true
		m.files++
		m.bytes += c.size
		if strong {
			m.strongFiles++
			m.strongBytes += c.size
		}
	}
	return m, nil
}

// comparable reports whether two entries share a hash kind, so content decides
// whether they match and path and size do not.
func comparable(a, b printEntry) bool {
	return a.sha != "" && b.sha != "" || a.sample != "" && b.sample != ""
}

// passes applies the thresholds: enough of both sides' top bytes match, enough of
// it by content, and enough evidence that the content is not just common.
func (m printMatch) passes(p dirPrint) bool {
	larger := max(p.entrySize, m.candidateSize)
	return larger > 0 && m.bytes*10 >= larger*printCoverage &&
		m.strongBytes*10 >= p.entrySize*printStrongPart &&
		(p.bytes >= printMinBytes || m.strongFiles >= printMinStrong)
}

// restore adds the print's tags to dir and returns the line to log (line formats
// the tags and dir's path), "" when dir already had them all.
func restore(q Querier, p dirPrint, dir int64, m printMatch, line func(tags, path string) string) (string, error) {
	added := int64(0)
	for _, tag := range p.tags {
		r, err := q.Exec("INSERT OR IGNORE INTO dir_tag (tag_id, dir_id) VALUES (?, ?)", tag, dir)
		if err != nil {
			return "", err
		}
		n, _ := r.RowsAffected()
		added += n
	}
	if added == 0 {
		return "", nil
	}
	path, err := BuildDirPath(q, &dir)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s (%d of %d files matched)", line(tagList(p.tagNames), path), m.files, len(p.entries)), nil
}

func tagList(names []string) string {
	if len(names) == 1 {
		return "tag #" + names[0]
	}
	return "tags #" + strings.Join(names, ", #")
}

// ReattachDirTags gives the tags of each kept print to every dir that matches it
// and ends the prints that found one; prints whose tags were all deleted end too.
// It returns a line per dir that gained tags. Its cost follows the prints, not
// the index.
func (d *DB) ReattachDirTags() ([]string, error) {
	var some bool
	if err := d.QueryRow("SELECT EXISTS (SELECT 1 FROM dir_print)").Scan(&some); err != nil || !some {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := &stmtCache{tx: tx, stmts: map[string]*sql.Stmt{}}
	if _, err := tx.Exec("DELETE FROM dir_print WHERE NOT EXISTS (SELECT 1 FROM dir_print_tag WHERE print_id = dir_print.id)"); err != nil {
		return nil, err
	}
	ids, err := queryIDs(tx, "SELECT id FROM dir_print ORDER BY id")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, id := range ids {
		p, err := loadPrint(q, id)
		if err != nil {
			return nil, err
		}
		lines, matched, err := matchPrint(q, p, nil, func(tags, path string) string {
			return "Restored " + tags + " on directory: " + path
		})
		if err != nil {
			return nil, err
		}
		out = append(out, lines...)
		if matched {
			if _, err := tx.Exec("DELETE FROM dir_print WHERE id = ?", id); err != nil {
				return nil, err
			}
		}
	}
	return out, tx.Commit()
}

// matchPrint restores p's tags on every candidate that passes and skip does not
// rule out, and reports whether any passed.
func matchPrint(q Querier, p dirPrint, skip func(dir int64) (bool, error), line func(tags, path string) string) (lines []string, matched bool, err error) {
	cands, err := candidates(q, p)
	if err != nil {
		return nil, false, err
	}
	for _, c := range cands {
		if skip != nil {
			if s, err := skip(c); err != nil {
				return nil, false, err
			} else if s {
				continue
			}
		}
		m, err := score(q, p, c)
		if err != nil {
			return nil, false, err
		}
		if !m.passes(p) {
			continue
		}
		matched = true
		l, err := restore(q, p, c, m, line)
		if err != nil {
			return nil, false, err
		}
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines, matched, nil
}

// ReattachLiveDirTags copies the tags of each tagged Dir that is gone from disk
// but still indexed (moved, and only its new path scanned) to every dir on disk
// that matches it. The old record keeps its tags until a prune, whose print then
// finds the new dir tagged already. Copying, not moving, keeps an unmounted disk
// that only looks gone from costing a tag. It stats each tagged Dir.
func (d *DB) ReattachLiveDirTags() ([]string, error) {
	tagged, err := queryIDs(d, "SELECT DISTINCT dir_id FROM dir_tag WHERE dir_id IS NOT NULL")
	if err != nil || len(tagged) == 0 {
		return nil, err
	}
	paths := dirPaths{}
	type source struct {
		id   int64
		path string
	}
	var gone []source
	for _, id := range tagged {
		p, err := paths.dir(d, id)
		if err != nil {
			return nil, err
		}
		if g, _, err := missing(p); err == nil && g {
			gone = append(gone, source{id, p})
		}
	}
	if len(gone) == 0 {
		return nil, nil
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := &stmtCache{tx: tx, stmts: map[string]*sql.Stmt{}}
	var out []string
	for _, s := range gone {
		p, err := livePrint(q, s.id)
		if err != nil {
			return nil, err
		}
		if len(p.entries) == 0 {
			continue
		}
		skip := func(c int64) (bool, error) {
			if c == s.id {
				return true, nil
			}
			cp, err := paths.dir(q, c)
			if err != nil {
				return false, err
			}
			if strings.HasPrefix(cp, s.path+string(os.PathSeparator)) {
				return true, nil // a stale record under the source
			}
			g, _, err := missing(cp)
			return g || err != nil, nil
		}
		lines, _, err := matchPrint(q, p, skip, func(tags, path string) string {
			return "Copied " + tags + " of moved directory " + s.path + " to: " + path
		})
		if err != nil {
			return nil, err
		}
		out = append(out, lines...)
	}
	return out, tx.Commit()
}
