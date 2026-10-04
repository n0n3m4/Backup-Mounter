package main

import (
	"archive/tar"
	"encoding/gob"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// --- in-memory tree of an archive's entries (tar and 7z) ---

type arcNode struct {
	fileInfo
	kids map[string]*arcNode // directories only
	r    io.ReaderAt         // regular files with data; nil = empty
	off  int64
}

// arcFS is the backend of a tar or 7z volume: the whole tree is built at open, read-only after.
type arcFS struct {
	root  *arcNode
	mtime time.Time // archive file's mtime, for directories the archive does not list
}

func newArcFS(mtime time.Time) *arcFS {
	return &arcFS{root: &arcNode{fileInfo: newInfo(".", 0x4000, 0, mtime, time.Time{}, time.Time{}), kids: map[string]*arcNode{}}, mtime: mtime}
}

// arcName turns an archive member name into an fs.FS path that cannot leave the root.
func arcName(s string) string {
	s = strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(s, `\`, "/")), "/")
	if s == "" {
		return "."
	}
	return s
}

// add inserts name (an arcName) with fi (its name is set here), creating missing parent
// directories. A directory keeps its children when listed again; any other entry replaces
// what was there (last entry wins, as with tar).
func (a *arcFS) add(name string, fi fileInfo, r io.ReaderAt, off int64) *arcNode {
	fi.name = path.Base(name)
	if fi.size == 0 || !fi.Mode().IsRegular() {
		r, off, fi.size = nil, 0, 0
	}
	if name == "." {
		if fi.IsDir() {
			a.root.fileInfo = fi
		}
		return a.root
	}
	n := a.root
	parts := strings.Split(name, "/")
	for _, p := range parts[:len(parts)-1] {
		k := n.kids[p]
		if k == nil || k.kids == nil { // missing, or a file in the way
			k = &arcNode{fileInfo: newInfo(p, 0x4000, 0, a.mtime, time.Time{}, time.Time{}), kids: map[string]*arcNode{}}
			n.kids[p] = k
		}
		n = k
	}
	last := parts[len(parts)-1]
	k := n.kids[last]
	if k == nil || k.kids == nil || !fi.IsDir() {
		k = &arcNode{}
		if fi.IsDir() {
			k.kids = map[string]*arcNode{}
		}
		n.kids[last] = k
	}
	k.fileInfo, k.r, k.off = fi, r, off
	return k
}

func (a *arcFS) lookup(name string) (*arcNode, error) {
	n := a.root
	if name == "." {
		return n, nil
	}
	for _, p := range strings.Split(name, "/") {
		if n = n.kids[p]; n == nil {
			return nil, fs.ErrNotExist
		}
	}
	return n, nil
}

func (a *arcFS) stat(name string) (fileInfo, error) {
	n, err := a.lookup(name)
	if err != nil {
		return fileInfo{}, err
	}
	return n.fileInfo, nil
}

func (a *arcFS) readDir(name string) ([]fileInfo, error) {
	n, err := a.lookup(name)
	if err != nil {
		return nil, err
	}
	if n.kids == nil {
		return nil, fs.ErrInvalid
	}
	out := make([]fileInfo, 0, len(n.kids))
	for _, k := range n.kids {
		out = append(out, k.fileInfo)
	}
	return out, nil
}

func (a *arcFS) open(name string) (io.ReaderAt, error) {
	n, err := a.lookup(name)
	if err != nil {
		return nil, err
	}
	if n.r == nil {
		return nil, fs.ErrInvalid
	}
	return io.NewSectionReader(n.r, n.off, n.size), nil
}

// --- tar: one pass over the headers records every entry's data offset in <archive>.tidx ---

// isTar: "ustar" at 257 covers POSIX ("ustar\0") and GNU ("ustar ") headers.
// ponytail: v7 tar without magic is not detected; checksum-validate block 0 if anyone has one.
func isTar(r io.ReaderAt) bool {
	var b [5]byte
	_, err := r.ReadAt(b[:], 257)
	return err == nil && string(b[:]) == "ustar"
}

type tarEntry struct {
	Name         string
	Off, Size    int64
	Type         byte
	Link         string
	Mtime, Atime time.Time
}

type tarIndex struct {
	ArchiveSize, ArchiveMtime int64
	Entries                   []tarEntry
}

// openTar indexes the tar stream (headers read from src, a bare file or the decompressed
// stream) and serves file data from data, the cached reader over the same bytes.
func openTar(file string, st os.FileInfo, src, data io.ReaderAt, size int64) (volume, error) {
	name := filepath.Base(file)
	var ents []tarEntry
	loaded := false
	for _, p := range idxPaths(file, ".tidx") {
		if e, err := loadTarIndex(p, st); err == nil {
			log.Printf("%s: tar index loaded from %s", name, p)
			ents, loaded = e, true
			break
		}
	}
	if !loaded {
		var err error
		if ents, err = scanTar(name, src, size); err != nil {
			return volume{}, fmt.Errorf("%s: %w", file, err)
		}
		saveTarIndex(name, idxPaths(file, ".tidx"), tarIndex{st.Size(), st.ModTime().UnixNano(), ents})
	}
	a := newArcFS(st.ModTime())
	for _, e := range ents {
		n := arcName(e.Name)
		switch e.Type {
		case tar.TypeDir:
			a.add(n, newInfo("", 0x4000, 0, e.Mtime, time.Time{}, e.Atime), nil, 0)
		case tar.TypeReg:
			a.add(n, newInfo("", 0x8000, e.Size, e.Mtime, time.Time{}, e.Atime), data, e.Off)
		case tar.TypeLink: // the target's data; 0 bytes when it is not (yet) in the tree
			fi := newInfo("", 0x8000, 0, e.Mtime, time.Time{}, e.Atime)
			var r io.ReaderAt
			var off int64
			if t, err := a.lookup(arcName(e.Link)); err == nil && t.r != nil {
				fi.size, r, off = t.size, t.r, t.off
			}
			a.add(n, fi, r, off)
		default: // symlink, device, fifo: 0-byte file (disk.go newInfo)
			a.add(n, newInfo("", 0, 0, e.Mtime, time.Time{}, e.Atime), nil, 0)
		}
	}
	log.Printf("%s: tar, %d entries, %d MiB", name, len(ents), size>>20)
	return volume{name: name, kind: "tar", label: name, size: size, fsys: volFS{a}}, nil
}

// seekReader reads an io.ReaderAt sequentially; Seek lets archive/tar skip file data unread.
type seekReader struct {
	r         io.ReaderAt
	pos, size int64
}

func (s *seekReader) Read(p []byte) (int, error) {
	if s.pos >= s.size {
		return 0, io.EOF
	}
	p = p[:min(int64(len(p)), s.size-s.pos)]
	n, err := s.r.ReadAt(p, s.pos)
	s.pos += int64(n)
	if n == len(p) {
		err = nil
	}
	return n, err
}

func (s *seekReader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		s.pos = off
	case io.SeekCurrent:
		s.pos += off
	case io.SeekEnd:
		s.pos = s.size + off
	}
	return s.pos, nil
}

func scanTar(name string, src io.ReaderAt, size int64) ([]tarEntry, error) {
	sr := &seekReader{r: src, size: size}
	tr := tar.NewReader(sr)
	var ents []tarEntry
	sparse, lastLog := 0, time.Now()
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if isSparse(hdr) {
			// ponytail: GNU sparse entries skipped; walk the sparse map if a backup uses --sparse.
			sparse++
			continue
		}
		ents = append(ents, tarEntry{hdr.Name, sr.pos, hdr.Size, hdr.Typeflag, hdr.Linkname, hdr.ModTime, hdr.AccessTime})
		if time.Since(lastLog) >= 10*time.Second {
			lastLog = time.Now()
			log.Printf("%s: indexing tar, %d entries, %d MiB", name, len(ents), sr.pos>>20)
		}
	}
	if sparse > 0 {
		log.Printf("%s: %d GNU sparse entries skipped (not supported)", name, sparse)
	}
	return ents, nil
}

func isSparse(hdr *tar.Header) bool {
	if hdr.Typeflag == tar.TypeGNUSparse {
		return true
	}
	for k := range hdr.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return true
		}
	}
	return false
}

func loadTarIndex(p string, st os.FileInfo) ([]tarEntry, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var ix tarIndex
	if err := gob.NewDecoder(f).Decode(&ix); err != nil {
		return nil, err
	}
	if ix.ArchiveSize != st.Size() || ix.ArchiveMtime != st.ModTime().UnixNano() {
		return nil, fmt.Errorf("stale index")
	}
	return ix.Entries, nil
}

// saveTarIndex writes the index to the first candidate that works; failure only costs a re-scan next time.
func saveTarIndex(name string, paths []string, ix tarIndex) {
	for _, p := range paths {
		os.MkdirAll(filepath.Dir(p), 0o755)
		f, err := os.Create(p + ".tmp")
		if err != nil {
			continue
		}
		err = gob.NewEncoder(f).Encode(ix)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(p+".tmp", p)
		}
		if err == nil {
			log.Printf("%s: tar index written to %s", name, p)
			return
		}
		os.Remove(p + ".tmp")
	}
	log.Printf("%s: tar index not saved, the next mount re-scans", name)
}
