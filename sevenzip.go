package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/bodgit/sevenzip"
)

// sevenMagic starts every 7z archive.
// ponytail: SFX stubs (signature after an .exe) are not sniffed; scan the first MiB like bodgit does if one turns up.
var sevenMagic = []byte("7z\xbc\xaf\x27\x1c")

// open7z mounts a 7z archive: each folder (solid block) is one forward-only stream, so the
// folder starts are spanReader's restart points and the one live decoder serves every file.
//
//	file -> raw chunkCache (1/8) -> sevenzip (header, packed streams) -> spanReader -> chunkCache (7/8) -> arcFS
//
// ponytail: no password (NewReaderWithPassword + a --password flag), no multi-volume .7z.001 (one ReaderAt).
func open7z(file string, f *os.File, st os.FileInfo, budget int64) (volume, error) {
	name := filepath.Base(file)
	z, err := sevenzip.NewReader(newChunkCache(f, st.Size(), 4<<20, budget/8), st.Size())
	if re := (*sevenzip.ReadError)(nil); errors.As(err, &re) && re.Encrypted {
		return volume{}, fmt.Errorf("%s: password-protected 7z not supported (%w)", file, err)
	}
	if err != nil {
		return volume{}, fmt.Errorf("%s: %w", file, err)
	}
	s := &spanReader{open: func(i int) (io.Reader, error) { return z.FolderReader(i) }}
	for i := range z.NumFolders() {
		s.starts = append(s.starts, s.size)
		s.size += z.FolderSize(i)
	}
	s.cache = newChunkCache(s, s.size, 4<<20, budget/8*7)
	a := newArcFS(st.ModTime())
	for _, zf := range z.File {
		h := zf.FileHeader
		info := func(mode uint32) fileInfo {
			return newInfo("", mode, int64(h.UncompressedSize), h.Modified, h.Created, h.Accessed)
		}
		n, m := arcName(h.Name), h.Mode()
		switch folder, off, ok := zf.Folder(); {
		case m.IsDir():
			a.add(n, info(0x4000), nil, 0)
		case !m.IsRegular(): // symlink (stored as its target text), device: 0-byte file
			a.add(n, info(0), nil, 0)
		case ok:
			a.add(n, info(0x8000), s.cache, s.starts[folder]+off)
		default:
			a.add(n, info(0x8000), nil, 0)
		}
	}
	log.Printf("%s: 7z, %d entries, %d folders, %d MiB", name, len(z.File), z.NumFolders(), s.size>>20)
	return volume{name: name, kind: "7z", label: name, size: s.size, fsys: volFS{a}}, nil
}
