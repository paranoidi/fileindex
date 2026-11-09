# Pending tags are kept by sample

A pending tag lives on its Instance, so a file that moves or leaves before its full hash lands used to lose its tag at the next prune. paras-commander tags whole trees of large files (a directory of films marked "watched") and must not read terabytes to make those tags durable. So the cheap hash is the sample, the hash of a file's first, middle and last 8 KB (the whole file up to 24 KB), and an Instance's pending tags are copied to `sample_tag`, keyed by size and sample, when the Instance is deleted. Every hash commit gives each sample tag to the content of any File with that size and sample (a content tag) and to any fast-hashed Instance with them (a pending tag), and ends it.

The sample replaces the first-8-KB hash everywhere, potential duplicates included. Head alone matches a download still in progress, or one cut short, against the finished file. The last 8 KB tell those apart, even when a client preallocates the full size, and the middle 8 KB catch a gap left in between.

## Considered options

- **Tags on (size, sample) from the start.** Store every pending tag by sample instead of by Instance. Rejected: an unhashed Instance has no sample until a fast hash, and the tag must show at once on the path the user tagged.
- **A delete trigger on instance.** It would cover every delete site, but the insert trigger measured in ADR 0001 cost 88% on scan batches. The four delete sites (prune of files, prune and removal of trees, removal of one instance) call `keepPending*` explicitly, after one cheap `EXISTS` on `pending_tag`.
- **Head and tail only.** One seek fewer per file, but misses a file whose middle is still a hole.

## Consequences

- Two different files with the same size and sample would share a sample tag. For media this does not happen; files of a fixed size that differ only in the unsampled bytes (disk images, databases) could. The sample decides only where a lost tag goes back to, never whether two files are duplicates.
- A pending tag on an Instance with no sample is still lost when the Instance goes. paras-commander fast-hashes what it tags.
- A sample tag returns only when a File or fast-hashed Instance with its pair is committed; a walk alone does not bring it back.
- `tag-gc DAYS` also ends sample tags kept for more than DAYS days.
- Schema v12 clears every stored first-8-KB hash, as none is a sample: fast-hashed files are plain unhashed ones until a scan hashes them again.
