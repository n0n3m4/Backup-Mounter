package filesystem_exfat

// backup-mounter: proves directory reads honour NoFatChain (Windows-style
// contiguous multi-cluster directories whose FAT entries are zero).

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestNoFatChainDirectory(t *testing.T) {
	// This library never grows a directory past one cluster, so build two
	// one-cluster directories /d and /e on consecutive clusters and splice /e's
	// cluster onto /d the way Windows lays out a 2-cluster directory.
	const perDir = 42 // 42 × 3 entries fill a 4 KiB cluster to 2 free slots
	img := filepath.Join(t.TempDir(), "img")
	fsys, err := Format(img, 8<<20, FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range []string{"/d", "/e"} {
		if err := fsys.MkDir(p, 0o755); err != nil {
			t.Fatal(err)
		}
		for j := 0; j < perDir; j++ {
			if err := fsys.WriteFile(fmt.Sprintf("%s/f%03d", p, i*perDir+j), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	fsys.Close()

	fs := openTestFS(t, img)
	le := binary.LittleEndian
	cs := int64(fs.info.ClusterSize())
	root, err := fs.readDirBuf(fs.info.RootDirectoryCluster)
	if err != nil {
		t.Fatal(err)
	}
	find := func(name uint16) int { // offset of the 0x85 entry whose 1-char name matches
		for off := 0; off+3*dirEntrySize <= len(root); off += dirEntrySize {
			if root[off] == exfatEntryFile && root[off+dirEntrySize+3] == 1 && le.Uint16(root[off+2*dirEntrySize+2:]) == name {
				return off
			}
		}
		t.Fatalf("entry %q not found", rune(name))
		return 0
	}
	d, e := find('d')+dirEntrySize, find('e')
	first := le.Uint32(root[d+20:])
	if second := le.Uint32(root[e+dirEntrySize+20:]); second != first+1 {
		t.Skipf("/d at cluster %d, /e at %d: not consecutive", first, second)
	}
	// /d's first cluster must not end the listing: mark its free slots unused-but-nonzero.
	dataBase := fs.info.ClusterHeapOffsetBytes(fs.partOffset)
	c1 := make([]byte, cs)
	if _, err := fs.f.ReadAt(c1, dataBase+int64(first-2)*cs); err != nil {
		t.Fatal(err)
	}
	for off := 0; off < len(c1); off += dirEntrySize {
		if c1[off] == exfatEntryEnd {
			c1[off] = exfatEntryFile &^ 0x80
		}
	}
	if _, err := fs.f.WriteAt(c1, dataBase+int64(first-2)*cs); err != nil {
		t.Fatal(err)
	}
	for k := 0; k < 3; k++ { // delete /e's entry set
		root[e+k*dirEntrySize] &^= 0x80
	}
	root[d+1] |= exfatFlagNoFatChain
	le.PutUint64(root[d+8:], uint64(2*cs))
	le.PutUint64(root[d+24:], uint64(2*cs))
	if err := fs.writeDirBuf(fs.info.RootDirectoryCluster, root); err != nil {
		t.Fatal(err)
	}
	fatBase := fs.info.FATOffsetBytes(fs.partOffset)
	if _, err := fs.f.WriteAt(make([]byte, 8), fatBase+int64(first)*4); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	ro, err := OpenReader(f, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	if names, err := ro.ListDir("/d"); err != nil || len(names) != 2*perDir {
		t.Fatalf("ListDir(/d) = %d entries, err %v; want %d", len(names), err, 2*perDir)
	}
	if _, err := ro.Stat(fmt.Sprintf("/d/f%03d", 2*perDir-1)); err != nil {
		t.Fatalf("Stat last file: %v", err)
	}
}
