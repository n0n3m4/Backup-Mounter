package main

import (
	"bytes"
	"io"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// A read far into a span caches the whole chunks the live decoder passes on the way.
func TestSpanGapFill(t *testing.T) {
	src := make([]byte, 10<<20)
	rand.New(rand.NewSource(2)).Read(src)
	s := &spanReader{size: int64(len(src)), starts: []int64{0},
		open: func(int) (io.Reader, error) { return bytes.NewReader(src), nil }}
	s.cache = newChunkCache(s, s.size, 1<<20, 64<<20)
	p := make([]byte, 100)
	if _, err := s.cache.ReadAt(p, 5<<20+7); err != nil || !bytes.Equal(p, src[5<<20+7:][:100]) {
		t.Fatalf("ReadAt: %v", err)
	}
	for i := int64(0); i <= 5; i++ {
		e, ok := s.cache.m[i]
		if !ok {
			t.Fatalf("chunk %d not cached", i)
		}
		if !bytes.Equal(e.data, src[i<<20:(i+1)<<20]) {
			t.Fatalf("chunk %d: wrong bytes", i)
		}
	}
}

// A sequential read (chunk 0, then 1) prefetches the following chunks in the background, one
// burst capped at the semaphore's NumCPU slots (later gets top up), with the right bytes.
func TestChunkCacheReadAhead(t *testing.T) {
	const chunk = 4096
	src := make([]byte, 64*chunk)
	rand.New(rand.NewSource(3)).Read(src)
	c := newChunkCache(bytes.NewReader(src), int64(len(src)), chunk, 64*chunk).withAhead()
	if c.ahead != 32 {
		t.Fatalf("ahead = %d, want 32 (half the budget)", c.ahead)
	}
	if _, err := c.ReadAt(make([]byte, 10), chunk-5); err != nil { // chunks 0 and 1: sequential
		t.Fatal(err)
	}
	want := min(c.ahead, int64(cap(prefetchSem)))
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
		c.mu.Lock()
		n := int64(0)
		for j := int64(1); j <= want; j++ {
			if e, ok := c.m[j]; ok && e.el != nil {
				n++
			}
		}
		c.mu.Unlock()
		if n == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d chunks prefetched after 2 s", n, want)
		}
	}
	c.mu.Lock()
	for j := int64(1); j <= want; j++ {
		if !bytes.Equal(c.m[j].data, src[j*chunk:(j+1)*chunk]) {
			t.Fatalf("chunk %d: wrong bytes", j)
		}
	}
	if _, ok := c.m[c.ahead+2]; ok {
		t.Fatalf("chunk %d prefetched beyond ahead", c.ahead+2)
	}
	c.mu.Unlock()
	// A lone read (previous chunk not cached) reads nothing ahead.
	if _, err := c.ReadAt(make([]byte, 10), 50*chunk); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if c.missing(50) || !c.missing(51) {
		t.Fatal("non-sequential read triggered read-ahead")
	}
}
