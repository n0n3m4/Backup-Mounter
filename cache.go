package main

import (
	"container/list"
	"io"
	"sync"
)

// chunkCache is an io.ReaderAt that reads its source in fixed-size chunks and
// keeps them in an LRU bounded by a byte budget. Concurrent readers of the same
// missing chunk wait for one fetch instead of each issuing their own.
// ponytail: one global mutex; shard by chunk index if lock contention shows in profiles. Prefetch would plug in here.
type chunkCache struct {
	r      io.ReaderAt
	size   int64
	chunk  int64
	budget int64

	mu   sync.Mutex
	m    map[int64]*cacheEntry
	lru  *list.List // of *cacheEntry, front = most recently used
	used int64
}

type cacheEntry struct {
	idx  int64
	data []byte
	err  error
	done chan struct{} // closed once data/err are set
	el   *list.Element // nil while the fetch is in flight
}

func newChunkCache(r io.ReaderAt, size, chunk, budget int64) *chunkCache {
	return &chunkCache{r: r, size: size, chunk: chunk, budget: budget, m: map[int64]*cacheEntry{}, lru: list.New()}
}

func (c *chunkCache) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		if pos >= c.size {
			return n, io.EOF
		}
		data, err := c.get(pos / c.chunk)
		if err != nil {
			return n, err
		}
		n += copy(p[n:], data[pos%c.chunk:])
	}
	return n, nil
}

func (c *chunkCache) get(idx int64) ([]byte, error) {
	c.mu.Lock()
	if e, ok := c.m[idx]; ok {
		if e.el != nil {
			c.lru.MoveToFront(e.el)
		}
		c.mu.Unlock()
		<-e.done
		return e.data, e.err
	}
	e := &cacheEntry{idx: idx, done: make(chan struct{})}
	c.m[idx] = e
	c.mu.Unlock()

	buf := make([]byte, min(c.chunk, c.size-idx*c.chunk))
	n, err := c.r.ReadAt(buf, idx*c.chunk)
	if n == len(buf) {
		err = nil
	} else if err == nil {
		err = io.ErrUnexpectedEOF
	}
	e.data, e.err = buf, err

	c.mu.Lock()
	if err != nil {
		delete(c.m, idx) // don't cache failures; the next reader retries
	} else {
		e.el = c.lru.PushFront(e)
		c.used += int64(len(buf))
		for c.used > c.budget {
			old := c.lru.Remove(c.lru.Back()).(*cacheEntry)
			delete(c.m, old.idx)
			c.used -= int64(len(old.data))
		}
	}
	c.mu.Unlock()
	close(e.done)
	return e.data, e.err
}
