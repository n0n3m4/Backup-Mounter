package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// EROFS, read side ported from erofs-utils 1.9.4 lib/{super,namei,dir,data}.c (this file) and
// lib/{zmap,decompress}.c (erofs_z.go). Every on-disk layout of a single-device image is read:
// flat (plain/inline), chunk-based, compressed with full or compact lcluster indexes, 48-bit
// encoded extents, big pclusters, ztailpacking, fragments, dedupe; LZ4, MicroLZMA, DEFLATE, zstd.
//
//	file -> raw chunkCache (1/8: superblock, inodes, dirents, indexes, plain data)
//	     -> erofsInode.ReadAt -> pcl cache (7/8, one decoded pcluster per entry, keyed by its physical offset)
//	     -> arcFS (one "erofs" volume) | openVolumes per --image path

var erofsMagic = []byte{0xE2, 0xE1, 0xF5, 0xE0} // at 1024

func isErofs(f io.ReaderAt) bool {
	var b [4]byte
	_, err := f.ReadAt(b[:], 1024)
	return err == nil && bytes.Equal(b[:], erofsMagic)
}

const (
	erofsLayoutFlatPlain = iota
	erofsLayoutCompressedFull
	erofsLayoutFlatInline
	erofsLayoutCompressedCompact
	erofsLayoutChunkBased
)

const erofsNullAddr = ^uint64(0)

type erofsVol struct {
	path     string
	raw, pcl *chunkCache
	blkbits  uint
	blksz    int64
	meta     int64 // meta_blkaddr << blkbits
	epoch    int64
	nsec     uint32 // fixed_nsec of compact inodes
	incompat uint32
	algs     uint16 // available compression algorithms (bit per Z_EROFS_COMPRESSION_*)
	lzmaDict uint32
	packed   *erofsInode // nil when packed_nid == 0
}

type erofsInode struct {
	v                *erofsVol
	nid              uint64
	iloc             int64
	isize, xattrSize int64
	layout           uint8
	mode             uint16
	size             int64
	mtime            time.Time
	blkaddr          uint64 // flat: start block, erofsNullAddr = all zeros
	chunkFormat      uint16
	chunkBits        uint

	z       zState       // compressed layouts, filled lazily (erofs_z.go)
	aheadTo atomic.Int64 // readAhead high-water mark
}

func (v *erofsVol) read(off int64, n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := v.raw.ReadAt(b, off); err != nil {
		return nil, fmt.Errorf("%s: erofs: read %d bytes at %d: %w", v.path, n, off, err)
	}
	return b, nil
}

func (v *erofsVol) blkoff(x int64) int64 { return x & (v.blksz - 1) }

func roundUp(x, a int64) int64 { return (x + a - 1) / a * a }

func openErofs(path string, f *os.File, st os.FileInfo, budget int64, images []string) ([]volume, error) {
	le := binary.LittleEndian
	base := filepath.Base(path)
	v := &erofsVol{path: path, raw: newChunkCache(f, st.Size(), 4<<20, budget/8), pcl: newCache(budget / 8 * 7)}
	errf := func(format string, a ...any) error {
		return fmt.Errorf("%s: erofs: "+format, append([]any{path}, a...)...)
	}

	// erofs_read_superblock
	head, err := v.read(0, 1024+128)
	if err != nil {
		return nil, err
	}
	sb := head[1024:]
	v.blkbits = uint(sb[12])
	if v.blkbits < 9 || v.blkbits > 16 {
		return nil, errf("block size 2^%d not supported", v.blkbits)
	}
	v.blksz = 1 << v.blkbits
	v.incompat = le.Uint32(sb[80:])
	if bad := v.incompat &^ 0x1ff; bad != 0 {
		return nil, errf("unsupported incompatible feature %#x", bad)
	}
	if v.incompat&0x100 != 0 {
		return nil, errf("metabox (compressed metadata) not supported")
	}
	sbSize := 128 + int64(sb[13])*16
	csumLen := v.blksz // crc32c over [1024, 1024+csumLen)
	if csumLen > 1024 {
		csumLen -= 1024
	}
	if b, err := v.read(0, int(1024+max(sbSize, csumLen))); err != nil {
		return nil, err
	} else {
		sb = b[1024:]
	}
	if le.Uint32(sb[8:])&1 != 0 { // SB_CHKSUM
		c := bytes.Clone(sb[:csumLen])
		sum := le.Uint32(c[4:])
		le.PutUint32(c[4:], 0)
		if ^crc32.Checksum(c, crc32.MakeTable(crc32.Castagnoli)) != sum {
			return nil, errf("superblock checksum mismatch")
		}
	}
	v.meta = int64(le.Uint32(sb[40:])) << v.blkbits
	rootNid := uint64(le.Uint16(sb[14:]))
	if v.incompat&0x80 != 0 && le.Uint64(sb[112:]) != 0 { // 48BIT
		rootNid = le.Uint64(sb[112:])
	}
	packedNid := le.Uint64(sb[96:])
	if packedNid>>63 != 0 {
		return nil, errf("corrupt packed nid")
	}
	v.epoch = int64(le.Uint64(sb[24:]))
	v.nsec = le.Uint32(sb[32:])
	if v.incompat&0x8 != 0 && le.Uint16(sb[86:]) != 0 {
		return nil, errf("%d extra devices (multi-device image) not supported", le.Uint16(sb[86:]))
	}
	label := string(bytes.TrimRight(sb[64:80], "\x00"))

	// z_erofs_parse_cfgs
	if v.incompat&0x2 == 0 { // legacy: LZ4 only, no config records
		if le.Uint16(sb[84:]) != 0 || v.incompat&1 != 0 {
			v.algs = 1
		}
	} else {
		v.algs = le.Uint16(sb[84:])
		if v.algs&^0xf != 0 {
			return nil, errf("unknown compression algorithms %#x", v.algs&^0xf)
		}
		off := 1024 + sbSize
		for alg := 0; alg < 4; alg++ {
			if v.algs&(1<<alg) == 0 {
				continue
			}
			off = roundUp(off, 4)
			b, err := v.read(off, 2)
			if err != nil {
				return nil, err
			}
			n := int(le.Uint16(b))
			if n == 0 {
				return nil, errf("corrupt compression config")
			}
			if b, err = v.read(off+2, n); err != nil {
				return nil, err
			}
			if alg == 1 && n >= 4 {
				v.lzmaDict = le.Uint32(b)
			}
			off += 2 + int64(n)
		}
	}
	if v.algs&1 != 0 && v.incompat&1 == 0 {
		return nil, errf("LZ4 without 0padding (pre-2020 mkfs.erofs) not supported")
	}

	if packedNid != 0 {
		if v.packed, err = v.readInode(packedNid); err != nil {
			return nil, err
		}
	}
	root, err := v.readInode(rootNid)
	if err != nil {
		return nil, err
	}
	if root.mode&0xf000 != 0x4000 {
		return nil, errf("root is not a directory")
	}

	// ponytail: whole tree in RAM at open (~150 B/entry); lazy backend if a 10M-file image shows up.
	a := newArcFS(root.mtime)
	seen := map[uint64]*erofsInode{rootNid: root}
	var total int64
	entries := 0
	var walk func(dir *erofsInode, name string) error
	walk = func(dir *erofsInode, name string) error {
		return dir.readDir(func(child string, nid uint64) error {
			ino := seen[nid]
			if ino == nil {
				var err error
				if ino, err = v.readInode(nid); err != nil {
					return err
				}
				seen[nid] = ino
				if ino.mode&0xf000 == 0x8000 {
					total += ino.size
				}
			} else if ino.mode&0xf000 == 0x4000 {
				return errf("directory nid %d linked twice", nid)
			}
			p := strings.TrimPrefix(name+"/"+child, "/")
			entries++
			switch ino.mode & 0xf000 {
			case 0x4000:
				a.add(p, newInfo("", 0x4000, 0, ino.mtime, time.Time{}, time.Time{}), nil, 0)
				return walk(ino, p)
			case 0x8000:
				a.add(p, newInfo("", 0x8000, ino.size, ino.mtime, time.Time{}, time.Time{}), ino, 0)
				// ponytail: files under 1 MiB are not probed (one pcluster decode each; a rootfs has 50k of them).
				if ino.size >= 1<<20 && isDiskImage(ino, ino.size) {
					log.Printf("%s: disk image %s (%d MiB), mountable with --image", base, p, ino.size>>20)
				}
			default: // symlink, device, fifo, socket: 0-byte file
				a.add(p, newInfo("", 0, 0, ino.mtime, time.Time{}, time.Time{}), nil, 0)
			}
			return nil
		})
	}
	if err := walk(root, ""); err != nil {
		return nil, err
	}
	log.Printf("%s: erofs, %d entries, %s, label %q, %d MiB", base, entries, v.algNames(), label, total>>20)

	if len(images) == 0 {
		if label == "" {
			label = base
		}
		return []volume{{name: base, kind: "erofs", label: label, size: total, fsys: volFS{a}}}, nil
	}
	var vols []volume
	for _, name := range images {
		n, err := a.lookup(arcName(name))
		if err != nil || n.r == nil {
			return nil, errf("no file %q in erofs", name)
		}
		vols = append(vols, openVolumes(name, io.NewSectionReader(n.r, n.off, n.size), n.size)...)
	}
	return vols, nil
}

func (v *erofsVol) algNames() string {
	var s []string
	for i, n := range []string{"lz4", "lzma", "deflate", "zstd"} {
		if v.algs&(1<<i) != 0 {
			s = append(s, n)
		}
	}
	if s == nil {
		return "uncompressed"
	}
	return strings.Join(s, "+")
}

// readInode is erofs_read_inode_from_disk.
func (v *erofsVol) readInode(nid uint64) (*erofsInode, error) {
	le := binary.LittleEndian
	if nid>>63 != 0 {
		return nil, fmt.Errorf("%s: erofs: nid %d in metabox not supported", v.path, nid)
	}
	n := &erofsInode{v: v, nid: nid, iloc: v.meta + int64(nid)<<5}
	b, err := v.read(n.iloc, 32)
	if err != nil {
		return nil, err
	}
	ifmt := le.Uint16(b)
	if ifmt&^0x1f != 0 {
		return nil, fmt.Errorf("%s: erofs: unsupported i_format %#x of nid %d", v.path, ifmt, nid)
	}
	n.layout = uint8(ifmt >> 1 & 7)
	if n.layout > erofsLayoutChunkBased {
		return nil, fmt.Errorf("%s: erofs: unsupported data layout %d of nid %d", v.path, n.layout, nid)
	}
	if icount := int64(le.Uint16(b[2:])); icount != 0 {
		n.xattrSize = 12 + 4*(icount-1)
	}
	n.mode = le.Uint16(b[4:])
	iu, nb := le.Uint32(b[16:]), uint64(le.Uint16(b[6:]))
	if ifmt&1 != 0 { // extended, 64 bytes (may straddle a block: our ReadAt is linear)
		n.isize = 64
		if b, err = v.read(n.iloc, 64); err != nil {
			return nil, err
		}
		n.size = int64(le.Uint64(b[8:]))
		n.mtime = time.Unix(int64(le.Uint64(b[32:])), int64(le.Uint32(b[40:])))
	} else {
		n.isize = 32
		if n.mode&0xf000 == 0x4000 || ifmt>>4&1 == 0 { // i_nb is nlink, 32-bit block address
			nb = 0
		}
		n.size = int64(le.Uint32(b[8:]))
		n.mtime = time.Unix(v.epoch+int64(le.Uint32(b[12:])), int64(v.nsec))
	}
	switch n.mode & 0xf000 {
	case 0x4000, 0x8000, 0xa000:
		n.blkaddr = uint64(iu) | nb<<32
		mask := uint64(1)<<48 - 1
		if ifmt&1 == 0 && (n.mode&0xf000 == 0x4000 || ifmt>>4&1 == 0) {
			mask = 1<<32 - 1
		}
		if n.layout == erofsLayoutFlatPlain && (n.blkaddr^erofsNullAddr)&mask == 0 {
			n.blkaddr = erofsNullAddr
		}
	case 0x2000, 0x6000, 0x1000, 0xc000:
	default:
		return nil, fmt.Errorf("%s: erofs: bogus mode %o of nid %d", v.path, n.mode, nid)
	}
	if n.layout == erofsLayoutChunkBased {
		n.chunkFormat = uint16(iu)
		if n.chunkFormat&^0x7f != 0 {
			return nil, fmt.Errorf("%s: erofs: unsupported chunk format %#x of nid %d", v.path, n.chunkFormat, nid)
		}
		n.chunkBits = v.blkbits + uint(n.chunkFormat&0x1f)
	}
	if n.size < 0 {
		return nil, fmt.Errorf("%s: erofs: bogus size of nid %d", v.path, nid)
	}
	return n, nil
}

// readDir is erofs_iterate_dir + traverse_dirents (no fsck checks): fn gets every name but . and ..
func (n *erofsInode) readDir(fn func(name string, nid uint64) error) error {
	le := binary.LittleEndian
	v := n.v
	buf := make([]byte, v.blksz)
	corrupt := func(what string, pos int64) error {
		return fmt.Errorf("%s: erofs: %s in directory nid %d at %d", v.path, what, n.nid, pos)
	}
	for pos := int64(0); pos < n.size; {
		maxsize := int(min(n.size-pos, v.blksz))
		blk := buf[:maxsize]
		if _, err := n.ReadAt(blk, pos); err != nil {
			return err
		}
		if maxsize < 12 {
			return corrupt("short block", pos)
		}
		next := int(le.Uint16(blk[8:]))
		if next < 12 || int64(next) >= v.blksz || next > maxsize {
			return corrupt("invalid de[0].nameoff", pos)
		}
		end := next
		for de := 0; de < end; de += 12 {
			nameoff := int(le.Uint16(blk[de+8:]))
			if nameoff != next {
				return corrupt("bogus dirent nameoff", pos)
			}
			var namelen int
			if de+12 >= end {
				namelen = bytes.IndexByte(blk[nameoff:], 0)
				if namelen < 0 {
					namelen = maxsize - nameoff
				}
			} else if de+22 <= maxsize {
				namelen = int(le.Uint16(blk[de+20:])) - nameoff
			}
			if namelen <= 0 || namelen > 255 || nameoff+namelen > maxsize {
				return corrupt("bogus dirent namelen", pos)
			}
			name := string(blk[nameoff : nameoff+namelen])
			if name != "." && name != ".." {
				if err := fn(name, le.Uint64(blk[de:])); err != nil {
					return err
				}
			}
			next += namelen
		}
		pos += int64(maxsize)
	}
	return nil
}

// zMap is erofs_map_blocks for every layout: [la, la+llen) of the file, stored at pa (plen bytes).
type zMap struct {
	la, llen, pa, plen int64
	mapped, meta       bool
	encoded, fragment  bool
	fullMapped         bool
	partial            bool // PARTIAL_REF: the pcluster decodes to more than llen
	alg                int  // 0..3 Z_EROFS_COMPRESSION_*, 4 shifted, 5 interlaced
	fragOff            int64
}

const (
	zAlgShifted    = 4
	zAlgInterlaced = 5
)

// mapFlat is __erofs_map_blocks (flat and chunk-based layouts); la < size.
func (n *erofsInode) mapFlat(la int64) (zMap, error) {
	v := n.v
	m := zMap{la: la}
	if n.layout != erofsLayoutChunkBased {
		tailpacking := n.layout == erofsLayoutFlatInline
		if !tailpacking && n.blkaddr == erofsNullAddr {
			m.llen = n.size - la
			return m, nil
		}
		nblocks := (n.size + v.blksz - 1) >> v.blkbits
		if tailpacking {
			nblocks--
		}
		pos := nblocks << v.blkbits
		m.mapped = true
		if la < pos {
			m.pa = int64(n.blkaddr)<<v.blkbits + la
			m.llen = pos - la
		} else {
			m.pa = n.iloc + n.isize + n.xattrSize + v.blkoff(la)
			m.llen = n.size - la
			m.meta = true
			if v.blkoff(m.pa)+m.llen > v.blksz {
				return m, fmt.Errorf("%s: erofs: inline data across blocks @ nid %d", v.path, n.nid)
			}
		}
		m.plen = m.llen
		return m, nil
	}
	unit := int64(4) // block map
	if n.chunkFormat&0x20 != 0 {
		unit = 8 // chunk indexes
	}
	chunknr := la >> n.chunkBits
	b, err := v.read(roundUp(n.iloc+n.isize+n.xattrSize, unit)+unit*chunknr, int(unit))
	if err != nil {
		return m, err
	}
	le := binary.LittleEndian
	m.la = chunknr << n.chunkBits
	m.llen = min(int64(1)<<n.chunkBits, roundUp(n.size-m.la, v.blksz))
	if unit == 8 {
		mask := uint64(1)<<32 - 1
		if n.chunkFormat&0x40 != 0 {
			mask = 1<<48 - 1
		}
		startblk := (uint64(le.Uint16(b))<<32 | uint64(le.Uint32(b[4:]))) & mask
		if (startblk^erofsNullAddr)&mask != 0 {
			m.pa, m.mapped = int64(startblk)<<v.blkbits, true
		}
	} else if startblk := le.Uint32(b); startblk != 0xffffffff { // kernel compares the u32 with (u32)NULL_ADDR
		m.pa, m.mapped = int64(startblk)<<v.blkbits, true
	}
	m.plen = m.llen
	return m, nil
}

// ReadAt is erofs_read_raw_data / z_erofs_read_data, forwards, one extent per step.
func (n *erofsInode) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= n.size {
		return 0, io.EOF
	}
	var eof error
	if rest := n.size - off; int64(len(p)) > rest {
		p, eof = p[:rest], io.EOF
	}
	v := n.v
	done := 0
	for done < len(p) {
		pos := off + int64(done)
		var m zMap
		var err error
		if n.layout == erofsLayoutCompressedFull || n.layout == erofsLayoutCompressedCompact {
			m, err = n.zMapBlocks(pos)
		} else {
			m, err = n.mapFlat(pos)
		}
		if err != nil {
			return done, err
		}
		if m.la > pos || m.la+m.llen <= pos {
			return done, fmt.Errorf("%s: erofs: extent [%d,+%d) does not cover %d in nid %d", v.path, m.la, m.llen, pos, n.nid)
		}
		dst := p[done : done+int(min(int64(len(p)-done), m.la+m.llen-pos))]
		skip := pos - m.la
		switch {
		case !m.mapped:
			clear(dst)
		case m.fragment:
			if v.packed == nil || n == v.packed {
				return done, fmt.Errorf("%s: erofs: fragment without packed inode in nid %d", v.path, n.nid)
			}
			if k, err := v.packed.ReadAt(dst, m.fragOff+skip); k < len(dst) {
				return done, fmt.Errorf("%s: erofs: fragment of nid %d: %w", v.path, n.nid, err)
			}
		case !m.encoded:
			if _, err := v.raw.ReadAt(dst, m.pa+skip); err != nil {
				return done, err
			}
		case m.alg >= zAlgShifted:
			if err := n.readPlain(m, dst, skip); err != nil {
				return done, err
			}
		default:
			data, err := v.pcl.fetch(m.pa, func() ([]byte, error) { return v.decode(m) })
			if err != nil {
				return done, err
			}
			if int64(len(data)) < m.llen {
				return done, fmt.Errorf("%s: erofs: pcluster at %d decodes to %d bytes, want %d", v.path, m.pa, len(data), m.llen)
			}
			copy(dst, data[skip:])
			if pos+int64(len(dst)) == m.la+m.llen { // once per extent, when the reader reaches its end
				n.readAhead(m.la + m.llen)
			}
		}
		done += len(dst)
	}
	return done, eof
}

// readAhead keeps the encoded pclusters of the next readAheadBytes (at most 1024 extents per
// trigger) decoding in the background, one goroutine each while prefetchSem has room. aheadTo remembers how far the window
// was mapped, so a trigger at the next extent end only maps and launches what is new; a reader
// that jumps elsewhere resets it. Depth is what buys the cores: a 4-extent look-ahead gave 3x on
// 8 cores, this gives ~8x.
// ponytail: fixed byte window and extent cap; per-stream positions if two readers interleave in one file.
func (n *erofsInode) readAhead(la int64) {
	v := n.v
	end := min(la+readAheadBytes, n.size)
	from := n.aheadTo.Load()
	if from < la || from > end {
		from = la
	}
	for i := 0; i < 1024 && from < end; i++ {
		m, err := n.zMapBlocks(from)
		if err != nil || m.la+m.llen <= from {
			return
		}
		if m.mapped && m.encoded && !m.fragment && m.alg < zAlgShifted && v.pcl.missing(m.pa) {
			select {
			case prefetchSem <- struct{}{}:
			default:
				return // all cores busy: the next extent end tops the window up
			}
			go func() {
				defer func() { <-prefetchSem }()
				v.pcl.fetch(m.pa, func() ([]byte, error) { return v.decode(m) })
			}()
		}
		from = m.la + m.llen
		n.aheadTo.Store(from)
	}
}
