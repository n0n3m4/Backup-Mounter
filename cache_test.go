package main

import (
	"bytes"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
)

type countingReader struct {
	*bytes.Reader
	reads atomic.Int64
}

func (r *countingReader) ReadAt(p []byte, off int64) (int, error) {
	r.reads.Add(1)
	return r.Reader.ReadAt(p, off)
}

func TestChunkCache(t *testing.T) {
	src := make([]byte, 1<<20+12345) // not a multiple of the chunk size
	rand.New(rand.NewSource(1)).Read(src)
	under := &countingReader{Reader: bytes.NewReader(src)}
	const chunk, budget = 4096, 10 * 4096
	c := newChunkCache(under, int64(len(src)), chunk, budget)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for i := 0; i < 2000; i++ {
				off := rng.Int63n(int64(len(src)))
				p := make([]byte, rng.Intn(3*chunk))
				n, err := c.ReadAt(p, off)
				want := src[off:min(off+int64(len(p)), int64(len(src)))]
				if n != len(want) || !bytes.Equal(p[:n], want) {
					t.Errorf("ReadAt(%d, %d) = %d, %v: data mismatch", len(p), off, n, err)
					return
				}
				if n < len(p) && err == nil {
					t.Errorf("short read without error at %d", off)
				}
				c.mu.Lock()
				used := c.used
				c.mu.Unlock()
				if used > budget {
					t.Errorf("cache uses %d bytes, budget %d", used, budget)
				}
			}
		}(int64(g))
	}
	wg.Wait()

	// Repeated reads of one chunk hit the cache: exactly one underlying read.
	c2 := newChunkCache(under, int64(len(src)), chunk, budget)
	before := under.reads.Load()
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c2.ReadAt(make([]byte, 100), 8192+10) }()
	}
	wg.Wait()
	if got := under.reads.Load() - before; got != 1 {
		t.Errorf("50 concurrent reads of one chunk did %d underlying reads, want 1", got)
	}
}
