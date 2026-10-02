package flate

import "io"

// Fork additions: the two constants inflate.go needs from the dropped writer
// files, and checkpoint/resume for random access into a DEFLATE stream.

const (
	maxMatchOffset = 1 << 15 // deflate.go
	endBlockMarker = 256     // huffman_bit_writer.go
)

// Checkpoint describes a deflate block boundary: the next block starts at
// input byte In, after NBits already-consumed bits of that stream (value Bits,
// low bit first); Out bytes have been produced so far; Window is the last
// ≤32 KiB of output (what the next block may reference).
type Checkpoint struct {
	In          int64
	Bits, NBits uint8
	Out         int64
	Window      []byte
}

// checkpoint is called at nextBlock entry: toRead is drained (Read only steps
// when it is), so the history already holds every byte produced so far.
func (f *decompressor) checkpoint() Checkpoint {
	d := &f.dict
	var w []byte
	if d.full {
		w = append(append(w, d.hist[d.wrPos:]...), d.hist[:d.wrPos]...)
	} else {
		w = append(w, d.hist[:d.wrPos]...)
	}
	return Checkpoint{
		In:     f.roffset,
		Bits:   uint8(f.b & (1<<f.nb - 1)),
		NBits:  uint8(f.nb),
		Out:    f.woffset + int64(d.availRead()),
		Window: w,
	}
}

// Indexer decompresses like NewReader and calls fn at every block boundary
// (not after the final block). r must be an io.ByteReader so In is exact.
type Indexer struct{ f *decompressor }

func NewIndexer(r Reader, fn func(Checkpoint)) *Indexer {
	f := NewReader(r).(*decompressor)
	f.onBlock = fn
	return &Indexer{f}
}

func (x *Indexer) Read(p []byte) (int, error) { return x.f.Read(p) }
func (x *Indexer) Close() error               { return x.f.Close() }

// In is the number of input bytes consumed; after io.EOF it is the offset of
// the first byte after the deflate stream (e.g. a gzip trailer).
func (x *Indexer) In() int64 { return x.f.roffset }

// Resume returns a decoder that starts at a Checkpoint: r positioned at
// Checkpoint.In, plus its Bits/NBits and Window.
func Resume(r Reader, bits, nbits uint8, window []byte) io.ReadCloser {
	f := NewReaderDict(r, window).(*decompressor)
	f.b, f.nb = uint32(bits), uint(nbits)
	return f
}
