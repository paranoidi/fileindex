# File tags belong to content

File tags are stored against the content's sha256, not against an Instance. Moving, renaming or copying a file therefore needs no handling: any Instance with that content has the tag, however and whenever the index learns of it. Removing a location never removes a tag; only `tag remove`, `tags rm` and `tag-gc` do.

An Instance with no content yet holds a **Pending tag** until its hash lands, because paras-commander tags files before hashing them.

## Considered options

- **Instance tags restored by a later scan.** Save a tag when prune removes its Instance and restore it when the content reappears. Rejected: the tag waits until the old path is rescanned, which users doing partial scans may never do, and choosing which copy gets it needs a timestamp rule with false positives.
- **Keep tagged File rows alive.** Key tags by `file.id` and exempt tagged rows from the orphan sweep. Rejected: absent content would distort `stats` (File count, total size).
- **Triggers on `file` to maintain `gone_at`.** Designed first, as they cover every delete site. Rejected on measurement: the insert trigger made `CommitScanBatch` 88% slower even when it never updated a row, with or without a `WHEN` clause. `tag-gc` re-checks that content is absent instead of trusting `gone_at` alone.

## Consequences

- Tagging or untagging one copy applies to all copies, including copies outside a `tag -r` target. The output says how many.
- An edited file inherits its old content's tags, and the old content keeps them too, until `tag-gc` or until a copy of it returns. Otherwise saving a tagged document would drop its tag.
- A file moved and edited before any scan loses its tag: nothing links the two.
- Empty files can't be tagged, since every empty file is the same content.
- Dir tags still belong to the Dir record and are lost when a dir moves on disk (ADR 0003 keeps them by a print of the dir's files).
