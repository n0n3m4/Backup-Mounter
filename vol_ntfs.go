package main

import (
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"www.velocidex.com/golang/go-ntfs/parser"
)

// ntfsVol serializes every call into go-ntfs behind one mutex.
// ponytail: per-volume global lock; go-ntfs has internal LRUs whose concurrency we don't trust, revisit if parallel copies crawl.
type ntfsVol struct {
	mu  *sync.Mutex
	ctx *parser.NTFSContext
}

func openNTFS(r *io.SectionReader) (fs.FS, string, error) {
	paged, err := parser.NewPagedReader(r, 64<<10, 1024)
	if err != nil {
		return nil, "", err
	}
	ctx, err := parser.GetNTFSContext(paged, 0)
	if err != nil {
		return nil, "", err
	}
	v := ntfsVol{&sync.Mutex{}, ctx}
	return volFS{v}, v.label(), nil
}

// label reads $VOLUME_NAME (attribute 0x60) of $Volume (MFT entry 3).
func (v ntfsVol) label() string {
	vol, err := v.ctx.GetMFT(3)
	if err != nil {
		return ""
	}
	s, err := parser.OpenStream(v.ctx, vol, 0x60, parser.WILDCARD_STREAM_ID, parser.WILDCARD_STREAM_NAME)
	if err != nil {
		return ""
	}
	b := make([]byte, 256)
	n, _ := s.ReadAt(b, 0)
	u := make([]uint16, n/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(u))
}

func (v ntfsVol) entry(name string) (*parser.MFT_ENTRY, error) {
	root, err := v.ctx.GetMFT(5)
	if err != nil || name == "." {
		return root, err
	}
	return root.Open(v.ctx, name)
}

func ntfsInfo(name string, isDir bool, size int64, mtime, btime, atime time.Time) fileInfo {
	mode := uint32(0x8000)
	if isDir {
		mode = 0x4000
	}
	// ponytail: reparse points (symlinks/junctions) look like plain files or dirs.
	return newInfo(name, mode, size, mtime, btime, atime)
}

func (v ntfsVol) stat(name string) (fileInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, err := v.entry(name)
	if err != nil {
		return fileInfo{}, fmt.Errorf("%s: %w", err, fs.ErrNotExist)
	}
	// Stat returns one FileInfo per name (hard links) and per data stream;
	// the first is the unnamed stream under the primary name. Prefer the
	// entry carrying the name we were asked for.
	infos := parser.Stat(v.ctx, e)
	if len(infos) == 0 {
		if name == "." {
			return newInfo(".", 0x4000, 0, time.Time{}, time.Time{}, time.Time{}), nil
		}
		return fileInfo{}, fmt.Errorf("ntfs: no metadata for %q", name)
	}
	fi := infos[0]
	for _, i := range infos {
		if strings.EqualFold(i.Name, path.Base(name)) {
			fi = i
			break
		}
	}
	return ntfsInfo(path.Base(name), fi.IsDir, fi.Size, fi.Mtime, fi.Btime, fi.Atime), nil
}

// readDir lists the directory's own $I30 index, so a hard-linked file shows
// the name it has here (not one from another directory) and DOS 8.3 aliases
// are dropped. The 16 NTFS metafiles ($MFT, $Bitmap, ...) are hidden at the
// root like Windows does.
// ponytail: size and mtime come from the index entry, which NTFS updates lazily; stat() reads the MFT record and is exact.
func (v ntfsVol) readDir(name string) ([]fileInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, err := v.entry(name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", err, fs.ErrNotExist)
	}
	var out []fileInfo
	seen := map[uint64]bool{}
	for _, rec := range e.Dir(v.ctx) {
		fn := rec.File()
		ref := rec.MftReference()
		if seen[ref] || !rec.IsValid() || fn.NameType().Name == "DOS" || (name == "." && ref < 16) {
			continue
		}
		seen[ref] = true
		const fileNameDirFlag = 0x10000000 // FILE_NAME.Flags bit go-ntfs doesn't name
		isDir := parser.ParseUint32(fn.Reader, fn.Profile.Off_FILE_NAME_Flags+fn.Offset)&fileNameDirFlag != 0
		out = append(out, ntfsInfo(fn.Name(), isDir, int64(fn.FilenameSize()), fn.File_modified().Time, fn.Created().Time, fn.File_accessed().Time))
	}
	return out, nil
}

func (v ntfsVol) open(name string) (io.ReaderAt, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, err := v.entry(name)
	if err != nil {
		return nil, err
	}
	// The unnamed $DATA stream; "" (not the wildcard) so an ADS is never picked.
	s, err := parser.OpenStream(v.ctx, e, parser.ATTR_TYPE_DATA, parser.WILDCARD_STREAM_ID, "")
	if err != nil {
		return nil, err
	}
	return lockedReaderAt{v.mu, s}, nil
}
