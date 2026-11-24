# Unhashed files are counted per Dir, kept in Go

A caller that shows a scan's progress per Dir (paras-commander marks a Dir while any file under it still waits for its hash) needs one cheap question for each listed Dir: does an unhashed Instance live anywhere below it? `dir.unhashed_below` answers it: the number of Instances with no File (`file_id IS NULL`) in the Dir's subtree, its own included. `ListDir` reads it as `ListedDir.Unhashed`. A fast hash does not change it: a fast-hashed Instance is still unhashed.

Every writer that adds, removes, moves, hashes or unhashes an Instance, or moves or deletes a Dir, adjusts it in its own transaction through one helper (`bumpUnhashed`): the deltas are keyed by the Dir that holds the Instances, and one statement walks up `parent_id` and adds each delta to its Dir and every ancestor. `Recount` rebuilds all counts from the Instances, and the schema v14 migration runs it.

## Considered options

- **Triggers on `instance`.** Rejected: a trigger cannot use `WITH`, so walking up the ancestors needs a trigger that fires itself once per level, and every inserted or hashed row would pay for it. Scan batches already measure slower with a trigger on `file` (ADR 0001).
- **Recurse down per query.** Count `file_id IS NULL` in the subtree whenever a Dir is listed. Rejected: the cost follows the size of each listed Dir's subtree, which is unbounded (a top-level Dir holds millions of Instances), and a listing asks it for every subdir.
- **A boolean flag on `dir`.** Rejected: setting it is cheap, but clearing it when the last unhashed Instance below goes needs the count anyway.

## Consequences

- A new writer of `instance` or `dir` rows must adjust the counts in the same transaction; `TestUnhashedBelowScenario` checks every writer against a brute-force count.
- In `CommitDiscovered` and `CommitScanBatch` the bump runs before the one INSERT of new Instances: FTS5 flushes a segment at every statement after its writes. `PruneDir` bumps once per commit, not per Dir.
- A row hashed twice or discovered twice in one batch counts once.
- Opening a v13 database needs `upgrade-db`, which counts every unhashed Instance and its ancestors once. Older fileindex binaries refuse a v14 database.
