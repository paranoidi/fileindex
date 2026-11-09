# Directory tags are kept by a print of their files

A Dir has no content of its own, so its tags live on its row and ended with it: a directory moved outside paras-commander, moved across file systems by paras-commander (copy, then delete), or deleted and restored from a backup lost its tags. When a tagged Dir is deleted, its tags are now kept with a **print**: totals over its subtree and its 64 largest files, each with its path under the Dir, size, sha256 and sample. At the end of every scan, prune and `HashPaths`, each print looks for dirs holding its content and gives its tags to every one that matches; a print that found one ends.

A candidate is the dir that many levels above an instance with a printed entry's content, so a parent or a child of the moved dir is never mistaken for it. It passes when its own 64 largest files match the print by at least 90% of the larger side's bytes, at least half of the print's bytes match by content (sha256, or size and sample) rather than by path and size, and the print holds at least 1 MiB or three files match by content. Every passing candidate is tagged: as with file tags (ADR 0001), copies share tags.

A tagged Dir still indexed but gone from disk (moved, and only its new path scanned) is matched the same way at the end of a scan that indexed or hashed something, and its tags are copied, not moved: an unmounted disk only looks gone, and the old record keeps its tags until a prune, whose print then finds the new dir tagged already.

## Considered options

- **Match by dir name.** Rejected: renaming is the common move, and many dirs share names (`subs`, `2019`).
- **Match by file names and sizes alone.** Cheap and available after a walk, but a dir of common small files (a `LICENSE`, a template) would match everywhere. Content is required for at least half of the bytes; names only fill in where the two sides have no hash kind in common.
- **Find candidates from each new dir.** Every scan would look up every new file. Starting from the prints keeps the cost with the few prints, and probing only the largest entries that hold over half of a print's bytes cannot miss a passing candidate, since one must match that much by content.
- **Tag only the best candidate, or none when several pass.** Rejected for consistency with file tags.

## Consequences

- A dir's tags return only once the files at its new path are hashed or sampled: a walk alone gives no content to match.
- A print whose dir never returns is looked for at every scan until `tag-gc DAYS` ends it; a print costs about two indexed lookups per probed entry when nothing matches.
- A tagged Dir with no non-empty file keeps no print, and its tags are lost with it, as before.
- Deleting a dir in paras-commander keeps a print, so a copy of it already indexed elsewhere gets its tags at the next scan.
