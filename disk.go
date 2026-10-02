package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-volumes/gpt"
)

type volume struct {
	name  string // e.g. "disk.img#2"
	kind  string // ntfs, ext4, btrfs, fat32
	label string
	size  int64
	fsys  fs.FS
}

// probe identifies the filesystem at offset 0 of r by its magic.
func probe(r io.ReaderAt) string {
	at := func(off int64, n int) string {
		b := make([]byte, n)
		if _, err := r.ReadAt(b, off); err != nil {
			return ""
		}
		return string(b)
	}
	switch {
	case at(3, 8) == "NTFS    ":
		return "ntfs"
	case at(0x438, 2) == "\x53\xef":
		return "ext4"
	case at(0x10040, 8) == "_BHRfS_M":
		return "btrfs"
	case at(0x52, 8) == "FAT32   ":
		return "fat32"
	}
	return ""
}

// openVolumes splits a disk image into partitions and opens every one it can read.
func openVolumes(name string, r io.ReaderAt, size int64) []volume {
	parts := []*io.SectionReader{io.NewSectionReader(r, 0, size)}
	if probe(r) == "" { // a bare filesystem has no partition table to read
		list, err := gpt.List(r, size)
		if err != nil {
			log.Printf("%s: no readable partition table or filesystem: %v", name, err)
			return nil
		}
		parts = nil
		for _, p := range list {
			if p.Scheme == gpt.SchemeMBR && (p.MBRType == 0x05 || p.MBRType == 0x0F || p.MBRType == 0x85) {
				parts = append(parts, logicalPartitions(r, p.StartOffset, size)...)
				continue
			}
			parts = append(parts, io.NewSectionReader(r, p.StartOffset, p.Length))
		}
	}
	var vols []volume
	for i, p := range parts {
		vname := fmt.Sprintf("%s#%d", name, i+1)
		if len(parts) == 1 && p.Size() == size {
			vname = name
		}
		kind := probe(p)
		if kind == "" {
			log.Printf("%s: unknown filesystem, skipped", vname)
			continue
		}
		fsys, label, err := openFS(kind, p)
		if err != nil {
			log.Printf("%s: %s: %v, skipped", vname, kind, err)
			continue
		}
		vols = append(vols, volume{vname, kind, label, p.Size(), fsys})
	}
	return vols
}

func openFS(kind string, r *io.SectionReader) (fs.FS, string, error) {
	switch kind {
	case "ext4":
		return openExt4(r)
	case "btrfs":
		return openBtrfs(r)
	case "ntfs":
		return openNTFS(r)
	case "fat32":
		return openFAT32(r)
	}
	return nil, "", fmt.Errorf("unsupported")
}

// logicalPartitions walks the EBR chain of an MBR extended partition
// (go-volumes/gpt only reports the four primary slots).
func logicalPartitions(r io.ReaderAt, ext, size int64) []*io.SectionReader {
	var out []*io.SectionReader
	ebr := ext
	for i := 0; i < 128; i++ { // bound a looping chain
		var b [512]byte
		if _, err := r.ReadAt(b[:], ebr); err != nil || b[510] != 0x55 || b[511] != 0xAA {
			break
		}
		lba := func(e int) int64 { return int64(binary.LittleEndian.Uint32(b[e:])) * 512 }
		// Entry 1: the logical partition, relative to this EBR.
		if start, n := ebr+lba(446+8), lba(446+12); b[446+4] != 0 && n > 0 && start+n <= size {
			out = append(out, io.NewSectionReader(r, start, n))
		}
		// Entry 2: the next EBR, relative to the extended partition.
		if b[462+4] == 0 {
			break
		}
		ebr = ext + lba(462+8)
	}
	return out
}

// backend is what each vol_*.go provides; volFS turns it into an fs.FS.
// Names are fs.FS-style: "." for the root, "a/b" below it.
type backend interface {
	stat(name string) (fileInfo, error)
	readDir(name string) ([]fileInfo, error)
	open(name string) (io.ReaderAt, error) // regular files only
}

type volFS struct{ b backend }

func (v volFS) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	fi, err := v.b.stat(name)
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	return fi, nil
}

func (v volFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	infos, err := v.b.readDir(name)
	if err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
	}
	out := make([]fs.DirEntry, len(infos))
	for i, fi := range infos {
		out[i] = fs.FileInfoToDirEntry(fi)
	}
	slices.SortFunc(out, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return out, nil
}

func (v volFS) Open(name string) (fs.File, error) {
	fi, err := v.Stat(name)
	if err != nil {
		return nil, err
	}
	f := &file{fileInfo: fi.(fileInfo)}
	if fi.Mode().IsRegular() && fi.Size() > 0 {
		if f.r, err = v.b.open(name); err != nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
	}
	return f, nil
}

type fileInfo struct {
	name  string
	size  int64
	mode  fs.FileMode
	mtime time.Time
}

func (fi fileInfo) Name() string       { return fi.name }
func (fi fileInfo) Size() int64        { return fi.size }
func (fi fileInfo) Mode() fs.FileMode  { return fi.mode }
func (fi fileInfo) ModTime() time.Time { return fi.mtime }
func (fi fileInfo) IsDir() bool        { return fi.mode.IsDir() }
func (fi fileInfo) Sys() any           { return nil }

// newInfo maps a POSIX mode to a read-only FileInfo.
// ponytail: symlinks, devices and fifos show as 0-byte files; WinFsp reparse points if anyone needs them.
func newInfo(name string, posixMode uint32, size int64, mtime time.Time) fileInfo {
	switch posixMode & 0xF000 {
	case 0x4000:
		return fileInfo{name, 0, fs.ModeDir | 0o555, mtime}
	case 0x8000:
		return fileInfo{name, size, 0o444, mtime}
	}
	return fileInfo{name, 0, 0o444, mtime}
}

// file is an opened volume entry. ReadAt is what the mount layer uses.
type file struct {
	fileInfo
	r   io.ReaderAt
	off int64
}

func (f *file) Stat() (fs.FileInfo, error) { return f.fileInfo, nil }
func (f *file) Close() error               { return nil }

func (f *file) ReadAt(p []byte, off int64) (int, error) {
	if f.r == nil || off >= f.size {
		return 0, io.EOF
	}
	if rest := f.size - off; int64(len(p)) > rest {
		n, err := f.r.ReadAt(p[:rest], off)
		if err == nil {
			err = io.EOF
		}
		return n, err
	}
	return f.r.ReadAt(p, off)
}

func (f *file) Read(p []byte) (int, error) {
	if f.IsDir() {
		return 0, fs.ErrInvalid
	}
	n, err := f.ReadAt(p, f.off)
	f.off += int64(n)
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

// lockedReaderAt serializes a ReaderAt that is not safe for concurrent use.
type lockedReaderAt struct {
	mu *sync.Mutex
	r  io.ReaderAt
}

func (l lockedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.r.ReadAt(p, off)
}
