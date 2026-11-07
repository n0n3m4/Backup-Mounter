# backup-mounter

Mounts a disk image backup on Windows: a squashfs holding disk images, a
VHD/VHDX, a raw MBR/GPT disk image, or a single-partition (bare filesystem)
image (optionally gzip/xz compressed), an Acronis True Image `.tib`
partition backup, or a qcow2 / VMDK virtual disk, all detected by signature. It opens everything in-process, reads the
partition table and mounts every readable filesystem as its own read-only
drive, from `Z:` downwards. A `.tar`, `.tar.gz`, `.tar.xz` or `.7z` archive is
mounted as one drive labelled with the archive's file name, and so is an EROFS
image (a folder of its files; `--image path/in/erofs` mounts the disk images
inside it instead). Nothing is extracted, and no loop device is
attached.

A RAM cache (default 4 GB, `--cache`) keeps reads from a NAS large and sequential.
For a squashfs, 7/8 holds raw bytes in 4 MiB chunks and 1/8 decompressed squashfs
blocks; for gzip/xz, qcow2/VMDK, 7z and EROFS the split is the other way round
(1/8 raw, 7/8 decompressed); a `.tib` uses all of it for decompressed blocks.

## Requirements

- Windows x64 with [WinFsp](https://winfsp.dev/) installed.
- Building: Go 1.26+ (no cgo).

## Build

    make build      # backup-mounter.exe (CGO_ENABLED=0 GOOS=windows, -tags xz,zstd,lz4)
    make fixtures   # test images, needs mkfs.ext4/btrfs/vfat/ntfs/exfat, sgdisk, parted, mksquashfs, qemu-img, 7z, mkfs.erofs, erofs-dedup ($EROFS_DEDUP);
                    # ntfs-3g (FUSE, optional) for the NTFS compressed/sparse files
    make test

Always build with `-tags xz,zstd,lz4`, or xz/zstd/lz4-compressed squashfs files won't open.

## Usage

    backup-mounter.exe [flags] \\nas\share\backup.squashfs   # or disk.vhd, disk.vhdx, disk.img, disk.img.gz, disk.img.xz, backup.tib, disk.qcow2, disk.vmdk, backup.tar[.gz|.xz], backup.7z, backup.erofs

| flag | meaning |
|---|---|
| `--cache 4G` | RAM cache budget (`512M`, `16G`, ...); the process stays near this plus a quarter |
| `--readahead 16M` | decode-ahead window per file (squashfs images, .tib, EROFS); raise it to keep more cores busy, `0` turns it off |
| `--image path` | disk image inside the squashfs or erofs (every candidate is listed in the log at mount time); repeatable; squashfs default auto-detects every VHD/VHDX, MBR/GPT disk or bare filesystem, largest first; erofs default mounts the erofs itself as one folder |
| `--watchdog 1m` | a read stuck this long makes the mounter dump goroutines, unmount and exit, so a frozen volume frees itself (`0` = never) |
| `--log file` | append the log to a file instead of the console (with `--debug` also WinFsp's request trace); a read stuck for 30 s is logged with a goroutine dump |
| `--walk` | instead of mounting, print path, size and sha256 of every file (also works on Linux, which never mounts) |
| `--debug` | WinFsp request trace, plus a heap/cache/reads-in-progress line every 10 s; if memory jumps far past the cache budget, writes `heap-<unix>.pprof` next to the log and dumps every goroutine |

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
- qcow2: standalone images only (plain, zlib or zstd compressed); backing files
  (overlays) and encryption are refused.
- VMDK: monolithicSparse, streamOptimized (OVA exports) and descriptor files with
  FLAT/VMFS/SPARSE/ZERO extents (twoGbMaxExtent*, ESXi `-flat.vmdk`; the extent
  files must sit next to the descriptor). Delta/snapshot disks, COWD (ESX
  vmfsSparse) and SESPARSE extents are refused.
- qcow2 and VMDK are opened as top-level files only, not inside a squashfs.
- tar: plain, gzip or multi-block xz (`xz -T0`); the first mount reads every
  header once (the whole stream for .gz/.xz) and saves the entry list to
  `<archive>.tidx` (or the user cache dir). GNU sparse entries are skipped; v7
  tar without the `ustar` magic, tar.zst and tar.bz2 are not recognised.
- 7z: solid or not, every coder the bodgit/sevenzip library has (LZMA/LZMA2, BCJ,
  PPMd, Deflate, bzip2, zstd, ...). Password-protected, multi-volume (`.7z.001`)
  and self-extracting archives are not supported. Reading a file deep inside a
  solid block decodes the block from its start (the bytes are kept in the cache).
- Archives are opened as top-level files only, not inside a squashfs.
- EROFS (read side ported from erofs-utils 1.9.4): uncompressed, chunk-based and
  compressed files (LZ4, LZMA, DEFLATE, zstd; full or compact indexes, big pclusters,
  ztailpacking, fragments, dedupe, 48-bit extents as written by erofs-dedup), any
  directory depth, hard links. Refused with an error: multi-device images, metabox
  (compressed metadata), LZ4 images from mkfs.erofs before 2020 (no 0padding), unknown
  feature bits. Extended attributes are ignored. Top-level file only. `--image` takes a
  path inside the erofs (`dir/disk.img`).
- FAT12/16: not supported (skipped with a log line).
- Symlinks (and devices, fifos) appear as empty files; NTFS reparse points
  appear as their plain file/dir. NTFS metafiles (`$MFT`, ...) are hidden.
- FAT/exFAT volume roots have no directory entry and show 1970; FAT access times are date-only.
- btrfs shows the top-level subvolume, not the `set-default` one; single-device only.
- Background read-ahead (`--readahead`, default 16 MiB, up to one decode per CPU core) runs only where blocks decompress
  independently: disk images inside squashfs, .tib, EROFS; never as extra raw disk reads.
- NTFS-compressed (LZNT1) and sparse files are read per compression unit from the run list;
  NTFS reads run in parallel. ext4 serializes reads per open file.

`third_party/` holds patched forks (changes are marked `backup-mounter` in the source):
`btrfs` (read-only range reader, subvolume crossing, zstd padding fix),
`exfat` (multi-cluster NoFatChain directories listed in full, timestamps, opens no
longer allocate file-sized buffers, bitmap cycle check), `squashfs` (1 MiB uncompressed
block size mask fix, LZ4, block lists read on demand instead of all at open),
`fat32` (timestamps, bitmap cycle check),
`flate` (Go's inflate with resumable checkpoints) and `sevenzip` (folder streams exported, `export.go`).
