# fileindex

Indexes local files by content hash (MD5 + SHA-256) into a SQLite database, so you can find duplicates, copies, moves and missing files across disks. JPEGs are also hashed without EXIF metadata, so re-tagged photos still match.

## Install

```sh
go install github.com/paranoidi/fileindex/cmd/fileindex@latest
```

Pure Go (no CGO). Or `task build` / `task install`.

## Usage

```sh
fileindex init                  # create config and database (offers shell autocomplete)
fileindex init --autocomplete   # only register shell autocomplete (bash/zsh/fish)
fileindex init --non-interactive # never prompt
fileindex scan ~/Photos         # index files
fileindex duplicates            # files with more than one copy
fileindex match some/dir        # where else do these files exist?
fileindex find '*.mkv'          # search by name
fileindex verify                # re-check indexed files against disk
fileindex prune                 # forget files gone from disk
```

Other commands: `diff`, `ls`, `tree`, `mv`, `remove`, `tag`/`tags`/`find-tagged`/`tag-gc`, `export`/`import` (CSV), `stats`, `vacuum`, `upgrade-db` (migrate the database after updating fileindex). See `fileindex <command> --help`.

A file's tags belong to its content: every copy has them, and moving, renaming or editing a file keeps them. Tagging or untagging one copy applies to all of them. Empty files can't be tagged. A directory's tags stay on that directory record. When the record goes (the directory moved or was deleted), its tags are kept with a print of its largest files, and a later scan that hashes or samples a directory holding the same files at another path gives it the tags back; a tagged directory moved on disk and scanned only at its new path gets them at once. A directory with no files keeps no print. A tag stays when its content leaves the index, so it can come back with the content. A tag on a file not fully hashed yet stays on that path until its full hash lands, unless the file has a sample (`scan --fast`): then the tag survives the file moving or leaving the index, and returns when a scan finds a file of the same size and sample. `tag-gc DAYS` deletes the tags of content gone for more than DAYS days, and those sample-kept tags and directory prints kept that long.

## Scanning

`scan` works in two phases. First it walks the paths and records every new or changed file (name, size, mtime) right away, in batches, so `find`, `ls` and `tree` see it before any hashing happens. Next it prunes records of files and dirs that are gone from disk, so stale names disappear before any hashing starts. Last it hashes the files that are not hashed yet; duplicates, `match`, `diff` and `verify` only use hashed files. An interrupted scan resumes hashing on the next run without the files having to change, and a file whose size or mtime changed is unhashed again until it is rehashed. `scan --no-hash` stops after the walk and pruning, leaving the files unhashed until a later scan. `export` skips unhashed files.

`scan --fast` (`-f`) hashes only a sample of files larger than 24 KB: their first, middle and last 8 KB, so a big tree is done in a fraction of the time. The middle and last pieces catch a file that is cut short or still being written into its full preallocated size. Those files stay unhashed until a later plain `scan` hashes them fully. `duplicates` lists files with the same size and sample as potential duplicates, and counts the files not fully hashed; `duplicates -f` fully hashes just the potential ones to confirm or rule them out. Re-tagged JPEGs differ in their sample, so only a full scan finds those.

## Configuration

`~/.config/fileindex/config.ini`, created on first run. Controls which files to scan (default `*`, i.e. all; narrow with e.g. `*.jpg,*.mkv`), excluded dirs/files, worker count and database path (default `~/.local/share/fileindex/index.sqlite`). Any option can be overridden with `FILEINDEX_<SECTION>_<OPTION>`.

## Development

```sh
task test     # go test ./...
task verify   # lint + race tests + build
```

Domain terms are in [CONTEXT.md](CONTEXT.md).
