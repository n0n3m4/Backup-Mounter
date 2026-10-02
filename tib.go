package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/adler32"
	"io"
	"log"
	"math/bits"
	"sort"
)

// Acronis True Image 2014-2017 sector-mode partition backup (.tib, "legacy"):
//
//	[32-byte header][blocks: bitmap preamble + zlib of the present clusters, in any order]
//	[inline chunk map: u8 L, L bytes TLV, zlib of count x 12 transposed bytes]
//	[MD5 manifest, metadata][trailer][u32 trailer size][u32 0x94E18A2B][padding][48-byte footer]
//
// The chunk map (record r = partition block r) is the only way to find a block.
// ponytail: full partition backups only; incremental/differential, encrypted,
// multi-volume, TI 2018+ (.tib with the map elsewhere, .tibx) and TI 2013 or older are refused.

const (
	tibMagic     = 0xA2B924CE
	tibDiskMagic = 0x94E18A2B
	tibFileMagic = 0x94E18A2C
)

type tibRec struct {
	off int64 // relative to the end of the header (file offset 32)
	n   uint32
}

type tibReader struct {
	r                        io.ReaderAt
	size, blockSize, cluster int64
	pre                      int // bitmap bytes in front of each block
	recs                     []tibRec
}

func openTib(name string, r io.ReaderAt, size int64) (*tibReader, error) {
	var h [32]byte
	if _, err := r.ReadAt(h[:], 0); err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	switch {
	case le.Uint32(h[0:]) != tibMagic:
		return nil, errors.New("not a .tib")
	case le.Uint16(h[6:]) != 0:
		return nil, errors.New("unsupported .tib generation (True Image 2013 or older)")
	case le.Uint32(h[0x14:]) != 1:
		return nil, errors.New("multi-volume .tib (_v2…) not supported")
	}
	sum := le.Uint32(h[0x18:])
	copy(h[0x18:0x1c], []byte{0, 0, 0, 0})
	if adler32.Checksum(h[:]) != sum {
		return nil, errors.New(".tib header checksum mismatch")
	}

	var b [8]byte
	if _, err := r.ReadAt(b[:], size-40); err != nil {
		return nil, err
	}
	end := 32 + int64(le.Uint64(b[:]))
	if end < 40 || end > size-48 {
		return nil, fmt.Errorf(".tib footer: slice end %d out of range", end)
	}
	if _, err := r.ReadAt(b[:], end-8); err != nil {
		return nil, err
	}
	switch le.Uint32(b[4:]) {
	case tibDiskMagic:
	case tibFileMagic:
		return nil, errors.New("file-level backup, not a disk image")
	default:
		return nil, errors.New(".tib trailer magic not found")
	}

	t := &tibReader{r: r}
	recs, at, tlv, err := t.findMap(end - 8 - int64(le.Uint32(b[:])))
	if err != nil {
		return nil, err
	}
	t.cluster = int64(tlv[2] * tlv[3])
	t.blockSize = t.cluster * int64(tlv[4])
	t.pre = int(tlv[4] / 8)
	if from, _ := tileStart(recs, t.pre, at-32); from != 0 {
		if recs, err = t.earlyFragments(from, recs); err != nil {
			return nil, err
		}
	}
	t.recs = recs
	t.size = int64(len(recs)) * t.blockSize
	stored := 0
	for _, rec := range recs {
		if rec.n != 0 {
			stored++
		}
	}
	log.Printf("%s: Acronis .tib, %d blocks of %d KiB, %d stored, %d MiB partition", name, len(recs), t.blockSize>>10, stored, t.size>>20)
	return t, nil
}

// findMap scans backwards from limit for the terminal inline chunk map, which sits
// right before the MD5 manifest and metadata (~20-25 bytes per block from the end).
func (t *tibReader) findMap(limit int64) ([]tibRec, int64, map[uint16]uint64, error) {
	const chunk, slack = 16 << 20, 128
	for top := limit; top > 32 && limit-top < 1<<30; top -= chunk {
		base := max(32, top-chunk)
		buf := make([]byte, top+slack-base)
		n, err := t.r.ReadAt(buf, base)
		if int64(n) < top-base {
			return nil, 0, nil, fmt.Errorf("reading .tib at %d: %v", base, err)
		}
		buf = buf[:n]
		for i := int(top-base) - 1; i >= 0; i-- {
			if _, _, ok := tibCandidate(buf[i:]); !ok {
				continue
			}
			recs, _, tlv, err := t.decodeMap(base + int64(i))
			if err != nil {
				continue
			}
			if _, ok := tileStart(recs, int(tlv[4]/8), base+int64(i)-32); ok {
				return recs, base + int64(i), tlv, nil
			}
		}
	}
	return nil, 0, nil, errors.New("no inline chunk map found (True Image 2018+ format not supported)")
}

// earlyFragments walks the block stream from the start to collect inline chunk
// maps written before the terminal one (whose first stored block is at from).
// ponytail: forward walk inflates every block before from; fine while fragments come early.
func (t *tibReader) earlyFragments(from int64, tail []tibRec) ([]tibRec, error) {
	var recs []tibRec
	pos, done := int64(32), int64(0)
	for pos < 32+from {
		head := make([]byte, 66)
		n, _ := t.r.ReadAt(head, pos)
		if _, _, ok := tibCandidate(head[:n]); ok {
			if fr, next, _, err := t.decodeMap(pos); err == nil {
				if s, ok := tileStart(fr, t.pre, pos-32); ok && s == done {
					recs, pos, done = append(recs, fr...), next, next-32
					continue
				}
			}
		}
		k, err := inflatedLen(io.NewSectionReader(t.r, pos+int64(t.pre), 1<<62), io.Discard)
		if err != nil {
			return nil, fmt.Errorf(".tib block at %d: %v", pos, err)
		}
		pos += int64(t.pre) + k
	}
	if done != from {
		return nil, errors.New(".tib: chunk map fragments do not cover the block stream")
	}
	return append(recs, tail...), nil
}

// tibCandidate checks the cheap signature of an inline chunk map at b[0]:
// L in 8..64, a TLV that starts with tag 2 and holds tags 2, 3, 4 and 6, then a zlib header.
func tibCandidate(b []byte) (l int, tlv map[uint16]uint64, ok bool) {
	if len(b) < 4 || b[0] < 8 || b[0] > 64 || b[1] != 2 || b[2] != 0 || b[3] != 2 {
		return 0, nil, false
	}
	l = int(b[0])
	if len(b) < l+2 || b[1+l] != 0x78 {
		return 0, nil, false
	}
	tlv = map[uint16]uint64{}
	for p := b[1 : 1+l]; len(p) > 0; {
		if len(p) < 3 {
			return 0, nil, false
		}
		tag, n, v := binary.LittleEndian.Uint16(p), int(p[2]), p[3:]
		if n&0x80 != 0 {
			if len(p) < 4 {
				return 0, nil, false
			}
			n, v = (n<<8|int(p[3]))&0x7fff, p[4:]
		}
		if n > len(v) {
			return 0, nil, false
		}
		var x uint64
		for i := min(n, 8) - 1; i >= 0; i-- {
			x = x<<8 | uint64(v[i])
		}
		tlv[tag], p = x, v[n:]
	}
	for _, tag := range []uint16{2, 3, 4, 6} {
		if tlv[tag] == 0 || tlv[tag] >= 1<<32 {
			return 0, nil, false
		}
	}
	return l, tlv, tlv[4]%8 == 0
}

// decodeMap inflates the inline chunk map at off; next is the offset just past it.
func (t *tibReader) decodeMap(off int64) (recs []tibRec, next int64, tlv map[uint16]uint64, err error) {
	head := make([]byte, 66)
	n, _ := t.r.ReadAt(head, off)
	l, tlv, ok := tibCandidate(head[:n])
	if !ok {
		return nil, 0, nil, errors.New("no chunk map")
	}
	count := int(tlv[6])
	var plain bytes.Buffer
	k, err := inflatedLen(io.NewSectionReader(t.r, off+1+int64(l), 1<<62), &limitWriter{&plain, count * 12})
	if err != nil || plain.Len() != count*12 {
		return nil, 0, nil, fmt.Errorf("chunk map inflates to %d bytes, want %d: %v", plain.Len(), count*12, err)
	}
	p := plain.Bytes() // transposed: p[c*count+r] is byte c of record r
	recs = make([]tibRec, count)
	var acc int64
	for r := range recs {
		var v uint64
		for c := 7; c >= 0; c-- {
			v = v<<8 | uint64(p[c*count+r])
		}
		var n uint32
		for c := 11; c >= 8; c-- {
			n = n<<8 | uint32(p[c*count+r])
		}
		d := int64(v >> 1) // zigzag
		if v&1 != 0 {
			d = -d
		}
		recs[r] = tibRec{acc + d, n}
		acc = acc + d + int64(n)
	}
	return recs, off + 1 + int64(l) + k, tlv, nil
}

// tileStart reports where the stored records start if, sorted by offset, they
// tile the block stream up to `to` without gaps or overlaps.
func tileStart(recs []tibRec, pre int, to int64) (from int64, ok bool) {
	var s []tibRec
	for _, r := range recs {
		if r.n != 0 {
			if int(r.n) < pre+2 {
				return 0, false
			}
			s = append(s, r)
		}
	}
	sort.Slice(s, func(i, j int) bool { return s[i].off < s[j].off })
	pos := to
	if len(s) > 0 {
		pos = s[0].off
	}
	from = pos
	for _, r := range s {
		if r.off != pos {
			return 0, false
		}
		pos += int64(r.n)
	}
	return from, from >= 0 && pos == to
}

// inflatedLen inflates the zlib stream at r into w and returns its compressed length.
func inflatedLen(r io.Reader, w io.Writer) (int64, error) {
	c := &countReader{r: bufio.NewReader(r)}
	zr, err := zlib.NewReader(c) // a flate.Reader is used as is, so c.n stops exactly at the stream end
	if err != nil {
		return 0, err
	}
	_, err = io.Copy(w, zr)
	return c.n, err
}

type countReader struct {
	r *bufio.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
func (c *countReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

// limitWriter fails once more than n bytes arrive (a false candidate must not inflate forever).
type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if len(p) > l.n {
		return 0, errors.New("chunk map longer than its record count")
	}
	l.n -= len(p)
	return l.w.Write(p)
}

// ReadAt decodes the blocks p covers; the caller's chunkCache (chunk = blockSize)
// makes that one whole block per call, decoded at most once while cached.
func (t *tibReader) ReadAt(p []byte, off int64) (n int, err error) {
	for n < len(p) {
		pos := off + int64(n)
		if pos >= t.size {
			return n, io.EOF
		}
		blk, err := t.block(pos / t.blockSize)
		if err != nil {
			return n, err
		}
		n += copy(p[n:], blk[pos%t.blockSize:])
	}
	return n, nil
}

func (t *tibReader) block(i int64) ([]byte, error) {
	out := make([]byte, t.blockSize)
	rec := t.recs[i]
	if rec.n == 0 {
		return out, nil // sparse
	}
	raw := make([]byte, rec.n)
	if _, err := t.r.ReadAt(raw, 32+rec.off); err != nil {
		return nil, fmt.Errorf(".tib block %d: %v", i, err)
	}
	bm := raw[:t.pre]
	present := 0
	for _, b := range bm {
		present += bits.OnesCount8(b)
	}
	data := make([]byte, int64(present)*t.cluster)
	zr, err := zlib.NewReader(bytes.NewReader(raw[t.pre:]))
	if err == nil {
		if _, err = io.ReadFull(zr, data); err == nil {
			if k, e := zr.Read(make([]byte, 1)); k != 0 || e != io.EOF { // EOF also verifies the adler32
				err = fmt.Errorf("trailing data or bad checksum (%v)", e)
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf(".tib block %d: %v", i, err)
	}
	for c, j := 0, int64(0); c < len(bm)*8; c++ {
		if bm[c/8]&(1<<(c%8)) != 0 {
			copy(out[int64(c)*t.cluster:], data[j:j+t.cluster])
			j += t.cluster
		}
	}
	return out, nil
}
