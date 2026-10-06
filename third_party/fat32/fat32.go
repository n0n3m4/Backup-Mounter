package filesystem_fat32

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"strings"
	"time"
	"unicode/utf16"

	filesystem "github.com/go-filesystems/interface"
	"github.com/go-volumes/gpt"
	"github.com/go-volumes/safeio"
)

// timeNow returns the wall-clock time stamped into newly written directory
// entries. It is a package var so tests can pin it to a fixed instant.
var timeNow = time.Now

// fatEpochDate is the FAT date word for 1980-01-01 (the FAT epoch): the value
// used for entries whose time predates the format's representable range.
const fatEpochDate uint16 = (1 << 5) | 1 // month=1, day=1, year=0 (1980)

// encodeFATDateTime encodes t into the FAT 16-bit date and time words plus the
// creation tenth-of-a-second byte. FAT stores local wall-clock time with
// 2-second resolution. Times before 1980 clamp to the FAT epoch.
//
//	date: bits 15..9 = year-1980, 8..5 = month (1-12), 4..0 = day (1-31)
//	tod:  bits 15..11 = hour, 10..5 = minute, 4..0 = seconds/2
func encodeFATDateTime(t time.Time) (date, tod uint16, tenth uint8) {
	t = t.Local()
	if t.Year() < 1980 {
		return fatEpochDate, 0, 0
	}
	date = uint16(t.Year()-1980)<<9 | uint16(t.Month())<<5 | uint16(t.Day())
	tod = uint16(t.Hour())<<11 | uint16(t.Minute())<<5 | uint16(t.Second()/2)
	tenth = uint8((t.Second()%2)*100 + t.Nanosecond()/10_000_000)
	return date, tod, tenth
}

const (
	sectorSize       = 512
	dirEntrySize     = 32
	fatAttrReadOnly  = 0x01
	fatAttrVolumeID  = 0x08
	fatAttrDirectory = 0x10
	fatAttrLongName  = 0x0F
	fatModeDir       = 0o040755
	fatModeDirRO     = 0o040555
	fatModeFile      = 0o100644
	fatModeFileRO    = 0o100444
)

type rootDirEntry struct {
	cluster uint32
	name    string
	attr    uint8
	size    uint32

	mtime, btime, atime time.Time // backup-mounter: write, creation, last-access
}

// Stat is what Stat returns: filesystem.Stat plus the entry's timestamps, which
// the interface has no accessor for. Zero for the root directory. backup-mounter addition.
type Stat struct {
	filesystem.Stat
	Mtime, Btime, Atime time.Time
}

// Times returns the last-write, creation and last-access times.
func (s Stat) Times() (mtime, btime, atime time.Time) { return s.Mtime, s.Btime, s.Atime }

// decodeFATDateTime inverts encodeFATDateTime: local wall-clock time at 2-second
// resolution, plus the creation entry's 10 ms count (0..199).
func decodeFATDateTime(date, tod uint16, tenth uint8) time.Time {
	return time.Date(1980+int(date>>9), time.Month(date>>5&0xF), int(date&0x1F),
		int(tod>>11), int(tod>>5&0x3F), int(tod&0x1F)*2+int(tenth)/100, int(tenth)%100*10_000_000, time.Local)
}

// fatEntryTimes decodes WrtTime/WrtDate, CrtTime/CrtDate/CrtTimeTenth and LstAccDate.
func fatEntryTimes(entry []byte) (mtime, btime, atime time.Time) {
	le := binary.LittleEndian
	return decodeFATDateTime(le.Uint16(entry[24:]), le.Uint16(entry[22:]), 0),
		decodeFATDateTime(le.Uint16(entry[16:]), le.Uint16(entry[14:]), entry[13]),
		decodeFATDateTime(le.Uint16(entry[18:]), 0, 0)
}

// Info holds the fields decoded from the FAT32 BIOS parameter block.
type Info struct {
	OEMName           string
	BytesPerSector    uint16
	SectorsPerCluster uint8
	ReservedSectors   uint16
	FATCount          uint8
	TotalSectors      uint32
	FATSize           uint32
	HiddenSectors     uint32
	RootCluster       uint32
	FSInfoSector      uint16
	BackupBootSector  uint16
	DriveNumber       uint8
	VolumeID          uint32
	VolumeLabel       string
	TypeLabel         string
}

// ClusterSize returns the size of one allocation cluster in bytes.
func (info Info) ClusterSize() uint32 {
	return uint32(info.BytesPerSector) * uint32(info.SectorsPerCluster)
}

// FATOffset returns the absolute byte offset of the first FAT.
func (info Info) FATOffset(partOffset int64) int64 {
	return partOffset + int64(info.ReservedSectors)*int64(info.BytesPerSector)
}

// DataOffset returns the absolute byte offset of the first data cluster.
func (info Info) DataOffset(partOffset int64) int64 {
	return partOffset +
		(int64(info.ReservedSectors)+int64(info.FATCount)*int64(info.FATSize))*int64(info.BytesPerSector)
}

// RootDirOffset returns the absolute byte offset of the root directory cluster.
func (info Info) RootDirOffset(partOffset int64) int64 {
	return info.DataOffset(partOffset) + int64(info.RootCluster-2)*int64(info.ClusterSize())
}

// diskRW combines the read, write, and close operations needed by FS.
type diskRW interface {
	io.ReaderAt
	io.WriterAt
	io.Closer
}

// FS represents an opened FAT32 image.
type fat32FS struct {
	f          diskRW
	partOffset int64
	info       Info
}

var (
	openFile            = os.OpenFile
	openPartitionOffset = partitionOffset
	openReadInfo        = readInfo
)

// Verify implementation of the common filesystem interface.
var _ filesystem.Filesystem = (*fat32FS)(nil)

// Verify implementation of the optional in-place truncation interface.
var _ filesystem.Truncater = (*fat32FS)(nil)

// Open opens imagePath, optionally selecting a partition, and parses the FAT32 BPB.
func Open(imagePath string, partIndex int) (filesystem.Filesystem, error) {
	f, err := openFile(imagePath, os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("fat32: open %s: %w", imagePath, err)
	}
	deviceSize, err := deviceFileSize(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("fat32: stat %s: %w", imagePath, err)
	}
	off, err := openPartitionOffset(f, deviceSize, partIndex)
	if err != nil {
		f.Close()
		return nil, err
	}
	info, err := openReadInfo(f, off)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &fat32FS{f: f, partOffset: off, info: info}, nil
}

// Close releases the underlying file handle.
func (fs *fat32FS) Close() error { return fs.f.Close() }

// Info returns the decoded boot-sector metadata.
func (fs *fat32FS) Info() Info { return fs.info }

// PartitionOffset returns the byte offset of the selected partition.
func (fs *fat32FS) PartitionOffset() int64 { return fs.partOffset }

// Stat returns basic metadata for the root directory or a path entry.
func (fs *fat32FS) Stat(path string) (filesystem.Stat, error) {
	if path == "/" {
		return filesystem.NewStat(fatModeDir, uint64(fs.info.ClusterSize()), uint64(fs.info.RootCluster)), nil
	}
	entry, _, err := fs.resolvePath(path)
	if err != nil {
		return nil, err
	}
	return Stat{filesystem.NewStat(entry.mode(), uint64(entry.size), uint64(entry.cluster)), entry.mtime, entry.btime, entry.atime}, nil
}

// ListDir lists the entries of the directory at path.
func (fs *fat32FS) ListDir(path string) ([]filesystem.DirEntry, error) {
	var dirCluster uint32
	if path == "/" {
		dirCluster = fs.info.RootCluster
	} else {
		entry, _, err := fs.resolvePath(path)
		if err != nil {
			return nil, err
		}
		if entry.attr&fatAttrDirectory == 0 {
			return nil, fmt.Errorf("fat32: %q is not a directory", path)
		}
		dirCluster = entry.cluster
	}
	buf, err := fs.readDirBuf(dirCluster)
	if err != nil {
		return nil, err
	}
	return parseRootDirEntries(buf), nil
}

// ReadFile reads and returns the contents of the regular file at path.
func (fs *fat32FS) ReadFile(path string) ([]byte, error) {
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
	return fs.readClusterChain(entry.cluster, uint64(entry.size))
}

// WriteFile creates or overwrites the regular file at path with data and permission bits.
func (fs *fat32FS) WriteFile(path string, data []byte, perm os.FileMode) error {
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
	startOff, count, exists := fat32FindEntry(buf, name)
	if exists {
		old8dot3 := buf[startOff+(count-1)*dirEntrySize : startOff+count*dirEntrySize]
		oldCluster := uint32(binary.LittleEndian.Uint16(old8dot3[20:22]))<<16 |
			uint32(binary.LittleEndian.Uint16(old8dot3[26:28]))
		if oldCluster >= 2 {
			if err := fs.freeChain(oldCluster); err != nil {
				return err
			}
		}
		for i := 0; i < count; i++ {
			buf[startOff+i*dirEntrySize] = 0xE5
		}
	}
	var firstCluster uint32
	if len(data) > 0 {
		firstCluster, err = fs.writeData(data)
		if err != nil {
			return err
		}
	}
	nLFN := 0
	if needsLFN(name) {
		nLFN = (len(utf16.Encode([]rune(name))) + 12) / 13
	}
	var slotOff int
	buf, slotOff = fs.ensureDirSlots(buf, nLFN+1)
	if slotOff < 0 {
		return fmt.Errorf("fat32: directory is full")
	}
	attr := byte(0x20)
	if perm&0o200 == 0 {
		attr |= fatAttrReadOnly
	}
	writeDirEntry(buf, slotOff, name, attr, firstCluster, uint32(len(data)))
	return fs.writeDirBuf(parentCluster, buf)
}

// ReadLink always returns an error; FAT32 does not support symbolic links.
func (fs *fat32FS) ReadLink(path string) (string, error) {
	return "", fmt.Errorf("fat32: %q is not a symbolic link", path)
}

// MkDir creates a new empty directory at path.
func (fs *fat32FS) MkDir(path string, perm os.FileMode) error {
	name, parentCluster, err := fs.getParentDir(path)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("fat32: %q already exists", path)
	}
	buf, err := fs.readDirBuf(parentCluster)
	if err != nil {
		return err
	}
	_, _, exists := fat32FindEntry(buf, name)
	if exists {
		return fmt.Errorf("fat32: %q already exists", path)
	}
	nLFN := 0
	if needsLFN(name) {
		nLFN = (len(utf16.Encode([]rune(name))) + 12) / 13
	}
	var slotOff int
	buf, slotOff = fs.ensureDirSlots(buf, nLFN+1)
	if slotOff < 0 {
		return fmt.Errorf("fat32: directory is full")
	}
	cluster, err := fs.allocCluster()
	if err != nil {
		return err
	}
	if err := fs.setFATEntry(cluster, 0x0FFFFFFF); err != nil {
		return err
	}
	clusterBuf := make([]byte, fs.info.ClusterSize())
	copy(clusterBuf[0:11], []byte(".          "))
	clusterBuf[11] = fatAttrDirectory
	binary.LittleEndian.PutUint16(clusterBuf[20:22], uint16(cluster>>16))
	binary.LittleEndian.PutUint16(clusterBuf[26:28], uint16(cluster))
	copy(clusterBuf[32:43], []byte("..         "))
	clusterBuf[43] = fatAttrDirectory
	binary.LittleEndian.PutUint16(clusterBuf[52:54], uint16(parentCluster>>16))
	binary.LittleEndian.PutUint16(clusterBuf[58:60], uint16(parentCluster))
	clusterOff := fs.info.DataOffset(fs.partOffset) + int64(cluster-2)*int64(fs.info.ClusterSize())
	if _, err := fs.f.WriteAt(clusterBuf, clusterOff); err != nil {
		return fmt.Errorf("fat32: write directory cluster: %w", err)
	}
	attr := byte(fatAttrDirectory)
	if perm&0o200 == 0 {
		attr |= fatAttrReadOnly
	}
	writeDirEntry(buf, slotOff, name, attr, cluster, 0)
	return fs.writeDirBuf(parentCluster, buf)
}

// DeleteFile removes the regular file at path, freeing its cluster chain.
func (fs *fat32FS) DeleteFile(path string) error {
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
	e8dot3 := buf[startOff+(count-1)*dirEntrySize : startOff+count*dirEntrySize]
	if e8dot3[11]&fatAttrDirectory != 0 {
		return fmt.Errorf("fat32: %q is a directory", path)
	}
	firstCluster := uint32(binary.LittleEndian.Uint16(e8dot3[20:22]))<<16 |
		uint32(binary.LittleEndian.Uint16(e8dot3[26:28]))
	if firstCluster >= 2 {
		if err := fs.freeChain(firstCluster); err != nil {
			return err
		}
	}
	for i := 0; i < count; i++ {
		buf[startOff+i*dirEntrySize] = 0xE5
	}
	return fs.writeDirBuf(parentCluster, buf)
}

// DeleteDir removes the directory at path, recursively deleting any contents.
func (fs *fat32FS) DeleteDir(path string) error {
	name, parentCluster, err := fs.getParentDir(path)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("fat32: cannot delete root directory")
	}
	buf, err := fs.readDirBuf(parentCluster)
	if err != nil {
		return err
	}
	startOff, count, found := fat32FindEntry(buf, name)
	if !found {
		return fmt.Errorf("fat32: %q not found: %w", path, iofs.ErrNotExist)
	}
	e8dot3 := buf[startOff+(count-1)*dirEntrySize : startOff+count*dirEntrySize]
	if e8dot3[11]&fatAttrDirectory == 0 {
		return fmt.Errorf("fat32: %q is not a directory", path)
	}
	firstCluster := uint32(binary.LittleEndian.Uint16(e8dot3[20:22]))<<16 |
		uint32(binary.LittleEndian.Uint16(e8dot3[26:28]))
	if firstCluster >= 2 {
		if err := fs.deleteAllContents(firstCluster); err != nil {
			return err
		}
		if err := fs.freeChain(firstCluster); err != nil {
			return err
		}
	}
	for i := 0; i < count; i++ {
		buf[startOff+i*dirEntrySize] = 0xE5
	}
	return fs.writeDirBuf(parentCluster, buf)
}

// Rename moves the entry at oldPath to newPath.
// If newPath already exists it is replaced.
func (fs *fat32FS) Rename(oldPath, newPath string) error {
	oldName, oldParentCluster, err := fs.getParentDir(oldPath)
	if err != nil {
		return err
	}
	if oldName == "" {
		return fmt.Errorf("fat32: cannot rename root directory")
	}
	newName, newParentCluster, err := fs.getParentDir(newPath)
	if err != nil {
		return err
	}
	if newName == "" {
		return fmt.Errorf("fat32: cannot rename to root directory")
	}
	if oldParentCluster == newParentCluster && strings.EqualFold(oldName, newName) {
		return nil
	}
	oldBuf, err := fs.readDirBuf(oldParentCluster)
	if err != nil {
		return err
	}
	oldStart, oldCount, oldFound := fat32FindEntry(oldBuf, oldName)
	if !oldFound {
		return fmt.Errorf("fat32: %q not found: %w", oldPath, iofs.ErrNotExist)
	}
	old8dot3 := oldBuf[oldStart+(oldCount-1)*dirEntrySize : oldStart+oldCount*dirEntrySize]
	oldCluster := uint32(binary.LittleEndian.Uint16(old8dot3[20:22]))<<16 |
		uint32(binary.LittleEndian.Uint16(old8dot3[26:28]))
	oldAttr := old8dot3[11]
	oldSize := binary.LittleEndian.Uint32(old8dot3[28:32])
	// Snapshot the source entry so the rename preserves its timestamps rather
	// than restamping them to "now" (POSIX rename leaves mtime untouched). The
	// backing bytes get cleared when the old slots are marked deleted below.
	oldStamp := append([]byte(nil), old8dot3...)

	var newBuf []byte
	if newParentCluster == oldParentCluster {
		newBuf = oldBuf
	} else {
		newBuf, err = fs.readDirBuf(newParentCluster)
		if err != nil {
			return err
		}
	}
	newStart, newCount, newFound := fat32FindEntry(newBuf, newName)
	if newFound {
		new8dot3 := newBuf[newStart+(newCount-1)*dirEntrySize : newStart+newCount*dirEntrySize]
		existCluster := uint32(binary.LittleEndian.Uint16(new8dot3[20:22]))<<16 |
			uint32(binary.LittleEndian.Uint16(new8dot3[26:28]))
		if existCluster >= 2 {
			if err := fs.freeChain(existCluster); err != nil {
				return err
			}
		}
		for i := 0; i < newCount; i++ {
			newBuf[newStart+i*dirEntrySize] = 0xE5
		}
	}
	for i := 0; i < oldCount; i++ {
		oldBuf[oldStart+i*dirEntrySize] = 0xE5
	}
	nLFN := 0
	if needsLFN(newName) {
		nLFN = (len(utf16.Encode([]rune(newName))) + 12) / 13
	}
	destBuf := oldBuf
	if newParentCluster != oldParentCluster {
		destBuf = newBuf
	}
	var slotOff int
	destBuf, slotOff = fs.ensureDirSlots(destBuf, nLFN+1)
	if slotOff < 0 {
		return fmt.Errorf("fat32: directory is full")
	}
	writeDirEntry(destBuf, slotOff, newName, oldAttr, oldCluster, oldSize)
	// Preserve the original creation / write / last-access timestamps; only
	// the name (and possibly parent) changed. writeDirEntry stamped "now".
	new8 := slotOff + nLFN*dirEntrySize
	copy(destBuf[new8+13:new8+20], oldStamp[13:20]) // CrtTimeTenth, CrtTime, CrtDate, LstAccDate
	copy(destBuf[new8+22:new8+26], oldStamp[22:26]) // WrtTime, WrtDate
	if newParentCluster != oldParentCluster {
		// destBuf points to the (possibly grown) newBuf; reassign back so the
		// post-grow buffer is what we persist.
		newBuf = destBuf
		if err := fs.writeDirBuf(oldParentCluster, oldBuf); err != nil {
			return err
		}
		return fs.writeDirBuf(newParentCluster, newBuf)
	}
	// Same-parent rename: destBuf is the (possibly grown) oldBuf.
	oldBuf = destBuf
	return fs.writeDirBuf(oldParentCluster, oldBuf)
}

// Truncate resizes the regular file at path to newSize bytes in place.
//
// Shrinking frees the trailing portion of the cluster chain and zero-fills any
// slack in the last retained cluster so that a subsequent grow reads zeros.
// Growing allocates additional zero-filled clusters and links them onto the
// chain. In both cases the 8.3 directory entry's FileSize (and, when the file
// transitions to or from zero length, its first-cluster fields) is updated.
func (fs *fat32FS) Truncate(path string, newSize int64) error {
	if newSize < 0 {
		return fmt.Errorf("fat32: negative truncate size %d", newSize)
	}
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
	e8dot3 := buf[startOff+(count-1)*dirEntrySize : startOff+count*dirEntrySize]
	if e8dot3[11]&fatAttrDirectory != 0 {
		return fmt.Errorf("fat32: %q is a directory", path)
	}

	le := binary.LittleEndian
	firstCluster := uint32(le.Uint16(e8dot3[20:22]))<<16 | uint32(le.Uint16(e8dot3[26:28]))
	oldSize := int64(le.Uint32(e8dot3[28:32]))
	if newSize == oldSize {
		return nil
	}

	clusterSize := int64(fs.info.ClusterSize())
	oldClusters := (oldSize + clusterSize - 1) / clusterSize
	newClusters := (newSize + clusterSize - 1) / clusterSize

	switch {
	case newSize < oldSize:
		if err := fs.shrinkChain(firstCluster, newClusters, newSize); err != nil {
			return err
		}
		if newClusters == 0 {
			firstCluster = 0
		}
	case newSize > oldSize:
		newFirst, err := fs.growChain(firstCluster, oldClusters, newClusters, oldSize)
		if err != nil {
			return err
		}
		firstCluster = newFirst
	}

	le.PutUint16(e8dot3[20:22], uint16(firstCluster>>16))
	le.PutUint16(e8dot3[26:28], uint16(firstCluster))
	le.PutUint32(e8dot3[28:32], uint32(newSize))
	return fs.writeDirBuf(parentCluster, buf)
}

// shrinkChain trims the cluster chain starting at firstCluster down to
// keepClusters clusters, freeing the remainder, and zero-fills the slack bytes
// (from newSize to the end of cluster) in the last retained cluster.
func (fs *fat32FS) shrinkChain(firstCluster uint32, keepClusters, newSize int64) error {
	if firstCluster < 2 {
		return nil
	}
	if keepClusters == 0 {
		return fs.freeChain(firstCluster)
	}
	clusterSize := int64(fs.info.ClusterSize())
	fatBase := fs.info.FATOffset(fs.partOffset)
	dataBase := fs.info.DataOffset(fs.partOffset)

	// Walk to the last cluster we keep (index keepClusters-1).
	cluster := firstCluster
	for i := int64(0); i < keepClusters-1; i++ {
		var next [4]byte
		if _, err := fs.f.ReadAt(next[:], fatBase+int64(cluster)*4); err != nil {
			return fmt.Errorf("fat32: read FAT entry for cluster %d: %w", cluster, err)
		}
		nextCluster := binary.LittleEndian.Uint32(next[:]) & 0x0FFFFFFF
		if nextCluster < 2 || nextCluster >= 0x0FFFFFF7 {
			// Chain is already shorter than the file claimed; nothing to free.
			cluster = nextCluster
			break
		}
		cluster = nextCluster
	}
	if cluster < 2 || cluster >= 0x0FFFFFF7 {
		return nil
	}

	// Free everything after the last retained cluster.
	var next [4]byte
	if _, err := fs.f.ReadAt(next[:], fatBase+int64(cluster)*4); err != nil {
		return fmt.Errorf("fat32: read FAT entry for cluster %d: %w", cluster, err)
	}
	tail := binary.LittleEndian.Uint32(next[:]) & 0x0FFFFFFF
	if err := fs.setFATEntry(cluster, 0x0FFFFFFF); err != nil {
		return err
	}
	if tail >= 2 && tail < 0x0FFFFFF7 {
		if err := fs.freeChain(tail); err != nil {
			return err
		}
	}

	// Zero-fill the slack in the last retained cluster so a later grow reads
	// zeros rather than stale data.
	slack := newSize % clusterSize
	if slack != 0 {
		zeroLen := clusterSize - slack
		zeros := make([]byte, zeroLen)
		off := dataBase + int64(cluster-2)*clusterSize + slack
		if _, err := fs.f.WriteAt(zeros, off); err != nil {
			return fmt.Errorf("fat32: zero slack in cluster %d: %w", cluster, err)
		}
	}
	return nil
}

// growChain extends the file from oldClusters to newClusters clusters,
// allocating zero-filled clusters and linking them in. It zero-fills the slack
// in the previously last cluster (from oldSize to the cluster boundary) and
// returns the file's first cluster (a freshly allocated one when the file was
// previously empty).
func (fs *fat32FS) growChain(firstCluster uint32, oldClusters, newClusters, oldSize int64) (uint32, error) {
	clusterSize := int64(fs.info.ClusterSize())
	fatBase := fs.info.FATOffset(fs.partOffset)
	dataBase := fs.info.DataOffset(fs.partOffset)

	// Zero the slack in the existing last cluster so the grown region reads
	// zeros across the old-size boundary.
	if firstCluster >= 2 {
		slack := oldSize % clusterSize
		if slack != 0 {
			last := firstCluster
			for i := int64(0); i < oldClusters-1; i++ {
				var next [4]byte
				if _, err := fs.f.ReadAt(next[:], fatBase+int64(last)*4); err != nil {
					return 0, fmt.Errorf("fat32: read FAT entry for cluster %d: %w", last, err)
				}
				nextCluster := binary.LittleEndian.Uint32(next[:]) & 0x0FFFFFFF
				if nextCluster < 2 || nextCluster >= 0x0FFFFFF7 {
					last = 0
					break
				}
				last = nextCluster
			}
			if last >= 2 {
				zeros := make([]byte, clusterSize-slack)
				off := dataBase + int64(last-2)*clusterSize + slack
				if _, err := fs.f.WriteAt(zeros, off); err != nil {
					return 0, fmt.Errorf("fat32: zero slack in cluster %d: %w", last, err)
				}
			}
		}
	}

	need := newClusters - oldClusters
	if need <= 0 {
		return firstCluster, nil
	}

	// Allocate the additional clusters, zero-fill them, and chain them together.
	allocated := make([]uint32, 0, need)
	rollback := func() {
		for _, c := range allocated {
			_ = fs.setFATEntry(c, 0)
		}
	}
	zeroCluster := make([]byte, clusterSize)
	for i := int64(0); i < need; i++ {
		c, err := fs.allocCluster()
		if err != nil {
			rollback()
			return 0, err
		}
		if err := fs.setFATEntry(c, 0x0FFFFFFF); err != nil {
			rollback()
			return 0, err
		}
		off := dataBase + int64(c-2)*clusterSize
		if _, err := fs.f.WriteAt(zeroCluster, off); err != nil {
			rollback()
			return 0, fmt.Errorf("fat32: zero new cluster %d: %w", c, err)
		}
		allocated = append(allocated, c)
	}
	for i := 0; i < len(allocated)-1; i++ {
		if err := fs.setFATEntry(allocated[i], allocated[i+1]); err != nil {
			rollback()
			return 0, err
		}
	}

	if firstCluster < 2 {
		// File was empty: the first allocated cluster becomes the file head.
		return allocated[0], nil
	}

	// Link the new run onto the end of the existing chain.
	// C1: the walk to the chain tail is unbounded for a forged cyclic chain;
	// guard it with a VisitSet (cycle) and a LoopGuard (over-long chain).
	last := firstCluster
	guard := safeio.NewLoopGuard(chainGuardLimit(fs))
	var visited visitBits // backup-mounter: bitmap, not a map
	for {
		if err := guard.Next(); err != nil {
			rollback()
			return 0, fmt.Errorf("fat32: grow chain from %d: %w", firstCluster, err)
		}
		if err := visited.Check(uint64(last)); err != nil {
			rollback()
			return 0, fmt.Errorf("fat32: grow chain from %d: %w", firstCluster, err)
		}
		var next [4]byte
		if _, err := fs.f.ReadAt(next[:], fatBase+int64(last)*4); err != nil {
			rollback()
			return 0, fmt.Errorf("fat32: read FAT entry for cluster %d: %w", last, err)
		}
		nextCluster := binary.LittleEndian.Uint32(next[:]) & 0x0FFFFFFF
		if nextCluster < 2 || nextCluster >= 0x0FFFFFF7 {
			break
		}
		last = nextCluster
	}
	if err := fs.setFATEntry(last, allocated[0]); err != nil {
		rollback()
		return 0, err
	}
	return firstCluster, nil
}

func readInfo(r io.ReaderAt, partOffset int64) (Info, error) {
	buf := make([]byte, sectorSize)
	if _, err := r.ReadAt(buf, partOffset); err != nil {
		return Info{}, fmt.Errorf("fat32: read boot sector: %w", err)
	}
	if buf[510] != 0x55 || buf[511] != 0xAA {
		return Info{}, fmt.Errorf("fat32: invalid boot sector signature")
	}

	le := binary.LittleEndian
	bytesPerSector := le.Uint16(buf[11:])
	if !isPowerOfTwo(uint32(bytesPerSector)) || bytesPerSector < uint16(sectorSize) {
		return Info{}, fmt.Errorf("fat32: invalid bytes per sector %d", bytesPerSector)
	}
	sectorsPerCluster := buf[13]
	if !isPowerOfTwo(uint32(sectorsPerCluster)) {
		return Info{}, fmt.Errorf("fat32: invalid sectors per cluster %d", sectorsPerCluster)
	}
	reservedSectors := le.Uint16(buf[14:])
	if reservedSectors == 0 {
		return Info{}, fmt.Errorf("fat32: reserved sector count is zero")
	}
	fatCount := buf[16]
	if fatCount == 0 {
		return Info{}, fmt.Errorf("fat32: FAT count is zero")
	}
	rootEntries := le.Uint16(buf[17:])
	if rootEntries != 0 {
		return Info{}, fmt.Errorf("fat32: root entry count %d indicates FAT12/16", rootEntries)
	}

	totalSectors := uint32(le.Uint16(buf[19:]))
	if totalSectors == 0 {
		totalSectors = le.Uint32(buf[32:])
	}
	if totalSectors == 0 {
		return Info{}, fmt.Errorf("fat32: total sector count is zero")
	}

	fatSize := uint32(le.Uint16(buf[22:]))
	if fatSize == 0 {
		fatSize = le.Uint32(buf[36:])
	}
	if fatSize == 0 {
		return Info{}, fmt.Errorf("fat32: FAT size is zero")
	}

	rootCluster := le.Uint32(buf[44:])
	if rootCluster < 2 {
		return Info{}, fmt.Errorf("fat32: invalid root cluster %d", rootCluster)
	}

	typeLabel := trimASCII(buf[82:90])
	if typeLabel != "" && typeLabel != "FAT32" {
		return Info{}, fmt.Errorf("fat32: unexpected filesystem type label %q", typeLabel)
	}

	return Info{
		OEMName:           trimASCII(buf[3:11]),
		BytesPerSector:    bytesPerSector,
		SectorsPerCluster: sectorsPerCluster,
		ReservedSectors:   reservedSectors,
		FATCount:          fatCount,
		TotalSectors:      totalSectors,
		FATSize:           fatSize,
		HiddenSectors:     le.Uint32(buf[28:]),
		RootCluster:       rootCluster,
		FSInfoSector:      le.Uint16(buf[48:]),
		BackupBootSector:  le.Uint16(buf[50:]),
		DriveNumber:       buf[64],
		VolumeID:          le.Uint32(buf[67:]),
		VolumeLabel:       trimASCII(buf[71:82]),
		TypeLabel:         typeLabel,
	}, nil
}

// deviceFileSize returns the byte length of the backing file, used as the
// deviceSize bound the partition-table parser validates every extent against.
func deviceFileSize(f *os.File) (int64, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// partitionOffset returns the byte offset from the image start to the requested
// partition. partIndex selects a specific partition slot (0-based); pass -1 to
// auto-select the first populated partition. Returns 0 when no partition table
// is present (a bare FAT32 image).
//
// M2: the bespoke MBR/GPT parser this used to carry validated the GPT entry
// size only as "< 128" — a forged entrySize of 0xFFFFFFFF sailed through and
// triggered a ~4 GB allocation plus a tableOff+index*entrySize offset overflow.
// It is replaced by the shared, hardened go-volumes/gpt parser, which bounds
// the entry array and every partition extent against deviceSize (computed in
// int64 with overflow checks) so a malicious table can neither over-allocate
// nor point us at an out-of-range offset. The historical "no table → offset 0"
// fallback for raw mkfs.fat images is preserved via gpt.ErrNoTable.
func partitionOffset(r io.ReaderAt, deviceSize int64, partIndex int) (int64, error) {
	if deviceSize <= 0 {
		// Without a usable device size we cannot validate partition extents;
		// treat the input as a bare filesystem image rather than guessing.
		return 0, nil
	}

	var p gpt.Partition
	var err error
	if partIndex >= 0 {
		p, err = gpt.ByIndex(r, deviceSize, partIndex)
	} else {
		p, err = gpt.First(r, deviceSize)
	}
	if err != nil {
		// No partition table at all (no GPT header, no MBR signature) is a bare
		// FAT32 image at offset 0. For the auto-select path a present-but-empty
		// table (signature but no populated slot) is likewise treated as a bare
		// image, matching the historical "first populated, else offset 0"
		// behaviour. An explicit partIndex that is empty/out-of-range stays an
		// error so callers learn the slot they asked for is unusable.
		if errors.Is(err, gpt.ErrNoTable) {
			return 0, nil
		}
		if partIndex < 0 && errors.Is(err, gpt.ErrNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("fat32: partition table: %w", err)
	}
	return p.StartOffset, nil
}

// maxDirClusters caps how many clusters a single directory may occupy.
// Exposed as a package-level var so tests can shrink it without having to
// build images millions of cluster-sized entries large.
var maxDirClusters = 256

// readDirBuf reads the full cluster chain of the directory at startCluster.
func (fs *fat32FS) readDirBuf(startCluster uint32) ([]byte, error) {
	maxBytes := uint64(maxDirClusters) * uint64(fs.info.ClusterSize())
	return fs.readClusterChain(startCluster, maxBytes)
}

// writeDirBuf writes buf back to the cluster chain starting at startCluster.
// When buf is longer than the existing chain, the chain is extended by
// allocating fresh clusters from the FAT and linking them in. This makes the
// root directory (and any subdirectory) grow uniformly past the first cluster.
func (fs *fat32FS) writeDirBuf(startCluster uint32, buf []byte) error {
	clusterSize := int64(fs.info.ClusterSize())
	dataBase := fs.info.DataOffset(fs.partOffset)
	fatBase := fs.info.FATOffset(fs.partOffset)
	cluster := startCluster
	// C1: bound the chain walk. A freshly allocated extension cluster (below)
	// is also a "visit", so the budget is cluster count plus the dir cap worth
	// of fresh links; a forged cyclic chain that re-enters an existing cluster
	// is rejected by the VisitSet before it can loop.
	guard := safeio.NewLoopGuard(chainGuardLimit(fs) + maxDirClusters)
	var visited visitBits // backup-mounter: bitmap, not a map
	for pos := 0; pos < len(buf); pos += int(clusterSize) {
		if err := guard.Next(); err != nil {
			return fmt.Errorf("fat32: write directory chain from %d: %w", startCluster, err)
		}
		if err := visited.Check(uint64(cluster)); err != nil {
			return fmt.Errorf("fat32: write directory chain from %d: %w", startCluster, err)
		}
		off := dataBase + int64(cluster-2)*clusterSize
		end := pos + int(clusterSize)
		if end > len(buf) {
			end = len(buf)
		}
		padded := make([]byte, clusterSize)
		copy(padded, buf[pos:end])
		if _, err := fs.f.WriteAt(padded, off); err != nil {
			return fmt.Errorf("fat32: write directory cluster %d: %w", cluster, err)
		}
		if end >= len(buf) {
			break
		}
		var nextEntry [4]byte
		if _, err := fs.f.ReadAt(nextEntry[:], fatBase+int64(cluster)*4); err != nil {
			return fmt.Errorf("fat32: read FAT entry for cluster %d: %w", cluster, err)
		}
		next := binary.LittleEndian.Uint32(nextEntry[:]) & 0x0FFFFFFF
		if next >= 0x0FFFFFF8 {
			// End of current chain — extend with a freshly allocated cluster.
			newCluster, err := fs.allocCluster()
			if err != nil {
				return fmt.Errorf("fat32: extend directory chain: %w", err)
			}
			if err := fs.setFATEntry(newCluster, 0x0FFFFFFF); err != nil {
				return err
			}
			if err := fs.setFATEntry(cluster, newCluster); err != nil {
				return err
			}
			// Zero the new cluster so that the unused 32-byte slots end with
			// the 0x00 sentinel byte that directory parsers expect.
			zeroBuf := make([]byte, clusterSize)
			newOff := dataBase + int64(newCluster-2)*clusterSize
			if _, err := fs.f.WriteAt(zeroBuf, newOff); err != nil {
				return fmt.Errorf("fat32: zero new directory cluster %d: %w", newCluster, err)
			}
			cluster = newCluster
			continue
		}
		if next < 2 {
			// Mirrors the tolerant behaviour in readClusterChain: an out-of-
			// range FAT pointer terminates the walk silently.
			break
		}
		cluster = next
	}
	return nil
}

// fat32FindEntry searches buf for a directory entry whose parsed name (LFN or
// 8.3 short name) matches name case-insensitively.
// Returns (startOff, totalCount, found) where startOff is the offset of the
// first LFN entry (or the 8.3 entry when no LFN precedes it) and totalCount
// is the total number of 32-byte slots in the entry chain.
func fat32FindEntry(buf []byte, name string) (int, int, bool) {
	le := binary.LittleEndian
	var lfnChars []uint16
	lfnStart := -1
	lfnCount := 0
	for offset := 0; offset+dirEntrySize <= len(buf); offset += dirEntrySize {
		entry := buf[offset : offset+dirEntrySize]
		b0 := entry[0]
		if b0 == 0x00 {
			return -1, 0, false
		}
		if b0 == 0xE5 {
			lfnChars = nil
			lfnStart = -1
			lfnCount = 0
			continue
		}
		if entry[11] == fatAttrLongName {
			if lfnStart < 0 {
				lfnStart = offset
			}
			lfnCount++
			var chars [13]uint16
			n := 0
			for _, off := range []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30} {
				c := le.Uint16(entry[off:])
				if c == 0x0000 || c == 0xFFFF {
					break
				}
				chars[n] = c
				n++
			}
			lfnChars = append(chars[:n], lfnChars...)
			continue
		}
		// Skip the volume-label entry (ATTR_VOLUME_ID): it is not a file and
		// must not match a lookup by name.
		if entry[11]&fatAttrVolumeID != 0 {
			lfnChars = nil
			lfnStart = -1
			lfnCount = 0
			continue
		}
		// 8.3 short entry
		var entryName string
		if len(lfnChars) > 0 {
			entryName = string(utf16.Decode(lfnChars))
		} else {
			entryName = shortName(entry)
		}
		if strings.EqualFold(entryName, name) {
			startOff := offset
			count := 1
			if lfnStart >= 0 {
				startOff = lfnStart
				count = lfnCount + 1
			}
			return startOff, count, true
		}
		lfnChars = nil
		lfnStart = -1
		lfnCount = 0
	}
	return -1, 0, false
}

// fat32FindFreeSlot returns the offset of the first free (0x00 or 0xE5) 32-byte
// slot in buf, or -1 if none exists.
func fat32FindFreeSlot(buf []byte) int {
	for offset := 0; offset+dirEntrySize <= len(buf); offset += dirEntrySize {
		b := buf[offset]
		if b == 0x00 || b == 0xE5 {
			return offset
		}
	}
	return -1
}

// fat32FindNFreeSlots returns the offset of the first run of n consecutive free
// (0x00 or 0xE5) 32-byte slots in buf, or -1 if no such run exists.
func fat32FindNFreeSlots(buf []byte, n int) int {
	for offset := 0; offset+n*dirEntrySize <= len(buf); offset += dirEntrySize {
		run := 0
		for i := 0; i < n; i++ {
			b := buf[offset+i*dirEntrySize]
			if b == 0x00 || b == 0xE5 {
				run++
			} else {
				break
			}
		}
		if run == n {
			return offset
		}
	}
	return -1
}

// ensureDirSlots returns buf, possibly extended by one or more zero-filled
// clusters, such that it contains at least n consecutive free 32-byte slots.
// Together with writeDirBuf's chain-extension logic this lets a directory
// grow past its initial single-cluster footprint.
//
// The returned offset is the position of the n-slot run inside the (possibly
// grown) buffer.
func (fs *fat32FS) ensureDirSlots(buf []byte, n int) ([]byte, int) {
	off := fat32FindNFreeSlots(buf, n)
	if off >= 0 {
		return buf, off
	}
	clusterSize := int(fs.info.ClusterSize())
	// Grow one cluster at a time until either we fit or we exceed the
	// safety cap shared with readDirBuf (256 clusters worth of dir buf
	// by default; tests may shrink this).
	for {
		if len(buf) >= maxDirClusters*clusterSize {
			return buf, -1
		}
		buf = append(buf, make([]byte, clusterSize)...)
		off = fat32FindNFreeSlots(buf, n)
		if off >= 0 {
			return buf, off
		}
	}
}

// needsLFN reports whether name requires LFN directory entries because it cannot
// be represented exactly as an 8.3 short name (e.g. lowercase, too long, etc.).
func needsLFN(name string) bool {
	short := toShortNameBytes(name)
	return shortName(short[:]) != name
}

// makeShortAlias generates an 8.3 alias for a long filename using the tilde
// notation (e.g. "LONGFI~1" + ext), guaranteed not to collide with any short
// name already present in taken. FAT requires every 8.3 short name in a
// directory to be unique; without the uniqueness check every long name sharing
// the same first characters would alias to "BASE~1.EXT" and fsck.fat would
// report "Duplicate directory entry" for each one.
//
// The numeric suffix grows as needed: ~1..~9, then the stem is shortened to
// keep the suffix within the 8-character short-name field (e.g. "LONGF~10",
// "LON~100"), matching the canonical Windows/Linux numeric-tail scheme.
func makeShortAlias(name string, taken map[[11]byte]bool) [11]byte {
	upper := strings.ToUpper(name)
	dot := strings.LastIndex(upper, ".")
	var base, ext string
	if dot >= 0 {
		base = upper[:dot]
		ext = upper[dot+1:]
	} else {
		base = upper
	}
	// Strip spaces and dots from base (not valid in short name stems).
	var clean strings.Builder
	for _, c := range base {
		if c != ' ' && c != '.' {
			clean.WriteRune(c)
		}
	}
	base = clean.String()
	if len(ext) > 3 {
		ext = ext[:3]
	}

	for n := 1; ; n++ {
		suffix := "~" + fmt.Sprintf("%d", n)
		// The stem (before the tilde) plus the suffix must fit in 8 chars.
		stemLen := 8 - len(suffix)
		if stemLen < 1 {
			stemLen = 1
		}
		stem := base
		if len(stem) > stemLen {
			stem = stem[:stemLen]
		}
		shortBase := stem + suffix

		var result [11]byte
		for i := range result {
			result[i] = ' '
		}
		for i := 0; i < 8 && i < len(shortBase); i++ {
			result[i] = shortBase[i]
		}
		for i := 0; i < 3 && i < len(ext); i++ {
			result[8+i] = ext[i]
		}
		if taken == nil || !taken[result] {
			return result
		}
	}
}

// collectShortNames returns the set of 8.3 short names already present in a
// directory buffer (skipping free, deleted, LFN, and volume-label slots) so a
// new alias can be chosen that does not collide with any of them.
func collectShortNames(buf []byte) map[[11]byte]bool {
	taken := make(map[[11]byte]bool)
	for offset := 0; offset+dirEntrySize <= len(buf); offset += dirEntrySize {
		b0 := buf[offset]
		if b0 == 0x00 {
			break
		}
		if b0 == 0xE5 {
			continue
		}
		if buf[offset+11] == fatAttrLongName {
			continue
		}
		if buf[offset+11]&fatAttrVolumeID != 0 {
			continue
		}
		var short [11]byte
		copy(short[:], buf[offset:offset+11])
		taken[short] = true
	}
	return taken
}

// lfnChecksum computes the FAT LFN checksum of an 8.3 short name entry.
func lfnChecksum(short [11]byte) byte {
	var sum byte
	for _, b := range short {
		sum = (sum>>1 | sum<<7) + b
	}
	return sum
}

// writeDirEntry writes LFN entries (when needed) followed by the 8.3 entry into
// buf at slotOff. The caller must ensure enough consecutive free slots exist.
func writeDirEntry(buf []byte, slotOff int, name string, attr byte, cluster uint32, size uint32) {
	le := binary.LittleEndian
	lfnOffsets := []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30}

	nameWords := utf16.Encode([]rune(name))
	var short [11]byte
	N := 0
	if needsLFN(name) {
		N = (len(nameWords) + 12) / 13
		// The 8.3 alias must be unique within the directory, so derive it
		// against the short names already written into buf.
		short = makeShortAlias(name, collectShortNames(buf))
	} else {
		short = toShortNameBytes(name)
	}
	csum := lfnChecksum(short)

	// Write LFN entries: seq N is first on disk (last portion of name + 0x40),
	// seq 1 is last on disk before the 8.3 entry (first portion of name).
	for seq := N; seq >= 1; seq-- {
		diskSlot := N - seq
		entryOff := slotOff + diskSlot*dirEntrySize
		for i := 0; i < dirEntrySize; i++ {
			buf[entryOff+i] = 0
		}
		seqByte := byte(seq)
		if seq == N {
			seqByte |= 0x40
		}
		buf[entryOff] = seqByte
		buf[entryOff+11] = fatAttrLongName
		buf[entryOff+13] = csum
		charStart := (seq - 1) * 13
		for i, pos := range lfnOffsets {
			idx := charStart + i
			var c uint16
			if idx < len(nameWords) {
				c = nameWords[idx]
			} else if idx == len(nameWords) {
				c = 0x0000
			} else {
				c = 0xFFFF
			}
			le.PutUint16(buf[entryOff+pos:], c)
		}
	}

	// Write 8.3 short-name entry.
	e8 := slotOff + N*dirEntrySize
	for i := 0; i < dirEntrySize; i++ {
		buf[e8+i] = 0
	}
	copy(buf[e8:e8+11], short[:])
	buf[e8+11] = attr
	// Creation / write / last-access timestamps. Without these the entry shows
	// an invalid 1980-00-00 date that fsck.fat and Windows flag; callers that
	// must preserve an existing timestamp (rename) overwrite these after.
	date, tod, tenth := encodeFATDateTime(timeNow())
	buf[e8+13] = tenth              // CrtTimeTenth
	le.PutUint16(buf[e8+14:], tod)  // CrtTime
	le.PutUint16(buf[e8+16:], date) // CrtDate
	le.PutUint16(buf[e8+18:], date) // LstAccDate (date only)
	le.PutUint16(buf[e8+22:], tod)  // WrtTime
	le.PutUint16(buf[e8+24:], date) // WrtDate
	le.PutUint16(buf[e8+20:], uint16(cluster>>16))
	le.PutUint16(buf[e8+26:], uint16(cluster))
	le.PutUint32(buf[e8+28:], size)
}

// pathComponents splits "/a/b/c" into ["a", "b", "c"].
func pathComponents(path string) []string {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

// resolvePath walks the full path, returning the final entry and its parent
// directory cluster. For "/" it returns a synthetic root entry.
func (fs *fat32FS) resolvePath(path string) (rootDirEntry, uint32, error) {
	if !strings.HasPrefix(path, "/") {
		return rootDirEntry{}, 0, fmt.Errorf("fat32: unsupported path %q", path)
	}
	components := pathComponents(path)
	parentCluster := fs.info.RootCluster
	var entry rootDirEntry
	for i, name := range components {
		buf, err := fs.readDirBuf(parentCluster)
		if err != nil {
			return rootDirEntry{}, 0, err
		}
		startOff, count, found := fat32FindEntry(buf, name)
		if !found {
			return rootDirEntry{}, 0, fmt.Errorf("fat32: %q not found: %w", path, iofs.ErrNotExist)
		}
		off8dot3 := startOff + (count-1)*dirEntrySize
		entry = toRootDirEntry(buf[off8dot3 : off8dot3+dirEntrySize])
		if i < len(components)-1 {
			if entry.attr&fatAttrDirectory == 0 {
				return rootDirEntry{}, 0, fmt.Errorf("fat32: intermediate path %q is not a directory", path)
			}
			parentCluster = entry.cluster
		}
	}
	return entry, parentCluster, nil
}

// getParentDir returns the last path component and the cluster of its parent
// directory. Returns an error for paths that require traversing non-existent
// or non-directory intermediate components.
func (fs *fat32FS) getParentDir(path string) (name string, parentCluster uint32, err error) {
	if !strings.HasPrefix(path, "/") {
		return "", 0, fmt.Errorf("fat32: unsupported path %q", path)
	}
	components := pathComponents(path)
	if len(components) == 0 {
		return "", fs.info.RootCluster, nil
	}
	parentCluster = fs.info.RootCluster
	for i := 0; i < len(components)-1; i++ {
		buf, err := fs.readDirBuf(parentCluster)
		if err != nil {
			return "", 0, err
		}
		startOff, count, found := fat32FindEntry(buf, components[i])
		if !found {
			return "", 0, fmt.Errorf("fat32: parent directory %q not found: %w", components[i], iofs.ErrNotExist)
		}
		off8dot3 := startOff + (count-1)*dirEntrySize
		e := toRootDirEntry(buf[off8dot3 : off8dot3+dirEntrySize])
		if e.attr&fatAttrDirectory == 0 {
			return "", 0, fmt.Errorf("fat32: intermediate path %q is not a directory", components[i])
		}
		parentCluster = e.cluster
	}
	return components[len(components)-1], parentCluster, nil
}

// deleteAllContents recursively removes all files and subdirectories inside
// the directory at dirCluster, freeing their cluster chains. The directory
// cluster itself is not freed; that is the caller's responsibility.
func (fs *fat32FS) deleteAllContents(dirCluster uint32) error {
	buf, err := fs.readDirBuf(dirCluster)
	if err != nil {
		return err
	}
	for _, e := range parseRootDirMetadata(buf) {
		if e.name == "." || e.name == ".." {
			continue
		}
		if e.attr&fatAttrDirectory != 0 && e.cluster >= 2 {
			if err := fs.deleteAllContents(e.cluster); err != nil {
				return err
			}
		}
		if e.cluster >= 2 {
			if err := fs.freeChain(e.cluster); err != nil {
				return err
			}
		}
	}
	return nil
}

func parseRootDirEntries(buf []byte) []filesystem.DirEntry {
	metadata := parseRootDirMetadata(buf)
	entries := make([]filesystem.DirEntry, 0, len(metadata))
	for _, entry := range metadata {
		entries = append(entries, filesystem.NewDirEntry(uint64(entry.cluster), entry.name, entry.attr))
	}
	return entries
}

func parseRootDirMetadata(buf []byte) []rootDirEntry {
	le := binary.LittleEndian
	entries := make([]rootDirEntry, 0)
	var lfnChars []uint16
	for offset := 0; offset+dirEntrySize <= len(buf); offset += dirEntrySize {
		entry := buf[offset : offset+dirEntrySize]
		switch entry[0] {
		case 0x00:
			return entries
		case 0xE5:
			lfnChars = nil
			continue
		}
		if entry[11] == fatAttrLongName {
			var chars [13]uint16
			n := 0
			for _, off := range []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30} {
				c := le.Uint16(entry[off:])
				if c == 0x0000 || c == 0xFFFF {
					break
				}
				chars[n] = c
				n++
			}
			lfnChars = append(chars[:n], lfnChars...)
			continue
		}
		// The volume-label entry (ATTR_VOLUME_ID) is filesystem metadata, not a
		// file; skip it so it never surfaces in ListDir/Stat.
		if entry[11]&fatAttrVolumeID != 0 {
			lfnChars = nil
			continue
		}
		var name string
		if len(lfnChars) > 0 {
			name = string(utf16.Decode(lfnChars))
		} else {
			name = shortName(entry)
		}
		lfnChars = nil
		e := toRootDirEntry(entry)
		e.name = name
		entries = append(entries, e)
	}
	return entries
}

func toRootDirEntry(entry []byte) rootDirEntry {
	mtime, btime, atime := fatEntryTimes(entry)
	clusterHigh := binary.LittleEndian.Uint16(entry[20:22])
	clusterLow := binary.LittleEndian.Uint16(entry[26:28])
	return rootDirEntry{
		mtime: mtime, btime: btime, atime: atime,
		cluster: uint32(clusterHigh)<<16 | uint32(clusterLow),
		name:    shortName(entry),
		attr:    entry[11],
		size:    binary.LittleEndian.Uint32(entry[28:32]),
	}
}

func (entry rootDirEntry) mode() uint16 {
	if entry.attr&fatAttrDirectory != 0 {
		if entry.attr&fatAttrReadOnly != 0 {
			return fatModeDirRO
		}
		return fatModeDir
	}
	if entry.attr&fatAttrReadOnly != 0 {
		return fatModeFileRO
	}
	return fatModeFile
}

func rootPathName(path string, prefix string) (string, error) {
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("%s: unsupported path %q", prefix, path)
	}
	name := strings.TrimPrefix(path, "/")
	if name == "" {
		return "", nil
	}
	if strings.Contains(name, "/") {
		return "", fmt.Errorf("%s: nested paths are not supported %q", prefix, path)
	}
	return name, nil
}

func shortName(entry []byte) string {
	base := strings.TrimRight(string(entry[0:8]), " ")
	ext := strings.TrimRight(string(entry[8:11]), " ")
	if ext == "" {
		return base
	}
	return base + "." + ext
}

func isPowerOfTwo(value uint32) bool {
	return value != 0 && value&(value-1) == 0
}

func trimASCII(raw []byte) string {
	return strings.TrimRight(string(raw), " \x00")
}

// clusterCount returns the number of data clusters the volume can hold, used to
// bound every FAT-chain walk and chain-derived allocation. It is computed from
// the BPB geometry (already validated by readInfo: SectorsPerCluster is a
// non-zero power of two), so the division cannot trap.
func (fs *fat32FS) clusterCount() int64 {
	return int64(fs.info.TotalSectors) / int64(fs.info.SectorsPerCluster)
}

// chainGuardLimit is the iteration ceiling applied to every FAT-chain walk via
// safeio.NewLoopGuard. A legitimate chain visits at most clusterCount distinct
// clusters, so clusterCount+1 never trips on valid data while still bounding a
// forged chain that walks cluster numbers beyond the volume's real geometry
// (which the VisitSet alone would not catch, since those clusters are all
// distinct). It is a package-level var so tests can shrink it to exercise the
// guard's error branch without building a volume with millions of clusters.
var chainGuardLimit = func(fs *fat32FS) int {
	return int(fs.clusterCount()) + 1
}

// readClusterChain follows the FAT chain starting at start and returns up to
// size bytes.
//
// C1/M3: a malicious image can forge a cyclic FAT chain (cluster N → N) or a
// FileSize of 0xFFFFFFFF. The original code would then append forever (hang)
// and/or pre-size buf to ~4 GB (OOM). The walk is now bounded two ways: a
// safeio.VisitSet rejects any revisited cluster (cycle → ErrCycle) and a
// safeio.LoopGuard sized to the cluster count caps the iteration count even for
// a non-cyclic but over-long chain. The result buffer is grown incrementally
// (no pre-allocation from the attacker-controlled size) and clamped to the
// real on-disk capacity via safeio.MakeBytes, so size only ever trims a buffer
// we already legitimately read.
func (fs *fat32FS) readClusterChain(start uint32, size uint64) ([]byte, error) {
	if start == 0 {
		return []byte{}, nil
	}
	clusterSize := int64(fs.info.ClusterSize())
	fatBase := fs.info.FATOffset(fs.partOffset)
	dataBase := fs.info.DataOffset(fs.partOffset)

	// Cap the requested size at the volume's real data capacity so a forged
	// FileSize cannot drive an over-large allocation. clusterCount and
	// clusterSize are both bounded positive (BPB geometry was validated by
	// readInfo), so the product stays a sane positive int64. The chain itself
	// is bounded below, so buf never exceeds maxBytes regardless.
	maxBytes := fs.clusterCount() * clusterSize
	if size > uint64(maxBytes) {
		size = uint64(maxBytes)
	}

	var buf []byte
	guard := safeio.NewLoopGuard(chainGuardLimit(fs))
	var visited visitBits // backup-mounter: bitmap, not a map
	cluster := start
	for {
		if cluster < 2 || cluster >= 0x0FFFFFF7 {
			break
		}
		if uint64(len(buf)) >= size {
			break
		}
		if err := guard.Next(); err != nil {
			return nil, fmt.Errorf("fat32: cluster chain from %d: %w", start, err)
		}
		if err := visited.Check(uint64(cluster)); err != nil {
			return nil, fmt.Errorf("fat32: cluster chain from %d: %w", start, err)
		}
		clusterBuf := make([]byte, clusterSize)
		off := dataBase + int64(cluster-2)*clusterSize
		if _, err := fs.f.ReadAt(clusterBuf, off); err != nil {
			return nil, fmt.Errorf("fat32: read cluster %d: %w", cluster, err)
		}
		buf = append(buf, clusterBuf...)
		var nextEntry [4]byte
		if _, err := fs.f.ReadAt(nextEntry[:], fatBase+int64(cluster)*4); err != nil {
			return nil, fmt.Errorf("fat32: read FAT entry for cluster %d: %w", cluster, err)
		}
		next := binary.LittleEndian.Uint32(nextEntry[:]) & 0x0FFFFFFF
		if next >= 0x0FFFFFF8 {
			break
		}
		cluster = next
	}
	if uint64(len(buf)) > size {
		buf = buf[:size]
	}
	return buf, nil
}

// writeData allocates FAT clusters, writes data into them, and returns the first cluster.
func (fs *fat32FS) writeData(data []byte) (uint32, error) {
	clusterSize := int64(fs.info.ClusterSize())
	numClusters := (int64(len(data)) + clusterSize - 1) / clusterSize
	allocated := make([]uint32, numClusters)
	for i := range allocated {
		c, err := fs.allocCluster()
		if err != nil {
			for _, ac := range allocated[:i] {
				_ = fs.setFATEntry(ac, 0)
			}
			return 0, err
		}
		if err := fs.setFATEntry(c, 0x0FFFFFFF); err != nil {
			return 0, err
		}
		allocated[i] = c
	}
	for i := 0; i < len(allocated)-1; i++ {
		if err := fs.setFATEntry(allocated[i], allocated[i+1]); err != nil {
			return 0, err
		}
	}
	dataBase := fs.info.DataOffset(fs.partOffset)
	for i, c := range allocated {
		off := dataBase + int64(c-2)*clusterSize
		start := int64(i) * clusterSize
		end := start + clusterSize
		if end > int64(len(data)) {
			clusterBuf := make([]byte, clusterSize)
			copy(clusterBuf, data[start:])
			if _, err := fs.f.WriteAt(clusterBuf, off); err != nil {
				return 0, fmt.Errorf("fat32: write cluster %d: %w", c, err)
			}
		} else {
			if _, err := fs.f.WriteAt(data[start:end], off); err != nil {
				return 0, fmt.Errorf("fat32: write cluster %d: %w", c, err)
			}
		}
	}
	return allocated[0], nil
}

// allocCluster scans the FAT and returns the first free cluster number (≥ 2).
func (fs *fat32FS) allocCluster() (uint32, error) {
	clusterCount := fs.info.TotalSectors / uint32(fs.info.SectorsPerCluster)
	fatBase := fs.info.FATOffset(fs.partOffset)
	var buf [4]byte
	for c := uint32(2); c < clusterCount+2; c++ {
		if _, err := fs.f.ReadAt(buf[:], fatBase+int64(c)*4); err != nil {
			return 0, fmt.Errorf("fat32: read FAT entry: %w", err)
		}
		if binary.LittleEndian.Uint32(buf[:])&0x0FFFFFFF == 0 {
			return c, nil
		}
	}
	return 0, fmt.Errorf("fat32: no free clusters")
}

// setFATEntry writes a 32-bit FAT entry for cluster, preserving the upper 4
// reserved bits, and mirrors the write across every FAT copy declared in the
// BPB (FAT0, FAT1, …). Keeping the mirror in sync is required for fsck.vfat /
// fsck_msdos to consider the volume clean.
func (fs *fat32FS) setFATEntry(cluster uint32, value uint32) error {
	fatBase := fs.info.FATOffset(fs.partOffset)
	fatBytes := int64(fs.info.FATSize) * int64(fs.info.BytesPerSector)
	off := fatBase + int64(cluster)*4
	var buf [4]byte
	if _, err := fs.f.ReadAt(buf[:], off); err != nil {
		return fmt.Errorf("fat32: read FAT entry for cluster %d: %w", cluster, err)
	}
	existing := binary.LittleEndian.Uint32(buf[:])
	binary.LittleEndian.PutUint32(buf[:], (existing&0xF0000000)|(value&0x0FFFFFFF))
	for i := uint8(0); i < fs.info.FATCount; i++ {
		mirrorOff := off + int64(i)*fatBytes
		if _, err := fs.f.WriteAt(buf[:], mirrorOff); err != nil {
			return fmt.Errorf("fat32: write FAT%d entry for cluster %d: %w", i, cluster, err)
		}
	}
	return nil
}

// freeChain marks every cluster in the FAT chain starting at start as free.
//
// C1: bounded by a safeio.VisitSet (cycle) and LoopGuard (over-long chain) so a
// forged cyclic chain frees each cluster at most once and terminates instead of
// spinning forever.
func (fs *fat32FS) freeChain(start uint32) error {
	fatBase := fs.info.FATOffset(fs.partOffset)
	guard := safeio.NewLoopGuard(chainGuardLimit(fs))
	var visited visitBits // backup-mounter: bitmap, not a map
	cluster := start
	for cluster >= 2 && cluster < 0x0FFFFFF7 {
		if err := guard.Next(); err != nil {
			return fmt.Errorf("fat32: free chain from %d: %w", start, err)
		}
		if err := visited.Check(uint64(cluster)); err != nil {
			return fmt.Errorf("fat32: free chain from %d: %w", start, err)
		}
		var next [4]byte
		if _, err := fs.f.ReadAt(next[:], fatBase+int64(cluster)*4); err != nil {
			return fmt.Errorf("fat32: read FAT entry for cluster %d: %w", cluster, err)
		}
		nextCluster := binary.LittleEndian.Uint32(next[:]) & 0x0FFFFFFF
		if err := fs.setFATEntry(cluster, 0); err != nil {
			return err
		}
		if nextCluster >= 0x0FFFFFF8 {
			break
		}
		cluster = nextCluster
	}
	return nil
}

// writeRootDir writes the root directory buffer back to disk.
func (fs *fat32FS) writeRootDir(buf []byte) error {
	return fs.writeDirBuf(fs.info.RootCluster, buf)
}

// toShortNameBytes converts a filename to an 11-byte FAT 8.3 short name (uppercase, space-padded).
func toShortNameBytes(name string) [11]byte {
	name = strings.ToUpper(name)
	var result [11]byte
	for i := range result {
		result[i] = ' '
	}
	dot := strings.LastIndex(name, ".")
	var base, ext string
	if dot >= 0 {
		base = name[:dot]
		ext = name[dot+1:]
	} else {
		base = name
	}
	for i := 0; i < 8 && i < len(base); i++ {
		result[i] = base[i]
	}
	for i := 0; i < 3 && i < len(ext); i++ {
		result[8+i] = ext[i]
	}
	return result
}

// findRootDirSlot returns (offset, true) if an entry with shortName exists,
// or (offset, false) if a free slot was found. Returns (-1, false) when full.
func findRootDirSlot(buf []byte, shortName [11]byte) (int, bool) {
	freeSlot := -1
	for offset := 0; offset+dirEntrySize <= len(buf); offset += dirEntrySize {
		first := buf[offset]
		if first == 0x00 {
			if freeSlot < 0 {
				freeSlot = offset
			}
			break
		}
		if first == 0xE5 {
			if freeSlot < 0 {
				freeSlot = offset
			}
			continue
		}
		if buf[offset+11] == fatAttrLongName {
			continue
		}
		var existing [11]byte
		copy(existing[:], buf[offset:offset+11])
		if existing == shortName {
			return offset, true
		}
	}
	return freeSlot, false
}
