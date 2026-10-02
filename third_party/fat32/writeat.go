package filesystem_fat32

import (
	"encoding/binary"
	"fmt"
	iofs "io/fs"
	"os"

	filesystem "github.com/go-filesystems/interface"
)

// Verify implementation of the optional write-at-an-offset interface.
//
// The assertion is on the File, not on the filesystem: writability is a
// property of the opened object, and this is what a caller's
// `f.(filesystem.WritableFile)` probe finds.
var _ filesystem.WritableFile = (*fat32File)(nil)

// maxFAT32FileSize is the largest file FAT32 can describe: the directory
// entry's FileSize field is 32 bits, so 4 GiB − 1 is not a policy choice but
// the format's ceiling. A write that would cross it is refused rather than
// silently truncated by the uint32 conversion when the entry is rewritten.
const maxFAT32FileSize = int64(1<<32) - 1

// fatEntriesPerScanRead is how many FAT entries allocClusterRun pulls per
// ReadAt: 4 KiB of FAT, i.e. one page.
//
// The driver's allocCluster reads FOUR BYTES per candidate cluster, so finding
// a free cluster near the end of a large FAT costs one syscall per cluster
// scanned, and allocating k clusters costs k such scans. Writing a file in
// blocks turns that into the dominant cost — it is a second quadratic hiding
// behind the one this file exists to remove. Scanning a page at a time and
// taking the whole run in one pass fixes both halves at once, and reads the
// same bytes with the same meaning.
const fatEntriesPerScanRead = 1024

// WriteAt writes len(p) bytes at off, in place.
//
// This is the method the whole type exists for. Before it, a positional write
// on FAT32 could only be expressed as ReadFile + splice + WriteFile: the whole
// file read, the whole file reallocated, the whole file written back, for every
// request. Here the chain resolved at OpenFile turns an offset into (cluster
// index, offset within cluster) by division, and the cost is the bytes the
// caller actually asked for, plus one cluster allocation run when the file has
// to grow.
//
// It follows io.WriterAt to the letter: it writes all of p or returns a
// non-nil error, and it never reports a short write with a nil error, which a
// caller reads as success. It DOES extend the file — an offset past the
// current end is legal, and the gap between the old end and off reads back as
// zeros, exactly as ReadFile would report it, because the clusters covering
// the gap are zero-filled as they are allocated and the slack in the old last
// cluster is zeroed too.
//
// Size follows immediately: a WritableFile is a handle the caller mutates, not
// the snapshot a read-only File is, and the directory entry is rewritten
// before WriteAt returns so a Stat through the Filesystem agrees as well.
//
// Concurrency: the file's own lock is held exclusively, so concurrent WriteAt
// calls are serialised — stricter than io.WriterAt requires, and correct for
// overlapping ranges too. It says nothing about another handle on the same
// volume: as everywhere else in this driver, two Filesystem-level writers are
// the caller's problem, and this File's lock cannot see them.
func (f *fat32File) WriteAt(p []byte, off int64) (int, error) {
	if f.closed.Load() {
		return 0, os.ErrClosed
	}
	if off < 0 {
		return 0, fmt.Errorf("fat32: WriteAt: negative offset %d", off)
	}
	if len(p) == 0 {
		return 0, nil
	}
	// off is caller-supplied and len(p) can be large; compute the end before
	// anything derives a cluster index from it, so an overflow becomes an
	// error rather than a negative offset far inside the volume.
	end := off + int64(len(p))
	if end < off || end > maxFAT32FileSize {
		return 0, fmt.Errorf("fat32: WriteAt: offset %d + %d bytes exceeds the 4 GiB FAT32 file limit", off, len(p))
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if end > f.size {
		if err := f.resizeLocked(end); err != nil {
			return 0, err
		}
	}

	n := 0
	for n < len(p) {
		cur := off + int64(n)
		idx := cur / f.clusterSize
		within := cur % f.clusterSize
		chunk := f.clusterSize - within
		if want := int64(len(p) - n); chunk > want {
			chunk = want
		}
		diskOff := f.dataBase + int64(f.clusters[idx]-2)*f.clusterSize + within
		m, err := f.fs.f.WriteAt(p[n:n+int(chunk)], diskOff)
		n += m
		if err != nil {
			return n, fmt.Errorf("fat32: write cluster %d: %w", f.clusters[idx], err)
		}
	}
	return n, nil
}

// Truncate resizes the file to size bytes.
//
// Growing extends it with zeros: FAT32 has no sparse representation, so the
// new clusters are really allocated and really written as zeros, and a caller
// must not read a successful grow as "free". Shrinking frees the clusters past
// the new end and zeroes the slack in the last one kept, so a later grow reads
// zeros rather than the bytes that used to be there — the same rule the
// path-scoped Truncate on the Filesystem follows.
//
// The directory entry is rewritten before Truncate returns, so Size and a
// Filesystem-level Stat agree at once.
func (f *fat32File) Truncate(size int64) error {
	if f.closed.Load() {
		return os.ErrClosed
	}
	if size < 0 {
		return fmt.Errorf("fat32: Truncate: negative size %d", size)
	}
	if size > maxFAT32FileSize {
		return fmt.Errorf("fat32: Truncate: size %d exceeds the 4 GiB FAT32 file limit", size)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if size == f.size {
		return nil
	}
	return f.resizeLocked(size)
}

// Sync reports whether everything written through this File has reached the
// backing store.
//
// What it can promise depends on what the volume was opened over, and saying
// so is the point of the method: this driver buffers NOTHING of its own —
// WriteAt has already issued the write to the image before it returns, and the
// directory entry with it — so Sync's whole job is to push the layer beneath.
// When that layer has a Sync (an *os.File, which is the normal case: Open uses
// os.OpenFile, and this is fsync(2)) it is called and its error returned. When
// it has none — an in-memory image in a test, a caller's own io.WriterAt — the
// data is already as durable as that layer makes it and Sync returns nil,
// having done nothing, because there is nothing left to do rather than because
// the guarantee was quietly dropped.
//
// A server answering NFSv3 COMMIT can therefore report FILE_SYNC honestly on a
// file-backed image, which is the case that matters.
func (f *fat32File) Sync() error {
	if f.closed.Load() {
		return os.ErrClosed
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if s, ok := f.fs.f.(interface{ Sync() error }); ok {
		if err := s.Sync(); err != nil {
			return fmt.Errorf("fat32: sync image: %w", err)
		}
	}
	return nil
}

// resizeLocked changes the file's length to newSize, allocating or freeing
// clusters and rewriting the directory entry. f.mu must be held for writing.
//
// It is the O(clusters added or freed) counterpart of the Filesystem's
// path-scoped Truncate, which walks the chain from its head on every call
// because it has nothing else to go on. Here the chain is already in memory,
// so the tail is f.clusters[len-1] and no walk is needed — that difference is
// what turns a sequence of appending writes from quadratic into linear.
func (f *fat32File) resizeLocked(newSize int64) error {
	oldClusters := int64(len(f.clusters))
	newClusters := (newSize + f.clusterSize - 1) / f.clusterSize

	switch {
	case newClusters > oldClusters:
		if err := f.growLocked(newClusters); err != nil {
			return err
		}
	case newClusters < oldClusters:
		if err := f.shrinkLocked(newClusters); err != nil {
			return err
		}
	}

	// Zero the slack between the old end of file and the end of the cluster
	// holding it. Growing past it must read as zeros, not as whatever the
	// cluster held when it was last used for something else.
	if newSize > f.size && oldClusters > 0 {
		if slack := f.size % f.clusterSize; slack != 0 {
			last := f.clusters[oldClusters-1]
			zeros := make([]byte, f.clusterSize-slack)
			off := f.dataBase + int64(last-2)*f.clusterSize + slack
			if _, err := f.fs.f.WriteAt(zeros, off); err != nil {
				return fmt.Errorf("fat32: zero slack in cluster %d: %w", last, err)
			}
		}
	}
	// Shrinking leaves the tail of the last retained cluster readable through
	// the raw image; zero it for the same reason the path-scoped Truncate
	// does, so a later grow cannot resurrect it.
	if newSize < f.size && newClusters > 0 {
		if slack := newSize % f.clusterSize; slack != 0 {
			last := f.clusters[newClusters-1]
			zeros := make([]byte, f.clusterSize-slack)
			off := f.dataBase + int64(last-2)*f.clusterSize + slack
			if _, err := f.fs.f.WriteAt(zeros, off); err != nil {
				return fmt.Errorf("fat32: zero slack in cluster %d: %w", last, err)
			}
		}
	}

	var first uint32
	if len(f.clusters) > 0 {
		first = f.clusters[0]
	}
	if err := f.fs.setDirEntryExtent(f.path, first, newSize); err != nil {
		return err
	}
	f.size = newSize
	return nil
}

// growLocked extends the chain to newClusters clusters. f.mu must be held for
// writing.
//
// The whole run is allocated in one FAT scan, zero-filled, and linked; on any
// failure every cluster taken is put back, so a failed grow leaves the FAT as
// it found it rather than leaking clusters no file references.
func (f *fat32File) growLocked(newClusters int64) error {
	need := int(newClusters - int64(len(f.clusters)))
	run, err := f.fs.allocClusterRun(need)
	if err != nil {
		return err
	}
	rollback := func() {
		for _, c := range run {
			_ = f.fs.setFATEntry(c, 0)
		}
	}
	// Claim each cluster before writing to it: an unmarked cluster is free,
	// and a second allocation in the same volume would hand it out again.
	for _, c := range run {
		if err := f.fs.setFATEntry(c, 0x0FFFFFFF); err != nil {
			rollback()
			return err
		}
	}
	zero := make([]byte, f.clusterSize)
	for _, c := range run {
		if _, err := f.fs.f.WriteAt(zero, f.dataBase+int64(c-2)*f.clusterSize); err != nil {
			rollback()
			return fmt.Errorf("fat32: zero new cluster %d: %w", c, err)
		}
	}
	// Link the run internally, then onto the existing tail. Doing it in this
	// order means the file's chain is never briefly longer than its recorded
	// size: the run is unreachable from the file until the last link lands.
	for i := 0; i < len(run)-1; i++ {
		if err := f.fs.setFATEntry(run[i], run[i+1]); err != nil {
			rollback()
			return err
		}
	}
	if n := len(f.clusters); n > 0 {
		if err := f.fs.setFATEntry(f.clusters[n-1], run[0]); err != nil {
			rollback()
			return err
		}
	}
	f.clusters = append(f.clusters, run...)
	return nil
}

// shrinkLocked cuts the chain down to newClusters clusters, freeing the rest.
// f.mu must be held for writing. newClusters == 0 frees the whole chain and
// leaves the file with no first cluster, which is how FAT spells "empty".
func (f *fat32File) shrinkLocked(newClusters int64) error {
	drop := f.clusters[newClusters:]
	if newClusters > 0 {
		// Terminate the retained chain FIRST. If freeing then fails partway,
		// the file still describes exactly the clusters it claims; the worst
		// outcome is clusters marked in use that nothing references, which
		// fsck reports and repairs. The reverse order would leave the file
		// pointing at clusters marked free — the failure that loses data.
		if err := f.fs.setFATEntry(f.clusters[newClusters-1], 0x0FFFFFFF); err != nil {
			return err
		}
	}
	for _, c := range drop {
		if err := f.fs.setFATEntry(c, 0); err != nil {
			return err
		}
	}
	f.clusters = f.clusters[:newClusters]
	return nil
}

// allocClusterRun returns n free cluster numbers, scanning the FAT once.
//
// It is the batching counterpart of allocCluster, which reads one 4-byte entry
// per syscall and restarts from cluster 2 on every call: allocating k clusters
// that way costs O(k · clusterCount) reads, which for a file written in blocks
// is quadratic in the file's size on its own. This reads a page of FAT at a
// time and collects the whole run in one pass. The entries mean exactly what
// allocCluster takes them to mean — the low 28 bits zero is "free".
//
// The scan is bounded by BOTH the volume's cluster count and the FAT's own
// declared length, so a boot sector claiming more clusters than its FAT can
// describe cannot make the scan read past the first FAT into its mirror.
// The clusters are returned unclaimed; the caller marks them.
func (fs *fat32FS) allocClusterRun(n int) ([]uint32, error) {
	if n <= 0 {
		return nil, nil
	}
	last := int64(fs.info.TotalSectors/uint32(fs.info.SectorsPerCluster)) + 2
	if fatEntries := int64(fs.info.FATSize) * int64(fs.info.BytesPerSector) / 4; fatEntries < last {
		last = fatEntries
	}
	fatBase := fs.info.FATOffset(fs.partOffset)
	out := make([]uint32, 0, n)
	buf := make([]byte, fatEntriesPerScanRead*4)
	for c := int64(2); c < last && len(out) < n; {
		want := int64(fatEntriesPerScanRead)
		if rem := last - c; rem < want {
			want = rem
		}
		b := buf[:want*4]
		if _, err := fs.f.ReadAt(b, fatBase+c*4); err != nil {
			return nil, fmt.Errorf("fat32: read FAT entries from cluster %d: %w", c, err)
		}
		for i := int64(0); i < want && len(out) < n; i++ {
			if binary.LittleEndian.Uint32(b[i*4:])&0x0FFFFFFF == 0 {
				out = append(out, uint32(c+i))
			}
		}
		c += want
	}
	if len(out) < n {
		return nil, fmt.Errorf("fat32: no free clusters")
	}
	return out, nil
}

// setDirEntryExtent rewrites the first-cluster and length fields of the
// directory entry for path, and refreshes its write timestamp.
//
// FAT records a file's length in its directory entry and nowhere else, so a
// positional write that extends the file is not complete until this lands: a
// Stat, a ReadFile, or a reopen would all still report the old length. It is
// the file-scoped twin of the tail of the path-scoped Truncate, which does the
// same three field writes; the two are kept separate because Truncate already
// holds the directory buffer it needs for its own checks and re-reading it
// here would cost a second pass for nothing.
func (fs *fat32FS) setDirEntryExtent(path string, firstCluster uint32, size int64) error {
	name, parentCluster, err := fs.getParentDir(path)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("fat32: %q is not a regular file", path)
	}
	buf, err := fs.readDirBuf(parentCluster)
	if err != nil {
		return err
	}
	startOff, count, found := fat32FindEntry(buf, name)
	if !found {
		return fmt.Errorf("fat32: %q not found: %w", path, iofs.ErrNotExist)
	}
	e := buf[startOff+(count-1)*dirEntrySize : startOff+count*dirEntrySize]
	le := binary.LittleEndian
	le.PutUint16(e[20:22], uint16(firstCluster>>16))
	le.PutUint16(e[26:28], uint16(firstCluster))
	le.PutUint32(e[28:32], uint32(size))
	// POSIX refreshes mtime on a write, and FAT has the field: WrtTime and
	// WrtDate. Nothing in the fleet reads it yet — interface.Stat has no time
	// accessor — but writing it costs nothing and means the volume is honest
	// when a real OS mounts it, which is the whole point of matching an
	// on-disk format.
	date, tod, _ := encodeFATDateTime(timeNow())
	le.PutUint16(e[22:24], tod)
	le.PutUint16(e[24:26], date)
	return fs.writeDirBuf(parentCluster, buf)
}
