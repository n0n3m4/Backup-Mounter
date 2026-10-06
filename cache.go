package main

import (
	"container/list"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"weak"
)

// prefetchSem bounds the background read-ahead fills of all caches together.
var prefetchSem = make(chan struct{}, runtime.NumCPU())

// readAheadBytes is the decode-ahead window per file (--readahead); 0 turns read-ahead off.
var readAheadBytes int64 = 16 << 20

// activeReads counts WinFsp reads in progress (mount_windows.go), for the --debug memory line.
var activeReads atomic.Int64

// logFile is the --log destination (nil = console); mount_windows.go also hands it to WinFsp.
var logFile *os.File

// watchdogLimit (--watchdog): a WinFsp read stuck this long makes readWatchdog unmount and exit,
// so a frozen mount never needs the reset button; 0 disables.
var watchdogLimit time.Duration

// chunkCache is an io.ReaderAt that reads its source in fixed-size chunks and
// keeps them in an LRU bounded by a byte budget. Concurrent readers of the same
// missing chunk wait for one fetch instead of each issuing their own. A cache made
// withAhead reads the next chunks ahead of every get in the background (see readAhead).
// ponytail: one global mutex; shard by chunk index if lock contention shows in profiles.
type chunkCache struct {
	r      io.ReaderAt
	size   int64
	chunk  int64
	budget int64

	ahead int64 // chunks to read ahead of each get; 0 = off

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
	c := &chunkCache{r: r, size: size, chunk: chunk, budget: budget, m: map[int64]*cacheEntry{}, lru: list.New()}
	cachesMu.Lock()
	caches = append(caches, weak.Make(c))
	cachesMu.Unlock()
	return c
}

// caches lists every live cache, for the memory reports. Weak, so a dropped cache
// (each test open makes new ones) is still collected.
var (
	cachesMu sync.Mutex
	caches   []weak.Pointer[chunkCache]
)

// cacheReport is " used/budget MiB (entries, in flight)" for every cache.
func cacheReport() string {
	var s string
	cachesMu.Lock()
	defer cachesMu.Unlock()
	live := caches[:0]
	for _, w := range caches {
		c := w.Value()
		if c == nil {
			continue
		}
		live = append(live, w)
		c.mu.Lock()
		s += fmt.Sprintf(" %d/%d MiB (%d entries, %d in flight)", c.used>>20, c.budget>>20, c.lru.Len(), len(c.m)-c.lru.Len())
		c.mu.Unlock()
	}
	caches = live
	return s
}

// withAhead turns on read-ahead of readAheadBytes (1..1024 chunks, at most half the budget so
// prefetched chunks never evict the one being read). Only for sources that decode
// independent blocks, where it buys CPU cores: over a plain file it would be extra IO,
// over a spanReader there is only one decoder to run.
func (c *chunkCache) withAhead() *chunkCache {
	if readAheadBytes > 0 {
		c.ahead = min(max(readAheadBytes/c.chunk, 1), 1024, c.budget/c.chunk/2)
	}
	return c
}

// newCache is a bare key -> bytes LRU for fetch; its ReadAt is never used.
func newCache(budget int64) *chunkCache { return newChunkCache(nil, 0, 1, budget) }

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
	data, err := c.load(idx)
	if err == nil {
		c.readAhead(idx)
	}
	return data, err
}

// readAhead starts one background load per missing chunk idx+1..idx+ahead while prefetchSem has room,
// once the access looks sequential (chunk idx-1 is cached): scattered metadata reads decode nothing extra.
// ponytail: "previous chunk cached" as the sequential test; track per-stream positions if it misfires.
func (c *chunkCache) readAhead(idx int64) {
	if c.ahead == 0 || idx == 0 || c.missing(idx-1) {
		return
	}
	for j := idx + 1; j <= min(idx+c.ahead, (c.size-1)/c.chunk); j++ {
		if !c.missing(j) {
			continue
		}
		select {
		case prefetchSem <- struct{}{}:
		default:
			return
		}
		go func() { defer func() { <-prefetchSem }(); c.load(j) }()
	}
}

// missing reports whether chunk j is neither cached nor in flight.
func (c *chunkCache) missing(j int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.m[j]
	return !ok
}

// load is get without the read-ahead.
func (c *chunkCache) load(idx int64) ([]byte, error) {
	return c.fetch(idx, func() ([]byte, error) {
		buf := make([]byte, min(c.chunk, c.size-idx*c.chunk))
		n, err := c.r.ReadAt(buf, idx*c.chunk)
		if n == len(buf) {
			err = nil
		} else if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return buf, err
	})
}

// fetch returns the entry for key, calling fill once on a miss (concurrent callers wait for it).
// Entries may have any length; the budget counts their bytes.
func (c *chunkCache) fetch(key int64, fill func() ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	if e, ok := c.m[key]; ok {
		if e.el != nil {
			c.lru.MoveToFront(e.el)
		}
		c.mu.Unlock()
		<-e.done
		return e.data, e.err
	}
	e := &cacheEntry{idx: key, done: make(chan struct{})}
	c.m[key] = e
	c.mu.Unlock()

	e.data, e.err = fill()

	c.mu.Lock()
	if e.err != nil {
		delete(c.m, key) // don't cache failures; the next reader retries
	} else {
		c.insert(e)
	}
	c.mu.Unlock()
	close(e.done)
	return e.data, e.err
}

// put caches a chunk somebody else already decoded (spanReader's gap fill); a chunk present or in flight wins.
func (c *chunkCache) put(idx int64, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[idx]; ok {
		return
	}
	e := &cacheEntry{idx: idx, data: data, done: make(chan struct{})}
	close(e.done)
	c.m[idx] = e
	c.insert(e)
}

// insert adds a filled entry to the LRU and evicts down to the budget. c.mu held.
func (c *chunkCache) insert(e *cacheEntry) {
	e.el = c.lru.PushFront(e)
	c.used += int64(len(e.data))
	for c.used > c.budget {
		old := c.lru.Remove(c.lru.Back()).(*cacheEntry)
		delete(c.m, old.idx)
		c.used -= int64(len(old.data))
	}
}
