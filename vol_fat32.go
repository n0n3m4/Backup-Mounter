package main

import (
	"io"
	"io/fs"
	"path"
	"time"

	fat32 "github.com/go-filesystems/fat32"
	filesystem "github.com/go-filesystems/interface"
)

// fat32Vol: the library exposes no timestamps, so every mtime is zero.
type fat32Vol struct{ fs filesystem.Filesystem }

func openFAT32(r *io.SectionReader) (fs.FS, string, error) {
	f, err := fat32.OpenReader(r, r.Size())
	if err != nil {
		return nil, "", err
	}
	label := ""
	if l, ok := f.(filesystem.LabelReader); ok {
		label = l.Label()
	}
	return volFS{fat32Vol{f}}, label, nil
}

func (v fat32Vol) stat(name string) (fileInfo, error) {
	st, err := v.fs.Stat(path.Join("/", name))
	if err != nil {
		return fileInfo{}, err
	}
	return newInfo(path.Base(name), uint32(st.Mode()), int64(st.Size()), time.Time{}), nil
}

// ponytail: one Stat (a path lookup) per entry, O(n²) in directory size; fine for an ESP, revisit for a 10k-file camera card.
func (v fat32Vol) readDir(name string) ([]fileInfo, error) {
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

func (v fat32Vol) open(name string) (io.ReaderAt, error) {
	return v.fs.(filesystem.Opener).OpenFile(path.Join("/", name))
}
