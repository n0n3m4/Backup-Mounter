# backup-mounter

Mounts a squashfs backup of a whole disk on Windows. It opens the squashfs
in-process, finds the raw disk image(s) inside, reads the MBR/GPT partition
table and mounts every readable filesystem as its own read-only drive, from
`Z:` downwards. Nothing is extracted, and no VHD or loop device is attached.

A RAM cache (default 8 GB) keeps reads from a NAS large and sequential:
7/8 holds raw squashfs bytes in 4 MiB chunks, 1/8 holds decompressed
squashfs blocks.

## Requirements

- Windows x64 with [WinFsp](https://winfsp.dev/) installed.
- Building: Go 1.26+ (no cgo).

## Build

    make build      # backup-mounter.exe (CGO_ENABLED=0 GOOS=windows, -tags xz,zstd,lz4)
    make fixtures   # test images, needs mkfs.ext4/btrfs/vfat/ntfs, sgdisk, parted, mksquashfs
    make test

Always build with `-tags xz,zstd,lz4`, or xz/zstd/lz4-compressed squashfs files won't open.

## Usage

    backup-mounter.exe [flags] \\nas\share\backup.squashfs

| flag | meaning |
|---|---|
| `--cache 8G` | RAM cache budget (`512M`, `16G`, ...) |
| `--image path` | disk image inside the squashfs; repeatable; default auto-detects every file with an MBR/GPT or a bare filesystem, largest first |
| `--walk` | instead of mounting, print path, size and sha256 of every file (also works on Linux, which never mounts) |
| `--debug` | WinFsp debug log |

Ctrl-C unmounts everything.

## Filesystems

ext4, btrfs (zlib/lzo/zstd, subvolumes and snapshots shown as directories),
NTFS, FAT32. Partitions: GPT, MBR including logical partitions, or a bare
filesystem image.
Squashfs compression: gzip, xz, zstd, lz4 (incl. `-Xhc`).

Known gaps:
- exFAT, FAT12/16: not supported (skipped with a log line).
- Symlinks (and devices, fifos) appear as empty files; NTFS reparse points
  appear as their plain file/dir. NTFS metafiles (`$MFT`, ...) are hidden.
- FAT32 timestamps are not exposed by the library and show as zero.
- btrfs shows the top-level subvolume, not the `set-default` one; single-device only.
- No read-ahead/prefetch yet; the chunk cache is where it would plug in.
- Each NTFS volume serializes all reads behind one lock; ext4 serializes per open file.

`third_party/` holds two patched forks (changes are marked `backup-mounter` in the source):
`btrfs` (read-only range reader, subvolume crossing, zstd padding fix) and
`squashfs` (1 MiB uncompressed block size mask fix).
