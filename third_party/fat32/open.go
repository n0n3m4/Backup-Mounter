package filesystem_fat32

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	filesystem "github.com/go-filesystems/interface"
	"github.com/go-volumes/safeio"
)

// Verify implementation of the optional read-at-an-offset interface.
var _ filesystem.Opener = (*fat32FS)(nil)

// fat32File is an open regular file on a FAT32 volume, backing
// filesystem.File.
//
// FAT32 has no extents: a file is a singly-linked list of fixed-size clusters
// threaded through the FAT, and the only way to learn where byte N lives is to
// walk that list from the start. Walking it on every ReadAt would make random
// access quadratic, so the walk happens exactly once, at OpenFile, and what it
// materialises is the CLUSTER NUMBERS — not the data. That is the whole point
// of the type: a 4 GiB file with 4 KiB clusters costs 4 MiB of uint32s to
// address, instead of 4 GiB of contents to read.
//
// The walk itself is the same one readClusterChain performs, with the same
// safeio guards against forged images (cycles, over-long chains), and reads go
// through the same *fat32FS block layer. Nothing here bypasses either.
//
// The chain and the size are settled at OpenFile and change only when a write
// through this File extends or truncates it (see writeat.go), so mu — an
// RWMutex — is held for reading by ReadAt and for writing by WriteAt,
// Truncate and Sync. Concurrent ReadAt calls therefore proceed in parallel, as
// io.ReaderAt requires; concurrent writes are serialised, which is stricter
// than io.WriterAt demands and never wrong. The one lock-free field, closed,
// is atomic so a use-after-close is reported rather than raced on.
type fat32File struct {
	fs *fat32FS
	// path is the file's path in the volume. It is kept because extending or
	// truncating the file has to rewrite the directory entry — FAT stores a
	// file's length there and nowhere else — and there is no back-pointer
	// from a cluster to the entry that owns it.
	path string
	// mu guards clusters and size against a concurrent extend or truncate.
	mu sync.RWMutex
	// clusters holds the file's chain in order: clusters[i] is the cluster
	// number holding bytes [i*clusterSize, (i+1)*clusterSize).
	clusters []uint32
	// size is the readable length in bytes. It is the directory entry's
	// FileSize clamped to what the chain can actually address, so it always
	// equals len(fs.ReadFile(path)) — a truncated or forged chain shortens
	// the file rather than inventing zeros.
	size        int64
	clusterSize int64
	dataBase    int64
	closed      atomic.Bool
}

var _ filesystem.File = (*fat32File)(nil)

// OpenFile opens the regular file at path for random access.
//
// It resolves the path and walks the FAT chain to build the cluster list, but
// reads none of the file's contents: the cost is one FAT entry read per
// cluster, not one data cluster read per cluster. Directories and "/" are
// rejected with the same message ReadFile uses for them.
func (fs *fat32FS) OpenFile(path string) (filesystem.File, error) {
	if path == "/" {
		return nil, fmt.Errorf("fat32: %q is not a regular file", path)
	}
	entry, _, err := fs.resolvePath(path)
	if err != nil {
		return nil, err
	}
	if entry.attr&fatAttrDirectory != 0 {
		return nil, fmt.Errorf("fat32: %q is not a regular file", path)
	}
	clusters, size, err := fs.chainClusters(entry.cluster, uint64(entry.size))
	if err != nil {
		return nil, err
	}
	return &fat32File{
		fs:          fs,
		path:        path,
		clusters:    clusters,
		size:        size,
		clusterSize: int64(fs.info.ClusterSize()),
		dataBase:    fs.info.DataOffset(fs.partOffset),
	}, nil
}

// chainClusters walks the FAT chain starting at start and returns the cluster
// numbers covering up to size bytes, together with the number of bytes those
// clusters actually address.
//
// It mirrors readClusterChain's loop exactly — same termination conditions,
// same safeio.LoopGuard and safeio.VisitSet bounding a forged chain, same
// clamp of an attacker-controlled FileSize to the volume's real capacity — but
// reads only FAT entries, never data. Keeping the two walks in step is what
// guarantees OpenFile+ReadAt and ReadFile agree byte for byte, including on
// images where the chain ends before FileSize says it should.
func (fs *fat32FS) chainClusters(start uint32, size uint64) ([]uint32, int64, error) {
	if start == 0 {
		return nil, 0, nil
	}
	clusterSize := int64(fs.info.ClusterSize())
	fatBase := fs.info.FATOffset(fs.partOffset)

	maxBytes := fs.clusterCount() * clusterSize
	if size > uint64(maxBytes) {
		size = uint64(maxBytes)
	}

	var clusters []uint32
	var covered int64
	guard := safeio.NewLoopGuard(chainGuardLimit(fs))
	var visited safeio.VisitSet
	cluster := start
	for {
		if cluster < 2 || cluster >= 0x0FFFFFF7 {
			break
		}
		if uint64(covered) >= size {
			break
		}
		if err := guard.Next(); err != nil {
			return nil, 0, fmt.Errorf("fat32: cluster chain from %d: %w", start, err)
		}
		if err := visited.Check(uint64(cluster)); err != nil {
			return nil, 0, fmt.Errorf("fat32: cluster chain from %d: %w", start, err)
		}
		clusters = append(clusters, cluster)
		covered += clusterSize
		var nextEntry [4]byte
		if _, err := fs.f.ReadAt(nextEntry[:], fatBase+int64(cluster)*4); err != nil {
			return nil, 0, fmt.Errorf("fat32: read FAT entry for cluster %d: %w", cluster, err)
		}
		next := binary.LittleEndian.Uint32(nextEntry[:]) & 0x0FFFFFFF
		if next >= 0x0FFFFFF8 {
			break
		}
		cluster = next
	}
	if covered > int64(size) {
		covered = int64(size)
	}
	return clusters, covered, nil
}

// Size returns the file's readable length in bytes: the directory entry's
// FileSize read at OpenFile, clamped to what the chain addresses, and then
// tracking every extend or truncate performed through this File. No I/O.
func (f *fat32File) Size() int64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.size
}

// Close releases the File. FAT32 files hold no per-file handle — the volume's
// single descriptor stays owned by the Filesystem — so Close only marks the
// File unusable, which turns a use-after-close into a clear os.ErrClosed
// instead of a silent read of stale cluster numbers. It is idempotent.
func (f *fat32File) Close() error {
	f.closed.Store(true)
	return nil
}

// ReadAt implements io.ReaderAt to the letter, which is the contract every
// generic consumer (io.SectionReader above all) silently depends on:
//
//   - it fills p completely and returns a nil error whenever the bytes exist;
//   - it returns n < len(p) only together with a non-nil error;
//   - a read that runs into the end of the file returns io.EOF, with the
//     bytes it did get, and an offset at or past Size() returns 0, io.EOF.
//
// The loop translates a byte offset into (cluster index, offset within
// cluster) by division — possible only because the chain was resolved up
// front — and issues one ReadAt per cluster crossed, never more than the
// caller asked for. Reads go through fs.f, the same block layer every other
// path in this package uses; concurrent calls are safe because nothing here
// mutates shared state and *os.File.ReadAt is itself concurrent-safe.
func (f *fat32File) ReadAt(p []byte, off int64) (int, error) {
	if f.closed.Load() {
		return 0, os.ErrClosed
	}
	if off < 0 {
		return 0, fmt.Errorf("fat32: ReadAt: negative offset %d", off)
	}
	// The read lock keeps a read from observing a half-applied extend; it is
	// shared, so parallel ReadAt calls are not serialised against each other.
	f.mu.RLock()
	defer f.mu.RUnlock()
	if off >= f.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) {
		cur := off + int64(n)
		if cur >= f.size {
			return n, io.EOF
		}
		idx := cur / f.clusterSize
		within := cur % f.clusterSize
		chunk := f.clusterSize - within
		if rem := f.size - cur; chunk > rem {
			chunk = rem
		}
		if want := int64(len(p) - n); chunk > want {
			chunk = want
		}
		diskOff := f.dataBase + int64(f.clusters[idx]-2)*f.clusterSize + within
		m, err := f.fs.f.ReadAt(p[n:n+int(chunk)], diskOff)
		n += m
		if err != nil {
			return n, fmt.Errorf("fat32: read cluster %d: %w", f.clusters[idx], err)
		}
	}
	return n, nil
}
