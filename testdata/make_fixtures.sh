#!/bin/sh
# Builds test fixtures without root: filesystems are populated by the mkfs
# tools themselves, laid into partitioned disk images with dd, then wrapped in
# squashfs; VHD/VHDX, qcow2 and VMDK copies via qemu-img. Usage: testdata/make_fixtures.sh OUTDIR, then
# BM_FIXTURES=OUTDIR go test -tags xz,zstd,lz4 ./...
set -eu
OUT=${1:?usage: make_fixtures.sh OUTDIR}
mkdir -p "$OUT"
ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$OUT"
EROFS_DEDUP=${EROFS_DEDUP:-erofs-dedup} # ~/Backup-EROFS/erofs-dedup, not on PATH
populate() { go -C "$ROOT" run ./testdata/populate "$1" "$PWD/$2" "$PWD/src"; } # FAT kinds: no mtools, so the readers' own libraries write
fusermount -u ntfsmnt 2>/dev/null || true # left mounted by an aborted run
rm -rf ntfsmnt manifest-ntfs.sha256 src tarsrc gpt mbr vhd vmdk erofs ./*.img ./*.vhd ./*.vhdx ./*.qcow2 ./*.squashfs ./*.tar* ./*.tidx ./*.7z ./*.erofs ./*.erofs.idx manifest.sha256 manifest-tar.sha256 manifest-erofs.sha256

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

# --- archives: tar (plain, no dir entries, gz, multi-block xz) and 7z (solid, non-solid, encrypted) of a copy with a hard link and a >100-char path ---
cp -a src tarsrc && ln tarsrc/hello.txt tarsrc/hello-hardlink.txt                   # tar stores the second name as a hard link
L=$(printf 'l%.0s' $(seq 1 120)); mkdir "tarsrc/$L" && echo long > "tarsrc/$L/$L.txt" # > 100 chars: PAX path records
(cd tarsrc && find . -type f -exec sha256sum {} + | sort -k2) > manifest-tar.sha256
tar --format=posix -cf tarsrc.tar -C tarsrc .
(cd tarsrc && find . \( -type f -o -type l -o \( -type d -empty \) \) | tar --format=posix --no-recursion -cf ../tarsrc-nodirs.tar -T -) # parents synthesised
gzip -k tarsrc.tar
xz -k -0 -T2 --block-size=1MiB tarsrc.tar                         # multi-block
(cd tarsrc && 7z a -snl -mtc=on -mta=on -bso0 ../tarsrc.7z .)     # solid LZMA2; symlink kept as a link; ctime/atime stored
(cd tarsrc && 7z a -snl -ms=off -bso0 ../tarsrc-ns.7z .)          # one folder per file
(cd tarsrc && 7z a -snl -phunter2 -mhe=on -bso0 ../tarsrc-enc.7z .) # encrypted headers: must be refused

# --- filesystem images ---
mkfs.ext4 -q -F -L ext4vol -d src ext4.img 64M
truncate -s 128M btrfs.img
mkfs.btrfs -q -f -L btrfsvol --compress zstd -u rw:vol1 -u rw:vol1/inner -u rw:dir/vol2 -r src btrfs.img \
  || mkfs.btrfs -q -f -L btrfsvol -u rw:vol1 -u rw:vol1/inner -u rw:dir/vol2 -r src btrfs.img
rm -f fat32.img && mkfs.vfat -F 32 -s 1 -n FATVOL -C fat32.img 65536 >/dev/null && populate -kind=fat32 fat32.img
truncate -s 64M exfat.img && mkfs.exfat -L EXFATVOL exfat.img >/dev/null && populate -kind=exfat exfat.img
truncate -s 64M ntfs.img && mkfs.ntfs -q -F -Q -L ntfsvol ntfs.img >/dev/null 2>&1
ntfscp -q ntfs.img src/hello.txt hello.txt && ntfscp -q ntfs.img src/dir/random.bin random.bin # root files only: ntfscp cannot mkdir
# NTFS compression (LZNT1 units, raw units, sparse units, partial tail) + a sparse file, through ntfs-3g FUSE; skipped without it
if command -v ntfs-3g >/dev/null && mkdir -p ntfsmnt && ntfs-3g -o compression ntfs.img ntfsmnt 2>/dev/null; then
  mkdir ntfsmnt/comp
  python3 -c "import os,struct; os.setxattr('ntfsmnt/comp','system.ntfs_attrib',struct.pack('<I',0x810),follow_symlinks=False)" # DIRECTORY|COMPRESSED: new files inherit it (0x820)
  seq 1 3000000 > ntfsmnt/comp/seq.txt                # every unit LZNT1
  head -c 1M /dev/urandom > ntfsmnt/comp/random.bin   # incompressible: units stored raw
  (for i in $(seq 1 8); do head -c 65536 /dev/zero; head -c 65536 /dev/urandom; head -c 20000 /dev/zero; done) > ntfsmnt/comp/mixed.bin # sparse units merged across neighbours (old go-ntfs bug)
  truncate -s 8M ntfsmnt/sparse.bin && printf middle | dd of=ntfsmnt/sparse.bin bs=1 seek=4000000 conv=notrunc 2>/dev/null # plain sparse, not compressed
  (cd ntfsmnt && find . -type f -exec sha256sum {} + | sort -k2) > manifest-ntfs.sha256
  fusermount -u ntfsmnt
fi
rmdir ntfsmnt 2>/dev/null || true

# --- GPT disk: ext4, btrfs, fat32, ntfs, exfat at 1, 65, 193, 257, 321 MiB ---
mkdir gpt
truncate -s 386M gpt/disk.img
sgdisk -o -n 1:2048:133119 -t 1:8300 -n 2:133120:395263 -t 2:8300 \
  -n 3:395264:526335 -t 3:0700 -n 4:526336:657407 -t 4:0700 \
  -n 5:657408:788479 -t 5:0700 gpt/disk.img >/dev/null
put() { dd if="$1" of="$2" bs=1M seek="$3" conv=notrunc status=none; }
put ext4.img gpt/disk.img 1
put btrfs.img gpt/disk.img 65
put fat32.img gpt/disk.img 193
put ntfs.img gpt/disk.img 257
put exfat.img gpt/disk.img 321
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

# --- VHD/VHDX: top-level copies of the GPT disk, plus a squashfs holding one of each ---
qemu-img convert -q -f raw -O vpc -o subformat=dynamic,force_size=on gpt/disk.img gpt-dyn.vhd
qemu-img convert -q -f raw -O vpc -o subformat=fixed,force_size=on gpt/disk.img gpt-fixed.vhd
qemu-img convert -q -f raw -O vhdx -o subformat=dynamic gpt/disk.img gpt.vhdx
mkdir vhd
cp gpt.vhdx vhd/
qemu-img convert -q -f raw -O vpc -o subformat=dynamic,force_size=on mbr/disk.img vhd/mbr-dyn.vhd

# --- qcow2 (plain, zlib, zstd; an overlay with a backing file must be refused) and every VMDK subformat ---
qemu-img convert -q -f raw -O qcow2 gpt/disk.img gpt.qcow2
qemu-img convert -q -f raw -O qcow2 -c gpt/disk.img gpt-zlib.qcow2
qemu-img convert -q -f raw -O qcow2 -c -o compression_type=zstd gpt/disk.img gpt-zstd.qcow2
qemu-img create -q -f qcow2 -b gpt.qcow2 -F qcow2 overlay.qcow2
mkdir vmdk
qemu-img convert -q -f raw -O vmdk -o subformat=monolithicSparse gpt/disk.img vmdk/sparse.vmdk
qemu-img convert -q -f raw -O vmdk -o subformat=streamOptimized gpt/disk.img vmdk/stream.vmdk
qemu-img convert -q -f raw -O vmdk -o subformat=monolithicFlat mbr/disk.img vmdk/flat.vmdk           # + flat-flat.vmdk
qemu-img convert -q -f raw -O vmdk -o subformat=twoGbMaxExtentSparse mbr/disk.img vmdk/split-s.vmdk # + split-s-s001.vmdk
qemu-img convert -q -f raw -O vmdk -o subformat=twoGbMaxExtentFlat mbr/disk.img vmdk/split-f.vmdk   # + split-f-f001.vmdk

mksquashfs gpt gpt-zstd.squashfs -comp zstd -b 1M -noappend -quiet -no-progress
mksquashfs gpt gpt-lz4.squashfs -comp lz4 -Xhc -b 1M -noappend -quiet -no-progress
mksquashfs mbr mbr-gzip.squashfs -comp gzip -b 1M -noappend -quiet -no-progress
mksquashfs mbr mbr-4k.squashfs -comp zstd -b 4K -noappend -quiet -no-progress  # 51200 block entries over ~25 metadata blocks: lazy block index
mksquashfs vhd vhd-zstd.squashfs -comp zstd -b 1M -noappend -quiet -no-progress
# after mbr-gzip.squashfs, so the squashfs does not contain these (the rm at the top removes mbr/)
gzip -k mbr/disk.img                                         # one member, indexed on first open (disk.img.gz.idx)
xz -k -0 -T2 --block-size=8MiB mbr/disk.img                  # 25 blocks: random access via the xz index
xz -0 -T1 -c mbr/bare-ext4.img > mbr/single.img.xz           # one block (-T1: xz >= 5.6 is multi-threaded, hence multi-block, by default): must be rejected
# --- EROFS: erofs-dedup (holes, raw, MicroLZMA; a second copy fully deduplicated; an empty file) ---
"$EROFS_DEDUP" create dedup.erofs mbr/disk.img mbr/bare-ext4.img 2>/dev/null
"$EROFS_DEDUP" add dedup.erofs -name disk-copy.img mbr/disk.img 2>/dev/null
: > empty.bin && "$EROFS_DEDUP" add dedup.erofs empty.bin 2>/dev/null && rm empty.bin
(sha256sum mbr/disk.img mbr/bare-ext4.img | sed 's#  mbr/#  #'; echo "$(sha256sum < mbr/disk.img | cut -d' ' -f1)  disk-copy.img"; echo "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  empty.bin") > manifest-erofs.sha256
# stock mkfs.erofs images of src, one per on-disk layout / feature (mkfs 1.9 supports only -b 4096)
mkdir erofs
mkfs.erofs                                         erofs/none.erofs       src >/dev/null # uncompressed, inline tails
mkfs.erofs --chunksize=16384                       erofs/chunk.erofs      src >/dev/null # CHUNK_BASED
mkfs.erofs -zlz4                                   erofs/lz4.erofs        src >/dev/null # legacy LZ4, no compr_cfgs
mkfs.erofs -zlz4hc -Eforce-inode-compact           erofs/compact.erofs    src >/dev/null
mkfs.erofs -zlz4hc -Eforce-inode-extended          erofs/extended.erofs   src >/dev/null
mkfs.erofs -zlz4hc -C65536                         erofs/lz4-bigp.erofs   src >/dev/null # big pcluster
mkfs.erofs -zlz4hc -Efragments,dedupe,ztailpacking erofs/lz4-frag.erofs   src >/dev/null # packed inode, PARTIAL_REF, inline pcluster
mkfs.erofs -zlz4hc -Ededupe,fragments              erofs/dedupe.erofs     src >/dev/null # dedupe needs fragments: mkfs 1.9.1 -Ededupe alone writes a 4 KiB image fsck rejects
mkfs.erofs -zlzma                                  erofs/lzma.erofs       src >/dev/null
mkfs.erofs -zlzma -C32768 -Efragments              erofs/lzma-frag.erofs  src >/dev/null # not +ztailpacking: mkfs 1.9.1 writes the packed tail shifted but flags it interlaced (erofsfuse reads garbage too)
mkfs.erofs -zdeflate                               erofs/deflate.erofs    src >/dev/null
mkfs.erofs -zdeflate -Ededupe,fragments,ztailpacking erofs/deflate-dd.erofs src >/dev/null
mkfs.erofs -zzstd                                  erofs/zstd.erofs       src >/dev/null
mkfs.erofs -zzstd -C16384 -Eall-fragments          erofs/zstd-16k.erofs   src >/dev/null # whole files in the packed inode
rm -rf gpt vhd ext4.img btrfs.img fat32.img ntfs.img exfat.img # mbr/ stays: raw disk + bare partition, tested as top-level files
ls -l "$OUT"
