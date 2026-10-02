package main

import (
	"io"
	"io/fs"

	btrfs "github.com/go-filesystems/btrfs"
)

// btrfsVol uses the fork's read-only view (third_party/btrfs/readonly.go),
// which reads by range and crosses into subvolumes and snapshots.
// ponytail: shows the top-level subvolume (id 5), not the `set-default` one; mount the default if a backup relies on it.
type btrfsVol struct{ ro *btrfs.RO }

func openBtrfs(r *io.SectionReader) (fs.FS, string, error) {
	ro, err := btrfs.OpenRO(r, r.Size())
	if err != nil {
		return nil, "", err
	}
	return volFS{btrfsVol{ro}}, ro.Label(), nil
}

func btrfsInfo(i btrfs.ROInfo) fileInfo {
	return newInfo(i.Name, uint32(i.Mode), int64(i.Size), i.MTime)
}

func (v btrfsVol) stat(name string) (fileInfo, error) {
	i, err := v.ro.Stat(name)
	return btrfsInfo(i), err
}

func (v btrfsVol) readDir(name string) ([]fileInfo, error) {
	list, err := v.ro.ReadDir(name)
	out := make([]fileInfo, len(list))
	for i, e := range list {
		out[i] = btrfsInfo(e)
	}
	return out, err
}

func (v btrfsVol) open(name string) (io.ReaderAt, error) { return v.ro.OpenFile(name) }
