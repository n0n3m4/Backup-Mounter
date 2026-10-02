// Command populate copies a directory tree into a FAT32 or exFAT image using
// the same libraries the mounter reads with (there is no mtools here).
// Symlinks become empty regular files, matching the mounter's view of them.
// Usage: go run ./testdata/populate -kind exfat|fat32 IMAGE SRCDIR
package main

import (
	"flag"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	exfat "github.com/go-filesystems/exfat"
	fat32 "github.com/go-filesystems/fat32"
	filesystem "github.com/go-filesystems/interface"
)

func main() {
	kind := flag.String("kind", "", "exfat or fat32")
	flag.Parse()
	if flag.NArg() != 2 {
		log.Fatal("usage: populate -kind exfat|fat32 IMAGE SRCDIR")
	}
	img, src := flag.Arg(0), flag.Arg(1)
	open := map[string]func(string, int) (filesystem.Filesystem, error){"exfat": exfat.Open, "fat32": fat32.Open}[*kind]
	if open == nil {
		log.Fatalf("unknown -kind %q", *kind)
	}
	v, err := open(img, -1)
	if err != nil {
		log.Fatal(err)
	}
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == src {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		dst := "/" + filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			return v.MkDir(dst, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			return v.WriteFile(dst, nil, 0o644)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return v.WriteFile(dst, b, 0o644)
	})
	if err == nil {
		err = v.Close()
	}
	if err != nil {
		log.Fatal(err)
	}
}
