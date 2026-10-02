package main

import (
	"io"
	"io/fs"
	"path"
	"time"

	exfat "github.com/go-filesystems/exfat"
	fat32 "github.com/go-filesystems/fat32"
	filesystem "github.com/go-filesystems/interface"
)

// fatVol serves FAT32 and exFAT. Timestamps come from the forks' Stat (third_party/{fat32,exfat}); the
// upstream interface has none.
type fatVol struct{ fs filesystem.Filesystem }

func openFAT32(r *io.SectionReader) (fs.FS, string, error) {
	f, err := fat32.OpenReader(r, r.Size())
	if err != nil {
		return nil, "", err
	}
	fsys, label := wrapFAT(f)
	return fsys, label, nil
}

func openExFAT(r *io.SectionReader) (fs.FS, string, error) {
	f, err := exfat.OpenReader(r, r.Size())
	if err != nil {
		return nil, "", err
	}
	fsys, label := wrapFAT(f)
	return fsys, label, nil
}

func wrapFAT(f filesystem.Filesystem) (fs.FS, string) {
	label := ""
	if l, ok := f.(filesystem.LabelReader); ok {
		label = l.Label()
	}
	return volFS{fatVol{f}}, label
}

func (v fatVol) stat(name string) (fileInfo, error) {
	st, err := v.fs.Stat(path.Join("/", name))
	if err != nil {
		return fileInfo{}, err
	}
	var mtime, btime, atime time.Time // the root has no directory entry, so no times
	if t, ok := st.(interface {
		Times() (mtime, btime, atime time.Time)
	}); ok {
		mtime, btime, atime = t.Times()
	}
	return newInfo(path.Base(name), uint32(st.Mode()), int64(st.Size()), mtime, btime, atime), nil
}

// ponytail: one Stat (a path lookup) per entry, O(n²) in directory size; fine for an ESP, revisit for a 10k-file camera card.
func (v fatVol) readDir(name string) ([]fileInfo, error) {
	entries, err := v.fs.ListDir(path.Join("/", name))
	if err != nil {
		return nil, err
	}
	var out []fileInfo
	for _, e := range entries {
		if e.Name() == "." || e.Name() == ".." {
			continue
		}
		fi, err := v.stat(path.Join(name, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, fi)
	}
	return out, nil
}

func (v fatVol) open(name string) (io.ReaderAt, error) {
	return v.fs.(filesystem.Opener).OpenFile(path.Join("/", name))
}
