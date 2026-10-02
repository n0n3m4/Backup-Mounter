package flate

import (
	"bufio"
	"bytes"
	stdflate "compress/flate"
	"io"
	"math/rand"
	"testing"
)

// Index 8 MiB of mixed zero/random/text data and resume at every checkpoint.
func TestResume(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var src []byte
	for len(src) < 8<<20 {
		chunk := make([]byte, 1+rng.Intn(300<<10))
		switch rng.Intn(3) {
		case 0: // zeros
		case 1:
			rng.Read(chunk) // incompressible: stored blocks
		case 2:
			for i := range chunk {
				chunk[i] = "abcdefgh \n"[rng.Intn(10)]
			}
		}
		src = append(src, chunk...)
	}
	var comp bytes.Buffer
	w, _ := stdflate.NewWriter(&comp, stdflate.DefaultCompression)
	w.Write(src)
	w.Close()
	comp.WriteString("TRAILER")

	var cps []Checkpoint
	var last int64 = -1 << 62
	x := NewIndexer(bufio.NewReader(bytes.NewReader(comp.Bytes())), func(cp Checkpoint) {
		if cp.Out-last >= 64<<10 {
			cps, last = append(cps, cp), cp.Out
		}
	})
	out, err := io.ReadAll(x)
	if err != nil || !bytes.Equal(out, src) {
		t.Fatalf("indexer output differs (err %v)", err)
	}
	if got := comp.Bytes()[x.In():]; string(got) != "TRAILER" {
		t.Fatalf("In() = %d, trailing %q", x.In(), got)
	}
	if len(cps) < 20 {
		t.Fatalf("only %d checkpoints", len(cps))
	}
	for _, cp := range cps {
		if cp.NBits > 7 || int64(len(cp.Window)) != min(cp.Out, 32<<10) {
			t.Fatalf("bad checkpoint %+v", cp)
		}
		r := Resume(bufio.NewReader(bytes.NewReader(comp.Bytes()[cp.In:])), cp.Bits, cp.NBits, cp.Window)
		tail, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(tail, src[cp.Out:]) {
			t.Fatalf("resume at in=%d out=%d: err %v, %d bytes, want %d", cp.In, cp.Out, err, len(tail), len(src)-int(cp.Out))
		}
	}
	t.Logf("%d checkpoints", len(cps))
}
