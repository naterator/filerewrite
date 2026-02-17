# filerewrite

A small utility that rewrites a file’s contents in-place.

It opens each file in read-write mode, reads the data in chunks (default: 8 MB), and immediately writes those exact same bytes back to the same locations using `pread(2)` and `pwrite(2)`. After the rewrite is complete, it restores the original access and modification timestamps.

The tool only processes regular files — symlinks, directories, etc. are skipped. It does **not** try to detect or avoid rewriting the same data through hard links, so hard-linked files will be processed (and rewritten) multiple times.

## Usage

```bash
filerewrite [flags] file ...
```

### Flags

- `-v`, `-verbose`: Enable verbose logging.
- `-b`, `-buffersize`: Rewrite buffer size in MB (default: `8`).
- `-h`, `-help`: Show help.

Buffer size must be greater than `0`.

## Primary Use Case

This is particularly handy on ZFS. When you change properties like compression level, deduplication settings, recordsize, etc., those changes only affect future writes. Already-written blocks stay untouched. Running `filerewrite` forces the file system to re-apply the current settings to existing data.

## Typical Usage Example

```bash
find /path/to/dataset -xdev -type f -print0 | xargs -0 filerewrite
```

Use a larger buffer size, for example 64 MB:

```bash
find /path/to/dataset -xdev -type f -print0 | xargs -0 filerewrite -b 64
```

If any input path might begin with `-`, pass `--` before file arguments:

```bash
filerewrite [flags] -- file1 file2 ...
```

## Important Warnings

- Do **not** run this on a live, in-use filesystem, since there’s an implicit read-write race that can corrupt data if anything modifies the file between the read and the write.
- On ZFS filesystems that have snapshots, rewriting blocks likely doesn’t free any space until all snapshots that reference the old blocks are deleted. This applies to other similar facilities in ZFS that necessitate linking to additional data blocks.

## Portability

Should build and run without issues on FreeBSD, Linux, macOS, and most other modern UNIX-like systems.
