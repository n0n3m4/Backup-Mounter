package filesystem_fat32

import (
	"encoding/binary"
	"fmt"
	"os"

	filesystem "github.com/go-filesystems/interface"
)

// Compile-time check: fat32FS satisfies the optional filesystem.Resizer
// interface. Resize / Grow / Shrink are also exposed directly for callers
// that want the direction-specific entry point.
var _ filesystem.Resizer = (*fat32FS)(nil)

// truncater is the subset of *os.File we need to grow / shrink the backing
// file. We accept any io implementation that can be type-asserted to this
// surface; in production the backing store is always an *os.File, but tests
// frequently swap in an in-memory shim.
type truncater interface {
	Truncate(size int64) error
}

// fat32SpecMinClusters is the cluster count below which the FAT
// specification reclassifies the volume as FAT16 (65525). The package's
// Format intentionally produces sub-spec volumes for small test images and
// fsck.vfat accepts them, so we don't enforce the spec floor — but the
// constant is exposed for callers / tests that want to.
const fat32SpecMinClusters = 65525

// resizeMinDataClusters is the smallest data-cluster count we'll allow
// after Grow / Shrink. We need at least one cluster for the root directory
// (cluster 2), plus enough headroom that fsck doesn't blow up on the empty
// data region. Anything below 16 is pathological.
const resizeMinDataClusters = 16

// Resize implements filesystem.Resizer: dispatches to Grow or Shrink based
// on the comparison between newSize and the current on-disk size. Returns
// nil when newSize already matches the current size.
func (fs *fat32FS) Resize(newSize int64) error {
	cur, err := fs.currentSize()
	if err != nil {
		return err
	}
	switch {
	case newSize == cur:
		return nil
	case newSize > cur:
		return fs.Grow(newSize)
	default:
		return fs.Shrink(newSize)
	}
}

// Grow extends the FAT32 filesystem to newSizeBytes. The new size must be a
// multiple of the cluster size and strictly larger than the current size.
//
// Grow rewrites the FAT region to cover every newly-addressable cluster.
// When the new FAT is larger than the current FAT, the data region shifts
// forward — every allocated cluster keeps its number but its byte offset
// moves by FATCount * (newFATSize - oldFATSize) bytes. The shift is done
// from the tail of the data region backwards so reads always see fresh
// bytes. After the shift the new FAT sectors are zero-filled and the
// existing FAT contents are mirrored across every FAT copy.
//
// The BPB total-sector and FAT-size fields are rewritten in both the
// primary and backup boot sectors, and the FSInfo free-count is refreshed
// by walking the FAT.
func (fs *fat32FS) Grow(newSizeBytes int64) error {
	if fs.partOffset != 0 {
		return fmt.Errorf("fat32: Grow: refusing to resize filesystem inside a partition (offset %d); resize the partition first", fs.partOffset)
	}
	cur, err := fs.currentSize()
	if err != nil {
		return err
	}
	clusterSize := int64(fs.info.ClusterSize())
	bytesPerSector := int64(fs.info.BytesPerSector)
	if newSizeBytes <= cur {
		return fmt.Errorf("fat32: Grow: new size %d <= current size %d", newSizeBytes, cur)
	}
	if newSizeBytes%clusterSize != 0 {
		return fmt.Errorf("fat32: Grow: new size %d is not a multiple of cluster size %d", newSizeBytes, clusterSize)
	}
	newTotalSectors := newSizeBytes / bytesPerSector
	if newTotalSectors > int64(^uint32(0)) {
		return fmt.Errorf("fat32: Grow: new size %d exceeds 32-bit sector count", newSizeBytes)
	}

	// Solve for newFATSizeSectors and newClusterCount the same way Format
	// does, but starting from the existing FAT size as the lower bound. We
	// never shrink the FAT on Grow — that would risk losing slack mtools
	// already addressed.
	newFATSize, newClusterCount, err := pickFATSize(
		newTotalSectors,
		int64(fs.info.ReservedSectors),
		int64(fs.info.FATCount),
		int64(fs.info.SectorsPerCluster),
		bytesPerSector,
		int64(fs.info.FATSize),
	)
	if err != nil {
		return fmt.Errorf("fat32: Grow: %w", err)
	}
	if newClusterCount < resizeMinDataClusters {
		return fmt.Errorf("fat32: Grow: new size %d yields %d data clusters, below floor %d",
			newSizeBytes, newClusterCount, resizeMinDataClusters)
	}

	oldFATSize := int64(fs.info.FATSize)
	oldClusterCount := int64(fs.info.TotalSectors) / int64(fs.info.SectorsPerCluster)
	oldDataBase := fs.info.DataOffset(fs.partOffset)
	newDataBase := fs.partOffset +
		(int64(fs.info.ReservedSectors)+int64(fs.info.FATCount)*newFATSize)*bytesPerSector

	t, ok := fs.f.(truncater)
	if !ok {
		return fmt.Errorf("fat32: Grow: backing store does not support Truncate")
	}
	// Extend the backing file first — Truncate gives implicit zero-fill, so
	// the freshly-allocated tail is safe to write into.
	if err := t.Truncate(newSizeBytes); err != nil {
		return fmt.Errorf("fat32: Grow: truncate to %d: %w", newSizeBytes, err)
	}

	// If the FAT grew, every data cluster's byte offset shifts forward by
	// the same delta. Walk clusters from highest to lowest so the source
	// bytes are always read before being overwritten.
	if newFATSize != oldFATSize {
		if newFATSize < oldFATSize {
			// Defensive: pickFATSize is bounded by oldFATSize, so this
			// branch is unreachable. Guard against a future refactor.
			return fmt.Errorf("fat32: Grow: newFATSize %d < oldFATSize %d (internal)", newFATSize, oldFATSize)
		}
		if err := fs.shiftDataRegion(oldDataBase, newDataBase, oldClusterCount, clusterSize); err != nil {
			return fmt.Errorf("fat32: Grow: shift data region: %w", err)
		}
		// Mirror the old FAT contents into every FAT copy at its new offset.
		// FAT0 stays put (its base offset is `reservedSectors*bytesPerSector`
		// which is unchanged) but extends in size — its existing first
		// `oldFATSize` sectors keep their content, and the new tail is
		// already zero from Truncate. FAT1+ live at a moved offset, so
		// copy the canonical FAT0 contents into each.
		if err := fs.relocateFATCopies(oldFATSize, newFATSize); err != nil {
			return fmt.Errorf("fat32: Grow: rebuild FAT copies: %w", err)
		}
		// Persist the new FAT size in the BPB.
		if err := fs.writeFATSize(uint32(newFATSize)); err != nil {
			return fmt.Errorf("fat32: Grow: write FAT size: %w", err)
		}
		fs.info.FATSize = uint32(newFATSize)
	}

	if err := fs.writeTotalSectors(uint32(newTotalSectors)); err != nil {
		return fmt.Errorf("fat32: Grow: write total sectors: %w", err)
	}
	fs.info.TotalSectors = uint32(newTotalSectors)

	if err := fs.refreshFSInfo(); err != nil {
		return fmt.Errorf("fat32: Grow: refresh FSInfo: %w", err)
	}
	return nil
}

// Shrink truncates the FAT32 filesystem to newSizeBytes. The new size must
// be a multiple of the cluster size, strictly smaller than the current
// size, and large enough that no in-use cluster falls beyond the new data
// region.
//
// Shrink leaves the FAT region at its current size (just with unused slack
// at the tail) and refuses to drop any cluster whose FAT entry is non-zero.
// fsck.vfat tolerates a FAT larger than strictly needed; rewriting the FAT
// to a smaller size would require shifting the data region backward, which
// adds risk without a corresponding payoff.
func (fs *fat32FS) Shrink(newSizeBytes int64) error {
	if fs.partOffset != 0 {
		return fmt.Errorf("fat32: Shrink: refusing to resize filesystem inside a partition (offset %d); resize the partition first", fs.partOffset)
	}
	cur, err := fs.currentSize()
	if err != nil {
		return err
	}
	clusterSize := int64(fs.info.ClusterSize())
	bytesPerSector := int64(fs.info.BytesPerSector)
	if newSizeBytes >= cur {
		return fmt.Errorf("fat32: Shrink: new size %d >= current size %d", newSizeBytes, cur)
	}
	if newSizeBytes%clusterSize != 0 {
		return fmt.Errorf("fat32: Shrink: new size %d is not a multiple of cluster size %d", newSizeBytes, clusterSize)
	}

	newTotalSectors := newSizeBytes / bytesPerSector
	dataSectors := newTotalSectors -
		int64(fs.info.ReservedSectors) -
		int64(fs.info.FATCount)*int64(fs.info.FATSize)
	if dataSectors <= 0 {
		return fmt.Errorf("fat32: Shrink: new size %d leaves no data sectors", newSizeBytes)
	}
	newClusterCount := uint32(dataSectors / int64(fs.info.SectorsPerCluster))
	if newClusterCount < resizeMinDataClusters {
		return fmt.Errorf("fat32: Shrink: new size %d yields %d data clusters, below floor %d",
			newSizeBytes, newClusterCount, resizeMinDataClusters)
	}

	// Walk every FAT entry whose cluster number is past the new ceiling.
	// If any is allocated we refuse the shrink — losing data is never our
	// call to make. The upper bound is the actual data-region cluster
	// count, NOT TotalSectors/SectorsPerCluster: the latter includes the
	// reserved area and the FAT region, so it ranges past the FAT entries
	// that legitimately address real clusters (anything beyond reads the
	// adjacent FAT1 header bytes, which look like an in-use entry and
	// would cause false positives).
	curDataSectors := int64(fs.info.TotalSectors) -
		int64(fs.info.ReservedSectors) -
		int64(fs.info.FATCount)*int64(fs.info.FATSize)
	curClusterCount := uint32(curDataSectors / int64(fs.info.SectorsPerCluster))
	fatBase := fs.info.FATOffset(fs.partOffset)
	var buf [4]byte
	for c := newClusterCount + 2; c < curClusterCount+2; c++ {
		if _, err := fs.f.ReadAt(buf[:], fatBase+int64(c)*4); err != nil {
			return fmt.Errorf("fat32: Shrink: read FAT entry %d: %w", c, err)
		}
		entry := binary.LittleEndian.Uint32(buf[:]) & 0x0FFFFFFF
		if entry != 0 {
			return fmt.Errorf("fat32: Shrink: cluster %d is in use (FAT entry 0x%08x); new size would lose data",
				c, entry)
		}
	}

	// Zero the FAT entries that fall outside the new cluster range in every
	// FAT copy. They are already zero on a clean image but mtools / fsck
	// occasionally complain when stale end-of-chain markers linger.
	fatBytes := int64(fs.info.FATSize) * bytesPerSector
	var zero [4]byte
	for i := uint8(0); i < fs.info.FATCount; i++ {
		base := fatBase + int64(i)*fatBytes
		for c := newClusterCount + 2; c < curClusterCount+2; c++ {
			if _, err := fs.f.WriteAt(zero[:], base+int64(c)*4); err != nil {
				return fmt.Errorf("fat32: Shrink: zero FAT%d entry %d: %w", i, c, err)
			}
		}
	}

	// Check that the backing store supports Truncate *before* mutating the
	// BPB. If it doesn't, we'd be leaving an inconsistent on-disk state.
	t, ok := fs.f.(truncater)
	if !ok {
		return fmt.Errorf("fat32: Shrink: backing store does not support Truncate")
	}

	// Update BPB total sectors in the primary and backup boot sectors.
	if err := fs.writeTotalSectors(uint32(newTotalSectors)); err != nil {
		return fmt.Errorf("fat32: Shrink: write total sectors: %w", err)
	}
	fs.info.TotalSectors = uint32(newTotalSectors)

	// Truncate the underlying file last — once metadata is consistent.
	if err := t.Truncate(newSizeBytes); err != nil {
		return fmt.Errorf("fat32: Shrink: truncate to %d: %w", newSizeBytes, err)
	}

	if err := fs.refreshFSInfo(); err != nil {
		return fmt.Errorf("fat32: Shrink: refresh FSInfo: %w", err)
	}
	return nil
}

// pickFATSize replays Format's FAT-size convergence to choose the smallest
// FAT (in sectors, per copy) that covers every cluster of a volume with
// totalSectors of bytesPerSector bytes each. The returned FAT size is never
// smaller than minFATSize, which lets Grow lock in the existing FAT layout
// when the requested grow comfortably fits within it.
//
// Returns (fatSizeSectors, dataClusters, error). The error is non-nil only
// when the requested totalSectors leaves no room for even one data cluster.
func pickFATSize(totalSectors, reservedSectors, fatCount, sectorsPerCluster, bytesPerSector, minFATSize int64) (int64, uint32, error) {
	if totalSectors-reservedSectors <= 0 {
		return 0, 0, fmt.Errorf("totalSectors %d leaves no data sectors", totalSectors)
	}
	fatSize := minFATSize
	if fatSize < 1 {
		fatSize = 1
	}
	for i := 0; i < 64; i++ {
		clusterCount := (totalSectors - reservedSectors - fatCount*fatSize) / sectorsPerCluster
		if clusterCount <= 0 {
			return 0, 0, fmt.Errorf("totalSectors %d, FATSize %d leaves no data clusters", totalSectors, fatSize)
		}
		needed := (clusterCount+2)*4/bytesPerSector + 1
		if needed <= fatSize {
			return fatSize, uint32(clusterCount), nil
		}
		fatSize = needed
	}
	return 0, 0, fmt.Errorf("FAT size did not converge for totalSectors %d", totalSectors)
}

// shiftDataRegion copies every existing data cluster from its old byte
// offset to its new byte offset. clusters are numbered identically before
// and after; only oldBase and newBase differ. Walking from the highest
// cluster down avoids overwriting unread bytes when newBase > oldBase.
func (fs *fat32FS) shiftDataRegion(oldBase, newBase, clusterCount, clusterSize int64) error {
	if newBase == oldBase || clusterCount == 0 {
		return nil
	}
	cluster := make([]byte, clusterSize)
	// Cluster numbers run 2..clusterCount+1 (cluster 2 lives at offset 0
	// in the data region). We iterate the actual data range covered by
	// the old layout — anything past that is freshly-allocated zero space.
	for i := clusterCount - 1; i >= 0; i-- {
		oldOff := oldBase + i*clusterSize
		newOff := newBase + i*clusterSize
		if _, err := fs.f.ReadAt(cluster, oldOff); err != nil {
			return fmt.Errorf("read cluster at %d: %w", oldOff, err)
		}
		if _, err := fs.f.WriteAt(cluster, newOff); err != nil {
			return fmt.Errorf("write cluster at %d: %w", newOff, err)
		}
	}
	return nil
}

// relocateFATCopies rewrites every FAT copy to span newFATSize sectors. The
// canonical contents are read from FAT0 (which occupies bytes
// [reservedSectors*bytesPerSector, reservedSectors*bytesPerSector +
// oldFATSize*bytesPerSector)) — exactly the bytes that Truncate left
// untouched at the head of the FAT region. The new tail of every FAT copy
// is left at zero (Truncate fills with zeros, which is also the FAT32
// "free cluster" marker).
//
// For each FAT copy i ≥ 1, the new on-disk location is
//
//	reservedSectors*bytesPerSector + i*newFATSize*bytesPerSector
//
// so we copy the oldFATSize sectors of canonical content into that slot.
func (fs *fat32FS) relocateFATCopies(oldFATSize, newFATSize int64) error {
	bytesPerSector := int64(fs.info.BytesPerSector)
	fat0Base := fs.info.FATOffset(fs.partOffset)
	oldFATBytes := oldFATSize * bytesPerSector
	newFATBytes := newFATSize * bytesPerSector

	// Read the entire FAT0 once — it serves as the source for every copy.
	fat0 := make([]byte, oldFATBytes)
	if _, err := fs.f.ReadAt(fat0, fat0Base); err != nil {
		return fmt.Errorf("read FAT0: %w", err)
	}

	// FAT0 itself just needs its tail zeroed — but Truncate already did
	// that, so no work. Copies 1+ live at moved offsets.
	for i := int64(1); i < int64(fs.info.FATCount); i++ {
		newOff := fat0Base + i*newFATBytes
		if _, err := fs.f.WriteAt(fat0, newOff); err != nil {
			return fmt.Errorf("write FAT%d at %d: %w", i, newOff, err)
		}
		// Zero the tail past the canonical content. Truncate already did
		// this for byte ranges past EOF, but the bytes at
		// [newOff+oldFATBytes, newOff+newFATBytes) used to be data-region
		// content (now relocated) — they MUST be zeroed.
		if newFATBytes > oldFATBytes {
			zero := make([]byte, newFATBytes-oldFATBytes)
			if _, err := fs.f.WriteAt(zero, newOff+oldFATBytes); err != nil {
				return fmt.Errorf("zero FAT%d tail at %d: %w", i, newOff+oldFATBytes, err)
			}
		}
	}
	// FAT0's tail (bytes [fat0Base+oldFATBytes, fat0Base+newFATBytes))
	// used to be either FAT1 content or freshly-allocated zero, depending
	// on layout. Zero it explicitly so leftover FAT1 entries don't masquerade
	// as in-use clusters on FAT0.
	if newFATBytes > oldFATBytes {
		zero := make([]byte, newFATBytes-oldFATBytes)
		if _, err := fs.f.WriteAt(zero, fat0Base+oldFATBytes); err != nil {
			return fmt.Errorf("zero FAT0 tail at %d: %w", fat0Base+oldFATBytes, err)
		}
	}
	return nil
}

// writeFATSize rewrites BPB_FATSz32 (offset 36) in the primary and backup
// boot sectors. The legacy 16-bit BPB_FATSz16 field at offset 22 is kept
// at 0 — a FAT32 volume must have it zero.
func (fs *fat32FS) writeFATSize(newSize uint32) error {
	bootSize := int64(fs.info.BytesPerSector)
	for _, off := range fs.bootSectorOffsets() {
		buf := make([]byte, bootSize)
		if _, err := fs.f.ReadAt(buf, off); err != nil {
			return fmt.Errorf("read boot sector at %d: %w", off, err)
		}
		if buf[510] != 0x55 || buf[511] != 0xAA {
			return fmt.Errorf("boot signature missing at %d", off)
		}
		binary.LittleEndian.PutUint16(buf[22:24], 0)
		binary.LittleEndian.PutUint32(buf[36:40], newSize)
		if _, err := fs.f.WriteAt(buf, off); err != nil {
			return fmt.Errorf("write boot sector at %d: %w", off, err)
		}
	}
	return nil
}

// currentSize returns the on-disk size of the backing file. For bare
// images this is the entire file; for partitioned images it is the same
// value, and Grow / Shrink refuse to operate in that case anyway.
func (fs *fat32FS) currentSize() (int64, error) {
	// Preferred path: the backing store is an *os.File whose Stat works.
	if sf, ok := fs.f.(interface {
		Stat() (os.FileInfo, error)
	}); ok {
		fi, err := sf.Stat()
		if err != nil {
			return 0, fmt.Errorf("fat32: stat backing file: %w", err)
		}
		return fi.Size(), nil
	}
	// Fall back to the boot-sector total-sector count. Used by the mock
	// disk in unit tests; matches the actual layout for any real image.
	return int64(fs.info.TotalSectors) * int64(fs.info.BytesPerSector), nil
}

// writeTotalSectors rewrites the 32-bit total-sector field at boot-sector
// offset 32 (and clears the legacy 16-bit field at offset 19, which a
// FAT32 volume must keep at 0). The change is mirrored to the backup boot
// sector when BPB.BackupBootSector is set.
func (fs *fat32FS) writeTotalSectors(newTotal uint32) error {
	bootSize := int64(fs.info.BytesPerSector)
	for _, off := range fs.bootSectorOffsets() {
		buf := make([]byte, bootSize)
		if _, err := fs.f.ReadAt(buf, off); err != nil {
			return fmt.Errorf("read boot sector at %d: %w", off, err)
		}
		if buf[510] != 0x55 || buf[511] != 0xAA {
			return fmt.Errorf("boot signature missing at %d", off)
		}
		binary.LittleEndian.PutUint16(buf[19:21], 0)
		binary.LittleEndian.PutUint32(buf[32:36], newTotal)
		if _, err := fs.f.WriteAt(buf, off); err != nil {
			return fmt.Errorf("write boot sector at %d: %w", off, err)
		}
	}
	return nil
}

// bootSectorOffsets returns the absolute byte offsets of every boot sector
// we need to keep in sync: the primary BPB and the backup (when recorded).
func (fs *fat32FS) bootSectorOffsets() []int64 {
	offs := []int64{fs.partOffset}
	if fs.info.BackupBootSector != 0 {
		offs = append(offs, fs.partOffset+int64(fs.info.BackupBootSector)*int64(fs.info.BytesPerSector))
	}
	return offs
}

// refreshFSInfo walks the FAT, counts the free clusters, and writes the
// new count into the FSInfo sector (offset 488). The "next free" hint at
// offset 492 is left at 0xFFFFFFFF ("unknown"), matching what Format
// writes — the allocator scans from cluster 2 anyway, so the hint is
// purely advisory.
//
// When FSInfoSector is 0 (a non-conforming image, e.g. some hand-crafted
// fixtures) the function is a no-op. Likewise, when the recorded
// signature doesn't match, we leave the sector alone rather than overwrite
// something that isn't an FSInfo.
func (fs *fat32FS) refreshFSInfo() error {
	if fs.info.FSInfoSector == 0 {
		return nil
	}
	bytesPerSector := int64(fs.info.BytesPerSector)
	off := fs.partOffset + int64(fs.info.FSInfoSector)*bytesPerSector

	buf := make([]byte, bytesPerSector)
	if _, err := fs.f.ReadAt(buf, off); err != nil {
		return fmt.Errorf("read FSInfo at %d: %w", off, err)
	}
	if binary.LittleEndian.Uint32(buf[0:4]) != 0x41615252 ||
		binary.LittleEndian.Uint32(buf[484:488]) != 0x61417272 ||
		binary.LittleEndian.Uint32(buf[508:512]) != 0xAA550000 {
		return nil
	}

	// Cluster count = data sectors / sectors per cluster. Using TotalSectors
	// directly over-counts: the value includes the reserved + FAT regions,
	// so entries past the real data range would be sampled inside the
	// adjacent FAT1 / FAT2 headers and counted as either free or in-use
	// based on whatever happens to live there.
	dataSectors := int64(fs.info.TotalSectors) -
		int64(fs.info.ReservedSectors) -
		int64(fs.info.FATCount)*int64(fs.info.FATSize)
	clusterCount := uint32(dataSectors / int64(fs.info.SectorsPerCluster))
	fatBase := fs.info.FATOffset(fs.partOffset)
	free := uint32(0)
	var entry [4]byte
	for c := uint32(2); c < clusterCount+2; c++ {
		if _, err := fs.f.ReadAt(entry[:], fatBase+int64(c)*4); err != nil {
			return fmt.Errorf("read FAT entry %d: %w", c, err)
		}
		if binary.LittleEndian.Uint32(entry[:])&0x0FFFFFFF == 0 {
			free++
		}
	}

	binary.LittleEndian.PutUint32(buf[488:492], free)
	if _, err := fs.f.WriteAt(buf, off); err != nil {
		return fmt.Errorf("write FSInfo at %d: %w", off, err)
	}
	return nil
}
