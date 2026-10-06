package main

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"log"
	"sort"
	"strings"

	"github.com/KarpelesLab/squashfs"
)

type image struct {
	name string
	ino  *squashfs.Inode
}

// findImages returns the disk images to mount: those named by --image, else every one
// detected inside the squashfs, largest first. Detection always runs and is logged, so the
// user sees every possible --image value at mount time.
func findImages(base string, sq *squashfs.Superblock, names []string) ([]image, error) {
	var found []image
	err := fs.WalkDir(sq, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		ino, err := sq.FindInode(p, false)
		if err != nil {
			return err
		}
		if isDiskImage(ino, int64(ino.Size)) {
			found = append(found, image{p, ino})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].ino.Size > found[j].ino.Size })
	for _, img := range found {
		log.Printf("%s: disk image %s (%d MiB), mountable with --image", base, img.name, img.ino.Size>>20)
	}
	if len(names) == 0 {
		return found, nil
	}
	var imgs []image
	for _, n := range names {
		n = strings.TrimPrefix(n, "/")
		ino, err := sq.FindInode(n, true)
		if err != nil {
			return nil, fmt.Errorf("--image %s: %w", n, err)
		}
		imgs = append(imgs, image{n, ino})
	}
	return imgs, nil
}

// isDiskImage: a VHD/VHDX, an MBR signature with a populated partition entry,
// a GPT header, or a bare filesystem we can read.
func isDiskImage(r io.ReaderAt, size int64) bool {
	return rawDiskSignature(r) || sniffContainer(r, size) != ""
}

// rawDiskSignature: a GPT header, an MBR with a populated partition entry, or a
// filesystem we can read, all within the first 64 KiB.
func rawDiskSignature(r io.ReaderAt) bool {
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
