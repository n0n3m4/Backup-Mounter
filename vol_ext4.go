package main

import (
	"io"
	"io/fs"
	"path"
	"sync"

	"github.com/aoiflux/libext"
)

type ext4Vol struct{ fs *libext.FS }

func openExt4(r *io.SectionReader) (fs.FS, string, error) {
	x, err := libext.OpenWithSize(r, uint64(r.Size()))
	if err != nil {
		return nil, "", err
	}
	return volFS{ext4Vol{x}}, x.Superblock().VolumeName, nil
}

func (v ext4Vol) inode(name string) (libext.Inode, error) {
	e, err := v.fs.LookupPath("/" + name)
	if err != nil {
		return libext.Inode{}, err
	}
	return v.fs.ReadInode(e.Inode)
}

func (v ext4Vol) stat(name string) (fileInfo, error) {
	in, err := v.inode(name)
	return newInfo(path.Base(name), uint32(in.Mode), int64(in.Size), in.Mtime), err
}

func (v ext4Vol) readDir(name string) ([]fileInfo, error) {
	dir, err := v.inode(name)
	if err != nil {
		return nil, err
	}
	entries, err := v.fs.ListDirEx(dir.Number, libext.DirOptions{WithInodeMetadata: true})
	if err != nil {
		return nil, err
	}
	out := make([]fileInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, newInfo(e.Name, uint32(e.Mode), int64(e.Size), e.Times.Mtime))
	}
	return out, nil
}

// open gives each handle its own *libext.File: it carries a seek offset and
// a lazily built block map, so it is not safe to share.
func (v ext4Vol) open(name string) (io.ReaderAt, error) {
	in, err := v.inode(name)
	if err != nil {
		return nil, err
	}
	f, err := v.fs.Open(in.Number)
	return lockedReaderAt{&sync.Mutex{}, f}, err
}
