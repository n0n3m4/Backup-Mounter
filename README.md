# backup-mounter

Mounts a disk image backup on Windows: a squashfs holding disk images, a
VHD/VHDX, a raw MBR/GPT disk image, or a single-partition (bare filesystem)
image (optionally gzip/xz compressed), or an Acronis True Image `.tib`
partition backup, all detected by signature. It opens everything in-process, reads the
partition table and mounts every readable filesystem as its own read-only
drive, from `Z:` downwards. Nothing is extracted, and no loop device is
attached.

A RAM cache (default 8 GB) keeps reads from a NAS large and sequential:
7/8 holds raw squashfs bytes in 4 MiB chunks, 1/8 holds decompressed
squashfs blocks.

## Requirements

- Windows x64 with [WinFsp](https://winfsp.dev/) installed.
- Building: Go 1.26+ (no cgo).

## Build

    make build      # backup-mounter.exe (CGO_ENABLED=0 GOOS=windows, -tags xz,zstd,lz4)
    make fixtures   # test images, needs mkfs.ext4/btrfs/vfat/ntfs/exfat, sgdisk, parted, mksquashfs, qemu-img
    make test

Always build with `-tags xz,zstd,lz4`, or xz/zstd/lz4-compressed squashfs files won't open.

## Usage

    backup-mounter.exe [flags] \\nas\share\backup.squashfs   # or disk.vhd, disk.vhdx, disk.img, disk.img.gz, disk.img.xz, backup.tib

| flag | meaning |
|---|---|
| `--cache 8G` | RAM cache budget (`512M`, `16G`, ...) |
| `--image path` | disk image inside the squashfs; repeatable; default auto-detects every VHD/VHDX, MBR/GPT disk or bare filesystem, largest first |
| `--walk` | instead of mounting, print path, size and sha256 of every file (also works on Linux, which never mounts) |
| `--debug` | WinFsp debug log |

Ctrl-C unmounts everything.

## Filesystems

ext4, btrfs (zlib/lzo/zstd, subvolumes and snapshots shown as directories),
NTFS, FAT32, exFAT. Partitions: GPT, MBR including logical partitions, or a bare
filesystem image.
Squashfs compression: gzip, xz, zstd, lz4 (incl. `-Xhc`).

Known gaps:
- Acronis `.tib`: True Image 2014-2017 full partition backups only (unencrypted,
  single volume). Incremental/differential, encrypted, TI 2018+ (and `.tibx`)
  and TI 2013-or-older archives are refused with an error.
- FAT12/16: not supported (skipped with a log line).
- Symlinks (and devices, fifos) appear as empty files; NTFS reparse points
  appear as their plain file/dir. NTFS metafiles (`$MFT`, ...) are hidden.
- FAT32 and exFAT timestamps are not exposed by the libraries and show as zero.
- btrfs shows the top-level subvolume, not the `set-default` one; single-device only.
- No read-ahead/prefetch yet; the chunk cache is where it would plug in.
- Each NTFS volume serializes all reads behind one lock; ext4 serializes per open file.

`third_party/` holds three patched forks (changes are marked `backup-mounter` in the source):
`btrfs` (read-only range reader, subvolume crossing, zstd padding fix),
`exfat` (multi-cluster NoFatChain directories listed in full) and
`squashfs` (1 MiB uncompressed block size mask fix).
