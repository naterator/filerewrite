# filerewrite

A small utility that rewrites a file’s contents in-place.

It opens each file in read-write mode, verifies that the opened file still matches the path inspected by `lstat(2)`, reads the data in chunks (default: 8 MB), and immediately writes those exact same bytes back to the same locations using `pread(2)` and `pwrite(2)`. After the rewrite is complete, it flushes the rewritten data, restores the original access and modification timestamps through the opened file descriptor, flushes the restored timestamps, and only then closes the file.

Only regular files are rewritten. Paths that cannot be opened or rewritten, plus non-regular files such as symlinks and directories, are reported and contribute to a non-zero exit status. By default, hard-linked files are processed once per path; with `--dedup-hardlinks`, later paths that point at the same device/inode pair are skipped without being treated as failures. With `--skip-sparse`, files that appear sparse based on their allocated block count are skipped instead of being rewritten.

Supported operating systems: Linux, macOS, FreeBSD, NetBSD, and OpenBSD.

## Installation

### Pre-built binaries

Download a binary from the [latest GitHub release](https://github.com/naterator/filerewrite/releases/latest). Binaries are available for all supported OS/architecture combinations (`linux`, `darwin`, `freebsd`, `netbsd`, `openbsd` × `amd64`, `arm64`).

```bash
# Example: Linux amd64
curl -L -o filerewrite https://github.com/naterator/filerewrite/releases/latest/download/filerewrite-linux-amd64
chmod +x filerewrite
sudo mv filerewrite /usr/local/bin/
```

Each release asset includes a `.sha256` checksum file for verification.

### From source

```bash
go install github.com/naterator/filerewrite@latest
```

Or clone and build manually:

```bash
git clone https://github.com/naterator/filerewrite.git
cd filerewrite
go build -ldflags "-s -w" -trimpath
```

### Self-update

An existing installation can update itself to the latest release:

```bash
filerewrite --selfupdate
```

## Usage

```bash
filerewrite [flags] file ...
```

### Flags

- `-v`, `--verbose`: Enable verbose logging.
- `-b`, `--buffersize`: Rewrite buffer size in MB (default: `8`).
- `-n`, `--dry-run`: Report files that would be rewritten without modifying them.
- `--stats`: Print a one-line summary after processing.
- `--dedup-hardlinks`: Skip duplicate hard-linked files within a single invocation.
- `--skip-sparse`: Skip files that appear sparse instead of rewriting them.
- `--selfupdate`: Check GitHub releases for a newer version and replace the current executable. When this flag is present, all other command-line parameters are ignored.
- `--version`: Print the current version and exit.
- `-h`, `--help`: Show help.

Buffer size must be greater than `0` and small enough to fit in the platform `int` range after conversion to bytes.

## Reporting Modes

- `--dry-run` prints a plain `WOULD REWRITE <path>` line to `stderr` for regular files that would be processed and does not open files for write access.
- `--dry-run --dedup-hardlinks` prints a plain `WOULD SKIP HARDLINK <path>` line to `stderr` for later paths that reference the same inode as an earlier path in the same invocation.
- `--dry-run --skip-sparse` prints a plain `WOULD SKIP SPARSE <path>` line to `stderr` for files that would be skipped by the sparse-file guardrail.
- `--stats` prints a plain summary line to `stderr`:
  ```
  Summary: paths=5 rewritten=4 would_rewrite=0 skipped_non_regular=0 skipped_hardlinks=1 skipped_sparse=0 failures=0 bytes_rewritten=10485760
  ```

## Exit Status

- `0`: All requested files were rewritten successfully or intentionally skipped by non-failure options such as `--dedup-hardlinks` or `--skip-sparse`.
- `1`: At least one path could not be rewritten, was missing, was not a regular file, changed identity between `lstat(2)` and `open(2)`, or hit a late flush/close failure.
- `2`: Invalid command-line usage, such as missing file arguments or an invalid buffer size.

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

Preview what would be rewritten and print summary statistics:

```bash
find /path/to/dataset -xdev -type f -print0 | xargs -0 filerewrite --dry-run --stats
```

Avoid rewriting the same inode multiple times when the input includes hard links:

```bash
find /path/to/dataset -xdev -type f -print0 | xargs -0 filerewrite --dedup-hardlinks --stats
```

Skip files that appear sparse so VM images and hole-punched files are left untouched:

```bash
find /path/to/dataset -xdev -type f -print0 | xargs -0 filerewrite --skip-sparse --stats
```

If any input path might begin with `-`, pass `--` before file arguments:

```bash
filerewrite [flags] -- file1 file2 ...
```

## Important Warnings

- Do **not** run this on a live, in-use filesystem, since there’s an implicit read-write race that can corrupt data if anything modifies the file between the read and the write.
- The `lstat(2)`/`open(2)` identity check only protects the gap before the file is opened. It does not make concurrent rewrites safe after the descriptor is open.
- On ZFS filesystems that have snapshots, rewriting blocks likely doesn’t free any space until all snapshots that reference the old blocks are deleted. This applies to other similar facilities in ZFS that necessitate linking to additional data blocks.
- Sparse files can be expanded into fully allocated files when their holes are rewritten. Use `--skip-sparse` if you want an opt-in guardrail for sparse images or VM disks, or dry-run first if you are unsure whether the input set includes them.

## License

[BSD 3-Clause License](LICENSE)
