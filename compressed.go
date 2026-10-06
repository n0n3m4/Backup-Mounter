package main

import (
	"bufio"
	"bytes"
	stdflate "compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/n0n3m4/backup-mounter/third_party/flate"
	"github.com/unxed/xz"
)

// indexSpan is the uncompressed distance between gzip checkpoints (a variable so the test can shrink it).
var indexSpan int64 = 16 << 20

// openCompressed returns the decompressed contents of a gzip or xz file, the
// partition inside an Acronis .tib, or the virtual disk of a qcow2 or VMDK, as a
// random-access reader; ok=false means f is none of them and the caller carries on.
//
//	gzip/xz:     raw chunkCache (1/8 budget) -> spanReader (one live decoder) -> decompressed chunkCache (7/8)
//	.tib:        file (blocks are random-access) -> tibReader -> decompressed chunkCache (whole budget, chunk = block)
//	qcow2/VMDK:  raw chunkCache (1/8) -> qcow2 / vmdk reader (+ sibling extent files) -> decompressed chunkCache (7/8)
func openCompressed(path string, f *os.File, st os.FileInfo, budget int64) (r io.ReaderAt, size int64, ok bool, err error) {
	var magic [32]byte
	n, _ := f.ReadAt(magic[:], 0)
	var s *spanReader
	switch {
	case bytes.HasPrefix(magic[:n], []byte("QFI\xfb")), bytes.HasPrefix(magic[:n], []byte("KDMV")), bytes.HasPrefix(magic[:n], []byte(vmdkDescMagic)):
		raw := newChunkCache(f, st.Size(), 4<<20, budget/8)
		if magic[0] == 'Q' {
			r, size, err = openQcow2(filepath.Base(path), raw)
		} else {
			r, size, err = openVMDK(path, raw, st.Size())
		}
		if err != nil {
			return nil, 0, true, fmt.Errorf("%s: %w", path, err)
		}
		return newChunkCache(r, size, 4<<20, budget/8*7), size, true, nil // no read-ahead: an uncompressed qcow2 / flat VMDK would make it pure IO
	case bytes.HasPrefix(magic[:n], []byte("COWD")):
		return nil, 0, true, fmt.Errorf("%s: COWD sparse VMDK (ESX vmfsSparse / Workstation 3) not supported", path)
	case n >= 2 && string(magic[:2]) == "\x1f\x8b":
		s, err = openGzip(path, f, st, newChunkCache(f, st.Size(), 4<<20, budget/8))
	case n >= 6 && string(magic[:6]) == "\xfd7zXZ\x00":
		s, err = openXZ(path, newChunkCache(f, st.Size(), 4<<20, budget/8), st.Size())
	case n >= 4 && string(magic[:4]) == "\xce\x24\xb9\xa2": // Acronis .tib
		t, err := openTib(filepath.Base(path), f, st.Size())
		if err != nil {
			return nil, 0, true, fmt.Errorf("%s: %w", path, err)
		}
		return newChunkCache(t, t.size, t.blockSize, budget).withAhead(), t.size, true, nil
	default:
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, true, fmt.Errorf("%s: %w", path, err)
	}
	s.cache = newChunkCache(s, s.size, 4<<20, budget/8*7)
	return s.cache, s.size, true, nil
}

// spanReader is an io.ReaderAt over a stream that can only be decoded forward
// from a set of known restart points (gzip checkpoints, xz blocks).
// ponytail: one live decoder behind one mutex; per-span decoders if parallel fills matter.
type spanReader struct {
	mu     sync.Mutex
	size   int64
	starts []int64                        // uncompressed offset where span i begins, ascending, starts[0] = 0
	open   func(i int) (io.Reader, error) // decoder positioned at starts[i]

	cache *chunkCache // the cache wrapping this reader, if any: bytes skipped to reach a read are put there

	cur    io.Reader
	curIdx int
	curPos int64
}

func (s *spanReader) span(off int64) int {
	return sort.Search(len(s.starts), func(i int) bool { return s.starts[i] > off }) - 1
}

func (s *spanReader) seek(i int) (err error) {
	if c, ok := s.cur.(io.Closer); ok {
		c.Close() // returns pooled decoders (7z)
	}
	s.cur, s.curIdx, s.curPos = nil, i, s.starts[i]
	s.cur, err = s.open(i)
	return err
}

func (s *spanReader) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 || off >= s.size {
		return 0, io.EOF
	}
	short := int64(len(p)) > s.size-off
	if short {
		p = p[:s.size-off]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Keep the live decoder when it is at or before off, but not behind off's span start.
	if i := s.span(off); s.cur == nil || s.curPos > off || s.curPos < s.starts[i] {
		if err = s.seek(i); err != nil {
			s.cur = nil
			return 0, err
		}
	}
	// The decoder will pass through whole chunks before off: cache them instead of
	// discarding (a solid 7z read out of archive order would re-decode them otherwise).
	if s.cache != nil {
		ch := s.cache.chunk
		for g := (s.curPos + ch - 1) / ch * ch; g+ch <= off; g += ch {
			buf := make([]byte, ch)
			if _, err = s.read(buf, g); err != nil {
				return 0, err
			}
			s.cache.put(g/ch, buf)
		}
	}
	n, err = s.read(p, off)
	if short && n == len(p) {
		return n, io.EOF
	}
	return n, err
}

// read fills p from off with the live decoder, which ReadAt placed at or before off. s.mu held.
func (s *spanReader) read(p []byte, off int64) (n int, err error) {
	for err == nil && n < len(p) {
		var k int64
		if s.curPos < off {
			k, err = io.CopyN(io.Discard, s.cur, off-s.curPos)
		} else {
			var kk int
			kk, err = s.cur.Read(p[n:])
			n, k = n+kk, int64(kk)
		}
		s.curPos += k
		if err == io.EOF && s.curPos < s.size { // end of an xz block: continue in the next one
			if i := s.span(s.curPos); s.starts[i] == s.curPos && i > s.curIdx {
				err = s.seek(i)
			} else {
				err = io.ErrUnexpectedEOF
			}
		}
	}
	if n < len(p) {
		s.cur = nil
		return n, err
	}
	return n, nil
}

// --- xz: the stream's own block index gives the restart points ---

func openXZ(path string, raw io.ReaderAt, csize int64) (*spanReader, error) {
	blocks, err := xz.ParseBlocks(raw, csize)
	if err != nil {
		return nil, err
	}
	if len(blocks) <= 1 {
		return nil, errors.New("single-block xz, no random access: recompress with xz -T0 --block-size=16MiB")
	}
	s := &spanReader{}
	for _, b := range blocks {
		s.starts = append(s.starts, b.UncompressedOffset)
		s.size = b.UncompressedOffset + b.UncompressedSize
	}
	s.open = func(i int) (io.Reader, error) {
		b := blocks[i]
		return xz.ReaderConfig{}.NewBlockReader(bufio.NewReader(io.NewSectionReader(raw, b.Offset, b.CompressedSize)), b.StreamFlags)
	}
	log.Printf("%s: xz, %d blocks, %d MiB", filepath.Base(path), len(blocks), s.size>>20)
	return s, nil
}

// --- gzip: one indexing pass records checkpoints in <archive>.idx ---

// Index file, little-endian: compressed windows from offset 0, then n idxRecords, then idxFooter.
type idxRecord struct {
	COff, UOff, WinOff uint64 // COff relative to the end of the gzip header
	WinLen             uint32
	Bits, NBits        uint8
	_                  uint16
}

type idxFooter struct {
	Magic                     [8]byte
	ArchiveSize, ArchiveMtime int64
	USize, Span, TableOff, N  int64
	_                         int64
}

var idxMagic = [8]byte{'B', 'M', 'G', 'Z', 'I', 'D', 'X', '1'}

func openGzip(path string, f *os.File, st os.FileInfo, raw io.ReaderAt) (*spanReader, error) {
	hdrLen, err := gzipHeaderLen(io.NewSectionReader(f, 0, st.Size()))
	if err != nil {
		return nil, err
	}
	name := filepath.Base(path)
	paths := idxPaths(path, ".idx")
	var idx *os.File
	var ft idxFooter
	var recs []idxRecord
	for _, p := range paths {
		if idx, ft, recs, err = loadIndex(p, st); err == nil {
			log.Printf("%s: index loaded from %s, %d checkpoints", name, p, len(recs))
			break
		}
	}
	if idx == nil {
		if idx, ft, recs, err = buildIndex(name, paths, f, st, hdrLen); err != nil {
			return nil, err
		}
	}
	s := &spanReader{size: ft.USize}
	for _, r := range recs {
		s.starts = append(s.starts, int64(r.UOff))
	}
	s.open = func(i int) (io.Reader, error) {
		r := recs[i]
		win, err := readWindow(idx, r)
		if err != nil {
			return nil, err
		}
		c := hdrLen + int64(r.COff)
		return flate.Resume(bufio.NewReaderSize(io.NewSectionReader(raw, c, st.Size()-c), 64<<10), r.Bits, r.NBits, win), nil
	}
	return s, nil
}

// idxPaths lists where an index for the archive at path may live: next to it, else the user cache dir.
func idxPaths(path, ext string) []string {
	ps := []string{path + ext}
	if dir, err := os.UserCacheDir(); err == nil {
		// ponytail: same base name in two directories shares one cache entry (size/mtime check catches it), key by full path if that bites.
		ps = append(ps, filepath.Join(dir, "backup-mounter", filepath.Base(path)+ext))
	}
	return ps
}

// gzipHeaderLen parses a gzip member header (RFC 1952) and returns its length.
func gzipHeaderLen(r io.Reader) (int64, error) {
	br := bufio.NewReader(r)
	var h [10]byte
	if _, err := io.ReadFull(br, h[:]); err != nil {
		return 0, err
	}
	if h[2] != 8 {
		return 0, fmt.Errorf("gzip: unknown compression method %d", h[2])
	}
	n, flg := int64(10), h[3]
	if flg&0x04 != 0 { // FEXTRA
		var l [2]byte
		if _, err := io.ReadFull(br, l[:]); err != nil {
			return 0, err
		}
		k, err := br.Discard(int(binary.LittleEndian.Uint16(l[:])))
		if err != nil {
			return 0, err
		}
		n += 2 + int64(k)
	}
	for _, bit := range []byte{0x08, 0x10} { // FNAME, FCOMMENT: NUL-terminated
		if flg&bit != 0 {
			s, err := br.ReadBytes(0)
			if err != nil {
				return 0, err
			}
			n += int64(len(s))
		}
	}
	if flg&0x02 != 0 { // FHCRC
		n += 2
	}
	return n, nil
}

func loadIndex(p string, st os.FileInfo) (*os.File, idxFooter, []idxRecord, error) {
	var ft idxFooter
	idx, err := os.Open(p)
	if err != nil {
		return nil, ft, nil, err
	}
	ist, err := idx.Stat()
	if err == nil && ist.Size() >= 64 {
		err = binary.Read(io.NewSectionReader(idx, ist.Size()-64, 64), binary.LittleEndian, &ft)
	}
	if err == nil && (ist.Size() < 64 || ft.Magic != idxMagic || ft.ArchiveSize != st.Size() ||
		ft.ArchiveMtime != st.ModTime().UnixNano() || ft.N < 1 || ft.TableOff+ft.N*32+64 != ist.Size()) {
		err = errors.New("stale or foreign index")
	}
	var recs []idxRecord
	if err == nil {
		recs = make([]idxRecord, ft.N)
		err = binary.Read(io.NewSectionReader(idx, ft.TableOff, ft.N*32), binary.LittleEndian, recs)
	}
	if err != nil {
		idx.Close()
		return nil, ft, nil, err
	}
	return idx, ft, recs, nil
}

func readWindow(idx io.ReaderAt, r idxRecord) ([]byte, error) {
	b := make([]byte, r.WinLen)
	if _, err := idx.ReadAt(b, int64(r.WinOff)); err != nil {
		return nil, err
	}
	return io.ReadAll(stdflate.NewReader(bytes.NewReader(b)))
}

// buildIndex decompresses the whole archive once, writing a checkpoint every
// indexSpan bytes to the first of paths that can be created.
func buildIndex(name string, paths []string, f *os.File, st os.FileInfo, hdrLen int64) (*os.File, idxFooter, []idxRecord, error) {
	var ft idxFooter
	var out *os.File
	var p string
	var err error
	for _, p = range paths {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if out, err = os.Create(p + ".tmp"); err == nil {
			break
		}
	}
	if out == nil {
		return nil, ft, nil, fmt.Errorf("cannot create index: %w", err)
	}
	defer func() {
		if out != nil {
			out.Close()
			os.Remove(out.Name())
		}
	}()
	log.Printf("%s: building gzip index %s (one full decompression pass)", name, p)

	w := bufio.NewWriterSize(out, 1<<20)
	var recs []idxRecord
	var woff int64
	var werr error
	var comp bytes.Buffer
	zw, _ := stdflate.NewWriter(&comp, stdflate.BestSpeed)
	start, lastLog := time.Now(), time.Now()
	x := flate.NewIndexer(bufio.NewReaderSize(io.NewSectionReader(f, hdrLen, st.Size()-hdrLen), 4<<20), func(cp flate.Checkpoint) {
		if len(recs) > 0 && cp.Out-int64(recs[len(recs)-1].UOff) < indexSpan {
			return
		}
		comp.Reset()
		zw.Reset(&comp)
		zw.Write(cp.Window)
		zw.Close()
		if _, err := w.Write(comp.Bytes()); err != nil && werr == nil {
			werr = err
		}
		recs = append(recs, idxRecord{COff: uint64(cp.In), UOff: uint64(cp.Out), WinOff: uint64(woff), WinLen: uint32(comp.Len()), Bits: cp.Bits, NBits: cp.NBits})
		woff += int64(comp.Len())
		if time.Since(lastLog) >= 10*time.Second {
			lastLog = time.Now()
			log.Printf("%s: indexing %d%% (%d MiB out)", name, 100*(hdrLen+cp.In)/st.Size(), cp.Out>>20)
		}
	})
	crc := crc32.NewIEEE()
	usize, err := io.Copy(crc, x)
	if err != nil {
		return nil, ft, nil, err
	}
	if werr != nil {
		return nil, ft, nil, werr
	}
	end := hdrLen + x.In()
	var tr [8]byte
	if _, err := f.ReadAt(tr[:], end); err != nil {
		return nil, ft, nil, fmt.Errorf("gzip trailer: %w", err)
	}
	if end+8 != st.Size() {
		// ponytail: one gzip member only; multi-member (cat a.gz b.gz, bgzip) would need a checkpoint per member.
		return nil, ft, nil, errors.New("multi-member gzip not supported")
	}
	if binary.LittleEndian.Uint32(tr[:4]) != crc.Sum32() || binary.LittleEndian.Uint32(tr[4:]) != uint32(usize) {
		log.Printf("%s: gzip crc mismatch, contents may be corrupt", name)
	}
	ft = idxFooter{Magic: idxMagic, ArchiveSize: st.Size(), ArchiveMtime: st.ModTime().UnixNano(),
		USize: usize, Span: indexSpan, TableOff: woff, N: int64(len(recs))}
	if err := binary.Write(w, binary.LittleEndian, recs); err != nil {
		return nil, ft, nil, err
	}
	if err := binary.Write(w, binary.LittleEndian, &ft); err != nil {
		return nil, ft, nil, err
	}
	if err := w.Flush(); err != nil {
		return nil, ft, nil, err
	}
	if err := out.Close(); err != nil {
		return nil, ft, nil, err
	}
	if err := os.Rename(out.Name(), p); err != nil {
		return nil, ft, nil, err
	}
	out = nil
	log.Printf("%s: index written to %s, %d checkpoints, %d MiB, %s", name, p, len(recs), usize>>20, time.Since(start).Round(time.Second))
	idx, err := os.Open(p)
	return idx, ft, recs, err
}
