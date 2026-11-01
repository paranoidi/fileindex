# fileindex

Indexes local files by content hash (MD5 + SHA-256) into a SQLite database, so you can find duplicates, copies, moves and missing files across disks. JPEGs are also hashed without EXIF metadata, so re-tagged photos still match.

## Install

```sh
go install github.com/paranoidi/fileindex/cmd/fileindex@latest
```

Pure Go (no CGO). Or `task build` / `task install`.

## Usage

```sh
fileindex init                  # create config and database
fileindex scan ~/Photos         # index files
fileindex duplicates            # files with more than one copy
fileindex match some/dir        # where else do these files exist?
fileindex find '*.mkv'          # search by name
fileindex verify                # re-check indexed files against disk
fileindex prune                 # forget files gone from disk
```

Other commands: `diff`, `ls`, `tree`, `mv`, `remove`, `tag`/`tags`/`find-tagged`, `export`/`import` (CSV), `stats`, `vacuum`. See `fileindex <command> --help`.

## Scanning

`scan` works in two phases. First it walks the paths and records every new or changed file (name, size, mtime) right away, in batches, so `find`, `ls` and `tree` see it before any hashing happens. Next it prunes records of files and dirs that are gone from disk, so stale names disappear before any hashing starts. Last it hashes the files that are not hashed yet; duplicates, `match`, `diff` and `verify` only use hashed files. An interrupted scan resumes hashing on the next run without the files having to change, and a file whose size or mtime changed is unhashed again until it is rehashed. `scan --no-hash` stops after the walk and pruning, leaving the files unhashed until a later scan. `export` skips unhashed files.

## Configuration

`~/.config/fileindex/config.ini`, created on first run. Controls which files to scan (default `*`, i.e. all; narrow with e.g. `*.jpg,*.mkv`), excluded dirs/files, worker count and database path (default `~/.local/share/fileindex/index.sqlite`). Any option can be overridden with `FILEINDEX_<SECTION>_<OPTION>`.

## Development

```sh
task test     # go test ./...
task verify   # lint + race tests + build
```

Domain terms are in [CONTEXT.md](CONTEXT.md).
