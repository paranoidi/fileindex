package db

import (
	"encoding/json"
)

// dir.unhashed_below is the number of instances with no file (file_id IS NULL)
// in the dir's subtree, the dir's own included. Every writer that adds, removes,
// moves, hashes or unhashes an instance, or moves or deletes a dir, adjusts it
// in its own transaction (bumpUnhashed), so a dir's scanning state reads from one
// row. It is kept in Go and not by triggers: a trigger cannot use WITH, and
// would fire once per level per row (docs/adr/0004).

// unhashedDeltas is the change to the unhashed count of instances, by the dir
// that holds them. The dir's ancestors follow in bumpUnhashed.
type unhashedDeltas map[int64]int64

// add records n unhashed instances gained (or lost, when negative) by dir. A nil
// dir is the virtual root, which holds none.
func (m unhashedDeltas) add(dir *int64, n int64) {
	if dir != nil {
		m[*dir] += n
	}
}

// bumpUnhashed adds each delta to its dir and to every ancestor, in one
// statement. Seeds that share ancestors sum there. Zero sums are not written.
func bumpUnhashed(q Querier, deltas unhashedDeltas) error {
	pairs := make([][2]int64, 0, len(deltas))
	for id, n := range deltas {
		if n != 0 {
			pairs = append(pairs, [2]int64{id, n})
		}
	}
	if len(pairs) == 0 {
		return nil
	}
	list, err := json.Marshal(pairs)
	if err != nil {
		return err
	}
	_, err = q.Exec(`WITH RECURSIVE up(id, n) AS (
    SELECT value->>0, value->>1 FROM json_each(?)
    UNION ALL
    SELECT d.parent_id, up.n FROM up JOIN dir d ON d.id = up.id WHERE d.parent_id IS NOT NULL
)
UPDATE dir SET unhashed_below = unhashed_below + s.n
FROM (SELECT id, SUM(n) AS n FROM up GROUP BY id HAVING SUM(n) <> 0) s WHERE dir.id = s.id`, string(list))
	return err
}

// Recount rebuilds every dir's unhashed count from the instances: a repair for a
// database whose counts were damaged, and what the schema migration runs.
func (d *DB) Recount() error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := recount(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// recount zeroes the counters, then walks up from each dir holding unhashed
// instances.
// ponytail: costs unhashed dirs x depth; a top-down pass over the tree would
// not, if that ever shows.
func recount(q Querier) error {
	if _, err := q.Exec("UPDATE dir SET unhashed_below = 0 WHERE unhashed_below <> 0"); err != nil {
		return err
	}
	_, err := q.Exec(`WITH RECURSIVE up(id, n) AS (
    SELECT dir_id, COUNT(*) FROM instance WHERE file_id IS NULL GROUP BY dir_id
    UNION ALL
    SELECT d.parent_id, up.n FROM up JOIN dir d ON d.id = up.id WHERE d.parent_id IS NOT NULL
)
UPDATE dir SET unhashed_below = s.n FROM (SELECT id, SUM(n) AS n FROM up GROUP BY id) s WHERE dir.id = s.id`)
	return err
}
