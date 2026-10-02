package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"hash/adler32"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func tibHeader(version uint16, seq uint32) []byte {
	h := make([]byte, 32)
	le := binary.LittleEndian
	le.PutUint32(h[0:], tibMagic)
	le.PutUint16(h[4:], 0x20)
	le.PutUint16(h[6:], version)
	le.PutUint64(h[8:], 0x1122334455667788)
	le.PutUint32(h[0x10:], 7)
	le.PutUint32(h[0x14:], seq)
	le.PutUint32(h[0x1c:], 32)
	le.PutUint32(h[0x18:], adler32.Checksum(h))
	return h
}

// writeTib synthesizes a legacy .tib of img: 256 KiB blocks in shuffled order, all-zero
// blocks sparse, zero clusters cleared from the bitmap. frag > 0 writes an early chunk
// map fragment for blocks [0,frag) after those blocks, as TI does at the start of a backup.
func writeTib(t *testing.T, img []byte, dst string, frag int) {
	const cl, bs = 4096, 64 * 4096
	img = append(img, make([]byte, (bs-len(img)%bs)%bs)...)
	count := len(img) / bs
	var out bytes.Buffer
	out.Write(tibHeader(0, 1))
	recs := make([]tibRec, count)
	rnd := rand.New(rand.NewPCG(1, 2))
	writeMap := func(recs []tibRec) {
		n := len(recs)
		plain := make([]byte, 12*n)
		var acc int64
		for r, rec := range recs {
			var e [12]byte
			if rec.n != 0 {
				d := rec.off - acc
				v := uint64(d) << 1
				if d < 0 {
					v = uint64(-d)<<1 | 1
				}
				binary.LittleEndian.PutUint64(e[:], v)
				acc = rec.off
			}
			binary.LittleEndian.PutUint32(e[8:], rec.n)
			acc += int64(rec.n)
			for c := range 12 {
				plain[c*n+r] = e[c]
			}
		}
		out.Write([]byte{19, 2, 0, 2, 0, 2, 3, 0, 1, 8, 4, 0, 1, 64, 6, 0, 3, byte(n), byte(n >> 8), byte(n >> 16)})
		zw := zlib.NewWriter(&out)
		zw.Write(plain)
		zw.Close()
	}
	writeBlocks := func(lo, hi int) {
		for _, i := range rnd.Perm(hi - lo) {
			b := img[(lo+i)*bs : (lo+i+1)*bs]
			var bm [8]byte
			var data []byte
			for c := range 64 {
				if x := b[c*cl : (c+1)*cl]; slices.ContainsFunc(x, func(v byte) bool { return v != 0 }) {
					bm[c/8] |= 1 << (c % 8)
					data = append(data, x...)
				}
			}
			if data == nil {
				continue // sparse: zero-length record
			}
			off := int64(out.Len() - 32)
			out.Write(bm[:])
			zw, _ := zlib.NewWriterLevel(&out, zlib.BestSpeed)
			zw.Write(data)
			zw.Close()
			recs[lo+i] = tibRec{off, uint32(int64(out.Len()-32) - off)}
		}
	}
	writeBlocks(0, frag)
	if frag > 0 {
		writeMap(recs[:frag])
	}
	writeBlocks(frag, count)
	writeMap(recs[frag:])
	filler := make([]byte, 100<<10) // stands in for the MD5 manifest and metadata
	for i := range filler {
		filler[i] = byte(rnd.Uint32())
	}
	out.Write(filler)
	out.Write(make([]byte, 37)) // trailer body
	binary.Write(&out, binary.LittleEndian, [2]uint32{37, tibDiskMagic})
	slice := uint64(out.Len() - 32)
	out.Write(make([]byte, 81))
	binary.Write(&out, binary.LittleEndian, struct {
		Zero, Vol uint32
		Slice     uint64
	}{0, 7, slice})
	h := tibHeader(0, 1)
	slices.Reverse(h)
	out.Write(h)
	if err := os.WriteFile(dst, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTibHeaderErrors(t *testing.T) {
	for _, tc := range []struct {
		h    []byte
		want string
	}{
		{tibHeader(1, 1), "True Image 2013 or older"},
		{tibHeader(0, 2), "multi-volume"},
	} {
		if _, err := openTib("x", bytes.NewReader(tc.h), 32); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("err = %v, want %q", err, tc.want)
		}
	}
}

// excerpts serves the head and tail of a large file and fails in between.
type excerpts struct {
	head, tail []byte
	size       int64
}

func (e excerpts) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) <= int64(len(e.head)) {
		return copy(p, e.head[off:]), nil
	}
	if t := e.size - int64(len(e.tail)); off >= t && off+int64(len(p)) <= e.size {
		return copy(p, e.tail[off-t:]), nil
	}
	return 0, errors.New("outside the excerpts")
}

// TestTibSample checks a real TI 2014 backup through its first 16 MiB and last 64 MiB:
// BM_TIB_SAMPLE=dir holding tib-head.bin and tib-tail.bin.
func TestTibSample(t *testing.T) {
	dir := os.Getenv("BM_TIB_SAMPLE")
	if dir == "" {
		t.Skip("BM_TIB_SAMPLE not set")
	}
	head, err := os.ReadFile(filepath.Join(dir, "tib-head.bin"))
	if err != nil {
		t.Fatal(err)
	}
	tail, err := os.ReadFile(filepath.Join(dir, "tib-tail.bin"))
	if err != nil {
		t.Fatal(err)
	}
	tr, err := openTib("sample", excerpts{head, tail, 109_700_283_904}, 109_700_283_904)
	if err != nil {
		t.Fatal(err)
	}
	stored := 0
	for _, r := range tr.recs {
		if r.n != 0 {
			stored++
		}
	}
	if len(tr.recs) != 610504 || stored != 567858 || tr.blockSize != 262144 || tr.pre != 8 {
		t.Fatalf("count %d stored %d blockSize %d pre %d", len(tr.recs), stored, tr.blockSize, tr.pre)
	}
	if tr.recs[15032].off != 0 || tr.recs[429970] != (tibRec{58039, 131486}) || tr.recs[0] != (tibRec{8037163, 74063}) {
		t.Fatalf("recs[15032]=%v recs[429970]=%v recs[0]=%v", tr.recs[15032], tr.recs[429970], tr.recs[0])
	}
	buf := make([]byte, 16)
	if _, err := tr.ReadAt(buf, 15032*262144); err != nil {
		t.Fatal(err)
	}
	if want := []byte{0x00, 0x40, 0, 0, 0xc8, 0x3f, 0, 0, 0x28, 0x40, 0, 0, 0, 0, 0, 0}; !bytes.Equal(buf, want) {
		t.Fatalf("block 15032 starts % x, want % x", buf, want)
	}
	if _, err := tr.ReadAt(buf, 0); err != nil || string(buf[3:11]) != "NTFS    " {
		t.Fatalf("block 0: %v, boot sector % x", err, buf)
	}
	if k := probe(tr); k != "ntfs" {
		t.Fatalf("probe = %q, want ntfs", k)
	}
	inHead := 0
	for i, r := range tr.recs {
		if r.n != 0 && 32+r.off+int64(r.n) <= int64(len(head)) {
			inHead++
			if _, err := tr.block(int64(i)); err != nil {
				t.Errorf("block %d: %v", i, err)
			}
		}
	}
	if inHead != 180 {
		t.Errorf("%d stored blocks inside the head, want 180", inHead)
	}
	for i, r := range tr.recs { // the first stored block past the head is outside the excerpts: error, not panic
		if r.n != 0 && 32+r.off > int64(len(head)) && 32+r.off < 1<<30 {
			if _, err := tr.ReadAt(buf, int64(i)*262144); err == nil {
				t.Fatalf("block %d lies outside the excerpts, want an error", i)
			}
			break
		}
	}
}
