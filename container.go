package main

import (
	"fmt"
	"io"
	"log"

	"github.com/aoiflux/libvhdi/reader"
)

// sniffContainer recognises the virtual-disk wrappers we can open:
// "vhdxfile" at 0, "conectix" at 0 (dynamic VHD footer copy) or at size-512
// (every VHD footer). Anything else ("" ) is treated as a raw disk or filesystem.
func sniffContainer(r io.ReaderAt, size int64) string {
	at := func(off int64) string {
		var b [8]byte
		if off < 0 {
			return ""
		}
		n, _ := r.ReadAt(b[:], off)
		return string(b[:n])
	}
	switch {
	case at(0) == "vhdxfile":
		return "vhdx"
	case at(0) == "conectix", at(size-512) == "conectix":
		return "vhd"
	}
	return ""
}

// unwrap turns a VHD/VHDX into its virtual disk; anything else passes through untouched.
// ponytail: differencing disks are opened without parent lookup and fail on read; add a
// ParentResolver over the squashfs / the file's directory if such backups turn up.
func unwrap(name string, r io.ReaderAt, size int64) (io.ReaderAt, int64, error) {
	kind := sniffContainer(r, size)
	if kind == "" {
		return r, size, nil
	}
	d, err := reader.Open(r, &reader.Options{
		Size:            size,
		ParentResolver:  reader.NoParentResolution,
		AllowDirtyImage: true, // read-only: a backup taken mid-write is still worth a look
	})
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", kind, err)
	}
	if d.NeedsParent() {
		return nil, 0, fmt.Errorf("%s: differencing disk, parent not supported", kind)
	}
	if d.IsDirty() {
		log.Printf("%s: %s log could not be replayed, contents may be stale", name, kind)
	}
	for _, w := range d.Warnings() {
		log.Printf("%s: %s: %s", name, kind, w)
	}
	log.Printf("%s: %s, %d MiB virtual", name, kind, d.Size()>>20)
	return d, int64(d.Size()), nil
}
