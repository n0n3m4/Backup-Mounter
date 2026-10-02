#!/bin/sh
# Builds test fixtures without root: filesystems are populated by the mkfs
# tools themselves, laid into partitioned disk images with dd, then wrapped in
# squashfs. Usage: testdata/make_fixtures.sh OUTDIR, then
# BM_FIXTURES=OUTDIR go test -tags xz,zstd,lz4 ./...
set -eu
OUT=${1:?usage: make_fixtures.sh OUTDIR}
mkdir -p "$OUT"
cd "$OUT"
rm -rf src gpt mbr ./*.img ./*.squashfs manifest.sha256

# --- source tree (ext4 and btrfs get a copy; manifest lists regular files) ---
mkdir -p src/dir/sub src/empty src/vol1/inner src/dir/vol2
echo hello > src/hello.txt
: > src/zero.bin
printf 'x%.0s' $(seq 1 3000) > src/dir/inline-ish.txt
head -c 3000000 /dev/urandom > src/dir/random.bin
yes "compressible line of text" | head -c 5000000 > src/dir/sub/compressible.txt
echo "unicode" > "src/dir/Ünïcödé file.txt"
head -c 1000000 /dev/urandom > src/vol1/in-subvol.bin
echo inner > src/vol1/inner/deep.txt
echo two > src/dir/vol2/v2.txt
truncate -s 8M src/sparse.bin && printf middle | dd of=src/sparse.bin bs=1 seek=4000000 conv=notrunc 2>/dev/null
ln -s dir/random.bin src/link
(cd src && find . -type f -exec sha256sum {} + | sort -k2) > manifest.sha256

# --- filesystem images ---
mkfs.ext4 -q -F -L ext4vol -d src ext4.img 64M
truncate -s 128M btrfs.img
mkfs.btrfs -q -f -L btrfsvol --compress zstd -u rw:vol1 -u rw:vol1/inner -u rw:dir/vol2 -r src btrfs.img \
  || mkfs.btrfs -q -f -L btrfsvol -u rw:vol1 -u rw:vol1/inner -u rw:dir/vol2 -r src btrfs.img
rm -f fat32.img && mkfs.vfat -F 32 -s 1 -n FATVOL -C fat32.img 65536 >/dev/null # empty: no mtools
truncate -s 64M ntfs.img && mkfs.ntfs -q -F -Q -L ntfsvol ntfs.img >/dev/null 2>&1
ntfscp -q ntfs.img src/hello.txt hello.txt && ntfscp -q ntfs.img src/dir/random.bin random.bin # root files only: ntfscp cannot mkdir

# --- GPT disk: ext4, btrfs, fat32, ntfs at 1, 65, 193, 257 MiB ---
mkdir gpt
truncate -s 322M gpt/disk.img
sgdisk -o -n 1:2048:133119 -t 1:8300 -n 2:133120:395263 -t 2:8300 \
  -n 3:395264:526335 -t 3:0700 -n 4:526336:657407 -t 4:0700 gpt/disk.img >/dev/null
put() { dd if="$1" of="$2" bs=1M seek="$3" conv=notrunc status=none; }
put ext4.img gpt/disk.img 1
put btrfs.img gpt/disk.img 65
put fat32.img gpt/disk.img 193
put ntfs.img gpt/disk.img 257
echo "not a disk" > gpt/README.txt

# --- MBR disk: primary ext4, logical ntfs + fat32 in an extended partition; plus a bare ext4 image ---
mkdir mbr
truncate -s 200M mbr/disk.img
parted -s mbr/disk.img mklabel msdos mkpart primary ext4 1MiB 65MiB mkpart extended 65MiB 199MiB \
  mkpart logical ntfs 66MiB 130MiB mkpart logical fat32 131MiB 195MiB
put ext4.img mbr/disk.img 1
put ntfs.img mbr/disk.img 66
put fat32.img mbr/disk.img 131
cp ext4.img mbr/bare-ext4.img

mksquashfs gpt gpt-zstd.squashfs -comp zstd -b 1M -noappend -quiet -no-progress
mksquashfs gpt gpt-lz4.squashfs -comp lz4 -Xhc -b 1M -noappend -quiet -no-progress
mksquashfs mbr mbr-gzip.squashfs -comp gzip -b 1M -noappend -quiet -no-progress
rm -rf gpt mbr ext4.img btrfs.img fat32.img ntfs.img
ls -l "$OUT"
