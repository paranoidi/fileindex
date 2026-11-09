# fileindex

Indexes local files by content hash into a SQLite database so copies, moves and missing files can be found.

## Language

**File**:
A unique piece of content, identified by its hashes and size. Many locations can hold the same File.
_Avoid_: blob, content

**Instance**:
One location of a File: a filename inside a Dir. It records the size and mtime seen at the last walk or hash. An Instance is **unhashed** (no File yet) from the moment the walk finds it until it is hashed; it is findable by name meanwhile. An unhashed Instance can be **fast-hashed**: it records its **Sample**, but still has no File. A scan runs in this order: walk (index), prune, hash.
_Avoid_: copy, path entry

**Dir**:
A directory node in the indexed tree; Dirs form a hierarchy from the filesystem root.

**Tag**:
A user-assigned label on a File's content or on a Dir. A File's Tag outlives its File: it applies again when the same content returns.

**Sample**:
The hash of a file's first, middle and last 8 KB (the whole file up to 24 KB). With the size, a cheap stand-in for the content that a cut-short or partly written file does not share.
_Avoid_: fast hash, head hash, quick hash

**Pending tag**:
A Tag held by an unhashed Instance until its hash lands, when it moves to the content.

**Sample tag**:
A Pending tag whose fast-hashed Instance was deleted, kept by size and Sample until a File or fast-hashed Instance with the same pair takes it back.

**Duplicate**:
A File that has more than one Instance.

**Potential duplicate**:
Instances with the same size and Sample, at least one of them only fast-hashed, so a full hash may still tell them apart.

## Relationships

- A **File** has one or more hashed **Instances**; an unhashed **Instance** has no **File**
- An **Instance** lives in exactly one **Dir**
- A **Dir** has zero or one parent **Dir**
- A **Tag** attaches to the content of a **File** and to **Dirs**, so every **Instance** of a **File** has its **Tags**
