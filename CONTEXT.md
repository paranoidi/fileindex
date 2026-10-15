# fileindex

Indexes local files by content hash into a SQLite database so copies, moves and missing files can be found.

## Language

**File**:
A unique piece of content, identified by its hashes and size. Many locations can hold the same File.
_Avoid_: blob, content

**Instance**:
One location of a File: a filename inside a Dir.
_Avoid_: copy, path entry

**Dir**:
A directory node in the indexed tree; Dirs form a hierarchy from the filesystem root.

**Tag**:
A user-assigned label on an Instance or a Dir.

**Duplicate**:
A File that has more than one Instance.

## Relationships

- A **File** has one or more **Instances**
- An **Instance** lives in exactly one **Dir**
- A **Dir** has zero or one parent **Dir**
- A **Tag** attaches to **Instances** and **Dirs**, not to **Files**
