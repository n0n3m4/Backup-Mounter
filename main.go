// Command backup-mounter mounts every filesystem inside the disk images of a
// squashfs backup as a read-only Windows drive (via WinFsp), without
// extracting anything.
package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/KarpelesLab/squashfs"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

func main() {
	cacheFlag := flag.String("cache", "8G", "RAM cache budget (e.g. 8G, 512M)")
	var images multiFlag
	flag.Var(&images, "image", "path of a disk image inside the squashfs (repeatable; default: auto-detect)")
	walk := flag.Bool("walk", false, "print path, size and sha256 of every file instead of mounting")
	debug := flag.Bool("debug", false, "verbose WinFsp logging")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s [flags] backup.squashfs\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	budget, err := parseSize(*cacheFlag)
	if err != nil {
		log.Fatalf("--cache: %v", err)
	}
	vols, err := openStack(flag.Arg(0), budget, images)
	if err != nil {
		log.Fatal(err)
	}
	if len(vols) == 0 {
		log.Fatal("no readable filesystems found")
	}
	for _, v := range vols {
		log.Printf("%s: %s, label %q, %d MiB", v.name, v.kind, v.label, v.size>>20)
	}
	if *walk {
		for _, v := range vols {
			if err := walkVolume(os.Stdout, v); err != nil {
				log.Fatalf("%s: %v", v.name, err)
			}
		}
		return
	}
	if err := mount(vols, *debug); err != nil {
		log.Fatal(err)
	}
}

// openStack: file -> raw chunk cache -> squashfs -> disk images ->
// decompressed chunk cache -> partitions -> volumes.
func openStack(path string, budget int64, images []string) ([]volume, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	sq, err := squashfs.New(newChunkCache(f, st.Size(), 4<<20, budget/8*7))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	imgs, err := findImages(sq, images)
	if err != nil {
		return nil, err
	}
	if len(imgs) == 0 {
		return nil, fmt.Errorf("%s: no disk images found (use --image)", path)
	}
	var vols []volume
	for _, img := range imgs {
		size := int64(img.ino.Size)
		r := newChunkCache(img.ino, size, int64(sq.BlockSize), budget/8/int64(len(imgs)))
		vols = append(vols, openVolumes(img.name, r, size)...)
	}
	return vols, nil
}

func parseSize(s string) (int64, error) {
	mult := int64(1)
	switch strings.ToUpper(s[len(s)-min(len(s), 1):]) {
	case "K":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	case "T":
		mult = 1 << 40
	}
	if mult > 1 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return n * mult, nil
}

// walkVolume prints "<volume>/<path> <size> <sha256>" for every regular file.
func walkVolume(w io.Writer, v volume) error {
	return fs.WalkDir(v.fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		f, err := v.fsys.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		_, err = fmt.Fprintf(w, "%s/%s %d %x\n", v.name, p, n, h.Sum(nil))
		return err
	})
}
