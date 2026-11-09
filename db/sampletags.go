package db

import "encoding/json"

// A pending tag lives on its instance, so deleting the instance would end it. When
// the instance has a sample hash, keepPending* first copies its pending tags to
// sample_tag, keyed by size and sample, and reattachSampled hands them to the next
// file or fast-hashed instance with the same pair: the file moved before its full
// hash landed. See docs/adr/0002-pending-tags-kept-by-sample.md.

// keepPendingInsert and keepPendingWhere copy the pending tags of sampled,
// non-empty instances (alias i); callers put the FROM between them and append
// their scope to the WHERE.
const keepPendingInsert = `INSERT OR IGNORE INTO sample_tag (tag_id, size, sha256_sample, created_at)
SELECT p.tag_id, i.size, i.sha256_sample, p.created_at `

const keepPendingWhere = ` CROSS JOIN pending_tag p ON p.instance_id = i.id
WHERE i.sha256_sample IS NOT NULL AND i.size > 0`

// keepPendingFiles keeps the pending tags of the named instances (names is a JSON
// array) of dir.
func keepPendingFiles(q Querier, dir int64, names string) error {
	_, err := q.Exec(keepPendingInsert+"FROM instance i"+keepPendingWhere+
		" AND i.dir_id = ? AND i.filename IN (SELECT value FROM json_each(?))", dir, names)
	return err
}

// keepPendingTree keeps the pending tags of every instance under dir.
func keepPendingTree(q Querier, dir int64) error {
	_, err := q.Exec(treeCTE+keepPendingInsert+"FROM dir_tree t CROSS JOIN instance i ON i.dir_id = t.id"+keepPendingWhere, dir)
	return err
}

// keepPendingInstance keeps the pending tags of one instance.
func keepPendingInstance(q Querier, id int64) error {
	_, err := q.Exec(keepPendingInsert+"FROM instance i"+keepPendingWhere+" AND i.id = ?", id)
	return err
}

// anyPending reports whether any instance holds a pending tag: without one, the
// keepPending* queries have nothing to do.
func anyPending(q Querier) (bool, error) {
	var some bool
	err := q.QueryRow("SELECT EXISTS (SELECT 1 FROM pending_tag)").Scan(&some)
	return some, err
}

// sampleMatch is the condition that an alias st of sample_tag matches file f or
// fast-hashed instance i.
const (
	sampleFileMatch     = `f.sha256_sample = st.sha256_sample AND f.filesize = st.size`
	sampleInstanceMatch = `i.size = st.size AND i.sha256_sample = st.sha256_sample AND i.file_id IS NULL AND i.sha256_sample IS NOT NULL`
)

// reattachSampled gives each sample tag to the content of every file with its size
// and sample (a content tag) and to every fast-hashed instance with them (a
// pending tag), and ends the sample tags that found one. The queries start from
// sample_tag, which is small, so their cost does not follow the index.
func reattachSampled(q Querier) error {
	var some bool
	if err := q.QueryRow("SELECT EXISTS (SELECT 1 FROM sample_tag)").Scan(&some); err != nil || !some {
		return err
	}
	for _, query := range []string{
		`INSERT OR IGNORE INTO content_tag (tag_id, sha256, created_at)
SELECT st.tag_id, f.sha256, st.created_at FROM sample_tag st CROSS JOIN file f ON ` + sampleFileMatch,
		`INSERT OR IGNORE INTO pending_tag (tag_id, instance_id, created_at)
SELECT st.tag_id, i.id, st.created_at FROM sample_tag st CROSS JOIN instance i INDEXED BY idx_instance_fast ON ` + sampleInstanceMatch,
		`DELETE FROM sample_tag AS st WHERE EXISTS (SELECT 1 FROM file f WHERE ` + sampleFileMatch + `)
OR EXISTS (SELECT 1 FROM instance i INDEXED BY idx_instance_fast WHERE ` + sampleInstanceMatch + `)`,
	} {
		if _, err := q.Exec(query); err != nil {
			return err
		}
	}
	return nil
}

// UnsampledInstances counts the target's non-empty instances that have neither a
// full hash nor a sample: a pending tag on one is lost if it goes before a hash,
// where a fast hash would keep it.
func (d *DB) UnsampledInstances(t TagTarget) (int, error) {
	list, err := json.Marshal(t.Instances)
	if err != nil {
		return 0, err
	}
	var n int
	err = d.QueryRow(`SELECT COUNT(*) FROM instance WHERE id IN (SELECT value FROM json_each(?))
AND file_id IS NULL AND sha256_sample IS NULL AND size > 0`, string(list)).Scan(&n)
	return n, err
}
