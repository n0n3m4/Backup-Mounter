package main

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"

	"github.com/KarpelesLab/squashfs"
)

type image struct {
	name string
	ino  *squashfs.Inode
}

// findImages returns the disk images inside the squashfs, largest first.
// names (from --image) bypass detection.
func findImages(sq *squashfs.Superblock, names []string) ([]image, error) {
	var imgs []image
	for _, n := range names {
		n = strings.TrimPrefix(n, "/")
		ino, err := sq.FindInode(n, true)
		if err != nil {
			return nil, fmt.Errorf("--image %s: %w", n, err)
		}
		imgs = append(imgs, image{n, ino})
	}
	if len(names) == 0 {
		err := fs.WalkDir(sq, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			ino, err := sq.FindInode(p, false)
			if err != nil {
				return err
			}
			if isDiskImage(ino) {
				imgs = append(imgs, image{p, ino})
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.SliceStable(imgs, func(i, j int) bool { return imgs[i].ino.Size > imgs[j].ino.Size })
	return imgs, nil
}

// isDiskImage: MBR signature with a populated partition entry, a GPT header,
// or a bare filesystem we can read.
func isDiskImage(r io.ReaderAt) bool {
	var b [1024]byte
	if n, _ := r.ReadAt(b[:], 0); n < len(b) {
		return false
	}
	if string(b[512:520]) == "EFI PART" {
		return true
	}
	if b[510] == 0x55 && b[511] == 0xAA {
		for i := 446; i < 510; i += 16 {
			if b[i+4] != 0 && !bytes.Equal(b[i+12:i+16], []byte{0, 0, 0, 0}) {
				return true
			}
		}
	}
	return probe(r) != ""
}
