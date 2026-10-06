package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/flate"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/unxed/xz/lzma"
)

// Compressed EROFS inodes: a port of erofs-utils 1.9.4 lib/zmap.c (lcluster indexes, 48-bit
// extents) and lib/decompress.c. Function names follow the C ones; control flow is kept so the two
// can be read side by side. Differences: the map recorder holds a copy of the index pack instead of
// a metabuf pointer, map_blocks_ext returns the fragment offset in the map instead of storing it in
// the inode (goroutine safety), and every map gets the FIEMAP treatment (full decoded extent), so a
// pcluster is always decoded whole and cached under its physical offset.

const (
	zAdviseCompacted2B  = 0x0001 // compact layout
	zAdviseExtents      = 0x0001 // full layout
	zAdviseBigPcluster1 = 0x0002
	zAdviseBigPcluster2 = 0x0004
	zAdviseInline       = 0x0008
	zAdviseInterlaced   = 0x0010
	zAdviseFragment     = 0x0020

	zTypePlain   = 0
	zTypeHead1   = 1
	zTypeNonhead = 2
	zTypeHead2   = 3
	zTypeMax     = 4

	zLiPartialRef = 1 << 15
	zLiD0Cblkcnt  = 1 << 11

	zPclusterMaxSize  = 1 << 20
	zPclusterMaxDsize = 12 << 20

	zPlenPartial = 1 << 27
	zPlenFmtBit  = 28
	zPlenMask    = zPclusterMaxSize<<1 - 1

	zFindTail = 0x8
	zFiemap   = 0x2
)

// zState is the z_erofs part of struct erofs_inode, set once by fillInodeLazy.
type zState struct {
	once         sync.Once
	err          error
	advise       uint16
	alg          [2]int
	lclusterBits uint
	idataSize    int64
	fragmentOff  int64 // z_fragmentoff / fragmentoff (a union in C)
	tailHeadLcn  int64 // z_tailextent_headlcn
	extents      int64
}

// zRecorder is struct z_erofs_maprecorder.
type zRecorder struct {
	n                  *erofsInode
	m                  *zMap
	lcn                int64
	typ, headtype      int
	clusterofs         int64
	delta              [2]int64
	pblk, compressedbl int64
	nextpackoff        int64
	partialref         bool
}

func (n *erofsInode) zCorrupt(format string, a ...any) error {
	return fmt.Errorf("%s: erofs: nid %d: "+format, append([]any{n.v.path, n.nid}, a...)...)
}

// zMapBlocks is z_erofs_map_blocks_iter with EROFS_GET_BLOCKS_FIEMAP; la < size.
func (n *erofsInode) zMapBlocks(la int64) (zMap, error) {
	z := &n.z
	z.once.Do(func() { z.err = n.fillInodeLazy() })
	if z.err != nil {
		return zMap{}, z.err
	}
	m := zMap{la: la}
	var err error
	if n.layout == erofsLayoutCompressedFull && z.advise&zAdviseExtents != 0 {
		err = n.mapExt(&m)
	} else {
		err = n.mapFo(&m, zFiemap)
	}
	if err == nil {
		err = n.mapSanityCheck(&m)
	}
	return m, err
}

// loadFull is z_erofs_load_full_lcluster.
func (r *zRecorder) loadFull(lcn int64) error {
	n := r.n
	pos := roundUp(n.iloc+n.isize+n.xattrSize, 8) + 8 + 8 + lcn*8 // Z_EROFS_FULL_INDEX_START
	di, err := n.v.read(pos, 8)
	if err != nil {
		return err
	}
	le := binary.LittleEndian
	r.lcn = lcn
	r.nextpackoff = pos + 8
	advise := le.Uint16(di)
	r.typ = int(advise & (zTypeMax - 1))
	if r.typ == zTypeNonhead {
		r.clusterofs = 1 << n.z.lclusterBits
		r.delta[0] = int64(le.Uint16(di[4:]))
		if r.delta[0]&zLiD0Cblkcnt != 0 {
			if n.z.advise&(zAdviseBigPcluster1|zAdviseBigPcluster2) == 0 {
				return n.zCorrupt("CBLKCNT without big pcluster")
			}
			r.compressedbl = r.delta[0] &^ zLiD0Cblkcnt
			r.delta[0] = 1
		}
		r.delta[1] = int64(le.Uint16(di[6:]))
	} else {
		r.partialref = advise&zLiPartialRef != 0
		r.clusterofs = int64(le.Uint16(di[2:]))
		r.pblk = int64(le.Uint32(di[4:]))
	}
	return nil
}

// decodeCompactedBits is decode_compactedbits: in is the whole index pack.
func decodeCompactedBits(lobits uint, in []byte, pos uint) (lo int64, typ int) {
	v := binary.LittleEndian.Uint32(in[pos/8:]) >> (pos & 7)
	return int64(v & (1<<lobits - 1)), int(v>>lobits) & 3
}

// getCompactedLaDistance is get_compacted_la_distance.
func getCompactedLaDistance(lobits, encodebits uint, vcnt int, in []byte, i int) int64 {
	var lo, d1 int64
	var typ int
	for {
		lo, typ = decodeCompactedBits(lobits, in, encodebits*uint(i))
		if typ != zTypeNonhead {
			return d1
		}
		d1++
		if i++; i >= vcnt {
			break
		}
	}
	// vcnt - 1 (NONHEAD) item
	if lo&zLiD0Cblkcnt == 0 {
		d1 += lo - 1
	}
	return d1
}

// loadCompact is z_erofs_load_compact_lcluster.
func (r *zRecorder) loadCompact(lcn int64, lookahead bool) error {
	n := r.n
	v := n.v
	ebase := roundUp(n.iloc+n.isize+n.xattrSize, 8) + 8
	lclusterBits := n.z.lclusterBits
	totalidx := (n.size + v.blksz - 1) >> v.blkbits
	bigPcluster := n.z.advise&zAdviseBigPcluster1 != 0

	if lcn >= totalidx || lclusterBits > 14 {
		return n.zCorrupt("lcn %d out of range (or lclusterbits %d)", lcn, lclusterBits)
	}
	r.lcn = lcn
	// used to align to 32-byte (compacted_2b) alignment
	compacted4bInitial := ((32 - ebase%32) / 4) & 7
	compacted2b := int64(0)
	if n.z.advise&zAdviseCompacted2B != 0 && compacted4bInitial < totalidx {
		compacted2b = (totalidx - compacted4bInitial) / 16 * 16
	}
	pos := ebase
	amortizedshift := uint(2) // compact_4b
	if lcn >= compacted4bInitial {
		pos += compacted4bInitial * 4
		lcn -= compacted4bInitial
		if lcn < compacted2b {
			amortizedshift = 1
		} else {
			pos += compacted2b * 2
			lcn -= compacted2b
		}
	}
	pos += lcn << amortizedshift

	// figure out the lcluster count in this pack
	var vcnt int
	if amortizedshift == 2 && lclusterBits <= 14 {
		vcnt = 2
	} else if amortizedshift == 1 && lclusterBits <= 12 {
		vcnt = 16
	} else {
		return n.zCorrupt("unsupported compact index (lclusterbits %d)", lclusterBits)
	}
	packsize := int64(vcnt) << amortizedshift
	in, err := v.read(pos/packsize*packsize, int(packsize))
	if err != nil {
		return err
	}
	// it doesn't equal to round_up(..)
	r.nextpackoff = pos/packsize*packsize + packsize
	lobits := max(lclusterBits, 12) // ilog2(Z_EROFS_LI_D0_CBLKCNT) + 1
	vcntLog := uint(1)
	if vcnt == 16 {
		vcntLog = 4
	}
	encodebits := uint((packsize-4)*8) >> vcntLog
	bytesIn := pos & (packsize - 1)
	i := int(bytesIn >> amortizedshift)

	lo, typ := decodeCompactedBits(lobits, in, encodebits*uint(i))
	r.typ = typ
	if typ == zTypeNonhead {
		r.clusterofs = 1 << lclusterBits
		// figure out lookahead_distance: delta[1] if needed
		if lookahead {
			r.delta[1] = getCompactedLaDistance(lobits, encodebits, vcnt, in, i)
		}
		if lo&zLiD0Cblkcnt != 0 {
			if !bigPcluster {
				return n.zCorrupt("CBLKCNT without big pcluster")
			}
			r.compressedbl = lo &^ zLiD0Cblkcnt
			r.delta[0] = 1
			return nil
		} else if i+1 != vcnt {
			r.delta[0] = lo
			return nil
		}
		// since the last lcluster in the pack is special, of which lo saves delta[1] rather
		// than delta[0]. Hence, get delta[0] by the previous lcluster indirectly.
		lo, typ = decodeCompactedBits(lobits, in, encodebits*uint(i-1))
		if typ != zTypeNonhead {
			lo = 0
		} else if lo&zLiD0Cblkcnt != 0 {
			lo = 1
		}
		r.delta[0] = lo + 1
		return nil
	}
	r.clusterofs = lo
	r.delta[0] = 0
	// figure out blkaddr (pblk) for HEAD lclusters
	var nblk int64
	if !bigPcluster {
		nblk = 1
		for i > 0 {
			i--
			lo, typ = decodeCompactedBits(lobits, in, encodebits*uint(i))
			if typ == zTypeNonhead {
				i -= int(lo)
			}
			if i >= 0 {
				nblk++
			}
		}
	} else {
		for i > 0 {
			i--
			lo, typ = decodeCompactedBits(lobits, in, encodebits*uint(i))
			if typ == zTypeNonhead {
				if lo&zLiD0Cblkcnt != 0 {
					i--
					nblk += lo &^ zLiD0Cblkcnt
					continue
				}
				// bigpcluster shouldn't have plain d0 == 1
				if lo <= 1 {
					return n.zCorrupt("plain d0 %d in big pcluster", lo)
				}
				i -= int(lo) - 2
				continue
			}
			nblk++
		}
	}
	r.pblk = int64(binary.LittleEndian.Uint32(in[packsize-4:])) + nblk
	return nil
}

// loadLcluster is z_erofs_load_lcluster_from_disk.
func (r *zRecorder) loadLcluster(lcn int64, lookahead bool) error {
	n := r.n
	var err error
	if n.layout == erofsLayoutCompressedCompact {
		err = r.loadCompact(lcn, lookahead)
	} else {
		err = r.loadFull(lcn)
	}
	if err != nil {
		return err
	}
	if r.typ >= zTypeMax {
		return n.zCorrupt("unknown type %d @ lcn %d", r.typ, lcn)
	} else if r.typ != zTypeNonhead && r.clusterofs >= 1<<n.z.lclusterBits {
		return n.zCorrupt("clusterofs %d @ lcn %d", r.clusterofs, lcn)
	}
	return nil
}

// lookback is z_erofs_extent_lookback.
func (r *zRecorder) lookback(lookbackDistance int64) error {
	n := r.n
	lclusterBits := n.z.lclusterBits
	for r.lcn >= lookbackDistance {
		lcn := r.lcn - lookbackDistance
		if err := r.loadLcluster(lcn, false); err != nil {
			return err
		}
		if r.typ == zTypeNonhead {
			lookbackDistance = r.delta[0]
			if lookbackDistance == 0 {
				break
			}
			continue
		}
		r.headtype = r.typ
		r.m.la = lcn<<lclusterBits | r.clusterofs
		return nil
	}
	return n.zCorrupt("bogus lookback distance %d @ lcn %d", lookbackDistance, r.lcn)
}

// compressedLen is z_erofs_get_extent_compressedlen.
func (r *zRecorder) compressedLen(initialLcn int64) error {
	n := r.n
	bigpcl1 := n.z.advise&zAdviseBigPcluster1 != 0
	bigpcl2 := n.z.advise&zAdviseBigPcluster2 != 0
	lcn := r.lcn + 1

	if (r.headtype == zTypeHead1 && !bigpcl1) ||
		((r.headtype == zTypePlain || r.headtype == zTypeHead2) && !bigpcl2) ||
		lcn<<n.z.lclusterBits >= n.size {
		r.compressedbl = 1
	}
	if r.compressedbl == 0 {
		if err := r.loadLcluster(lcn, false); err != nil {
			return err
		}
		if r.typ == zTypeNonhead {
			if r.delta[0] != 1 {
				return n.zCorrupt("bogus CBLKCNT @ lcn %d", lcn)
			}
			if r.compressedbl == 0 {
				return n.zCorrupt("cannot find CBLKCNT @ lcn %d", lcn)
			}
		} else {
			// if the 1st NONHEAD lcluster is actually PLAIN or HEAD type rather than
			// CBLKCNT, it's a 1 block-sized pcluster.
			r.compressedbl = 1
		}
	}
	r.m.plen = r.compressedbl << n.v.blkbits
	return nil
}

// decompressedLen is z_erofs_get_extent_decompressedlen.
func (r *zRecorder) decompressedLen() error {
	n := r.n
	m := r.m
	lclusterBits := n.z.lclusterBits
	lcn, headlcn := r.lcn, m.la>>lclusterBits
	for {
		// handle the last EOF pcluster (no next HEAD lcluster)
		if lcn<<lclusterBits >= n.size {
			m.llen = n.size - m.la
			return nil
		}
		if err := r.loadLcluster(lcn, true); err != nil {
			return err
		}
		if r.typ == zTypeNonhead {
			// work around invalid d1 generated by pre-1.0 mkfs
			if r.delta[1] == 0 {
				r.delta[1] = 1
			}
		} else {
			if lcn != headlcn {
				break // ends at the next HEAD lcluster
			}
			r.delta[1] = 1
		}
		lcn += r.delta[1]
	}
	m.llen = lcn<<lclusterBits + r.clusterofs - m.la
	return nil
}

// mapFo is z_erofs_map_blocks_fo.
func (n *erofsInode) mapFo(m *zMap, flags int) error {
	v := n.v
	z := &n.z
	fragment := z.advise&zAdviseFragment != 0
	ztailpacking := z.idataSize != 0
	lclusterBits := z.lclusterBits
	r := &zRecorder{n: n, m: m}

	ofs := m.la
	if flags&zFindTail != 0 {
		ofs = n.size - 1
	}
	if fragment && flags&zFindTail == 0 && z.tailHeadLcn == 0 {
		m.la, m.llen = 0, n.size
		m.mapped, m.fragment, m.fragOff = true, true, z.fragmentOff
		return nil
	}
	initialLcn := ofs >> lclusterBits
	endoff := ofs & (1<<lclusterBits - 1)

	if err := r.loadLcluster(initialLcn, false); err != nil {
		return err
	}
	if flags&zFindTail != 0 && ztailpacking {
		z.fragmentOff = r.nextpackoff
	}
	m.mapped, m.encoded = true, true
	end := (r.lcn + 1) << lclusterBits

	switch r.typ {
	case zTypePlain, zTypeHead1, zTypeHead2:
		if endoff >= r.clusterofs {
			r.headtype = r.typ
			m.la = r.lcn<<lclusterBits | r.clusterofs
			// For ztailpacking files, in order to inline data more effectively, special EOF
			// lclusters are now supported which can have three parts at most.
			if ztailpacking && end > n.size {
				end = n.size
			}
			break
		}
		// m.lcn should be >= 1 if endoff < m.clusterofs
		if r.lcn == 0 {
			return n.zCorrupt("invalid logical cluster 0")
		}
		end = r.lcn<<lclusterBits | r.clusterofs
		m.fullMapped = true
		r.delta[0] = 1
		fallthrough
	case zTypeNonhead:
		// get the corresponding first chunk
		if err := r.lookback(r.delta[0]); err != nil {
			return err
		}
	}
	if r.partialref {
		m.partial = true
	}
	m.llen = end - m.la

	if flags&zFindTail != 0 {
		z.tailHeadLcn = r.lcn
		// for non-compact indexes, fragmentoff is 64 bits
		if fragment && n.layout == erofsLayoutCompressedFull {
			z.fragmentOff |= r.pblk << 32
		}
	}
	if ztailpacking && r.lcn == z.tailHeadLcn {
		m.meta = true
		m.pa = z.fragmentOff
		m.plen = z.idataSize
		if v.blkoff(m.pa)+m.plen > v.blksz {
			return n.zCorrupt("invalid tail-packing pclustersize %d", m.plen)
		}
	} else if fragment && r.lcn == z.tailHeadLcn {
		*m = zMap{la: m.la, llen: m.llen, mapped: true, fragment: true, fragOff: z.fragmentOff}
	} else {
		m.pa = r.pblk << v.blkbits
		if err := r.compressedLen(initialLcn); err != nil {
			return err
		}
	}

	switch r.headtype {
	case zTypePlain:
		if m.llen > m.plen {
			return n.zCorrupt("plain pcluster: llen %d > plen %d", m.llen, m.plen)
		}
		if z.advise&zAdviseInterlaced != 0 {
			m.alg = zAlgInterlaced
		} else {
			m.alg = zAlgShifted
		}
	case zTypeHead2:
		m.alg = z.alg[1]
	default:
		m.alg = z.alg[0]
	}

	if flags&zFiemap != 0 {
		if err := r.decompressedLen(); err != nil {
			return err
		}
		m.fullMapped = true
	}
	return nil
}

// mapExt is z_erofs_map_blocks_ext (48-bit encoded extents).
func (n *erofsInode) mapExt(m *zMap) error {
	v := n.v
	z := &n.z
	le := binary.LittleEndian
	interlaced := z.advise&zAdviseInterlaced != 0
	recsz := int64(4) << (z.advise >> 1 & 3)
	pos := roundUp(roundUp(n.iloc+n.isize+n.xattrSize, 8)+8, recsz) // Z_EROFS_MAP_HEADER_END
	bmask := v.blksz - 1
	lend := n.size
	var lstart, pa int64
	var plen uint32
	var last bool

	if recsz <= 8 { // offsetof(pstart_hi)
		if recsz <= 4 { // offsetof(pstart_lo)
			b, err := v.read(pos, 8)
			if err != nil {
				return err
			}
			pa = int64(le.Uint64(b))
			pos += 8
			lstart = 0
		} else {
			lstart = m.la &^ (1<<z.lclusterBits - 1)
			pos += (lstart >> z.lclusterBits) * recsz
			pa = -1 // EROFS_NULL_ADDR
		}
		for ; lstart <= m.la; lstart += 1 << z.lclusterBits {
			ext, err := v.read(pos, int(recsz))
			if err != nil {
				return err
			}
			plen = le.Uint32(ext)
			if pa != -1 {
				m.pa = pa
				pa += int64(plen & zPlenMask)
			} else {
				m.pa = int64(le.Uint32(ext[4:]))
			}
			pos += recsz
		}
		last = lstart >= roundUp(lend, 1<<z.lclusterBits)
		lend = min(lstart, lend)
		lstart -= 1 << z.lclusterBits
	} else {
		lstart = lend
		l, r := int64(0), z.extents
		for l < r {
			mid := l + (r-l)/2
			ext, err := v.read(pos+mid*recsz, int(recsz))
			if err != nil {
				return err
			}
			la := int64(le.Uint32(ext[12:]))
			epa := int64(le.Uint32(ext[4:])) | int64(le.Uint32(ext[8:]))<<32
			if recsz > 16 { // offsetof(lstart_hi)
				la |= int64(le.Uint32(ext[16:])) << 32
			}
			if la > m.la {
				r = mid
				if la > lend {
					return n.zCorrupt("extent lstart %d beyond %d", la, lend)
				}
				lend = la
			} else {
				l = mid + 1
				if m.la == la {
					r = min(l+1, r)
				}
				lstart = la
				plen = le.Uint32(ext)
				m.pa = epa
			}
		}
		last = l >= z.extents
	}

	if lstart < lend {
		m.la = lstart
		if last && z.advise&zAdviseFragment != 0 {
			m.mapped, m.fragment = true, true
			m.fragOff = int64(plen)
			if recsz > 4 {
				m.fragOff |= m.pa << 32
			}
		} else if plen&zPlenMask != 0 {
			m.mapped, m.fullMapped, m.encoded = true, true, true
			fmtv := plen >> zPlenFmtBit
			m.partial = plen&zPlenPartial != 0
			m.plen = int64(plen & zPlenMask)
			if fmtv != 0 {
				m.alg = int(fmtv - 1)
			} else if interlaced && (m.pa|m.plen)&bmask == 0 {
				m.alg = zAlgInterlaced
			} else {
				m.alg = zAlgShifted
			}
		}
	}
	m.llen = lend - m.la
	return nil
}

// fillInodeLazy is z_erofs_fill_inode_lazy.
func (n *erofsInode) fillInodeLazy() error {
	z := &n.z
	le := binary.LittleEndian
	h, err := n.v.read(roundUp(n.iloc+n.isize+n.xattrSize, 8), 8)
	if err != nil {
		return err
	}
	// if the highest bit of the 8-byte map header is set, the whole file is stored in the
	// packed inode. The rest bits keeps z_fragmentoff.
	if h[7]>>7 != 0 {
		z.advise = zAdviseFragment
		z.fragmentOff = int64(le.Uint64(h) ^ 1<<63)
		z.tailHeadLcn = 0
		return nil
	}
	z.advise = le.Uint16(h[4:])
	z.lclusterBits = n.v.blkbits + uint(h[7]&15)
	if n.layout == erofsLayoutCompressedFull && z.advise&zAdviseExtents != 0 {
		z.extents = int64(le.Uint32(h)) | int64(le.Uint16(h[6:]))<<32
		return nil
	}
	z.alg = [2]int{int(h[6] & 15), int(h[6] >> 4)}
	if z.advise&zAdviseFragment != 0 {
		z.fragmentOff = int64(le.Uint32(h))
	} else if z.advise&zAdviseInline != 0 {
		z.idataSize = int64(le.Uint16(h[2:]))
	}
	if n.layout == erofsLayoutCompressedCompact &&
		(z.advise&zAdviseBigPcluster1 == 0) != (z.advise&zAdviseBigPcluster2 == 0) {
		return n.zCorrupt("big pcluster head1/2 of compact indexes should be consistent")
	}
	if z.idataSize != 0 || z.advise&zAdviseFragment != 0 {
		var m zMap
		if err := n.mapFo(&m, zFindTail); err != nil {
			return err
		}
	}
	return nil
}

// mapSanityCheck is z_erofs_map_sanity_check.
func (n *erofsInode) mapSanityCheck(m *zMap) error {
	if !m.encoded {
		return nil
	}
	if m.alg > zAlgInterlaced {
		return n.zCorrupt("unknown algorithm %d @ %d", m.alg, m.la)
	}
	if m.alg < 4 && n.v.algs&(1<<m.alg) == 0 {
		return n.zCorrupt("inconsistent algorithm %d", m.alg)
	}
	if m.plen > zPclusterMaxSize || m.llen > zPclusterMaxDsize {
		return n.zCorrupt("pcluster too large (plen %d, llen %d)", m.plen, m.llen)
	}
	return nil
}

// readPlain serves a shifted or interlaced (uncompressed) pcluster straight from the raw cache:
// z_erofs_decompress for those two formats, by byte position. Not routed through the pcl cache,
// because interlaced output depends on blkoff(la), which a pa-keyed entry could not tell apart.
func (n *erofsInode) readPlain(m zMap, dst []byte, skip int64) error {
	v := n.v
	if m.llen > m.plen {
		return n.zCorrupt("plain pcluster: llen %d > plen %d", m.llen, m.plen)
	}
	if m.alg == zAlgShifted {
		_, err := v.raw.ReadAt(dst, m.pa+skip)
		return err
	}
	if m.plen > v.blksz {
		return n.zCorrupt("interlaced pcluster larger than a block")
	}
	for len(dst) > 0 {
		i := v.blkoff(v.blkoff(m.la) + skip)
		k := min(int64(len(dst)), v.blksz-i)
		if i+k > m.plen {
			return n.zCorrupt("interlaced pcluster: offset %d beyond plen %d", i+k, m.plen)
		}
		if _, err := v.raw.ReadAt(dst[:k], m.pa+i); err != nil {
			return err
		}
		dst, skip = dst[k:], skip+k
	}
	return nil
}

var zstdDec, _ = zstd.NewReader(nil)

// decode is z_erofs_decompress for LZ4 / MicroLZMA / DEFLATE / zstd. It returns the whole
// decoded stream: llen bytes, or (PARTIAL_REF) at least llen, so a deduplicated pcluster is
// cached once and every reference takes its prefix.
// ponytail: a fresh decoder per pcluster; pool them if profiles show allocation churn.
func (v *erofsVol) decode(m zMap) ([]byte, error) {
	in, err := v.read(m.pa, int(m.plen))
	if err != nil {
		return nil, err
	}
	// z_erofs_fixup_insize: compressed data is right-aligned in the pcluster
	margin := 0
	for margin < len(in) && in[margin] == 0 {
		margin++
	}
	if margin >= len(in) {
		return nil, fmt.Errorf("%s: erofs: all-zero pcluster at %d", v.path, m.pa)
	}
	in = in[margin:]
	llen := int(m.llen)
	var out []byte
	switch m.alg {
	case 0: // LZ4 block; pierrec has no partial decode, so grow the buffer until the stream fits
		for size := llen; ; size *= 2 {
			buf := make([]byte, size)
			k, e := lz4.UncompressBlock(in, buf)
			if e == nil {
				out, err = buf[:k], nil
				break
			}
			if err = e; !m.partial || size >= zPclusterMaxDsize {
				break
			}
		}
	case 1: // MicroLZMA = .lzma with props ^= 0xff stored first, header and range coder's leading 0 dropped
		size := uint64(llen)
		if m.partial {
			size = zPclusterMaxDsize
		}
		dict := v.lzmaDict
		if dict == 0 {
			dict = 8 << 20 // Z_EROFS_LZMA_MAX_DICT_SIZE
		}
		hdr := make([]byte, 14)
		hdr[0] = ^in[0]
		binary.LittleEndian.PutUint32(hdr[1:], dict)
		binary.LittleEndian.PutUint64(hdr[5:], size)
		var r io.Reader
		if r, err = lzma.NewReader(io.MultiReader(bytes.NewReader(hdr), bytes.NewReader(in[1:]))); err == nil {
			out, err = readStream(r, llen, m.partial)
		}
	case 2: // raw DEFLATE
		out, err = readStream(flate.NewReader(bytes.NewReader(in)), llen, m.partial)
	case 3:
		out, err = zstdDec.DecodeAll(in, nil)
	default:
		err = errors.New("unsupported algorithm")
	}
	if err == nil && (len(out) < llen || !m.partial && len(out) != llen) {
		err = fmt.Errorf("decoded %d bytes, want %d", len(out), llen)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: erofs: pcluster at %d (algorithm %d): %w", v.path, m.pa, m.alg, err)
	}
	return out, nil
}

// readStream reads exactly llen bytes, or (partial) everything up to the stream's end or error,
// accepting a truncated tail once llen bytes are there.
func readStream(r io.Reader, llen int, partial bool) ([]byte, error) {
	if !partial {
		out := make([]byte, llen)
		_, err := io.ReadFull(r, out)
		return out, err
	}
	out, err := io.ReadAll(r)
	if len(out) >= llen {
		err = nil
	}
	return out, err
}
