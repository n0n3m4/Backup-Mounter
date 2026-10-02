package filesystem_btrfs

// readonly.go is the one file added on top of upstream go-filesystems/btrfs
// v0.1.0 for backup-mounter. It gives a read-only, concurrency-safe view of a
// whole filesystem:
//   - OpenRO skips buildSpaceManager (a full walk of every tree at mount, only
//     needed for writes) and the unbounded per-FS node cache.
//   - paths cross into subvolumes/snapshots (a DIR_ITEM whose location key is a
//     ROOT_ITEM switches to that subvolume's tree), which pathLookup does not do.
//   - ROFile.ReadAt decodes only the extents overlapping the requested range,
//     instead of readFileData's whole-file read.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

var errReadOnly = errors.New("btrfs: read-only view")

// roDev adapts a plain io.ReaderAt to blockBackend; every write fails.
type roDev struct {
	io.ReaderAt
	size int64
}

func (d roDev) WriteAt([]byte, int64) (int, error) { return 0, errReadOnly }
func (d roDev) Sync() error                        { return nil }
func (d roDev) Size() (int64, error)               { return d.size, nil }
func (d roDev) Truncate(int64) error               { return errReadOnly }
func (d roDev) Close() error                       { return nil }

// RO is a read-only view of a single-device btrfs filesystem starting at
// offset 0 of r. Safe for concurrent use.
type RO struct {
	fs *btrfsFS

	rootsOnce sync.Once
	roots     map[uint64]uint64 // subvolume id -> tree root logical address
	rootsErr  error
}

// ROInfo is the metadata of one inode. Tree/Ino identify it.
type ROInfo struct {
	Name  string
	Mode  uint16 // POSIX mode incl. S_IFMT bits
	Size  uint64
	MTime time.Time
	ATime time.Time
	OTime time.Time // creation
	Tree  uint64
	Ino   uint64
}

func (i ROInfo) IsDir() bool     { return i.Mode&0xF000 == 0x4000 }
func (i ROInfo) IsRegular() bool { return i.Mode&0xF000 == 0x8000 }

// OpenRO opens the filesystem read-only. The space manager is not built, so
// the write methods of the underlying FS must never be reached from here.
func OpenRO(r io.ReaderAt, size int64) (*RO, error) {
	dev := roDev{r, size}
	sb, err := readSuperblock(dev, 0)
	if err != nil {
		return nil, err
	}
	pool := newDevicePool(dev, 0, sb)
	if err := loadChunkTree(pool, 0, sb); err != nil {
		return nil, fmt.Errorf("btrfs: load chunk tree: %w", err)
	}
	fsRoot, err := resolveRootTree(pool, 0, sb)
	if err != nil {
		return nil, fmt.Errorf("btrfs: resolve FS_TREE: %w", err)
	}
	return &RO{fs: &btrfsFS{f: pool, rwa: pool, sb: sb, fsTreeRoot: fsRoot}}, nil
}

func (ro *RO) Label() string { return ro.fs.Label() }

// subvolRoot maps a subvolume id to its tree root, enumerating once.
func (ro *RO) subvolRoot(id uint64) (uint64, error) {
	ro.rootsOnce.Do(func() {
		subs, err := enumerateSubvolumes(ro.fs)
		ro.roots, ro.rootsErr = map[uint64]uint64{}, err
		for _, s := range subs {
			ro.roots[s.ID] = s.RootBytenr
		}
	})
	if ro.rootsErr != nil {
		return 0, ro.rootsErr
	}
	root, ok := ro.roots[id]
	if !ok || root == 0 {
		return 0, fmt.Errorf("btrfs: subvolume %d: %w", id, ErrNotFound)
	}
	return root, nil
}

type roDirEnt struct {
	name    string
	locType uint8
	objID   uint64
}

// parseDirEnts decodes packed btrfs_dir_item structs, keeping the location key type.
func parseDirEnts(d []byte) []roDirEnt {
	le := binary.LittleEndian
	var out []roDirEnt
	for off := 0; off+dirItemHdrSize <= len(d); {
		nameEnd := off + dirItemHdrSize + int(le.Uint16(d[off+0x1B:]))
		if nameEnd > len(d) {
			break
		}
		out = append(out, roDirEnt{string(d[off+dirItemHdrSize : nameEnd]), d[off+8], le.Uint64(d[off:])})
		off = nameEnd + int(le.Uint16(d[off+0x19:]))
	}
	return out
}

// target resolves a directory entry to (tree, inode), entering subvolumes.
func (ro *RO) target(tree uint64, e roDirEnt) (uint64, uint64, error) {
	if e.locType == typeRootItem {
		root, err := ro.subvolRoot(e.objID)
		return root, rootDirObjID, err
	}
	return tree, e.objID, nil
}

func (ro *RO) lookup(tree, dir uint64, name string) (uint64, uint64, error) {
	sb, r := ro.fs.sb, ro.fs.f
	if leaf, it, err := searchTree(r, 0, sb, tree, dir, typeDirItem, hashDirName(name)); err == nil {
		for _, e := range parseDirEnts(it.data(leaf)) {
			if e.name == name {
				return ro.target(tree, e)
			}
		}
	}
	// Hash collision or missing DIR_ITEM: scan DIR_INDEX like lookupDirEntry.
	items, _ := collectPrefixItems(r, 0, sb, tree, dir, typeDirIndex)
	for _, m := range items {
		for _, e := range parseDirEnts(m.data) {
			if e.name == name {
				return ro.target(tree, e)
			}
		}
	}
	return 0, 0, fmt.Errorf("btrfs: %q not found in inode %d: %w", name, dir, ErrNotFound)
}

func (ro *RO) inode(tree, ino uint64) (ROInfo, error) {
	leaf, it, err := searchTree(ro.fs.f, 0, ro.fs.sb, tree, ino, typeInodeItem, 0)
	if err != nil {
		return ROInfo{}, fmt.Errorf("btrfs: inode %d: %w", ino, err)
	}
	d := it.data(leaf)
	if len(d) < inodeItemSize {
		return ROInfo{}, fmt.Errorf("btrfs: inode %d: INODE_ITEM too short", ino)
	}
	le := binary.LittleEndian
	readTime := func(off int) time.Time { return time.Unix(int64(le.Uint64(d[off:])), int64(le.Uint32(d[off+8:]))) }
	return ROInfo{
		Mode:  uint16(le.Uint32(d[inodeOffMode:])),
		Size:  le.Uint64(d[inodeOffSize:]),
		MTime: readTime(inodeOffMTime),
		ATime: readTime(inodeOffATime),
		OTime: readTime(inodeOffOTime),
		Tree:  tree,
		Ino:   ino,
	}, nil
}

// Stat resolves a slash-separated path from the top-level subvolume.
func (ro *RO) Stat(p string) (ROInfo, error) {
	tree, ino := ro.fs.fsTreeRoot, rootDirObjID
	p = strings.Trim(path.Clean("/"+p), "/")
	if p != "" {
		for _, name := range strings.Split(p, "/") {
			var err error
			if tree, ino, err = ro.lookup(tree, ino, name); err != nil {
				return ROInfo{}, err
			}
		}
	}
	info, err := ro.inode(tree, ino)
	info.Name = path.Base("/" + p)
	return info, err
}

// ReadDir lists a directory with full metadata for every entry.
func (ro *RO) ReadDir(p string) ([]ROInfo, error) {
	dir, err := ro.Stat(p)
	if err != nil {
		return nil, err
	}
	if !dir.IsDir() {
		return nil, fmt.Errorf("btrfs: %q is not a directory", p)
	}
	items, err := collectPrefixItems(ro.fs.f, 0, ro.fs.sb, dir.Tree, dir.Ino, typeDirIndex)
	if errors.Is(err, ErrNotFound) {
		return nil, nil // empty directory
	} else if err != nil {
		return nil, err
	}
	var out []ROInfo
	for _, m := range items {
		for _, e := range parseDirEnts(m.data) {
			tree, ino, err := ro.target(dir.Tree, e)
			if err != nil {
				return nil, err
			}
			info, err := ro.inode(tree, ino)
			if err != nil {
				return nil, err
			}
			info.Name = e.name
			out = append(out, info)
		}
	}
	return out, nil
}

// ROFile reads a regular file by range.
type ROFile struct {
	ro    *RO
	size  int64
	exts  []prefixMatch // EXTENT_DATA items, sorted by file offset
	mu    sync.Mutex    // guards the decompressed-extent cache below
	cIdx  int
	cData []byte
}

// OpenFile opens a regular file for ReadAt.
// ponytail: all EXTENT_DATA items are loaded at open (~100 bytes/extent); search the tree per read if huge compressed files make that hurt.
func (ro *RO) OpenFile(p string) (*ROFile, error) {
	info, err := ro.Stat(p)
	if err != nil {
		return nil, err
	}
	if !info.IsRegular() {
		return nil, fmt.Errorf("btrfs: %q is not a regular file", p)
	}
	f := &ROFile{ro: ro, size: int64(info.Size), cIdx: -1}
	if info.Size > 0 {
		f.exts, err = collectPrefixItems(ro.fs.f, 0, ro.fs.sb, info.Tree, info.Ino, typeExtentData)
		if errors.Is(err, ErrNotFound) {
			err = nil // fully sparse file
		}
	}
	return f, err
}

func (f *ROFile) Size() int64  { return f.size }
func (f *ROFile) Close() error { return nil }

// extent returns extent i's decoded payload (inline or compressed), caching the last one.
func (f *ROFile) extent(i int) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cIdx == i {
		return f.cData, nil
	}
	d, le := f.exts[i].data, binary.LittleEndian
	raw := d[extDataHdrSize:] // inline payload
	if d[extDataOffType] == extentDataRegular {
		diskLen := le.Uint64(d[extDataOffDiskNumBytes:])
		if diskLen > maxDecompressRAM {
			return nil, fmt.Errorf("btrfs: compressed extent of %d bytes", diskLen)
		}
		phys, err := f.ro.fs.sb.physAddr(0, le.Uint64(d[extDataOffDiskBytenr:]))
		if err != nil {
			return nil, err
		}
		raw = make([]byte, diskLen)
		if n, err := f.ro.fs.f.ReadAt(raw, phys); n < len(raw) {
			return nil, fmt.Errorf("btrfs: read compressed extent: %w", err)
		}
	}
	out, err := decompressExtent(raw, d[extDataOffCompression], clampRAM(le.Uint64(d[extDataOffRamBytes:]), maxDecompressRAM))
	if err != nil {
		return nil, err
	}
	f.cIdx, f.cData = i, out
	return out, nil
}

func (f *ROFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("btrfs: negative offset")
	}
	if off >= f.size {
		return 0, io.EOF
	}
	var eof error
	if int64(len(p)) > f.size-off {
		p, eof = p[:f.size-off], io.EOF
	}
	clear(p) // holes and prealloc extents read as zeros
	end := uint64(off) + uint64(len(p))
	sb, r, le := f.ro.fs.sb, f.ro.fs.f, binary.LittleEndian

	i := sort.Search(len(f.exts), func(i int) bool { return f.exts[i].k.offset > uint64(off) }) - 1
	for i = max(i, 0); i < len(f.exts) && f.exts[i].k.offset < end; i++ {
		start, d := f.exts[i].k.offset, f.exts[i].data
		if len(d) <= extDataOffType {
			continue
		}
		comp, typ, ram := d[extDataOffCompression], d[extDataOffType], le.Uint64(d[extDataOffRamBytes:])
		var extOff, length uint64
		switch typ {
		case extentDataInline:
			length = ram
		case extentDataRegular:
			if len(d) < extDataRegularSize || le.Uint64(d[extDataOffDiskBytenr:]) == 0 {
				continue // sparse
			}
			extOff, length = le.Uint64(d[extDataOffOffset:]), le.Uint64(d[extDataOffNumBytes:])
		default:
			continue // prealloc: zeros
		}
		lo, hi := max(start, uint64(off)), min(start+length, end)
		if lo >= hi {
			continue
		}
		dst := p[lo-uint64(off) : hi-uint64(off)]
		src := extOff + (lo - start) // offset inside the decoded extent

		if typ == extentDataRegular && comp == compressionNone {
			phys, err := sb.physAddr(0, le.Uint64(d[extDataOffDiskBytenr:])+src)
			if err != nil {
				return 0, err
			}
			if n, err := r.ReadAt(dst, phys); n < len(dst) {
				return 0, fmt.Errorf("btrfs: read extent: %w", err)
			}
			continue
		}
		data, err := f.extent(i)
		if err != nil {
			return 0, err
		}
		if src < uint64(len(data)) {
			copy(dst, data[src:])
		}
	}
	return len(p), eof
}
